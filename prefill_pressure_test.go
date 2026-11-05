package main

import (
	"encoding/json"
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
	engineMetrics.backends[name] = &engineScrape{status: v.Status, view: v}
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

// TestBackendPrefillJSONFields keeps the new BACKENDS fields wired to their
// documented JSON names.
func TestBackendPrefillJSONFields(t *testing.T) {
	var b Backend
	if err := json.Unmarshal([]byte(`{"name":"x","url":"http://x","maxConcurrentLargePrefill":1,"largePrefillKVThresholdPct":80,"largePrefillMaxQueue":16}`), &b); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if b.LargePrefillKVThresholdPct != 80 || b.LargePrefillMaxQueue != 16 {
		t.Fatalf("fields = %v/%v, want 80/16", b.LargePrefillKVThresholdPct, b.LargePrefillMaxQueue)
	}
}
