// Package web implements the unauthenticated HTTP endpoints for the Phase 0
// auth flow: /auth/callback, /auth/exchange, and /auth/login.
// All three routes bypass auth.Middleware — they are public entry points in
// the login flow and registered directly on the mux in cmd/server/main.go.
package web

import (
	"embed"
	"log/slog"
	"net/http"
	"os"
	"text/template"
)

//go:embed static/callback.html
var callbackStaticFS embed.FS

// callbackTemplateData holds the server-rendered values substituted into
// callback.html at boot time. Fields are exported for html/template access.
type callbackTemplateData struct {
	SupabaseProjectURL string
	SupabaseAnonKey    string
	MCPPublicURL       string
}

// callbackHandler serves GET /auth/callback by rendering the embedded
// callback.html template with env-sourced values substituted.
type callbackHandler struct {
	tmpl *template.Template
	data callbackTemplateData
}

// newCallbackHandler parses the embedded callback.html template once at boot
// and captures the env vars needed for substitution. This is safe to call
// multiple times (e.g., in tests) — each call returns an independent handler.
func newCallbackHandler() *callbackHandler {
	tmpl := template.Must(
		template.New("callback.html").ParseFS(callbackStaticFS, "static/callback.html"),
	)
	return &callbackHandler{
		tmpl: tmpl,
		data: callbackTemplateData{
			SupabaseProjectURL: os.Getenv("SUPABASE_PROJECT_URL"),
			SupabaseAnonKey:    os.Getenv("SUPABASE_ANON_KEY"),
			MCPPublicURL:       os.Getenv("MCP_PUBLIC_URL"),
		},
	}
}

// ServeHTTP responds to GET /auth/callback with the rendered callback HTML.
// Non-GET methods receive 405 Method Not Allowed.
func (h *callbackHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.tmpl.ExecuteTemplate(w, "callback.html", h.data); err != nil {
		slog.Error("web: callback template render failed", "error", err)
	}
}

// RegisterCallback registers GET /auth/callback on the given mux.
// Call this before wrapping /mcp with auth.Middleware so the callback route
// is reachable without a bearer token.
func RegisterCallback(mux *http.ServeMux) {
	h := newCallbackHandler()
	mux.Handle("/auth/callback", h)
}
