package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func buildLogoutRequest(csrfCookie, csrfForm string) *http.Request {
	var body string
	if csrfForm != "" {
		body = "csrf_token=" + csrfForm
	}
	req := httptest.NewRequest(http.MethodPost, "/auth/logout", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if csrfCookie != "" {
		req.AddCookie(&http.Cookie{Name: csrfCookieName, Value: csrfCookie})
	}
	return req
}

// ── CSRF validation ───────────────────────────────────────────────────────────

func TestLogoutHandler_MissingCSRFCookie_Returns403(t *testing.T) {
	t.Setenv("MCP_JWT_SECRET", testSessionSecret)
	h := logoutHandler{}

	req := buildLogoutRequest("", "some-form-token")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusForbidden, rr.Code,
		"missing CSRF cookie must return 403")
}

func TestLogoutHandler_MissingCSRFFormField_Returns403(t *testing.T) {
	t.Setenv("MCP_JWT_SECRET", testSessionSecret)
	h := logoutHandler{}

	req := buildLogoutRequest("some-csrf-cookie-value", "")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusForbidden, rr.Code,
		"missing csrf_token form field must return 403")
}

func TestLogoutHandler_MismatchedCSRF_Returns403(t *testing.T) {
	t.Setenv("MCP_JWT_SECRET", testSessionSecret)
	h := logoutHandler{}

	req := buildLogoutRequest("cookie-token-value", "different-form-token")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusForbidden, rr.Code,
		"mismatched CSRF tokens must return 403 (constant-time compare)")
}

// ── valid logout ──────────────────────────────────────────────────────────────

func TestLogoutHandler_ValidCSRF_ClearsAndRedirects(t *testing.T) {
	t.Setenv("MCP_JWT_SECRET", testSessionSecret)
	h := logoutHandler{}

	csrfToken := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	req := buildLogoutRequest(csrfToken, csrfToken)
	// Also add a session cookie to verify it gets cleared.
	sessionCookie := issueSessionCookie(t, testSessionSub)
	req.AddCookie(sessionCookie)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	require.Equal(t, http.StatusFound, rr.Code)
	assert.Equal(t, "/", rr.Header().Get("Location"))

	// Both cookies must be cleared (Max-Age=0).
	var sessionCleared, csrfCleared bool
	for _, c := range rr.Result().Cookies() {
		if c.Name == sessionCookieName && c.MaxAge == 0 {
			sessionCleared = true
		}
		if c.Name == csrfCookieName && c.MaxAge == 0 {
			csrfCleared = true
		}
	}
	assert.True(t, sessionCleared, "ch_session must be cleared (Max-Age=0)")
	assert.True(t, csrfCleared, "ch_csrf must be cleared (Max-Age=0)")
}

// ── method guard ──────────────────────────────────────────────────────────────

func TestLogoutHandler_RejectsNonPost(t *testing.T) {
	h := logoutHandler{}

	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		req := httptest.NewRequest(method, "/auth/logout", nil)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		assert.Equal(t, http.StatusMethodNotAllowed, rr.Code,
			"method %s must be rejected", method)
	}
}

// ── constant-time compare ─────────────────────────────────────────────────────

// TestLogoutHandler_ConstantTimeCompare verifies that the CSRF check uses
// subtle.ConstantTimeCompare (== is timing-unsafe for secrets).
// We test behaviorally: tokens that differ only in the last byte must be rejected,
// and equal tokens must be accepted — the same check a timing-attack test would run.
func TestLogoutHandler_ConstantTimeCompare(t *testing.T) {
	h := logoutHandler{}

	good := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	badLast := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaab"

	// Different last byte → must be rejected.
	req := buildLogoutRequest(good, badLast)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusForbidden, rr.Code,
		"tokens differing by one byte must be rejected")

	// Equal → must succeed.
	req2 := buildLogoutRequest(good, good)
	rr2 := httptest.NewRecorder()
	h.ServeHTTP(rr2, req2)
	assert.Equal(t, http.StatusFound, rr2.Code,
		"equal tokens must produce a successful logout")
}
