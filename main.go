package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ─── Config ───────────────────────────────────────────────────────────────

type Backend struct {
	Name                        string `json:"name"`
	URL                         string `json:"url"`
	MaxConcurrent               int    `json:"maxConcurrent"`               // 0 = unlimited
	Tier                        int    `json:"tier"`                        // 0 = king (always admitted), 1 = subject
	GpuWeight                   int    `json:"gpuWeight"`                   // GPU cost when tier-0 active; 0 = no cost
	BlockOnTier0                int    `json:"blockOnTier0"`                // block when tier0 in-flight >= this; 0 = disabled
	MaxQueueDepth               int    `json:"maxQueueDepth"`               // queued beyond maxConcurrent before 429 (default 2)
	MaxConcurrentLargePrefill   int    `json:"maxConcurrentLargePrefill"`   // max concurrent large prefills; 0 = disabled
	LargePrefillThresholdTokens int    `json:"largePrefillThresholdTokens"` // new-token count to qualify as "large prefill" (default 8192)
}

// ─── Per-backend slot manager ─────────────────────────────────────────────

type slotManager struct {
	inflight int32 // atomic
	max      int32
	waiting  int32 // atomic; currently queued
	maxQueue int32
	notify   chan struct{}
}

func newSlot(max, maxQueue int32) *slotManager {
	return &slotManager{
		max:      max,
		maxQueue: maxQueue,
		notify:   make(chan struct{}, 1),
	}
}

// acquire returns true if a slot was obtained (immediately or after queued wait).
// false = queue full or timeout/cancel.
func (s *slotManager) acquire(grace time.Duration, cancel <-chan struct{}) bool {
	// Fast path: CAS a free slot.
	for {
		cur := atomic.LoadInt32(&s.inflight)
		if cur >= s.max {
			break
		}
		if atomic.CompareAndSwapInt32(&s.inflight, cur, cur+1) {
			return true
		}
	}

	// Queue path: check depth, then wait.
	if atomic.AddInt32(&s.waiting, 1) > s.maxQueue {
		atomic.AddInt32(&s.waiting, -1)
		return false // queue full
	}
	defer atomic.AddInt32(&s.waiting, -1)

	deadline := time.NewTimer(grace)
	defer deadline.Stop()
	for {
		select {
		case <-s.notify:
		case <-deadline.C:
			return false
		case <-cancel:
			return false
		}
		// Retr-try CAS after wake.
		for {
			cur := atomic.LoadInt32(&s.inflight)
			if cur >= s.max {
				break // spurious, loop back to wait
			}
			if atomic.CompareAndSwapInt32(&s.inflight, cur, cur+1) {
				return true
			}
		}
	}
}

func (s *slotManager) release() {
	atomic.AddInt32(&s.inflight, -1)
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

func (s *slotManager) snapshot() (inflight, max, waiting, maxQueue int32) {
	return atomic.LoadInt32(&s.inflight), s.max, atomic.LoadInt32(&s.waiting), s.maxQueue
}

// ─── GPU budget (weighted semaphore, only active when tier-0 is busy) ──────

type gpuBudget struct {
	mu       sync.Mutex
	cond     *sync.Cond
	used     int
	max      int
	waiting  int
	maxQueue int
}

func (g *gpuBudget) tryAcquire(weight, maxQueue int, grace time.Duration, cancel <-chan struct{}) bool {
	g.mu.Lock()
	if g.used+weight <= g.max {
		g.used += weight
		g.mu.Unlock()
		return true
	}
	// Queue.
	if g.waiting >= maxQueue {
		g.mu.Unlock()
		return false
	}
	g.waiting++
	// Stay locked for cond.Wait.
	done := make(chan struct{})
	timer := time.NewTimer(grace)
	go func() {
		select {
		case <-timer.C:
			g.mu.Lock()
			g.cond.Broadcast()
			g.mu.Unlock()
		case <-cancel:
			g.mu.Lock()
			g.cond.Broadcast()
			g.mu.Unlock()
		case <-done:
		}
	}()
	g.cond.Wait()
	close(done)
	g.waiting--
	timer.Stop()
	// After wake: re-check under lock.
	if g.used+weight <= g.max {
		g.used += weight
		g.mu.Unlock()
		return true
	}
	// Still can't fit — timeout or lost race.
	g.mu.Unlock()
	return false
}

func (g *gpuBudget) release(weight int) {
	g.mu.Lock()
	g.used -= weight
	g.cond.Broadcast()
	g.mu.Unlock()
}

func (g *gpuBudget) snapshot() (used, max, waiting int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.used, g.max, g.waiting
}

// ─── EWMA duration tracker ────────────────────────────────────────────────

type durationTracker struct {
	mu    sync.Mutex
	ewma  float64 // seconds
	count int
}

func (d *durationTracker) record(dur time.Duration) {
	seconds := dur.Seconds()
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.count == 0 {
		d.ewma = seconds
	} else {
		alpha := 0.3
		d.ewma = alpha*seconds + (1-alpha)*d.ewma
	}
	d.count++
}

func (d *durationTracker) avgSeconds() float64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.count == 0 {
		return 15.0 // default estimate
	}
	return d.ewma
}

// ─── Global state ─────────────────────────────────────────────────────────

var (
	backends       []Backend
	client         = &http.Client{Timeout: 30 * time.Second}
	slots          map[string]*slotManager // per-backend concurrency slots
	prefillSlots   map[string]*slotManager // per-backend large-prefill slots (released on first response byte)
	gpu            *gpuBudget
	queueTimeout   = 30 * time.Second
	durations      map[string]*durationTracker // per backend name
	proxyTransport = &http.Transport{
		// ResponseHeaderTimeout: max wait for first byte from vLLM after
		// sending the request. If vLLM hangs after accepting the connection,
		// we give up and free the slot instead of holding it forever.
		ResponseHeaderTimeout: 5 * time.Minute,
		// IdleConnTimeout: close idle backend connections
		IdleConnTimeout: 60 * time.Second,
	}
)

// tier0Inflight returns total in-flight requests across all tier-0 backends.
func tier0Inflight() int32 {
	var total int32
	for _, b := range backends {
		if b.Tier == 0 {
			if s, ok := slots[b.Name]; ok {
				in, _, _, _ := s.snapshot()
				total += in
			}
		}
	}
	return total
}

func main() {
	port := flag.Int("port", 80, "listen port")
	flag.Parse()

	backendsJSON := os.Getenv("BACKENDS")
	if backendsJSON == "" {
		log.Fatal("BACKENDS env var required")
	}
	if err := json.Unmarshal([]byte(backendsJSON), &backends); err != nil {
		log.Fatalf("invalid BACKENDS JSON: %v", err)
	}
	if len(backends) == 0 {
		log.Fatal("at least one backend required")
	}

	if qt := os.Getenv("MAX_QUEUE_TIMEOUT"); qt != "" {
		if d, err := time.ParseDuration(qt); err == nil {
			queueTimeout = d
		}
	}

	slots = make(map[string]*slotManager)
	prefillSlots = make(map[string]*slotManager)
	durations = make(map[string]*durationTracker)
	totalWeight := 0
	for _, b := range backends {
		durations[b.Name] = &durationTracker{}
		if b.MaxConcurrent > 0 {
			mq := int32(b.MaxQueueDepth)
			if mq == 0 {
				mq = 2
			}
			slots[b.Name] = newSlot(int32(b.MaxConcurrent), mq)
		}
		if b.MaxConcurrentLargePrefill > 0 {
			prefillSlots[b.Name] = newSlot(int32(b.MaxConcurrentLargePrefill), 4)
		}
		if b.GpuWeight > 0 {
			totalWeight += b.GpuWeight
		}
		fields := []string{}
		if b.MaxConcurrent > 0 {
			fields = append(fields, fmt.Sprintf("maxConcurrent=%d", b.MaxConcurrent))
		} else {
			fields = append(fields, "unlimited")
		}
		if b.Tier > 0 {
			fields = append(fields, fmt.Sprintf("tier=%d", b.Tier))
			if b.BlockOnTier0 > 0 {
				fields = append(fields, fmt.Sprintf("blockOnTier0>=%d", b.BlockOnTier0))
			}
		} else {
			fields = append(fields, "tier=0(king)")
		}
		if b.GpuWeight > 0 {
			fields = append(fields, fmt.Sprintf("gpuWeight=%d", b.GpuWeight))
		}
		if b.MaxConcurrentLargePrefill > 0 {
			thresh := b.LargePrefillThresholdTokens
			if thresh == 0 {
				thresh = 8192
			}
			fields = append(fields, fmt.Sprintf("largePrefill=%d/thresh%d", b.MaxConcurrentLargePrefill, thresh))
		}
		log.Printf("  %s -> %s (%s)", b.Name, b.URL, strings.Join(fields, ", "))
	}

	if totalWeight > 0 {
		maxBudget := 0
		mb := os.Getenv("MAX_GPU_BUDGET")
		if mb != "" {
			if _, err := fmt.Sscanf(mb, "%d", &maxBudget); err != nil {
				log.Fatalf("invalid MAX_GPU_BUDGET %q: %v", mb, err)
			}
		}
		if mb == "" {
			maxBudget = 4 // default per docs
		}
		if maxBudget > 0 {
			gpu = &gpuBudget{max: maxBudget, maxQueue: 4}
			gpu.cond = sync.NewCond(&gpu.mu)
			log.Printf("  GPU budget: max=%d (active only when tier-0 in-flight)", maxBudget)
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "vLLM router OK")
	})
	mux.HandleFunc("/stats", handleStats)
	mux.HandleFunc("/v1/models", handleModels)
	mux.HandleFunc("/", handleProxy)

	addr := fmt.Sprintf(":%d", *port)
	log.Printf("vLLM router listening on %s with %d backends (queueTimeout=%s, gpuBudget=%v)",
		addr, len(backends), queueTimeout, gpu != nil)
	log.Fatal(http.ListenAndServe(addr, mux))
}

// ─── /stats ───────────────────────────────────────────────────────────────

func handleStats(w http.ResponseWriter, r *http.Request) {
	type stat struct {
		Name            string  `json:"name"`
		URL             string  `json:"url"`
		Tier            int     `json:"tier"`
		INFlight        int32   `json:"inFlight"`
		MaxConcurrent   int32   `json:"maxConcurrent"`
		Waiting         int32   `json:"waiting"`
		MaxQueueDepth   int32   `json:"maxQueueDepth"`
		PrefillInFlight int32   `json:"prefillInFlight"`
		PrefillWaiting  int32   `json:"prefillWaiting"`
		PrefillMax      int32   `json:"prefillMax"`
		AvgDurationS    float64 `json:"avgDurationS"`
	}
	t0Inflight := tier0Inflight()

	stats := make([]stat, 0, len(backends))
	for _, b := range backends {
		in, max, waiting, mq := int32(0), int32(0), int32(0), int32(0)
		if s, ok := slots[b.Name]; ok {
			in, max, waiting, mq = s.snapshot()
		}
		pIn, pMax, pWaiting, _ := int32(0), int32(0), int32(0), int32(0)
		if s, ok := prefillSlots[b.Name]; ok {
			pIn, pMax, pWaiting, _ = s.snapshot()
		}
		stats = append(stats, stat{
			Name:            b.Name,
			URL:             b.URL,
			Tier:            b.Tier,
			INFlight:        in,
			MaxConcurrent:   max,
			Waiting:         waiting,
			MaxQueueDepth:   mq,
			PrefillInFlight: pIn,
			PrefillWaiting:  pWaiting,
			PrefillMax:      pMax,
			AvgDurationS:    durations[b.Name].avgSeconds(),
		})
	}

	gUsed, gMax, gWaiting := 0, 0, 0
	if gpu != nil {
		gUsed, gMax, gWaiting = gpu.snapshot()
	}

	resp := map[string]any{
		"backends":      stats,
		"gpuUsed":       gUsed,
		"gpuBudget":     gMax,
		"gpuWaiting":    gWaiting,
		"tier0Inflight": t0Inflight,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// ─── /v1/models ───────────────────────────────────────────────────────────

func handleModels(w http.ResponseWriter, r *http.Request) {
	type modelEntry struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		OwnedBy string `json:"owned_by"`
	}
	type modelsResp struct {
		Object string       `json:"object"`
		Data   []modelEntry `json:"data"`
	}

	var wg sync.WaitGroup
	results := make([][]modelEntry, len(backends))
	for i, b := range backends {
		wg.Add(1)
		go func(idx int, backend Backend) {
			defer wg.Done()
			resp, err := client.Get(backend.URL + "/v1/models")
			if err != nil {
				results[idx] = []modelEntry{{ID: backend.Name, Object: "model", OwnedBy: "vllm"}}
				return
			}
			defer resp.Body.Close()
			if resp.StatusCode != 200 {
				results[idx] = []modelEntry{{ID: backend.Name, Object: "model", OwnedBy: "vllm"}}
				return
			}
			var mr modelsResp
			if err := json.NewDecoder(resp.Body).Decode(&mr); err != nil {
				results[idx] = []modelEntry{{ID: backend.Name, Object: "model", OwnedBy: "vllm"}}
				return
			}
			results[idx] = mr.Data
		}(i, b)
	}
	wg.Wait()

	merged := modelsResp{Object: "list", Data: []modelEntry{}}
	for _, entries := range results {
		merged.Data = append(merged.Data, entries...)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(merged)
}

// ─── 429 helper ───────────────────────────────────────────────────────────

func write429(w http.ResponseWriter, reason, backend string, retryAfter int) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Retry-After", fmt.Sprintf("%d", retryAfter))
	w.Header().Set("X-Router-Reason", reason)
	w.Header().Set("X-Router-Backend", backend)
	w.WriteHeader(http.StatusTooManyRequests)
	fmt.Fprintf(w, `{"error":{"type":"%s","message":"backend at capacity","backend":"%s","retry_after":%d}}`,
		reason, backend, retryAfter)
}

// estimateNewTokens estimates the number of NEW tokens being prefilled in this request.
// In a multi-turn conversation, only the last message is new input; prior messages
// are cached from previous turns (radix/prefix cache). For the first request of a
// session (≤2 messages: system + user), ALL content is new prefill.
// Also handles the completions API `prompt` field.
// Rough estimate: ~4 bytes per token (conservative for Qwen tokenizer).
func estimateNewTokens(body []byte) int {
	if len(body) == 0 {
		return 0
	}

	// Try chat completions format first.
	var chat struct {
		Messages []struct {
			Role    string `json:"role"`
			Content any    `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &chat) == nil && len(chat.Messages) > 0 {
		charCount := 0
		if len(chat.Messages) <= 2 {
			// First request of session — all content is new prefill.
			for _, m := range chat.Messages {
				charCount += contentLen(m.Content)
			}
		} else {
			// Continuation — only last message is new input.
			charCount = contentLen(chat.Messages[len(chat.Messages)-1].Content)
		}
		return charCount / 4
	}

	// Try completions API format.
	var completion struct {
		Prompt any `json:"prompt"`
	}
	if json.Unmarshal(body, &completion) == nil {
		return contentLen(completion.Prompt) / 4
	}

	return 0
}

// contentLen extracts character count from a message content field.
// Content can be a string, or an array of content parts (each with "text" field).
func contentLen(content any) int {
	switch v := content.(type) {
	case string:
		return len(v)
	case []any:
		total := 0
		for _, part := range v {
			if m, ok := part.(map[string]any); ok {
				if t, ok := m["text"].(string); ok {
					total += len(t)
				}
			}
		}
		return total
	default:
		return 0
	}
}

// estimateRetryAfter returns a rough seconds estimate of when a slot frees.
func estimateRetryAfter(name string) int {
	avg := durations[name].avgSeconds()
	max := 1
	if s, ok := slots[name]; ok {
		_, mx, _, _ := s.snapshot()
		max = int(mx)
	}
	if max < 1 {
		max = 1
	}
	estimate := int(avg / float64(max))
	if estimate < 1 {
		estimate = 1
	}
	return estimate
}

// ─── Proxy ────────────────────────────────────────────────────────────────

func handleProxy(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.URL.Path, "/v1/") {
		http.NotFound(w, r)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		// This will catch MaxBytesError (truncated body) and other read errors
		http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		return
	}
	r.Body.Close()

	// Resolve backend from model name.
	target := backends[0]
	modelName := ""
	if len(body) > 0 {
		var partial struct {
			Model string `json:"model"`
		}
		if json.Unmarshal(body, &partial) == nil && partial.Model != "" {
			modelName = partial.Model
			for _, b := range backends {
				if b.Name == modelName {
					target = b
					break
				}
			}
		}
	}
	if qModel := r.URL.Query().Get("model"); qModel != "" {
		for _, b := range backends {
			if b.Name == qModel {
				target = b
				break
			}
		}
	}

	cancelCh := r.Context().Done()
	t0Inflight := tier0Inflight()

	// ── Step 1: Cross-tier blocking ────────────────────────────────────────
	// If tier-0 in-flight count meets or exceeds this backend's blockOnTier0
	// threshold, reject immediately. Don't queue — AxonHub handles cloud fallback.
	if target.BlockOnTier0 > 0 && int(t0Inflight) >= target.BlockOnTier0 {
		retry := 1
		for _, b := range backends {
			if b.Tier == 0 {
				avg := int(durations[b.Name].avgSeconds())
				if avg > retry {
					retry = avg
				}
			}
		}
		log.Printf("%s %s -> %s (model=%s) REJECTED: blocked by tier-0 (tier0Inflight=%d >= threshold %d)",
			r.Method, r.URL.Path, target.Name, modelName, t0Inflight, target.BlockOnTier0)
		write429(w, "blocked-by-tier0", target.Name, retry)
		return
	}

	// ── Step 2: GPU budget (only for tier-1 subjects, only when tier-0 active) ──
	// The king (tier 0) is never constrained by GPU budget — it IS the budget.
	// Subjects with gpuWeight get squeezed into the remaining budget when king runs.
	// When king is idle, no budget check at all.
	if gpu != nil && target.Tier > 0 && target.GpuWeight > 0 && t0Inflight > 0 {
		used, maxB, _ := gpu.snapshot()
		if used+target.GpuWeight > maxB {
			log.Printf("%s %s -> %s (model=%s) queued at GPU budget (gpu=%d+%d/%d, tier0=%d)",
				r.Method, r.URL.Path, target.Name, modelName,
				used, target.GpuWeight, maxB, t0Inflight)
			if !gpu.tryAcquire(target.GpuWeight, 4, 5*time.Second, cancelCh) {
				retry := estimateRetryAfter(target.Name)
				log.Printf("%s %s -> %s (model=%s) 429: GPU budget full", r.Method, r.URL.Path, target.Name, modelName)
				write429(w, "gpu-budget-full", target.Name, retry)
				return
			}
		} else {
			if !gpu.tryAcquire(target.GpuWeight, 4, 5*time.Second, cancelCh) {
				retry := estimateRetryAfter(target.Name)
				log.Printf("%s %s -> %s (model=%s) 429: GPU budget full (race)", r.Method, r.URL.Path, target.Name, modelName)
				write429(w, "gpu-budget-full", target.Name, retry)
				return
			}
		}
		defer gpu.release(target.GpuWeight)
	}

	// ── Step 3: Per-backend slot ──────────────────────────────────────────
	sem := slots[target.Name]
	if sem != nil {
		in, maxC, waiting, _ := sem.snapshot()
		if in >= maxC {
			log.Printf("%s %s -> %s (model=%s) queued at backend (%d/%d, waiting=%d, tier0=%d)",
				r.Method, r.URL.Path, target.Name, modelName,
				in, maxC, waiting+1, t0Inflight)
		}
		if !sem.acquire(queueTimeout, cancelCh) {
			retry := estimateRetryAfter(target.Name)
			stage := "backend-full"
			_, _, qW, qMax := sem.snapshot()
			if qW >= qMax {
				stage = "backend-queue-full"
			}
			log.Printf("%s %s -> %s (model=%s) 429: backend at capacity", r.Method, r.URL.Path, target.Name, modelName)
			write429(w, stage, target.Name, retry)
			return
		}
		defer sem.release()
	}

	// ── Step 3.5: Large prefill throttle ─────────────────────────────────
	// Limits how many concurrent large prefills a backend processes. A large
	// prefill is one whose estimated NEW tokens (last message only, not cached
	// context) exceed the backend's threshold. The slot is held during prefill
	// only — released when the first response byte arrives (decode begins).
	// Small requests skip this entirely. Soft-queued up to maxQueue, then 429.
	prefillSem := prefillSlots[target.Name]
	prefillRelease := func() {} // no-op by default
	if prefillSem != nil {
		threshold := target.LargePrefillThresholdTokens
		if threshold == 0 {
			threshold = 8192
		}
		newTokens := estimateNewTokens(body)
		if newTokens >= threshold {
			pin, pmaxC, pwaiting, _ := prefillSem.snapshot()
			if pin >= pmaxC {
				log.Printf("%s %s -> %s (model=%s) queued at prefill (%d/%d, waiting=%d, newTokens=%d)",
					r.Method, r.URL.Path, target.Name, modelName,
					pin, pmaxC, pwaiting+1, newTokens)
			}
			if !prefillSem.acquire(queueTimeout, cancelCh) {
				retry := estimateRetryAfter(target.Name)
				stage := "prefill-full"
				_, _, pqW, pqMax := prefillSem.snapshot()
				if pqW >= pqMax {
					stage = "prefill-queue-full"
				}
				log.Printf("%s %s -> %s (model=%s) 429: prefill at capacity (newTokens=%d)",
					r.Method, r.URL.Path, target.Name, modelName, newTokens)
				write429(w, stage, target.Name, retry)
				return
			}
			prefillReleased := false
			prefillRelease = func() {
				if !prefillReleased {
					prefillReleased = true
					prefillSem.release()
				}
			}
		}
	}

	// ── Step 4: Forward ───────────────────────────────────────────────────
	in, maxC, waiting, _ := int32(0), int32(0), int32(0), int32(0)
	if sem != nil {
		in, maxC, waiting, _ = sem.snapshot()
	}
	gUsed, gMax, _ := 0, 0, 0
	if gpu != nil {
		gUsed, gMax, _ = gpu.snapshot()
	}
	if target.Tier > 0 && target.GpuWeight > 0 && t0Inflight > 0 {
		log.Printf("%s %s -> %s (model=%s) inflight=%d/%d waiting=%d gpu=%d/%d tier0=%d",
			r.Method, r.URL.Path, target.Name, modelName, in, maxC, waiting, gUsed, gMax, t0Inflight)
	} else if sem != nil {
		log.Printf("%s %s -> %s (model=%s) inflight=%d/%d waiting=%d",
			r.Method, r.URL.Path, target.Name, modelName, in, maxC, waiting)
	} else {
		log.Printf("%s %s -> %s (model=%s)", r.Method, r.URL.Path, target.Name, modelName)
	}

	backendURL, err := url.Parse(target.URL)
	if err != nil {
		http.Error(w, "invalid backend URL", http.StatusInternalServerError)
		return
	}

	start := time.Now()
	proxy := httputil.NewSingleHostReverseProxy(backendURL)
	proxy.FlushInterval = -1         // flush immediately for SSE streaming
	proxy.Transport = proxyTransport // response timeout prevents slot leaks
	// Wrap ResponseWriter to capture status + detect completion for duration
	// tracking. Also releases the prefill slot on first response byte.
	tracker := &respTracker{
		ResponseWriter: w,
		status:         200,
		onFirstByte:    prefillRelease,
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))

	proxy.ServeHTTP(tracker, r)

	prefillRelease() // fallback: release if no response was ever written
	durations[target.Name].record(time.Since(start))
}

type respTracker struct {
	http.ResponseWriter
	status      int
	wrote       bool
	onFirstByte func() // called on first WriteHeader/Write; used to release prefill slot
}

func (t *respTracker) WriteHeader(code int) {
	if !t.wrote {
		t.status = code
		t.wrote = true
		if t.onFirstByte != nil {
			t.onFirstByte()
		}
	}
	t.ResponseWriter.WriteHeader(code)
}

func (t *respTracker) Write(b []byte) (int, error) {
	if !t.wrote {
		t.wrote = true
		if t.onFirstByte != nil {
			t.onFirstByte()
		}
	}
	return t.ResponseWriter.Write(b)
}

func (t *respTracker) Flush() {
	if f, ok := t.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
