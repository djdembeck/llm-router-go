package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

func saPost(t *testing.T, url, body string) (int, http.Header, string) {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, string(data)
}

// saPostAsync issues the request on a goroutine: required for gated
// engines (the upstream response only lands once the test releases the
// gate, so a synchronous POST would deadlock the test).
func saPostAsync(t *testing.T, url, body string) chan int {
	t.Helper()
	out := make(chan int, 1)
	go func() {
		code, _, _ := saPost(t, url, body)
		out <- code
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
		handleProxy(w, r)
		s.mu.Lock()
		s.cnt--
		s.cv.Signal()
		s.mu.Unlock()
	})
	s.ts = httptest.NewServer(mux)
	return s
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
// env for backend "smoke" (maxConcurrent 2, queue 4). Cleanup LIFO:
// drain handlers → release engine gates → close the server.
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
		// 0 = inherit each backend's maxConcurrent; per-backend resolution
		// happens in main(), not in the loader.
		cfg, err := loadSessionAdmissionConfig(saEnv("SESSION_ADMISSION", "on"), limited)
		if err != nil || cfg.MaxAdmitted != 0 || cfg.MaxWaiting != 4 ||
			cfg.IdleTTL != 5*time.Minute || cfg.KeySource != "prompt_cache_key" {
			t.Fatalf("cfg = %+v, %v", cfg, err)
		}
	})
	t.Run("zero-queue-inherits-2", func(t *testing.T) {
		cfg, err := loadSessionAdmissionConfig(saEnv("SESSION_ADMISSION", "on"),
			[]Backend{{Name: "z", MaxConcurrent: 2, MaxQueueDepth: 0}})
		if err != nil || cfg.MaxWaiting != 2 {
			t.Fatalf("waiting = %+v %v", cfg, err)
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

func TestSessionAdmission_ExpiryTimerReleasesWaiters(t *testing.T) {
	const ttl = 5 * time.Second
	c := newSAManualClock()
	a := newSessionAdmission(1, 3, ttl, c)
	ctx := context.Background()

	tA, _ := a.acquire(ctx, "kA", time.Minute)
	tA.release() // idle now; cap 1 occupies it for a full TTL
	out := make(chan *sessionAdmissionTicket, 1)
	go func() {
		t3, _ := a.acquire(ctx, "kO", time.Minute)
		out <- t3
	}()
	time.Sleep(50 * time.Millisecond)
	select {
	case <-out:
		t.Fatal("newcomer leaked through a retained idle lease")
	case <-time.After(5 * time.Millisecond):
	}
	// The expiry timer — no new request, no snapshot — fires the queue.
	c.Advance(ttl + time.Millisecond)
	select {
	case tk := <-out:
		tk.release()
	case <-time.After(2 * time.Second):
		t.Fatal("expiry timer did not release the newcomer")
	}
	if held, _ := a.snapshot("kA"); held {
		t.Fatal("kA lease back after expiry")
	}
}

// Clock-only FIFO progression: A's idle expiry freely grants a slot,
// and the queue head — C, confirmed enqueued before D arrives — must be
// the one admitted. D's completion follows in the second expiry tick.
func TestSessionAdmission_FIFOOrderWithoutTraffic(t *testing.T) {
	const ttl = 10 * time.Second
	c := newSAManualClock()
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
	tB, _ := a.acquire(ctx, "kB", time.Minute)
	tA.release()
	tB.release() // both idle; A idled first

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

	// Both idle expiries are due in the same manual tick: A armed first,
	// so A's drain runs first and grants the queue head C on the freed
	// slot; B's drain then frees the last slot and grants D. Both grants
	// publish in one Advance, so completions race on the channel —
	// assert the full set, order-insensitively.
	c.Advance(ttl + time.Millisecond)
	done := map[string]bool{}
	deadline := time.After(2 * time.Second)
	for len(done) < 2 {
		select {
		case got := <-out:
			done[got] = true
		case <-deadline:
			t.Fatalf("expiries admitted %v, want C and D", done)
		}
	}
}

func TestSessionAdmission_FIFOSameIdentityPastBlocked(t *testing.T) {
	// C/D/C with cap 1: D blocks on the full lease; the LATER C (same
	// identity) slips past D to the held lease and is granted. D's
	// deadline is 1 minute on the manual clock, so keep the TTL well
	// below it: D must not time out before the C lease expires.
	const ttl = 10 * time.Second
	c := newSAManualClock()
	a := newSessionAdmission(1, 5, ttl, c)
	ctx := context.Background()

	tC1, _ := a.acquire(ctx, "kC", time.Minute)
	dOut := make(chan error, 1)
	go func() { _, e := a.acquire(ctx, "kD", time.Minute); dOut <- e }()
	cOut := make(chan *sessionAdmissionTicket, 1)
	go func() {
		t3, e := a.acquire(ctx, "kC", time.Minute)
		cOut <- t3
		_ = e
	}()
	time.Sleep(50 * time.Millisecond)
	select {
	case tk := <-cOut:
		tk.release()
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
	// An identity whose lease fully expired is a NEW identity: it cannot
	// skip earlier queued newcomers.
	const ttl = 5 * time.Second
	c := newSAManualClock()
	a := newSessionAdmission(1, 4, ttl, c)
	ctx := context.Background()

	tA, _ := a.acquire(ctx, "kA", time.Minute)
	tA.release()
	cOut := make(chan *sessionAdmissionTicket, 1)
	go func() { tk, _ := a.acquire(ctx, "kC", time.Minute); cOut <- tk }()
	c.Advance(ttl + time.Millisecond)
	select {
	case tk := <-cOut:
		// C's turn completes immediately: its lease goes idle NOW; the
		// full idle TTL from this point must elapse before anything else
		// (A) may take the slot.
		tk.release()
	case <-time.After(2 * time.Second):
		t.Fatal("C not released on A's expiry")
	}
	aOut := make(chan *sessionAdmissionTicket, 1)
	go func() {
		tk, _ := a.acquire(ctx, "kA", time.Minute)
		aOut <- tk
	}()
	time.Sleep(50 * time.Millisecond)
	select {
	case <-aOut:
		t.Fatal("expired A leapt over newcomer C in the queue")
	case <-time.After(50 * time.Millisecond):
	}
	// C's ticket was already received when the expiry timer granted it
	// above (it enqueued only while A's lease existed); release it now so
	// its idle expiry is the event that frees the slot for A.
	c.Advance(ttl + 2*time.Millisecond) // C's full TTL elapses; grant publication is asynchronous to the clock advancement.
	select {
	case tk := <-aOut:
		tk.release()
	case <-time.After(2 * time.Second):
		t.Fatal("A did not enter after C's expiration")
	}
}

func TestSessionAdmission_CancellationAndTimeout(t *testing.T) {
	const ttl = 10 * time.Second
	c := newSAManualClock()
	a := newSessionAdmission(1, 4, ttl, c)
	ctx := context.Background()

	// kA granted then released: idle lease occupies the only slot; C and
	// D queue. Cancelling C must not admit D; the expiry timer does.
	tA, _ := a.acquire(ctx, "kA", time.Minute)
	tA.release()
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
		t.Fatal("D admitted while the retained kA lease still occupied the slot")
	case <-time.After(50 * time.Millisecond):
	}
	c.Advance(ttl + time.Millisecond)
	select {
	case tk := <-dOut:
		tk.release()
	case <-time.After(2 * time.Second):
		t.Fatal("D not admitted after the kA lease expired")
	}

	// Queue deadline (hotelTimeout semantics measured on the manager
	// clock) terminates without admission.
	toOut := make(chan error, 1)
	go func() { _, err := a.acquire(ctx, "kTO", 3*time.Second); toOut <- err }()
	time.Sleep(50 * time.Millisecond)
	c.Advance(ttl * 2) // beyond kTO's 3s deadline; kD lease (running) survives
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
	tA, _ := a.acquire(context.Background(), "kA", time.Minute)
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
	// maxWaiting=0 rejects all newcomers once the cap is occupied.
	b := newSessionAdmission(1, 0, ttl, c)
	bA, _ := b.acquire(context.Background(), "kA", time.Minute)
	if _, err := b.acquire(context.Background(), "kN", time.Minute); !errors.Is(err, errSessionAdmissionQueueFull) {
		t.Fatalf("waiting=0 err = %v, want queue-full", err)
	}
	bA.release()
	// kA's expiry frees the slot; queued B finally proceeds.
	tA.release()
	c.Advance(ttl + time.Millisecond)
	select {
	case err := <-first:
		if err != nil {
			t.Fatalf("B terminal = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("B not admitted after the cap lease expired")
	}
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
	const ttl = 5 * time.Second
	s, engine, c := saHTTPSetup(t, ttl, 2, 4)
	url := s.ts.URL + "/v1/chat/completions"

	for _, k := range []string{"A", "B"} {
		out := saPostAsync(t, url, saChatBody("smoke", k, "hello"))
		time.Sleep(150 * time.Millisecond) // let the request land in the gate
		engine.releaseKey(k)
		if code := <-out; code != 200 {
			t.Fatalf("key %s status = %d", k, code)
		}
	}
	s.awaitIdle(t, 2*time.Second)
	ka := saAdmKey(t, "A", "smoke")
	kb := saAdmKey(t, "B", "smoke")
	feed := sessions.feed()
	if row := saAdmRow(feed, ka); row == nil || !row.AdmissionLease || row.AdmissionIdleSinceMs <= 0 {
		t.Fatalf("A row = %+v, want idle held", row)
	}
	if row := saAdmRow(feed, kb); row == nil || !row.AdmissionLease || row.AdmissionIdleSinceMs <= 0 {
		t.Fatalf("B row = %+v, want idle held", row)
	}

	// C blocks on the admission gate: it must hold no GPU, execution, or
	// prefill permit.
	cDone := make(chan int, 1)
	go func() { code, _, _ := saPost(t, url, saChatBody("smoke", "C", "hello")); cDone <- code }()
	time.Sleep(200 * time.Millisecond)
	if st, err := http.Get(s.ts.URL + "/stats"); err == nil {
		sb, _ := io.ReadAll(st.Body)
		st.Body.Close()
		var stats struct {
			Backends []struct {
				InFlight        int32 `json:"inFlight"`
				Waiting         int32 `json:"waiting"`
				PrefillInFlight int32 `json:"prefillInFlight"`
				PrefillWaiting  int32 `json:"prefillWaiting"`
			} `json:"backends"`
		}
		if json.Unmarshal(sb, &stats) == nil && len(stats.Backends) == 1 {
			if b := stats.Backends[0]; b.InFlight != 0 || b.Waiting != 0 ||
				b.PrefillInFlight != 0 || b.PrefillWaiting != 0 {
				t.Fatalf("queued C leaked downstream permits: %+v", b)
			}
		} else {
			t.Fatalf("stats shape: %s", sb)
		}
	} else {
		t.Fatal(err)
	}
	if n := len(engine.Seen()); n != 2 {
		t.Fatalf("C forwarded pre-lease: %v", engine.Seen())
	}

	// The expiry timer (not a new request) frees a slot; C proceeds.
	c.Advance(ttl + time.Millisecond)
	engine.releaseKey("C")
	select {
	case code := <-cDone:
		if code != 200 {
			t.Fatalf("C status = %d", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("C not admitted by the lease expiry timer")
	}
	if got := engine.Seen(); len(got) != 3 || got[2] != "C" {
		t.Fatalf("seen = %v, want [A B C]", got)
	}
	s.awaitIdle(t, 2*time.Second)
	feed = sessions.feed()
	if row := saAdmRow(feed, ka); row == nil || row.AdmissionLease || row.AdmissionIdleSinceMs != 0 {
		t.Fatalf("expired A row = %+v, want false/0", row)
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
	url := s.ts.URL + "/v1/chat/completions"

	for _, k := range []string{"A", "B"} {
		done := saPostAsync(t, url, saChatBody("smoke", k, "hello"))
		time.Sleep(150 * time.Millisecond)
		engine.releaseKey(k)
		if code := <-done; code != 200 {
			t.Fatalf("key %s = %d", k, code)
		}
	}
	s.awaitIdle(t, 2*time.Second)

	// C queues against the two retained idle leases. A's next turn must
	// reach the fake upstream while the cap is occupied and C is queued.
	cDone := make(chan int, 1)
	go func() { code, _, _ := saPost(t, url, saChatBody("smoke", "C", "hello")); cDone <- code }()
	turn2 := saPostAsync(t, url, saChatBody("smoke", "A", "turn two"))
	time.Sleep(150 * time.Millisecond)
	// Turn 1's gate opens: the returning holder COMPLETES before the
	// retention check. (Turn 2 later gets its OWN fresh-identity lease
	// and gate once the retained set expires on the manual clock.)
	engine.releaseKey("A")
	if code := <-turn2; code != 200 {
		t.Fatalf("returning A status = %d", code)
	}
	// C's handler is intentionally queued in the manager at this point:
	// do NOT awaitIdle here (no drain is possible while it waits).
	select {
	case <-cDone:
		t.Fatal("C released before the retained leases expired")
	case <-time.After(100 * time.Millisecond):
	}
	c.Advance(ttl + 2*time.Millisecond)
	engine.releaseKey("C")
	select {
	case code := <-cDone:
		if code != 200 {
			t.Fatalf("C = %d", code)
		}
	case <-time.After(2 * time.Second):
		mgr := sessionAdmissions["smoke"]
		mgr.mu.Lock()
		le, ok := mgr.leaseMap[saAdmKey(t, "C", "smoke")]
		var leInfo any = "absent"
		if ok {
			leInfo = fmt.Sprintf("idle=%v count=%v", le.idle, le.count)
		}
		q, w := mgr.queue.Len(), mgr.waiting
		names := map[string]bool{}
		for e := mgr.queue.Front(); e != nil; e = e.Next() {
			names[e.Value.(*sessionAdmissionWaiter).key] = true
		}
		var leaseKeys []string
		for k := range mgr.leaseMap {
			leaseKeys = append(leaseKeys, k)
		}
		mgr.mu.Unlock()
		t.Fatalf("C not admitted after TTL; leases=%v(kC state=%v) queue=%d waiting=%d\nqueues=%v",
			leaseKeys, leInfo, q, w, names)
	}
}

func TestSessionAdmission_AcceptedLifetimeHold(t *testing.T) {
	const ttl = 3 * time.Second
	s, engine, c := saHTTPSetup(t, ttl, 2, 4)
	url := s.ts.URL + "/v1/chat/completions"

	// A1 takes an execution slot; A2 (same key) is lease-admitted and
	// QUEUED at the backend slot (maxConcurrent 2 both run; the second one
	// stays a QUEUED ACCEPTED request: admitted, not yet running).
	a1Done := make(chan int, 1)
	go func() { code, _, _ := saPost(t, url, saChatBody("smoke", "A", "one")); a1Done <- code }()
	time.Sleep(100 * time.Millisecond)
	a2Done := make(chan int, 1)
	go func() { code, _, _ := saPost(t, url, saChatBody("smoke", "A", "two")); a2Done <- code }()
	time.Sleep(100 * time.Millisecond)
	cDone := make(chan int, 1)
	go func() { code, _, _ := saPost(t, url, saChatBody("smoke", "C", "x")); cDone <- code }()

	// Far past the TTL: two outstanding accepted A requests hold the
	// lease; C (new identity) must not be invested.
	c.Advance(ttl * 10)
	if n := len(engine.Seen()); n != 2 {
		t.Fatalf("engine seen = %v, want two A requests", engine.Seen())
	}
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
	c.Advance(ttl * 10)
	select {
	case <-cDone:
		t.Fatal("C admitted while A2 still runs")
	case <-time.After(50 * time.Millisecond):
	}
	engine.releaseKey("A") // A2 done: the lease goes idle NOW, not earlier
	select {
	case code := <-a2Done:
		if code != 200 {
			t.Fatalf("A2 = %d", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("A2 did not complete")
	}
	// A2's deferred release arms the idle expiry timer strictly after
	// the client read; C is still queued (no drain possible), so wait
	// for A's lease to report idle before the advance.
	saWaitIdleLease(t, "smoke", saAdmKey(t, "A", "smoke"))
	c.Advance(ttl + 2*time.Millisecond)
	engine.releaseKey("C")
	select {
	case code := <-cDone:
		if code != 200 {
			t.Fatalf("C = %d", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("C not admitted after the full idle TTL from the final completion")
	}
	if got := engine.Seen(); len(got) != 3 || got[2] != "C" {
		t.Fatalf("seen = %v, want C last", got)
	}
	s.awaitIdle(t, 3*time.Second)
}

func TestSessionAdmission_TerminalBounds(t *testing.T) {
	const ttl = 5 * time.Second

	t.Run("queue-full", func(t *testing.T) {
		s, engine, c := saHTTPSetup(t, ttl, 1, 1)
		url := s.ts.URL + "/v1/chat/completions"
		aDone := saPostAsync(t, url, saChatBody("smoke", "A", "x"))
		time.Sleep(150 * time.Millisecond)
		bDone := make(chan int, 1)
		go func() { code, _, _ := saPost(t, url, saChatBody("smoke", "B", "x")); bDone <- code }()
		time.Sleep(150 * time.Millisecond)
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
		engine.releaseKey("A")
		if c0 := <-aDone; c0 != 200 {
			t.Fatalf("A = %d", c0)
		}
		// A's handler only arms the idle expiry timer when its deferred
		// ticket release lands — strictly after the client read — so wait
		// for the idle arm before advancing (B is still queued, so a full
		// drain is impossible and not needed).
		saWaitIdleLease(t, "smoke", saAdmKey(t, "A", "smoke"))
		// A's expiry timer frees the lease (clock-driven, no traffic); B
		// (still queued) proceeds and opens its OWN upstream gate — release
		// it before the test ends.
		c.Advance(ttl + 2*time.Millisecond)
		time.Sleep(100 * time.Millisecond)
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
		go func() { code, _, _ := saPost(t, url, saChatBody("smoke", "A", "x")); aDone <- code }()
		time.Sleep(150 * time.Millisecond)
		bDone := make(chan int, 1)
		go func() { code, _, _ := saPost(t, url, saChatBody("smoke", "B", "x")); bDone <- code }()
		time.Sleep(150 * time.Millisecond)
		// B's admission deadline (queueTimeout on the manual clock).
		c.Advance(queueTimeout + time.Second)
		select {
		case code := <-bDone:
			if code != http.StatusTooManyRequests {
				t.Fatalf("B = %d, want 429", code)
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
		go func() { code, _, _ := saPost(t, url, saChatBody("smoke", "A", "x")); aDone <- code }()
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
	s := newSAServing()
	t.Cleanup(func() { s.ts.Close() })
	t.Cleanup(func() { s.awaitIdle(t, 5*time.Second) })

	url := s.ts.URL + "/v1/chat/completions"

	// b1 gets "S" via the client key; b2 gets "S" via query-model routing
	// (no model field) — a DIFFERENT backend scope, so a different lease.
	done1 := make(chan int, 1)
	go func() { code, _, _ := saPost(t, url, saChatBody("b1", "S", "x")); done1 <- code }()
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
	c.Advance(ttl + 2*time.Millisecond)
	if held, _ := sessionAdmissions["b1"].snapshot(saAdmKey(t, "S", "b1")); held {
		t.Fatal("b1 lease outlived its full idle TTL")
	}
	if held, _ := sessionAdmissions["b2"].snapshot(saAdmKey(t, "S", "b2")); !held {
		t.Fatal("b2's active lease died under b1's expiry")
	}

	// Now b2 completes: ITS lease only becomes idle now, on its own clock.
	engine.releaseKey("S") // b2's gate
	if code2 := <-done2; code2 != 200 {
		t.Fatalf("b2 = %d", code2)
	}

	// Per-backend caps: b2's fresh key enqueues behind b2's own fresh key
	// lease; b1 (empty after its expiry) grants immediately.
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
	case <-out2:
		t.Fatal("b2 fresh key admitted before b2's idle lease expired")
	case <-time.After(100 * time.Millisecond):
	}
	c.Advance(ttl + 2*time.Millisecond) // b2's idle TTL: frees b2's own slot
	select {
	case tk2 := <-out2:
		releaseTk2 := tk2
		defer releaseTk2.release()
	case <-time.After(2 * time.Second):
		t.Fatal("b2 fresh key not granted after b2's own lease expired")
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
	// the live/aggregate map; the lease is independent authority. It
	// enqueues (cap 1, kA idle-holding) and must be driven out the
	// SAME leaks/expiry path while the feed stores evict.
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
	c.Advance(ttl + 2*time.Millisecond)
	resB := <-outB
	if resB.err != nil {
		t.Fatalf("kB not granted after kA lease expiry: %v", resB.err)
	}
	tk := resB.tk
	if held, _ := a.snapshot("kA"); held {
		t.Fatal("kA lease outlived its TTL under eviction pressure")
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
