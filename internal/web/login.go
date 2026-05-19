package web

import (
	"embed"
	"log/slog"
	"net/http"
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
func (h *loginHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.tmpl.ExecuteTemplate(w, "login.html", h.data); err != nil {
		slog.Error("web: login template render failed", "error", err)
	}
}

// RegisterLogin registers GET /auth/login on the given mux.
// Call this before wrapping /mcp with auth.Middleware so the login page is
// reachable without a bearer token.
func RegisterLogin(mux *http.ServeMux) {
	h := newLoginHandler()
	mux.Handle("/auth/login", h)
}
