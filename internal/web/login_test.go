package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoginHandler_ServesHTML(t *testing.T) {
	t.Setenv("SUPABASE_PROJECT_URL", "https://logintest.supabase.co")
	t.Setenv("SUPABASE_ANON_KEY", "login-anon-key")
	t.Setenv("MCP_PUBLIC_URL", "https://mcp.example.com")

	h := newLoginHandler()
	req := httptest.NewRequest(http.MethodGet, "/auth/login", nil)
	rr := httptest.NewRecorder()

	h.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusOK, rr.Code)
	assert.Contains(t, rr.Header().Get("Content-Type"), "text/html")

	body := rr.Body.String()

	// Template substitutions are present in the response.
	assert.Contains(t, body, "https://logintest.supabase.co",
		"SUPABASE_PROJECT_URL must be substituted into login.html")
	assert.Contains(t, body, "login-anon-key",
		"SUPABASE_ANON_KEY must be substituted into login.html")
	assert.Contains(t, body, "https://mcp.example.com",
		"MCP_PUBLIC_URL must be substituted into login.html")
}

func TestLoginHandler_RejectsNonGet(t *testing.T) {
	h := newLoginHandler()

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		req := httptest.NewRequest(method, "/auth/login", nil)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		assert.Equal(t, http.StatusMethodNotAllowed, rr.Code, "method %s should be rejected", method)
	}
}

func TestLoginHandler_ContainsExpectedElements(t *testing.T) {
	t.Setenv("SUPABASE_PROJECT_URL", "https://proj.supabase.co")
	t.Setenv("SUPABASE_ANON_KEY", "key-xyz")
	t.Setenv("MCP_PUBLIC_URL", "")

	h := newLoginHandler()
	req := httptest.NewRequest(http.MethodGet, "/auth/login", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	body := rr.Body.String()

	// The login page must have an email input and call /auth/v1/recover.
	assert.True(t, strings.Contains(body, `type="email"`) || strings.Contains(body, "email"),
		"login.html must contain an email input")
	assert.Contains(t, body, "/auth/v1/recover",
		"login.html must call POST /auth/v1/recover against Supabase")
	assert.Contains(t, body, "/auth/callback",
		"login.html must set redirect_to pointing to /auth/callback")
}

func TestRegisterLogin_RoutesCorrectly(t *testing.T) {
	t.Setenv("SUPABASE_PROJECT_URL", "https://x.supabase.co")
	t.Setenv("SUPABASE_ANON_KEY", "k")
	t.Setenv("MCP_PUBLIC_URL", "")

	mux := http.NewServeMux()
	RegisterLogin(mux)

	req := httptest.NewRequest(http.MethodGet, "/auth/login", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusOK, rr.Code)
}

// ── ?next= plumbing (PR-1 additions) ─────────────────────────────────────────

func TestLoginHandler_SafeNext_AppearsInTemplate(t *testing.T) {
	t.Setenv("SUPABASE_PROJECT_URL", "https://proj.supabase.co")
	t.Setenv("SUPABASE_ANON_KEY", "key-xyz")
	t.Setenv("MCP_PUBLIC_URL", "https://mcp.example.com")

	h := newLoginHandler()
	req := httptest.NewRequest(http.MethodGet, "/auth/login?next=%2Fviewer%2F", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	body := rr.Body.String()
	// The safe next value must appear in the JS (as NEXT constant).
	assert.Contains(t, body, "/viewer/",
		"safe ?next=/viewer/ must appear in the rendered template")
}

func TestLoginHandler_UnsafeNext_IsDropped(t *testing.T) {
	t.Setenv("SUPABASE_PROJECT_URL", "https://proj.supabase.co")
	t.Setenv("SUPABASE_ANON_KEY", "key-xyz")
	t.Setenv("MCP_PUBLIC_URL", "https://mcp.example.com")

	h := newLoginHandler()
	req := httptest.NewRequest(http.MethodGet, "/auth/login?next=https%3A%2F%2Fevil.com", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	body := rr.Body.String()
	// The unsafe value must NOT appear in the rendered output.
	assert.NotContains(t, body, "evil.com",
		"unsafe ?next= must not appear in the template — IsSafe dropped it")
}

func TestLoginHandler_Dashboard_Next_IsAccepted(t *testing.T) {
	t.Setenv("SUPABASE_PROJECT_URL", "https://proj.supabase.co")
	t.Setenv("SUPABASE_ANON_KEY", "key-xyz")
	t.Setenv("MCP_PUBLIC_URL", "https://mcp.example.com")

	h := newLoginHandler()
	req := httptest.NewRequest(http.MethodGet, "/auth/login?next=%2Fdashboard", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	body := rr.Body.String()
	assert.Contains(t, body, "/dashboard",
		"safe ?next=/dashboard must appear in the template")
}

// TestSafeNext_Logic tests the safeNext helper directly.
func TestSafeNext_Logic(t *testing.T) {
	tests := []struct {
		rawQuery string
		want     string
	}{
		{"next=%2Fviewer%2F", "/viewer/"},
		{"next=%2Fdashboard", "/dashboard"},
		{"next=https%3A%2F%2Fevil.com", ""},
		{"next=%2Fadmin", ""},
		{"", ""},
	}
	for _, tc := range tests {
		req := httptest.NewRequest(http.MethodGet, "/auth/login?"+tc.rawQuery, nil)
		got := safeNext(req)
		assert.Equal(t, tc.want, got, "safeNext with query %q", tc.rawQuery)
	}
}

// TestSupabaseRedirectTo_WithNext verifies the redirect_to URL includes next.
func TestSupabaseRedirectTo_WithNext(t *testing.T) {
	url := supabaseRedirectTo("https://mcp.example.com", "/viewer/")
	assert.Equal(t, "https://mcp.example.com/auth/callback?next=%2Fviewer%2F", url)
}

// TestSupabaseRedirectTo_NoNext verifies the redirect_to URL without next.
func TestSupabaseRedirectTo_NoNext(t *testing.T) {
	url := supabaseRedirectTo("https://mcp.example.com", "")
	assert.Equal(t, "https://mcp.example.com/auth/callback", url)
}

// ── already-signed-in redirect (PR-2 additions) ───────────────────────────────

// TestLoginHandler_AlreadySignedIn_RedirectsToDashboard verifies that a user
// with a valid ch_session is 302-redirected to /dashboard (AC-8).
func TestLoginHandler_AlreadySignedIn_RedirectsToDashboard(t *testing.T) {
	t.Setenv("MCP_JWT_SECRET", testSessionSecret)
	t.Setenv("SUPABASE_PROJECT_URL", "https://proj.supabase.co")
	t.Setenv("SUPABASE_ANON_KEY", "key-xyz")
	t.Setenv("MCP_PUBLIC_URL", "https://mcp.example.com")

	h := newLoginHandler()
	req := httptest.NewRequest(http.MethodGet, "/auth/login", nil)
	req.AddCookie(issueSessionCookie(t, testSessionSub))

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	require.Equal(t, http.StatusFound, rr.Code,
		"valid session must 302-redirect away from login")
	assert.Equal(t, "/dashboard", rr.Header().Get("Location"),
		"must redirect to /dashboard by default")
}

// TestLoginHandler_AlreadySignedIn_WithSafeNext_RedirectsToNext verifies that
// an authenticated user is redirected to a valid ?next= target (AC-8).
func TestLoginHandler_AlreadySignedIn_WithSafeNext_RedirectsToNext(t *testing.T) {
	t.Setenv("MCP_JWT_SECRET", testSessionSecret)
	t.Setenv("SUPABASE_PROJECT_URL", "https://proj.supabase.co")
	t.Setenv("SUPABASE_ANON_KEY", "key-xyz")
	t.Setenv("MCP_PUBLIC_URL", "https://mcp.example.com")

	h := newLoginHandler()
	req := httptest.NewRequest(http.MethodGet, "/auth/login?next=%2Fviewer%2F", nil)
	req.AddCookie(issueSessionCookie(t, testSessionSub))

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	require.Equal(t, http.StatusFound, rr.Code)
	assert.Equal(t, "/viewer/", rr.Header().Get("Location"),
		"must redirect to safe ?next= value when signed in")
}

// TestLoginHandler_AlreadySignedIn_WithUnsafeNext_RedirectsToDashboard verifies
// that an unsafe ?next= is ignored and the user goes to /dashboard (AC-8).
func TestLoginHandler_AlreadySignedIn_WithUnsafeNext_RedirectsToDashboard(t *testing.T) {
	t.Setenv("MCP_JWT_SECRET", testSessionSecret)
	t.Setenv("SUPABASE_PROJECT_URL", "https://proj.supabase.co")
	t.Setenv("SUPABASE_ANON_KEY", "key-xyz")
	t.Setenv("MCP_PUBLIC_URL", "https://mcp.example.com")

	h := newLoginHandler()
	req := httptest.NewRequest(http.MethodGet, "/auth/login?next=https%3A%2F%2Fevil.com", nil)
	req.AddCookie(issueSessionCookie(t, testSessionSub))

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	require.Equal(t, http.StatusFound, rr.Code)
	assert.Equal(t, "/dashboard", rr.Header().Get("Location"),
		"unsafe ?next= must be ignored — fallback to /dashboard")
}

// TestLoginHandler_MalformedCookie_ServesForm verifies that a malformed session
// cookie doesn't cause a 5xx — the form is served normally (AC-8).
func TestLoginHandler_MalformedCookie_ServesForm(t *testing.T) {
	t.Setenv("MCP_JWT_SECRET", testSessionSecret)
	t.Setenv("SUPABASE_PROJECT_URL", "https://proj.supabase.co")
	t.Setenv("SUPABASE_ANON_KEY", "key-xyz")
	t.Setenv("MCP_PUBLIC_URL", "https://mcp.example.com")

	h := newLoginHandler()
	req := httptest.NewRequest(http.MethodGet, "/auth/login", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "not-a-valid-cookie"})

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusOK, rr.Code,
		"malformed session cookie must not 5xx — form serves normally")
}
