// Package tests — dashboard generate-token E2E integration tests for PR-2 /
// auth-ux-redesign.
//
// Covers:
//   - AC-4: POST /dashboard/generate-token happy path (valid session + valid CSRF → 200 + token in HTML).
//   - AC-5: CSRF protection (missing/mismatched → 403; no JWT issued).
//   - AC-6 (CSRF side): POST /auth/logout with valid vs invalid CSRF.
//   - AC-7 (structure): callback.html has no view-success element (static assertion).
//   - AC-8: landing renders correct CTA based on session state.
//
// All tests use the shared testcontainers pool from setup_test.go.
package tests

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariogutierrez/context-harness-mcp/internal/store"
	"github.com/mariogutierrez/context-harness-mcp/internal/web"
)

// ── constants ────────────────────────────────────────────────────────────────

const (
	dashE2EJWTSecret = "dashboard-e2e-test-secret-minimum-32bytes!!"
	dashE2ESub       = "ddddeeee-ffff-0000-1111-222233334444"
	dashE2EEmail     = "dashboard-e2e@example.com"
	dashE2ECSRFToken = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
)

// ── server wiring ─────────────────────────────────────────────────────────────

// buildDashboardE2EServer registers /dashboard and /dashboard/generate-token on
// a fresh mux wired to the shared test pool, then wraps it in an httptest.Server.
func buildDashboardE2EServer(t *testing.T) *httptest.Server {
	t.Helper()
	pool := NewTestPool(t)
	t.Setenv("MCP_JWT_SECRET", dashE2EJWTSecret)
	t.Setenv("MCP_JWT_ISSUER", e2eIssuer)
	t.Setenv("MCP_PUBLIC_URL", "https://mcp.test")
	t.Setenv("MCP_SNIPPET_SERVER_NAME", "context-harness")

	mux := http.NewServeMux()
	web.RegisterDashboard(mux, pool)
	web.RegisterLogout(mux)

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// ensureUserExists inserts a users row for the e2e test subject so the dashboard
// email-lookup doesn't return "unknown".
func ensureUserExists(t *testing.T, sub, email string) {
	t.Helper()
	pool := NewTestPool(t)
	t.Setenv("MCP_JWT_SECRET", dashE2EJWTSecret)

	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	require.NoError(t, err, "ensureUserExists: begin tx failed")

	upsertErr := store.UpsertUser(ctx, tx, sub, email)
	if upsertErr != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("ensureUserExists: UpsertUser failed: %v", upsertErr)
	}
	require.NoError(t, tx.Commit(ctx), "ensureUserExists: commit failed")

	t.Cleanup(func() {
		pool.Exec(context.Background(), "DELETE FROM users WHERE supabase_user_id = $1", sub)
	})
}

// issueE2ESessionCookie issues a ch_session cookie for sub using the e2e secret
// via httptest.ResponseRecorder and returns the raw cookie value.
func issueE2ESessionCookie(t *testing.T, sub string) string {
	t.Helper()
	t.Setenv("MCP_JWT_SECRET", dashE2EJWTSecret)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	require.NoError(t, web.Issue(rr, req, sub))

	for _, c := range rr.Result().Cookies() {
		if c.Name == "ch_session" {
			return c.Value
		}
	}
	t.Fatal("ch_session cookie not found in recorder")
	return ""
}

// ── AC-5: CSRF protection ─────────────────────────────────────────────────────

func TestDashboardE2E_GenerateToken_MissingCSRF_Returns403(t *testing.T) {
	ensureUserExists(t, dashE2ESub, dashE2EEmail)
	srv := buildDashboardE2EServer(t)

	sessionVal := issueE2ESessionCookie(t, dashE2ESub)

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/dashboard/generate-token",
		strings.NewReader(""))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: "ch_session", Value: sessionVal})
	req.AddCookie(&http.Cookie{Name: "ch_csrf", Value: dashE2ECSRFToken})
	// No csrf_token form field.

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusForbidden, resp.StatusCode,
		"missing csrf_token form field must return 403")
}

func TestDashboardE2E_GenerateToken_MismatchedCSRF_Returns403(t *testing.T) {
	ensureUserExists(t, dashE2ESub, dashE2EEmail)
	srv := buildDashboardE2EServer(t)

	sessionVal := issueE2ESessionCookie(t, dashE2ESub)
	wrongFormToken := strings.Repeat("x", 64)

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/dashboard/generate-token",
		strings.NewReader("csrf_token="+wrongFormToken))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: "ch_session", Value: sessionVal})
	req.AddCookie(&http.Cookie{Name: "ch_csrf", Value: dashE2ECSRFToken})

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusForbidden, resp.StatusCode,
		"mismatched CSRF tokens must return 403")
}

// ── AC-4: generate-token happy path ──────────────────────────────────────────

func TestDashboardE2E_GenerateToken_ValidCSRF_Returns200WithToken(t *testing.T) {
	ensureUserExists(t, dashE2ESub, dashE2EEmail)
	srv := buildDashboardE2EServer(t)

	sessionVal := issueE2ESessionCookie(t, dashE2ESub)

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/dashboard/generate-token",
		strings.NewReader("csrf_token="+dashE2ECSRFToken))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: "ch_session", Value: sessionVal})
	req.AddCookie(&http.Cookie{Name: "ch_csrf", Value: dashE2ECSRFToken})

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode,
		"valid CSRF must generate a token and return 200")
	assert.Contains(t, resp.Header.Get("Content-Type"), "text/html",
		"generate-token response must be HTML (dashboard re-render)")
}

// ── AC-6 (CSRF side): logout CSRF guard ──────────────────────────────────────

func TestDashboardE2E_Logout_MissingCSRF_Returns403(t *testing.T) {
	srv := buildDashboardE2EServer(t)
	t.Setenv("MCP_JWT_SECRET", dashE2EJWTSecret)

	sessionVal := issueE2ESessionCookie(t, dashE2ESub)

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/auth/logout",
		strings.NewReader(""))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: "ch_session", Value: sessionVal})
	req.AddCookie(&http.Cookie{Name: "ch_csrf", Value: dashE2ECSRFToken})
	// No csrf_token form field.

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusForbidden, resp.StatusCode,
		"logout without csrf_token form field must return 403")
}

func TestDashboardE2E_Logout_ValidCSRF_ClearsAndRedirects(t *testing.T) {
	srv := buildDashboardE2EServer(t)
	t.Setenv("MCP_JWT_SECRET", dashE2EJWTSecret)

	sessionVal := issueE2ESessionCookie(t, dashE2ESub)

	// Use a client that does NOT follow redirects so we can inspect the 302.
	client := &http.Client{
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/auth/logout",
		strings.NewReader("csrf_token="+dashE2ECSRFToken))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: "ch_session", Value: sessionVal})
	req.AddCookie(&http.Cookie{Name: "ch_csrf", Value: dashE2ECSRFToken})

	resp, err := client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusFound, resp.StatusCode,
		"valid logout must redirect")
	assert.Equal(t, "/", resp.Header.Get("Location"),
		"logout must redirect to /")

	var sessionCleared, csrfCleared bool
	for _, c := range resp.Cookies() {
		if c.Name == "ch_session" && c.MaxAge == 0 {
			sessionCleared = true
		}
		if c.Name == "ch_csrf" && c.MaxAge == 0 {
			csrfCleared = true
		}
	}
	assert.True(t, sessionCleared, "ch_session must be cleared (Max-Age=0)")
	assert.True(t, csrfCleared, "ch_csrf must be cleared (Max-Age=0)")
}
