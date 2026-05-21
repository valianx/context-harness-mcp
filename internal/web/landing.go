// Package web implements the unauthenticated HTTP endpoints for the Phase 0
// auth flow and the product landing page.
package web

import (
	"embed"
	"log/slog"
	"net/http"
	"text/template"
)

//go:embed static/landing.html
var landingStaticFS embed.FS

// landingTemplateData holds the values substituted into landing.html at
// render time. Authenticated is true when the request carries a valid
// ch_session cookie so the template can show "Dashboard →" instead of "Sign in →".
type landingTemplateData struct {
	Authenticated bool
}

// landingHandler serves GET / by rendering the embedded landing.html template
// with session-awareness: authenticated users see a "Dashboard →" CTA.
//
// Distinct from the callback/login handlers because:
//   - Path is the root "/" — matches every unhandled request, so we must
//     explicitly reject any path that isn't exactly "/" to avoid hijacking
//     legitimate 404s (e.g., a typo'd /mcp call should still 404, not land).
type landingHandler struct {
	tmpl *template.Template
}

// newLandingHandler parses the embedded landing.html template once at boot.
func newLandingHandler() *landingHandler {
	tmpl := template.Must(
		template.New("landing.html").ParseFS(landingStaticFS, "static/landing.html"),
	)
	return &landingHandler{tmpl: tmpl}
}

// ServeHTTP responds to GET / with the rendered landing HTML.
// All other paths under "/" fall through to 404 NotFound so that
// mistyped routes (e.g. "/foo") don't render the landing.
// Non-GET methods receive 405 Method Not Allowed.
func (h *landingHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	_, authenticated := Read(r)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store") // per-request auth check — no caching
	if err := h.tmpl.ExecuteTemplate(w, "landing.html", landingTemplateData{
		Authenticated: authenticated == nil,
	}); err != nil {
		slog.Error("web: landing template render failed", "error", err)
	}
}

// RegisterLanding registers the landing page handler at GET /.
// Should be wired in cmd/server/main.go alongside the other unauthenticated
// routes (callback, login, exchange, webhook) — i.e., outside the
// auth.Middleware wrap on /mcp.
func RegisterLanding(mux *http.ServeMux) {
	h := newLandingHandler()
	mux.Handle("/", h)
}
