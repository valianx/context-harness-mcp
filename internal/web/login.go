package web

import (
	"embed"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"text/template"
)

//go:embed static/login.html
var loginStaticFS embed.FS

// loginTemplateData holds the server-rendered values substituted into
// login.html at boot time.
type loginTemplateData struct {
	SupabaseProjectURL string
	SupabaseAnonKey    string
	MCPPublicURL       string
	// Next is the validated redirect target passed through from the ?next= query
	// param. The login page's JS embeds this in the Supabase redirect_to URL so
	// the callback page can thread it through to /auth/exchange.
	// Empty string when the next param is absent or fails IsSafe.
	Next string
}

// loginHandler serves GET /auth/login by rendering the embedded login.html
// template with env-sourced substitutions.
type loginHandler struct {
	tmpl *template.Template
	data loginTemplateData
}

// newLoginHandler parses the embedded login.html template once at boot and
// captures the env vars needed for substitution.
func newLoginHandler() *loginHandler {
	tmpl := template.Must(
		template.New("login.html").ParseFS(loginStaticFS, "static/login.html"),
	)
	return &loginHandler{
		tmpl: tmpl,
		data: loginTemplateData{
			SupabaseProjectURL: os.Getenv("SUPABASE_PROJECT_URL"),
			SupabaseAnonKey:    os.Getenv("SUPABASE_ANON_KEY"),
			MCPPublicURL:       os.Getenv("MCP_PUBLIC_URL"),
		},
	}
}

// ServeHTTP responds to GET /auth/login with the rendered login HTML.
// Non-GET methods receive 405 Method Not Allowed.
//
// PR-1: reads ?next= from the query string, validates it via IsSafe, and passes
// it to the template as {{.Next}}.
// PR-2: if a valid ch_session is already present, 302-redirects to next (when
// safe) or /dashboard so the user bypasses the login form (AC-8).
func (h *loginHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	// PR-2: already-authenticated users are forwarded directly to the dashboard
	// (or to their intended destination when ?next= is valid).
	if _, err := Read(r); err == nil {
		dest := "/dashboard"
		if n := safeNext(r); n != "" {
			dest = n
		}
		http.Redirect(w, r, dest, http.StatusFound)
		return
	}

	// Build per-request template data. Base data (project URL, anon key,
	// public URL) is fixed at boot; Next varies per request.
	data := loginTemplateData{
		SupabaseProjectURL: h.data.SupabaseProjectURL,
		SupabaseAnonKey:    h.data.SupabaseAnonKey,
		MCPPublicURL:       h.data.MCPPublicURL,
		Next:               safeNext(r),
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.tmpl.ExecuteTemplate(w, "login.html", data); err != nil {
		slog.Error("web: login template render failed", "error", err)
	}
}

// safeNext reads ?next= from the request query string and returns it only if
// IsSafe passes. Returns an empty string for missing or unsafe values.
func safeNext(r *http.Request) string {
	next := r.URL.Query().Get("next")
	if IsSafe(next) {
		return next
	}
	return ""
}

// supabaseRedirectTo builds the redirect_to URL sent to Supabase when issuing a
// magic link. When next is set, it is appended so the callback page can
// thread it through to /auth/exchange.
//
//	MCP_PUBLIC_URL + "/auth/callback"              (no next)
//	MCP_PUBLIC_URL + "/auth/callback?next=" + enc  (valid next)
func supabaseRedirectTo(mcpPublicURL, next string) string {
	base := mcpPublicURL + "/auth/callback"
	if next != "" {
		base += "?next=" + url.QueryEscape(next)
	}
	return base
}

// RegisterLogin registers GET /auth/login on the given mux.
// Call this before wrapping /mcp with auth.Middleware so the login page is
// reachable without a bearer token.
func RegisterLogin(mux *http.ServeMux) {
	h := newLoginHandler()
	mux.Handle("/auth/login", h)
}
