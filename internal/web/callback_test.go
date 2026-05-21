package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCallbackHandler_ServesHTML(t *testing.T) {
	t.Setenv("SUPABASE_PROJECT_URL", "https://test.supabase.co")
	t.Setenv("SUPABASE_ANON_KEY", "anon-key-test")
	t.Setenv("MCP_PUBLIC_URL", "https://mcp.example.com")

	h := newCallbackHandler()
	req := httptest.NewRequest(http.MethodGet, "/auth/callback", nil)
	rr := httptest.NewRecorder()

	h.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusOK, rr.Code)
	assert.Contains(t, rr.Header().Get("Content-Type"), "text/html")

	body := rr.Body.String()

	// AC-1: template substitutions are present in the response.
	assert.Contains(t, body, "https://test.supabase.co",
		"SUPABASE_PROJECT_URL must be substituted into callback.html")
	assert.Contains(t, body, "anon-key-test",
		"SUPABASE_ANON_KEY must be substituted into callback.html")
	assert.Contains(t, body, "https://mcp.example.com",
		"MCP_PUBLIC_URL must be substituted into callback.html")
}

func TestCallbackHandler_RejectsNonGet(t *testing.T) {
	h := newCallbackHandler()

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		req := httptest.NewRequest(method, "/auth/callback", nil)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		assert.Equal(t, http.StatusMethodNotAllowed, rr.Code, "method %s should be rejected", method)
	}
}

func TestCallbackHandler_ContainsExpectedElements(t *testing.T) {
	t.Setenv("SUPABASE_PROJECT_URL", "https://proj.supabase.co")
	t.Setenv("SUPABASE_ANON_KEY", "key-abc")
	t.Setenv("MCP_PUBLIC_URL", "")

	h := newCallbackHandler()
	req := httptest.NewRequest(http.MethodGet, "/auth/callback", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	body := rr.Body.String()

	// The page must contain elements necessary for the auth callback flow.
	assert.True(t, strings.Contains(body, "/auth/exchange"),
		"callback.html must reference /auth/exchange for the token exchange call")
	assert.True(t, strings.Contains(body, "access_token") || strings.Contains(body, "fragment"),
		"callback.html must handle the access_token fragment")

	// AC-3: no password form rendered after removing the "Set your access password" panel.
	assert.NotContains(t, body, `type="password"`,
		"callback.html must not render any password input")
	assert.NotContains(t, body, "setpw",
		"callback.html must not contain leftover setpw form id or handler")
	assert.NotContains(t, body, "Set your access password",
		"callback.html must not contain the old password-set panel copy")
}

func TestRegisterCallback_RoutesCorrectly(t *testing.T) {
	t.Setenv("SUPABASE_PROJECT_URL", "https://x.supabase.co")
	t.Setenv("SUPABASE_ANON_KEY", "k")
	t.Setenv("MCP_PUBLIC_URL", "")

	mux := http.NewServeMux()
	RegisterCallback(mux)

	req := httptest.NewRequest(http.MethodGet, "/auth/callback", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusOK, rr.Code)
}
