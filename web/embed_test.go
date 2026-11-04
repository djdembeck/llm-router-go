//go:build webui

package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

// fixtureFS is a self-contained stand-in for web/build: the test must run
// without a frontend build present (CI builds the package with -tags
// webui but may not have bun run build output).
var fixtureFS = fstest.MapFS{
	"index.html":          &fstest.MapFile{Data: []byte("<!doctype html><title>llm-router-go</title>")},
	"_app/version.json":   &fstest.MapFile{Data: []byte(`{"version":"test"}`)},
	"assets/index.abc.js": &fstest.MapFile{Data: []byte("console.log(1)")},
}

func TestHandlerFor_SPA(t *testing.T) {
	h := handlerFor(fixtureFS)
	tests := []struct {
		name     string
		path     string
		accept   string
		wantCode int
		wantSub  string
	}{
		{"v1 chat hard 404", "/v1/chat/completions", "", 404, ""},
		{"v1 models hard 404", "/v1/models", "", 404, ""},
		{"metrics stream hard 404", "/metrics/stream", "", 404, ""},
		{"stats hard 404", "/stats", "", 404, ""},
		{"health hard 404", "/health", "", 404, ""},
		{"asset json served", "/_app/version.json", "", 200, `"version":"test"`},
		{"asset js served", "/assets/index.abc.js", "", 200, "console.log(1)"},
		{"unknown navigation serves shell", "/some/nav", "", 200, "<title>llm-router-go</title>"},
		{"root serves shell", "/", "", 200, "<title>llm-router-go</title>"},
		{"missing asset 404", "/assets/missing.xyz", "", 404, ""},
		{"asset-looking path never serves shell", "/some/nav", "application/json", 404, ""},
		{"star accept serves shell", "/some/nav", "*/*", 200, "<title>llm-router-go</title>"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			if tt.accept != "" {
				req.Header.Set("Accept", tt.accept)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tt.wantCode {
				t.Fatalf("GET %s (Accept: %q) = %d, want %d", tt.path, tt.accept, rec.Code, tt.wantCode)
			}
			if tt.wantSub != "" && !strings.Contains(rec.Body.String(), tt.wantSub) {
				t.Errorf("GET %s body = %q, want substring %q", tt.path, rec.Body.String(), tt.wantSub)
			}
			if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
				t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
			}
		})
	}
}
