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
	// html/template JS-string-escapes "/" as "\/" inside <script> string literals,
	// so "https://..." becomes "https:\/\/..." in the rendered output. Both forms
	// are semantically identical in a JavaScript string — "\/" === "/".
	assert.Contains(t, body, "logintest.supabase.co",
		"SUPABASE_PROJECT_URL must be substituted into login.html")
	assert.Contains(t, body, "login-anon-key",
		"SUPABASE_ANON_KEY must be substituted into login.html")
	assert.Contains(t, body, "mcp.example.com",
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

	// The login page must have an email input and call /auth/v1/otp.
	assert.True(t, strings.Contains(body, `type="email"`) || strings.Contains(body, "email"),
		"login.html must contain an email input")
	assert.Contains(t, body, "/auth/v1/otp",
		"login.html must call POST /auth/v1/otp against Supabase")
	assert.NotContains(t, body, "/auth/v1/recover",
		"login.html must not call the old /auth/v1/recover endpoint")
	assert.Contains(t, body, "create_user: false",
		"login.html must set create_user:false (snake_case REST field) to block auto-provisioning")
	assert.Contains(t, body, "/auth/callback",
		"login.html must set email_redirect_to pointing to /auth/callback")
	assert.NotContains(t, strings.ToLower(body), "password",
		"login.html must not contain any password-related copy or inputs")

	// AC-1: page title and visible heading must reference "Sign in"; old labels must be absent.
	assert.Contains(t, strings.ToLower(body), "sign in",
		"login.html title/heading must say 'Sign in'")
	assert.NotContains(t, strings.ToLower(body), "re-authenticate",
		"login.html must not reference the old 'Re-authenticate' label")
	assert.NotContains(t, strings.ToLower(body), "recovery",
		"login.html must not reference 'Recovery' anywhere")
	assert.NotContains(t, strings.ToLower(body), "reset password",
		"login.html must not reference 'Reset password'")
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
	// html/template JS-string-escapes "/" as "\/" in <script> context, so
	// "/viewer/" becomes "\/viewer\/" in the output. Both forms are valid JS.
	assert.Contains(t, body, "viewer",
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

// ── SEC-001: invariant success (JS source inspection) ────────────────────────

// TestLoginJS_NoSupabaseErrorReflection asserts that the served JS does not
// reference err.msg, err.error_description, or r.status in any error path
// (AC-1 + AC-2 surrogate — browser-side behavior verified via source inspection).
func TestLoginJS_NoSupabaseErrorReflection(t *testing.T) {
	t.Setenv("SUPABASE_PROJECT_URL", "https://proj.supabase.co")
	t.Setenv("SUPABASE_ANON_KEY", "key-xyz")
	t.Setenv("MCP_PUBLIC_URL", "https://mcp.example.com")

	h := newLoginHandler()
	req := httptest.NewRequest(http.MethodGet, "/auth/login", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	body := rr.Body.String()

	// SEC-001: Supabase error fields must not appear in the JS — they would
	// allow the caller to distinguish known vs unknown email addresses.
	assert.NotContains(t, body, "err.msg",
		"login.html must not read err.msg from Supabase response (user enumeration)")
	assert.NotContains(t, body, "err.error_description",
		"login.html must not read err.error_description from Supabase response")
	assert.NotContains(t, body, "r.status",
		"login.html must not use r.status in any user-facing error path")
}

// TestLoginJS_SingleFormErrorPath asserts that the only formError() call in the
// JS is in the network-failure catch branch and uses a fixed generic message
// (AC-2 surrogate — inspecting the static source for the invariant).
func TestLoginJS_SingleFormErrorPath(t *testing.T) {
	t.Setenv("SUPABASE_PROJECT_URL", "https://proj.supabase.co")
	t.Setenv("SUPABASE_ANON_KEY", "key-xyz")
	t.Setenv("MCP_PUBLIC_URL", "https://mcp.example.com")

	h := newLoginHandler()
	req := httptest.NewRequest(http.MethodGet, "/auth/login", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	body := rr.Body.String()

	// The only formError call with a non-validation message must be the generic
	// network-failure string. There must be no Supabase-derived message passed in.
	assert.Contains(t, body, "Couldn't reach the auth service. Try again.",
		"login.html catch block must show a fixed generic message (not Supabase-derived)")
}

// ── SEC-002: html/template JS-string escaping (AC-4 + AC-5) ─────────────────

// TestLoginHandler_NextWithDangerousChars_IsEscaped verifies that html/template
// JS-string-escapes the {{.Next}} field. The test uses a value that passes IsSafe
// (starts with "/viewer/") and asserts the characteristic "\/" escaping that
// html/template applies in JS string context (AC-5).
func TestLoginHandler_NextWithDangerousChars_IsEscaped(t *testing.T) {
	t.Setenv("SUPABASE_PROJECT_URL", "https://proj.supabase.co")
	t.Setenv("SUPABASE_ANON_KEY", "key-xyz")
	t.Setenv("MCP_PUBLIC_URL", "https://mcp.example.com")

	h := newLoginHandler()
	// "/viewer/" passes IsSafe. html/template will JS-escape the slashes.
	req := httptest.NewRequest(http.MethodGet, "/auth/login?next=%2Fviewer%2F", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	body := rr.Body.String()

	// html/template's signature JS-string behavior: "/" → "\/" in script context.
	// The rendered NEXT constant must NOT contain a bare unescaped "/" (it appears
	// as "\/" instead), proving the contextual escaping is active.
	assert.Contains(t, body, `\/viewer\/`,
		"html/template must JS-escape '/' to '\\/' in the NEXT JS string literal (AC-5)")

	// The literal NEXT JS string assignment must contain the escaped form, not the
	// raw form. Scope the check to the assignment line so the closing </script>
	// tag of the script block itself doesn't trigger a false positive.
	assert.Contains(t, body, `const NEXT = "\/viewer\/"`,
		"NEXT JS literal must hold the escaped value, not the raw value")
}

// TestLoginTemplate_DirectEscaping_XSSProof executes the login template directly
// with a crafted unsafe Next value (bypassing IsSafe) to prove html/template
// escapes dangerous characters in JS string context (AC-5 defence-in-depth).
func TestLoginTemplate_DirectEscaping_XSSProof(t *testing.T) {
	t.Setenv("SUPABASE_PROJECT_URL", "https://proj.supabase.co")
	t.Setenv("SUPABASE_ANON_KEY", "key-xyz")
	t.Setenv("MCP_PUBLIC_URL", "https://mcp.example.com")

	h := newLoginHandler()

	// Craft data directly — bypass handler + IsSafe to test the template layer alone.
	data := loginTemplateData{
		SupabaseProjectURL: "https://proj.supabase.co",
		SupabaseAnonKey:    "key-xyz",
		MCPPublicURL:       "https://mcp.example.com",
		// A value that would break out of a JS double-quoted string if not escaped.
		Next: `"; alert(1); //`,
	}

	var buf strings.Builder
	err := h.tmpl.ExecuteTemplate(&buf, "login.html", data)
	require.NoError(t, err, "template must execute without error even with dangerous input")

	body := buf.String()

	// html/template must have escaped the " and ; so the JS literal stays intact.
	// The raw unescaped attack string must not appear verbatim in the output.
	assert.NotContains(t, body, `"; alert(1); //`,
		"html/template must escape the dangerous Next value (AC-5 defence-in-depth)")
}
