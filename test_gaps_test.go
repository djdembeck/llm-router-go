package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestProxy_CompletionTTFT_5xxBodyExcluded pins the handleProxy completion
// gate (computeTTFT): an upstream 5xx that DOES carry a body sets the
// tracker's firstByteAt, so a missing status gate would leak the body
// latency into the session's AvgTTFTMs as if it were a serving TTFT. The
// genuine 2xx stream's TTFT from the second request is the only sample the
// session exposes.
func TestProxy_CompletionTTFT_5xxBodyExcluded(t *testing.T) {
	// Identical conversation for both requests (sessionKeyFor hashes
	// everything except the last message; a single message hashes
	// itself) — both fold into ONE session aggregate.
	const chatBody = `{"model":"m1","stream":true,"messages":[{"role":"user","content":"hi"}]}`

	var call int32
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Context().Err() != nil {
			return
		}
		switch atomic.AddInt32(&call, 1) {
		case 1:
			// (a) First call: 500 WITH a JSON body on a stream:true
			// request. The body bytes bump the tracker's firstByteAt —
			// the completion gate must keep them out of the session's
			// TTFT aggregate.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, `{"error":{"message":"upstream exploded","type":"server_error"}}`)
		default:
			// (b) Second call: the 2xx streaming control. Deliberable
			// first-byte delay (~120ms) so the 2xx sample is clearly
			// distinct from the ~0ms 5xx body latency.
			w.Header().Set("Content-Type", "text/event-stream")
			time.Sleep(120 * time.Millisecond)
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, "data: chunk0\n\n")
			if fl, ok := w.(http.Flusher); ok {
				fl.Flush()
			}
			fmt.Fprint(w, "data: [DONE]\n\n")
		}
	}))
	defer fake.Close()

	withRouterEnv(t, []Backend{{Name: "m1", URL: fake.URL, Tier: 0}}, nil, nil)
	proxies["m1"] = newTestProxy(fake.URL)
	saveSessions := sessions
	saveMetrics := metrics
	sessions, metrics = newSessionRegistry(), newTestMetrics()
	t.Cleanup(func() { sessions, metrics = saveSessions, saveMetrics })

	router := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handleProxy(w, r)
	}))
	defer router.Close()

	// (a) 5xx WITH a body.
	respA, err := http.Post(router.URL+"/v1/chat/completions", "application/json", strings.NewReader(chatBody))
	if err != nil {
		t.Fatal(err)
	}
	bodyA, _ := io.ReadAll(respA.Body)
	respA.Body.Close()
	if respA.StatusCode != http.StatusInternalServerError {
		t.Fatalf("first request status = %d, want 500 (upstream 5xx with body)", respA.StatusCode)
	}
	if !strings.Contains(string(bodyA), "upstream exploded") {
		t.Fatalf("5xx body = %q, want the upstream error JSON (body must be present for the gate to matter)", bodyA)
	}

	// (b) 2xx streaming control.
	respB, err := http.Post(router.URL+"/v1/chat/completions", "application/json", strings.NewReader(chatBody))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, respB.Body)
	respB.Body.Close()
	if respB.StatusCode != http.StatusOK {
		t.Fatalf("second request status = %d, want 200 (streaming control)", respB.StatusCode)
	}

	feed := sessions.feed()
	if len(feed.Sessions) != 1 {
		t.Fatalf("sessions = %+v, want exactly 1 (both requests share the conversation key)", feed.Sessions)
	}
	s := feed.Sessions[0]
	if s.N != 2 || s.LastStatus != http.StatusOK {
		t.Fatalf("session = n %d status %d, want n 2 status 200 (both completions folded)", s.N, s.LastStatus)
	}
	// The 5xx body latency must contribute NOTHING: if it leaked (the
	// gate regressed), AvgTTFTMs = mix(≈0, ≈120) < the 2xx sample; only
	// the 2xx sample alone lands at ~120ms.
	if s.AvgTTFTMs <= 0 {
		t.Fatalf("avgTTFTMs = %v, want >0 (the 2xx stream's first-byte latency)", s.AvgTTFTMs)
	}
	if s.AvgTTFTMs < 100 {
		t.Errorf("avgTTFTMs = %v, want ≈120 (the 2xx sample only; a leaked 5xx body latency would half the mean)", s.AvgTTFTMs)
	}
	if s.AvgTTFTMs > 600 {
		t.Errorf("avgTTFTMs = %v, want ≈120 (not a different order of magnitude)", s.AvgTTFTMs)
	}
}

// TestEngine_ParsePromText_HistogramBucketsDontConsumeCap pins the
// exact-name filter for histogram families: an exposition with MORE
// than promSampleCap consumed _bucket lines for ONE histogram (plus its
// _sum/_count) must NOT truncate at the bucket lines — the parser
// retains only the _sum/_count parts of a histogram base name, so the
// buckets are skipped without ever counting against the cap.
func TestEngine_ParsePromText_HistogramBucketsDontConsumeCap(t *testing.T) {
	const buckets = promSampleCap * 3 // 3x the cap of _bucket lines

	t.Run("buckets_skip_cap_sum_count_survive", func(t *testing.T) {
		var body bytes.Buffer
		// labelSet pins the histogram's label set so every line is a
		// well-formed family member; the bucket lines must be filtered
		// on the name, not the labels.
		label := `{model_name="m"}`
		for i := range buckets {
			fmt.Fprintf(&body, "vllm:e2e_request_latency_seconds_bucket%s{le=\"%d\"} %d\n", label, i, i)
		}
		body.WriteString("vllm:e2e_request_latency_seconds_sum" + label + " 42.5\n")
		body.WriteString("vllm:e2e_request_latency_seconds_count" + label + " 3.0\n")

		samples, truncated := parsePromText(body.Bytes())
		if truncated {
			t.Fatalf("truncated = true, want false (%d _bucket lines must not consume the cap)", buckets)
		}
		got := 0
		for _, sm := range samples {
			switch sm.name {
			case "vllm:e2e_request_latency_seconds_sum":
				if sm.value != 42.5 {
					t.Errorf("_sum = %v, want 42.5", sm.value)
				}
				got++
			case "vllm:e2e_request_latency_seconds_count":
				if sm.value != 3 {
					t.Errorf("_count = %v, want 3", sm.value)
				}
				got++
			case "vllm:e2e_request_latency_seconds_bucket":
				t.Errorf("_bucket line %q retained — buckets must be filtered by exact name", sm.name)
			}
		}
		if len(samples) != 2 {
			t.Fatalf("samples = %d, want 2 (_sum + _count only: buckets retained=%d)", len(samples), got)
		}
	})
}

// TestEngine_Scrape_BaselinesResetAfterTruncation pins the outage
// semantics of a truncated scrape: baselines must be reset so the FIRST
// valid scrape after the truncation is baseline-only (no spurious rate
// spanning the gap) and the SECOND valid scrape publishes the true rate.
func TestEngine_Scrape_BaselinesResetAfterTruncation(t *testing.T) {
	r := newEngineRegistry()
	r.add("k", "http://127.0.0.1:1/metrics") // url unused; bodies are fed directly
	r.mu.Lock()
	s := r.backends["k"]
	r.mu.Unlock()

	// Prime: a valid scrape establishing the prompt_tokens_total baseline
	// plus counter+histogram state.
	body := `
vllm:num_requests_running{model_name="m"} 2
vllm:prompt_tokens_total{model_name="m"} 10000
vllm:e2e_request_latency_seconds_sum{model_name="m"} 9
vllm:e2e_request_latency_seconds_count{model_name="m"} 3
`
	s.scrapeBody([]byte(body))
	if s.view.Status != "ok" {
		t.Fatalf("prime scrape status = %q, want ok", s.view.Status)
	}
	if !s.counterSeen(promSample{name: "vllm:prompt_tokens_total", labels: map[string]string{"model_name": "m"}}) {
		t.Fatal("prime scrape did not baseline the prompt counter")
	}
	if h, ok := s.hists["vllm:e2e_request_latency_seconds"]; !ok || h.sum != 9 || h.cnt != 3 {
		t.Fatalf("prime histogram state = %+v (ok %v), want sum=9 cnt=3 baselined", h, ok)
	}
	if s.view.PrefillTokS != 0 {
		t.Fatalf("prime prefillTokS = %v, want 0 (first sight is baseline-only)", s.view.PrefillTokS)
	}

	// Truncated scrape: consume the whole cap with consumed-name samples,
	// so the _sum/_count tail the parser would need is cut off.
	var trunc bytes.Buffer
	for i := range promSampleCap {
		fmt.Fprintf(&trunc, "%s{i=\"%d\"} 1\n", consumedNameSeq[i%len(consumedNameSeq)], i)
	}
	trunc.WriteString("vllm:prompt_tokens_total{model_name=\"m\"} 9999\n")
	s.scrapeBody(trunc.Bytes())
	if s.view.Status != "err" {
		t.Fatalf("truncated scrape status = %q, want err", s.view.Status)
	}
	if s.view.PrefillTokS != 0 || s.view.E2EMs != 0 {
		t.Errorf("truncated scrape published interval fields prefill %v / e2e %v, want both 0",
			s.view.PrefillTokS, s.view.E2EMs)
	}
	if s.counterSeen(promSample{name: "vllm:prompt_tokens_total", labels: map[string]string{"model_name": "m"}}) {
		t.Error("counter baseline survived truncation, want reset (no delta spanning the gap)")
	}
	if h, ok := s.hists["vllm:e2e_request_latency_seconds"]; ok && (h.have || !h.t.IsZero() || h.sum != 0) {
		t.Errorf("histogram baseline survived truncation: %+v, want reset (have=false, t zero, sum 0)", h)
	}

	// Recovery scrape 1 (valid, counter ADVANCED from the prime): must be
	// baseline-only — the reset means there is no prior value, so no
	// spurious 8999/dt rate spanning the truncation gap.
	s.scrapeBody([]byte(`
vllm:num_requests_running{model_name="m"} 2
vllm:prompt_tokens_total{model_name="m"} 10500
vllm:e2e_request_latency_seconds_sum{model_name="m"} 33
vllm:e2e_request_latency_seconds_count{model_name="m"} 9
`))
	if s.view.Status != "ok" {
		t.Fatalf("recovery scrape status = %q, want ok", s.view.Status)
	}
	if s.view.PrefillTokS != 0 {
		t.Errorf("recovery scrape prefillTokS = %v, want 0 (baseline-only after reset; a non-zero rate spans the gap)", s.view.PrefillTokS)
	}
	if s.view.E2EMs != 0 || s.view.E2ESamples != 0 {
		t.Errorf("recovery scrape e2e = %vms / %d samples, want 0/0 (baseline-only histogram)", s.view.E2EMs, s.view.E2ESamples)
	}
	if !s.counterSeen(promSample{name: "vllm:prompt_tokens_total", labels: map[string]string{"model_name": "m"}}) {
		t.Fatal("recovery scrape did not re-baseline the prompt counter")
	}

	// Recovery scrape 2 (valid, +500 tokens over recovery 1): the true
	// interval rate must now publish — the baseline is this interval's,
	// not the pre-truncation one. A real (small) dt makes the rate
	// assertion clock-independent.
	time.Sleep(50 * time.Millisecond)
	s.scrapeBody([]byte(`
vllm:num_requests_running{model_name="m"} 2
vllm:prompt_tokens_total{model_name="m"} 11000
vllm:e2e_request_latency_seconds_sum{model_name="m"} 38
vllm:e2e_request_latency_seconds_count{model_name="m"} 10
`))
	// The baseline now holds recovery-1's value: the interval is
	// recovery2 vs recovery1, provable without a wall-clock assumption.
	if got := s.counters[counterKey(promSample{name: "vllm:prompt_tokens_total", labels: map[string]string{"model_name": "m"}})]; got != 11000 {
		t.Errorf("prompt counter baseline = %v, want 11000 (re-baselined from recovery scrape 1)", got)
	}
	if s.view.PrefillTokS == 0 {
		t.Errorf("second recovery prefillTokS = 0, want >0 (a real interval rate after re-baselining)")
	}
	if s.view.E2EMs <= 0 {
		t.Errorf("second recovery e2eMs = %v, want >0 ((38-33)/(10-9) s now converts to ms)", s.view.E2EMs)
	}
}

// TestGPURegistry_TruncatedBodyWithholdsDevices pins the GPU scrape's
// treatment of a capped exposition: a 200 body with more consumed GPU
// samples than promSampleCap is incomplete — partial frames must be
// withheld (no devices published, status err with a truncation error),
// and the next valid body recovers the normal feed.
func TestGPURegistry_TruncatedBodyWithholdsDevices(t *testing.T) {
	var body atomic.Value // string: the response the fake endpoint serves
	body.Store(truncatedGPUBody(t))

	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, body.Load().(string))
	}))
	defer fake.Close()

	g := newGPURegistry()
	g.configure(fake.URL)

	// Truncated scrape: partial DCGM lines swallowed by the cap.
	g.scrape()
	frame := g.feed()
	if frame.Status != "err" {
		t.Fatalf("status = %q, want err (truncated gpu body)", frame.Status)
	}
	if !strings.Contains(frame.LastError, "truncated") {
		t.Errorf("lastError = %q, want the truncation reason", frame.LastError)
	}
	if v := g.view(); len(v) != 0 {
		t.Fatalf("view = %d devices, want 0 (partial frames must not publish as current)", len(v))
	}

	// Recovery scrape: a valid, untruncated body. Clear the production
	// scrape-loop's 1s err backoff: calling scrape() directly would be
	// skipped until errNext passes, but the test wants immediate recovery.
	g.mu.Lock()
	g.errNext = time.Time{}
	g.mu.Unlock()
	body.Store(gpuBody(t, 2))
	g.scrape()
	frame = g.feed()
	if frame.Status != "ok" {
		t.Fatalf("recovery status = %q, want ok", frame.Status)
	}
	if frame.LastError != "" {
		t.Errorf("recovery lastError = %q, want empty", frame.LastError)
	}
	v := g.view()
	if len(v) != 2 {
		t.Fatalf("recovery view = %d devices, want 2", len(v))
	}
	if v[0].Key != "GPU-REC-0" || v[1].Key != "GPU-REC-1" {
		t.Errorf("recovery keys = %q/%q, want GPU-REC-0/GPU-REC-1", v[0].Key, v[1].Key)
	}
	if v[0].UtilPct != 40 || v[0].MemUsedMB != 4096 {
		t.Errorf("recovery device0 = %+v, want util 40, mem 4096MB", v[0])
	}
}

// truncatedGPUBody builds a 200 body with >promSampleCap consumed GPU
// sample lines: enough DCGM_FI_DEV_GPU_UTIL lines (fanned across GPUs so
// parseGPUDevices would publish partial frames if the cap passed them) to
// force the cap.
func truncatedGPUBody(t *testing.T) string {
	t.Helper()
	var b bytes.Buffer
	// Strictly more than the cap: at exactly the cap the parser retains
	// everything and reports truncated=false.
	for i := range promSampleCap + 4 {
		fmt.Fprintf(&b, "DCGM_FI_DEV_GPU_UTIL{UUID=\"GPU-TRUNC-%d\",gpu=\"%d\"} %d\n", i, i%4, i)
	}
	return b.String()
}

// gpuBody builds a valid DCGM exposition for n devices.
func gpuBody(t *testing.T, n int) string {
	t.Helper()
	var b bytes.Buffer
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "DCGM_FI_DEV_GPU_UTIL{UUID=\"GPU-REC-%d\",gpu=\"%d\"} %d\n", i, i, 40+i)
		fmt.Fprintf(&b, "DCGM_FI_DEV_FB_USED{UUID=\"GPU-REC-%d\",gpu=\"%d\"} 4096\n", i, i)
	}
	return b.String()
}

// TestProxy_Rejection_SessionWholeConversationEstimate pins the multi-turn
// split on a REJECTED request: the session record must carry the
// WHOLE-conversation prefill estimate (the same figure the admission gate
// sees), while the burst feed's newTokensEst carries the last-message-only
// estimate. A long conversation with a short final turn makes the two
// estimates unambiguously different.
func TestProxy_Rejection_SessionWholeConversationEstimate(t *testing.T) {
	const longChars = 4000 // 1000 estimated tokens per prior turn
	// 4 prior turns of 4000 chars + a short final turn: prefillTok ≈
	// (4·4000+4)/4 = 4001 (whole conversation), newTokensEst = 4/4 = 1
	// (last message only).
	msgs := make([]string, 0, 5)
	for range 4 {
		msgs = append(msgs, `{"role":"user","content":"`+strings.Repeat("a", longChars)+`"}`)
	}
	msgs = append(msgs, `{"role":"tool","content":"ok"}`)
	body := `{"model":"m1","stream":true,"messages":[` + strings.Join(msgs, ",") + `]}`

	arrived := make(chan struct{})
	var arrivedOnce sync.Once
	releaseA := make(chan struct{})
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Pin request A in-flight: holds the single slot so B gets the
		// 429. Released by the test via releaseA: the alternative (the
		// request context) never fires here — the single connection
		// blocks the server's read loop, so a client-received FIN sits
		// unread and r.Context() is never canceled.
		arrivedOnce.Do(func() { close(arrived) })
		select {
		case <-releaseA:
		case <-r.Context().Done():
		}
		w.Header().Set("Connection", "close")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"id":"x"}`)
	}))
	defer fake.Close()

	withRouterEnv(t, []Backend{{Name: "m1", URL: fake.URL, Tier: 0, MaxConcurrent: 1}},
		map[string]int{"m1": 1}, map[string]int{"m1": 1})
	proxies["m1"] = newTestProxy(fake.URL)
	saveSessions := sessions
	saveMetrics := metrics
	sessions, metrics = newSessionRegistry(), newTestMetrics()
	t.Cleanup(func() { sessions, metrics = saveSessions, saveMetrics })
	saveQT := queueTimeout
	queueTimeout = 300 * time.Millisecond
	t.Cleanup(func() { queueTimeout = saveQT })

	// Connection: close on every reply: A's late 5xx (written through
	// the tracker after the client canceled) lands on a dead client
	// socket, and a keep-alive conn serving that write would keep
	// httptest.Server.Close blocked.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Connection", "close")
		handleProxy(w, r)
	}))
	defer srv.Close()

	// A occupies the slot (goroutine: the handler blocks on the context).
	// It holds the single slot until released — once the test is done
	// with it (B's 429 in hand) it is canceled; the proxy retires its
	// session entry (499) when the handler unwinds, so the feed drains.
	// A dedicated client: its pooled conns are closed explicitly at
	// teardown so the httptest servers close without waiting on
	// keep-alive idle sockets.
	cli := &http.Client{}
	defer cli.CloseIdleConnections()
	ctxA, cancelA := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancelA()
	doneA := make(chan struct{})
	go func() {
		defer close(doneA)
		respA, err := http.NewRequestWithContext(ctxA, "POST", srv.URL+"/v1/chat/completions", strings.NewReader(body))
		if err != nil {
			return
		}
		out, err := cli.Do(respA)
		if err != nil || out == nil {
			return
		}
		io.Copy(io.Discard, out.Body)
		out.Body.Close()
	}()

	// A must have won the single slot and reached the upstream before B is
	// sent: if B were admitted first, it would wedge A behind the queue.
	select {
	case <-arrived:
	case <-time.After(2 * time.Second):
		t.Fatal("request A did not reach the upstream within 2s")
	}

	// B: same conversation, same body — waits past queueTimeout -> 429.
	// The dedicated client (not DefaultClient) so its pooled conn is
	// closed at teardown and the routing server closes cleanly.
	reqB, err := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	respB, err := cli.Do(reqB)
	if err != nil {
		t.Fatal(err)
	}
	respB.Body.Close()
	if respB.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 (slot exhausted)", respB.StatusCode)
	}
	close(releaseA) // done holding the slot; the fake replies, A completes.
	// (t.Fatal paths exit the process before teardown, so no deferred
	// release is needed.)
	cancelA() // retire A's client (response may already be in flight).

	// Join A's client: its conn is the one that could outlive the test
	// and block the httptest teardown.
	select {
	case <-doneA:
	case <-time.After(5 * time.Second):
		t.Fatal("request A's client did not finish after cancel")
	}

	// Wait for A to finish while B's rejection is already folded: the
	// session must drain to zero live requests before the assertions.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(sessions.feed().Live) == 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if l := sessions.feed().Live; len(l) != 0 {
		t.Fatalf("live = %+v, want empty (A's canceled request must retire)", l)
	}

	// 1. Session record: the whole-conversation estimate. The session
	// carries A (499, whole-conversation est) and B (429, same): both
	// frames use estimatePrefillTokens, never the last-message figure.
	feed := sessions.feed()
	if len(feed.Sessions) != 1 {
		t.Fatalf("sessions = %+v, want 1 (both requests share the conversation key)", feed.Sessions)
	}
	sf := feed.Sessions[0]
	if sf.NewTok < 3900 || sf.NewTok > 5000 {
		t.Errorf("session newTok = %d, want ≈4001 (whole-conversation prefill estimate)", sf.NewTok)
	}
	var found429 bool
	for _, q := range sf.Reqs {
		if q.Status == http.StatusTooManyRequests {
			found429 = true
			if q.NewTok < 3900 || q.NewTok > 5000 {
				t.Errorf("429 frame newTok = %d, want ≈4001 (whole-conversation estimate, not last-message)", q.NewTok)
			}
		}
	}
	if !found429 {
		t.Errorf("session reqs = %+v, want a 429 frame (the rejected request folded in)", sf.Reqs)
	}

	// 2. Burst feed: the same request's newTokensEst is last-message-only.
	var found bool
	for _, rs := range metrics.burst(100) {
		if rs.Status == http.StatusTooManyRequests {
			found = true
			if rs.NewTokensEst < 0 || rs.NewTokensEst > 16 {
				t.Errorf("burst newTokensEst = %d, want the last-message estimate (~1), not the whole conversation", rs.NewTokensEst)
			}
		}
	}
	if !found {
		t.Fatal("no 429 sample in the burst ring")
	}
}
