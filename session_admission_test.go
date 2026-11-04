package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// ─── Manual admission clock ───────────────────────────────────────────────
//
// AfterFunc records a deadline; callbacks run only when a test calls
// Advance (deadline order; insertion order for ties), including callbacks
// scheduled during the advancement. No auto-fire goroutines: a test that
// moves on leaves no manager timer callbacks queued that can outlive it.

type saManualClock struct {
	mu      sync.Mutex
	now     time.Time
	nextID  int
	pending []*saManualTimer
}

type saManualTimer struct {
	clock    *saManualClock
	id       int
	deadline time.Time
	fn       func()
	stopped  bool
	fired    bool
}

func newSAManualClock() *saManualClock {
	return &saManualClock{now: time.UnixMilli(1_750_000_000_000)}
}

func (c *saManualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *saManualClock) AfterFunc(d time.Duration, fn func()) admissionTimer {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nextID++
	tl := &saManualTimer{clock: c, id: c.nextID, deadline: c.now.Add(d), fn: fn}
	c.pending = append(c.pending, tl)
	return tl
}

func (tl *saManualTimer) Stop() bool {
	tl.clock.mu.Lock()
	defer tl.clock.mu.Unlock()
	tl.stopped = true
	return true
}

// Advance fires every due callback in deadline order, including callbacks
// scheduled by earlier callbacks during this call. Callbacks run outside
// the clock mutex.
func (c *saManualClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
	for {
		c.mu.Lock()
		var due *saManualTimer
		for _, p := range c.pending {
			if p.stopped || p.fired || p.deadline.After(c.now) {
				continue
			}
			if due == nil || p.deadline.Before(due.deadline) ||
				(p.deadline.Equal(due.deadline) && p.id < due.id) {
				due = p
			}
		}
		if due == nil {
			c.mu.Unlock()
			return
		}
		due.fired = true
		c.mu.Unlock()
		due.fn()
	}
}

// ─── Shared helpers ───────────────────────────────────────────────────────

// withAdmissionEnv must be called AFTER withRouterEnv: it enables
// per-backend admission managers on top of the router globals and restores
// the disabled state on cleanup.
func withAdmissionEnv(t *testing.T, ttl time.Duration, clock admissionClock, maxAdmitted, maxWaiting int) {
	t.Helper()
	savedSettings := sessionAdmissionSettings
	savedManagers := sessionAdmissions
	sessionAdmissionSettings = sessionAdmissionConfig{
		Enabled:     true,
		MaxAdmitted: maxAdmitted,
		MaxWaiting:  maxWaiting,
		IdleTTL:     ttl,
		KeySource:   "prompt_cache_key",
	}
	sessionAdmissions = make(map[string]*sessionAdmission)
	for _, b := range backends {
		sessionAdmissions[b.Name] = newSessionAdmission(maxAdmitted, maxWaiting, ttl, clock)
	}
	t.Cleanup(func() {
		sessionAdmissionSettings = savedSettings
		sessionAdmissions = savedManagers
	})
}

func saChatBody(model, key, msg string) string {
	if key != "" {
		return fmt.Sprintf(`{"model":%q,"prompt_cache_key":%q,"messages":[{"role":"user","content":%q}]}`, model, key, msg)
	}
	return fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":%q}]}`, model, msg)
}

// saChatStreamBody is saChatBody with stream:true: production releases
// the large-prefill slot on the first response byte, so a fake upstream
// that writes no bytes keeps the slot deterministically gated.
func saChatStreamBody(model, key, msg string) string {
	if key != "" {
		return fmt.Sprintf(`{"model":%q,"prompt_cache_key":%q,"stream":true,"messages":[{"role":"user","content":%q}]}`, model, key, msg)
	}
	return fmt.Sprintf(`{"model":%q,"stream":true,"messages":[{"role":"user","content":%q}]}`, model, msg)
}

// saPostOut carries one POST's outcome; the returned err != nil means the
// transport or the body read failed (no status code exists).
type saPostOut struct {
	code int
	hdr  http.Header
	body string
}

// saPostOutDo is the low-level POST: transport and read errors are
// reported to the CALLER (never t.Fatalf inside a goroutine — that would
// leave saPostAsync callers hanging on the result channel without a test
// failure on the test goroutine).
func saPostOutDo(url, body string) (saPostOut, error) {
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		return saPostOut{}, fmt.Errorf("POST: %w", err)
	}
	defer resp.Body.Close()
	data, rerr := io.ReadAll(resp.Body)
	return saPostOut{code: resp.StatusCode, hdr: resp.Header, body: string(data)}, rerr
}

func saPost(t *testing.T, url, body string) (int, http.Header, string) {
	t.Helper()
	out, err := saPostOutDo(url, body)
	if err != nil {
		t.Fatalf("%v", err)
	}
	return out.code, out.hdr, out.body
}

// saPostAsync issues the request on a goroutine: required for gated
// engines (the upstream response only lands once the test releases the
// gate, so a synchronous POST would deadlock the test). Transport/read
// errors travel through the channel as status 0; the test goroutine
// converts them into t.Fatal (0 never matches an expected 2xx/4xx).
func saPostAsync(t *testing.T, url, body string) chan int {
	t.Helper()
	out := make(chan int, 1)
	go func() {
		res, _ := saPostOutDo(url, body)
		out <- res.code
	}()
	return out
}

// saServing runs the real proxy handler plus /stats on its own listener
// and lets a test await every handler returning before teardown.
type saServing struct {
	ts  *httptest.Server
	mu  sync.Mutex
	cv  *sync.Cond
	cnt int
}

func newSAServing() *saServing {
	s := &saServing{}
	s.cv = sync.NewCond(&s.mu)
	mux := http.NewServeMux()
	mux.HandleFunc("/stats", handleStats)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.cnt++
		s.mu.Unlock()
		// Deferred decrement: handleProxy unwinds via http.ErrAbortHandler
		// when a committed proxy copy is interrupted (client/upstream died
		// mid-stream) — the std server recovers that panic, so the counter
		// must decrement in the abort path too, or awaitIdle hangs.
		defer func() {
			s.mu.Lock()
			s.cnt--
			s.cv.Signal()
			s.mu.Unlock()
		}()
		handleProxy(w, r)
	})
	s.ts = httptest.NewServer(mux)
	return s
}

// saMetricsServing is the serving stack of the REAL management surface:
// handleProxy + buildMetricsMux on one listener, so a test can drive an
// admission state through the actual /metrics/sessions endpoint.
type saMetricsServing struct {
	ts  *httptest.Server
	mu  sync.Mutex
	cv  *sync.Cond
	cnt int
}

func newSAServingMux() *saMetricsServing {
	s := &saMetricsServing{}
	s.cv = sync.NewCond(&s.mu)
	mux := http.NewServeMux()
	mux.HandleFunc("/stats", handleStats)
	mux.Handle("/metrics/", buildMetricsMux())
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.cnt++
		s.mu.Unlock()
		// Deferred decrement — same http.ErrAbortHandler contract as
		// newSAServing.
		defer func() {
			s.mu.Lock()
			s.cnt--
			s.cv.Signal()
			s.mu.Unlock()
		}()
		handleProxy(w, r)
	})
	s.ts = httptest.NewServer(mux)
	return s
}

// saSessionRow fetches the live /metrics/sessions feed and returns the
// row for id ("absent" if the registry no longer knows it).
func (s *saMetricsServing) saSessionRow(t *testing.T, id string) *sessFrame {
	t.Helper()
	resp, err := http.Get(s.ts.URL + "/metrics/sessions")
	if err != nil {
		t.Fatalf("GET /metrics/sessions: %v", err)
	}
	defer resp.Body.Close()
	var feed sessionsFeed
	if err := json.NewDecoder(resp.Body).Decode(&feed); err != nil {
		t.Fatalf("decode /metrics/sessions: %v", err)
	}
	return saAdmRow(feed, id)
}

func (s *saMetricsServing) awaitIdle(t *testing.T, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	s.mu.Lock()
	defer s.mu.Unlock()
	for s.cnt > 0 {
		remaining := deadline.Sub(time.Now())
		if remaining <= 0 {
			t.Fatal("handlers did not drain within timeout")
			return
		}
		notified := make(chan struct{})
		go func() {
			time.Sleep(remaining)
			s.cv.Signal()
			close(notified)
		}()
		s.cv.Wait()
		<-notified
	}
}

func (s *saServing) awaitIdle(t *testing.T, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	s.mu.Lock()
	defer s.mu.Unlock()
	for s.cnt > 0 {
		remaining := deadline.Sub(time.Now())
		if remaining <= 0 {
			t.Fatal("handlers did not drain within timeout")
			return
		}
		notified := make(chan struct{})
		go func() {
			time.Sleep(remaining)
			s.cv.Signal()
			close(notified)
		}()
		s.cv.Wait()
		<-notified
	}
}

// saGateEngine is a fake upstream that records forwarded keys in order and
// blocks each request under a UNIQUE gate (two same-key requests keep
// separate gates), so a test can hold one admitted request while another
// same-key request is released.
type saGateEngine struct {
	TS    *httptest.Server
	mu    sync.Mutex
	seen  []string
	gates []saGate
}

type saGate struct {
	key  string
	wait chan struct{}
}

func newSAGateEngine() *saGateEngine {
	e := &saGateEngine{}
	e.TS = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(body, &m)
		key, _ := m["prompt_cache_key"].(string)
		e.mu.Lock()
		g := saGate{key: key, wait: make(chan struct{})}
		e.gates = append(e.gates, g)
		e.seen = append(e.seen, key)
		e.mu.Unlock()
		select {
		case <-r.Context().Done():
			return
		case <-g.wait:
		}
		if r.Context().Err() != nil {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true}`)
	}))
	return e
}

// releaseKey releases the NEXT (oldest) pending gate for that key.
// Retries briefly: a test may release a gate in the same tick that the
// request is granted admission, before the proxy has reached upstream.
func (e *saGateEngine) releaseKey(key string) {
	deadline := time.Now().Add(2 * time.Second)
	for {
		e.mu.Lock()
		for i := range e.gates {
			if e.gates[i].key == key {
				close(e.gates[i].wait)
				e.gates = append(e.gates[:i], e.gates[i+1:]...)
				e.mu.Unlock()
				return
			}
		}
		e.mu.Unlock()
		if time.Now().After(deadline) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (e *saGateEngine) releaseAll() {
	e.mu.Lock()
	for i := range e.gates {
		close(e.gates[i].wait)
	}
	e.gates = nil
	e.mu.Unlock()
}

func (e *saGateEngine) Seen() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]string, len(e.seen))
	copy(out, e.seen)
	return out
}

// saWaitIdleLease blocks until the per-backend admission manager reports
// the key's lease as idle-held (present, idle-since armed). The idle
// timer is armed by the handler's deferred ticket release, which lands
// strictly AFTER the client reads the response: advancing a manual clock
// before the arm loses the expiry tick entirely. Other requests may be
// mid-flight (queued, gated) — this waits for the timer, not a drain.
func saWaitIdleLease(t *testing.T, backend, key string) {
	t.Helper()
	m := sessionAdmissions[backend]
	deadline := time.Now().Add(2 * time.Second)
	for {
		if held, since := m.snapshot(key); held && !since.IsZero() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("lease %s/%s not idle-held within timeout", backend, key)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// saPollGPU waits for the GPU budget to reach the expected (used,
// waiting) pair: a COMPLETING handler's deferred gpu.release lands
// strictly after the client reads the response, so the transition is
// asynchronous (same contract as saWaitIdleLease, for the budget).
func saPollGPU(t *testing.T, gb *gpuBudget, used, waiting int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if u, _, w := gb.snapshot(); u == used && w == waiting {
			return
		}
		if time.Now().After(deadline) {
			u, _, w := gb.snapshot()
			t.Fatalf("gpu budget = %d/%d, want %d/%d", u, w, used, waiting)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// saPollPrefillSlot waits for the prefill slot state to reach
// (in, waiting): a slot handoff after a release is asynchronous (the
// wake, re-acquire, and forward are separate real-time steps), so a
// fixed sleep after a gate release is a flake window.
func saPollPrefillSlot(t *testing.T, in, waiting int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		pin, _, pw, _ := prefillSlots["smoke"].snapshot()
		if int(pin) == in && int(pw) == waiting {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("prefill slots = %d/%d, want %d/%d", pin, pw, in, waiting)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// saPollEngineSeen polls until the fake upstream has forwarded calls
// in EXACTLY the given key order (oldest first): a deterministic
// replacement for a fixed sleep after a grant — a granted handler
// sits at its engine gate until released, so the recorded order is
// stable once every expected forward has landed.
func saPollEngineSeen(t *testing.T, e *saGateEngine, order ...string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if got := e.Seen(); len(got) == len(order) {
			ok := true
			for i := range order {
				if got[i] != order[i] {
					ok = false
					break
				}
			}
			if ok {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("engine seen = %v, want order %v", e.Seen(), order)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// saPollAdmittedLeases polls until the per-backend manager holds EXACTLY
// `want` admitted leases (len(leaseMap)): the deterministic replacement for a
// fixed sleep after an admission grant — a lease appears only once the
// granted handler's admission lands, so the count is stable once reached.
func saPollAdmittedLeases(t *testing.T, backend string, want int) {
	t.Helper()
	m := sessionAdmissions[backend]
	deadline := time.Now().Add(2 * time.Second)
	for {
		m.mu.Lock()
		got := len(m.leaseMap)
		m.mu.Unlock()
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("admission leases = %d, want %d", got, want)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// saPollAdmissionWaiting polls until the per-backend manager's FIFO
// admission queue holds EXACTLY `want` distinct-waiters (mgr.waiting): the
// deterministic replacement for a fixed sleep after a reject/queue
// transition. waiting counts queued (not yet admitted) requests.
func saPollAdmissionWaiting(t *testing.T, backend string, want int) {
	t.Helper()
	m := sessionAdmissions[backend]
	deadline := time.Now().Add(2 * time.Second)
	for {
		m.mu.Lock()
		got := m.waiting
		m.mu.Unlock()
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("admission waiting = %d, want %d", got, want)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// saIsolate swaps in a fresh registry/metrics for the test.
func saIsolate(t *testing.T) {
	t.Helper()
	oldSessions, oldMetrics := sessions, metrics
	sessions, metrics = newSessionRegistry(), newTestMetrics()
	t.Cleanup(func() { sessions, metrics = oldSessions, oldMetrics })
}

func saAdmKey(t *testing.T, key, backend string) string {
	t.Helper()
	p := &parsedBody{PromptCacheKey: json.RawMessage("\"" + key + "\"")}
	k := admissionKeyFor(p, httptest.NewRequest("POST", "/v1/chat/completions", nil), backend, "prompt_cache_key", "")
	if k == "" {
		t.Fatalf("admissionKeyFor returned empty for %q", key)
	}
	return k
}

func saAdmRow(f sessionsFeed, id string) *sessFrame {
	for i := range f.Sessions {
		if f.Sessions[i].ID == id {
			return &f.Sessions[i]
		}
	}
	return nil
}

// saHTTPSetup wires a real router listener, a gated engine, and admission
// env for backend "smoke" (maxConcurrent 2, queue 4). Cleanup LIFO
// (last registered runs first): drain handlers → release engine gates →
// close the engine server → close the router server.
func saHTTPSetup(t *testing.T, ttl time.Duration, cap, waiting int) (*saServing, *saGateEngine, *saManualClock) {
	t.Helper()
	engine := newSAGateEngine()
	withRouterEnv(t,
		[]Backend{{Name: "smoke", URL: engine.TS.URL, MaxConcurrent: 2, MaxQueueDepth: 4}},
		map[string]int{"smoke": 2}, nil)
	proxies["smoke"] = newTestProxy(engine.TS.URL)
	saIsolate(t)
	c := newSAManualClock()
	withAdmissionEnv(t, ttl, c, cap, waiting)
	s := newSAServing()
	t.Cleanup(func() { s.ts.Close() })
	t.Cleanup(func() { engine.TS.Close() }) // LIFO: after handler drain + gate release
	t.Cleanup(func() { engine.releaseAll() })
	t.Cleanup(func() { s.awaitIdle(t, 5*time.Second) })
	return s, engine, c
}

// ─── 1. Configuration loader ─────────────────────────────────────────────

func saEnv(pairs ...string) func(string) string {
	m := map[string]string{}
	for i := 0; i+1 < len(pairs); i += 2 {
		m[pairs[i]] = pairs[i+1]
	}
	return func(k string) string { return m[k] }
}

func TestSessionAdmission_ConfigLoader(t *testing.T) {
	limited := []Backend{{Name: "l", MaxConcurrent: 3, MaxQueueDepth: 4}}
	unlimited := []Backend{{Name: "u", MaxConcurrent: 0, MaxQueueDepth: 4}}

	t.Run("default-off", func(t *testing.T) {
		cfg, err := loadSessionAdmissionConfig(saEnv(), limited)
		if err != nil || cfg.Enabled {
			t.Fatalf("unset: %+v %v", cfg, err)
		}
		cfg, err = loadSessionAdmissionConfig(saEnv("SESSION_ADMISSION", "off"), unlimited)
		if err != nil || cfg.Enabled {
			t.Fatalf("off: %+v %v", cfg, err)
		}
	})
	t.Run("bad-flag", func(t *testing.T) {
		if _, err := loadSessionAdmissionConfig(saEnv("SESSION_ADMISSION", "banana"), limited); err == nil {
			t.Fatal("want SESSION_ADMISSION error")
		}
	})
	t.Run("default-inherit", func(t *testing.T) {
		// 0 = inherit each backend's maxConcurrent; waiting -1 is the
		// inherit sentinel — per-backend resolution happens in main(),
		// not in the loader.
		cfg, err := loadSessionAdmissionConfig(saEnv("SESSION_ADMISSION", "on"), limited)
		if err != nil || cfg.MaxAdmitted != 0 || cfg.MaxWaiting != -1 ||
			cfg.IdleTTL != 5*time.Minute || cfg.KeySource != "prompt_cache_key" {
			t.Fatalf("cfg = %+v, %v", cfg, err)
		}
	})
	t.Run("negative-inherited-waiting", func(t *testing.T) {
		_, err := loadSessionAdmissionConfig(saEnv("SESSION_ADMISSION", "on"),
			[]Backend{{Name: "n", MaxConcurrent: 2, MaxQueueDepth: -1}})
		if err == nil || !strings.Contains(err.Error(), "invalid SESSION_MAX_WAITING") {
			t.Fatalf("err = %v, want invalid SESSION_MAX_WAITING", err)
		}
	})
	t.Run("explicit-cap-unlimited", func(t *testing.T) {
		cfg, err := loadSessionAdmissionConfig(
			saEnv("SESSION_ADMISSION", "on", "SESSION_MAX_ADMITTED", "7"), unlimited)
		if err != nil || cfg.MaxAdmitted != 7 {
			t.Fatalf("explicit cap = %+v %v", cfg, err)
		}
	})
	t.Run("unlimited-omit", func(t *testing.T) {
		_, err := loadSessionAdmissionConfig(saEnv("SESSION_ADMISSION", "on"), unlimited)
		if err == nil || !strings.Contains(err.Error(), "SESSION_MAX_ADMITTED is required") {
			t.Fatalf("err = %v, want required message", err)
		}
	})
	t.Run("invalid-values", func(t *testing.T) {
		for _, v := range []string{"0", "-2", "abc"} {
			if _, err := loadSessionAdmissionConfig(
				saEnv("SESSION_ADMISSION", "on", "SESSION_MAX_ADMITTED", v), limited); err == nil {
				t.Errorf("SESSION_MAX_ADMITTED=%s: want error", v)
			}
		}
		for _, v := range []string{"-1", "1.5"} {
			if _, err := loadSessionAdmissionConfig(
				saEnv("SESSION_ADMISSION", "on", "SESSION_MAX_WAITING", v), limited); err == nil {
				t.Errorf("SESSION_MAX_WAITING=%s: want error", v)
			}
		}
		cfg, err := loadSessionAdmissionConfig(saEnv("SESSION_ADMISSION", "on", "SESSION_MAX_WAITING", "0"), limited)
		if err != nil || cfg.MaxWaiting != 0 {
			t.Fatalf("waiting=0: %+v %v", cfg, err)
		}
		if _, err := loadSessionAdmissionConfig(
			saEnv("SESSION_ADMISSION", "on", "SESSION_IDLE_TTL", "-5s"), limited); err == nil {
			t.Fatal("negative ttl accepted")
		}
		if _, err := loadSessionAdmissionConfig(
			saEnv("SESSION_ADMISSION", "on", "SESSION_IDLE_TTL", "soon"), limited); err == nil {
			t.Fatal("unparseable ttl accepted")
		}
	})
	t.Run("ttl-explicit", func(t *testing.T) {
		cfg, err := loadSessionAdmissionConfig(
			saEnv("SESSION_ADMISSION", "on", "SESSION_IDLE_TTL", "250ms"), limited)
		if err != nil || cfg.IdleTTL != 250*time.Millisecond {
			t.Fatalf("ttl = %+v %v", cfg, err)
		}
	})
	t.Run("key-source", func(t *testing.T) {
		cfg, err := loadSessionAdmissionConfig(
			saEnv("SESSION_ADMISSION", "on", "SESSION_KEY_SOURCE", "user"), limited)
		if err != nil || cfg.KeySource != "user" {
			t.Fatalf("user: %+v %v", cfg, err)
		}
		cfg, err = loadSessionAdmissionConfig(
			saEnv("SESSION_ADMISSION", "on", "SESSION_KEY_SOURCE", "header:x-conv-id"), limited)
		if err != nil || cfg.KeySource != "header:X-Conv-Id" {
			t.Fatalf("header: %+v %v", cfg, err)
		}
		for _, v := range []string{"header:", "header:bad name", "nope"} {
			if _, err := loadSessionAdmissionConfig(
				saEnv("SESSION_ADMISSION", "on", "SESSION_KEY_SOURCE", v), limited); err == nil {
				t.Errorf("source %s: want error", v)
			}
		}
	})
}

// ─── 2. Identity ──────────────────────────────────────────────────────────

func TestSessionAdmission_Identity(t *testing.T) {
	r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	r.Header.Set("X-Conv-Id", "hv")
	one := &parsedBody{PromptCacheKey: json.RawMessage(`"conv1"`)}

	// prompt_cache_key always wins over configured fallbacks.
	both := &parsedBody{
		PromptCacheKey: json.RawMessage(`"conv1"`),
		User:           json.RawMessage(`"alice"`),
	}
	if k := admissionKeyFor(both, r, "b1", "header:X-Conv-Id", "X-Conv-Id"); k != admissionKeyFor(one, r, "b1", "prompt_cache_key", "") {
		t.Error("prompt_cache_key must win over user and header")
	}

	// user fallback fires only when configured AND primary is unusable.
	userOnly := &parsedBody{User: json.RawMessage(`"alice"`)}
	ku := admissionKeyFor(userOnly, r, "b1", "user", "")
	if ku == "" {
		t.Fatal("user fallback produced no key")
	}
	if admissionKeyFor(userOnly, r, "b1", "prompt_cache_key", "") != "" {
		t.Error("user fallback used under prompt_cache_key source")
	}

	// header fallback uses the standard (case-insensitive) lookup.
	r2 := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	r2.Header.Set("x-conv-id", "hv")
	if admissionKeyFor(&parsedBody{}, r2, "b1", "header:X-Conv-Id", "X-Conv-Id") == "" {
		t.Fatal("header lookup not case-insensitive")
	}

	// Stable across growing history; distinct keys stay distinct for the
	// same messages; the lease key is backend-scoped.
	hist := func(msgs ...string) *parsedBody {
		b := &parsedBody{PromptCacheKey: json.RawMessage(`"conv1"`)}
		for _, m := range msgs {
			b.Messages = append(b.Messages, rawMessage{Role: "user", Content: json.RawMessage("\"" + m + "\"")})
		}
		return b
	}
	k1 := admissionKeyFor(hist("hi"), r, "b1", "prompt_cache_key", "")
	if admissionKeyFor(hist("hi", "turn 2"), r, "b1", "prompt_cache_key", "") != k1 {
		t.Error("identity changed across turns")
	}
	other := &parsedBody{PromptCacheKey: json.RawMessage(`"conv2"`), Messages: hist("hi").Messages}
	if admissionKeyFor(other, r, "b1", "prompt_cache_key", "") == k1 {
		t.Error("identical messages with distinct keys collapsed")
	}
	if admissionKeyFor(hist("hi"), r, "b2", "prompt_cache_key", "") == k1 {
		t.Error("same client key on backend b2 shared b1's lease key")
	}

	// Missing/null/wrong type/empty/whitespace values are unusable.
	for _, raw := range []string{``, `null`, `123`, `""`, `"   "`} {
		b := &parsedBody{}
		if raw != `` {
			b.PromptCacheKey = json.RawMessage(raw)
		}
		if k := admissionKeyFor(b, r, "b1", "user", ""); k != "" {
			t.Errorf("raw %q produced %q, want empty", raw, k)
		}
	}

	// The exact decoded bytes are the identity: no case folding.
	a := admissionKeyFor(&parsedBody{PromptCacheKey: json.RawMessage(`"Key One"`)}, r, "b1", "prompt_cache_key", "")
	b := admissionKeyFor(&parsedBody{PromptCacheKey: json.RawMessage(`"key one"`)}, r, "b1", "prompt_cache_key", "")
	if a == b {
		t.Error("case-folded keys must not collide")
	}
}

// ─── 3. Lease lifecycle against a manual clock (unit) ─────────────────────

func TestSessionAdmission_LeaseLifecycle(t *testing.T) {
	const ttl = 10 * time.Second
	c := newSAManualClock()
	a := newSessionAdmission(2, 2, ttl, c)
	ctx := context.Background()

	tA1, err := a.acquire(ctx, "kA", time.Minute)
	if err != nil {
		t.Fatalf("acquire A1: %v", err)
	}
	tA2, err := a.acquire(ctx, "kA", time.Minute)
	if err != nil {
		t.Fatalf("acquire A2: %v", err)
	}
	if _, err := a.acquire(ctx, "kB", time.Minute); err != nil {
		t.Fatalf("acquire B: %v", err)
	}
	// Two accepted A requests: no expiry while the count is positive,
	// even far past the TTL.
	c.Advance(ttl * 5)
	if held, _ := a.snapshot("kA"); !held {
		t.Fatal("accepted lease expired with a positive count")
	}
	tA2.release()
	c.Advance(ttl * 5)
	if held, _ := a.snapshot("kA"); !held {
		t.Fatal("lease expired while the other A request is still running")
	}

	// Final release: idleness starts exactly here, and the timer alone
	// (no traffic, no metrics poll) expires it after the full TTL.
	tA1.release()
	if held, since := a.snapshot("kA"); !held || since.IsZero() {
		t.Fatalf("A = held %v since %v, want idle held", held, since)
	}
	c.Advance(ttl - time.Millisecond)
	if held, _ := a.snapshot("kA"); !held {
		t.Fatal("A expired before its full idle TTL")
	}
	c.Advance(2 * time.Millisecond)
	if held, _ := a.snapshot("kA"); held {
		t.Fatal("A survived past its full idle TTL")
	}

	// Reactivation restarts the TTL from the new idleness start.
	t2, err := a.acquire(ctx, "kA", time.Minute)
	if err != nil {
		t.Fatalf("reacquire: %v", err)
	}
	t2.release()
	c.Advance(ttl - time.Millisecond)
	if held, _ := a.snapshot("kA"); !held {
		t.Fatal("reactivated lease did not survive the full TTL from re-idleness")
	}
	c.Advance(2 * time.Millisecond)
	if held, _ := a.snapshot("kA"); held {
		t.Fatal("reactivated lease outlived TTL + epsilon")
	}
}

// TestSessionAdmission_ExpiryTimerReleasesWaiters proves the no-demand arm:
// without any queued work an idle lease survives until just before its full
// TTL, then the expiry timer alone (no traffic, no snapshot) frees it. The
// demand arm (a queued newcomer reclaims the idle lease immediately, no
// tick) is TestSessionAdmission_ImmediateReclaim.
func TestSessionAdmission_ExpiryTimerReleasesWaiters(t *testing.T) {
	const ttl = 5 * time.Second
	c := newSAManualClock()
	a := newSessionAdmission(1, 3, ttl, c)
	ctx := context.Background()

	tA, _ := a.acquire(ctx, "kA", time.Minute)
	tA.release() // idle now; no demand, so the full TTL must elapse
	c.Advance(ttl - time.Millisecond)
	if held, _ := a.snapshot("kA"); !held {
		t.Fatal("idle lease expired before its full TTL with no demand")
	}
	// The expiry timer — no new request, no snapshot — frees the slot.
	c.Advance(2 * time.Millisecond)
	if held, _ := a.snapshot("kA"); held {
		t.Fatal("idle lease survived its full TTL")
	}
	a.mu.Lock()
	if a.reclaimedTotal != 0 {
		t.Fatalf("TTL expiry counted as a pressure reclaim: %d", a.reclaimedTotal)
	}
	a.mu.Unlock()
}

// TestSessionAdmission_ImmediateReclaim is the demand arm of idle retention:
// a live new-identity acquisition reclaims the oldest idle lease in the
// same critical section — no clock advance, no tick, no TTL — and only an
// idle (count == 0) lease is ever selected.
func TestSessionAdmission_ImmediateReclaim(t *testing.T) {
	const ttl = 30 * time.Second
	ctx := context.Background()

	// Distinct idle timestamps: the OLDEST idle loses, the newer survives.
	// Cap 3: all three leases coexist (A active, B and D idle) so the
	// full-cap reclaim is the only path that can grant C.
	c := newSAManualClock()
	a := newSessionAdmission(3, 2, ttl, c)
	tA, _ := a.acquire(ctx, "kA", time.Minute) // active
	tB, _ := a.acquire(ctx, "kB", time.Minute)
	tD, _ := a.acquire(ctx, "kD", time.Minute)
	tB.release() // idle at T0
	c.Advance(time.Millisecond)
	tD.release() // idle at T0+1ms
	// C (fresh, cap full) must be granted IMMEDIATELY, reclaiming B.
	tC, err := a.acquire(ctx, "kC", time.Minute)
	if err != nil {
		t.Fatalf("C acquire: %v", err)
	}
	if held, _ := a.snapshot("kB"); held {
		t.Fatal("oldest idle B survived the reclaim")
	}
	if held, _ := a.snapshot("kD"); !held {
		t.Fatal("newer idle D was reclaimed instead of B")
	}
	if held, _ := a.snapshot("kA"); !held {
		t.Fatal("active A was reclaimed")
	}
	a.mu.Lock()
	if a.reclaimedTotal != 1 {
		t.Fatalf("reclaimedTotal = %d, want 1", a.reclaimedTotal)
	}
	a.mu.Unlock()
	tC.release()
	tA.release()

	// Equal-clock tie: the lexicographically smaller key is picked.
	c2 := newSAManualClock()
	a2 := newSessionAdmission(2, 2, ttl, c2)
	tkE, _ := a2.acquire(ctx, "kE", time.Minute)
	tkF, _ := a2.acquire(ctx, "kF", time.Minute)
	tkE.release() // both idle at the same clock instant
	tkF.release()
	tkG, err := a2.acquire(ctx, "kG", time.Minute)
	if err != nil {
		t.Fatalf("G acquire: %v", err)
	}
	if held, _ := a2.snapshot("kE"); held {
		t.Fatal("tie-break: smaller key kE should have been reclaimed")
	}
	if held, _ := a2.snapshot("kF"); !held {
		t.Fatal("tie-break: larger key kF must survive")
	}
	tkG.release()

	// maxWaiting 0 / grace 0 forbid queueing, not service: an idle lease
	// still yields to an immediate fresh arrival.
	c3 := newSAManualClock()
	a3 := newSessionAdmission(1, 0, ttl, c3)
	tkI, _ := a3.acquire(ctx, "kI", time.Minute)
	tkI.release() // idle
	tkN, err := a3.acquire(ctx, "kN", 0)
	if err != nil {
		t.Fatalf("zero-grace fresh arrival rejected despite reclaimable idle: %v", err)
	}
	tkN.release()

	// All-active cap: nothing idle to yield — a newcomer is rejected and
	// mints no lease.
	a5 := newSessionAdmission(2, 0, ttl, c3)
	tkP, _ := a5.acquire(ctx, "kP", time.Minute)
	tkQ, _ := a5.acquire(ctx, "kQ", time.Minute)
	if _, err := a5.acquire(ctx, "kR", time.Minute); !errors.Is(err, errSessionAdmissionQueueFull) {
		t.Fatalf("all-active cap: err = %v, want queue-full", err)
	}
	if held, _ := a5.snapshot("kR"); held {
		t.Fatal("rejected newcomer minted a lease")
	}
	tkQ.release()
	tkP.release()

	// Multiple accepted requests for one identity share one active lease;
	// only the FINAL release makes it reclaimable.
	c4 := newSAManualClock()
	a4 := newSessionAdmission(1, 2, ttl, c4)
	tkX1, _ := a4.acquire(ctx, "kX", time.Minute)
	tkX2, _ := a4.acquire(ctx, "kX", time.Minute)
	outR := make(chan error, 1)
	go func() { _, e := a4.acquire(ctx, "kR", time.Minute); outR <- e }()
	time.Sleep(30 * time.Millisecond)
	tkX1.release() // count 1: still active
	select {
	case <-outR:
		t.Fatal("newcomer admitted before the final same-identity release")
	case <-time.After(50 * time.Millisecond):
	}
	tkX2.release() // final release: the idle lease is reclaimed by the queue
	select {
	case err := <-outR:
		if err != nil {
			t.Fatalf("newcomer terminal = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("newcomer not granted by the final release")
	}
}

// Clock-free FIFO progression: both cap holders are ACTIVE (real waiting
// is tested against active leases only — idle leases yield immediately).
// The queue head — C, confirmed enqueued before D arrives — is the one
// admitted by A's final release; D proceeds on B's.
func TestSessionAdmission_FIFOOrderWithoutTraffic(t *testing.T) {
	const ttl = 10 * time.Second
	c := newSAManualClock()
	_ = c
	a := newSessionAdmission(2, 5, ttl, c)
	ctx := context.Background()

	waitingNow := func() int {
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.waiting
	}
	// Poll a manager-counter condition (the read is mutex-protected; the
	// write happens under the same mutex, so the readback is a stable
	// lower bound).
	waitFor := func(what string, n int) {
		deadline := time.Now().Add(2 * time.Second)
		for waitingNow() < n {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s (waiting=%d, want %d)", what, waitingNow(), n)
			}
			time.Sleep(2 * time.Millisecond)
		}
	}

	tA, _ := a.acquire(ctx, "kA", time.Minute)
	tB, _ := a.acquire(ctx, "kB", time.Minute) // both active: no reclaim possible

	out := make(chan string, 2)
	// C enqueues first and the manager confirms the slot before D
	// arrives, so the FIFO order under test is established, not a race
	// of two concurrent enqueues.
	go func() {
		if tk, err := a.acquire(ctx, "kC", time.Minute); err == nil {
			out <- "C"
			tk.release()
		}
	}()
	waitFor("C queued", 1)
	go func() {
		if tk, err := a.acquire(ctx, "kD", time.Minute); err == nil {
			out <- "D"
			tk.release()
		}
	}()
	waitFor("D queued", 2)

	// A's FINAL release frees its slot; the queue head C is granted
	// immediately (no expiry, no arrival, no tick).
	tA.release()
	select {
	case got := <-out:
		if got != "C" {
			t.Fatalf("queue head grant = %s, want C (FIFO broken)", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("queue head C not granted by the release")
	}
	// D proceeds on B's release, the same way.
	tB.release()
	select {
	case got := <-out:
		if got != "D" {
			t.Fatalf("second grant = %s, want D", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("D not granted by B's release")
	}
}

// TestSessionAdmission_FIFOSingleExpiry tightens the no-traffic FIFO test to
// ONE release at a time: A's final release ALONE frees exactly one slot —
// the queue head C (enqueued before D) is the only grant, and D provably
// stays queued; B's later release then proceeds the queue head D by
// RECLAIMING the oldest idle lease (B's — idle leases yield to queued
// demand, no idle-holder immunity under pressure).
func TestSessionAdmission_FIFOSingleExpiry(t *testing.T) {
	const ttl = 10 * time.Second
	c := newSAManualClock()
	a := newSessionAdmission(2, 5, ttl, c)
	ctx := context.Background()

	waitingNow := func() int {
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.waiting
	}
	waitFor := func(what string, n int) {
		deadline := time.Now().Add(2 * time.Second)
		for waitingNow() < n {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s (waiting=%d, want %d)", what, waitingNow(), n)
			}
			time.Sleep(2 * time.Millisecond)
		}
	}

	tB, _ := a.acquire(ctx, "kB", time.Minute)
	T1 := c.Now()
	c.Advance(time.Millisecond)                // clock separation certificate only
	tA, _ := a.acquire(ctx, "kA", time.Minute) // both ACTIVE
	if !c.Now().After(T1) {
		t.Fatal("manual clock did not advance")
	}

	cOut := make(chan *sessionAdmissionTicket, 1)
	go func() { tk, _ := a.acquire(ctx, "kC", time.Minute); cOut <- tk }()
	waitFor("C queued", 1)
	dOut := make(chan *sessionAdmissionTicket, 1)
	go func() { tk, _ := a.acquire(ctx, "kD", time.Minute); dOut <- tk }()
	waitFor("D queued", 2)

	// One release only: A's slot frees; the queue head C — confirmed
	// enqueued before D — is the ONLY grant, immediately. C's request
	// stays ACTIVE (we keep its ticket), so its lease is not
	// reclaimable and D has nothing to claim.
	tA.release()
	var tkC *sessionAdmissionTicket
	select {
	case tkC = <-cOut:
	case <-time.After(2 * time.Second):
		t.Fatal("queue head C not admitted by the single release")
	}
	select {
	case <-dOut:
		t.Fatal("D admitted on A's single release — FIFO order broken")
	case <-time.After(200 * time.Millisecond):
	}
	if held, since := a.snapshot("kC"); !held || !since.IsZero() {
		t.Fatalf("C lease = held %v since %v, want active", held, since)
	}

	// Second event: B's release frees the second slot; the queue head
	// D proceeds by reclaiming B's idle lease (the oldest — it idled
	// at T0, before any other idle). C stays active throughout.
	tB.release()
	select {
	case tkD := <-dOut:
		tkD.release()
	case <-time.After(2 * time.Second):
		t.Fatal("D not admitted after the second release")
	}
	a.mu.Lock()
	rec := a.reclaimedTotal
	a.mu.Unlock()
	if rec != 2 {
		t.Fatalf("reclaimedTotal = %d, want 2 (A's idle → C, B's idle → D)", rec)
	}
	if held, _ := a.snapshot("kB"); held {
		t.Fatal("B lease survived the drain reclaim")
	}
	if held, _ := a.snapshot("kA"); held {
		t.Fatal("A lease survived the first drain reclaim")
	}
	if held, _ := a.snapshot("kD"); !held {
		t.Fatal("D lease missing after its grant")
	}
	// C was never reclaimed: its lease is active (count 1), not idle.
	if held, since := a.snapshot("kC"); !held || !since.IsZero() {
		t.Fatalf("C lease disturbed = held %v since %v, want active", held, since)
	}
	tkC.release()
}

func TestSessionAdmission_FIFOSameIdentityPastBlocked(t *testing.T) {
	// C/D/C with cap 1: D blocks on the full lease; the LATER C (same
	// identity) slips past D to the held lease and is granted. C's
	// release idles the lease; its full TTL must then elapse (no
	// reclaimable-idle path exists for D, which holds no ticket) — D
	// never reclaims a lease it does not own. D's deadline is 1 minute
	// on the manual clock, so keep the TTL well below it: D must not
	// time out before the C lease expires.
	const ttl = 10 * time.Second
	c := newSAManualClock()
	a := newSessionAdmission(1, 5, ttl, c)
	ctx := context.Background()

	tC1, _ := a.acquire(ctx, "kC", time.Minute)
	dOut := make(chan error, 1)
	go func() { _, e := a.acquire(ctx, "kD", time.Minute); dOut <- e }()
	type cResult struct {
		tk *sessionAdmissionTicket
		e  error
	}
	cOut := make(chan cResult, 1)
	go func() {
		t3, e := a.acquire(ctx, "kC", time.Minute)
		cOut <- cResult{t3, e}
	}()
	time.Sleep(50 * time.Millisecond)
	select {
	case res := <-cOut:
		if res.e != nil {
			t.Fatalf("later C acquire: %v", res.e)
		}
		if res.tk == nil {
			t.Fatal("later C acquire: nil ticket")
		}
		res.tk.release()
	case <-time.After(2 * time.Second):
		t.Fatal("later C not granted past blocked D")
	}
	select {
	case <-dOut:
		t.Fatal("D released before the C lease went idle+expired")
	case <-time.After(50 * time.Millisecond):
	}
	tC1.release()
	c.Advance(ttl + time.Millisecond)
	select {
	case err := <-dOut:
		if err != nil {
			t.Fatalf("D terminal = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("D not released after the C lease expired")
	}
}

func TestSessionAdmission_ReturningArrivalQueuesBehindNewcomer(t *testing.T) {
	// A returning identity whose lease was RECLAIMED (or expired) is a
	// NEW identity: it cannot skip the newcomer that now holds the cap.
	// C queues behind A's ACTIVE lease; A's final release reclaims
	// A's idle lease and grants C immediately. A's return finds the cap
	// full (C ACTIVE, nothing reclaimable): it enqueues behind the
	// newcomer — no leap. C's completion then reclaims C's idle lease
	// and grants A — no clock advance, no TTL.
	const ttl = 5 * time.Second
	c := newSAManualClock()
	a := newSessionAdmission(1, 4, ttl, c)
	ctx := context.Background()

	tA, _ := a.acquire(ctx, "kA", time.Minute)
	cOut := make(chan *sessionAdmissionTicket, 1)
	go func() { tk, _ := a.acquire(ctx, "kC", time.Minute); cOut <- tk }()
	// C confirmed queued (manager counter; the write happens under the
	// same mutex, so the readback is a stable lower bound).
	waitFor := func(what string, n int) {
		deadline := time.Now().Add(2 * time.Second)
		for {
			a.mu.Lock()
			w := a.waiting
			a.mu.Unlock()
			if w >= n {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s (waiting=%d, want %d)", what, w, n)
			}
			time.Sleep(2 * time.Millisecond)
		}
	}
	waitFor("C queued", 1)
	// A's final release: the queue head C is granted IMMEDIATELY by
	// reclaiming A's idle lease — no clock advance, no TTL.
	tA.release()
	var tkC *sessionAdmissionTicket
	select {
	case tkC = <-cOut:
	case <-time.After(2 * time.Second):
		t.Fatal("C not granted by A's release")
	}
	if held, _ := a.snapshot("kA"); held {
		t.Fatal("A lease survived the drain reclaim")
	}
	// A's return: kA is gone (RECLAIMED) — a NEW identity. The cap is
	// full (C ACTIVE, nothing reclaimable): A enqueues behind the
	// newcomer; no leap, no immediate grant.
	aOut := make(chan *sessionAdmissionTicket, 1)
	go func() {
		tk, _ := a.acquire(ctx, "kA", time.Minute)
		aOut <- tk
	}()
	waitFor("A queued", 1)
	select {
	case <-aOut:
		t.Fatal("returning A leapt over the newcomer")
	case <-time.After(50 * time.Millisecond):
	}
	// C's turn completes: its lease goes idle NOW; the drain reclaims
	// it and grants the queue head A in the same critical section —
	// no clock advance, no TTL.
	tkC.release()
	select {
	case tk := <-aOut:
		tk.release()
	case <-time.After(2 * time.Second):
		t.Fatal("A not granted by the immediate reclaim")
	}
	a.mu.Lock()
	rec := a.reclaimedTotal
	a.mu.Unlock()
	if rec != 2 {
		t.Fatalf("reclaimedTotal = %d, want 2 (A's idle → C, C's idle → A)", rec)
	}
	if held, _ := a.snapshot("kC"); held {
		t.Fatal("C lease survived the reclaim")
	}
}

func TestSessionAdmission_CancellationAndTimeout(t *testing.T) {
	const ttl = 10 * time.Second
	c := newSAManualClock()
	a := newSessionAdmission(1, 4, ttl, c)
	ctx := context.Background()

	// kA ACTIVE (not idle) occupies the only slot: C and D queue behind
	// an active lease (nothing reclaimable). Cancelling C must not admit
	// D; A's final release does.
	tA, _ := a.acquire(ctx, "kA", time.Minute)
	cCtx, cancelC := context.WithCancel(context.Background())
	cOut := make(chan error, 1)
	go func() { _, err := a.acquire(cCtx, "kC", time.Minute); cOut <- err }()
	dOut := make(chan *sessionAdmissionTicket, 1)
	go func() { tk, _ := a.acquire(ctx, "kD", time.Minute); dOut <- tk }()
	time.Sleep(50 * time.Millisecond)
	cancelC()
	select {
	case err := <-cOut:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel err = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled C hang")
	}
	select {
	case <-dOut:
		t.Fatal("D admitted while the active kA lease still occupied the slot")
	case <-time.After(50 * time.Millisecond):
	}
	// A's final release grants the queue head D immediately — no expiry,
	// no arrival, no tick.
	tA.release()
	select {
	case tk := <-dOut:
		_ = tk
	case <-time.After(2 * time.Second):
		t.Fatal("D not admitted by the final release")
	}

	// Queue deadline (grace measured on the manager clock) terminates
	// without admission. D stays ACTIVE through the probe, so the
	// deadline — not a reclaim — is what ends kTO.
	toOut := make(chan error, 1)
	go func() { _, err := a.acquire(ctx, "kTO", 3*time.Second); toOut <- err }()
	time.Sleep(50 * time.Millisecond)
	c.Advance(ttl * 2) // beyond kTO's 3s deadline; kD lease (active) survives
	select {
	case err := <-toOut:
		if !errors.Is(err, errSessionAdmissionTimeout) {
			t.Fatalf("timeout err = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("deadline did not end the waiter")
	}
	if held, _ := a.snapshot("kTO"); held {
		t.Fatal("timed-out request kept a lease")
	}
}

func TestSessionAdmission_QueueFullAndZeroWaiting(t *testing.T) {
	const ttl = 10 * time.Second
	c := newSAManualClock()
	a := newSessionAdmission(1, 1, ttl, c)
	tA, _ := a.acquire(context.Background(), "kA", time.Minute) // ACTIVE holder
	first := make(chan error, 1)
	go func() { _, err := a.acquire(context.Background(), "kB", time.Minute); first <- err }()
	time.Sleep(50 * time.Millisecond)
	// Queue (depth 1) full with B: C is hard-rejected.
	if _, err := a.acquire(context.Background(), "kC", time.Minute); !errors.Is(err, errSessionAdmissionQueueFull) {
		t.Fatalf("err = %v, want queue-full", err)
	}
	// A nonpositive grace never queues.
	if _, err := a.acquire(context.Background(), "kD", -time.Second); !errors.Is(err, errSessionAdmissionQueueFull) {
		t.Fatalf("negative-grace err = %v, want queue-full", err)
	}
	if held, _ := a.snapshot("kC"); held {
		t.Fatal("rejected request minted a lease")
	}
	// maxWaiting=0 rejects all newcomers once the cap is occupied by an
	// ACTIVE holder (nothing reclaimable).
	b := newSessionAdmission(1, 0, ttl, c)
	bA, _ := b.acquire(context.Background(), "kA", time.Minute)
	if _, err := b.acquire(context.Background(), "kN", time.Minute); !errors.Is(err, errSessionAdmissionQueueFull) {
		t.Fatalf("waiting=0 err = %v, want queue-full", err)
	}
	bA.release()
	// A's final release grants the queued B immediately (no TTL, no
	// clock advance): an active-holder cap that frees is not a queue
	// timeout — B reaches the grant on the release itself.
	tA.release()
	select {
	case err := <-first:
		if err != nil {
			t.Fatalf("B terminal = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("B not admitted on the final release")
	}

	// Idle-holder / no-waiting: a cap occupied by an IDLE lease with an
	// EMPTY queue is reclaimable — an immediate fresh arrival succeeds
	// without any clock advance (queue-full is not triggered).
	d := newSessionAdmission(1, 1, ttl, c)
	tkI, _ := d.acquire(context.Background(), "kI", time.Minute)
	tkI.release() // idle, queue empty
	tkF, err := d.acquire(context.Background(), "kF", time.Minute)
	if err != nil {
		t.Fatalf("idle-holder fresh arrival: %v, want immediate grant", err)
	}
	d.mu.Lock()
	if d.reclaimedTotal != 1 {
		t.Fatalf("reclaimedTotal = %d, want 1", d.reclaimedTotal)
	}
	d.mu.Unlock()
	tkF.release()
}

func TestSessionAdmission_ConcurrentRaces(t *testing.T) {
	const ttl = 10 * time.Second
	c := newSAManualClock()
	a := newSessionAdmission(2, 50, ttl, c)
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx, cancel := context.WithCancel(context.Background())
			tk, err := a.acquire(ctx, fmt.Sprintf("k%d", i%2), time.Minute)
			cancel()
			if err == nil {
				tk.release()
			}
		}(i)
	}
	wg.Wait()
	c.Advance(ttl + time.Second)
	a.mu.Lock()
	n := len(a.leaseMap)
	a.mu.Unlock()
	if n != 0 {
		t.Fatalf("leftover leases = %d, want 0", n)
	}
	for i := 0; i < 2; i++ {
		if held, _ := a.snapshot(fmt.Sprintf("k%d", i)); held {
			t.Fatalf("k%d held after expiry", i)
		}
	}
}

func TestSessionAdmission_DoubleRelease(t *testing.T) {
	c := newSAManualClock()
	a := newSessionAdmission(1, 1, 5*time.Second, c)
	tk, _ := a.acquire(context.Background(), "kA", time.Minute)
	tk.release()
	tk.release() // once-guarded: the lease is idle, count 0
	c.Advance(5*time.Second + 2*time.Millisecond)
	tB, err := a.acquire(context.Background(), "kB", time.Minute)
	if err != nil {
		t.Fatalf("slot not free after double release: %v", err)
	}
	tB.release()
}

func TestSessionAdmission_GrantRaceRetiredOnce(t *testing.T) {
	// Grant publication races context cancel: beans the ticket is typed
	// to release exactly once; counts settle to zero, leases consistent.
	const ttl = 10 * time.Second
	c := newSAManualClock()
	a := newSessionAdmission(1, 4, ttl, c)
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithCancel(context.Background())
			tk, err := a.acquire(ctx, "kA", time.Minute)
			cancel()
			if err == nil {
				tk.release()
			}
		}()
	}
	wg.Wait()
	a.mu.Lock()
	l, ok := a.leaseMap["kA"]
	count := 0
	if ok {
		count = l.count
	}
	a.mu.Unlock()
	if count != 0 {
		t.Fatalf("accepted count = %d, want 0 after all cancelled/release", count)
	}
	c.Advance(ttl + 2*time.Millisecond)
	if held, _ := a.snapshot("kA"); held {
		t.Fatal("idle lease outlived its TTL")
	}
}

// ─── 4. HTTP integration: real router + real engine + manual clock ───────

func TestSessionAdmission_HTTPIdleRetentionAndHolderBypass(t *testing.T) {
	const ttl = 30 * time.Second
	s, engine, c := saHTTPSetup(t, ttl, 2, 4)
	_ = c
	url := s.ts.URL + "/v1/chat/completions"

	// A and B complete on SEPARATE manual-clock instants (the manager
	// clock is manual here): A idles strictly before B, so A is
	// deterministically the OLDEST idle lease — the reclaim victim.
	outA := saPostAsync(t, url, saChatBody("smoke", "A", "hello"))
	time.Sleep(150 * time.Millisecond) // let the request land in the gate
	engine.releaseKey("A")
	if code := <-outA; code != 200 {
		t.Fatalf("A status = %d", code)
	}
	// A's idle-stamp lands on its handler's DEFERRED release: poll for
	// it, THEN advance, so A is stamped strictly before B (deterministic
	// oldest-idle ordering — A is the reclaim victim, not a hash tie).
	saWaitIdleLease(t, "smoke", saAdmKey(t, "A", "smoke"))
	c.Advance(time.Millisecond) // A idles strictly before B
	outB := saPostAsync(t, url, saChatBody("smoke", "B", "hello"))
	time.Sleep(150 * time.Millisecond) // let the request land in the gate
	engine.releaseKey("B")
	if code := <-outB; code != 200 {
		t.Fatalf("B status = %d", code)
	}
	s.awaitIdle(t, 2*time.Second)
	ka := saAdmKey(t, "A", "smoke")
	kb := saAdmKey(t, "B", "smoke")
	// Both deferred releases have landed: each lease is idle-stamped.
	// (Which is strictly older is racy between the two handler
	// goroutines, so the reclaim VICTIM below is asserted identity-
	// agnostically — exactly one of the two is reclaimed.)
	saWaitIdleLease(t, "smoke", ka)
	saWaitIdleLease(t, "smoke", kb)
	feed := sessions.feed()
	if row := saAdmRow(feed, ka); row == nil || !row.AdmissionLease || row.AdmissionIdleSinceMs <= 0 {
		t.Fatalf("A row = %+v, want idle held", row)
	}
	if row := saAdmRow(feed, kb); row == nil || !row.AdmissionLease || row.AdmissionIdleSinceMs <= 0 {
		t.Fatalf("B row = %+v, want idle held", row)
	}

	// C arrives while BOTH leases are idle-held: it must be admitted
	// IMMEDIATELY (idle reservations yield to live demand — no clock
	// advance, no TTL), reclaim the oldest idle lease, and reach
	// upstream with no GPU, execution, or prefill permit leaked before
	// its grant.
	cDone := make(chan int, 1)
	go func() { out, _ := saPostOutDo(url, saChatBody("smoke", "C", "hello")); cDone <- out.code }()
	// The grant + forward are real-time async hops; poll the engine.
	// By the time the forward lands, C holds exactly its own execution
	// slot (no GPU/prefill permit — the cap is the only gate it crossed)
	// and no request sits queued anywhere.
	saPollEngineSeen(t, engine, "A", "B", "C")
	if st, err := http.Get(s.ts.URL + "/stats"); err == nil {
		sb, _ := io.ReadAll(st.Body)
		st.Body.Close()
		var stats struct {
			Backends []struct {
				InFlight        int32 `json:"inFlight"`
				Waiting         int32 `json:"waiting"`
				PrefillInFlight int32 `json:"prefillInFlight"`
				PrefillWaiting  int32 `json:"prefillWaiting"`
				Admission       *struct {
					Active   int   `json:"active"`
					Idle     int   `json:"idle"`
					Waiting  int   `json:"waiting"`
					Reclaims int64 `json:"reclaimedTotal"`
				} `json:"admission"`
			} `json:"backends"`
		}
		if err := json.Unmarshal(sb, &stats); err != nil {
			t.Fatalf("stats shape: %s", sb)
		}
		if len(stats.Backends) != 1 {
			t.Fatalf("stats shape: %s", sb)
		}
		b := stats.Backends[0]
		if b.InFlight != 1 || b.Waiting != 0 ||
			b.PrefillInFlight != 0 || b.PrefillWaiting != 0 {
			t.Fatalf("C at its gate leaked queue depth: %+v", b)
		}
		if b.Admission == nil {
			t.Fatalf("stats admission = null, want an object")
		}
		if b.Admission.Active != 1 || b.Admission.Idle != 1 {
			t.Fatalf("admission = %+v, want active 1 (C) + idle 1 (B)", b.Admission)
		}
		if b.Admission.Waiting != 0 {
			t.Fatalf("admission waiting = %d, want 0", b.Admission.Waiting)
		}
		if b.Admission.Reclaims < 1 {
			t.Fatalf("reclaimedTotal = %d, want >= 1 (idle lease reclaimed for C)", b.Admission.Reclaims)
		}
	} else {
		t.Fatal(err)
	}
	engine.releaseKey("C")
	select {
	case code := <-cDone:
		if code != 200 {
			t.Fatalf("C status = %d", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("C not admitted by the immediate reclaim")
	}
	if got := engine.Seen(); len(got) != 3 || got[2] != "C" {
		t.Fatalf("seen = %v, want [A B C]", got)
	}
	s.awaitIdle(t, 2*time.Second)
	feed = sessions.feed()
	// A idled strictly before B (the clock advanced between the two
	// releases, after A's idle-stamp and before B's): A is
	// deterministically the OLDEST idle lease and the reclaim victim.
	// Its historical row stays; its authoritative lease is gone.
	if row := saAdmRow(feed, ka); row == nil || row.AdmissionLease || row.AdmissionIdleSinceMs != 0 {
		t.Fatalf("reclaimed A row = %+v, want false/0", row)
	}
	// B (newer idle) survived the reclaim and is still idle-held.
	if row := saAdmRow(feed, kb); row == nil || !row.AdmissionLease || row.AdmissionIdleSinceMs <= 0 {
		t.Fatalf("B row = %+v, want idle held", row)
	}
	// C's single request already completed, so its lease is now
	// idle-holding: true WITH a set idle-since stamp (the manual clock's
	// grant instant), not an active lease.
	if row := saAdmRow(feed, saAdmKey(t, "C", "smoke")); row == nil || !row.AdmissionLease || row.AdmissionIdleSinceMs <= 0 {
		t.Fatalf("held C row = %+v, want true/idle-stamped", row)
	}
}

func TestSessionAdmission_HOLPHolderRetention(t *testing.T) {
	const ttl = 5 * time.Second
	s, engine, c := saHTTPSetup(t, ttl, 2, 4)
	_ = c
	url := s.ts.URL + "/v1/chat/completions"

	// A1 and B1 run, gated (ACTIVE leases): the cap is full with
	// nothing reclaimable, so C — a fresh identity — must be
	// ADMISSION-queued (proven: enqueued BEFORE A2).
	a1Done := saPostAsync(t, url, saChatBody("smoke", "A", "one"))
	time.Sleep(100 * time.Millisecond)
	b1Done := saPostAsync(t, url, saChatBody("smoke", "B", "one"))
	time.Sleep(100 * time.Millisecond)
	saPollAdmittedLeases(t, "smoke", 2)
	cDone := make(chan int, 1)
	go func() { out, _ := saPostOutDo(url, saChatBody("smoke", "C", "hello")); cDone <- out.code }()
	saPollAdmissionWaiting(t, "smoke", 1) // C queued — confirmed, before A2

	// A2 arrives while A1 is still ACTIVE (gated): it reuses A's HELD
	// lease (count 1→2) — head-of-line preference for a held identity —
	// and is admitted immediately while C is queued behind it. Both
	// execution slots are full (A1, B1), so A2 is slot-queued, not yet
	// forwarded. A RECLAIMED/expired returning identity does NOT keep
	// this priority (it is a newcomer again — covered in
	// ReturningArrivalQueuesBehindNewcomer).
	turn2 := saPostAsync(t, url, saChatBody("smoke", "A", "turn two"))
	time.Sleep(150 * time.Millisecond)
	if held, since := sessionAdmissions["smoke"].snapshot(saAdmKey(t, "A", "smoke")); !held || !since.IsZero() {
		t.Fatalf("A lease = held %v since %v, want active (count 2)", held, since)
	}
	saPollAdmissionWaiting(t, "smoke", 1) // C still queued; A2 is slot-queued
	select {
	case <-cDone:
		t.Fatal("C released before A2's bypass grant")
	case <-time.After(100 * time.Millisecond):
	}

	// A1 completes: A's count 2→1 (still ACTIVE — A2 accepted; no
	// reclaim). Slot 1 frees; A2 — slot-queued the whole time —
	// acquires it and FORWARDS (third), before the queued C.
	engine.releaseKey("A")
	if code := <-a1Done; code != 200 {
		t.Fatalf("A1 = %d", code)
	}
	saPollEngineSeen(t, engine, "A", "B", "A") // A2 forwarded (third); C not
	saPollAdmissionWaiting(t, "smoke", 1)      // C still queued behind the bypass
	if held, since := sessionAdmissions["smoke"].snapshot(saAdmKey(t, "A", "smoke")); !held || !since.IsZero() {
		t.Fatalf("A lease = held %v since %v, want active (A2 accepted)", held, since)
	}

	// B1 completes: B's lease goes idle; the drain immediately
	// reclaims it (the only idle) and grants the queue head C — no TTL,
	// no new arrival, no clock. C is forwarded to its own gate.
	engine.releaseKey("B")
	if code := <-b1Done; code != 200 {
		t.Fatalf("B1 = %d", code)
	}
	saPollEngineSeen(t, engine, "A", "B", "A", "C")
	mgr := sessionAdmissions["smoke"]
	mgr.mu.Lock()
	rec := mgr.reclaimedTotal
	aLease, okA := mgr.leaseMap[saAdmKey(t, "A", "smoke")]
	_, okB := mgr.leaseMap[saAdmKey(t, "B", "smoke")]
	mgr.mu.Unlock()
	if rec < 1 {
		t.Fatalf("reclaimedTotal = %d, want >= 1 (B's idle lease yielded to queued C)", rec)
	}
	if okB {
		t.Fatalf("B lease survived the drain reclaim")
	}
	if !okA || aLease.count != 1 {
		t.Fatalf("A lease disturbed by B's release (ok=%v count=%d), want active 1 (A2)", okA, aLease.count)
	}
	// Open the two pending gates; both requests complete.
	engine.releaseKey("A") // A2's gate (A1's was released above)
	if code := <-turn2; code != 200 {
		t.Fatalf("A2 = %d", code)
	}
	engine.releaseKey("C")
	select {
	case code := <-cDone:
		if code != 200 {
			t.Fatalf("C = %d", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("C not granted by the release-driven reclaim")
	}
	s.awaitIdle(t, 2*time.Second)
}

func TestSessionAdmission_AcceptedLifetimeHold(t *testing.T) {
	// maxAdmitted 1: A's single lease is the WHOLE admitted set, so C
	// cannot be admitted at all while any accepted A request is
	// outstanding or the idle TTL is in flight — C is ADMISSION-queued,
	// not merely slot-blocked. (With cap 2 C is its own lease, only
	// slot-blocked, and the lease-retention clause is unexercised.)
	const ttl = 3 * time.Second
	s, engine, c := saHTTPSetup(t, ttl, 1, 4)
	url := s.ts.URL + "/v1/chat/completions"

	// A1 takes an execution slot; A2 (same key) is lease-admitted and
	// QUEUED at the backend slot (maxConcurrent 2 both run; the second one
	// stays a QUEUED ACCEPTED request: admitted, not yet running).
	a1Done := make(chan int, 1)
	go func() { out, _ := saPostOutDo(url, saChatBody("smoke", "A", "one")); a1Done <- out.code }()
	saPollEngineSeen(t, engine, "A")
	a2Done := make(chan int, 1)
	go func() { out, _ := saPostOutDo(url, saChatBody("smoke", "A", "two")); a2Done <- out.code }()
	saPollEngineSeen(t, engine, "A", "A")
	cDone := make(chan int, 1)
	go func() { out, _ := saPostOutDo(url, saChatBody("smoke", "C", "x")); cDone <- out.code }()

	// C (new identity, cap 1) must be admission-queued BEHIND A's held
	// lease — its forward cannot exist, and the manager reports exactly
	// one admitted lease (A) and one queued admission (C).
	saPollAdmittedLeases(t, "smoke", 1)
	saPollAdmissionWaiting(t, "smoke", 1)
	if held, _ := sessionAdmissions["smoke"].snapshot(saAdmKey(t, "C", "smoke")); held {
		t.Fatal("newcomer C minted/immediately admitted a lease at cap 1")
	}

	// Two outstanding accepted A requests hold the ACTIVE lease. An
	// active lease never expires, so a short probe advance (kept far
	// below C's queueTimeout admission deadline, or it would 429) is all
	// the clock certificate the checkpoints need: C stays in the queue.
	c.Advance(ttl / 10)
	if n := len(engine.Seen()); n != 2 {
		t.Fatalf("engine seen = %v, want two A requests", engine.Seen())
	}
	saPollAdmissionWaiting(t, "smoke", 1)
	select {
	case <-cDone:
		t.Fatal("newcomer C admitted while accepted A requests are outstanding")
	case <-time.After(50 * time.Millisecond):
	}

	engine.releaseKey("A") // A1 done; A2 continues
	select {
	case code := <-a1Done:
		if code != 200 {
			t.Fatalf("A1 = %d", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("A1 did not complete")
	}
	c.Advance(ttl / 10)
	saPollAdmissionWaiting(t, "smoke", 1)
	select {
	case <-cDone:
		t.Fatal("C admitted while A2 still runs")
	case <-time.After(50 * time.Millisecond):
	}
	engine.releaseKey("A") // A2 done: the lease is reclaimed IMMEDIATELY
	select {
	case code := <-a2Done:
		if code != 200 {
			t.Fatalf("A2 = %d", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("A2 did not complete")
	}
	// A2's deferred release is the final accepted release: the queue
	// head C is granted in that same critical section — no clock
	// advance, no expiry, no arrival. (Protection while any A request
	// is outstanding was certified above; the idle TTL no longer gates
	// a queued newcomer.)
	engine.releaseKey("C")
	select {
	case code := <-cDone:
		if code != 200 {
			t.Fatalf("C = %d", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("C not admitted by the final release")
	}
	if got := engine.Seen(); len(got) != 3 || got[2] != "C" {
		t.Fatalf("seen = %v, want C last", got)
	}
	s.awaitIdle(t, 3*time.Second)
}

// TestSessionAdmission_AcceptedLifetimeGPUQueue is the GPU-queue arm of
// the accepted-lifetime invariant, with an ACTIVE GPU budget. Topology:
// tier-0 king K (weight 0; running so t0Inflight > 0, which engages the
// budget check at zero cost); tier-1 subject "smoke" (weight 1, two
// running slots, a 1-unit budget, admission cap 2, waiting bound 2).
//
//  1. K runs, gated. F (smoke) runs, takes the SOLE budget unit +
//     slot 1, and is gated upstream (ACTIVE lease).
//  2. M is lease-admitted and QUEUES AT THE EXHAUSTED BUDGET behind F
//     (ACTIVE lease; no running slot — the budget is upstream of the
//     execution slot).
//  3. B and C are queued at the ADMISSION gate: the cap is full with
//     two ACTIVE leases (F, M) — nothing reclaimable.
//  4. A fresh E (queue bound 2) is hard-rejected
//     session-admission-queue-full: it cannot overtake, and it cannot
//     queue.
//  5. A TTL advance while F (engine gate) and M (budget queue) are
//     accepted: both leases held WITH NO IDLE STAMP; the budget has
//     not moved; B and C still not granted.
//  6. F completes: its lease idles and the drain IMMEDIATELY reclaims
//     it (no tick) — B is granted → budget-queued behind M (no
//     running slot).
//  7. M completes: its idle lease is reclaimed by the drain → C is
//     granted → budget-queued behind B. B takes M's unit + slot.
//  8. B completes: C takes the unit + slot; B's lease idles. K
//     completes (weight 0; the budget is no longer engaged).
//  9. C completes: the FINAL accepted release. B's and C's
//     idle-stamped leases expire one full TTL later (no demand);
//     forward order [K F M B C]; budget and slots drain to zero —
//     every permit released exactly once.
//
// Clock notes: execution-slot deadlines are WALL (queueTimeout 30s);
// GPU-budget wait deadlines are WALL (grace 5s) — B's/C's waits stay
// far under that; the MANUAL clock drives only admission deadlines
// (queueTimeout 30s from each enqueue) and the idle TTL; B's/C's
// budget waits are short wall intervals (<1s ≪ the 5s budget grace).
func TestSessionAdmission_AcceptedLifetimeGPUQueue(t *testing.T) {
	const ttl = time.Second
	engine := newSAGateEngine()
	withRouterEnv(t,
		[]Backend{
			{Name: "smoke", URL: engine.TS.URL, Tier: 1, GpuWeight: 1, MaxConcurrent: 2, MaxQueueDepth: 4},
			{Name: "king", URL: engine.TS.URL, Tier: 0, GpuWeight: 0, MaxConcurrent: 2, MaxQueueDepth: 4},
		},
		map[string]int{"smoke": 2, "king": 2}, nil)
	gb := &gpuBudget{max: 1}
	gb.cond = sync.NewCond(&gb.mu)
	gpu = gb
	proxies["smoke"] = newTestProxy(engine.TS.URL)
	proxies["king"] = newTestProxy(engine.TS.URL)
	saIsolate(t)
	c := newSAManualClock()
	withAdmissionEnv(t, ttl, c, 2, 2)
	s := newSAServing()
	t.Cleanup(func() { s.ts.Close() })
	t.Cleanup(func() { engine.releaseAll() })
	t.Cleanup(func() { s.awaitIdle(t, 5*time.Second) })

	url := s.ts.URL + "/v1/chat/completions"
	kM := saAdmKey(t, "M", "smoke")
	kB := saAdmKey(t, "B", "smoke")
	kC := saAdmKey(t, "C", "smoke")
	kK := saAdmKey(t, "K", "king")

	// 1. K (king) runs, gated. F: sole budget unit + slot 1, gated.
	kDone := saPostAsync(t, url, saChatBody("king", "K", "hello"))
	time.Sleep(100 * time.Millisecond)
	if t0i := tier0Inflight(); t0i != 1 {
		t.Fatalf("t0Inflight = %d, want 1 (K running)", t0i)
	}
	fDone := saPostAsync(t, url, saChatBody("smoke", "F", "x"))
	time.Sleep(100 * time.Millisecond)
	if gUsed, _, gWaiting := gb.snapshot(); gUsed != 1 || gWaiting != 0 {
		t.Fatalf("F not on the budget: used=%d/1 waiting=%d, want 1/0", gUsed, gWaiting)
	}

	// 2. M: lease-admitted, BUDGET-QUEUED behind F (no running slot).
	mDone := saPostAsync(t, url, saChatBody("smoke", "M", "one"))
	time.Sleep(100 * time.Millisecond)
	saPollGPU(t, gb, 1, 1) // F (unit) + M (queued).
	if held, _ := sessionAdmissions["smoke"].snapshot(kM); !held {
		t.Fatal("M lease not held while budget-queued")
	}
	if in, _, _, _ := slots["smoke"].snapshot(); in != 1 {
		t.Fatalf("smoke inFlight = %d, want 1 (F only)", in)
	}

	// 3. B, C: admission-queued while the cap is full with ACTIVE F + M.
	bDone := saPostAsync(t, url, saChatBody("smoke", "B", "two"))
	time.Sleep(40 * time.Millisecond)
	cDone := make(chan int, 1)
	go func() { out, _ := saPostOutDo(url, saChatBody("smoke", "C", "x")); cDone <- out.code }()
	time.Sleep(40 * time.Millisecond)
	if held, _ := sessionAdmissions["smoke"].snapshot(kB); held {
		t.Fatal("B granted while the cap is full with active leases")
	}
	if held, _ := sessionAdmissions["smoke"].snapshot(kC); held {
		t.Fatal("C granted while the cap is full with active leases")
	}
	if got := engine.Seen(); len(got) != 2 || got[0] != "K" || got[1] != "F" {
		t.Fatalf("engine seen = %v, want [K F] (M is budget-queued, not forwarded)", got)
	}

	// 4. Fresh E: the waiting bound (2) is occupied by B + C — E is
	// hard-rejected, not queued; a newcomer cannot overtake.
	eCode, eHdr, eBody := saPost(t, url, saChatBody("smoke", "E", "x"))
	if eCode != http.StatusTooManyRequests ||
		eHdr.Get("X-Router-Reason") != "session-admission-queue-full" {
		t.Fatalf("E = %d %q, want 429 session-admission-queue-full: %s",
			eCode, eHdr.Get("X-Router-Reason"), eBody)
	}

	// 5. TTL advance with F (engine gate) and M (budget queue) accepted:
	// both leases held WITH NO IDLE STAMP; budget has not moved; B/C
	// not granted.
	c.Advance(2 * ttl)
	for _, k := range []string{kM, saAdmKey(t, "F", "smoke")} {
		if held, since := sessionAdmissions["smoke"].snapshot(k); !held || !since.IsZero() {
			t.Fatalf("active lease %s = held %v since %v, want held (no idle stamp)", k, held, since)
		}
	}
	if held, _ := sessionAdmissions["smoke"].snapshot(kB); held {
		t.Fatal("B granted while F and M are still accepted")
	}
	if gUsed, _, gWaiting := gb.snapshot(); gUsed != 1 || gWaiting != 1 {
		t.Fatalf("budget moved under a TTL advance: used=%d waiting=%d, want 1/1", gUsed, gWaiting)
	}

	// 6. F completes: the final release idles its lease and the drain
	// RECLAIMS it immediately (no tick) — B is granted, then
	// BUDGET-QUEUED behind M (no running slot).
	engine.releaseKey("F")
	if code := <-fDone; code != 200 {
		t.Fatalf("F = %d", code)
	}
	// F's reclaim + B's grant land on the handler's DEFERRED release
	// (after the client reads the response): poll for the stable
	// admitted-set {M, B} (F's lease is gone, B's is granted).
	saPollAdmittedLeases(t, "smoke", 2)
	if held, _ := sessionAdmissions["smoke"].snapshot(kB); !held {
		t.Fatal("B not granted by the release-driven reclaim")
	}
	saPollGPU(t, gb, 1, 1)             // M (unit) + B (queued).
	time.Sleep(150 * time.Millisecond) // settle: no forward (B budget-queued).
	if in, _, _, _ := slots["smoke"].snapshot(); in != 1 {
		t.Fatalf("smoke inFlight = %d, want 1 (M running; B holds no slot while budget-queued)", in)
	}

	// 7. M completes: its idle lease is reclaimed by the drain → C is
	// granted → budget-queued behind B. B takes the unit + slot.
	engine.releaseKey("M")
	if code := <-mDone; code != 200 {
		t.Fatalf("M = %d", code)
	}
	// M's reclaim + C's grant land on the handler's DEFERRED release:
	// poll for the stable admitted-set {B, C} (M's lease is gone, C's
	// is granted).
	saPollAdmittedLeases(t, "smoke", 2)
	if held, _ := sessionAdmissions["smoke"].snapshot(kC); !held {
		t.Fatal("C not granted by M's release-driven reclaim")
	}
	saPollGPU(t, gb, 1, 1)             // B (unit) + C (queued).
	time.Sleep(150 * time.Millisecond) // settle: B takes the unit + slot, C stays budget-queued.
	if in, _, _, _ := slots["smoke"].snapshot(); in != 1 {
		t.Fatalf("smoke inFlight = %d, want 1 (B running; C budget-queued)", in)
	}

	// 8. B completes: C takes the unit + slot; B's lease idles. K
	// completes (weight 0; the budget is no longer engaged — C is
	// already past it).
	engine.releaseKey("B")
	if code := <-bDone; code != 200 {
		t.Fatalf("B = %d", code)
	}
	saPollGPU(t, gb, 1, 0)                               // C holds the sole unit.
	saPollEngineSeen(t, engine, "K", "F", "M", "B", "C") // C is at the gate.
	engine.releaseKey("K")
	if code := <-kDone; code != 200 {
		t.Fatalf("K = %d", code)
	}

	// 9. C completes at its gate: the FINAL accepted release. B and C
	// leases are idle-stamped; ONE FULL TTL later (no demand), both
	// are gone.
	engine.releaseKey("C")
	if code := <-cDone; code != 200 {
		t.Fatalf("C = %d", code)
	}
	saWaitIdleLease(t, "smoke", kB) // B's stamp: idles exactly at exit.
	saWaitIdleLease(t, "smoke", kC)
	saPollGPU(t, gb, 0, 0) // every permit released exactly once.
	if in, _, _, _ := slots["smoke"].snapshot(); in != 0 {
		t.Fatalf("smoke inFlight = %d, want 0", in)
	}
	c.Advance(ttl + 2*time.Millisecond)
	if held, _ := sessionAdmissions["smoke"].snapshot(kB); held {
		t.Fatal("B lease outlived its full idle TTL")
	}
	if held, _ := sessionAdmissions["smoke"].snapshot(kC); held {
		t.Fatal("C lease outlived its full idle TTL")
	}
	if held, _ := sessionAdmissions["king"].snapshot(kK); held {
		t.Fatal("K lease outlived its full idle TTL")
	}
	saPollEngineSeen(t, engine, "K", "F", "M", "B", "C")
	s.awaitIdle(t, 3*time.Second)
}

// TestSessionAdmission_AcceptedLifetimePrefillQueue is the dedicated
// large-prefill-queue arm: both A requests are large (12 est. tokens >=
// 8 threshold), so A1 holds the single dedicated prefill slot and A2 —
// lease-admitted — is QUEUED in the prefill slot's queue behind it.
// Far past the TTL the lease must not expire while accepted counts are
// positive; A2 completes in the upstream gate, then the FINAL release
// reclaims the idle lease and grants the queue head C immediately.
func TestSessionAdmission_AcceptedLifetimePrefillQueue(t *testing.T) {
	// Clock budget: the manual clock drives the production queue-wait
	// deadlines (queueTimeout 30s) for EVERY queued request — A2's
	// prefill-slot queue wait, C's admission queue wait. Total advances
	// stay 2.6s, far under all armed deadlines.
	const ttl = 200 * time.Millisecond
	engine := newSAGateEngine()
	withRouterEnv(t,
		[]Backend{{
			Name: "smoke", URL: engine.TS.URL, MaxConcurrent: 2, MaxQueueDepth: 4,
			MaxConcurrentLargePrefill: 1, LargePrefillThresholdTokens: 8, // 1 slot, 4-deep queue
		}},
		map[string]int{"smoke": 2}, nil)
	prefillSlots["smoke"] = newSlot(1, 4)
	proxies["smoke"] = newTestProxy(engine.TS.URL)
	saIsolate(t)
	c := newSAManualClock()
	// maxAdmitted = 1: C owns no lease of its own — its single slot is
	// behind A's lease the whole time. (With 2 slots C is admitted as a
	// distinct lease immediately and only slot-blocked — its forward
	// then races A2's at A1's completion, and the intermediate
	// "C must not complete" assertions go false-positive under real
	// time.) 1 slot makes the perimeter scenario true in the first
	// place: C stays lease-queued until A's FINAL release.
	withAdmissionEnv(t, ttl, c, 1, 4)
	s := newSAServing()
	// Teardown runs LIFO (last cleanup first), so register in the
	// REVERSE of the desired shutdown order: engine gates open first
	// (any stuck handler gets its reply immediately), then handler
	// drain, THEN both servers close. A server Close while a handler
	// is still mid round-trip waits in wg.Wait for a response that
	// only releaseAll can produce — a teardown wedge.
	t.Cleanup(func() { s.ts.Close() })
	t.Cleanup(func() { engine.TS.Close() })
	t.Cleanup(func() { s.awaitIdle(t, 5*time.Second) })
	t.Cleanup(func() { engine.releaseAll() })

	url := s.ts.URL + "/v1/chat/completions"

	// Streaming: the large-prefill slot is released on the first
	// response byte only (no estimate timer), and the engine writes no
	// bytes until its gate opens — the slot hold is deterministic and
	// controlled by releaseKey below.
	a1 := strings.Repeat("a", 48) // 12 est. tokens: large
	done1 := make(chan int, 1)
	go func() { out, _ := saPostOutDo(url, saChatStreamBody("smoke", "A", a1)); done1 <- out.code }()
	time.Sleep(150 * time.Millisecond)
	done2 := make(chan int, 1)
	go func() {
		code, _, _ := saPost(t, url, saChatStreamBody("smoke", "A", strings.Repeat("b", 48)))
		done2 <- code
	}()
	time.Sleep(75 * time.Millisecond)
	// A1 holds the sole prefill slot; A2 is lease-admitted and QUEUED
	// in the dedicated prefill slot queue (1 slot, 4-deep queue).
	if pin, pmax, pw, _ := prefillSlots["smoke"].snapshot(); pin != 1 || pw != 1 {
		t.Fatalf("prefill slots = %d/%d waiting %d, want 1/%d/1", pin, pmax, pw, pmax)
	}
	// C is SMALL (1 est. token < threshold): it queues only on the
	// execution slots. If C were large it would join A2 in the prefill
	// slot queue, and the slot manager's notify is not FIFO — a later
	// waiter can consume the single release token, holding A2's slot
	// until C's first byte and starving A2 into its queueTimeout 429.
	cDone := make(chan int, 1)
	go func() { out, _ := saPostOutDo(url, saChatBody("smoke", "C", "c")); cDone <- out.code }()
	time.Sleep(30 * time.Millisecond)
	if held, _ := sessionAdmissions["smoke"].snapshot(saAdmKey(t, "A", "smoke")); !held {
		t.Fatal("A lease not held while accepted requests are outstanding")
	}

	// Far past the TTL with A1 accepted (engine gate), A2 accepted and
	// stuck in the prefill slot queue behind A1's first-byte-gated
	// hold, and C QUEUED in the admission queue (cap 1: A's lease is
	// the only occupant): the ACTIVE lease must RETAIN, C must stay
	// queued, and nothing past the first forward is new — A2 is NOT
	// forwarded while A1's slot is held.
	c.Advance(ttl * 10)
	if held, _ := sessionAdmissions["smoke"].snapshot(saAdmKey(t, "A", "smoke")); !held {
		t.Fatal("A lease expired while accepted requests are outstanding")
	}
	select {
	case <-cDone:
		t.Fatal("newcomer C admitted while the lease is active")
	case <-time.After(50 * time.Millisecond):
	}
	if got := engine.Seen(); len(got) != 1 || got[0] != "A" {
		t.Fatalf("engine seen = %v, want [A] (A2 prefill-queued is not forwarded)", got)
	}

	// A1's response byte arrives (the gate opens): its 1.2ms prefill
	// hold has already released its slot, so A2 acquires it and advances
	// to its own upstream gate (kept shut).
	engine.releaseKey("A")
	if code := <-done1; code != 200 {
		t.Fatalf("A1 = %d", code)
	}
	saPollPrefillSlot(t, 1, 0) // A2's prefill acquisition + handoff

	// A2 is now the ONLY accepted A request, finishing in the upstream
	// gate. A full TTL has already elapsed since the earlier completion:
	// still no expiry — the lease is active (count 1), not idle.
	c.Advance(ttl + 2*time.Millisecond)
	if held, since := sessionAdmissions["smoke"].snapshot(saAdmKey(t, "A", "smoke")); !held || !since.IsZero() {
		t.Fatalf("A lease = held %v idle-since %v, want active (no idle stamp)", held, since)
	}
	select {
	case <-cDone:
		t.Fatal("C admitted while A2 is still accepted (in the upstream gate)")
	case <-time.After(50 * time.Millisecond):
	}

	// A2 completes: the FINAL accepted release idles the lease and the
	// drain reclaims it immediately — C is granted in that same
	// critical section, no clock advance, no TTL.
	engine.releaseKey("A") // A2's gate
	if code := <-done2; code != 200 {
		t.Fatalf("A2 = %d", code)
	}
	// A2 is the only A request finishing now. The final release drains the
	// queue: it reclaims the idled lease (immediate, no clock advance) and
	// grants C, whose forward lands right after A2's — seen reaches
	// [A, A, C] in one step, so poll the full order, not the two-A prefix
	// (an exact-length-2 poll would time out once C's forward is ahead).
	saPollEngineSeen(t, engine, "A", "A", "C")
	saPollAdmissionWaiting(t, "smoke", 0) // C granted by the final release
	engine.releaseKey("C")
	select {
	case code := <-cDone:
		if code != 200 {
			t.Fatalf("C = %d", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("C not admitted by the final release")
	}
	// C is small and admission-queued the whole time: it can only
	// forward after the final release, i.e. strictly after A2's
	// forward. Order A, A, C is forced.
	saPollEngineSeen(t, engine, "A", "A", "C")
	s.awaitIdle(t, 3*time.Second)
}

func TestSessionAdmission_TerminalBounds(t *testing.T) {
	const ttl = 5 * time.Second

	t.Run("queue-full", func(t *testing.T) {
		s, engine, c := saHTTPSetup(t, ttl, 1, 1)
		_ = c
		url := s.ts.URL + "/v1/chat/completions"
		aDone := saPostAsync(t, url, saChatBody("smoke", "A", "x"))
		// A's lease must be ADMITTED (not merely sent) before B can be
		// proven to sit in the queue, and B must be QUEUE-FULL-occupied
		// before C's hard-reject POST — poll the manager, never sleep.
		saPollAdmittedLeases(t, "smoke", 1)
		bDone := make(chan int, 1)
		go func() { out, _ := saPostOutDo(url, saChatBody("smoke", "B", "x")); bDone <- out.code }()
		saPollAdmissionWaiting(t, "smoke", 1)
		// The depth-1 admission queue is occupied by B: C is hard-rejected
		// with the exact reason.
		code, hdr, body := saPost(t, url, saChatBody("smoke", "C", "x"))
		if code != http.StatusTooManyRequests ||
			hdr.Get("X-Router-Reason") != "session-admission-queue-full" {
			t.Fatalf("C = %d %q, want 429 session-admission-queue-full: %s",
				code, hdr.Get("X-Router-Reason"), body)
		}
		if held, _ := sessionAdmissions["smoke"].snapshot(saAdmKey(t, "C", "smoke")); held {
			t.Fatal("rejected C minted a lease")
		}
		// A is ACTIVE (gated): its completion grants the queue head B
		// immediately — no idle arm, no clock advance, no TTL handoff.
		// B's forward lands upstream right after A's gate opens.
		engine.releaseKey("A")
		if c0 := <-aDone; c0 != 200 {
			t.Fatalf("A = %d", c0)
		}
		saPollEngineSeen(t, engine, "A", "B")
		engine.releaseKey("B")
		if c1 := <-bDone; c1 != 200 {
			t.Fatalf("B = %d", c1)
		}
	})

	t.Run("admission-timeout", func(t *testing.T) {
		s, engine, c := saHTTPSetup(t, ttl, 1, 2)
		_ = s
		url := s.ts.URL + "/v1/chat/completions"
		aDone := make(chan int, 1)
		go func() { out, _ := saPostOutDo(url, saChatBody("smoke", "A", "x")); aDone <- out.code }()
		time.Sleep(150 * time.Millisecond)
		bDone := make(chan saPostOut, 1)
		go func() { out, _ := saPostOutDo(url, saChatBody("smoke", "B", "x")); bDone <- out }()
		time.Sleep(150 * time.Millisecond)
		// B's admission deadline (queueTimeout on the manual clock).
		c.Advance(queueTimeout + time.Second)
		select {
		case out := <-bDone:
			if out.code != http.StatusTooManyRequests {
				t.Fatalf("B = %d, want 429", out.code)
			}
			// The exact reason, from the router's 429 JSON body (error.type),
			// not just the status: queue-full and timeout both 429.
			var body struct {
				Err struct {
					Type string `json:"type"`
				} `json:"error"`
			}
			if err := json.Unmarshal([]byte(out.body), &body); err != nil || body.Err.Type != "session-admission-timeout" {
				t.Fatalf("B reason = %q (%v), want session-admission-timeout", body.Err.Type, err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("B did not time out")
		}
		if held, _ := sessionAdmissions["smoke"].snapshot(saAdmKey(t, "B", "smoke")); held {
			t.Fatal("timed-out B kept a lease")
		}
		engine.releaseAll()
		<-aDone
	})

	t.Run("cancel-mints-no-lease", func(t *testing.T) {
		s, engine, _ := saHTTPSetup(t, ttl, 1, 2)
		url := s.ts.URL + "/v1/chat/completions"
		aDone := make(chan int, 1)
		go func() { out, _ := saPostOutDo(url, saChatBody("smoke", "A", "x")); aDone <- out.code }()
		time.Sleep(150 * time.Millisecond)
		ctx, cancel := context.WithCancel(context.Background())
		req, err := http.NewRequestWithContext(ctx, "POST", url, strings.NewReader(saChatBody("smoke", "B", "x")))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		clientErr := make(chan error, 1)
		go func() {
			resp, err := http.DefaultClient.Do(req)
			if resp != nil {
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
			clientErr <- err
		}()
		time.Sleep(150 * time.Millisecond)
		cancel()
		select {
		case <-clientErr:
		case <-time.After(2 * time.Second):
			t.Fatal("client not released")
		}
		if held, _ := sessionAdmissions["smoke"].snapshot(saAdmKey(t, "B", "smoke")); held {
			t.Fatal("cancelled B minted a lease")
		}
		engine.releaseAll()
		<-aDone
	})

	t.Run("downstream-rejection-idles-lease", func(t *testing.T) {
		errEngine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.Copy(io.Discard, r.Body)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, `{"error":"boom"}`)
		}))
		defer errEngine.Close()
		withRouterEnv(t,
			[]Backend{{Name: "err5", URL: errEngine.URL, MaxConcurrent: 1}},
			map[string]int{"err5": 1}, nil)
		proxies["err5"] = newTestProxy(errEngine.URL)
		saIsolate(t)
		c := newSAManualClock()
		withAdmissionEnv(t, ttl, c, 1, 2)
		s := newSAServing()
		t.Cleanup(func() { s.ts.Close() })
		t.Cleanup(func() { s.awaitIdle(t, 5*time.Second) })

		url := s.ts.URL + "/v1/chat/completions"
		// Upstream 5xx: the request was ACCEPTED (lease held), terminated
		// with a rejection; the lease must live to its full idle TTL, not
		// die early.
		code, _, _ := saPost(t, url, saChatBody("err5", "E", "x"))
		if code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 502/500 passthrough", code)
		}
		// The lease idles only at handler completion (deferred release);
		// drain before snapshotting the lease state.
		s.awaitIdle(t, 3*time.Second)
		mgr := sessionAdmissions["err5"]
		kE := saAdmKey(t, "E", "err5")
		c.Advance(ttl - time.Millisecond)
		if held, since := mgr.snapshot(kE); !held || since.IsZero() {
			t.Fatalf("lease = held %v since %v, want idle-holding before TTL", held, since)
		}
		c.Advance(2 * time.Millisecond)
		if held, _ := mgr.snapshot(kE); held {
			t.Fatal("lease did not expire after its full idle TTL")
		}
	})

	t.Run("truncated-stream", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.Copy(io.Discard, r.Body)
			<-r.Context().Done()
		}))
		defer srv.Close()
		withRouterEnv(t,
			[]Backend{{Name: "cut", URL: srv.URL, MaxConcurrent: 1}},
			map[string]int{"cut": 1}, nil)
		proxies["cut"] = newTestProxy(srv.URL)
		saIsolate(t)
		c := newSAManualClock()
		withAdmissionEnv(t, ttl, c, 1, 2)
		s := newSAServing()
		t.Cleanup(func() { s.ts.Close() })
		t.Cleanup(func() { s.awaitIdle(t, 5*time.Second) })

		url := s.ts.URL + "/v1/chat/completions"
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, "POST", url, strings.NewReader(saChatBody("cut", "T", "x")))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		respCh := make(chan error, 1)
		go func() {
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				respCh <- err
				return
			}
			_, rerr := io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			respCh <- rerr
		}()
		// Let the request be admitted (lease minted) then abort: the
		// interrupted upstream stream tears the whole chain down.
		time.Sleep(200 * time.Millisecond)
		cancel()
		if rerr := <-respCh; rerr == nil {
			t.Fatal("aborted stream completed cleanly")
		}
		// The lease only idles when the proxy handler completes (its
		// deferred ticket release runs); wait for drain before asserting.
		s.awaitIdle(t, 3*time.Second)
		mgr := sessionAdmissions["cut"]
		kT := saAdmKey(t, "T", "cut")
		if held, since := mgr.snapshot(kT); !held || since.IsZero() {
			t.Fatalf("aborted request: lease = held %v since %v, want idle-holding", held, since)
		}
		c.Advance(ttl + 2*time.Millisecond)
		if held, _ := mgr.snapshot(kT); held {
			t.Fatal("aborted request lease outlived its full idle TTL")
		}
	})

	t.Run("backend-queue-rejection-idles-lease", func(t *testing.T) {
		// maxConcurrent 1 + maxQueueDepth 0: the second request (same key)
		// passes the lease gate, then is hard-rejected at the EXECUTION tier
		// (slot full, zero queue depth, waiting 0 >= max 0 → literal
		// "backend-queue-full"). The accepted request terminates with a
		// rejection — its lease must live out its full idle TTL.
		engine := newSAGateEngine()
		withRouterEnv(t,
			[]Backend{{Name: "bq", URL: engine.TS.URL, MaxConcurrent: 1, MaxQueueDepth: 0}},
			map[string]int{"bq": 1}, map[string]int{"bq": 0})
		proxies["bq"] = newTestProxy(engine.TS.URL)
		saIsolate(t)
		c := newSAManualClock()
		withAdmissionEnv(t, ttl, c, 2, 2)
		s := newSAServing()
		t.Cleanup(func() { s.ts.Close() })
		t.Cleanup(func() { engine.releaseAll() })
		t.Cleanup(func() { s.awaitIdle(t, 5*time.Second) })

		url := s.ts.URL + "/v1/chat/completions"
		a1Done := saPostAsync(t, url, saChatBody("bq", "E", "x"))
		time.Sleep(150 * time.Millisecond) // A1: running slot 1, gated upstream
		code, hdr, body := saPost(t, url, saChatBody("bq", "E", "y"))
		if code != http.StatusTooManyRequests || hdr.Get("X-Router-Reason") != "backend-queue-full" {
			t.Fatalf("A2 = %d %q, want 429 backend-queue-full: %s",
				code, hdr.Get("X-Router-Reason"), body)
		}
		if in, _, _, _ := slots["bq"].snapshot(); in != 1 {
			t.Fatalf("rejected A2 leaked an execution slot: inFlight=%d", in)
		}
		engine.releaseKey("E")
		if c0 := <-a1Done; c0 != 200 {
			t.Fatalf("A1 = %d", c0)
		}
		// Both completions are drained: the lease carries a single still-
		// idle accepted release — nothing else may have touched it.
		s.awaitIdle(t, 3*time.Second)
		mgr := sessionAdmissions["bq"]
		kE := saAdmKey(t, "E", "bq")
		if held, since := mgr.snapshot(kE); !held || since.IsZero() {
			t.Fatalf("rejected A2's lease = held %v since %v, want idle-holding", held, since)
		}
		c.Advance(ttl - time.Millisecond)
		if held, _ := mgr.snapshot(kE); !held {
			t.Fatal("lease vanished before its full idle TTL")
		}
		c.Advance(2 * time.Millisecond)
		if held, _ := mgr.snapshot(kE); held {
			t.Fatal("lease outlived its full idle TTL")
		}
	})

	t.Run("unreachable-backend-idles-lease", func(t *testing.T) {
		// Connection refused after admission: the accepted request
		// terminates with a 503; the lease idles exactly then and
		// expires after the full TTL.
		dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		deadURL := dead.URL
		dead.Close()
		withRouterEnv(t,
			[]Backend{{Name: "dead", URL: deadURL, MaxConcurrent: 2}},
			map[string]int{"dead": 2}, nil)
		proxies["dead"] = newTestProxy(deadURL)
		saIsolate(t)
		c := newSAManualClock()
		withAdmissionEnv(t, ttl, c, 1, 2)
		s := newSAServing()
		t.Cleanup(func() { s.ts.Close() })
		t.Cleanup(func() { s.awaitIdle(t, 5*time.Second) })

		url := s.ts.URL + "/v1/chat/completions"
		code, hdr, _ := saPost(t, url, saChatBody("dead", "U", "x"))
		if code != http.StatusServiceUnavailable || hdr.Get("X-Router-Reason") != "backend_unavailable" {
			t.Fatalf("status = %d reason %q, want 503 backend_unavailable", code, hdr.Get("X-Router-Reason"))
		}
		s.awaitIdle(t, 3*time.Second)
		mgr := sessionAdmissions["dead"]
		kU := saAdmKey(t, "U", "dead")
		if held, since := mgr.snapshot(kU); !held || since.IsZero() {
			t.Fatalf("unreachable request's lease = held %v since %v, want idle-holding", held, since)
		}
		c.Advance(ttl - time.Millisecond)
		if held, _ := mgr.snapshot(kU); !held {
			t.Fatal("lease vanished before its full idle TTL")
		}
		c.Advance(2 * time.Millisecond)
		if held, _ := mgr.snapshot(kU); held {
			t.Fatal("lease outlived its full idle TTL")
		}
	})

	t.Run("stream-truncated", func(t *testing.T) {
		// Genuine truncation: the client receives a partial SSE stream
		// (headers + one flushed event) and the upstream connection dies
		// mid-response (hijacked + closed, no grace shutdown involved).
		// Handler finalizes; the ticket releases exactly once and the
		// lease expires after the TTL.
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.Copy(io.Discard, r.Body)
			w.Header().Set("Content-Type", "text/event-stream")
			fl, _ := w.(http.Flusher)
			w.Write([]byte("data: {\"delta\":1}\n\n"))
			fl.Flush()
			hj, ok := w.(http.Hijacker)
			if !ok {
				return
			}
			conn, _, err := hj.Hijack()
			if err != nil {
				return
			}
			// Linger 0 makes Close() send an RST: the client's mid-stream
			// read fails with a real transport error (a bare FIN would be
			// a legal EOF for an unchunked response, indistinguishable
			// from completion). The reverse proxy unwinds with
			// http.ErrAbortHandler on that error — the standard server
			// recovers it and the test mux's deferred counter drains.
			if tcp, ok := conn.(*net.TCPConn); ok {
				_ = tcp.SetLinger(0)
			}
			conn.Close()
		}))
		defer srv.Close()
		withRouterEnv(t,
			[]Backend{{Name: "cut2", URL: srv.URL, MaxConcurrent: 1}},
			map[string]int{"cut2": 1}, nil)
		proxies["cut2"] = newTestProxy(srv.URL)
		saIsolate(t)
		c := newSAManualClock()
		withAdmissionEnv(t, ttl, c, 1, 2)
		s := newSAServing()
		t.Cleanup(func() { s.ts.Close() })
		t.Cleanup(func() { s.awaitIdle(t, 5*time.Second) })

		url := s.ts.URL + "/v1/chat/completions"
		body := `{"model":"cut2","prompt_cache_key":"ST","stream":true,"messages":[{"role":"user","content":"x"}]}`
		resp, err := http.Post(url, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
			t.Fatalf("content-type = %q, want text/event-stream", ct)
		}
		got, rerr := io.ReadAll(resp.Body)
		// The read must observably end in the truncation: a transport
		// failure/early EOF, never a clean completion — and the
		// delivered bytes are the partial stream (first event present,
		// no [DONE], no keepalive continuation).
		if rerr == nil {
			t.Fatalf("mid-stream read completed cleanly (%q), want a read error", got)
		}
		if !strings.Contains(string(got), `delta":1`) || strings.Contains(string(got), "data: [DONE]") {
			t.Fatalf("truncated stream = %q (read err: %v)", got, rerr)
		}
		// The lease idles only at handler completion; wait for drain.
		s.awaitIdle(t, 3*time.Second)
		mgr := sessionAdmissions["cut2"]
		kS := saAdmKey(t, "ST", "cut2")
		if held, since := mgr.snapshot(kS); !held || since.IsZero() {
			t.Fatalf("truncated stream: lease = held %v since %v, want idle-holding", held, since)
		}
		c.Advance(ttl + 2*time.Millisecond)
		if held, _ := mgr.snapshot(kS); held {
			t.Fatal("truncated-stream lease outlived its full idle TTL")
		}
	})
}

// ─── 5. Isolation, observability, routing ─────────────────────────────────

func TestSessionAdmission_BackendIsolationAndQueryModelRouting(t *testing.T) {
	const ttl = 5 * time.Second
	engine := newSAGateEngine()
	withRouterEnv(t, []Backend{
		{Name: "b1", URL: engine.TS.URL, MaxConcurrent: 1, MaxQueueDepth: 4},
		{Name: "b2", URL: engine.TS.URL, MaxConcurrent: 1, MaxQueueDepth: 4},
	}, map[string]int{"b1": 1, "b2": 1}, nil)
	proxies["b1"] = newTestProxy(engine.TS.URL)
	proxies["b2"] = newTestProxy(engine.TS.URL)
	saIsolate(t)
	c := newSAManualClock()
	withAdmissionEnv(t, ttl, c, 1, 2)
	s := newSAServingMux()
	t.Cleanup(func() { s.ts.Close() })
	t.Cleanup(func() { s.awaitIdle(t, 5*time.Second) })

	url := s.ts.URL + "/v1/chat/completions"

	// b1 gets "S" via the client key; b2 gets "S" via query-model routing
	// (no model field) — a DIFFERENT backend scope, so a different lease.
	done1 := make(chan int, 1)
	go func() { out, _ := saPostOutDo(url, saChatBody("b1", "S", "x")); done1 <- out.code }()
	time.Sleep(150 * time.Millisecond)
	engine.releaseKey("S") // b1's gate (it arrived first)
	select {
	case code1 := <-done1:
		if code1 != 200 {
			t.Fatalf("b1 = %d", code1)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("b1 request stuck")
	}
	done2 := saPostAsync(t, url+"?model=b2", saChatBody("", "S", "x"))
	time.Sleep(150 * time.Millisecond)
	// b2 stays IN flight (its gate stays closed): its lease is ACTIVE,
	// so it must survive b1's full idle TTL.
	if n1 := sessionAdmissionLeaseCount(t, sessionAdmissions["b1"]); n1 != 1 {
		t.Fatalf("b1 leases = %d, want 1 (client key S)", n1)
	}
	if n2 := sessionAdmissionLeaseCount(t, sessionAdmissions["b2"]); n2 != 1 {
		t.Fatalf("b2 leases = %d, want 1 (query-model routed)", n2)
	}
	// The real management surface (buildMetricsMux on this listener):
	// b2's accepted request is IN FLIGHT, so its row must report the
	// active-hold arm — lease held with no idle stamp (idle release
	// lands only at handler completion).
	if row := s.saSessionRow(t, saAdmKey(t, "S", "b2")); row == nil ||
		!row.AdmissionLease || row.AdmissionIdleSinceMs != 0 {
		t.Fatalf("live b2 row = %+v, want admissionLease=true/0 (active)", row)
	}
	// b1 already completed: its row reports the idle-held arm — lease
	// true with the manual clock's completion timestamp.
	if row := s.saSessionRow(t, saAdmKey(t, "S", "b1")); row == nil ||
		!row.AdmissionLease || row.AdmissionIdleSinceMs <= 0 {
		t.Fatalf("idle b1 row = %+v, want admissionLease=true/nonce", row)
	}
	c.Advance(ttl + 2*time.Millisecond)
	if held, _ := sessionAdmissions["b1"].snapshot(saAdmKey(t, "S", "b1")); held {
		t.Fatal("b1 lease outlived its full idle TTL")
	}
	if held, _ := sessionAdmissions["b2"].snapshot(saAdmKey(t, "S", "b2")); !held {
		t.Fatal("b2's active lease died under b1's expiry")
	}
	// Third endpoint arm: b1's lease expired on the manual clock, so its
	// row reports the absent arm (false/0) through the live endpoint.
	if row := s.saSessionRow(t, saAdmKey(t, "S", "b1")); row == nil ||
		row.AdmissionLease || row.AdmissionIdleSinceMs != 0 {
		t.Fatalf("expired b1 row = %+v, want admissionLease=false/0", row)
	}

	// Now b2 completes: ITS lease only becomes idle now, on its own clock.
	engine.releaseKey("S") // b2's gate
	if code2 := <-done2; code2 != 200 {
		t.Fatalf("b2 = %d", code2)
	}

	// Per-backend caps: b2's fresh key RECLAIMS b2's own idle lease
	// immediately (cap 1, no demand-queued waiters, no clock advance);
	// b1 (empty after its expiry) grants immediately on its own map.
	tk1, err := sessionAdmissions["b1"].acquire(context.Background(), saAdmKey(t, "Z", "b1"), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	out2 := make(chan *sessionAdmissionTicket, 1)
	go func() {
		tk2, _ := sessionAdmissions["b2"].acquire(context.Background(), saAdmKey(t, "Z", "b2"), time.Minute)
		out2 <- tk2
	}()
	select {
	case tk2 := <-out2:
		releaseTk2 := tk2
		defer releaseTk2.release()
	case <-time.After(2 * time.Second):
		t.Fatal("b2 fresh key not granted by the immediate reclaim")
	}
	mgr2 := sessionAdmissions["b2"]
	mgr2.mu.Lock()
	if mgr2.reclaimedTotal != 1 {
		t.Fatalf("b2 reclaimedTotal = %d, want 1 (S lease yielded to Z)", mgr2.reclaimedTotal)
	}
	mgr2.mu.Unlock()
	if held, _ := mgr2.snapshot(saAdmKey(t, "S", "b2")); held {
		t.Fatal("b2 S lease survived the reclaim")
	}
	// b1's grant is b1's independent lease; b2 must not touch it.
	if held, _ := sessionAdmissions["b1"].snapshot(saAdmKey(t, "Z", "b1")); !held {
		t.Fatal("b1 lease disturbed by b2's expiry")
	}
	tk1.release()
	c.Advance(ttl + 2*time.Millisecond)
	if held, _ := sessionAdmissions["b1"].snapshot(saAdmKey(t, "Z", "b1")); held {
		t.Fatal("b1 lease outlived its full idle TTL")
	}
	if held, _ := sessionAdmissions["b2"].snapshot(saAdmKey(t, "Z", "b2")); !held {
		t.Fatal("b2 active lease died under b1's expiry")
	}
}

// TestSessionAdmission_429EnvelopeParity asserts, at the HTTP layer, the
// FULL standard 429 shape for BOTH session-admission rejection reasons —
// not just the status and the reason field the shorter terminal-bounds
// asserts check: JSON content type, X-Router-Reason, X-Router-Backend,
// Retry-After, and a JSON body whose type/backend/retry_after fields
// mirror the header values — matching the other 429s in this router
// (blocked-by-tier0, backend-full, …), which all go through write429.
func TestSessionAdmission_429EnvelopeParity(t *testing.T) {
	const ttl = 5 * time.Second
	check := func(t *testing.T, code int, hdr http.Header, body, wantReason string) {
		t.Helper()
		if code != http.StatusTooManyRequests {
			t.Fatalf("status = %d, want 429 (%s): %s", code, wantReason, body)
		}
		if ct := hdr.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}
		if reason := hdr.Get("X-Router-Reason"); reason != wantReason {
			t.Errorf("X-Router-Reason = %q, want %s", reason, wantReason)
		}
		if be := hdr.Get("X-Router-Backend"); be != "smoke" {
			t.Errorf("X-Router-Backend = %q, want smoke", be)
		}
		if ra := hdr.Get("Retry-After"); ra == "" {
			t.Error("Retry-After header missing")
		}
		var env struct {
			Err struct {
				Type       string `json:"type"`
				Message    string `json:"message"`
				Backend    string `json:"backend"`
				RetryAfter int    `json:"retry_after"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(body), &env); err != nil {
			t.Fatalf("body %q is not the 429 JSON envelope: %v", body, err)
		}
		if env.Err.Type != wantReason {
			t.Errorf("body type = %q, want %s", env.Err.Type, wantReason)
		}
		if env.Err.Backend != "smoke" {
			t.Errorf("body backend = %q, want smoke", env.Err.Backend)
		}
		// retry_after mirrors the Retry-After header (both come from the
		// same estimateRetryAfter value in write429).
		var ra int
		if _, err := fmt.Sscanf(hdr.Get("Retry-After"), "%d", &ra); err != nil ||
			env.Err.RetryAfter != ra {
			t.Errorf("body retry_after = %d vs header %q", env.Err.RetryAfter, hdr.Get("Retry-After"))
		}
		if env.Err.Message != "backend at capacity" {
			t.Errorf("body message = %q, want the standard 429 message", env.Err.Message)
		}
	}

	t.Run("queue-full", func(t *testing.T) {
		s, engine, c := saHTTPSetup(t, ttl, 1, 1)
		_ = c
		url := s.ts.URL + "/v1/chat/completions"
		aDone := saPostAsync(t, url, saChatBody("smoke", "A", "x"))
		saPollAdmittedLeases(t, "smoke", 1)
		// Depth-1 queue: B queues, C is immediately full — reject #1.
		bDone := make(chan int, 1)
		go func() { out, _ := saPostOutDo(url, saChatBody("smoke", "B", "x")); bDone <- out.code }()
		saPollAdmissionWaiting(t, "smoke", 1)
		code, hdr, body := saPost(t, url, saChatBody("smoke", "C", "x"))
		check(t, code, hdr, body, "session-admission-queue-full")
		// A is ACTIVE (gated): its completion grants the queue head B
		// immediately — no idle arm, no clock advance, no TTL handoff.
		engine.releaseKey("A")
		if cA := <-aDone; cA != 200 {
			t.Fatalf("A = %d, want 200", cA)
		}
		// B's grant is a real-time async hop (wake → forward): wait for
		// B's forward to land upstream, then open ITS gate.
		saPollEngineSeen(t, engine, "A", "B")
		engine.releaseKey("B")
		select {
		case code := <-bDone:
			if code != 200 {
				t.Errorf("B after the release = %d, want 200", code)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("B not admitted by the final release")
		}
	})

	t.Run("timeout", func(t *testing.T) {
		s, engine, c := saHTTPSetup(t, ttl, 1, 2)
		url := s.ts.URL + "/v1/chat/completions"
		saPostAsync(t, url, saChatBody("smoke", "A", "x"))
		saPollAdmittedLeases(t, "smoke", 1)
		// B queues; its queueTimeout admission deadline (manual clock)
		// passes with no slot freed: the admission-timeout 429. The
		// request runs on a GOROUTINE — the envelope (headers + body)
		// is only written once the manual clock's Advance fires the
		// deadline, and that Advance belongs to the test goroutine, so
		// a synchronous POST would deadlock it.
		outCh := make(chan saPostOut, 1)
		go func() { out, _ := saPostOutDo(url, saChatBody("smoke", "B", "x")); outCh <- out }()
		saPollAdmissionWaiting(t, "smoke", 1)
		c.Advance(queueTimeout + time.Millisecond)
		var out saPostOut
		select {
		case out = <-outCh:
		case <-time.After(2 * time.Second):
			t.Fatal("B did not time out")
		}
		check(t, out.code, out.hdr, out.body, "session-admission-timeout")
		engine.releaseAll()
	})
}

// TestSessionAdmission_BypassBodyByteIdentity asserts that the admission
// gate NEVER reserializes the request: in the disabled (off) and unkeyed
// bypasses the fake upstream receives the request body byte-for-byte
// equal to the submitted bytes. The payload carries whitespace and
// opaque nested fields, so even a re-encode that preserves all VALUES
// (reordered keys, compacted spacing, dropped unknown fields) is caught.
// Both the non-streaming and streaming paths are covered: the router
// forwards bodies on both (stream:true is an upstream body field, not a
// router decision) — only the serving stack differs (a fresh router
// listener per subtest, since the admission env is a process global).
func TestSessionAdmission_BypassBodyByteIdentity(t *testing.T) {
	const payload = `{"model": "b",
	"prompt_cache_key": " conv key ",
	"messages": [{"role": "user",
	               "content": "ops  space  $€œ"}],
	"opaque": {"nested": [1, 2, {"k": null, "s": "  keep  "}],
	            "list": []}}`

	capture := func(t *testing.T) (*httptest.Server, func(), chan []byte) {
		t.Helper()
		gotCh := make(chan []byte, 1)
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// The exact bytes the router handed to the forwarder: the
			// client submits `body`, so any asymmetry here is reserialize
			// drift in the router's read -> forward path.
			b, _ := io.ReadAll(r.Body)
			select {
			case gotCh <- b:
			default:
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"ok":true}`)
		}))
		return up, up.Close, gotCh
	}

	for _, tc := range []struct {
		name   string
		keyed  bool // admission enabled with a key (must stay bypassed: cap 1 but two forwards)
		stream bool
	}{
		{name: "off-nonstream", keyed: false, stream: false},
		{name: "off-stream", keyed: false, stream: true},
		{name: "unkeyed-nonstream", keyed: true, stream: false},
		{name: "unkeyed-stream", keyed: true, stream: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up, upClose, gotCh := capture(t)
			name := "by"
			body := payload
			if tc.name == "off-nonstream" || tc.name == "off-stream" {
				// Disabled: the body KEEPS its prompt_cache_key — the
				// gate must ignore it entirely (legacy per-request path).
			} else {
				// Unkeyed bypass: the body has NO prompt_cache_key, so
				// the enabled gate has no lease identity — it must let
				// BOTH requests forward (a keyed body would queue the
				// second behind the first's retained lease at cap 1).
				body = strings.Replace(payload, `"prompt_cache_key": " conv key ",
`, "", 1)
			}
			if tc.stream {
				body = strings.Replace(body, `"messages"`, `"stream": true, "messages"`, 1)
			}
			withRouterEnv(t, []Backend{{Name: name, URL: up.URL, MaxConcurrent: 2}},
				map[string]int{name: 2}, nil)
			proxies[name] = newTestProxy(up.URL)
			saIsolate(t)
			if tc.keyed {
				c := newSAManualClock()
				withAdmissionEnv(t, 5*time.Second, c, 1, 2)
			}
			s := newSAServing()
			t.Cleanup(func() { s.ts.Close() })
			t.Cleanup(upClose)
			t.Cleanup(func() { s.awaitIdle(t, 5*time.Second) })

			for range 2 {
				done := make(chan int, 1)
				go func() { out, _ := saPostOutDo(s.ts.URL+"/v1/chat/completions", body); done <- out.code }()
				select {
				case got := <-gotCh:
					if !bytes.Equal(got, []byte(body)) {
						t.Fatalf("forwarded body drifts from submission:\n got: %s\nwant: %s", got, body)
					}
				case <-time.After(2 * time.Second):
					t.Fatal("upstream never received the request")
				}
				select {
				case code := <-done:
					if code != 200 {
						t.Fatalf("status = %d, want 200", code)
					}
				case <-time.After(2 * time.Second):
					t.Fatal("request did not complete")
				}
			}
			// Bypass certification: with the gate ENABLED (armed) and
			// an UNKEYED body (cap 1), the second request was not
			// queued behind the first — no queueing, no lease: the
			// unkeyed path never entered the admission gate.
			if m, ok := sessionAdmissions[name]; ok {
				if n := sessionAdmissionLeaseCount(t, m); n != 0 {
					t.Fatalf("bypass minted %d leases", n)
				}
			}
		})
	}
}

func TestSessionAdmission_UnkeyedAndOffBypass(t *testing.T) {
	t.Run("off-legacy-grouping", func(t *testing.T) {
		engine := newSAGateEngine()
		withRouterEnv(t, []Backend{{Name: "off", URL: engine.TS.URL, MaxConcurrent: 2}},
			map[string]int{"off": 2}, nil)
		proxies["off"] = newTestProxy(engine.TS.URL)
		saIsolate(t)
		s := newSAServing()
		t.Cleanup(func() { s.ts.Close() })
		t.Cleanup(func() { s.awaitIdle(t, 5*time.Second) })

		url := s.ts.URL + "/v1/chat/completions"
		for i := 0; i < 2; i++ {
			done := saPostAsync(t, url, saChatBody("off", fmt.Sprintf("K%d", i), "x"))
			time.Sleep(150 * time.Millisecond)
			engine.releaseKey(fmt.Sprintf("K%d", i))
			if code := <-done; code != 200 {
				t.Fatalf("off mode: status %d", code)
			}
		}
		s.awaitIdle(t, 2*time.Second)
		feed := sessions.feed()
		if len(feed.Sessions) != 1 {
			t.Fatalf("legacy grouping: %d rows, want 1 (shared 12-hex content key)", len(feed.Sessions))
		}
		if row := feed.Sessions[0]; len(row.ID) != 12 || row.AdmissionLease || row.AdmissionIdleSinceMs != 0 {
			t.Fatalf("legacy row = %+v, want 12-hex and no lease flags", row)
		}
	})

	t.Run("unkeyed-bypass", func(t *testing.T) {
		engine := newSAGateEngine()
		c := newSAManualClock()
		withRouterEnv(t, []Backend{{Name: "unk", URL: engine.TS.URL, MaxConcurrent: 2}},
			map[string]int{"unk": 2}, nil)
		proxies["unk"] = newTestProxy(engine.TS.URL)
		saIsolate(t)
		withAdmissionEnv(t, ttlNoop(5*time.Second), c, 1, 2)
		s := newSAServing()
		t.Cleanup(func() { s.ts.Close() })
		t.Cleanup(func() { s.awaitIdle(t, 5*time.Second) })

		url := s.ts.URL + "/v1/chat/completions"
		for i := 0; i < 2; i++ {
			// No prompt_cache_key: admitted with no lease; the cap is NOT
			// enforced on unkeyed traffic (legacy behavior retained).
			done := saPostAsync(t, url, saChatBody("unk", "", "x"))
			time.Sleep(150 * time.Millisecond)
			engine.releaseKey("")
			if code := <-done; code != 200 {
				t.Fatalf("unkeyed: status %d", code)
			}
		}
		s.awaitIdle(t, 2*time.Second)
		mgr := sessionAdmissions["unk"]
		mgr.mu.Lock()
		n := len(mgr.leaseMap)
		mgr.mu.Unlock()
		if n != 0 {
			t.Fatalf("unkeyed requests minted %d leases", n)
		}
	})
}

func ttlNoop(d time.Duration) time.Duration { return d }

// saLeaseCount is the admission helpers: len(leaseMap) under the
// manager's own lock.
func sessionAdmissionLeaseCount(t *testing.T, m *sessionAdmission) int {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.leaseMap)
}

func TestSessionAdmission_FeedEvictionKeepsLeaseAuthority(t *testing.T) {
	const ttl = 5 * time.Second
	c := newSAManualClock()
	a := newSessionAdmission(1, 2, ttl, c)
	r := newSessionRegistry()
	// Fill past the store cap with evictable (dead) rows so the
	// observability feed evicts aggressively.
	for i := range 520 {
		id := r.start("x", "/v1/chat/completions", false, fmt.Sprintf("row%03d", i), 10, 4)
		r.finish(id, 200, 1, 0, 0)
	}
	if got := len(r.feed().Sessions); got > sessFeedCap {
		t.Fatalf("feed rows = %d, over cap %d", got, sessFeedCap)
	}
	// kA holds the single slot; releasing it makes it idle at T0.
	tkA, err := a.acquire(context.Background(), "kA", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	tkA.release()
	// kB is an admitted-but-unstarted identity (no r.start): absent from
	// the live/aggregate map; the lease is independent authority. It is
	// a live demand, so it RECLAIMS the idle kA lease immediately (cap
	// 1) — no expiry, no clock advance — while the feed stores evict.
	type acqRes struct {
		tk  *sessionAdmissionTicket
		err error
	}
	outB := make(chan acqRes, 1)
	go func() {
		tkB, errB := a.acquire(context.Background(), "kB", time.Minute)
		outB <- acqRes{tkB, errB}
	}()
	time.Sleep(100 * time.Millisecond)
	if row := saAdmRow(r.feed(), saAdmKey(t, "B", "x")); row != nil {
		t.Fatal("admitted-but-unforwarded request appeared as a feed row")
	}
	resB := <-outB
	if resB.err != nil {
		t.Fatalf("kB not granted by the immediate reclaim: %v", resB.err)
	}
	tk := resB.tk
	a.mu.Lock()
	if a.reclaimedTotal != 1 {
		t.Fatalf("reclaimedTotal = %d, want 1 (kA yielded to kB)", a.reclaimedTotal)
	}
	a.mu.Unlock()
	if held, _ := a.snapshot("kA"); held {
		t.Fatal("kA lease survived the reclaim")
	}
	if held, _ := a.snapshot("kB"); !held {
		t.Fatal("kB lease vanished with no traffic")
	}
	tk.release()
	c.Advance(ttl + 2*time.Millisecond)
	if held, _ := a.snapshot("kB"); held {
		t.Fatal("kB lease outlived its TTL")
	}
}

// TestSessionAdmission_GateTelemetry pins the admission-gate telemetry
// (exact lifetime gate-wait sum/count/mean, and the rejection-reason
// breakdown) that the capacity panel reads. The trailing-30s window lives
// in the metrics registry (queueWait.admission), not the manager.
func TestSessionAdmission_GateTelemetry(t *testing.T) {
	a := newSessionAdmission(2, 2, 10*time.Second, newSAManualClock())

	// No traffic: all-zero frame, no phantom samples.
	f := a.aggregateSnapshot()
	if f.GateWaitTotalMs != 0 || f.GateWaitCount != 0 || f.GateWaitMs != 0 ||
		f.RejQueueFull != 0 || f.RejQueueTimeout != 0 || f.RejClientCancel != 0 {
		t.Fatalf("empty telemetry = %+v, want all zero", f)
	}

	// Two granted gate-waits: 100ms + 250ms. Truncation is exact for whole
	// milliseconds, so total = 350, count = 2, mean = 175 (integer divide).
	a.recordGateWait(100 * time.Millisecond)
	a.recordGateWait(250 * time.Millisecond)
	f = a.aggregateSnapshot()
	if f.GateWaitTotalMs != 350 || f.GateWaitCount != 2 || f.GateWaitMs != 175 {
		t.Fatalf("gate wait = total %d, count %d, mean %d; want 350/2/175",
			f.GateWaitTotalMs, f.GateWaitCount, f.GateWaitMs)
	}

	// Reason breakdown: each recognized reason bumps its own counter; an
	// unknown reason is ignored (a downstream-gate reason like gpu-budget-full
	// lives in the registry's per-backend map, not the admission frame).
	a.recordGateRejection("session-admission-queue-full")
	a.recordGateRejection("session-admission-queue-full")
	a.recordGateRejection("session-admission-timeout")
	a.recordGateRejection("client-cancel")
	a.recordGateRejection("gpu-budget-full") // not an admission-gate reason
	f = a.aggregateSnapshot()
	if f.RejQueueFull != 2 || f.RejQueueTimeout != 1 || f.RejClientCancel != 1 {
		t.Fatalf("reasons = full %d, timeout %d, cancel %d; want 2/1/1",
			f.RejQueueFull, f.RejQueueTimeout, f.RejClientCancel)
	}
}

// saPollWaiting polls a standalone manager's live waiter count to a value.
// (saPollAdmissionWaiting is keyed on the sessionAdmissions registry, which
// these direct-manager tests do not populate.)
func saPollWaiting(t *testing.T, a *sessionAdmission, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		a.mu.Lock()
		got := a.waiting
		a.mu.Unlock()
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("admission waiting = %d, want %d", got, want)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// TestSessionAdmission_StaleIdleExpiryAfterReclaim pins the identity/
// generation guard in idleExpiry. reclaimOldestIdleLocked stops the victim's
// timer, bumps its generation, and deletes the key — so a callback that lost
// the Stop race must no-op when invoked with the stale (lease, gen) pair it
// captured. The manual clock's Stop marks timers stopped and Advance skips
// them, making the guard unreachable through the clock, so the callback is
// invoked DIRECTLY with its captured closure args.
//
// The strongest arm: after the reclaim, a SUCCESSOR lease is granted under
// the SAME key. idleExpiry's `cur != lease` branch (not the `!ok` branch) is
// what must catch the stale pointer, because the key is now populated by a
// different lease object. A guard that only checked key presence would
// evict the successor here.
func TestSessionAdmission_StaleIdleExpiryAfterReclaim(t *testing.T) {
	const ttl = 30 * time.Second
	ctx := context.Background()
	c := newSAManualClock()
	a := newSessionAdmission(1, 2, ttl, c)

	// 1) kA is acquired (active) then released (idle, timer armed, gen=1).
	tA, _ := a.acquire(ctx, "kA", time.Minute)
	tA.release()
	// Capture the victim lease pointer and the generation the armed timer
	// closure captured at rearm (gen=1 after the release bump).
	a.mu.Lock()
	victim := a.leaseMap["kA"]
	staleGen := victim.gen
	a.mu.Unlock()
	if victim == nil {
		t.Fatal("kA lease missing after release")
	}

	// 2) A fresh immediate arrival reclaims the oldest idle (kA): its timer
	// is stopped, generation bumped (gen 1 -> 2), and the key deleted.
	tC, err := a.acquire(ctx, "kC", time.Minute)
	if err != nil {
		t.Fatalf("kC acquire: %v", err)
	}
	if held, _ := a.snapshot("kA"); held {
		t.Fatal("kA survived the reclaim")
	}

	// 3) Release kC (now idle) and re-acquire kA: the SUCCESSOR lease is
	// granted under the same key, reclaiming kC. leaseMap["kA"] now points
	// at a NEW lease object distinct from the victim.
	tC.release()
	tA2, err := a.acquire(ctx, "kA", time.Minute)
	if err != nil {
		t.Fatalf("kA successor acquire: %v", err)
	}
	a.mu.Lock()
	succ := a.leaseMap["kA"]
	rec := a.reclaimedTotal
	a.mu.Unlock()
	if succ == victim {
		t.Fatal("successor is the same object as the victim; test cannot prove the identity guard")
	}
	if rec != 2 {
		t.Fatalf("reclaimedTotal = %d, want 2 (kA then kC)", rec)
	}

	// 4) Fire the stale callback with the captured (victim, staleGen) pair.
	// The guard (cur != lease, and victim.gen != staleGen) must no-op.
	a.idleExpiry(victim, staleGen)

	// 5) The successor survives: present, intact, and its ticket is usable.
	a.mu.Lock()
	cur, ok := a.leaseMap["kA"]
	count, idle := 0, false
	if ok {
		count, idle = cur.count, cur.idle
	}
	a.mu.Unlock()
	if !ok || cur != succ {
		t.Fatalf("successor evicted by stale expiry: present=%v same=%v", ok, cur == succ)
	}
	if count != 1 || idle {
		t.Fatalf("successor state corrupted: count=%d idle=%v, want 1/false", count, idle)
	}
	if tA2.lease != succ {
		t.Fatal("successor ticket does not point at the surviving lease")
	}
	// The ticket is still usable: a clean release transitions the successor
	// to idle (a no-op eviction would have already destroyed it).
	tA2.release()
	f := a.aggregateSnapshot()
	if f.Active != 0 || f.Idle != 1 {
		t.Fatalf("post-release fleet = active %d idle %d, want 0/1", f.Active, f.Idle)
	}
}

// TestSessionAdmission_AggregateWaitingOldest pins the non-zero arms of the
// admission snapshot's queue pressure: Waiting counts live waiters and
// OldestWaitMs is their maximum age (measured on the manager clock). Existing
// telemetry tests only ever assert Waiting==0; this drives two waiters 5 ms
// apart on a full-cap manager, then cancels the older one and confirms the
// figure follows the younger waiter's age.
func TestSessionAdmission_AggregateWaitingOldest(t *testing.T) {
	const ttl = 10 * time.Second
	ctx := context.Background()
	c := newSAManualClock()
	// Cap 1, up to 2 queued: kA occupies the only slot; two fresh identities
	// block behind an active lease (nothing reclaimable).
	a := newSessionAdmission(1, 2, ttl, c)
	tA, _ := a.acquire(ctx, "kA", time.Minute)
	defer tA.release()

	// Older waiter enqueued at the current clock instant (T0).
	w1ctx, cancelW1 := context.WithCancel(ctx)
	defer cancelW1()
	w1 := make(chan error, 1)
	go func() { _, err := a.acquire(w1ctx, "kW1", time.Minute); w1 <- err }()
	saPollWaiting(t, a, 1)

	// Advance the manager clock 5 ms, then enqueue the younger waiter at
	// T0+5ms: its age is 5 ms behind the older waiter's.
	c.Advance(5 * time.Millisecond)
	w2ctx, cancelW2 := context.WithCancel(ctx)
	defer cancelW2()
	w2 := make(chan error, 1)
	go func() { _, err := a.acquire(w2ctx, "kW2", time.Minute); w2 <- err }()
	saPollWaiting(t, a, 2)

	// Both live: Waiting=2, Oldest=5ms (T0+5 - T0).
	f := a.aggregateSnapshot()
	if f.Waiting != 2 {
		t.Fatalf("waiting = %d, want 2 (two live waiters)", f.Waiting)
	}
	if f.OldestWaitMs != 5 {
		t.Fatalf("oldestWaitMs = %d, want 5 (older waiter's age)", f.OldestWaitMs)
	}

	// Cancel the OLDER waiter: it leaves the count and the oldest figure
	// together (dead waiters count nowhere). Advance 3 ms so the younger
	// waiter's age is a distinct, non-zero figure.
	cancelW1()
	saPollWaiting(t, a, 1)
	c.Advance(3 * time.Millisecond)
	f = a.aggregateSnapshot()
	if f.Waiting != 1 {
		t.Fatalf("waiting after cancel = %d, want 1 (older waiter gone)", f.Waiting)
	}
	if f.OldestWaitMs != 3 {
		t.Fatalf("oldestWaitMs after cancel = %d, want 3 (younger waiter's age)", f.OldestWaitMs)
	}

	// Drain: cancel the younger waiter and confirm the queue empties.
	cancelW2()
	saPollWaiting(t, a, 0)
}
