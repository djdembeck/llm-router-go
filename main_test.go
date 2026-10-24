package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
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
// estimatePrefillTokens
// ---------------------------------------------------------------------------

func TestEstimatePrefillTokens(t *testing.T) {
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
			name: "chat API three messages counts all",
			body: &parsedBody{
				Messages: []rawMessage{
					{Content: json.RawMessage(`"first"`)},
					{Content: json.RawMessage(`"second"`)},
					{Content: json.RawMessage(`"last"`)},
				},
			},
			want: (5 + 6 + 4) / 4, // 3
		},
		{
			name: "chat API four messages counts all",
			body: &parsedBody{
				Messages: []rawMessage{
					{Content: json.RawMessage(`"msg1"`)},
					{Content: json.RawMessage(`"msg2"`)},
					{Content: json.RawMessage(`"msg3"`)},
					{Content: json.RawMessage(`"msg4"`)},
				},
			},
			want: (4 + 4 + 4 + 4) / 4, // 4
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
			name: "multi-turn boundary: 3 messages counts all",
			body: &parsedBody{
				Messages: []rawMessage{
					{Content: json.RawMessage(`"aaaabbbbccccddddeeee"`)},
					{Content: json.RawMessage(`"fffFgggGhhhhIIIIjjjj"`)},
					{Content: json.RawMessage(`"last"`)},
				},
			},
			want: (20 + 20 + 4) / 4, // 11
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
			got := estimatePrefillTokens(tt.body)
			if tt.wantLess > 0 {
				if got < tt.wantLess || got > tt.want {
					t.Errorf("estimatePrefillTokens() = %d, expected between %d and %d", got, tt.wantLess, tt.want)
				}
			} else if got != tt.want {
				t.Errorf("estimatePrefillTokens() = %d, want %d", got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Integration: rawContentLen + estimatePrefillTokens interaction
// ---------------------------------------------------------------------------

func TestEstimatePrefillTokens_CountsFullConversation(t *testing.T) {
	// A short final turn must not shrink the estimate: the router has no
	// per-request evidence that the preceding prefix is cached, so the whole
	// conversation counts toward the large-prefill gate.

	bigContent := strings.Repeat("x", 100000)
	smallContent := "hi"

	body := &parsedBody{
		Messages: []rawMessage{
			{Content: json.RawMessage(`"` + bigContent + `"`)},   // 25000 tokens
			{Content: json.RawMessage(`"` + bigContent + `"`)},   // 25000 tokens
			{Content: json.RawMessage(`"` + smallContent + `"`)}, // 0 tokens after floor division
		},
	}

	got := estimatePrefillTokens(body)
	want := (100000 + 100000 + 2) / 4 // 50000

	if got != want {
		t.Errorf("multi-turn conversation: got %d tokens, want %d (all messages should count)", got, want)
	}

	// The same context as two messages must produce the same estimate.
	body2 := &parsedBody{
		Messages: []rawMessage{
			{Content: json.RawMessage(`"` + bigContent + `"`)},
			{Content: json.RawMessage(`"` + bigContent + `"`)},
		},
	}

	got2 := estimatePrefillTokens(body2)
	want2 := (100000 + 100000) / 4 // 50000

	if got2 != want2 {
		t.Errorf("two-message case: got %d tokens, want %d", got2, want2)
	}
}

// ---------------------------------------------------------------------------
// Large-prefill admission
// ---------------------------------------------------------------------------

// TestLargePrefillGateCountsMultiTurnPrefix holds one streaming request open
// and verifies that a second request with the same long context but a short
// final turn still waits for the single large-prefill slot.
func TestLargePrefillGateCountsMultiTurnPrefix(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseAll := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseAll()

	var reached atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		reached.Add(1)
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	origBackends, origSlots, origPrefillSlots := backends, slots, prefillSlots
	origDurations, origProxies := durations, proxies
	defer func() {
		backends, slots, prefillSlots = origBackends, origSlots, origPrefillSlots
		durations, proxies = origDurations, origProxies
	}()

	backends = []Backend{{
		Name:                        "probe",
		URL:                         upstream.URL,
		MaxConcurrent:               8,
		MaxQueueDepth:               5,
		MaxConcurrentLargePrefill:   1,
		LargePrefillThresholdTokens: 8192,
	}}
	slots = map[string]*slotManager{"probe": newSlot(8, 5)}
	prefillSlots = map[string]*slotManager{"probe": newSlot(1, 4)}
	durations = map[string]*durationTracker{"probe": {}}

	u, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}
	proxy := httputil.NewSingleHostReverseProxy(u)
	proxy.FlushInterval = -1
	proxy.Transport = proxyTransport
	proxies = map[string]*httputil.ReverseProxy{"probe": proxy}

	long := strings.Repeat("x", 200000)
	body := `{"model":"probe","stream":true,"messages":[` +
		`{"role":"system","content":"system"},` +
		`{"role":"user","content":"` + long + `"},` +
		`{"role":"assistant","content":"ack"},` +
		`{"role":"user","content":"continue"}]}`

	do := func() int {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		handleProxy(rec, req)
		return rec.Code
	}

	statuses := make(chan int, 2)
	go func() { statuses <- do() }()

	deadline := time.Now().Add(5 * time.Second)
	for reached.Load() != 1 {
		if time.Now().After(deadline) {
			releaseAll()
			<-statuses
			t.Fatal("first request did not reach upstream")
		}
		time.Sleep(5 * time.Millisecond)
	}

	go func() { statuses <- do() }()

	// Wait until the second request is either queued at the prefill gate or
	// (on failure) already forwarded.
	deadline = time.Now().Add(2 * time.Second)
	for {
		_, _, waiting, _ := prefillSlots["probe"].snapshot()
		if reached.Load() >= 2 || waiting == 1 || time.Now().After(deadline) {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}

	if got := reached.Load(); got != 1 {
		releaseAll()
		<-statuses
		<-statuses
		t.Fatalf("second request bypassed the large-prefill gate: upstream received %d requests", got)
	}
	if in, _, waiting, _ := prefillSlots["probe"].snapshot(); in != 1 || waiting != 1 {
		releaseAll()
		<-statuses
		<-statuses
		t.Fatalf("prefill gate state = inflight %d waiting %d, want 1/1", in, waiting)
	}

	releaseAll()
	for i := 0; i < 2; i++ {
		if code := <-statuses; code != http.StatusOK {
			t.Errorf("request %d returned status %d, want %d", i+1, code, http.StatusOK)
		}
	}
}
