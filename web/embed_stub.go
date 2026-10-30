//go:build !webui

package web

import "net/http"

// Handler returns a 404 handler used when the real frontend build is absent
// (plain `go build ./...`, CI lint/build jobs, unit tests). The Docker image
// and release builds compile with `-tags webui` to embed the real SPA.
func Handler() http.Handler {
	return http.NotFoundHandler()
}
