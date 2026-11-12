package main

import (
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ─── Harness ──────────────────────────────────────────────────────────────

type prefillHarness struct {
	s      *saServing
	engine *saGateEngine
	c      *saManualClock
}

func (h *prefillHarness) url() string { return h.s.ts.URL + "/v1/chat/completions" }

// newPrefillHarness wires a real router listener with a gated engine, one
// large-prefill permit, an 8-token large threshold, a 50% KV pressure
// threshold, and the requested pressure queue bound. kv is the initial
// engine KV percentage published in the fake scrape.
func newPrefillHarness(t *testing.T, kv float64, admission bool, maxQueue int) *prefillHarness {
	t.Helper()
	if maxQueue <= 0 {
		maxQueue = 4
	}
	engine := newSAGateEngine()
	b := Backend{
		Name:                        "smoke",
		URL:                         engine.TS.URL,
		MaxConcurrent:               2,
		MaxQueueDepth:               4,
		MaxConcurrentLargePrefill:   1,
		LargePrefillThresholdTokens: 8,
		LargePrefillKVThresholdPct:  50,
		LargePrefillMaxQueue:        maxQueue,
	}
	withRouterEnv(t, []Backend{b}, map[string]int{"smoke": 2}, nil)
	proxies["smoke"] = newTestProxy(engine.TS.URL)
	saIsolate(t)
	prefillSlots["smoke"] = newSlot(1, int32(maxQueue))
	prefillPressure["smoke"] = newPrefillPressureGate(int32(maxQueue))
	setEngineView(t, "smoke", engineView{Status: "ok", KVPct: kv})
	c := newSAManualClock()
	if admission {
		// Hold idle leases so a returning conversation stays warm for the
		// whole test window.
		withAdmissionEnvPolicy(t, time.Minute, c, 8, 8, false)
	}
	s := newSAServing()
	t.Cleanup(func() { s.ts.Close() })
	t.Cleanup(func() { engine.TS.Close() })
	t.Cleanup(func() { engine.releaseAll() })
	t.Cleanup(func() { s.awaitIdle(t, 5*time.Second) })
	return &prefillHarness{s: s, engine: engine, c: c}
}

func setEngineView(t *testing.T, name string, v engineView) {
	t.Helper()
	engineMetrics.mu.Lock()
	engineMetrics.backends[name] = &engineScrape{status: v.Status, view: v, lastT: time.Now()}
	engineMetrics.mu.Unlock()
}

func seenCount(e *saGateEngine, key string) int {
	n := 0
	for _, k := range e.Seen() {
		if k == key {
			n++
		}
	}
	return n
}

func waitUntil(t *testing.T, what string, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !f() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func prefillInFlight(name string) int32 {
	in, _, _, _ := prefillSlots[name].snapshot()
	return in
}

func TestPrefillModes_ParkingBoundAndCleanup(t *testing.T) {
	for _, end := range []string{"cancel", "deadline"} {
		t.Run(end, func(t *testing.T) {
			h := newPrefillHarness(t, 0, true, 1)
			configureDecodeFirst()
			oldTimeout := queueTimeout
			queueTimeout = 500 * time.Millisecond
			t.Cleanup(func() { queueTimeout = oldTimeout })
			setEngineView(t, "smoke", engineView{Status: "ok", RunningPresent: true, Running: 7})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan struct{})
			go func() {
				defer close(done)
				req, _ := http.NewRequestWithContext(ctx, http.MethodPost, h.url(), strings.NewReader(saChatBody("smoke", "P", strings.Repeat("x", 200))))
				req.Header.Set("Content-Type", "application/json")
				resp, err := http.DefaultClient.Do(req)
				if err == nil {
					io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
				}
			}()
			waitUntil(t, "headroom-only bounded park", func() bool { return prefillPressure["smoke"].snapshot() == 1 })
			overflow := saPostAsync(t, h.url(), saChatBody("smoke", "Q", strings.Repeat("x", 200)))
			if code := <-overflow; code != 429 {
				t.Fatalf("overflow status %d", code)
			}
			if end == "cancel" {
				cancel()
			}
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("parking did not terminate")
			}
			h.s.awaitIdle(t, 5*time.Second)
			if prefillPressure["smoke"].snapshot() != 0 || prefillInFlight("smoke") != 0 {
				t.Fatal("parking reservation leaked")
			}
			in, _, waiting, _ := slots["smoke"].snapshot()
			if in != 0 || waiting != 0 || seenCount(h.engine, "P") != 0 {
				t.Fatal("parked request took execution capacity")
			}
			saWaitIdleLease(t, "smoke", saAdmKey(t, "P", "smoke"))
		})
	}
}

func TestPrefillModes_Validation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		b       Backend
		invalid bool
	}{
		{"absent", Backend{}, false},
		{"balanced-retains-thresholds", Backend{LargePrefillMode: "balanced", LargePrefillMaxRunning: 4, LargePrefillMaxITLMs: 250}, false},
		{"empty-retains-thresholds", Backend{LargePrefillMaxRunning: 4, LargePrefillMaxITLMs: 250}, false},
		{"unknown", Backend{LargePrefillMode: "fast"}, true},
		{"negative-running", Backend{LargePrefillMaxRunning: -1}, true},
		{"negative-itl", Backend{LargePrefillMaxITLMs: -1}, true},
		{"nan-itl", Backend{LargePrefillMaxITLMs: math.NaN()}, true},
		{"infinite-itl", Backend{LargePrefillMaxITLMs: math.Inf(1)}, true},
		{"no-permit", Backend{LargePrefillMode: "decode-first", LargePrefillMaxRunning: 4}, true},
		{"no-threshold", Backend{LargePrefillMode: "decode-first", MaxConcurrentLargePrefill: 1}, true},
		{"running-only", Backend{LargePrefillMode: "decode-first", MaxConcurrentLargePrefill: 1, LargePrefillMaxRunning: 4}, false},
		{"itl-only", Backend{LargePrefillMode: "decode-first", MaxConcurrentLargePrefill: 1, LargePrefillMaxITLMs: 250}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := validatePrefillModes([]Backend{tc.b}); (err != nil) != tc.invalid {
				t.Fatalf("startup policy validation = %v, invalid=%t", err, tc.invalid)
			}
		})
	}
}

func configureDecodeFirst() {
	backends[0].LargePrefillMode = "decode-first"
	backends[0].LargePrefillKVThresholdPct = 0
	backends[0].LargePrefillMaxRunning = 4
	backends[0].LargePrefillMaxITLMs = 250
}

func TestPrefillModes_HeadroomConsumers(t *testing.T) {
	cases := []struct {
		name  string
		view  engineView
		stale bool
		park  bool
	}{
		{"seven-running", engineView{Status: "ok", RunningPresent: true, Running: 7}, false, true},
		{"three-running", engineView{Status: "ok", RunningPresent: true, Running: 3}, false, false},
		{"waiting-only", engineView{Status: "ok", WaitingPresent: true, Waiting: 7}, false, true},
		{"sum-components", engineView{Status: "ok", RunningPresent: true, WaitingPresent: true, Running: 2, Waiting: 2}, false, true},
		{"missing-gauges", engineView{Status: "ok", Running: 7, Waiting: 7}, false, false},
		{"absent-component", engineView{Status: "ok", RunningPresent: true, Running: 3, Waiting: 7}, false, false},
		{"busy-high-itl", engineView{Status: "ok", RunningPresent: true, Running: 1, ITLSamples: 1, ITLms: 300}, false, true},
		{"idle-high-itl", engineView{Status: "ok", RunningPresent: true, ITLSamples: 1, ITLms: 300}, false, false},
		{"unsampled-itl", engineView{Status: "ok", RunningPresent: true, Running: 1, ITLms: 300}, false, false},
		{"negative-itl", engineView{Status: "ok", RunningPresent: true, Running: 1, ITLSamples: 1, ITLms: -300}, false, false},
		{"nan-itl", engineView{Status: "ok", RunningPresent: true, Running: 1, ITLSamples: 1, ITLms: math.NaN()}, false, false},
		{"infinite-itl", engineView{Status: "ok", RunningPresent: true, Running: 1, ITLSamples: 1, ITLms: math.Inf(1)}, false, false},
		{"stale", engineView{Status: "ok", RunningPresent: true, Running: 7, ITLSamples: 1, ITLms: 300}, true, false},
		{"failed", engineView{Status: "err", RunningPresent: true, Running: 7}, false, false},
		{"missing", engineView{}, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newPrefillHarness(t, 0, false, 4)
			configureDecodeFirst()
			setEngineView(t, "smoke", tc.view)
			if tc.stale {
				engineMetrics.mu.Lock()
				engineMetrics.backends["smoke"].lastT = time.Now().Add(-6 * time.Second)
				engineMetrics.mu.Unlock()
			}
			req := saPostAsync(t, h.url(), saChatBody("smoke", "L", strings.Repeat("x", 200)))
			if tc.park {
				waitUntil(t, "headroom parking", func() bool { return prefillPressure["smoke"].snapshot() == 1 })
				if prefillInFlight("smoke") != 0 || seenCount(h.engine, "L") != 0 {
					t.Fatal("parked request consumed capacity")
				}
				small := saPostAsync(t, h.url(), saChatBody("smoke", "S", "hi"))
				waitUntil(t, "small bypass", func() bool { return seenCount(h.engine, "S") == 1 })
				h.engine.releaseKey("S")
				if code := <-small; code != 200 {
					t.Fatalf("small status %d", code)
				}
				setEngineView(t, "smoke", engineView{Status: "ok", RunningPresent: true, Running: 3, ITLSamples: 1, ITLms: 100})
			}
			waitUntil(t, "large forwarded with headroom", func() bool { return seenCount(h.engine, "L") == 1 })
			h.engine.releaseKey("L")
			if code := <-req; code != 200 {
				t.Fatalf("large status %d", code)
			}
		})
	}
}

func TestPrefillModes_WarmLeasePolicy(t *testing.T) {
	for _, mode := range []string{"", "balanced", "decode-first"} {
		t.Run("mode-"+mode, func(t *testing.T) {
			h := newPrefillHarness(t, 90, true, 4)
			backends[0].LargePrefillMode = mode
			backends[0].LargePrefillMaxRunning = 4
			backends[0].LargePrefillMaxITLMs = 250
			prime := saPostAsync(t, h.url(), saChatBody("smoke", "W", "hi"))
			waitUntil(t, "prime lease", func() bool { return seenCount(h.engine, "W") == 1 })
			h.engine.releaseKey("W")
			if code := <-prime; code != 200 {
				t.Fatal(code)
			}
			saWaitIdleLease(t, "smoke", saAdmKey(t, "W", "smoke"))
			setEngineView(t, "smoke", engineView{Status: "ok", KVPct: 90, RunningPresent: true, Running: 7})
			req := saPostAsync(t, h.url(), saChatBody("smoke", "W", strings.Repeat("x", 200)))
			if mode == "decode-first" {
				waitUntil(t, "warm headroom parking", func() bool { return prefillPressure["smoke"].snapshot() == 1 })
				if seenCount(h.engine, "W") != 1 {
					t.Fatal("warm lease bypassed headroom")
				}
				// KV remains high: only the existing warm KV exemption survives.
				setEngineView(t, "smoke", engineView{Status: "ok", KVPct: 90, RunningPresent: true, Running: 3})
			}
			waitUntil(t, "warm large forwarded", func() bool { return seenCount(h.engine, "W") == 2 })
			h.engine.releaseKey("W")
			if code := <-req; code != 200 {
				t.Fatal(code)
			}
		})
	}
}

func TestPrefillModes_LocalFallback(t *testing.T) {
	for _, unlimited := range []bool{false, true} {
		for _, status := range []string{"err", "missing", "stale"} {
			t.Run(fmt.Sprintf("unlimited=%t/%s", unlimited, status), func(t *testing.T) {
				h := newPrefillHarness(t, 0, false, 4)
				configureDecodeFirst()
				backends[0].LargePrefillMaxRunning = 1
				if unlimited {
					delete(slots, "smoke")
				}
				setEngineView(t, "smoke", engineView{Status: status})
				if status == "stale" {
					setEngineView(t, "smoke", engineView{Status: "ok", RunningPresent: true})
					engineMetrics.mu.Lock()
					engineMetrics.backends["smoke"].lastT = time.Now().Add(-6 * time.Second)
					engineMetrics.mu.Unlock()
				}
				small := saPostAsync(t, h.url(), saChatBody("smoke", "S", "hi"))
				waitUntil(t, "local execution", func() bool { return seenCount(h.engine, "S") == 1 })
				large := saPostAsync(t, h.url(), saChatBody("smoke", "L", strings.Repeat("x", 200)))
				waitUntil(t, "local fallback parking", func() bool { return prefillPressure["smoke"].snapshot() == 1 })
				if seenCount(h.engine, "L") != 0 {
					t.Fatal("local occupancy ignored")
				}
				h.engine.releaseKey("S")
				if code := <-small; code != 200 {
					t.Fatal(code)
				}
				waitUntil(t, "local fallback recovery", func() bool { return seenCount(h.engine, "L") == 1 })
				h.engine.releaseKey("L")
				if code := <-large; code != 200 {
					t.Fatal(code)
				}
			})
		}
	}
}

func TestPrefillModes_RechecksReleaseReservations(t *testing.T) {
	for _, stage := range []string{"permit", "execution"} {
		t.Run(stage, func(t *testing.T) {
			h := newPrefillHarness(t, 0, false, 4)
			configureDecodeFirst()
			setEngineView(t, "smoke", engineView{Status: "ok", RunningPresent: true, Running: 3})
			sem, n := prefillSlots["smoke"], 1
			if stage == "execution" {
				sem, n = slots["smoke"], 2
			}
			for range n {
				if !sem.acquire(time.Second, nil) {
					t.Fatal("reserve")
				}
			}
			held := n
			defer func() {
				for held > 0 {
					sem.release()
					held--
				}
			}()
			req := saPostAsync(t, h.url(), saChatBody("smoke", "R", strings.Repeat("x", 200)))
			waitUntil(t, "queued at "+stage, func() bool { _, _, w, _ := sem.snapshot(); return w == 1 })
			setEngineView(t, "smoke", engineView{Status: "ok", RunningPresent: true, Running: 7})
			sem.release()
			held--
			waitUntil(t, "repark after "+stage, func() bool { return prefillPressure["smoke"].snapshot() == 1 })
			if prefillInFlight("smoke") != 0 || seenCount(h.engine, "R") != 0 {
				t.Fatal("repark retained permit or forwarded")
			}
			in, _, _, _ := slots["smoke"].snapshot()
			if stage == "execution" && in != int32(held) {
				t.Fatalf("execution leak: %d", in)
			}
			setEngineView(t, "smoke", engineView{Status: "ok", RunningPresent: true, Running: 3})
			waitUntil(t, "forward after recovery", func() bool { return seenCount(h.engine, "R") == 1 })
			h.engine.releaseKey("R")
			if code := <-req; code != 200 {
				t.Fatal(code)
			}
		})
	}
}

// ─── Tests ────────────────────────────────────────────────────────────────

// TestPrefillPressure_ColdParksWarmBypasses is the core policy: a cold large
// prefill parks above the KV threshold without taking the permit, while a
// reused-lease (warm) large prefill and small requests pass immediately.
func TestPrefillPressure_ColdParksWarmBypasses(t *testing.T) {
	h := newPrefillHarness(t, 90, true, 4)
	longMsg := strings.Repeat("x", 200)

	// Small requests bypass the gate even under pressure, and this one
	// primes a warm lease for key W.
	prime := saPostAsync(t, h.url(), saChatBody("smoke", "W", "hi"))
	waitUntil(t, "small request forwarded", func() bool { return seenCount(h.engine, "W") == 1 })
	if got := prefillPressure["smoke"].snapshot(); got != 0 {
		t.Fatalf("small request parked under pressure: waiting=%d", got)
	}
	h.engine.releaseKey("W")
	if code := <-prime; code != 200 {
		t.Fatalf("small request status = %d", code)
	}
	saWaitIdleLease(t, "smoke", saAdmKey(t, "W", "smoke"))

	// Cold large request: parks, and must not hold the only permit.
	cold := saPostAsync(t, h.url(), saChatBody("smoke", "C", longMsg))
	waitUntil(t, "cold request parked", func() bool {
		return prefillPressure["smoke"].snapshot() == 1
	})
	if in := prefillInFlight("smoke"); in != 0 {
		t.Fatalf("parked cold request holds the prefill permit: inflight=%d", in)
	}

	// Warm large request: reused lease bypasses the pressure gate and reaches
	// the engine while the cold one is still parked.
	warm := saPostAsync(t, h.url(), saChatBody("smoke", "W", longMsg))
	waitUntil(t, "warm request forwarded", func() bool { return seenCount(h.engine, "W") == 2 })
	if seenCount(h.engine, "C") != 0 {
		t.Fatal("cold request bypassed the pressure gate")
	}
	if got := prefillPressure["smoke"].snapshot(); got != 1 {
		t.Fatalf("pressure waiting = %d, want the cold request still parked", got)
	}

	// Pressure clears; the warm request finishes and the cold one follows in
	// the order the permit actually frees.
	setEngineView(t, "smoke", engineView{Status: "ok", KVPct: 10})
	h.engine.releaseKey("W")
	if code := <-warm; code != 200 {
		t.Fatalf("warm request status = %d", code)
	}
	waitUntil(t, "cold request forwarded after pressure drop", func() bool {
		return seenCount(h.engine, "C") == 1
	})
	h.engine.releaseKey("C")
	if code := <-cold; code != 200 {
		t.Fatalf("cold request status = %d", code)
	}
	waitUntil(t, "pressure gate drained", func() bool {
		return prefillPressure["smoke"].snapshot() == 0
	})
}

// TestPrefillPressure_RiseAfterAcquireReparks exercises the release/requeue
// retry: a request acquires the permit in a lull, pressure rises while it
// waits behind another holder, and it must hand the permit back and park
// rather than forward into a full cache.
func TestPrefillPressure_RiseAfterAcquireReparks(t *testing.T) {
	h := newPrefillHarness(t, 10, false, 4)
	longMsg := strings.Repeat("x", 200)

	// Hold the only permit so the request queues for it in a lull.
	if !prefillSlots["smoke"].acquire(time.Second, nil) {
		t.Fatal("could not hold the prefill permit")
	}
	held := true
	defer func() {
		if held {
			prefillSlots["smoke"].release()
		}
	}()
	req := saPostAsync(t, h.url(), saChatBody("smoke", "R", longMsg))
	waitUntil(t, "request queued for the permit", func() bool {
		_, _, waiting, _ := prefillSlots["smoke"].snapshot()
		return waiting == 1
	})

	// Pressure rises exactly as the permit frees.
	setEngineView(t, "smoke", engineView{Status: "ok", KVPct: 90})
	prefillSlots["smoke"].release()
	held = false
	waitUntil(t, "request re-parks after acquisition", func() bool {
		return prefillPressure["smoke"].snapshot() == 1
	})
	if seenCount(h.engine, "R") != 0 {
		t.Fatal("request forwarded after re-check despite high pressure")
	}
	if in := prefillInFlight("smoke"); in != 0 {
		t.Fatalf("re-parked request holds the permit: inflight=%d", in)
	}

	// Pressure clears; the request reacquires and forwards.
	setEngineView(t, "smoke", engineView{Status: "ok", KVPct: 10})
	waitUntil(t, "request forwarded after pressure drop", func() bool {
		return seenCount(h.engine, "R") == 1
	})
	h.engine.releaseKey("R")
	if code := <-req; code != 200 {
		t.Fatalf("request status = %d", code)
	}
	waitUntil(t, "permit and pressure counter balanced", func() bool {
		return prefillInFlight("smoke") == 0 && prefillPressure["smoke"].snapshot() == 0
	})
}

// TestPrefillPressure_NonStreamTimerStartsAtForward proves a non-streaming
// request keeps its prefill permit through a long execution-slot wait: the
// estimated-hold timer must not start before forwarding.
func TestPrefillPressure_NonStreamTimerStartsAtForward(t *testing.T) {
	h := newPrefillHarness(t, 0, false, 4)
	savedRate := prefillTokensPerSec
	prefillTokensPerSec = 1_000_000_000 // estimated hold ≈ 0s
	t.Cleanup(func() { prefillTokensPerSec = savedRate })
	longMsg := strings.Repeat("x", 2000)

	// Hold both execution permits so the request parks after acquiring the
	// prefill permit.
	for range 2 {
		if !slots["smoke"].acquire(time.Second, nil) {
			t.Fatal("could not hold an execution permit")
		}
	}
	held := 2
	defer func() {
		for held > 0 {
			slots["smoke"].release()
			held--
		}
	}()

	req := saPostAsync(t, h.url(), saChatBody("smoke", "N", longMsg))
	waitUntil(t, "prefill permit acquired", func() bool { return prefillInFlight("smoke") == 1 })
	waitUntil(t, "request queued at execution", func() bool {
		_, _, waiting, _ := slots["smoke"].snapshot()
		return waiting >= 1
	})
	time.Sleep(150 * time.Millisecond) // >> the zero-second estimated hold
	if in := prefillInFlight("smoke"); in != 1 {
		t.Fatalf("prefill permit released before forwarding: inflight=%d", in)
	}
	if seenCount(h.engine, "N") != 0 {
		t.Fatal("request forwarded while the execution gate was full")
	}

	// Free one execution slot: the request forwards and only then can the
	// permit be released (timer or first byte).
	slots["smoke"].release()
	held--
	waitUntil(t, "request forwarded", func() bool { return seenCount(h.engine, "N") == 1 })
	h.engine.releaseKey("N")
	if code := <-req; code != 200 {
		t.Fatalf("request status = %d", code)
	}
	waitUntil(t, "prefill permit released", func() bool { return prefillInFlight("smoke") == 0 })
}

// TestPrefillPressure_FailsOpenWithoutMetrics: a scrape that is not "ok"
// must never freeze a large prefill.
func TestPrefillPressure_FailsOpenWithoutMetrics(t *testing.T) {
	h := newPrefillHarness(t, 99, false, 4)
	engineMetrics.mu.Lock()
	engineMetrics.backends["smoke"].view = engineView{Status: "err", KVPct: 99}
	engineMetrics.mu.Unlock()
	longMsg := strings.Repeat("x", 200)

	req := saPostAsync(t, h.url(), saChatBody("smoke", "F", longMsg))
	waitUntil(t, "large request forwarded despite missing metrics", func() bool {
		return seenCount(h.engine, "F") == 1
	})
	h.engine.releaseKey("F")
	if code := <-req; code != 200 {
		t.Fatalf("request status = %d", code)
	}
}

// TestPrefillPressure_QueueFullBoundsColdParking: parked cold requests are
// bounded; overflow receives the prefill-queue-full 429 instead of growing
// the queue.
func TestPrefillPressure_QueueFullBoundsColdParking(t *testing.T) {
	h := newPrefillHarness(t, 90, false, 1)
	longMsg := strings.Repeat("x", 200)

	first := saPostAsync(t, h.url(), saChatBody("smoke", "A", longMsg))
	waitUntil(t, "first cold request parked", func() bool {
		return prefillPressure["smoke"].snapshot() == 1
	})
	code, hdr, _ := saPost(t, h.url(), saChatBody("smoke", "B", longMsg))
	if code != 429 || hdr.Get("X-Router-Reason") != "prefill-queue-full" {
		t.Fatalf("second cold request = %d %q, want 429 prefill-queue-full",
			code, hdr.Get("X-Router-Reason"))
	}

	setEngineView(t, "smoke", engineView{Status: "ok", KVPct: 10})
	waitUntil(t, "first cold request forwarded", func() bool {
		return seenCount(h.engine, "A") == 1
	})
	h.engine.releaseKey("A")
	if code := <-first; code != 200 {
		t.Fatalf("first cold request status = %d", code)
	}
}

// TestPrefillPermit_GeneratedOutput exercises real proxy body copies. Reading
// every prefix chunk is a barrier: it has crossed the tracker, but must not
// release A's permit. No sleep is used to infer that B stayed blocked.
func TestPrefillPermit_GeneratedOutput(t *testing.T) {
	for _, output := range []struct {
		name string
		data string
	}{
		{"content", `{"choices":[{"delta":{"content":"hello"}}]}`},
		{"reasoning", `{"choices":[{"delta":{"reasoning_content":"thinking"}}]}`},
		{"reasoning-alias", `{"choices":[{"delta":{"reasoning":"thinking"}}]}`},
		{"tool", `{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{}"}}]}}]}`},
		{"completion", `{"choices":[{"text":"hello"}]}`},
	} {
		t.Run(output.name, func(t *testing.T) {
			h := newPrefillHarness(t, 0, false, 4)
			chunks := make(chan string)
			stop := make(chan struct{})
			forwardedB := make(chan struct{})
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				if strings.Contains(string(body), `"B"`) {
					close(forwardedB)
					fmt.Fprint(w, "B done")
					return
				}
				if strings.Contains(string(body), `"small"`) {
					fmt.Fprint(w, "small done")
					return
				}
				w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
				w.WriteHeader(http.StatusOK)
				w.(http.Flusher).Flush()
				for {
					select {
					case p := <-chunks:
						fmt.Fprint(w, p)
						w.(http.Flusher).Flush()
					case <-stop:
						return
					case <-r.Context().Done():
						return
					}
				}
			}))
			t.Cleanup(up.Close)
			t.Cleanup(func() { close(stop) })
			proxies["smoke"] = newTestProxy(up.URL)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			body := fmt.Sprintf(`{"model":"smoke","stream":true,"prompt_cache_key":"A","messages":[{"role":"user","content":%q}]}`, strings.Repeat("x", 200))
			req, _ := http.NewRequestWithContext(ctx, http.MethodPost, h.url(), strings.NewReader(body))
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if got := prefillInFlight("smoke"); got != 1 {
				t.Fatalf("headers released prefill: %d", got)
			}
			b := saPostAsync(t, h.url(), saChatBody("smoke", "B", strings.Repeat("x", 200)))
			saPollPrefillSlot(t, 1, 1)
			if code, _, _ := saPost(t, h.url(), saChatBody("smoke", "small", "hi")); code != 200 {
				t.Fatalf("small request = %d", code)
			}
			forward := func(p string) {
				t.Helper()
				select {
				case chunks <- p:
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				got := make([]byte, len(p))
				if _, err := io.ReadFull(resp.Body, got); err != nil || string(got) != p {
					t.Fatalf("forwarded bytes = %q, err %v, want %q", got, err, p)
				}
			}
			for _, p := range []string{
				": heartbeat\r\n\r\n",
				"data: {\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"\"}}]}\r\n\r\n",
				"data: {\"choices\":[{\"delta\":{}}]}\n\n",
				"data: not-json\n\n",
				"data: {\"usage\":{\"prompt_tokens\":50}}\n\n",
				"data: " + strings.Repeat("x", maxPrefillEventBytes) + "\n\n",
				"data: " + output.data[:len(output.data)/2],
			} {
				forward(p)
				if got := prefillInFlight("smoke"); got != 1 {
					t.Fatalf("prefix released prefill: %d", got)
				}
				select {
				case <-forwardedB:
					t.Fatal("B forwarded before generated output")
				default:
				}
			}
			forward(output.data[len(output.data)/2:] + "\r\n\r\n")
			select {
			case <-forwardedB:
			case <-ctx.Done():
				t.Fatal("B did not forward during A decode")
			}
			if code := <-b; code != 200 {
				t.Fatalf("B status = %d", code)
			}
			// A remains open and can continue decoding after handing off.
			forward("data: " + output.data + "\n\n")
			cancel()
			resp.Body.Close()
			h.s.awaitIdle(t, 5*time.Second)
			saPollPrefillSlot(t, 0, 0)
		})
	}
}

func TestPrefillPermit_ResponseWriterBoundaries(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(fmt.Sprintf("explicit=%v", explicit), func(t *testing.T) {
			rec := httptest.NewRecorder()
			releases := 0
			tracker := &respTracker{
				ResponseWriter: rec,
				status:         http.StatusOK,
				prefillRelease: func() { releases++ },
			}
			if explicit {
				tracker.WriteHeader(http.StatusOK)
			}
			tracker.Flush()
			if _, err := tracker.Write(nil); err != nil {
				t.Fatal(err)
			}
			if releases != 0 {
				t.Fatal("headers, flush or empty write released prefill")
			}
			tracker.Write([]byte("body"))
			tracker.Write([]byte("tail"))
			if releases != 1 || rec.Body.String() != "bodytail" {
				t.Fatalf("releases=%d body=%q", releases, rec.Body.String())
			}
		})
	}
}

func TestPrefillPermit_SSEFraming(t *testing.T) {
	for _, framing := range []string{"\n", "\r\n", "\r"} {
		t.Run(fmt.Sprintf("newline=%q", framing), func(t *testing.T) {
			observer := &prefillOutputObserver{}
			prefix := ": heartbeat" + framing + framing +
				"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0}]}}]}" + framing + framing +
				"data: [DONE]" + framing + framing
			for i := range prefix {
				if observer.observe([]byte{prefix[i]}) {
					t.Fatal("preamble was mistaken for generated output")
				}
			}
			output := "event: message" + framing + "data: {\"choices\":[" + framing +
				"data: {\"delta\":{\"content\":\"token\"}}]}" + framing + framing
			for i := range output {
				got := observer.observe([]byte{output[i]})
				// CR dispatches immediately, before the optional LF.
				want := i == len(output)-1
				if framing == "\r\n" {
					want = i == len(output)-2
				}
				if got != want {
					t.Fatalf("byte %d: output=%v, want %v", i, got, want)
				}
			}
		})
	}
}

func TestPrefillPermit_HeaderOnlyCleanup(t *testing.T) {
	for _, ending := range []string{"eof", "abort", "cancel"} {
		t.Run(ending, func(t *testing.T) {
			h := newPrefillHarness(t, 0, false, 4)
			end := make(chan struct{}, 1)
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "text/event-stream")
				if ending == "abort" {
					w.Header().Set("Content-Length", "1000")
				}
				w.WriteHeader(http.StatusOK)
				w.(http.Flusher).Flush()
				select {
				case <-end:
				case <-r.Context().Done():
				}
			}))
			t.Cleanup(up.Close)
			t.Cleanup(func() { close(end) })
			proxies["smoke"] = newTestProxy(up.URL)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			body := fmt.Sprintf(`{"model":"smoke","stream":true,"messages":[{"role":"user","content":%q}]}`, strings.Repeat("x", 200))
			req, _ := http.NewRequestWithContext(ctx, http.MethodPost, h.url(), strings.NewReader(body))
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if got := prefillInFlight("smoke"); got != 1 {
				t.Fatalf("headers released permit: %d", got)
			}
			if ending == "cancel" {
				cancel()
			} else {
				end <- struct{}{}
			}
			_, err = io.ReadAll(resp.Body)
			if ending == "eof" && err != nil {
				t.Fatal(err)
			}
			if ending == "abort" && err == nil {
				t.Fatal("truncated upstream body did not abort")
			}
			resp.Body.Close()
			h.s.awaitIdle(t, 5*time.Second)
			saPollPrefillSlot(t, 0, 0)
		})
	}
}
