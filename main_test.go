package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
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
