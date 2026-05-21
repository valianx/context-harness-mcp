package web

import (
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"text/template"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mariogutierrez/context-harness-mcp/internal/auth"
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
	GeneratedToken string // non-empty only after POST /dashboard/generate-token; empty on GET
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
// Flow: session check → CSRF check → issue JWT → re-render dashboard with token.
// The token is never stored server-side; subsequent GETs show the empty state (AC-4).
func (h *dashboardHandler) handleGenerateToken(w http.ResponseWriter, r *http.Request) {
	sess, err := Read(r)
	if err != nil {
		http.Redirect(w, r, "/auth/login", http.StatusFound)
		return
	}

	if !csrfValid(r) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}

	email, err := h.lookupEmail(r, sess.Sub)
	if err != nil {
		slog.Error("dashboard: user lookup failed on generate-token", "error", err, "sub_prefix", subPrefix(sess.Sub))
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	token, err := issueToken(sess.Sub, email)
	if err != nil {
		slog.Error("dashboard: IssueMCPToken failed", "error", err, "sub_prefix", subPrefix(sess.Sub))
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	// The CSRF token in the cookie is still valid — pass it back so the
	// re-rendered forms (generate-token and sign-out) remain functional.
	csrfToken, err := ensureCSRFCookie(w, r)
	if err != nil {
		slog.Error("dashboard: csrf cookie error on generate-token", "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.tmpl.ExecuteTemplate(w, "dashboard.html", dashboardTemplateData{
		Email:          email,
		CSRFToken:      csrfToken,
		GeneratedToken: token,
	}); err != nil {
		slog.Error("dashboard: template render failed after token gen", "error", err)
	}
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

// csrfValid implements the double-submit cookie check: both the ch_csrf cookie
// and the csrf_token form field must be present and equal (constant-time compare).
// Identical logic lives in logoutHandler.csrfValid — kept co-located for reviewability.
func csrfValid(r *http.Request) bool {
	cookie, err := r.Cookie(csrfCookieName)
	if err != nil || cookie.Value == "" {
		return false
	}
	formToken := r.FormValue("csrf_token")
	if formToken == "" {
		return false
	}
	// subtle.ConstantTimeCompare returns 1 on equal, 0 on not-equal.
	return subtle.ConstantTimeCompare([]byte(formToken), []byte(cookie.Value)) == 1
}

// issueToken wraps auth.IssueMCPToken so tests can verify the call in isolation.
// Production wiring always calls the real issuer; the function-level indirection
// keeps handleGenerateToken testable without exposing a field on dashboardHandler.
func issueToken(sub, email string) (string, error) {
	return auth.IssueMCPToken(sub, email)
}

// RegisterDashboard registers GET /dashboard and POST /dashboard/generate-token
// on the given mux. pool must be non-nil — the handler looks up user email from
// the DB on every authenticated GET.
func RegisterDashboard(mux *http.ServeMux, pool *pgxpool.Pool) {
	h := newDashboardHandler(pool)
	mux.Handle("/dashboard", h)
	mux.Handle("/dashboard/generate-token", h)
}
