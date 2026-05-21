package web

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── landing handler tests ─────────────────────────────────────────────────────

// TestLandingHandler_Unauthenticated_ShowsSignIn verifies the landing renders
// "Sign in →" and href="/auth/login" when no session is present (AC-8).
func TestLandingHandler_Unauthenticated_ShowsSignIn(t *testing.T) {
	t.Setenv("MCP_JWT_SECRET", testSessionSecret)

	h := newLandingHandler()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	body := rr.Body.String()
	assert.Contains(t, body, "Sign in →",
		"unauthenticated landing must show 'Sign in →' CTA")
	assert.Contains(t, body, `href="/auth/login"`,
		"unauthenticated landing must link to /auth/login")
	assert.NotContains(t, body, "Dashboard →",
		"unauthenticated landing must not show 'Dashboard →'")
}

// TestLandingHandler_Authenticated_ShowsDashboard verifies the landing renders
// "Dashboard →" and href="/dashboard" when a valid session is present (AC-8).
func TestLandingHandler_Authenticated_ShowsDashboard(t *testing.T) {
	t.Setenv("MCP_JWT_SECRET", testSessionSecret)

	h := newLandingHandler()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(issueSessionCookie(t, testSessionSub))

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	body := rr.Body.String()
	assert.Contains(t, body, "Dashboard →",
		"authenticated landing must show 'Dashboard →' CTA")
	assert.Contains(t, body, `href="/dashboard"`,
		"authenticated landing must link to /dashboard")
	assert.NotContains(t, body, "Sign in →",
		"authenticated landing must not show 'Sign in →'")
}

// TestLandingHandler_Footer_Unauthenticated_ShowsSignIn verifies that the
// footer link also changes based on session state (AC-8).
func TestLandingHandler_Footer_Unauthenticated_ShowsSignIn(t *testing.T) {
	t.Setenv("MCP_JWT_SECRET", testSessionSecret)

	h := newLandingHandler()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	body := rr.Body.String()
	assert.Contains(t, body, "Sign in",
		"footer must contain Sign in link for unauthenticated users")
}

// TestLandingHandler_Footer_Authenticated_ShowsDashboard verifies that the
// footer link shows "Dashboard" when authenticated (AC-8).
func TestLandingHandler_Footer_Authenticated_ShowsDashboard(t *testing.T) {
	t.Setenv("MCP_JWT_SECRET", testSessionSecret)

	h := newLandingHandler()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(issueSessionCookie(t, testSessionSub))

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	body := rr.Body.String()
	assert.Contains(t, body, "Dashboard",
		"footer must contain Dashboard link for authenticated users")
}

// TestLandingHandler_NonRootPath_Returns404 verifies that paths other than "/"
// get 404, not the landing page.
func TestLandingHandler_NonRootPath_Returns404(t *testing.T) {
	h := newLandingHandler()
	req := httptest.NewRequest(http.MethodGet, "/foo", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusNotFound, rr.Code)
}

// TestLandingHandler_RejectsNonGet verifies non-GET methods get 405.
func TestLandingHandler_RejectsNonGet(t *testing.T) {
	h := newLandingHandler()

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		req := httptest.NewRequest(method, "/", nil)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		assert.Equal(t, http.StatusMethodNotAllowed, rr.Code,
			"method %s must be rejected", method)
	}
}

// TestRegisterLanding_RoutesCorrectly verifies the handler is wired to "/".
func TestRegisterLanding_RoutesCorrectly(t *testing.T) {
	t.Setenv("MCP_JWT_SECRET", testSessionSecret)

	mux := http.NewServeMux()
	RegisterLanding(mux)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusOK, rr.Code)
}

// TestLandingHandler_NoCacheControl verifies the response sets no-store since
// the CTA varies per session.
func TestLandingHandler_NoCacheControl(t *testing.T) {
	t.Setenv("MCP_JWT_SECRET", testSessionSecret)

	h := newLandingHandler()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	assert.Contains(t, rr.Header().Get("Cache-Control"), "no-store",
		"landing must not be cached — CTA varies per session")
}
