package main

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestGateGraceAndExpiry(t *testing.T) {
	if got := gateGrace(time.Time{}, time.Second); got != time.Second {
		t.Fatalf("zero deadline grace = %v, want 1s", got)
	}
	if got := gateGrace(time.Now().Add(50*time.Millisecond), time.Second); got <= 0 || got > 60*time.Millisecond {
		t.Fatalf("near deadline grace = %v, want ~50ms", got)
	}
	if got := gateGrace(time.Now().Add(time.Minute), 5*time.Second); got != 5*time.Second {
		t.Fatalf("far deadline grace = %v, want 5s", got)
	}
	if got := gateGrace(time.Now().Add(-time.Second), time.Second); got >= 0 {
		t.Fatalf("past deadline grace = %v, want negative", got)
	}
	if preForwardExpired(time.Time{}) {
		t.Fatal("zero deadline reported expired")
	}
	if !preForwardExpired(time.Now().Add(-time.Millisecond)) {
		t.Fatal("past deadline reported live")
	}
}

// TestPreForwardDeadline_GrantAfterExpiryNotForwarded proves the post-grant
// check: admission is driven by a frozen manual clock while the absolute wall
// budget expires, so A's release grants B after the deadline even though the
// downstream execution slot is free. B must be rejected, never forwarded.
func TestPreForwardDeadline_GrantAfterExpiryNotForwarded(t *testing.T) {
	prevQueueTimeout := queueTimeout
	queueTimeout = 80 * time.Millisecond
	t.Cleanup(func() { queueTimeout = prevQueueTimeout })

	engine := newSAGateEngine()
	withRouterEnv(t,
		[]Backend{{Name: "smoke", URL: engine.TS.URL, MaxConcurrent: 8, MaxQueueDepth: 4}},
		map[string]int{"smoke": 8}, nil)
	proxies["smoke"] = newTestProxy(engine.TS.URL)
	saIsolate(t)
	c := newSAManualClock()
	withAdmissionEnv(t, time.Minute, c, 1, 4) // reclaim on: A's release grants B
	s := newSAServing()
	t.Cleanup(func() { s.ts.Close() })
	t.Cleanup(func() { engine.TS.Close() })
	t.Cleanup(func() { engine.releaseAll() })
	t.Cleanup(func() { s.awaitIdle(t, 5*time.Second) })

	url := s.ts.URL + "/v1/chat/completions"
	aDone := saPostAsync(t, url, saChatBody("smoke", "A", "hi"))
	waitUntil(t, "A forwarded", func() bool { return seenCount(engine, "A") == 1 })

	type postOut struct {
		code int
		hdr  http.Header
	}
	bCh := make(chan postOut, 1)
	go func() {
		out, _ := saPostOutDo(url, saChatBody("smoke", "B", "hi"))
		bCh <- postOut{code: out.code, hdr: out.hdr}
	}()
	saWaitWaiting(t, sessionAdmissions["smoke"], 1)

	// The admission clock is frozen, so B stays queued; the wall budget
	// expires while A still holds the lease.
	time.Sleep(150 * time.Millisecond)
	engine.releaseKey("A")
	if code := <-aDone; code != 200 {
		t.Fatalf("A status = %d", code)
	}

	select {
	case out := <-bCh:
		if out.code != http.StatusTooManyRequests || out.hdr.Get("X-Router-Reason") != "session-admission-timeout" {
			t.Fatalf("B = %d %q, want 429 session-admission-timeout",
				out.code, out.hdr.Get("X-Router-Reason"))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("B result not published")
	}
	if seenCount(engine, "B") != 0 {
		t.Fatal("request granted after the deadline was forwarded")
	}
}

// TestPreForwardDeadline_NinthIdentityTimesOut proves the bounded wait under
// sustained saturation: a fresh identity queued behind a held cap times out at
// roughly the configured budget, receives one capacity 429, and mints no lease.
func TestPreForwardDeadline_NinthIdentityTimesOut(t *testing.T) {
	prevQueueTimeout := queueTimeout
	queueTimeout = 200 * time.Millisecond
	t.Cleanup(func() { queueTimeout = prevQueueTimeout })

	engine := newSAGateEngine()
	withRouterEnv(t,
		[]Backend{{Name: "smoke", URL: engine.TS.URL, MaxConcurrent: 8, MaxQueueDepth: 4}},
		map[string]int{"smoke": 8}, nil)
	proxies["smoke"] = newTestProxy(engine.TS.URL)
	saIsolate(t)
	withAdmissionEnv(t, time.Minute, nil, 1, 4) // production clock: the waiter timer must fire
	s := newSAServing()
	t.Cleanup(func() { s.ts.Close() })
	t.Cleanup(func() { engine.TS.Close() })
	t.Cleanup(func() { engine.releaseAll() })
	t.Cleanup(func() { s.awaitIdle(t, 5*time.Second) })

	url := s.ts.URL + "/v1/chat/completions"
	aDone := saPostAsync(t, url, saChatBody("smoke", "A", "hi"))
	waitUntil(t, "A forwarded", func() bool { return seenCount(engine, "A") == 1 })

	start := time.Now()
	code, hdr, _ := saPost(t, url, saChatBody("smoke", "B", "hi"))
	elapsed := time.Since(start)
	if code != http.StatusTooManyRequests || hdr.Get("X-Router-Reason") != "session-admission-timeout" {
		t.Fatalf("B = %d %q, want 429 session-admission-timeout", code, hdr.Get("X-Router-Reason"))
	}
	if elapsed < 150*time.Millisecond || elapsed > 500*time.Millisecond {
		t.Fatalf("B waited %v, want ~200ms", elapsed)
	}
	if held, _ := sessionAdmissions["smoke"].snapshot(saAdmKey(t, "B", "smoke")); held {
		t.Fatal("timed-out identity minted a lease")
	}
	if seenCount(engine, "B") != 0 {
		t.Fatal("timed-out identity reached the engine")
	}
	engine.releaseKey("A")
	if code := <-aDone; code != 200 {
		t.Fatalf("A status = %d", code)
	}
}

// TestPreForwardDeadline_StreamOutlivesBudget proves the absolute deadline is
// a pre-forward gate only: a request that starts forwarding within budget can
// stream far past it without router termination.
func TestPreForwardDeadline_StreamOutlivesBudget(t *testing.T) {
	prevQueueTimeout := queueTimeout
	queueTimeout = 100 * time.Millisecond
	t.Cleanup(func() { queueTimeout = prevQueueTimeout })

	s, engine, _ := saHTTPSetup(t, time.Minute, 8, 8)
	longMsg := strings.Repeat("x", 200)
	start := time.Now()
	req := saPostAsync(t, s.ts.URL+"/v1/chat/completions", saChatBody("smoke", "S", longMsg))
	waitUntil(t, "request forwarded", func() bool { return seenCount(engine, "S") == 1 })

	// Hold the upstream response well beyond the admission budget.
	time.Sleep(300 * time.Millisecond)
	engine.releaseKey("S")
	if code := <-req; code != 200 {
		t.Fatalf("request status = %d, want 200 after streaming past the budget", code)
	}
	if elapsed := time.Since(start); elapsed < 300*time.Millisecond {
		t.Fatalf("request finished in %v, want >300ms", elapsed)
	}
}
