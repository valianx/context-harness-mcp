// Package web implements the unauthenticated HTTP endpoints for the Phase 0
// auth flow and the product landing page.
package web

import (
	_ "embed"
	"net/http"
)

// landingHTML is the fully static landing page served at GET /. Unlike the
// auth surfaces, it has no Go template variables — all links and copy are
// hardcoded at build time. Served as raw bytes.
//
//go:embed static/landing.html
var landingHTML []byte

// landingHandler serves GET / with the embedded landing page.
//
// Distinct from the callback/login handlers because:
//   - No template substitution needed (no env vars to inject).
//   - Path is the root "/" — matches every unhandled request, so we must
//     explicitly reject any path that isn't exactly "/" to avoid hijacking
//     legitimate 404s (e.g., a typo'd /mcp call should still 404, not land).
type landingHandler struct{}

// ServeHTTP responds to GET / with the embedded landing HTML.
// All other paths under "/" fall through to 404 NotFound so that
// mistyped routes (e.g. "/foo") don't render the landing.
// Non-GET methods receive 405 Method Not Allowed.
func (h landingHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	_, _ = w.Write(landingHTML)
}

// RegisterLanding registers the landing page handler at GET /.
// Should be wired in cmd/server/main.go alongside the other unauthenticated
// routes (callback, login, exchange, webhook) — i.e., outside the
// auth.Middleware wrap on /mcp.
func RegisterLanding(mux *http.ServeMux) {
	mux.Handle("/", landingHandler{})
}
