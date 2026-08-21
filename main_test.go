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
	"regexp"
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
	if first.KVHeldPct != nil {
		t.Errorf("kvHeldPct = %v, want nil (vLLM's kv_cache_usage_perc already includes cached blocks)", *first.KVHeldPct)
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

// ptr helpers for engineView v2 optional-field assertions.
func ptr64f(v float64) *float64 { return &v }
func ptr64i(v int64) *int64     { return &v }
func deref64f(p *float64, field string, t *testing.T) float64 {
	t.Helper()
	if p == nil {
		t.Fatalf("%s is nil, want a value", field)
	}
	return *p
}
func deref64i(p *int64, field string, t *testing.T) int64 {
	t.Helper()
	if p == nil {
		t.Fatalf("%s is nil, want a value", field)
	}
	return *p
}
func wantNear(t *testing.T, got, want float64, field string) {
	t.Helper()
	if got < want*0.98 || got > want*1.02 {
		t.Errorf("%s = %v, want ~%v (±2%% wall-clock)", field, got, want)
	}
}

// rewindBaselines shifts the counter + histogram baselines back by d, so the
// next scrape's deltas span a known ~2s interval without a 2s test sleep
// (same pattern as TestEngine_ScrapeVllm's prime).
func rewindBaselines(s *engineScrape, d time.Duration) {
	for k := range s.cTimes {
		s.cTimes[k] = s.cTimes[k].Add(-d)
	}
	for _, h := range s.hists {
		h.t = h.t.Add(-d)
	}
}

// TestEngine_V2Vllm exercises the full vLLM v2 scrape: gauges, by-source
// prefill split, lifetime counters, finished-reason breakdown, cache hit
// rates, and every histogram-derived mean (latency in ms, tokens raw).
func TestEngine_V2Vllm(t *testing.T) {
	r := newEngineRegistry()
	r.add("king", "http://127.0.0.1:1/metrics")
	r.mu.Lock()
	s := r.backends["king"]
	r.mu.Unlock()

	// Prime (one scrape ~2s of wall clock ago): baselines for counters +
	// histograms.
	prime := `
vllm:prompt_tokens_total{model_name="m",engine="0"} 1000
vllm:generation_tokens_total{model_name="m",engine="0"} 100
vllm:prompt_tokens_by_source_total{model_name="m",engine="0",source="local_compute"} 900
vllm:prompt_tokens_by_source_total{model_name="m",engine="0",source="local_cache_hit"} 100
vllm:time_to_first_token_seconds_sum{model_name="m",engine="0"} 0.78
vllm:time_to_first_token_seconds_count{model_name="m",engine="0"} 10
vllm:inter_token_latency_seconds_sum{model_name="m",engine="0"} 0.78
vllm:inter_token_latency_seconds_count{model_name="m",engine="0"} 10
vllm:e2e_request_latency_seconds_sum{model_name="m",engine="0"} 2.36
vllm:e2e_request_latency_seconds_count{model_name="m",engine="0"} 10
vllm:request_queue_time_seconds_sum{model_name="m",engine="0"} 0.78
vllm:request_queue_time_seconds_count{model_name="m",engine="0"} 10
vllm:request_prefill_time_seconds_sum{model_name="m",engine="0"} 1.46
vllm:request_prefill_time_seconds_count{model_name="m",engine="0"} 10
vllm:request_decode_time_seconds_sum{model_name="m",engine="0"} 5.24
vllm:request_decode_time_seconds_count{model_name="m",engine="0"} 10
vllm:request_time_per_output_token_seconds_sum{model_name="m",engine="0"} 0.044
vllm:request_time_per_output_token_seconds_count{model_name="m",engine="0"} 20
vllm:request_prompt_tokens_sum{model_name="m",engine="0"} 2500
vllm:request_prompt_tokens_count{model_name="m",engine="0"} 1
vllm:request_generation_tokens_sum{model_name="m",engine="0"} 110
vllm:request_generation_tokens_count{model_name="m",engine="0"} 1
`
	s.scrapeBody(parsePromText([]byte(prime)))
	// Rewind baselines ~2s and let the wall clock add ~0.2s more: the
	// deltas span a known ~2.2s interval (jitter ±few %) with no 2s sleep.
	rewindBaselines(s, 2*time.Second)
	time.Sleep(200 * time.Millisecond)

	body := `
vllm:num_requests_running{model_name="m",engine="0"} 4
vllm:num_requests_waiting{model_name="m",engine="0"} 2
vllm:num_requests_waiting_by_reason{model_name="m",engine="0",reason="capacity"} 3
vllm:num_requests_waiting_by_reason{model_name="m",engine="0",reason="deferred"} 1
vllm:kv_cache_usage_perc{model_name="m",engine="0"} 0.45
vllm:prompt_tokens_total{model_name="m",engine="0"} 3000
vllm:generation_tokens_total{model_name="m",engine="0"} 3500
vllm:prompt_tokens_by_source_total{model_name="m",engine="0",source="local_compute"} 2900
vllm:prompt_tokens_by_source_total{model_name="m",engine="0",source="local_cache_hit"} 1500
vllm:request_success_total{model_name="m",engine="0",finished_reason="stop"} 50
vllm:request_success_total{model_name="m",engine="0",finished_reason="length"} 7
vllm:request_success_total{model_name="m",engine="0",finished_reason="abort"} 3
vllm:num_preemptions_total{model_name="m",engine="0"} 4
vllm:prefix_cache_queries_total{model_name="m",engine="0"} 1000
vllm:prefix_cache_hits_total{model_name="m",engine="0"} 791
vllm:mm_cache_queries_total{model_name="m",engine="0"} 20
vllm:mm_cache_hits_total{model_name="m",engine="0"} 17
vllm:time_to_first_token_seconds_sum{model_name="m",engine="0"} 1.78
vllm:time_to_first_token_seconds_count{model_name="m",engine="0"} 20
vllm:inter_token_latency_seconds_sum{model_name="m",engine="0"} 1.78
vllm:inter_token_latency_seconds_count{model_name="m",engine="0"} 30
vllm:e2e_request_latency_seconds_sum{model_name="m",engine="0"} 4.96
vllm:e2e_request_latency_seconds_count{model_name="m",engine="0"} 20
vllm:request_queue_time_seconds_sum{model_name="m",engine="0"} 1.78
vllm:request_queue_time_seconds_count{model_name="m",engine="0"} 30
vllm:request_prefill_time_seconds_sum{model_name="m",engine="0"} 2.56
vllm:request_prefill_time_seconds_count{model_name="m",engine="0"} 30
vllm:request_decode_time_seconds_sum{model_name="m",engine="0"} 6.84
vllm:request_decode_time_seconds_count{model_name="m",engine="0"} 30
vllm:request_time_per_output_token_seconds_sum{model_name="m",engine="0"} 0.164
vllm:request_time_per_output_token_seconds_count{model_name="m",engine="0"} 30
vllm:request_prompt_tokens_sum{model_name="m",engine="0"} 7500
vllm:request_prompt_tokens_count{model_name="m",engine="0"} 3
vllm:request_generation_tokens_sum{model_name="m",engine="0"} 370
vllm:request_generation_tokens_count{model_name="m",engine="0"} 3
`
	s.scrapeBody(parsePromText([]byte(body)))
	v := s.view

	if v.Status != "ok" || v.Engine != "vllm" {
		t.Fatalf("status/engine = %q/%q, want ok/vllm", v.Status, v.Engine)
	}
	if v.Running != 4 || v.Waiting != 2 || v.KVPct != 45 {
		t.Errorf("core gauges = %v/%v/%v, want 4/2/45", v.Running, v.Waiting, v.KVPct)
	}
	// by-source prefill split over the ~2.2s window: compute 2000 tok,
	// cache 1400 tok; decode 3400 tok.
	wantNear(t, v.PrefillTokS, 909, "prefillTokS")
	wantNear(t, v.PrefillCacheTokS, 636, "prefillCacheTokS")
	wantNear(t, v.DecodeTokS, 1545, "decodeTokS")
	// Finished-reason breakdown + lifetime.
	if v.ReqDoneTotal != 60 {
		t.Errorf("reqDoneTotal = %d, want 60 (Σ finished reasons)", v.ReqDoneTotal)
	}
	if v.FinReasons == nil || v.FinReasons["stop"] != 50 || v.FinReasons["length"] != 7 || v.FinReasons["abort"] != 3 {
		t.Errorf("finReasons = %v, want stop=50 length=7 abort=3", v.FinReasons)
	}
	if got := deref64i(v.PreemptedTotal, "preemptedTotal", t); got != 4 {
		t.Errorf("preemptedTotal = %d, want 4", got)
	}
	if got := deref64f(v.WaitCap, "waitCap", t); got != 3 {
		t.Errorf("waitCap = %v, want 3", got)
	}
	if got := deref64f(v.WaitDefer, "waitDefer", t); got != 1 {
		t.Errorf("waitDefer = %v, want 1", got)
	}
	// Cache hit rates (lifetime hits/queries) + raw totals.
	if v.HitRate < 0.78 || v.HitRate > 0.80 {
		t.Errorf("hitRate = %v, want ~0.791", v.HitRate)
	}
	if got := deref64i(v.HitQueriesTotal, "hitQueriesTotal", t); got != 1000 {
		t.Errorf("hitQueriesTotal = %d, want 1000", got)
	}
	if got := deref64i(v.HitHitsTotal, "hitHitsTotal", t); got != 791 {
		t.Errorf("hitHitsTotal = %d, want 791", got)
	}
	if got := deref64i(v.MmQueriesTotal, "mmQueriesTotal", t); got != 20 {
		t.Errorf("mmQueriesTotal = %d, want 20", got)
	}
	if got := deref64i(v.MmHitsTotal, "mmHitsTotal", t); got != 17 {
		t.Errorf("mmHitsTotal = %d, want 17", got)
	}
	if got := deref64i(v.PromptTokTotal, "promptTokTotal", t); got != 3000 {
		t.Errorf("promptTokTotal = %d, want 3000 (latest raw counter)", got)
	}
	if got := deref64i(v.GenTokTotal, "genTokTotal", t); got != 3500 {
		t.Errorf("genTokTotal = %d, want 3500", got)
	}
	// Latency means (ms) over the ~2.2s interval.
	wantNear(t, v.TTFTms, 100, "ttftMs")                                // (1.78-0.78)/(20-10)
	wantNear(t, v.ITLms, 50, "itlMs")                                   // (1.78-0.78)/(30-10)
	wantNear(t, v.E2EMs, 260, "e2eMs")                                  // (4.96-2.36)/(20-10)
	wantNear(t, v.QueueMs, 50, "queueMs")                               // (1.78-0.78)/(30-10)
	wantNear(t, deref64f(v.PrefillMs, "prefillMs", t), 55, "prefillMs") // (2.56-1.46)/(30-10)
	wantNear(t, deref64f(v.DecodeMs, "decodeMs", t), 80, "decodeMs")    // (6.84-5.24)/(30-10)
	wantNear(t, deref64f(v.PerTokMs, "perTokMs", t), 12, "perTokMs")    // (0.164-0.044)/(30-20)
	// Token histograms are RAW token counts, NOT ×1000.
	wantNear(t, v.MeanPromptTok, 2500, "meanPromptTok")
	wantNear(t, v.MeanGenTok, 130, "meanGenTok")
	if v.MeanPromptTok > 10000 {
		t.Errorf("meanPromptTok = %v, looks ms-converted (want token values)", v.MeanPromptTok)
	}
	// SGLang-only fields must be nil on vLLM.
	if v.TTFTStreamMs != nil || v.TTFTNonStreamMs != nil {
		t.Errorf("vllm ttft stream split = %v/%v, want nil", v.TTFTStreamMs, v.TTFTNonStreamMs)
	}
	if v.FullPct != nil || v.SwaPct != nil || v.MambaPct != nil {
		t.Errorf("vllm pool pct = %v/%v/%v, want nil", v.FullPct, v.SwaPct, v.MambaPct)
	}
	if v.AbortedTotal != nil || v.StreamDoneTotal != nil || v.NonStreamDoneTotal != nil {
		t.Errorf("vllm sglang counters non-nil: %v/%v/%v", v.AbortedTotal, v.StreamDoneTotal, v.NonStreamDoneTotal)
	}
	if v.Retracted != nil || v.RetractedTokS != nil {
		t.Errorf("vllm retracted = %v/%v, want nil", v.Retracted, v.RetractedTokS)
	}
	if v.KVMemGB != nil || v.SLOCap != nil || v.CTXLen != nil {
		t.Errorf("vllm mem/slo/ctx non-nil: %v/%v/%v", v.KVMemGB, v.SLOCap, v.CTXLen)
	}
}

// TestEngine_V2Sglang exercises the full SGLang v2 scrape: pool gauges (MAX
// across ranks), token pool stats, retraction, aborted/streaming splits,
// is_streaming-split TTFT, and raw token histogram means.
func TestEngine_V2Sglang(t *testing.T) {
	r := newEngineRegistry()
	r.add("subject", "http://127.0.0.1:1/metrics")
	r.mu.Lock()
	s := r.backends["subject"]
	r.mu.Unlock()

	// Prime ~2s ago (rank 0 only is enough to baseline the metrics that
	// appear on both ranks; rank 1 baselines on this scrape's first sight).
	prime := `
sglang:num_retracted_input_tokens_total{tp_rank="0"} 100
sglang:realtime_tokens_total{tp_rank="0",mode="prefill_compute"} 2100
sglang:realtime_tokens_total{tp_rank="1",mode="prefill_compute"} 2100
sglang:realtime_tokens_total{tp_rank="0",mode="prefill_cache"} 500
sglang:realtime_tokens_total{tp_rank="1",mode="prefill_cache"} 500
sglang:realtime_tokens_total{tp_rank="0",mode="decode"} 1000
sglang:realtime_tokens_total{tp_rank="1",mode="decode"} 1000
sglang:time_to_first_token_seconds_sum{is_streaming="true"} 0.78
sglang:time_to_first_token_seconds_count{is_streaming="true"} 1
sglang:time_to_first_token_seconds_sum{is_streaming="false"} 0.45
sglang:time_to_first_token_seconds_count{is_streaming="false"} 1
sglang:inter_token_latency_seconds_sum 1.44
sglang:inter_token_latency_seconds_count 20
sglang:e2e_request_latency_seconds_sum 2.36
sglang:e2e_request_latency_seconds_count 10
sglang:queue_time_seconds_sum 0.78
sglang:queue_time_seconds_count 10
sglang:prompt_tokens_histogram_sum 1722
sglang:prompt_tokens_histogram_count 1
sglang:generation_tokens_histogram_sum 100
sglang:generation_tokens_histogram_count 1
`
	s.scrapeBody(parsePromText([]byte(prime)))
	// Same ~2.2s window as the vLLM test (rewind 2s + 200ms of wall clock).
	rewindBaselines(s, 2*time.Second)
	time.Sleep(200 * time.Millisecond)

	body := `
sglang:num_running_reqs{tp_rank="0"} 8
sglang:num_running_reqs{tp_rank="1"} 8
sglang:num_queue_reqs{tp_rank="0"} 3
sglang:num_queue_reqs{tp_rank="1"} 3
sglang:token_usage{tp_rank="0"} 0.7
sglang:token_usage{tp_rank="1"} 0.7
sglang:full_token_usage{tp_rank="0"} 0.6
sglang:swa_token_usage{tp_rank="0"} 0.25
sglang:mamba_usage{tp_rank="0"} 0.1
sglang:num_used_tokens{tp_rank="0"} 120000
sglang:max_total_num_tokens{tp_rank="0"} 160000
sglang:kv_available_tokens{tp_rank="0"} 20000
sglang:kv_evictable_tokens{tp_rank="0"} 5000
sglang:mamba_used_tokens{tp_rank="0"} 1000
sglang:mamba_available_tokens{tp_rank="0"} 3000
sglang:mamba_evictable_tokens{tp_rank="0"} 500
sglang:num_retracted_reqs{tp_rank="0"} 2
sglang:cache_hit_rate{tp_rank="0"} 0.42
sglang:max_running_requests_under_SLO{tp_rank="0"} 64
sglang:context_len{tp_rank="0"} 131072
sglang:kv_cache_memory_usage_gb{tp_rank="0"} 30.5
sglang:weight_memory_usage_gb{tp_rank="0"} 20.2
sglang:hicache_host_used_tokens{tp_rank="0"} 4000
sglang:hicache_host_total_tokens{tp_rank="0"} 8000
sglang:num_retracted_input_tokens_total{tp_rank="0"} 500
sglang:num_aborted_requests_total{tp_rank="0"} 11
sglang:num_requests_total{is_streaming="true",tp_rank="0"} 300
sglang:num_requests_total{is_streaming="false",tp_rank="0"} 200
sglang:prompt_tokens_total{is_streaming="true",tp_rank="0"} 5000
sglang:prompt_tokens_total{is_streaming="false",tp_rank="0"} 5000
sglang:generation_tokens_total{is_streaming="true",tp_rank="0"} 3000
sglang:generation_tokens_total{is_streaming="false",tp_rank="0"} 3000
sglang:realtime_tokens_total{tp_rank="0",mode="prefill_compute"} 4400
sglang:realtime_tokens_total{tp_rank="1",mode="prefill_compute"} 4400
sglang:realtime_tokens_total{tp_rank="0",mode="prefill_cache"} 2100
sglang:realtime_tokens_total{tp_rank="1",mode="prefill_cache"} 2100
sglang:realtime_tokens_total{tp_rank="0",mode="decode"} 9000
sglang:realtime_tokens_total{tp_rank="1",mode="decode"} 9000
sglang:time_to_first_token_seconds_sum{is_streaming="true"} 1.78
sglang:time_to_first_token_seconds_count{is_streaming="true"} 2
sglang:time_to_first_token_seconds_sum{is_streaming="false"} 0.75
sglang:time_to_first_token_seconds_count{is_streaming="false"} 3
sglang:inter_token_latency_seconds_sum 6.24
sglang:inter_token_latency_seconds_count 40
sglang:e2e_request_latency_seconds_sum 4.96
sglang:e2e_request_latency_seconds_count 20
sglang:queue_time_seconds_sum 1.78
sglang:queue_time_seconds_count 30
sglang:prompt_tokens_histogram_sum 4667
sglang:prompt_tokens_histogram_count 4
sglang:generation_tokens_histogram_sum 280
sglang:generation_tokens_histogram_count 3
`
	s.scrapeBody(parsePromText([]byte(body)))
	v := s.view

	if v.Status != "ok" || v.Engine != "sglang" {
		t.Fatalf("status/engine = %q/%q, want ok/sglang", v.Status, v.Engine)
	}
	if v.Running != 8 || v.Waiting != 3 || v.KVPct != 70 {
		t.Errorf("core gauges = %v/%v/%v, want 8/3/70 (max across ranks)", v.Running, v.Waiting, v.KVPct)
	}
	// Pool pcts (×100).
	wantNear(t, deref64f(v.FullPct, "fullPct", t), 60, "fullPct")
	wantNear(t, deref64f(v.SwaPct, "swaPct", t), 25, "swaPct")
	wantNear(t, deref64f(v.MambaPct, "mambaPct", t), 10, "mambaPct")
	// Pool token stats (MAX across ranks).
	wantNear(t, deref64f(v.KVUsedTok, "kvUsedTok", t), 120000, "kvUsedTok")
	wantNear(t, deref64f(v.KVCapTok, "kvCapTok", t), 160000, "kvCapTok")
	wantNear(t, deref64f(v.KVFreeTok, "kvFreeTok", t), 20000, "kvFreeTok")
	wantNear(t, deref64f(v.KVEvictTok, "kvEvictTok", t), 5000, "kvEvictTok")
	// Held (prefix-cached) share of the hand-out pool: 100·5000/(20000+5000).
	wantNear(t, deref64f(v.KVHeldPct, "kvHeldPct", t), 20, "kvHeldPct")
	wantNear(t, deref64f(v.MambaUsedTok, "mambaUsedTok", t), 1000, "mambaUsedTok")
	wantNear(t, deref64f(v.MambaCapTok, "mambaCapTok", t), 4500, "mambaCapTok") // used+avail+evict
	wantNear(t, deref64f(v.HiCacheHostUsedTok, "hicacheHostUsedTok", t), 4000, "hicacheHostUsedTok")
	wantNear(t, deref64f(v.HiCacheHostCapTok, "hicacheHostCapTok", t), 8000, "hicacheHostCapTok")
	// Retraction.
	wantNear(t, deref64f(v.Retracted, "retracted", t), 2, "retracted")
	wantNear(t, deref64f(v.RetractedTokS, "retractedTokS", t), 182, "retractedTokS") // (500-100)/~2.2s
	if got := deref64i(v.AbortedTotal, "abortedTotal", t); got != 11 {
		t.Errorf("abortedTotal = %d, want 11", got)
	}
	// Streaming splits + lifetime sums (both is_streaming summed).
	if got := deref64i(v.StreamDoneTotal, "streamDoneTotal", t); got != 300 {
		t.Errorf("streamDoneTotal = %d, want 300", got)
	}
	if got := deref64i(v.NonStreamDoneTotal, "nonStreamDoneTotal", t); got != 200 {
		t.Errorf("nonStreamDoneTotal = %d, want 200", got)
	}
	if v.ReqDoneTotal != 500 {
		t.Errorf("reqDoneTotal = %d, want 500 (stream+nonStream)", v.ReqDoneTotal)
	}
	if got := deref64i(v.PromptTokTotal, "promptTokTotal", t); got != 10000 {
		t.Errorf("promptTokTotal = %d, want 10000 (Σ is_streaming)", got)
	}
	if got := deref64i(v.GenTokTotal, "genTokTotal", t); got != 6000 {
		t.Errorf("genTokTotal = %d, want 6000", got)
	}
	if v.PreemptedTotal != nil {
		t.Errorf("preemptedTotal = %v, want nil (vLLM-only)", v.PreemptedTotal)
	}
	if v.FinReasons != nil {
		t.Errorf("finReasons = %v, want nil (vLLM-only)", v.FinReasons)
	}
	// Hit rate gauge (fraction).
	if v.HitRate < 0.41 || v.HitRate > 0.43 {
		t.Errorf("hitRate = %v, want ~0.42", v.HitRate)
	}
	// Rates over the ~2.2s interval: prefill = compute+cache summed across
	// both ranks = (4400-2100)×2 + (2100-500)×2 = 7800 tok;
	// decode = (9000-1000)×2 = 16000 tok.
	wantNear(t, v.PrefillTokS, 3545, "prefillTokS") // 7800/2.2
	wantNear(t, v.DecodeTokS, 7273, "decodeTokS")   // 16000/2.2
	// is_streaming-split TTFT means must be distinct; blended is a mix.
	stream := deref64f(v.TTFTStreamMs, "ttftStreamMs", t)
	nonStream := deref64f(v.TTFTNonStreamMs, "ttftNonStreamMs", t)
	wantNear(t, stream, 1000, "ttftStreamMs")      // (1.78-0.78)/(2-1) s
	wantNear(t, nonStream, 150, "ttftNonStreamMs") // (0.75-0.45)/(3-1) s
	if math.Abs(stream-nonStream) < 1 {
		t.Errorf("ttft stream/nonstream = %v/%v, want distinct per-label means", stream, nonStream)
	}
	wantNear(t, v.TTFTms, 433, "ttftMs (blended)") // (2.53-1.23)/(5-2) s
	wantNear(t, v.ITLms, 240, "itlMs")             // (6.24-1.44)/(40-20) s
	wantNear(t, v.E2EMs, 260, "e2eMs")             // (4.96-2.36)/(20-10) s
	wantNear(t, v.QueueMs, 50, "queueMs")          // (1.78-0.78)/(30-10) s
	// Raw token histogram means (NOT ms-converted).
	wantNear(t, v.MeanPromptTok, 981.67, "meanPromptTok") // (4667-1722)/(4-1)
	wantNear(t, v.MeanGenTok, 90, "meanGenTok")           // (280-100)/(3-1)
	// Capacity + memory.
	wantNear(t, deref64f(v.SLOCap, "sloCap", t), 64, "sloCap")
	wantNear(t, deref64f(v.CTXLen, "ctxLen", t), 131072, "ctxLen")
	wantNear(t, deref64f(v.KVMemGB, "kvMemGB", t), 30.5, "kvMemGB")
	wantNear(t, deref64f(v.WeightMemGB, "weightMemGB", t), 20.2, "weightMemGB")
	// vLLM-only fields must be nil on SGLang.
	if v.WaitCap != nil || v.WaitDefer != nil {
		t.Errorf("sglang waitCap/waitDefer = %v/%v, want nil", v.WaitCap, v.WaitDefer)
	}
	if v.PrefillMs != nil || v.DecodeMs != nil || v.PerTokMs != nil {
		t.Errorf("sglang per-stage ms = %v/%v/%v, want nil (vLLM-only)", v.PrefillMs, v.DecodeMs, v.PerTokMs)
	}
	if v.HitQueriesTotal != nil || v.MmQueriesTotal != nil {
		t.Errorf("sglang cache totals non-nil: %v/%v", v.HitQueriesTotal, v.MmQueriesTotal)
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
		Status             string           `json:"status"`
		Engine             string           `json:"engine"`
		Running            float64          `json:"running"`
		Waiting            float64          `json:"waiting"`
		KVPct              float64          `json:"kvPct"`
		HitRate            float64          `json:"hitRate"`
		PrefillTokS        float64          `json:"prefillTokS"`
		PrefillCacheTokS   float64          `json:"prefillCacheTokS"`
		DecodeTokS         float64          `json:"decodeTokS"`
		TTFTms             float64          `json:"ttftMs"`
		ITLms              float64          `json:"itlMs"`
		E2EMs              float64          `json:"e2eMs"`
		QueueMs            float64          `json:"queueMs"`
		PrefillMs          *float64         `json:"prefillMs"`
		DecodeMs           *float64         `json:"decodeMs"`
		PerTokMs           *float64         `json:"perTokMs"`
		MeanPromptTok      float64          `json:"meanPromptTok"`
		MeanGenTok         float64          `json:"meanGenTok"`
		WaitCap            *float64         `json:"waitCap"`
		WaitDefer          *float64         `json:"waitDefer"`
		Retracted          *float64         `json:"retracted"`
		RetractedTokS      *float64         `json:"retractedTokS"`
		FullPct            *float64         `json:"fullPct"`
		SwaPct             *float64         `json:"swaPct"`
		MambaPct           *float64         `json:"mambaPct"`
		KVUsedTok          *float64         `json:"kvUsedTok"`
		KVCapTok           *float64         `json:"kvCapTok"`
		KVFreeTok          *float64         `json:"kvFreeTok"`
		KVEvictTok         *float64         `json:"kvEvictTok"`
		MambaUsedTok       *float64         `json:"mambaUsedTok"`
		MambaCapTok        *float64         `json:"mambaCapTok"`
		HiCacheHostUsedTok *float64         `json:"hicacheHostUsedTok"`
		HiCacheHostCapTok  *float64         `json:"hicacheHostCapTok"`
		TTFTStreamMs       *float64         `json:"ttftStreamMs"`
		TTFTNonStreamMs    *float64         `json:"ttftNonStreamMs"`
		KVMemGB            *float64         `json:"kvMemGB"`
		WeightMemGB        *float64         `json:"weightMemGB"`
		SLOCap             *float64         `json:"sloCap"`
		CTXLen             *float64         `json:"ctxLen"`
		PreemptedTotal     *int64           `json:"preemptedTotal"`
		AbortedTotal       *int64           `json:"abortedTotal"`
		StreamDoneTotal    *int64           `json:"streamDoneTotal"`
		NonStreamDoneTotal *int64           `json:"nonStreamDoneTotal"`
		PromptTokTotal     *int64           `json:"promptTokTotal"`
		GenTokTotal        *int64           `json:"genTokTotal"`
		HitQueriesTotal    *int64           `json:"hitQueriesTotal"`
		HitHitsTotal       *int64           `json:"hitHitsTotal"`
		MmQueriesTotal     *int64           `json:"mmQueriesTotal"`
		MmHitsTotal        *int64           `json:"mmHitsTotal"`
		ReqDoneTotal       int64            `json:"reqDoneTotal"`
		FinReasons         map[string]int64 `json:"finReasons"`
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

// ---------------------------------------------------------------------------
// Sessions (live stack + conversation view)
// ---------------------------------------------------------------------------

var hex12 = regexp.MustCompile(`^[0-9a-f]{12}$`)

func sessById(t *testing.T, f sessionsFeed, id string) *sessFrame {
	t.Helper()
	for i := range f.Sessions {
		if f.Sessions[i].ID == id {
			return &f.Sessions[i]
		}
	}
	t.Fatalf("session %q not in feed: %+v", id, f.Sessions)
	return nil
}

func TestSessions_Grouping(t *testing.T) {
	// Same conversation prefix, different last message → same key.
	a := &parsedBody{Messages: []rawMessage{
		{Content: json.RawMessage(`"hi"`)},
		{Content: json.RawMessage(`"what is up"`)},
	}}
	b := &parsedBody{Messages: []rawMessage{
		{Content: json.RawMessage(`"hi"`)},
		{Content: json.RawMessage(`"completely different question"`)},
	}}
	ka, kb := sessionKeyFor(a), sessionKeyFor(b)
	if ka == "" || !hex12.MatchString(ka) {
		t.Fatalf("key = %q, want 12-hex", ka)
	}
	if ka != kb {
		t.Errorf("keys differ for same prefix: %q vs %q", ka, kb)
	}
	// Different first message → different key.
	c := &parsedBody{Messages: []rawMessage{
		{Content: json.RawMessage(`"hello there"`)},
		{Content: json.RawMessage(`"what is up"`)},
	}}
	if kc := sessionKeyFor(c); kc == ka {
		t.Errorf("different first message produced same key %q", kc)
	}
	// Single message uses itself as the prefix.
	d := &parsedBody{Messages: []rawMessage{{Content: json.RawMessage(`"hi"`)}}}
	// Per the identity rule (prefix = all-but-last, single message uses
	// itself), a one-message chat "hi" and the FIRST turn of a 2-turn chat
	// "hi" share the same prefix → same key. The fork happens at turn 2.
	if kd := sessionKeyFor(d); kd != ka {
		t.Errorf("single-message prefix key = %q, want same as 2-turn first-turn key %q", kd, ka)
	}
	// Turn 2 of that chat forks a NEW session: prefix is now both messages.
	e2 := &parsedBody{Messages: []rawMessage{
		{Content: json.RawMessage(`"hi"`)},
		{Content: json.RawMessage(`"second"`)},
		{Content: json.RawMessage(`"third"`)},
	}}
	if k2 := sessionKeyFor(e2); k2 == ka {
		t.Errorf("3-turn prefix collided with 1-turn key: %q", k2)
	}
	// Completions (prompt only, no messages) → no session.
	e := &parsedBody{Prompt: json.RawMessage(`"write a poem"`)}
	if ke := sessionKeyFor(e); ke != "" {
		t.Errorf("completions key = %q, want empty (no session)", ke)
	}
}

func TestSessions_LiveFinishFlow(t *testing.T) {
	r := newSessionRegistry()

	id := r.start("b1", "/v1/chat/completions", true, "abcdef012345", 100, 40)
	f := r.feed()
	if len(f.Live) != 1 {
		t.Fatalf("live = %d, want 1", len(f.Live))
	}
	l := f.Live[0]
	if l.ID != id || l.Sess != "abcdef012345" || l.Backend != "b1" ||
		l.Path != "/v1/chat/completions" || !l.Stream || l.Phase != "admitted" ||
		l.CtxTok != 100 || l.NewTok != 40 {
		t.Errorf("live entry = %+v, want admitted stream req", l)
	}
	if s := sessById(t, f, "abcdef012345"); s.LiveN != 1 || !s.Active || s.N != 0 {
		t.Errorf("session while live = %+v, want liveN=1 active n=0", s)
	}

	r.firstByte(id)
	f = r.feed()
	if len(f.Live) != 1 || f.Live[0].Phase != "streaming" {
		t.Fatalf("phase after firstByte = %q, want streaming", f.Live[0].Phase)
	}

	// finish: 200 streaming, dur 2000ms, ttft 150ms, 800 bytes out.
	r.finish(id, 200, 2000, 150, 800)
	f = r.feed()
	if len(f.Live) != 0 {
		t.Fatalf("live after finish = %d, want 0", len(f.Live))
	}
	s := sessById(t, f, "abcdef012345")
	if s.N != 1 || s.LiveN != 0 || s.LastStatus != 200 {
		t.Errorf("session after finish = %+v, want n=1 liveN=0 status=200", s)
	}
	if s.CtxTok != 100 || s.NewTok != 40 || s.CachedTok != 60 {
		t.Errorf("tokens = ctx %d new %d cached %d, want 100/40/60", s.CtxTok, s.NewTok, s.CachedTok)
	}
	if s.TokOutTotal != 200 { // 800 bytes / 4
		t.Errorf("tokOutTotal = %d, want 200", s.TokOutTotal)
	}
	if math.Abs(s.AvgTTFTMs-150) > 0.001 {
		t.Errorf("avgTTFTMs = %v, want 150", s.AvgTTFTMs)
	}
	if math.Abs(s.TotalDurS-2) > 0.001 {
		t.Errorf("totalDurS = %v, want 2", s.TotalDurS)
	}
	if s.Reqs == nil || len(s.Reqs) != 1 {
		t.Fatalf("reqs = %+v, want 1 entry (active+recent+top20)", s.Reqs)
	}
	q := s.Reqs[0]
	if q.Status != 200 || !q.Stream || q.CtxTok != 100 || q.NewTok != 40 ||
		q.CachedTok != 60 || q.TokOut != 200 {
		t.Errorf("req = %+v, want 200 stream ctx100/new40/cached60/tokOut200", q)
	}
	if math.Abs(q.DurMs-2000) > 0.001 || math.Abs(q.TTFTms-150) > 0.001 {
		t.Errorf("req dur/ttft = %v/%v, want 2000/150", q.DurMs, q.TTFTms)
	}
	if math.Abs(q.TokS-100) > 0.001 {
		t.Errorf("req tokS = %v, want 100 (200 tok / 2s)", q.TokS)
	}

	// A rejection for the same conversation: n=2, 429 in reqs (oldest first).
	r.finishRejection("b1", "/v1/chat/completions", "abcdef012345", 100, 40, 429, 12.5)
	f = r.feed()
	s = sessById(t, f, "abcdef012345")
	if s.N != 2 {
		t.Fatalf("n after rejection = %d, want 2", s.N)
	}
	if len(s.Reqs) != 2 || s.Reqs[0].Status != 200 || s.Reqs[1].Status != 429 {
		t.Errorf("reqs order = %+v, want [200, 429] oldest first", s.Reqs)
	}
	if s.LastStatus != 429 {
		t.Errorf("lastStatus = %d, want 429 (rejection is latest)", s.LastStatus)
	}
	// The streaming-only TTFT avg must not be polluted by the rejection.
	if math.Abs(s.AvgTTFTMs-150) > 0.001 {
		t.Errorf("avgTTFTMs = %v, want 150 (rejection has no ttft)", s.AvgTTFTMs)
	}

	// A second request under the SAME key → same session id, n=3.
	id2 := r.start("b1", "/v1/chat/completions", true, "abcdef012345", 130, 20)
	f = r.feed()
	s = sessById(t, f, "abcdef012345")
	if s.LiveN != 1 {
		t.Errorf("liveN after 2nd start = %d, want 1", s.LiveN)
	}
	// ctx/newTok track the LATEST request.
	if s.CtxTok != 130 || s.NewTok != 20 {
		t.Errorf("ctx/new = %d/%d, want 130/20 (latest request)", s.CtxTok, s.NewTok)
	}
	r.finish(id2, 200, 1000, 0, 400) // non-stream ttft 0 → avg unchanged

	// A different key → a NEW session.
	other := "999999999999"
	r.finishRejection("b1", "/v1/chat/completions", other, 50, 10, 429, 1)
	f = r.feed()
	if len(f.Sessions) != 2 {
		t.Fatalf("sessions = %d, want 2 (distinct keys)", len(f.Sessions))
	}
	o := sessById(t, f, other)
	if o.N != 1 || o.Backend != "b1" {
		t.Errorf("other session = %+v, want n=1 b1", o)
	}

	// Non-chat requests: sess "" — never in the session list.
	nc := r.start("b1", "/v1/completions", false, "", 7, 3)
	r.finish(nc, 200, 500, 0, 100)
	f = r.feed()
	if len(f.Sessions) != 2 {
		t.Errorf("sessions after non-chat = %d, want 2 (non-chat excluded)", len(f.Sessions))
	}
}

func TestSessions_PrefillProgress(t *testing.T) {
	r := newSessionRegistry()

	// newTok=20000 at the default 10000 tok/s → estPrefillMs = 2000.
	id := r.start("b1", "/v1/chat/completions", true, "abcdef012345", 500, 20000)
	f := r.feed()
	if len(f.Live) != 1 {
		t.Fatalf("live = %d, want 1", len(f.Live))
	}
	l := f.Live[0]
	if l.Phase != "admitted" || l.StreamMs != 0 {
		t.Errorf("admitted = phase %q streamMs %d, want admitted/0", l.Phase, l.StreamMs)
	}
	if l.EstPrefillMs < 1999 || l.EstPrefillMs > 2001 {
		t.Errorf("estPrefillMs = %d, want 2000±1", l.EstPrefillMs)
	}

	// first byte: phase flips and streamMs becomes the wall timestamp.
	r.firstByte(id)
	f = r.feed()
	l = f.Live[0]
	if l.Phase != "streaming" {
		t.Fatalf("phase after firstByte = %q, want streaming", l.Phase)
	}
	if l.StreamMs < l.StartMs || l.StreamMs > time.Now().UnixMilli() {
		t.Errorf("streamMs = %d, want within [startMs %d, now]", l.StreamMs, l.StartMs)
	}

	// Non-streaming requests get no estimate (their first byte marks
	// generation end, not prefill end).
	ns := r.start("b1", "/v1/chat/completions", false, "", 100, 20000)
	f = r.feed()
	var nsFrame *sessLiveFrame
	for i := range f.Live {
		if f.Live[i].ID == ns {
			nsFrame = &f.Live[i]
		}
	}
	if nsFrame == nil {
		t.Fatalf("non-streaming live entry missing")
	}
	if nsFrame.EstPrefillMs != 0 {
		t.Errorf("non-streaming estPrefillMs = %d, want 0", nsFrame.EstPrefillMs)
	}
}

func TestSessions_Caps(t *testing.T) {
	r := newSessionRegistry()
	// 105 starts: live ordering must be startMs ascending (oldest on top)
	// and the feed must respect the 256 cap (all 105 fit here).
	ids := make([]uint64, 0, 105)
	for range 105 {
		ids = append(ids, r.start("b1", "/v1/chat/completions", false, "", 1, 1))
	}
	f := r.feed()
	if len(f.Live) != 105 {
		t.Fatalf("live = %d, want 105 (under the 256 feed cap)", len(f.Live))
	}
	for i := 1; i < len(f.Live); i++ {
		if f.Live[i].StartMs < f.Live[i-1].StartMs {
			t.Fatalf("live not startMs ASC at %d: %d < %d", i, f.Live[i].StartMs, f.Live[i-1].StartMs)
		}
	}
	if f.Live[0].ID != ids[0] {
		t.Errorf("oldest on top: live[0].ID = %d, want %d", f.Live[0].ID, ids[0])
	}

	// reqs-inclusion rule: 3 sessions, only the active/recent one carries
	// non-nil reqs. S3 completed long ago (its lastMs is old, no live).
	k3 := "c33c33c33c33"
	id3 := r.start("b1", "/v1/chat/completions", true, k3, 10, 5)
	r.finish(id3, 200, 100, 50, 200)
	// Backdate S3 well past the 60s reqs window.
	r.mu.Lock()
	r.sessions[k3].LastMs = time.Now().Add(-10 * time.Minute).UnixMilli()
	r.mu.Unlock()

	k1 := "c11c11c11c11"
	id1 := r.start("b1", "/v1/chat/completions", true, k1, 10, 5)
	r.finish(id1, 200, 100, 50, 200) // recent → reqs included
	time.Sleep(10 * time.Millisecond)
	k2 := "c22c22c22c22"
	id2 := r.start("b1", "/v1/chat/completions", true, k2, 10, 5) // stays live
	_ = id2
	id2b := r.start("b1", "/v1/chat/completions", true, k2, 10, 5)
	// S2 has a completed req (recent lastMs, newer than S1) AND a live
	// request → active and most recent → sorts first.
	r.finish(id2b, 200, 100, 50, 200)

	f = r.feed()
	s1 := sessById(t, f, k1)
	s2 := sessById(t, f, k2)
	s3 := sessById(t, f, k3)
	if s1.Reqs == nil {
		t.Errorf("s1 reqs = nil, want non-nil (recent, within 60s)")
	}
	if s2.Reqs == nil {
		t.Errorf("s2 reqs = nil, want non-nil (live request present)")
	}
	if s3.Reqs != nil {
		t.Errorf("s3 reqs = %+v, want nil (lastMs older than 60s window)", s3.Reqs)
	}
	// Sort: active (s2, liveN>0) first, then by lastMs desc.
	if f.Sessions[0].ID != k2 {
		t.Errorf("sessions[0] = %s, want %s (active first)", f.Sessions[0].ID, k2)
	}
	if f.Sessions[1].ID != k1 {
		t.Errorf("sessions[1] = %s, want %s (most recent lastMs)", f.Sessions[1].ID, k1)
	}
	if f.Sessions[2].ID != k3 {
		t.Errorf("sessions[2] = %s, want %s (least recent)", f.Sessions[2].ID, k3)
	}
	if !s2.Active || !s1.Active || s3.Active {
		t.Errorf("active flags: s2=%v s1=%v s3=%v, want true/true/false", s2.Active, s1.Active, s3.Active)
	}
	if s2.LiveN != 1 {
		t.Errorf("s2 liveN = %d, want 1 (id2 still in-flight)", s2.LiveN)
	}
	// Live stack: 105 + id2 (id3/id1/id2b all finished). Ordering ASC.
	if len(f.Live) != 106 {
		t.Fatalf("live = %d, want 106", len(f.Live))
	}
	for i := 1; i < len(f.Live); i++ {
		if f.Live[i].StartMs < f.Live[i-1].StartMs {
			t.Fatalf("live not startMs ASC at %d", i)
		}
	}
}

func TestSessions_FeedJSON(t *testing.T) {
	r := newSessionRegistry()
	saveSessions := sessions
	sessions = r
	t.Cleanup(func() { sessions = saveSessions })

	srv := httptest.NewServer(buildMux())
	defer srv.Close()

	// A chat request (12-hex session) and a non-chat request (sess "").
	a := &parsedBody{Messages: []rawMessage{{Content: json.RawMessage(`"ping"`)}}}
	ka := sessionKeyFor(a)
	idA := sessions.start("b1", "/v1/chat/completions", true, ka, 200, 50)
	idB := sessions.start("b1", "/v1/completions", false, "", 10, 5)
	_ = idB
	time.Sleep(10 * time.Millisecond)
	sessions.firstByte(idA)

	resp, err := http.Get(srv.URL + "/metrics/sessions")
	if err != nil {
		t.Fatalf("sessions request: %v", err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var out struct {
		T    int64 `json:"t"`
		Live []struct {
			ID      uint64 `json:"id"`
			Sess    string `json:"sess"`
			Backend string `json:"backend"`
			Path    string `json:"path"`
			Stream  bool   `json:"stream"`
			Phase   string `json:"phase"`
			StartMs int64  `json:"startMs"`
			CtxTok  int    `json:"ctxTok"`
			NewTok  int    `json:"newTok"`
		} `json:"live"`
		Sessions []struct {
			ID          string  `json:"id"`
			Backend     string  `json:"backend"`
			N           int     `json:"n"`
			FirstMs     int64   `json:"firstMs"`
			LastMs      int64   `json:"lastMs"`
			Active      bool    `json:"active"`
			LiveN       int     `json:"liveN"`
			CtxTok      int     `json:"ctxTok"`
			NewTok      int     `json:"newTok"`
			CachedTok   int     `json:"cachedTok"`
			AvgTTFTMs   float64 `json:"avgTTFTMs"`
			TotalDurS   float64 `json:"totalDurS"`
			TokOutTotal int64   `json:"tokOutTotal"`
			LastStatus  int     `json:"lastStatus"`
			Reqs        []struct {
				TMs       int64   `json:"tMs"`
				Status    int     `json:"status"`
				Stream    bool    `json:"stream"`
				DurMs     float64 `json:"durMs"`
				TTFTms    float64 `json:"ttftMs"`
				CtxTok    int     `json:"ctxTok"`
				NewTok    int     `json:"newTok"`
				CachedTok int     `json:"cachedTok"`
				TokOut    int     `json:"tokOut"`
				TokS      float64 `json:"tokS"`
			} `json:"reqs"`
		} `json:"sessions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("sessions decode: %v", err)
	}
	if d := time.Now().UnixMilli() - out.T; d < 0 || d > 5000 {
		t.Errorf("t = %d, want near now (%d)", out.T, time.Now().UnixMilli())
	}
	if len(out.Live) != 2 {
		t.Fatalf("live = %d, want 2", len(out.Live))
	}
	// live sorted startMs ASC.
	if out.Live[0].StartMs > out.Live[1].StartMs {
		t.Errorf("live order = %d then %d, want ASC", out.Live[0].StartMs, out.Live[1].StartMs)
	}
	// Chat session: 12-hex id, streaming phase after firstByte, est tokens.
	chatIdx, nonChatIdx := -1, -1
	for i := range out.Live {
		switch out.Live[i].Sess {
		case ka:
			chatIdx = i
		case "":
			nonChatIdx = i
		}
	}
	if chatIdx < 0 || nonChatIdx < 0 {
		t.Fatalf("live entries missing chat/non-chat; got %+v", out.Live)
	}
	chat, nonChat := out.Live[chatIdx], out.Live[nonChatIdx]
	if !hex12.MatchString(chat.Sess) {
		t.Errorf("chat sess = %q, want 12-hex", chat.Sess)
	}
	if chat.Phase != "streaming" {
		t.Errorf("chat phase = %q, want streaming (firstByte fired)", chat.Phase)
	}
	if chat.CtxTok != 200 {
		t.Errorf("chat ctxTok = %d, want 200", chat.CtxTok)
	}
	if nonChat.Sess != "" {
		t.Errorf("non-chat sess = %q, want empty", nonChat.Sess)
	}
	// sessions: only the chat one (non-chat never creates a session).
	if len(out.Sessions) != 1 {
		t.Fatalf("sessions = %d, want 1 (non-chat excluded)", len(out.Sessions))
	}
	sj := out.Sessions[0]
	if sj.ID != ka || !sj.Active || sj.LiveN != 1 {
		t.Errorf("session = %+v, want id=%s active liveN=1", sj, ka)
	}
	if sj.CtxTok != 200 || sj.NewTok != 50 || sj.CachedTok != 150 {
		t.Errorf("session tokens = %d/%d/%d, want 200/50/150", sj.CtxTok, sj.NewTok, sj.CachedTok)
	}
	// A live session has no completed reqs yet: reqs is an empty slice (the
	// session is active+recent+top20) — but N=0 means nothing was appended,
	// so the ring is empty and the JSON is [] not null.
	if sj.Reqs == nil {
		t.Errorf("session reqs = null, want [] (active+recent, no completions yet)")
	}
}
