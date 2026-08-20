package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// rawContentLen
// ---------------------------------------------------------------------------

func TestRawContentLen(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want int
	}{
		{
			name: "empty RawMessage",
			raw:  ``,
			want: 0,
		},
		{
			name: "single byte (too short for string)",
			raw:  `"`,
			want: 0,
		},
		{
			name: "empty string",
			raw:  `""`,
			want: 0,
		},
		{
			name: "simple string",
			raw:  `"hello"`,
			want: 5,
		},
		{
			name: "string with escape sequences overcounts escape bytes",
			raw:  `"hello\nworld"`,
			want: 12, // len(`"hello\nworld"`) - 2 = 12; actual decoded is 11, but overcount is safe
		},
		{
			name: "UTF-8 multibyte overestimate",
			raw:  `"日本語"`,
			want: 9, // len([]byte("日本語")) = 9, which is len(raw) - 2
		},
		{
			name: "array of plain strings (not objects) returns 0",
			raw:  `["hello", "world"]`,
			want: 0, // unmarshal into []struct{Text json.RawMessage} fails, returns 0
		},
		{
			name: "array with object text field",
			raw:  `[{"text":"hello"}]`,
			want: 5,
		},
		{
			name: "array with object missing text field",
			raw:  `[{"other":"hello"}]`,
			want: 0,
		},
		{
			name: "array with mixed objects",
			raw:  `[{"text":"hi"},{"other":"bye"},{"text":"yo"}]`,
			want: 4, // "hi"=2 + "yo"=2
		},
		{
			name: "integer value",
			raw:  `123`,
			want: 0,
		},
		{
			name: "boolean true",
			raw:  `true`,
			want: 0,
		},
		{
			name: "boolean false",
			raw:  `false`,
			want: 0,
		},
		{
			name: "null",
			raw:  `null`,
			want: 0,
		},
		{
			name: "empty array",
			raw:  `[]`,
			want: 0,
		},
		{
			name: "long string 32k+ chars",
			raw:  `"` + strings.Repeat("a", 33000) + `"`,
			want: 33000,
		},
		{
			name: "overestimate errs safe: escape overcounts",
			raw:  `"a\n\t\b\f\r"`,
			want: 11, // len(raw)-2 = 11; decoded = 5, so overcount is safe direction
		},
		{
			name: "string with unicode escapes",
			raw:  `"a\u0042c"`,
			want: 8, // len(raw)-2 = 8; decoded = 3 ("aBc"), overcount is safe
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := rawContentLen(json.RawMessage(tt.raw))
			if got != tt.want {
				t.Errorf("rawContentLen(%q) = %d, want %d", tt.raw, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// estimateNewTokens
// ---------------------------------------------------------------------------

func TestEstimateNewTokens(t *testing.T) {
	tests := []struct {
		name     string
		body     *parsedBody
		want     int
		wantLess int // want <= got <= want (for approximate byte/token ratio tests)
	}{
		{
			name: "chat API one message all content is new",
			body: &parsedBody{
				Messages: []rawMessage{
					{Content: json.RawMessage(`"hello world"`)},
				},
			},
			want: 11 / 4, // 2
		},
		{
			name: "chat API two messages all content is new",
			body: &parsedBody{
				Messages: []rawMessage{
					{Content: json.RawMessage(`"first"`)},
					{Content: json.RawMessage(`"second"`)},
				},
			},
			want: (5 + 6) / 4, // 2
		},
		{
			name: "chat API three messages only last is new",
			body: &parsedBody{
				Messages: []rawMessage{
					{Content: json.RawMessage(`"first"`)},
					{Content: json.RawMessage(`"second"`)},
					{Content: json.RawMessage(`"last"`)},
				},
			},
			want: 4 / 4, // 1
		},
		{
			name: "chat API four messages only last is new",
			body: &parsedBody{
				Messages: []rawMessage{
					{Content: json.RawMessage(`"msg1"`)},
					{Content: json.RawMessage(`"msg2"`)},
					{Content: json.RawMessage(`"msg3"`)},
					{Content: json.RawMessage(`"msg4"`)},
				},
			},
			want: 4 / 4, // 1
		},
		{
			name: "completions API prompt field",
			body: &parsedBody{
				Prompt: json.RawMessage(`"complete this sentence"`),
			},
			want: 20 / 4, // 5
		},
		{
			name: "empty body no messages no prompt",
			body: &parsedBody{},
			want: 0,
		},
		{
			name: "large content 4M bytes verifies 4 bytes per token heuristic",
			body: &parsedBody{
				Messages: []rawMessage{
					{Content: json.RawMessage(`"` + strings.Repeat("a", 4_000_000) + `"`)},
				},
			},
			want: 4_000_000 / 4, // 1_000_000
		},
		{
			name: "8192 token boundary exact",
			body: &parsedBody{
				Messages: []rawMessage{
					{Content: json.RawMessage(`"` + strings.Repeat("a", 8192*4) + `"`)},
				},
			},
			want: 8192,
		},
		{
			name: "8191 tokens just below threshold",
			body: &parsedBody{
				Messages: []rawMessage{
					{Content: json.RawMessage(`"` + strings.Repeat("a", 8191*4) + `"`)},
				},
			},
			want: 8191,
		},
		{
			name: "8193 tokens just above threshold",
			body: &parsedBody{
				Messages: []rawMessage{
					{Content: json.RawMessage(`"` + strings.Repeat("a", 8193*4) + `"`)},
				},
			},
			want: 8193,
		},
		{
			name: "array content in messages sums correctly then divides by 4",
			body: &parsedBody{
				Messages: []rawMessage{
					{Content: json.RawMessage(`[{"text":"hello"},{"text":"world"}]`)},
				},
			},
			want: 10 / 4, // 2
		},
		{
			name: "mixed content types across messages",
			body: &parsedBody{
				Messages: []rawMessage{
					{Content: json.RawMessage(`"string content"`)},
					{Content: json.RawMessage(`[{"text":"array content"}]`)},
				},
			},
			want: (14 + 13) / 4, // 6
		},
		{
			name: "multi-turn boundary: exactly 2 messages counts all",
			body: &parsedBody{
				Messages: []rawMessage{
					{Content: json.RawMessage(`"a"`)},
					{Content: json.RawMessage(`"b"`)},
				},
			},
			want: (1 + 1) / 4, // 0 (floor division)
		},
		{
			name: "multi-turn boundary: 3 messages counts last only",
			body: &parsedBody{
				Messages: []rawMessage{
					{Content: json.RawMessage(`"aaaabbbbccccddddeeee"`)},
					{Content: json.RawMessage(`"fffFgggGhhhhIIIIjjjj"`)},
					{Content: json.RawMessage(`"last"`)},
				},
			},
			want: 4 / 4, // 1 — only "last", not the big ones
		},
		{
			name: "prompt with non-string type returns 0",
			body: &parsedBody{
				Prompt: json.RawMessage(`123`),
			},
			want: 0,
		},
		{
			name: "messages with null content",
			body: &parsedBody{
				Messages: []rawMessage{
					{Content: json.RawMessage(`null`)},
				},
			},
			want: 0,
		},
		{
			name: "completions with array prompt",
			body: &parsedBody{
				Prompt: json.RawMessage(`[{"text":"hello world"}]`),
			},
			want: 11 / 4, // 2
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := estimateNewTokens(tt.body)
			if tt.wantLess > 0 {
				if got < tt.wantLess || got > tt.want {
					t.Errorf("estimateNewTokens() = %d, expected between %d and %d", got, tt.wantLess, tt.want)
				}
			} else if got != tt.want {
				t.Errorf("estimateNewTokens() = %d, want %d", got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Integration: rawContentLen + estimateNewTokens interaction
// ---------------------------------------------------------------------------

func TestEstimateNewTokens_MultiTurnCaching(t *testing.T) {
	// Verify the multi-turn caching assumption: with >2 messages,
	// only the last message's content is counted as new tokens.
	// Prior messages, regardless of size, are assumed cached.

	bigContent := strings.Repeat("x", 100000)
	smallContent := "hi"

	body := &parsedBody{
		Messages: []rawMessage{
			{Content: json.RawMessage(`"` + bigContent + `"`)},   // 25000 tokens if counted
			{Content: json.RawMessage(`"` + bigContent + `"`)},   // 25000 tokens if counted
			{Content: json.RawMessage(`"` + smallContent + `"`)}, // only this one counted
		},
	}

	got := estimateNewTokens(body)
	want := 2 / 4 // 0 — floor division of "hi" (2 chars) / 4

	if got != want {
		t.Errorf("multi-turn caching: got %d tokens, want %d (only last message should count)", got, want)
	}

	// Verify that with 2 messages, the big content IS counted.
	body2 := &parsedBody{
		Messages: []rawMessage{
			{Content: json.RawMessage(`"` + bigContent + `"`)},
			{Content: json.RawMessage(`"` + bigContent + `"`)},
		},
	}

	got2 := estimateNewTokens(body2)
	want2 := (100000 + 100000) / 4 // 50000

	if got2 != want2 {
		t.Errorf("two-message case: got %d tokens, want %d (all messages should count)", got2, want2)
	}
}

// ---------------------------------------------------------------------------
// handleProxyError
// ---------------------------------------------------------------------------

func TestHandleProxyError_Classification(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		wantCode int
		wantErr  string // error.code in JSON body + X-Router-Reason
	}{
		{"connection refused", &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connect: connection refused")}, http.StatusServiceUnavailable, "backend_unavailable"},
		{"eof before response", io.EOF, http.StatusServiceUnavailable, "backend_unavailable"},
		{"i/o deadline", os.ErrDeadlineExceeded, http.StatusServiceUnavailable, "backend_unavailable"},
		{"malformed upstream response", errors.New("malformed HTTP response"), http.StatusBadGateway, "bad_gateway"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest("POST", "/v1/chat/completions", nil)

			handleProxyError(rec, req, tt.err, "qwen3.6-27b")

			if rec.Code != tt.wantCode {
				t.Errorf("status: got %d, want %d", rec.Code, tt.wantCode)
			}
			if got := rec.Header().Get("X-Router-Reason"); got != tt.wantErr {
				t.Errorf("X-Router-Reason: got %q, want %q", got, tt.wantErr)
			}
			if got := rec.Header().Get("Retry-After"); got == "" {
				t.Error("Retry-After header missing")
			}
			var body struct {
				Error struct {
					Message string `json:"message"`
					Type    string `json:"type"`
					Code    string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("response is not valid JSON: %v (body %q)", err, rec.Body.String())
			}
			if body.Error.Code != tt.wantErr {
				t.Errorf("error.code: got %q, want %q", body.Error.Code, tt.wantErr)
			}
			if body.Error.Type != "server_error" {
				t.Errorf("error.type: got %q, want %q", body.Error.Type, "server_error")
			}
			if !strings.Contains(body.Error.Message, "qwen3.6-27b") {
				t.Errorf("error.message should name the backend, got %q", body.Error.Message)
			}
		})
	}
}

func TestHandleProxyError_ClientCanceled(t *testing.T) {
	// Client went away mid-roundtrip: nothing can be written.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)

	handleProxyError(rec, req, context.Canceled, "qwen3.6-27b")

	if rec.Body.Len() != 0 {
		t.Errorf("expected no body on client cancel, got %q", rec.Body.String())
	}
}

func TestHandleProxyError_MidStream(t *testing.T) {
	// Engine died after the response started: the status line is already
	// committed, so the handler must leave the partial response untouched.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	tracker := &respTracker{ResponseWriter: rec}
	tracker.WriteHeader(http.StatusOK)
	tracker.Write([]byte("data: partial\n\n"))

	before := rec.Body.String()
	resetErr := &net.OpError{Op: "read", Net: "tcp", Err: errors.New("read: connection reset by peer")}
	handleProxyError(tracker, req, resetErr, "qwen3.6-27b")

	if rec.Body.String() != before {
		t.Errorf("mid-stream failure must not append an error body, got %q", rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Metrics registry (live observability)
// ---------------------------------------------------------------------------

// withRouterEnv replaces the router's global state for the duration of the
// test and restores it afterward.
func withRouterEnv(t *testing.T, list []Backend, slotMax, slotQueue map[string]int) {
	t.Helper()
	savedBackends := backends
	savedSlots := slots
	savedPrefillSlots := prefillSlots
	savedDurations := durations
	savedGpu := gpu
	savedProxies := proxies

	backends = list
	slots = make(map[string]*slotManager)
	prefillSlots = make(map[string]*slotManager)
	durations = make(map[string]*durationTracker)
	for _, b := range list {
		durations[b.Name] = &durationTracker{}
	}
	gpu = nil
	proxies = make(map[string]*httputil.ReverseProxy)
	for _, b := range list {
		if mx, ok := slotMax[b.Name]; ok && mx > 0 {
			mq := 2
			if q, ok := slotQueue[b.Name]; ok {
				mq = q
			}
			slots[b.Name] = newSlot(int32(mx), int32(mq))
		}
	}
	t.Cleanup(func() {
		backends = savedBackends
		slots = savedSlots
		prefillSlots = savedPrefillSlots
		durations = savedDurations
		gpu = savedGpu
		proxies = savedProxies
	})
}

func newTestMetrics() *metricsRegistry {
	return newMetricsRegistry()
}

func TestMetrics_RecordRequest_CounterIncrements(t *testing.T) {
	m := newTestMetrics()
	now := time.Now()
	m.recordRequest("primary", requestSample{
		T: now, Backend: "primary", Path: "/v1/chat/completions",
		Status: 200, Stream: false, DurMs: 12.5,
		NewTokensEst: 32, BytesIn: 100, BytesOut: 200,
	})
	m.recordRequest("primary", requestSample{
		T: now, Backend: "primary", Path: "/v1/chat/completions",
		Status: 429, DurMs: 3.2, NewTokensEst: 8, BytesIn: 50,
	})
	m.recordRequest("subject", requestSample{
		T: now, Backend: "subject", Path: "/v1/completions",
		Status: 200, NewTokensEst: 10, BytesIn: 10, BytesOut: 40,
	})

	s := m.snapshot("primary")
	if s.ReqTotal != 2 || s.BytesInTotal != 150 || s.BytesOutTotal != 200 || s.TokEstTotal != 40 {
		t.Errorf("primary counters = %+v, want reqTotal=2 bytesIn=150 bytesOut=200 tokEst=40", s)
	}
	if s.TTFTSampleCount != 0 {
		t.Errorf("ttftSampleCount = %d, want 0 (no streaming samples)", s.TTFTSampleCount)
	}
	s2 := m.snapshot("subject")
	if s2.ReqTotal != 1 || s2.TokEstTotal != 10 {
		t.Errorf("subject counters = %+v, want reqTotal=1 tokEst=10", s2)
	}
	// Unknown backend: zero snapshot, not a panic or stale value.
	if zero := m.snapshot("ghost"); zero != (backendMetricSnapshot{}) {
		t.Errorf("unknown backend snapshot = %+v, want zero", zero)
	}
}

func TestMetrics_TTFT_FirstByteEWMA(t *testing.T) {
	m := newTestMetrics()
	m.recordAccept("b", 0, 0)
	// recordFirstByte is the only TTFT entry point: it fires at first
	// response byte, while the request is still live.
	m.recordFirstByte("b", 100)
	m.recordFirstByte("b", 200)
	// zero/negative (non-streaming or unmeasured) is ignored.
	m.recordFirstByte("b", 0)

	s := m.snapshot("b")
	if s.TTFTSampleCount != 2 {
		t.Fatalf("ttftSampleCount = %d, want 2", s.TTFTSampleCount)
	}
	// EWMA (alpha 0.3): 100, then 0.3*200 + 0.7*100 = 130.
	if math.Abs(s.TTFTMs-130) > 1e-9 {
		t.Errorf("ttftMs = %v, want 130", s.TTFTMs)
	}
	// ttftMsNow is the most recent sample — what the live readout shows.
	if s.TTFTMsNow != 200 {
		t.Errorf("ttftMsNow = %v, want 200", s.TTFTMsNow)
	}
}

func TestMetrics_ComputeRates_LiveArrivalWindow(t *testing.T) {
	m := newTestMetrics()

	// 10 requests admitted this tick: 100 bytesIn + 200 bytesOut each,
	// 80 tokEst each. The live rate divides the pending window by the tick
	// interval — a full 30s-window rate, not a 500ms spike.
	for range 10 {
		m.recordAccept("b", 100, 80)
	}
	m.computeRates(500 * time.Millisecond)

	s := m.snapshot("b")
	// rate = pending / dt: (10*100 + 10*200)/0.5s in? No — the window
	// carries only the ADMITTED demand (bytesIn + tokEst), not the future
	// response bytes. reqRate = 10/0.5 = 20/s over the tick; that is the
	// true arrival rate this tick.
	if math.Abs(s.ReqRate-20.0) > 1e-9 {
		t.Errorf("reqRate = %v, want 20 (10 arrivals / 0.5s tick)", s.ReqRate)
	}
	// tokEstRate = 800/0.5 = 1600/s over the tick interval.
	if math.Abs(s.TokEstRate-1600.0) > 1e-9 {
		t.Errorf("tokEstRate = %v, want 1600 (800 est tokens / 0.5s)", s.TokEstRate)
	}
	// bytesRate = 1000/0.5 = 2000 B/s (admitted request bytes only;
	// response bytes are not demand — they are produced over the decode).
	if math.Abs(s.BytesRate-2000.0) > 1e-9 {
		t.Errorf("bytesRate = %v, want 2000", s.BytesRate)
	}

	// Nothing new this tick: the window decays by (dt/window)=0.5/30=1/60.
	// pending after tick 1: 10*(59/60)=9.8333; rate = pending/dt.
	m.computeRates(500 * time.Millisecond)
	s = m.snapshot("b")
	if math.Abs(s.ReqRate-19.6666666667) > 1e-6 { // 9.8333/0.5
		t.Errorf("reqRate after decay tick = %v, want 19.6667 (window decays, not resets)", s.ReqRate)
	}
	// pending is now 9.8333*(59/60) = 9.6667.

	// Completion does NOT reset the rate — it retires pending demand.
	m.recordRequest("b", requestSample{T: time.Now(), Backend: "b", Status: 200, BytesIn: 100, BytesOut: 200, NewTokensEst: 80, accepted: true})
	m.recordRequest("b", requestSample{T: time.Now(), Backend: "b", Status: 200, BytesIn: 100, BytesOut: 200, NewTokensEst: 80, accepted: true})
	m.computeRates(500 * time.Millisecond)
	s = m.snapshot("b")
	// pending after tick2 = 10*(59/60)^2 = 9.5056; minus 2 completions,
	// divided by 0.5s: 15.3389
	if math.Abs(s.ReqRate-15.338888888888885) > 1e-9 {
		t.Errorf("reqRate after 2 completions = %v, want 15.3389 (completions retire demand, decay keeps the tail)", s.ReqRate)
	}

	// Lifetime totals count completions only.
	if s.ReqTotal != 2 || s.BytesInTotal != 200 || s.BytesOutTotal != 400 || s.TokEstTotal != 160 {
		t.Errorf("lifetime counters = %+v, want reqTotal=2 bytesIn=200 bytesOut=400 tokEst=160", s)
	}
}

func TestMetrics_Rates_RejectionsDoNotInflate(t *testing.T) {
	m := newTestMetrics()
	// A 429 that never entered a backend has no accepted flag — it must not
	// add to (or retire) the pending window.
	m.recordRequest("b", requestSample{T: time.Now(), Backend: "b", Status: 429, BytesIn: 50, NewTokensEst: 8})
	m.computeRates(500 * time.Millisecond)
	s := m.snapshot("b")
	if s.ReqRate != 0 || s.TokEstRate != 0 || s.BytesRate != 0 {
		t.Errorf("rates after pure rejection = %+v, want all zero", s)
	}
	if s.ReqTotal != 1 {
		t.Errorf("reqTotal = %d, want 1 (rejection is a completed sample)", s.ReqTotal)
	}
}

func TestMetrics_BurstRing_EvictionAndLimit(t *testing.T) {
	m := newTestMetrics()
	m.ringCap = 500
	now := time.Now()
	for i := range 600 {
		m.recordRequest("b", requestSample{T: now.Add(time.Duration(i) * time.Millisecond), Backend: "b", Status: 200, NewTokensEst: i})
	}
	all := m.burst(1000)
	if len(all) != 500 {
		t.Fatalf("ring length = %d, want 500 after 600 inserts", len(all))
	}
	if all[0].NewTokensEst != 100 || all[499].NewTokensEst != 599 {
		t.Errorf("ring endpoints = [%d..%d], want [100..599] (newest last, oldest evicted)", all[0].NewTokensEst, all[499].NewTokensEst)
	}
	last10 := m.burst(10)
	if len(last10) != 10 || last10[0].NewTokensEst != 590 || last10[9].NewTokensEst != 599 {
		t.Errorf("burst(10) = [%d..%d] len=%d, want [590..599] len=10", firstTok(last10), lastTok(last10), len(last10))
	}
}

func firstTok(s []requestSample) int { return s[0].NewTokensEst }
func lastTok(s []requestSample) int  { return s[len(s)-1].NewTokensEst }

// ---------------------------------------------------------------------------
// engine metrics: parser + scrape semantics
// ---------------------------------------------------------------------------

func TestEngine_ParsePromText(t *testing.T) {
	body := []byte(`
# HELP vllm:num_requests_running Number of requests in model execution batches.
# TYPE vllm:num_requests_running gauge
vllm:num_requests_running{model_name="m",engine="0"} 3
vllm:num_requests_running{model_name="m",engine="1"} 2
vllm:kv_cache_usage_perc{model_name="m",engine="0"} 0.4
vllm:prompt_tokens_total{model_name="m",engine="0"} 1000
# a comment that looks like data
vllm:e2e_request_latency_seconds_bucket{le="+Inf"} 7
vllm:e2e_request_latency_seconds_count 5
vllm:e2e_request_latency_seconds_sum 12.5
not_a_number 1.2.3
`)
	samples := parsePromText(body)
	byName := map[string][]promSample{}
	for _, s := range samples {
		byName[s.name] = append(byName[s.name], s)
	}
	if len(byName["vllm:num_requests_running"]) != 2 {
		t.Fatalf("running samples = %d, want 2 (HELP/TYPE skipped)", len(byName["vllm:num_requests_running"]))
	}
	if byName["vllm:num_requests_running"][0].labels["model_name"] != "m" ||
		byName["vllm:num_requests_running"][0].labels["engine"] != "0" {
		t.Errorf("labels = %v, want model_name=m engine=0", byName["vllm:num_requests_running"][0].labels)
	}
	if byName["vllm:kv_cache_usage_perc"][0].value != 0.4 {
		t.Errorf("kv value = %v, want 0.4", byName["vllm:kv_cache_usage_perc"][0].value)
	}
	if byName["vllm:e2e_request_latency_seconds_sum"][0].value != 12.5 {
		t.Errorf("hist sum = %v, want 12.5", byName["vllm:e2e_request_latency_seconds_sum"][0].value)
	}
	if len(byName["not_a_number"]) != 0 {
		t.Errorf("unparseable line not skipped")
	}
}

func TestEngine_ParsePromText_EscapedLabel(t *testing.T) {
	body := []byte(`x{model_name="a\"b",engine="0"} 1
`)
	samples := parsePromText(body)
	if len(samples) != 1 || samples[0].labels["model_name"] != `a"b` {
		t.Fatalf("escaped label = %+v, want a\"b", samples)
	}
}

// fakeExpo serves a fixed Prometheus body for one scrape.
func TestEngine_ScrapeVllm(t *testing.T) {
	r := newEngineRegistry()
	r.add("king", "http://127.0.0.1:1/metrics") // url unused; we feed the body
	r.mu.Lock()
	s := r.backends["king"]
	r.mu.Unlock()

	// Prime the counter baseline ~2s ago: 1000 prompt tokens.
	key := counterKey(promSample{name: "vllm:prompt_tokens_total", labels: map[string]string{"model_name": "m", "engine": "0"}})
	s.counters[key] = 1000
	s.cTimes[key] = time.Now().Add(-2 * time.Second)

	// Two data-parallel engines: running sums, KV takes max, one scrape
	// interval later the counter has advanced 3000-1000=2000 tokens.
	body := `
vllm:num_requests_running{model_name="m",engine="0"} 3
vllm:num_requests_running{model_name="m",engine="1"} 2
vllm:num_requests_waiting{model_name="m",engine="0"} 1
vllm:num_requests_waiting{model_name="m",engine="1"} 0
vllm:kv_cache_usage_perc{model_name="m",engine="0"} 0.4
vllm:kv_cache_usage_perc{model_name="m",engine="1"} 0.6
vllm:prompt_tokens_total{model_name="m",engine="0"} 3000
vllm:generation_tokens_total{model_name="m",engine="0"} 500
vllm:time_to_first_token_seconds_sum{model_name="m",engine="0"} 10
vllm:time_to_first_token_seconds_count{model_name="m",engine="0"} 2
`
	s.scrapeBody(parsePromText([]byte(body)))
	first := s.view
	if first.Status != "ok" {
		t.Fatalf("status = %q, want ok", first.Status)
	}
	if first.Running != 5 || first.Waiting != 1 {
		t.Errorf("vllm gauges = running %v / waiting %v, want 5/1 (sum across engines)", first.Running, first.Waiting)
	}
	if first.KVPct != 60 {
		t.Errorf("kvPct = %v, want 60 (max across engines)", first.KVPct)
	}
	// ~2000 tokens over ~2s of real wall clock (wall-clock jitter ±1%).
	if first.PrefillTokS < 980 || first.PrefillTokS > 1020 {
		t.Errorf("prefillTokS = %v, want ~1000 (2000 tokens / ~2s)", first.PrefillTokS)
	}
	// TTFT mean needs two histogram samples: prime happened on this scrape,
	// so the mean arrives on the next one.
	body2 := `
vllm:num_requests_running{model_name="m",engine="0"} 3
vllm:num_requests_running{model_name="m",engine="1"} 2
vllm:kv_cache_usage_perc{model_name="m",engine="0"} 0.6
vllm:kv_cache_usage_perc{model_name="m",engine="1"} 0.4
vllm:time_to_first_token_seconds_sum{model_name="m",engine="0"} 30
vllm:time_to_first_token_seconds_count{model_name="m",engine="0"} 4
`
	s.scrapeBody(parsePromText([]byte(body2)))
	if got := s.view.TTFTms; got < 9900 || got > 10100 {
		t.Errorf("ttftMs = %v, want ~10000 ((30-10)/(4-2) s)", got)
	}
	// Counter reset (engine restart) re-baselines to 0 instead of negative.
	s2body := `
vllm:prompt_tokens_total{model_name="m",engine="0"} 10
`
	s.scrapeBody(parsePromText([]byte(s2body)))
	if s.view.PrefillTokS != 0 {
		t.Errorf("prefillTokS after counter reset = %v, want 0 (re-baseline)", s.view.PrefillTokS)
	}
}

func TestEngine_ScrapeSglang(t *testing.T) {
	r := newEngineRegistry()
	r.add("subject", "http://127.0.0.1:1/metrics")
	r.mu.Lock()
	s := r.backends["subject"]
	r.mu.Unlock()

	body := `
# scheduler gauges: replicated per tp rank -> max
sglang:num_running_reqs{model_name="m",engine_type="a",tp_rank="0",pp_rank="0",moe_ep_rank="0"} 4
sglang:num_running_reqs{model_name="m",engine_type="a",tp_rank="1",pp_rank="0",moe_ep_rank="0"} 4
sglang:num_queue_reqs{model_name="m",engine_type="a",tp_rank="0",pp_rank="0",moe_ep_rank="0"} 2
sglang:num_queue_reqs{model_name="m",engine_type="a",tp_rank="1",pp_rank="0",moe_ep_rank="0"} 2
sglang:token_usage{model_name="m",engine_type="a",tp_rank="0",pp_rank="0",moe_ep_rank="0"} 0.75
sglang:token_usage{model_name="m",engine_type="a",tp_rank="1",pp_rank="0",moe_ep_rank="0"} 0.75
sglang:realtime_tokens_total{model_name="m",engine_type="a",tp_rank="0",pp_rank="0",moe_ep_rank="0",mode="prefill_compute"} 4000
sglang:realtime_tokens_total{model_name="m",engine_type="a",tp_rank="0",pp_rank="0",moe_ep_rank="0",mode="prefill_cache"} 2000
sglang:realtime_tokens_total{model_name="m",engine_type="a",tp_rank="0",pp_rank="0",moe_ep_rank="0",mode="decode"} 6000
`
	s.scrapeBody(parsePromText([]byte(body)))
	if s.view.Status != "ok" {
		t.Fatalf("status = %q, want ok", s.view.Status)
	}
	if s.view.Running != 4 || s.view.Waiting != 2 {
		t.Errorf("sglang gauges = %v/%v, want 4/2 (max across ranks)", s.view.Running, s.view.Waiting)
	}
	if s.view.KVPct != 75 {
		t.Errorf("kvPct = %v, want 75", s.view.KVPct)
	}
	// First sight of a counter has no prior value: rates start at 0.
	if s.view.PrefillTokS != 0 || s.view.DecodeTokS != 0 {
		t.Errorf("first-sight rates = %v/%v, want 0/0 (no baseline yet)", s.view.PrefillTokS, s.view.DecodeTokS)
	}

	// realtime_tokens_total by mode: prefill = compute+cache, decode separate.
	body2 := `
sglang:num_running_reqs{model_name="m",engine_type="a",tp_rank="0",pp_rank="0",moe_ep_rank="0"} 4
sglang:token_usage{model_name="m",engine_type="a",tp_rank="0",pp_rank="0",moe_ep_rank="0"} 0.75
sglang:realtime_tokens_total{model_name="m",engine_type="a",tp_rank="0",pp_rank="0",moe_ep_rank="0",mode="prefill_compute"} 12000
sglang:realtime_tokens_total{model_name="m",engine_type="a",tp_rank="0",pp_rank="0",moe_ep_rank="0",mode="prefill_cache"} 6000
sglang:realtime_tokens_total{model_name="m",engine_type="a",tp_rank="0",pp_rank="0",moe_ep_rank="0",mode="decode"} 18000
`
	time.Sleep(300 * time.Millisecond)
	s.scrapeBody(parsePromText([]byte(body2)))
	// Δprefill = (12000-4000)+(6000-2000) = 12000 over ~0.3s wall clock.
	if s.view.PrefillTokS < 30000 || s.view.PrefillTokS > 60000 {
		t.Errorf("prefillTokS = %v, want ~40000/s (12000 tokens / ~0.3s)", s.view.PrefillTokS)
	}
	if s.view.DecodeTokS < 30000 {
		t.Errorf("decodeTokS = %v, want ~40000/s (12000 tokens / ~0.3s)", s.view.DecodeTokS)
	}
}

func TestEngine_EndpointOff(t *testing.T) {
	// A 404 (SGLang without --enable-metrics) must land on status "off",
	// not "err" — the sheet distinguishes "absent" from "unreachable".
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	r := newEngineRegistry()
	r.add("king", srv.URL+"/metrics")
	r.scrape("king")
	r.mu.Lock()
	status := r.backends["king"].status
	r.mu.Unlock()
	if status != "off" {
		t.Errorf("status = %q, want off (404 endpoint)", status)
	}

	// A dead endpoint is "err".
	r2 := newEngineRegistry()
	r2.add("dead", "http://127.0.0.1:1/metrics")
	r2.scrape("dead")
	r2.mu.Lock()
	status2 := r2.backends["dead"].status
	r2.mu.Unlock()
	if status2 != "err" {
		t.Errorf("status = %q, want err (unreachable)", status2)
	}
}

// frame mirrors the live metrics contract frame (field names, not types).
type frame struct {
	T        int64                  `json:"t"`
	Backends []frame_backends_entry `json:"backends"`
	GPU      struct {
		Used    int  `json:"used"`
		Budget  int  `json:"budget"`
		Waiting int  `json:"waiting"`
		Active  bool `json:"active"`
	} `json:"gpu"`
	Tier0Inflight int32 `json:"tier0Inflight"`
	Totals        struct {
		InFlight      int32   `json:"inFlight"`
		Waiting       int32   `json:"waiting"`
		ReqRate       float64 `json:"reqRate"`
		BytesRate     float64 `json:"bytesRate"`
		TokEstRate    float64 `json:"tokEstRate"`
		ReqTotal      int64   `json:"reqTotal"`
		BytesInTotal  int64   `json:"bytesInTotal"`
		BytesOutTotal int64   `json:"bytesOutTotal"`
		TokEstTotal   int64   `json:"tokEstTotal"`
	} `json:"totals"`
}

type frame_backend = frame_backends_entry

// frame_backends_entry mirrors one entry of the frame's backends[] array.
// frame.Backends is typed as []frame_backends_entry so frameBackend can
// return pointers into it.
type frame_backends_entry struct {
	Name            string  `json:"name"`
	URL             string  `json:"url"`
	Tier            int     `json:"tier"`
	InFlight        int32   `json:"inFlight"`
	MaxConcurrent   int32   `json:"maxConcurrent"`
	Waiting         int32   `json:"waiting"`
	MaxQueueDepth   int32   `json:"maxQueueDepth"`
	PrefillInFlight int32   `json:"prefillInFlight"`
	PrefillWaiting  int32   `json:"prefillWaiting"`
	PrefillMax      int32   `json:"prefillMax"`
	AvgDurationS    float64 `json:"avgDurationS"`
	EWMAms          float64 `json:"ewmaMs"`
	TTFTms          float64 `json:"ttftMs"`
	TTFTSampleCount int     `json:"ttftSampleCount"`
	TTFTMsNow       float64 `json:"ttftMsNow"`
	ReqRate         float64 `json:"reqRate"`
	BytesRate       float64 `json:"bytesRate"`
	TokEstRate      float64 `json:"tokEstRate"`
	ReqTotal        int64   `json:"reqTotal"`
	BytesInTotal    int64   `json:"bytesInTotal"`
	BytesOutTotal   int64   `json:"bytesOutTotal"`
	TokEstTotal     int64   `json:"tokEstTotal"`
	Engine          struct {
		Running     float64 `json:"running"`
		Waiting     float64 `json:"waiting"`
		KVPct       float64 `json:"kvPct"`
		PrefillTokS float64 `json:"prefillTokS"`
		DecodeTokS  float64 `json:"decodeTokS"`
		TTFTms      float64 `json:"ttftMs"`
		Status      string  `json:"status"`
	} `json:"engine"`
}

func frameBackend(t *testing.T, fr *frame, name string) *frame_backend {
	t.Helper()
	for i := range fr.Backends {
		if fr.Backends[i].Name == name {
			return &fr.Backends[i]
		}
	}
	t.Fatalf("backend %q not in frame", name)
	return nil
}

// sseFrames reads data frames off an SSE response one at a time.
func sseFrames(t *testing.T, resp *http.Response) *frameReader {
	t.Helper()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	return &frameReader{t: t, resp: resp, sc: sc}
}

type frameReader struct {
	t    *testing.T
	resp *http.Response
	sc   *bufio.Scanner
}

// nextFrame returns the next decoded SSE data frame.
func (r *frameReader) nextFrame() *frame {
	for r.sc.Scan() {
		line := r.sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		f := &frame{}
		if err := json.Unmarshal([]byte(line[len("data: "):]), f); err != nil {
			r.t.Fatalf("bad SSE frame %q: %v", line, err)
		}
		return f
	}
	r.t.Fatal("SSE stream ended without a data frame")
	return nil
}

// waitFor polls frames until pred matches or the deadline passes.
func (r *frameReader) waitFor(deadline time.Time, pred func(*frame) bool) *frame {
	for {
		f := r.nextFrame()
		if pred(f) {
			return f
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("timed out waiting for frame; last: %+v", f)
		}
	}
}

// close releases the SSE response.
func (r *frameReader) close() {
	r.resp.Body.Close()
}

// TestMetricsStream_LiveSnapshot proxies a held-in-flight streaming request
// and a 429 rejection through the real mux against a fake upstream, then
// asserts the SSE frame carries the contract's top-level keys and live
// values (inFlight, ttftSampleCount>0, gpu.active, tier0Inflight, totals).
//
// Sequence (deterministic via held fake responses):
//  1. stream req in-flight on "subject" -> assert inFlight/limits frame
//  2. release stream -> it completes; assert ttftSampleCount>0, tokEstTotal
//  3. king req held in-flight -> assert gpu.active + tier0Inflight
//  4. subject req while king busy -> 429 blocked-by-tier0
//  5. release king; burst feed contains streaming-200 + 429 samples
func TestMetricsStream_LiveSnapshot(t *testing.T) {
	withRouterEnv(t,
		[]Backend{
			{Name: "subject", URL: "http://127.0.0.1:1", Tier: 1, MaxConcurrent: 2, BlockOnTier0: 1},
			{Name: "king", URL: "http://127.0.0.1:1", Tier: 0, MaxConcurrent: 5},
		},
		map[string]int{"subject": 2, "king": 5},
		map[string]int{"subject": 2, "king": 4})
	// A configured GPU budget so the frame's gpu{} carries a real budget;
	// gpu.active must flip on when king (tier 0) goes in-flight.
	gpu = &gpuBudget{max: 4}
	gpu.cond = sync.NewCond(&gpu.mu)
	t.Cleanup(func() { gpu = nil })

	// holdStream / holdKing pin the two proxied requests in-flight until the
	// test closes them, making the frame-state -> 429 sequence deterministic
	// instead of a sleep race. Each hold also unblocks on client disconnect
	// so a test failure can't hang httptest.Server.Close.
	holdStream := make(chan struct{})
	holdKing := make(chan struct{})
	unblock := func(r *http.Request, ch <-chan struct{}) bool {
		select {
		case <-ch:
			return true
		case <-r.Context().Done():
			return false
		}
	}
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req parsedBody
		json.NewDecoder(r.Body).Decode(&req)
		if req.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			fl, _ := w.(http.Flusher)
			time.Sleep(150 * time.Millisecond) // deliberate first-byte delay -> measurable TTFT
			fmt.Fprintf(w, "data: chunk0\n\n")
			if fl != nil {
				fl.Flush()
			}
			// Hold the stream in-flight until the test closes holdStream:
			// guarantees an SSE frame is observable while subject.inFlight>=1.
			if !unblock(r, holdStream) {
				return
			}
			for i := range 2 {
				fmt.Fprintf(w, "data: chunk%d\n\n", i+1)
				if fl != nil {
					fl.Flush()
				}
				time.Sleep(30 * time.Millisecond)
			}
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		if !unblock(r, holdKing) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"cmpl-1"}`)
	}))
	defer fake.Close()
	proxies["subject"] = newTestProxy(fake.URL)
	proxies["king"] = newTestProxy(fake.URL)

	// Reset the shared registry so assertions see only this test's traffic.
	saveMetrics := metrics
	metrics = newTestMetrics()
	t.Cleanup(func() { metrics = saveMetrics })

	srv := httptest.NewServer(buildMux())
	defer srv.Close()

	// 1. Slow streaming request on "subject": first byte lands after ~150ms.
	streamDone := make(chan int, 1)
	go func() {
		resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json",
			strings.NewReader(`{"model":"subject","stream":true,"messages":[{"role":"user","content":"hello"}]}`))
		if err != nil {
			streamDone <- 599
			return
		}
		defer resp.Body.Close()
		io.Copy(io.Discard, resp.Body)
		streamDone <- resp.StatusCode
	}()

	// 2. Open the SSE feed (200ms interval = clamped floor).
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", srv.URL+"/metrics/stream?interval=200ms", nil)
	if err != nil {
		t.Fatalf("SSE request build: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("SSE request: %v", err)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("Cache-Control = %q, want no-cache", cc)
	}
	if ba := resp.Header.Get("X-Accel-Buffering"); ba != "no" {
		t.Errorf("X-Accel-Buffering = %q, want no", ba)
	}
	fr := sseFrames(t, resp)
	defer fr.close()
	deadline := time.Now().Add(10 * time.Second)

	// 3a. Frame while the streaming request is in flight: the streaming
	// request is in-flight, the limits match the config, and prefill is
	// absent (all zero).
	f := fr.waitFor(deadline, func(f *frame) bool {
		s := frameBackend(t, f, "subject")
		return len(f.Backends) == 2 && s.InFlight >= 1
	})
	subj := frameBackend(t, f, "subject")
	king := frameBackend(t, f, "king")
	if f.T <= 0 {
		t.Errorf("t = %d, want epoch ms > 0", f.T)
	}
	if subj.Tier != 1 || king.Tier != 0 {
		t.Errorf("tiers: subject=%d king=%d, want 1/0", subj.Tier, king.Tier)
	}
	if subj.MaxConcurrent != 2 || subj.MaxQueueDepth != 2 {
		t.Errorf("subject limits = %d/%d, want 2/2", subj.MaxConcurrent, subj.MaxQueueDepth)
	}
	if subj.PrefillMax != 0 || subj.PrefillInFlight != 0 || subj.PrefillWaiting != 0 {
		t.Errorf("subject prefill = %+v, want all 0 (no prefill limit)", subj)
	}
	if king.MaxConcurrent != 5 {
		t.Errorf("king.maxConcurrent = %d, want 5", king.MaxConcurrent)
	}

	// Release the stream: it now completes, and its sample (TTFT, tokEst,
	// bytesOut) lands in the counters and burst ring.
	close(holdStream)

	// 3b. Frame after the stream completes: TTFT sampled from the deliberate
	// 150ms first-byte delay, token estimate and req counter recorded.
	wantTok := estimateNewTokens(&parsedBody{
		Messages: []rawMessage{{Content: json.RawMessage(`"hello"`)}}})
	if wantTok == 0 {
		t.Fatal("test fixture must yield a nonzero token estimate")
	}
	fDone := fr.waitFor(deadline, func(f *frame) bool {
		s := frameBackend(t, f, "subject")
		return s.TTFTSampleCount >= 1
	})
	subj = frameBackend(t, fDone, "subject")
	if subj.ReqTotal < 1 {
		t.Errorf("subject.reqTotal = %d, want >= 1 (streaming request admitted)", subj.ReqTotal)
	}
	if subj.TTFTSampleCount < 1 || subj.TTFTms < 50 {
		t.Errorf("subject ttft = %v/%d, want >= 50ms with 1 sample (150ms first-byte delay)", subj.TTFTms, subj.TTFTSampleCount)
	}
	// TTFT is live: the first-byte sample appears in the frame as soon as
	// the head byte arrives — before the stream completes.
	if subj.TTFTMsNow < 50 {
		t.Errorf("subject.ttftMsNow = %v, want >= 50 (recorded at first byte, not completion)", subj.TTFTMsNow)
	}
	if subj.TokEstTotal != int64(wantTok) {
		t.Errorf("subject.tokEstTotal = %d, want %d (estimate of the test body)", subj.TokEstTotal, wantTok)
	}
	if fDone.Totals.ReqTotal < 1 {
		t.Errorf("totals.reqTotal = %d, want >= 1", fDone.Totals.ReqTotal)
	}

	// 4. 429 while "king" is busy: tier-1 subject is blocked by tier-0,
	// and gpu.active reflects the tier-0 in-flight request.
	kingBusy := make(chan struct{})
	go func() {
		resp, err := http.Post(srv.URL+"/v1/completions?model=king", "application/json",
			strings.NewReader(`{"model":"king","prompt":"slow"}`))
		if err != nil {
			close(kingBusy)
			return
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		close(kingBusy)
	}()
	fKing := fr.waitFor(deadline, func(f *frame) bool {
		return f.Tier0Inflight >= 1 && f.GPU.Active
	})
	if fKing.Tier0Inflight < 1 {
		t.Errorf("tier0Inflight = %d, want >= 1 (king in-flight)", fKing.Tier0Inflight)
	}
	if !fKing.GPU.Active {
		t.Errorf("gpu.active = false, want true (tier-0 in-flight > 0)")
	}
	if fKing.Totals.InFlight < 1 {
		t.Errorf("totals.inFlight = %d, want >= 1", fKing.Totals.InFlight)
	}

	resp2, err := http.Post(srv.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"subject","messages":[{"role":"user","content":"now"}]}`))
	if err != nil {
		t.Fatalf("blocked request: %v", err)
	}
	io.Copy(io.Discard, resp2.Body)
	resp2.Body.Close()
	if resp2.StatusCode != 429 {
		t.Errorf("blocked subject = %d, want 429 (blocked-by-tier0)", resp2.StatusCode)
	}
	if reason := resp2.Header.Get("X-Router-Reason"); reason != "blocked-by-tier0" {
		t.Errorf("X-Router-Reason = %q, want blocked-by-tier0", reason)
	}
	if status := <-streamDone; status != 200 {
		t.Errorf("streaming request status = %d, want 200", status)
	}
	// The 429 rejection sample is recorded by the handler BEFORE it writes the
	// response, so it is already in the ring by the time we read it here.
	// Release king's hold now: its 200 completion sample lands after the 429,
	// so the rejection is NOT the newest — but the burst assertion below only
	// requires the 429 be present, not newest. (See step 5.)
	close(holdKing)
	<-kingBusy

	// 5. Burst: the 429 rejection must be in the ring with queue-wait dur.
	burstResp, err := http.Get(srv.URL + "/metrics/burst")
	if err != nil {
		t.Fatalf("burst request: %v", err)
	}
	defer burstResp.Body.Close()
	var burst struct {
		Requests []burstReq `json:"requests"`
	}
	if err := json.NewDecoder(burstResp.Body).Decode(&burst); err != nil {
		t.Fatalf("burst decode: %v", err)
	}
	var rej *burstReq
	var streamed bool
	for i := range burst.Requests {
		rq := &burst.Requests[i]
		if rq.Status == 429 {
			rej = rq
		}
		if rq.Status == 200 && rq.Stream {
			streamed = true
			if rq.TTFTms < 50 || rq.BytesOut <= 0 {
				t.Errorf("streaming sample ttft=%v bytesOut=%d, want ttft>=50ms and bytesOut>0", rq.TTFTms, rq.BytesOut)
			}
		}
	}
	if rej == nil {
		t.Fatalf("no 429 rejection sample in burst: %+v", burst.Requests)
	}
	if rej.Backend != "subject" || rej.TTFTms != 0 || rej.BytesOut != 0 {
		t.Errorf("rejection sample = %+v, want backend=subject ttftMs=0 bytesOut=0", rej)
	}
	if !streamed {
		t.Errorf("no streaming 200 sample in burst: %+v", burst.Requests)
	}
	// Deterministic ring order (newest last): [streaming 200, 429, king 200].
	// The 429 is the newest *rejection* (nothing after it is a 4xx/5xx), and
	// the newest overall entry is the king's 200 completion (released last).
	var newestRejectStatus int
	for _, rq := range burst.Requests {
		if rq.Status >= 400 {
			newestRejectStatus = rq.Status
		}
	}
	if newestRejectStatus != 429 {
		t.Errorf("newest rejection in burst = %d, want 429", newestRejectStatus)
	}
	last := burst.Requests[len(burst.Requests)-1]
	if last.Status != 200 {
		t.Errorf("newest burst entry = %d, want 200 (king completion)", last.Status)
	}
}

// newTestProxy builds a reverse proxy (mirroring main()'s construction) for
// tests, pointing at an arbitrary upstream URL.
func newTestProxy(upstream string) *httputil.ReverseProxy {
	u, err := url.Parse(upstream)
	if err != nil {
		panic(err)
	}
	p := httputil.NewSingleHostReverseProxy(u)
	p.FlushInterval = -1
	p.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		handleProxyError(w, r, err, "test")
	}
	return p
}

// burstReq mirrors one entry of the /metrics/burst response.
type burstReq struct {
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

// TestMetricsStream_RouteShadowing verifies the proxy moved to /v1/ and that
// /v1/models + /metrics/stream are distinct, unshadowed routes.
func TestMetricsStream_RouteShadowing(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"object":"list","data":[{"id":"m1","object":"model","owned_by":"vllm"}]}`)
		default:
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"id":"proxied"}`)
		}
	}))
	defer fake.Close()

	withRouterEnv(t, []Backend{{Name: "m1", URL: fake.URL, Tier: 0}}, nil, nil)
	proxies["m1"] = newTestProxy(fake.URL)
	saveMetrics := metrics
	metrics = newTestMetrics()
	t.Cleanup(func() { metrics = saveMetrics })

	srv := httptest.NewServer(buildMux())
	defer srv.Close()

	// /v1/models serves the merged catalog, not the proxy.
	resp, err := http.Get(srv.URL + "/v1/models")
	if err != nil {
		t.Fatalf("models request: %v", err)
	}
	var models struct {
		Object string `json:"object"`
		Data   []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&models); err != nil {
		t.Fatalf("models decode: %v", err)
	}
	resp.Body.Close()
	if models.Object != "list" || len(models.Data) != 1 || models.Data[0].ID != "m1" {
		t.Errorf("models = %+v, want list with m1", models)
	}

	// /v1/... still proxies.
	resp2, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"m1","messages":[]}`))
	if err != nil {
		t.Fatalf("proxy request: %v", err)
	}
	var proxied struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp2.Body).Decode(&proxied); err != nil {
		t.Fatalf("proxy decode: %v", err)
	}
	resp2.Body.Close()
	if proxied.ID != "proxied" {
		t.Errorf("proxied = %+v, want id=proxied", proxied)
	}

	// /metrics/stream is a distinct route (SSE, not a 404/405 proxy miss).
	sctx, scancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer scancel()
	req, err := http.NewRequestWithContext(sctx, "GET", srv.URL+"/metrics/stream", nil)
	if err != nil {
		t.Fatalf("stream request build: %v", err)
	}
	resp3, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("stream request: %v", err)
	}
	if resp3.StatusCode != 200 || resp3.Header.Get("Content-Type") != "text/event-stream" {
		t.Errorf("stream status/content-type = %d/%q, want 200/text-event-stream", resp3.StatusCode, resp3.Header.Get("Content-Type"))
	}
	resp3.Body.Close()
}

func TestHandleBurst_LimitClamp(t *testing.T) {
	m := newTestMetrics()
	now := time.Now()
	for i := range 30 {
		m.recordRequest("b", requestSample{T: now, Backend: "b", Status: 200, NewTokensEst: i})
	}
	saveMetrics := metrics
	metrics = m
	t.Cleanup(func() { metrics = saveMetrics })

	srv := httptest.NewServer(buildMux())
	defer srv.Close()

	// limit=5 returns exactly 5, newest last.
	resp, err := http.Get(srv.URL + "/metrics/burst?limit=5")
	if err != nil {
		t.Fatalf("burst: %v", err)
	}
	var burst struct {
		Requests []struct {
			NewTokensEst int `json:"newTokensEst"`
		} `json:"requests"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&burst); err != nil {
		t.Fatalf("burst decode: %v", err)
	}
	resp.Body.Close()
	if len(burst.Requests) != 5 || burst.Requests[0].NewTokensEst != 25 || burst.Requests[4].NewTokensEst != 29 {
		t.Errorf("burst?limit=5 = %d entries [%d..%d], want 5 [25..29]",
			len(burst.Requests), firstTokInt(burst), lastTokInt(burst))
	}

	// limit over the max is clamped to 1000 (ring has only 30, so 30 back).
	resp2, err := http.Get(srv.URL + "/metrics/burst?limit=5000")
	if err != nil {
		t.Fatalf("burst: %v", err)
	}
	var burst2 struct {
		Requests []struct {
			NewTokensEst int `json:"newTokensEst"`
		} `json:"requests"`
	}
	if err := json.NewDecoder(resp2.Body).Decode(&burst2); err != nil {
		t.Fatalf("burst decode: %v", err)
	}
	resp2.Body.Close()
	if len(burst2.Requests) != 30 {
		t.Errorf("burst?limit=5000 = %d entries, want 30 (clamped)", len(burst2.Requests))
	}
}

func firstTokInt(b struct {
	Requests []struct {
		NewTokensEst int `json:"newTokensEst"`
	} `json:"requests"`
}) int {
	return b.Requests[0].NewTokensEst
}

func lastTokInt(b struct {
	Requests []struct {
		NewTokensEst int `json:"newTokensEst"`
	} `json:"requests"`
}) int {
	return b.Requests[len(b.Requests)-1].NewTokensEst
}
