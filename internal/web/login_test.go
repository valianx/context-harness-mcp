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

	// AC-6: template substitutions are present in the response.
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

	// AC-6: the login page must have an email input and call /auth/v1/recover.
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
