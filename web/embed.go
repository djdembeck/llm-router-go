//go:build webui

// Package web exposes the embedded SvelteKit static build (web/build) as an
// http.Handler. Build the real SPA with `cd web && bun run build`. The `webui`
// build tag is set by the Docker image and CI release builds so the compiled
// binary serves the dashboard; without the tag, embed_stub.go provides a 404
// fallback so plain `go build ./...`, `go vet`, and `go test` work in CI
// without the frontend build present.
package web

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed all:build
var FS embed.FS

// Handler returns an HTTP handler for the embedded SPA: real static files
// (_app, assets, ...) served directly, index.html as the SPA fallback for any
// other HTML-accepting path, and a hard 404 for the router's management and
// proxied routes that must never fall back to the shell.
func Handler() http.Handler {
	sub, err := fs.Sub(FS, "build")
	if err != nil {
		// Unreachable in practice: the embed guarantees build exists.
		panic(err)
	}
	files := http.FileServer(http.FS(sub))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path

		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")

		// The router's management and proxied routes are registered on the
		// parent mux; anything that reaches here is a miss and must get a
		// real 404, never the SPA shell.
		if p == "/stats" || p == "/health" ||
			strings.HasPrefix(p, "/metrics/") ||
			strings.HasPrefix(p, "/v1/") {
			http.NotFound(w, r)
			return
		}

		// Serve real static files (/_app/..., /assets/..., etc.) directly.
		if f, err := sub.Open(strings.TrimPrefix(p, "/")); err == nil {
			if info, ierr := f.Stat(); ierr == nil && !info.IsDir() {
				f.Close()
				files.ServeHTTP(w, r)
				return
			}
			f.Close()
		}

		// SPA fallback only for navigation-style requests. Paths that look
		// like static files (dot in the basename) and requests that do not
		// accept HTML must not receive the HTML shell: a browser that gets
		// HTML where a font/script is expected reports sanitizer/decode
		// errors instead of a clean 404.
		if !isAssetPath(p) && acceptsHTML(r) {
			raw, err := fs.ReadFile(sub, "index.html")
			if err != nil {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Write(raw)
			return
		}
		http.NotFound(w, r)
	})
}

// isAssetPath reports whether the request path names a file rather than an SPA
// route, by checking for a dot in the basename (e.g. /assets/x.woff2).
func isAssetPath(p string) bool {
	return strings.Contains(path.Base(p), ".")
}

// acceptsHTML reports whether the request expects an HTML document. Empty
// Accept and */* mean the client has no strong preference, which the shell
// can answer; asset fetches send narrow Accept values (font/, text/css).
func acceptsHTML(r *http.Request) bool {
	accept := r.Header.Get("Accept")
	return accept == "" ||
		strings.Contains(accept, "text/html") ||
		strings.Contains(accept, "*/*")
}
