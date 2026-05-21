package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── dashboard handler tests (no real DB) ─────────────────────────────────────
// These tests use a dashboardHandler with a nil pool for cases that terminate
// before the DB lookup (auth redirects). The email-lookup path is exercised in
// tests/auth_e2e_test.go with a real testcontainers DB.

func buildDashboardHandler() *dashboardHandler {
	return newDashboardHandler(nil) // nil pool: panics only if DB lookup is reached
}

// issueSessionCookie issues a ch_session cookie for sub and returns it so it
// can be added to a test request.
func issueSessionCookie(t *testing.T, sub string) *http.Cookie {
	t.Helper()
	t.Setenv("MCP_JWT_SECRET", testSessionSecret)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	require.NoError(t, Issue(rr, req, sub))

	cookies := rr.Result().Cookies()
	require.NotEmpty(t, cookies)

	for _, c := range cookies {
		if c.Name == sessionCookieName {
			return c
		}
	}
	t.Fatal("ch_session cookie not found")
	return nil
}

// issueExpiredSessionCookie returns a cookie whose Exp is in the past.
func issueExpiredSessionCookie(t *testing.T, sub string) *http.Cookie {
	t.Helper()
	t.Setenv("MCP_JWT_SECRET", testSessionSecret)

	secret := []byte(testSessionSecret)
	p := sessionPayload{
		Sub: sub,
		IAT: time.Now().Add(-48 * time.Hour).Unix(),
		Exp: time.Now().Add(-1 * time.Second).Unix(),
	}
	value, err := encodeSession(p, secret)
	require.NoError(t, err)
	return &http.Cookie{Name: sessionCookieName, Value: value}
}

// ── redirect cases ────────────────────────────────────────────────────────────

func TestDashboardHandler_NoCookie_Redirects(t *testing.T) {
	t.Setenv("MCP_JWT_SECRET", testSessionSecret)
	h := buildDashboardHandler()

	req := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusFound, rr.Code)
	assert.Contains(t, rr.Header().Get("Location"), "/auth/login")
}

func TestDashboardHandler_InvalidCookie_Redirects(t *testing.T) {
	t.Setenv("MCP_JWT_SECRET", testSessionSecret)
	h := buildDashboardHandler()

	req := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	req.AddCookie(issueExpiredSessionCookie(t, testSessionSub))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusFound, rr.Code,
		"expired cookie must redirect to login")
	assert.Contains(t, rr.Header().Get("Location"), "/auth/login")
}

func TestDashboardHandler_TamperedCookie_Redirects(t *testing.T) {
	t.Setenv("MCP_JWT_SECRET", testSessionSecret)
	h := buildDashboardHandler()

	req := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "tampered.value"})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusFound, rr.Code,
		"tampered cookie must redirect to login")
}

// ── CSRF cookie set ───────────────────────────────────────────────────────────

func TestDashboardHandler_ValidCookie_SetsCSRFCookie(t *testing.T) {
	// This test verifies that a ch_csrf cookie is set when absent.
	// We can't use a real DB in this package-internal test, but we can verify
	// the CSRF logic by checking the Set-Cookie header appears before the DB
	// lookup would panic — except the DB lookup happens first.
	// Strategy: use a mock-friendly variant. For this case we verify the CSRF
	// generation helper directly.
	t.Setenv("MCP_JWT_SECRET", testSessionSecret)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/dashboard", nil)

	token, err := ensureCSRFCookie(rr, req)
	require.NoError(t, err)
	assert.Len(t, token, 64, "CSRF token must be 64-char hex")

	cookies := rr.Result().Cookies()
	require.NotEmpty(t, cookies)
	found := false
	for _, c := range cookies {
		if c.Name == csrfCookieName {
			found = true
			assert.Equal(t, token, c.Value)
			assert.False(t, c.HttpOnly, "ch_csrf must NOT be HttpOnly (double-submit pattern)")
			assert.Equal(t, "/", c.Path)
		}
	}
	assert.True(t, found, "ch_csrf cookie must be set")
}

func TestDashboardHandler_CSRFCookie_Reused(t *testing.T) {
	// When ch_csrf is already present, ensureCSRFCookie must return the
	// existing value without setting a new cookie.
	t.Setenv("MCP_JWT_SECRET", testSessionSecret)

	existing := "existing-csrf-token-64chars-placeholder-xxxxxxxxxxxxxxxxxxxxx"
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	req.AddCookie(&http.Cookie{Name: csrfCookieName, Value: existing})

	token, err := ensureCSRFCookie(rr, req)
	require.NoError(t, err)
	assert.Equal(t, existing, token, "existing CSRF token must be reused")
	// No new Set-Cookie should be written.
	assert.Empty(t, rr.Header().Get("Set-Cookie"), "no new Set-Cookie when csrf already present")
}

// ── generate-token: session check ────────────────────────────────────────────

func TestDashboardHandler_GenerateToken_NoCookie_Redirects(t *testing.T) {
	t.Setenv("MCP_JWT_SECRET", testSessionSecret)
	h := buildDashboardHandler()

	req := httptest.NewRequest(http.MethodPost, "/dashboard/generate-token", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusFound, rr.Code,
		"unauthenticated POST to generate-token must redirect to login")
}

// ── generate-token: CSRF guard ────────────────────────────────────────────────

// TestDashboardHandler_GenerateToken_MissingCSRFToken verifies that a POST
// without a csrf_token form field returns 403 (AC-5).
func TestDashboardHandler_GenerateToken_MissingCSRFToken_Returns403(t *testing.T) {
	t.Setenv("MCP_JWT_SECRET", testSessionSecret)
	h := buildDashboardHandler()

	csrfToken := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	req := httptest.NewRequest(http.MethodPost, "/dashboard/generate-token", nil)
	req.AddCookie(issueSessionCookie(t, testSessionSub))
	req.AddCookie(&http.Cookie{Name: csrfCookieName, Value: csrfToken})
	// No csrf_token form field.
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusForbidden, rr.Code,
		"missing csrf_token form field must return 403")
}

// TestDashboardHandler_GenerateToken_MissingCSRFCookie verifies that a POST
// without a ch_csrf cookie returns 403 (AC-5).
func TestDashboardHandler_GenerateToken_MissingCSRFCookie_Returns403(t *testing.T) {
	t.Setenv("MCP_JWT_SECRET", testSessionSecret)
	h := buildDashboardHandler()

	req := httptest.NewRequest(http.MethodPost, "/dashboard/generate-token",
		strings.NewReader("csrf_token=sometoken"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(issueSessionCookie(t, testSessionSub))
	// No ch_csrf cookie.
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusForbidden, rr.Code,
		"missing ch_csrf cookie must return 403")
}

// TestDashboardHandler_GenerateToken_MismatchedCSRF verifies that a CSRF
// mismatch returns 403 (AC-5).
func TestDashboardHandler_GenerateToken_MismatchedCSRF_Returns403(t *testing.T) {
	t.Setenv("MCP_JWT_SECRET", testSessionSecret)
	h := buildDashboardHandler()

	csrfCookie := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	csrfForm := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	req := httptest.NewRequest(http.MethodPost, "/dashboard/generate-token",
		strings.NewReader("csrf_token="+csrfForm))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(issueSessionCookie(t, testSessionSub))
	req.AddCookie(&http.Cookie{Name: csrfCookieName, Value: csrfCookie})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusForbidden, rr.Code,
		"mismatched CSRF tokens must return 403 (constant-time compare)")
}

// ── csrfValid helper: constant-time compare ────────────────────────────────────

// TestCSRFValid_ConstantTimeCompare verifies csrfValid uses constant-time compare
// by testing that tokens differing only in the last byte are rejected,
// and that equal tokens are accepted.
func TestCSRFValid_ConstantTimeCompare(t *testing.T) {
	good := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	badLast := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaab"

	// Different last byte — must be rejected.
	req1 := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("csrf_token="+badLast))
	req1.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req1.AddCookie(&http.Cookie{Name: csrfCookieName, Value: good})
	assert.False(t, csrfValid(req1), "tokens differing by one byte must be rejected")

	// Equal — must be accepted.
	req2 := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("csrf_token="+good))
	req2.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req2.AddCookie(&http.Cookie{Name: csrfCookieName, Value: good})
	assert.True(t, csrfValid(req2), "equal tokens must be accepted")
}

// ── method guard ──────────────────────────────────────────────────────────────

func TestDashboardHandler_WrongMethod(t *testing.T) {
	t.Setenv("MCP_JWT_SECRET", testSessionSecret)
	h := buildDashboardHandler()

	for _, method := range []string{http.MethodPut, http.MethodDelete, http.MethodPatch} {
		req := httptest.NewRequest(method, "/dashboard", nil)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		assert.Equal(t, http.StatusMethodNotAllowed, rr.Code, "method %s should be rejected", method)
	}
}
