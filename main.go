package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/djdembeck/llm-router-go/web"
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

// ─── Parsed body (single-pass, heavy fields stay raw) ──────────────────────

type rawMessage struct {
	Content json.RawMessage `json:"content"`
}

type parsedBody struct {
	Model    string          `json:"model"`
	Messages []rawMessage    `json:"messages"`
	Prompt   json.RawMessage `json:"prompt"`
	Stream   bool            `json:"stream"`
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
	// Fast path: CAS a free slot, but never barge ahead of queued waiters.
	for {
		cur := atomic.LoadInt32(&s.inflight)
		if cur >= s.max || atomic.LoadInt32(&s.waiting) > 0 {
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

	// A slot may already be free (e.g. a waiter timed out without consuming
	// its notify). Try once before sleeping.
	select {
	case <-cancel:
		return false
	default:
	}
	for {
		cur := atomic.LoadInt32(&s.inflight)
		if cur >= s.max {
			break
		}
		if atomic.CompareAndSwapInt32(&s.inflight, cur, cur+1) {
			return true
		}
	}

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
	mu      sync.Mutex
	cond    *sync.Cond
	used    int
	max     int
	waiting int
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
	var expired atomic.Bool
	done := make(chan struct{})
	timer := time.NewTimer(grace)
	go func() {
		select {
		case <-timer.C:
			expired.Store(true)
			g.mu.Lock()
			g.cond.Broadcast()
			g.mu.Unlock()
		case <-cancel:
			expired.Store(true)
			g.mu.Lock()
			g.cond.Broadcast()
			g.mu.Unlock()
		case <-done:
		}
	}()
	acquired := false
	for {
		g.cond.Wait()
		if g.used+weight <= g.max {
			// Fit wins even at the deadline boundary (same precedence as today).
			g.used += weight
			acquired = true
			break
		}
		if expired.Load() {
			break
		}
		// Woken by a partial release or lost the race to another waiter:
		// keep waiting until the deadline.
	}
	close(done)
	g.waiting--
	timer.Stop()
	g.mu.Unlock()
	return acquired
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

// ─── Metrics registry (live observability) ────────────────────────────────

// requestSample is one completed (or rejected) request, kept in the burst
// ring and folded into per-backend counters. accepted marks the demand side
// that recordAccept registered at admission; only accepted samples are
// retired from the pending window on completion.
type requestSample struct {
	T            time.Time
	Backend      string
	Path         string
	Status       int
	Stream       bool
	DurMs        float64
	TTFTMs       float64
	NewTokensEst int
	BytesIn      int64
	BytesOut     int64
	accepted     bool
}

// backendMetrics holds per-backend lifetime counters (completed requests
// only), the streaming TTFT EWMA (updated at first response byte), and the
// pending window — requests accepted but not yet completed, which feeds the
// live rates. Rates measure arrival, not completion: a decode can run for
// minutes, but the request's demand began the moment it was admitted.
type backendMetrics struct {
	reqTotal      int64
	bytesInTotal  int64
	bytesOutTotal int64
	tokEstTotal   int64
	ttftEwma      float64 // ms — EWMA over first-byte times, updated at first byte
	ttftSamples   int
	ttftMsNow     float64 // ms — the most recent first-byte sample (no decay)
	reqRate       float64
	bytesRate     float64
	tokEstRate    float64
	// pending: demand recorded at admission, not yet completed. computeRates
	// divides it by the tick interval to get the live rate, then decays the
	// remainder back by (interval / window).
	pending       float64
	pendingReq    float64
	pendingTokEst float64
}

// rateWindowS is how far back the live-rate window reaches. A single tick's
// arrivals produce a full-window rate, so even sparse traffic shows a real
// sustained rate instead of a 2×-per-tick spike that returns to zero.
const rateWindowS = 30

// backendMetricSnapshot is the lock-free view of backendMetrics used by the
// frame builder.
type backendMetricSnapshot struct {
	TTFTMs          float64
	TTFTSampleCount int
	TTFTMsNow       float64
	ReqRate         float64
	BytesRate       float64
	TokEstRate      float64
	ReqTotal        int64
	BytesInTotal    int64
	BytesOutTotal   int64
	TokEstTotal     int64
}

// metricsRegistry is the mutex-guarded home for request counters, the TTFT
// EWMA, and the bounded burst ring (newest appended, oldest evicted).
type metricsRegistry struct {
	mu       sync.Mutex
	backends map[string]*backendMetrics
	ring     []requestSample
	ringCap  int
}

func newMetricsRegistry() *metricsRegistry {
	return &metricsRegistry{backends: map[string]*backendMetrics{}, ringCap: 500}
}

var metrics = newMetricsRegistry()

// recordAccept records the demand side of a request the moment it is
// admitted for proxying: the pending window counters that feed the live
// rates. Rejections (429/413) are not "accepted" — they never enter a
// backend — and are not recorded here.
func (m *metricsRegistry) recordAccept(backend string, bytesIn int64, tokEst int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	bm, ok := m.backends[backend]
	if !ok {
		bm = &backendMetrics{}
		m.backends[backend] = bm
	}
	bm.pendingReq += 1
	bm.pending += float64(bytesIn)
	bm.pendingTokEst += float64(tokEst)
}

// recordFirstByte captures TTFT at first response byte, while the request is
// still live — not at completion, which would lag the operator by the full
// decode. Only streaming requests carry a first-byte time; the EWMA updates
// here so the dashboard sees the real current value as it arrives.
func (m *metricsRegistry) recordFirstByte(backend string, ttftMs float64) {
	if ttftMs <= 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	bm, ok := m.backends[backend]
	if !ok {
		return
	}
	if bm.ttftSamples == 0 {
		bm.ttftEwma = ttftMs
	} else {
		const alpha = 0.3
		bm.ttftEwma = alpha*ttftMs + (1-alpha)*bm.ttftEwma
	}
	bm.ttftSamples++
	bm.ttftMsNow = ttftMs
}

// recordRequest folds one COMPLETED (or rejected) request into per-backend
// lifetime counters and the burst ring, and retires it from the pending
// window if it had been accepted. Lifetime totals count completed work only;
// the pending counters are the live-rate source until completion.
func (m *metricsRegistry) recordRequest(backend string, s requestSample) {
	m.mu.Lock()
	defer m.mu.Unlock()
	bm, ok := m.backends[backend]
	if !ok {
		bm = &backendMetrics{}
		m.backends[backend] = bm
	}
	// Retire the pending demand for requests that were actually accepted and
	// proxied. Router-originated rejections (413/429) never enter the
	// pending window — counting them would inflate the live rate with
	// demand that never reached a backend.
	if s.accepted {
		bm.pendingReq--
		bm.pending -= float64(s.BytesIn)
		bm.pendingTokEst -= float64(s.NewTokensEst)
	}
	bm.reqTotal++
	bm.bytesInTotal += s.BytesIn
	bm.bytesOutTotal += s.BytesOut
	bm.tokEstTotal += int64(s.NewTokensEst)
	m.ring = append(m.ring, s)
	if len(m.ring) > m.ringCap {
		m.ring = m.ring[len(m.ring)-m.ringCap:]
	}
}

// burst returns up to limit ring samples, oldest first (newest last), capped
// at the ring length.
func (m *metricsRegistry) burst(limit int) []requestSample {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := len(m.ring)
	if limit > n {
		limit = n
	}
	if limit < 0 {
		limit = 0
	}
	out := make([]requestSample, limit)
	copy(out, m.ring[n-limit:])
	return out
}

// computeRates refreshes per-backend req/bytes/tok-est live rates. Each tick,
// the whole pending window is divided by the tick interval — a request
// admitted this tick contributes a full window-length rate — and then the
// window decays back by (interval / window) so its tail lingers for the
// rest of rateWindowS instead of vanishing on the next tick. The result is a
// sustained rate that tracks arrivals, not completion deltas: a 10-minute
// decode shows its true admission rate from the moment it was accepted.
func (m *metricsRegistry) computeRates(dt time.Duration) {
	if dt <= 0 {
		return
	}
	dtS := dt.Seconds()
	window := float64(rateWindowS)
	if window < dtS {
		window = dtS
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, bm := range m.backends {
		bm.reqRate = bm.pendingReq / dtS
		bm.bytesRate = float64(bm.pending) / dtS
		bm.tokEstRate = bm.pendingTokEst / dtS
		decay := dtS / window
		bm.pendingReq *= 1 - decay
		bm.pending *= 1 - decay
		bm.pendingTokEst *= 1 - decay
	}
}

func (m *metricsRegistry) snapshot(backend string) backendMetricSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	var zero backendMetricSnapshot
	bm, ok := m.backends[backend]
	if !ok {
		return zero
	}
	return backendMetricSnapshot{
		TTFTMs:          bm.ttftEwma,
		TTFTSampleCount: bm.ttftSamples,
		TTFTMsNow:       bm.ttftMsNow,
		ReqRate:         bm.reqRate,
		BytesRate:       bm.bytesRate,
		TokEstRate:      bm.tokEstRate,
		ReqTotal:        bm.reqTotal,
		BytesInTotal:    bm.bytesInTotal,
		BytesOutTotal:   bm.bytesOutTotal,
		TokEstTotal:     bm.tokEstTotal,
	}
}

// ─── Engine metrics (the backends' own /metrics) ──────────────────────────
//
// The router measures demand at admission; the engine measures execution.
// In-engine running/waiting, KV-cache pressure, and REAL token throughput
// (prompt + completion — not the ~4 bytes/token body estimate) only exist
// inside the engine, so we scrape each backend's Prometheus /metrics with a
// stdlib text-exposition parser at 1s. vLLM always serves /metrics; SGLang
// requires --enable-metrics. When the endpoint is absent or down the view
// degrades to status "off"/"err" and the sheet renders "—" — never a guess.
//
// Aggregation rules (from the engines' source, verified 2026-08-20):
//   - vLLM gauges are labeled [model_name, engine]; request counts SUM
//     across data-parallel engines, KV usage is per-pool so MAX.
//   - SGLang scheduler gauges are per tp/pp/moe rank (multiprocess
//     mostrecent) and replicate the same batch state — MAX across ranks.
//   - Token counters are rate-differenced between scrapes; a counter reset
//     (engine restart) re-baselines instead of going negative.
//   - TTFT mean comes from the histogram's _sum/_count deltas.
//   - SGLang realtime_tokens_total{mode}: prefill = prefill_compute +
//     prefill_cache, decode = decode.

// engineView is the per-backend engine truth published in the frame.
// Zero + status != "ok" means "no data" — the client renders "—".
type engineView struct {
	Running     float64 `json:"running"`
	Waiting     float64 `json:"waiting"`
	KVPct       float64 `json:"kvPct"` // 0..100
	PrefillTokS float64 `json:"prefillTokS"`
	DecodeTokS  float64 `json:"decodeTokS"`
	TTFTms      float64 `json:"ttftMs"`
	Status      string  `json:"status"` // "ok" | "off" | "err"
}

// promSample is one line of a Prometheus text exposition.
type promSample struct {
	name   string
	labels map[string]string
	value  float64
}

// histState tracks the previous _sum/_count pair for one histogram.
type histState struct {
	sum, cnt         float64
	t                time.Time
	lastSum, lastCnt float64
}

type engineScrape struct {
	url    string
	status string
	view   engineView
	// counters: rate-differenced between scrapes, keyed by name+labels.
	counters map[string]float64
	cTimes   map[string]time.Time
	// histograms: only the ones the dashboard reads.
	hists   map[string]*histState
	lastT   time.Time
	seenEng map[string]bool
}

type engineRegistry struct {
	mu       sync.Mutex
	backends map[string]*engineScrape
	client   *http.Client
}

func newEngineRegistry() *engineRegistry {
	return &engineRegistry{
		backends: map[string]*engineScrape{},
		client:   &http.Client{Timeout: 2 * time.Second},
	}
}

func (e *engineRegistry) add(name, url string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.backends[name] = &engineScrape{
		url:      url,
		status:   "off",
		counters: map[string]float64{},
		cTimes:   map[string]time.Time{},
		hists: map[string]*histState{
			"vllm:time_to_first_token_seconds":   {},
			"sglang:time_to_first_token_seconds": {},
		},
		seenEng: map[string]bool{},
	}
}

func (e *engineRegistry) view(backend string) engineView {
	e.mu.Lock()
	defer e.mu.Unlock()
	if s, ok := e.backends[backend]; ok {
		return s.view
	}
	return engineView{}
}

// loop scrapes every backend every second until the process exits.
func (e *engineRegistry) loop() {
	ticker := time.NewTicker(time.Second)
	for range ticker.C {
		e.mu.Lock()
		names := make([]string, 0, len(e.backends))
		for n := range e.backends {
			names = append(names, n)
		}
		e.mu.Unlock()
		for _, n := range names {
			e.scrape(n)
		}
	}
}

func (e *engineRegistry) scrape(name string) {
	e.mu.Lock()
	s, ok := e.backends[name]
	if !ok {
		e.mu.Unlock()
		return
	}
	url := s.url
	e.mu.Unlock()

	body, gone, ok := func() ([]byte, bool, bool) {
		resp, err := e.client.Get(url)
		if err != nil {
			return nil, false, false
		}
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed {
			return nil, true, false
		}
		if resp.StatusCode != http.StatusOK {
			return nil, false, false
		}
		b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		if err != nil {
			return nil, false, false
		}
		return b, false, true
	}()

	e.mu.Lock()
	defer e.mu.Unlock()
	if !ok {
		if gone {
			s.status = "off"
		} else {
			s.status = "err"
		}
		s.view.PrefillTokS = 0
		s.view.DecodeTokS = 0
		return
	}
	s.scrapeBody(parsePromText(body))
}

// scrapeBody folds one parsed exposition into the scrape state.
func (s *engineScrape) scrapeBody(samples []promSample) {
	now := time.Now()
	view := s.view

	// First pass: family detection + gauge accumulation.
	vllm, sg := false, false
	var running, waiting float64
	var kvMax float64
	haveKV := false
	for _, sm := range samples {
		switch {
		case strings.HasPrefix(sm.name, "vllm:"):
			vllm = true
			switch sm.name {
			case "vllm:num_requests_running":
				running += sm.value // sum across data-parallel engines
			case "vllm:num_requests_waiting":
				waiting += sm.value
			case "vllm:kv_cache_usage_perc":
				if sm.value > kvMax {
					kvMax = sm.value
					haveKV = true
				}
			}
		case strings.HasPrefix(sm.name, "sglang:"):
			sg = true
			switch sm.name {
			case "sglang:num_running_reqs":
				if sm.value > running {
					running = sm.value
				}
			case "sglang:num_queue_reqs":
				if sm.value > waiting {
					waiting = sm.value
				}
			case "sglang:token_usage":
				if sm.value > kvMax {
					kvMax = sm.value
					haveKV = true
				}
			}
		}
	}
	if len(samples) == 0 {
		s.status = "err"
		return
	}
	s.status = "ok"
	s.view = engineView{Status: "ok"}
	view = s.view
	if vllm {
		s.seenEng["vllm"] = true
		view.Running = running
		view.Waiting = waiting
	}
	if sg && !vllm {
		s.seenEng["sglang"] = true
		view.Running = running
		view.Waiting = waiting
	}
	if haveKV {
		view.KVPct = kvMax * 100
	}

	// Second pass: token counters + TTFT histograms, in the detected family.
	view.PrefillTokS = 0
	view.DecodeTokS = 0
	view.TTFTms = 0
	if vllm {
		for _, sm := range samples {
			switch sm.name {
			case "vllm:prompt_tokens_total":
				view.PrefillTokS += s.counterRate(sm, now)
			case "vllm:generation_tokens_total":
				view.DecodeTokS += s.counterRate(sm, now)
			}
		}
		s.updateHist("vllm:time_to_first_token_seconds", samples, now)
		view.TTFTms = s.histMean("vllm:time_to_first_token_seconds")
	} else if sg {
		for _, sm := range samples {
			if sm.name == "sglang:realtime_tokens_total" {
				switch sm.labels["mode"] {
				case "prefill_compute", "prefill_cache":
					view.PrefillTokS += s.counterRate(sm, now)
				case "decode":
					view.DecodeTokS += s.counterRate(sm, now)
				}
			}
		}
		s.updateHist("sglang:time_to_first_token_seconds", samples, now)
		view.TTFTms = s.histMean("sglang:time_to_first_token_seconds")
	}
	s.lastT = now
	s.view = view
}

// counterKey distinguishes samples of the same metric with different
// labels (mode, is_streaming, engine, rank...).
func counterKey(sm promSample) string {
	keys := make([]string, 0, len(sm.labels))
	for k := range sm.labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(sm.name)
	for _, k := range keys {
		b.WriteByte('\x00')
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(sm.labels[k])
	}
	return b.String()
}

// counterRate returns Δ/Δt for one counter sample. First sight and counter
// resets (engine restart) re-baseline: rate 0 for this scrape.
func (s *engineScrape) counterRate(sm promSample, now time.Time) float64 {
	k := counterKey(sm)
	rate := 0.0
	if prev, ok := s.counters[k]; ok {
		if dt := now.Sub(s.cTimes[k]).Seconds(); dt > 0 {
			if sm.value >= prev {
				rate = (sm.value - prev) / dt
			} else {
				rate = 0 // reset: re-baseline below
			}
		}
	}
	s.counters[k] = sm.value
	s.cTimes[k] = now
	return rate
}

// updateHist folds _sum/_count samples into one histogram's state. All
// label sets of the same histogram are summed, so multi-engine/multi-rank
// instances produce one true mean.
func (s *engineScrape) updateHist(base string, samples []promSample, now time.Time) {
	h, ok := s.hists[base]
	if !ok {
		return
	}
	var sum, cnt float64
	for _, sm := range samples {
		if sm.name == base+"_sum" {
			sum += sm.value
		} else if sm.name == base+"_count" {
			cnt += sm.value
		}
	}
	// A histogram with no observations yet emits no lines — skip.
	if sum == 0 && cnt == 0 {
		return
	}
	if h.t.IsZero() {
		h.sum, h.cnt = sum, cnt
	} else if sum >= h.sum && cnt >= h.cnt {
		if d := cnt - h.cnt; d > 0 {
			h.lastSum = (sum - h.sum) / d
		}
		h.sum, h.cnt = sum, cnt
	} else {
		h.sum, h.cnt = sum, cnt // reset: re-baseline
	}
	h.t = now
}

// histMean is the mean (ms) over the most recent scrape interval.
func (s *engineScrape) histMean(base string) float64 {
	if h, ok := s.hists[base]; ok {
		return h.lastSum * 1000
	}
	return 0
}

// ─── Prometheus text parser (stdlib, dashboard needs only) ────────────────

// parsePromText parses the Prometheus text exposition format well enough
// for gauges, counters, and histogram _sum/_count parts. HELP/TYPE lines,
// openmetrics timestamps, and unparseable lines are skipped.
func parsePromText(body []byte) []promSample {
	var out []promSample
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimRight(line, " \r")
		if line == "" || line[0] == '#' {
			continue
		}
		// Value is the last whitespace-delimited token; drop an optional
		// trailing epoch-seconds timestamp first.
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		val := fields[len(fields)-1]
		head := fields[0]
		// Some collectors emit "name{labels} value ts" — head already has
		// the labels; value is the token after it.
		if len(fields) >= 3 && strings.ContainsAny(head, "{}") {
			val = fields[len(fields)-2]
		}
		v, err := strconv.ParseFloat(val, 64)
		if err != nil {
			continue
		}
		name, labels, ok := splitPromHead(head)
		if !ok {
			continue
		}
		out = append(out, promSample{name: name, labels: labels, value: v})
	}
	return out
}

// splitPromHead splits "name{k=\"v\",...}" into name and labels.
func splitPromHead(head string) (string, map[string]string, bool) {
	brace := strings.IndexByte(head, '{')
	if brace < 0 {
		if head == "" || strings.ContainsAny(head, " \t") {
			return "", nil, false
		}
		return head, nil, true
	}
	name := head[:brace]
	if name == "" || strings.ContainsAny(name, " \t") {
		return "", nil, false
	}
	inner := head[brace+1:]
	if !strings.HasSuffix(inner, "}") {
		return "", nil, false
	}
	inner = inner[:len(inner)-1]
	labels := map[string]string{}
	if inner != "" {
		rest := inner
		for {
			eq := strings.IndexByte(rest, '=')
			if eq <= 0 {
				return "", nil, false
			}
			key := rest[:eq]
			rest = rest[eq+1:]
			if len(rest) < 2 || rest[0] != '"' {
				return "", nil, false
			}
			var sb strings.Builder
			closed := false
			for j := 1; j < len(rest); j++ {
				c := rest[j]
				if c == '\\' && j+1 < len(rest) {
					j++
					sb.WriteByte(rest[j])
					continue
				}
				if c == '"' {
					rest = rest[j+1:]
					closed = true
					break
				}
				sb.WriteByte(c)
			}
			if !closed {
				return "", nil, false
			}
			labels[key] = sb.String()
			if rest == "" {
				break
			}
			if !strings.HasPrefix(rest, ",") {
				return "", nil, false
			}
			rest = rest[1:]
			if rest == "" {
				break
			}
		}
	}
	return name, labels, true
}

// ─── Global state ─────────────────────────────────────────────────────────

var (
	backends            []Backend
	client              = &http.Client{Timeout: 30 * time.Second}
	slots               map[string]*slotManager // per-backend concurrency slots
	prefillSlots        map[string]*slotManager // per-backend large-prefill slots (released on first response byte)
	gpu                 *gpuBudget
	queueTimeout                                    = 30 * time.Second
	maxBodyBytes        int64                       = 16 << 20
	prefillTokensPerSec int                         = 10000
	durations           map[string]*durationTracker // per backend name
	proxies             map[string]*httputil.ReverseProxy
	engineMetrics       = newEngineRegistry() // per-backend /metrics scrape
	proxyTransport      = &http.Transport{
		ResponseHeaderTimeout: 5 * time.Minute,
		IdleConnTimeout:       60 * time.Second,
		MaxIdleConns:          256,
		MaxIdleConnsPerHost:   64,
		DisableCompression:    true,
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

// buildMux assembles the router's route table. /v1/ is the proxy prefix;
// management routes are registered before it. A future web.Handler() would
// be mounted at "/" by the caller, after this mux is built.
func buildMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "vLLM router OK")
	})
	mux.HandleFunc("/stats", handleStats)
	mux.HandleFunc("/v1/models", handleModels)
	mux.HandleFunc("/metrics/stream", handleMetricsStream)
	mux.HandleFunc("/metrics/burst", handleBurst)
	mux.HandleFunc("/v1/", handleProxy)
	return mux
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
	// The engine's own /metrics: real in-engine running/waiting, KV pressure,
	// and prompt+completion token rates. vLLM serves it always; SGLang only
	// with --enable-metrics (the scrape degrades to "off" when absent).
	for _, b := range backends {
		engineMetrics.add(b.Name, b.URL+"/metrics")
	}
	go engineMetrics.loop()

	if totalWeight > 0 {
		maxBudget := 0
		mb := os.Getenv("MAX_GPU_BUDGET")
		if mb != "" {
			v, err := strconv.Atoi(mb)
			if err != nil {
				log.Fatalf("invalid MAX_GPU_BUDGET %q: %v", mb, err)
			}
			maxBudget = v
		}
		if mb == "" {
			maxBudget = 4 // default per docs
		}
		if maxBudget > 0 {
			gpu = &gpuBudget{max: maxBudget}
			gpu.cond = sync.NewCond(&gpu.mu)
			log.Printf("  GPU budget: max=%d (active only when tier-0 in-flight)", maxBudget)
		}
	}

	if mb := os.Getenv("MAX_BODY_BYTES"); mb != "" {
		v, err := strconv.ParseInt(mb, 10, 64)
		if err != nil || v <= 0 {
			log.Fatalf("invalid MAX_BODY_BYTES %q", mb)
		}
		maxBodyBytes = v
	}

	if pts := os.Getenv("PREFILL_TOKENS_PER_SEC"); pts != "" {
		v, err := strconv.Atoi(pts)
		if err != nil || v <= 0 {
			log.Fatalf("invalid PREFILL_TOKENS_PER_SEC %q", pts)
		}
		prefillTokensPerSec = v
	}

	// Build reverse proxies once at startup.
	proxies = make(map[string]*httputil.ReverseProxy)
	for _, b := range backends {
		u, err := url.Parse(b.URL)
		if err != nil {
			log.Fatalf("invalid backend URL for %q: %v", b.Name, err)
		}
		p := httputil.NewSingleHostReverseProxy(u)
		p.FlushInterval = -1 // flush immediately for SSE streaming
		p.Transport = proxyTransport
		name := b.Name
		p.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
			handleProxyError(w, r, err, name)
		}
		proxies[b.Name] = p
	}

	mux := buildMux()

	// Mount the embedded SPA dashboard as the lowest-priority catch-all.
	// buildMux() already registered the more specific management and proxy
	// routes (/health, /stats, /v1/models, /metrics/*, /v1/); web.Handler()
	// 404s any of those that reach it, so nothing is shadowed. Without the
	// webui build tag web.Handler() is a 404 stub, so this is a no-op there.
	mux.Handle("/", web.Handler())

	// Refresh per-backend + fleet rates for the live metrics feed. The
	// actual elapsed time between ticks feeds the window decay, so a
	// scheduler stall does not under-count the interval.
	go func() {
		ticker := time.NewTicker(500 * time.Millisecond)
		last := time.Now()
		for range ticker.C {
			now := time.Now()
			metrics.computeRates(now.Sub(last))
			last = now
		}
	}()

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

// estimateNewTokens estimates NEW prefill tokens from the parsed body.
// Multi-turn (>2 messages): only the last message is new input; prior context
// is assumed cached (radix/prefix cache). ≤2 messages: all content is new.
// Completions API: the prompt field. ~4 bytes per token (conservative for Qwen).
func estimateNewTokens(b *parsedBody) int {
	if len(b.Messages) > 0 {
		chars := 0
		if len(b.Messages) <= 2 {
			for _, m := range b.Messages {
				chars += rawContentLen(m.Content)
			}
		} else {
			chars = rawContentLen(b.Messages[len(b.Messages)-1].Content)
		}
		return chars / 4
	}
	return rawContentLen(b.Prompt) / 4
}

// rawContentLen measures character content from raw JSON without decoding it.
// String: bytes minus the two quotes (escape sequences keep encoded length —
// slight overestimate, which errs toward throttling, the safe direction).
// Array: sum of each part's "text" field. Anything else: 0.
func rawContentLen(raw json.RawMessage) int {
	if len(raw) < 2 {
		return 0
	}
	if raw[0] == '"' {
		return len(raw) - 2
	}
	if raw[0] == '[' {
		var parts []struct {
			Text json.RawMessage `json:"text"`
		}
		if json.Unmarshal(raw, &parts) != nil {
			return 0
		}
		total := 0
		for _, p := range parts {
			if len(p.Text) >= 2 {
				total += len(p.Text) - 2
			}
		}
		return total
	}
	return 0
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

// ─── /metrics/stream (SSE) + /metrics/burst ───────────────────────────────

type backendFrame struct {
	Name            string     `json:"name"`
	URL             string     `json:"url"`
	Tier            int        `json:"tier"`
	InFlight        int32      `json:"inFlight"`
	MaxConcurrent   int32      `json:"maxConcurrent"`
	Waiting         int32      `json:"waiting"`
	MaxQueueDepth   int32      `json:"maxQueueDepth"`
	PrefillInFlight int32      `json:"prefillInFlight"`
	PrefillWaiting  int32      `json:"prefillWaiting"`
	PrefillMax      int32      `json:"prefillMax"`
	AvgDurationS    float64    `json:"avgDurationS"`
	EWMAms          float64    `json:"ewmaMs"`
	TTFTms          float64    `json:"ttftMs"`
	TTFTSampleCount int        `json:"ttftSampleCount"`
	TTFTMsNow       float64    `json:"ttftMsNow"`
	ReqRate         float64    `json:"reqRate"`
	BytesRate       float64    `json:"bytesRate"`
	TokEstRate      float64    `json:"tokEstRate"`
	ReqTotal        int64      `json:"reqTotal"`
	BytesInTotal    int64      `json:"bytesInTotal"`
	BytesOutTotal   int64      `json:"bytesOutTotal"`
	TokEstTotal     int64      `json:"tokEstTotal"`
	Engine          engineView `json:"engine"`
}

type gpuFrame struct {
	Used    int  `json:"used"`
	Budget  int  `json:"budget"`
	Waiting int  `json:"waiting"`
	Active  bool `json:"active"`
}

type totalsFrame struct {
	InFlight      int32   `json:"inFlight"`
	Waiting       int32   `json:"waiting"`
	ReqRate       float64 `json:"reqRate"`
	BytesRate     float64 `json:"bytesRate"`
	TokEstRate    float64 `json:"tokEstRate"`
	ReqTotal      int64   `json:"reqTotal"`
	BytesInTotal  int64   `json:"bytesInTotal"`
	BytesOutTotal int64   `json:"bytesOutTotal"`
	TokEstTotal   int64   `json:"tokEstTotal"`
}

type metricsFrame struct {
	T             int64          `json:"t"`
	Backends      []backendFrame `json:"backends"`
	GPU           gpuFrame       `json:"gpu"`
	Tier0Inflight int32          `json:"tier0Inflight"`
	Totals        totalsFrame    `json:"totals"`
}

// buildMetricsFrame assembles one full live snapshot per the live metrics
// contract.
func buildMetricsFrame() metricsFrame {
	f := metricsFrame{T: time.Now().UnixMilli(), Backends: make([]backendFrame, 0, len(backends))}
	f.Tier0Inflight = tier0Inflight()

	for _, b := range backends {
		bf := backendFrame{
			Name:         b.Name,
			URL:          b.URL,
			Tier:         b.Tier,
			AvgDurationS: durations[b.Name].avgSeconds(),
		}
		if s, ok := slots[b.Name]; ok {
			bf.InFlight, bf.MaxConcurrent, bf.Waiting, bf.MaxQueueDepth = s.snapshot()
		}
		if s, ok := prefillSlots[b.Name]; ok {
			bf.PrefillInFlight, bf.PrefillMax, bf.PrefillWaiting, _ = s.snapshot()
		}
		ms := metrics.snapshot(b.Name)
		bf.EWMAms = bf.AvgDurationS * 1000
		bf.TTFTms = ms.TTFTMs
		bf.TTFTSampleCount = ms.TTFTSampleCount
		bf.TTFTMsNow = ms.TTFTMsNow
		bf.ReqRate = ms.ReqRate
		bf.BytesRate = ms.BytesRate
		bf.TokEstRate = ms.TokEstRate
		bf.ReqTotal = ms.ReqTotal
		bf.BytesInTotal = ms.BytesInTotal
		bf.BytesOutTotal = ms.BytesOutTotal
		bf.TokEstTotal = ms.TokEstTotal
		bf.Engine = engineMetrics.view(b.Name)
		f.Backends = append(f.Backends, bf)

		f.Totals.InFlight += bf.InFlight
		f.Totals.Waiting += bf.Waiting
		f.Totals.ReqRate += ms.ReqRate
		f.Totals.BytesRate += ms.BytesRate
		f.Totals.TokEstRate += ms.TokEstRate
		f.Totals.ReqTotal += ms.ReqTotal
		f.Totals.BytesInTotal += ms.BytesInTotal
		f.Totals.BytesOutTotal += ms.BytesOutTotal
		f.Totals.TokEstTotal += ms.TokEstTotal
	}

	// Active means the GPU budget mechanism is engaged: a budget exists AND
	// tier-0 has in-flight requests. No budget configured → active false.
	f.GPU.Active = gpu != nil && f.Tier0Inflight > 0
	if gpu != nil {
		f.GPU.Used, f.GPU.Budget, f.GPU.Waiting = gpu.snapshot()
	}

	return f
}

// handleMetricsStream pushes one JSON snapshot per SSE data frame every
// interval (default 500ms, clamped 200ms..10s) until the client disconnects.
func handleMetricsStream(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	interval := 500 * time.Millisecond
	if v := r.URL.Query().Get("interval"); v != "" {
		d, err := time.ParseDuration(v)
		if err == nil {
			interval = d
		}
	}
	if interval < 200*time.Millisecond {
		interval = 200 * time.Millisecond
	}
	if interval > 10*time.Second {
		interval = 10 * time.Second
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		frame := buildMetricsFrame()
		buf, err := json.Marshal(frame)
		if err != nil {
			return
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", buf); err != nil {
			return
		}
		fl.Flush()
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
		}
	}
}

type burstRequestFrame struct {
	T            int64   `json:"t"`
	Backend      string  `json:"backend"`
	Path         string  `json:"path"`
	Status       int     `json:"status"`
	Stream       bool    `json:"stream"`
	DurMs        float64 `json:"durMs"`
	TTFTms       float64 `json:"ttftMs"`
	NewTokensEst int     `json:"newTokensEst"`
	BytesIn      int64   `json:"bytesIn"`
	BytesOut     int64   `json:"bytesOut"`
}

// handleBurst serves the recent-request ring as {"requests":[...]} (newest
// last), ?limit default 500, max 1000.
func handleBurst(w http.ResponseWriter, r *http.Request) {
	limit := 500
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > 1000 {
		limit = 1000
	}
	samples := metrics.burst(limit)
	out := make([]burstRequestFrame, 0, len(samples))
	for _, s := range samples {
		out = append(out, burstRequestFrame{
			T:            s.T.UnixMilli(),
			Backend:      s.Backend,
			Path:         s.Path,
			Status:       s.Status,
			Stream:       s.Stream,
			DurMs:        s.DurMs,
			TTFTms:       s.TTFTMs,
			NewTokensEst: s.NewTokensEst,
			BytesIn:      s.BytesIn,
			BytesOut:     s.BytesOut,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"requests": out})
}

// ─── Proxy ────────────────────────────────────────────────────────────────

// handleProxyError converts upstream transport failures into clean,
// OpenAI-style JSON errors instead of Go's bare "http: proxy error" 502.
//
// Classification:
//   - client canceled: log quietly; nothing can be written
//   - response already started (engine died mid-stream): the status line is
//     fixed; log and end the stream
//   - transport failure (dial refused/reset, EOF, timeout): 503
//     backend_unavailable + Retry-After — the engine is down or restarting;
//     the request never reached it, so retrying is safe
//   - anything else (e.g. malformed upstream response): 502 bad_gateway
func handleProxyError(w http.ResponseWriter, r *http.Request, err error, backend string) {
	if errors.Is(err, context.Canceled) {
		log.Printf("%s %s -> %s client canceled during upstream roundtrip", r.Method, r.URL.Path, backend)
		return
	}
	if t, ok := w.(*respTracker); ok && t.wrote {
		log.Printf("%s %s -> %s upstream failed mid-response: %v", r.Method, r.URL.Path, backend, err)
		return
	}
	status := http.StatusBadGateway
	code := "bad_gateway"
	var opErr *net.OpError
	if errors.As(err, &opErr) || errors.Is(err, io.EOF) || errors.Is(err, os.ErrDeadlineExceeded) {
		status = http.StatusServiceUnavailable
		code = "backend_unavailable"
	}
	log.Printf("%s %s -> %s %d %s: %v", r.Method, r.URL.Path, backend, status, code, err)
	// The request never produced an upstream response byte, so the sample
	// carries TTFT 0 and BytesOut 0. Record now (before the error body is
	// written through the tracker) so the error body doesn't pollute bytesOut.
	if t, ok := w.(*respTracker); ok {
		t.status = status
		t.finish(backend, r.URL.Path, status, 0, 0)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Retry-After", "5")
	w.Header().Set("X-Router-Reason", code)
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"message": fmt.Sprintf("backend %q unavailable (engine down or restarting): %v", backend, err),
			"type":    "server_error",
			"code":    code,
		},
	})
}

func handleProxy(w http.ResponseWriter, r *http.Request) {
	reqStart := time.Now()
	if !strings.HasPrefix(r.URL.Path, "/v1/") {
		http.NotFound(w, r)
		return
	}
	// Production: unreachable (main fatals on an empty BACKENDS); tests swap
	// the global backends slice, so a request racing teardown gets a clean
	// 503 instead of an index panic.
	if len(backends) == 0 {
		http.Error(w, "no backends configured", http.StatusServiceUnavailable)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			w.Header().Set("X-Router-Reason", "body-too-large")
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			// Model is unknown at this point; attribute to the default backend.
			metrics.recordRequest(backends[0].Name, requestSample{
				T:       time.Now(),
				Backend: backends[0].Name,
				Path:    r.URL.Path,
				Status:  http.StatusRequestEntityTooLarge,
				DurMs:   float64(time.Since(reqStart) / time.Millisecond),
				BytesIn: int64(len(body)),
			})
			return
		}
		http.Error(w, "failed to read request body", http.StatusBadRequest)
		return
	}
	r.Body.Close()

	// Resolve backend from model name.
	target := backends[0]
	var parsed parsedBody
	if len(body) > 0 {
		// Error tolerated: zero value keeps modelName "" and routes to backends[0],
		// identical to today's behavior on unmarshal failure.
		json.Unmarshal(body, &parsed)
	}
	modelName := parsed.Model
	if modelName != "" {
		for _, b := range backends {
			if b.Name == modelName {
				target = b
				break
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

	// Estimated NEW prefill tokens (request-body based, ~4 bytes/token).
	// Computed once, before any admission decision: the large-prefill
	// throttle and metrics both use this value.
	newTokens := estimateNewTokens(&parsed)
	reqContext := requestSample{
		T:            time.Now(),
		Backend:      target.Name,
		Path:         r.URL.Path,
		NewTokensEst: newTokens,
	}
	recordRejection := func(status int) {
		reqContext.Status = status
		reqContext.DurMs = float64(time.Since(reqStart) / time.Millisecond)
		metrics.recordRequest(reqContext.Backend, reqContext)
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
		recordRejection(http.StatusTooManyRequests)
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
				recordRejection(http.StatusTooManyRequests)
				return
			}
		} else {
			if !gpu.tryAcquire(target.GpuWeight, 4, 5*time.Second, cancelCh) {
				retry := estimateRetryAfter(target.Name)
				log.Printf("%s %s -> %s (model=%s) 429: GPU budget full (race)", r.Method, r.URL.Path, target.Name, modelName)
				write429(w, "gpu-budget-full", target.Name, retry)
				recordRejection(http.StatusTooManyRequests)
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
			recordRejection(http.StatusTooManyRequests)
			return
		}
		defer sem.release()
	}

	// ── Step 3.5: Large prefill throttle ─────────────────────────────────
	// Limits how many concurrent large prefills a backend processes. A large
	// prefill is one whose estimated NEW tokens (last message only, not cached
	// context) exceed the backend's threshold. The slot is released on the
	// first response byte for streaming requests, or after the estimated
	// prefill duration (newTokens / PREFILL_TOKENS_PER_SEC) for non-streaming
	// requests. Small requests skip this entirely. Soft-queued up to maxQueue,
	// then 429.
	prefillSem := prefillSlots[target.Name]
	prefillRelease := func() {} // no-op by default
	if prefillSem != nil {
		threshold := target.LargePrefillThresholdTokens
		if threshold == 0 {
			threshold = 8192
		}
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
				recordRejection(http.StatusTooManyRequests)
				return
			}
			var prefillOnce sync.Once
			prefillRelease = func() { prefillOnce.Do(prefillSem.release) }
			if !parsed.Stream {
				// Non-streaming: response headers arrive only after full
				// generation, so first-byte release would hold the slot through
				// all of decode. Bound the hold to the estimated prefill duration;
				// whichever fires first (timer or first byte) wins via the Once.
				hold := time.Duration(float64(newTokens) / float64(prefillTokensPerSec) * float64(time.Second))
				timer := time.AfterFunc(hold, prefillRelease)
				defer timer.Stop()
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

	// Duration + TTFT are measured from here (after admission/queue wait),
	// matching the existing per-backend EWMA semantics. Rejections record
	// queue-wait time from reqStart instead.
	start := time.Now()
	// The request is now admitted: its demand enters the live-rate window.
	// Completion retires it again via the accepted flag.
	reqContext.accepted = true
	metrics.recordAccept(target.Name, int64(len(body)), newTokens)
	proxy := proxies[target.Name]
	// Wrap ResponseWriter to capture status + detect completion for duration
	// tracking. Also releases the prefill slot on first response byte.
	tracker := &respTracker{
		ResponseWriter: w,
		status:         200,
		onFirstByte:    prefillRelease,
		backend:        target.Name,
		ttftStart:      start,
		reqStart:       start,
		stream:         parsed.Stream,
		newTokensEst:   newTokens,
		bytesIn:        int64(len(body)),
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))

	proxy.ServeHTTP(tracker, r)

	prefillRelease() // fallback: release if no response was ever written
	durations[target.Name].record(time.Since(start))

	// TTFT is only meaningful for streaming requests (time to first byte).
	var ttftMs float64
	if parsed.Stream && !tracker.firstByteAt.IsZero() {
		ttftMs = float64(tracker.firstByteAt.Sub(start) / time.Millisecond)
	}
	tracker.finish(target.Name, r.URL.Path, tracker.status, ttftMs, tracker.bytesOut)
}

type respTracker struct {
	http.ResponseWriter
	status      int
	wrote       bool
	onFirstByte func() // called on first WriteHeader/Write; used to release prefill slot
	// firstByteAt: time of the first response byte; if only WriteHeader
	// ever happens (no body byte), the WriteHeader time is the fallback.
	firstByteAt time.Time
	bodyByte    bool // a Write with n>0 has happened
	bytesOut    int64

	// Request context for metrics recording (set at construction).
	backend      string
	ttftStart    time.Time // admission time; TTFT is first-byte minus this
	reqStart     time.Time
	stream       bool
	newTokensEst int
	bytesIn      int64
	ttftSent     bool // first-byte TTFT already recorded (once)
	recorded     bool // finish guard: exactly one sample per request
}

func (t *respTracker) WriteHeader(code int) {
	if !t.wrote {
		t.status = code
		t.wrote = true
		if !t.bodyByte {
			t.firstByteAt = time.Now() // fallback; first body byte overrides
		}
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
	n, err := t.ResponseWriter.Write(b)
	if n > 0 {
		if !t.bodyByte {
			t.firstByteAt = time.Now()
			t.bodyByte = true
			// TTFT is live truth: record it now, while the request is still
			// decoding, so the dashboard sees it the moment it happens.
			if t.stream && !t.ttftSent {
				t.ttftSent = true
				metrics.recordFirstByte(t.backend, float64(t.firstByteAt.Sub(t.ttftStart)/time.Millisecond))
			}
		}
		t.bytesOut += int64(n)
	}
	return n, err
}

// finish records the metrics sample for this request exactly once. Called
// from the normal completion path (real status, TTFT, bytesOut) and from
// handleProxyError when it itself wrote a 502/503 (TTFTMs 0, BytesOut 0).
func (t *respTracker) finish(backend, path string, status int, ttftMs float64, bytesOut int64) {
	if t.recorded {
		return
	}
	t.recorded = true
	metrics.recordRequest(backend, requestSample{
		T:            time.Now(),
		Backend:      backend,
		Path:         path,
		Status:       status,
		Stream:       t.stream,
		DurMs:        float64(time.Since(t.reqStart) / time.Millisecond),
		TTFTMs:       ttftMs,
		NewTokensEst: t.newTokensEst,
		BytesIn:      t.bytesIn,
		BytesOut:     bytesOut,
	})
}

func (t *respTracker) Flush() {
	if f, ok := t.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
