package web

import (
	"crypto/rand"
	"embed"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"text/template"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mariogutierrez/context-harness-mcp/internal/store"
)

//go:embed static/dashboard.html
var dashboardStaticFS embed.FS

const (
	csrfCookieName = "ch_csrf"
	csrfTokenLen   = 32 // 32 random bytes → 64-char hex string
)

// dashboardTemplateData holds the values substituted into dashboard.html.
type dashboardTemplateData struct {
	Email          string
	CSRFToken      string
	GeneratedToken string // empty in PR-1 (generate-token is 501)
}

// dashboardHandler handles GET /dashboard and POST /dashboard/generate-token.
type dashboardHandler struct {
	tmpl *template.Template
	pool *pgxpool.Pool
}

// newDashboardHandler parses the embedded dashboard.html template once at boot.
func newDashboardHandler(pool *pgxpool.Pool) *dashboardHandler {
	tmpl := template.Must(
		template.New("dashboard.html").ParseFS(dashboardStaticFS, "static/dashboard.html"),
	)
	return &dashboardHandler{tmpl: tmpl, pool: pool}
}

// ServeHTTP dispatches GET and POST /dashboard/* to the appropriate sub-handler.
func (h *dashboardHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/dashboard":
		h.handleGet(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/dashboard/generate-token":
		h.handleGenerateToken(w, r)
	default:
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
	}
}

// handleGet serves GET /dashboard. Auth-gated: redirects to /auth/login on
// missing/invalid session. On a valid session it reads the user's email from
// the DB, ensures a ch_csrf cookie is present, and renders dashboard.html.
func (h *dashboardHandler) handleGet(w http.ResponseWriter, r *http.Request) {
	sess, err := Read(r)
	if err != nil {
		http.Redirect(w, r, "/auth/login?next=/dashboard", http.StatusFound)
		return
	}

	email, err := h.lookupEmail(r, sess.Sub)
	if err != nil {
		slog.Error("dashboard: user lookup failed", "error", err, "sub_prefix", subPrefix(sess.Sub))
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	csrfToken, err := ensureCSRFCookie(w, r)
	if err != nil {
		slog.Error("dashboard: csrf cookie error", "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.tmpl.ExecuteTemplate(w, "dashboard.html", dashboardTemplateData{
		Email:     email,
		CSRFToken: csrfToken,
	}); err != nil {
		slog.Error("dashboard: template render failed", "error", err)
	}
}

// handleGenerateToken handles POST /dashboard/generate-token.
// In PR-1 this returns 501 Not Implemented — the real implementation lands in PR-2.
// The form is rendered in dashboard.html so the button is visible but non-functional
// during the coexistence window (token issuance continues via the legacy callback path).
func (h *dashboardHandler) handleGenerateToken(w http.ResponseWriter, r *http.Request) {
	// Session check still runs so an unauthenticated POST gets a clean redirect.
	if _, err := Read(r); err != nil {
		http.Redirect(w, r, "/auth/login", http.StatusFound)
		return
	}
	http.Error(w, "Not Implemented", http.StatusNotImplemented)
}

// lookupEmail returns the email address for the given Supabase user UUID.
// Returns "unknown" when the user row is not found (race between sign-in and
// the row being written) so the page still renders rather than erroring.
func (h *dashboardHandler) lookupEmail(r *http.Request, sub string) (string, error) {
	u, err := store.GetUserByID(r.Context(), h.pool, sub)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "unknown", nil
		}
		return "", err
	}
	return u.Email, nil
}

// ensureCSRFCookie reads the ch_csrf cookie from the request. If absent or
// empty, it generates a fresh 64-char hex token, sets the cookie on the
// response, and returns the token value. If the cookie already exists the
// existing value is returned unchanged.
func ensureCSRFCookie(w http.ResponseWriter, r *http.Request) (string, error) {
	if c, err := r.Cookie(csrfCookieName); err == nil && c.Value != "" {
		return c.Value, nil
	}

	raw := make([]byte, csrfTokenLen)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	token := hex.EncodeToString(raw)

	http.SetCookie(w, &http.Cookie{
		Name:     csrfCookieName,
		Value:    token,
		Path:     "/",
		MaxAge:   sessionMaxAge,
		HttpOnly: false, // must be readable from the template for double-submit pattern
		Secure:   isHTTPS(r),
		SameSite: http.SameSiteStrictMode,
	})
	return token, nil
}

// clearCSRFCookie expires the ch_csrf cookie.
func clearCSRFCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     csrfCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   0,
		HttpOnly: false,
		Secure:   isHTTPS(r),
		SameSite: http.SameSiteStrictMode,
	})
}

// RegisterDashboard registers GET /dashboard and POST /dashboard/generate-token
// on the given mux. pool must be non-nil — the handler looks up user email from
// the DB on every authenticated GET.
func RegisterDashboard(mux *http.ServeMux, pool *pgxpool.Pool) {
	h := newDashboardHandler(pool)
	mux.Handle("/dashboard", h)
	mux.Handle("/dashboard/generate-token", h)
}
