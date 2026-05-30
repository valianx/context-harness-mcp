package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"

	oidcpkg "github.com/mariogutierrez/context-harness-mcp/internal/auth/oidc"
)

// TestOIDCCallback_StateMismatch_Returns4xx verifies threat #2 (AC-2):
// a callback with a state parameter that does not match the cookie is rejected
// before any code exchange occurs.
func TestOIDCCallback_StateMismatch_Returns4xx(t *testing.T) {
	t.Setenv("MCP_JWT_SECRET", "test-secret-oidc-state-mismatch12")

	// Issue a state cookie with state="correct-state".
	req := httptest.NewRequest(http.MethodGet, "/auth/login", nil)
	rr := httptest.NewRecorder()
	err := oidcpkg.IssueStateCookie(rr, req, "correct-state", "nonce", "pkce", "")
	if err != nil {
		t.Skipf("cannot issue state cookie (MCP_JWT_SECRET not set?): %v", err)
		return
	}

	// Build a callback request with the wrong state.
	callbackReq := httptest.NewRequest(
		http.MethodGet,
		"/auth/callback?code=abc&state=WRONG-STATE",
		nil,
	)
	// Transfer the ch_oidc cookie to the callback request.
	for _, c := range rr.Result().Cookies() {
		callbackReq.AddCookie(c)
	}

	callbackRR := httptest.NewRecorder()
	h := &oidcCallbackHandler{pool: nil, baseURL: "https://mcp.test"}
	h.ServeHTTP(callbackRR, callbackReq)

	assert.GreaterOrEqual(t, callbackRR.Code, 400, "state mismatch must return 4xx")
	assert.Less(t, callbackRR.Code, 500, "state mismatch must return 4xx (not 5xx)")
}

// TestOIDCCallback_MissingStateCookie_Returns4xx verifies that the callback
// handler rejects requests where the ephemeral state cookie is absent.
func TestOIDCCallback_MissingStateCookie_Returns4xx(t *testing.T) {
	callbackReq := httptest.NewRequest(
		http.MethodGet,
		"/auth/callback?code=abc&state=some-state",
		nil,
	)
	// No ch_oidc cookie — simulates a request that skipped /auth/login.

	rr := httptest.NewRecorder()
	h := &oidcCallbackHandler{pool: nil, baseURL: "https://mcp.test"}
	h.ServeHTTP(rr, callbackReq)

	assert.GreaterOrEqual(t, rr.Code, 400, "missing cookie must return 4xx")
}

// TestOIDCCallback_RejectsNonGet verifies that non-GET methods receive 405.
func TestOIDCCallback_RejectsNonGet(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		req := httptest.NewRequest(method, "/auth/callback", nil)
		rr := httptest.NewRecorder()
		h := &oidcCallbackHandler{pool: nil, baseURL: ""}
		h.ServeHTTP(rr, req)
		assert.Equal(t, http.StatusMethodNotAllowed, rr.Code, "method %s should be rejected", method)
	}
}

// TestOIDCCallback_ProviderError_Returns4xx verifies that a callback carrying
// an error parameter (e.g. user denied consent) is rejected gracefully.
func TestOIDCCallback_ProviderError_Returns4xx(t *testing.T) {
	t.Setenv("MCP_JWT_SECRET", "test-secret-oidc-provider-error12")

	// Issue a valid state cookie.
	req := httptest.NewRequest(http.MethodGet, "/auth/login", nil)
	rr := httptest.NewRecorder()
	err := oidcpkg.IssueStateCookie(rr, req, "mystate", "mynonce", "mypkce", "")
	if err != nil {
		t.Skipf("cannot issue state cookie: %v", err)
		return
	}

	// Callback with correct state but provider returned an error.
	callbackReq := httptest.NewRequest(
		http.MethodGet,
		"/auth/callback?state=mystate&error=access_denied&error_description=user+denied",
		nil,
	)
	for _, c := range rr.Result().Cookies() {
		callbackReq.AddCookie(c)
	}

	callbackRR := httptest.NewRecorder()
	h := &oidcCallbackHandler{pool: nil, baseURL: "https://mcp.test"}
	h.ServeHTTP(callbackRR, callbackReq)

	assert.GreaterOrEqual(t, callbackRR.Code, 400, "provider error must return 4xx")
}

// TestOIDCCallback_NonceMismatch_Returns4xx verifies threat #3 (AC-3, SEC-001):
// a callback where the id_token nonce does not match the cookie nonce is rejected
// with a 4xx response — no MCP JWT is issued.
//
// The exchange step is stubbed via exchangeOverride (the test seam) so the test
// runs without a network connection, a Docker container, or a real OIDC provider.
// The stub returns a valid-looking Identity whose Nonce differs from the value
// stored in the state cookie, exercising the exact branch at auth_callback.go
// step 6 (subtle.ConstantTimeCompare on nonce).
func TestOIDCCallback_NonceMismatch_Returns4xx(t *testing.T) {
	t.Setenv("MCP_JWT_SECRET", "test-secret-oidc-nonce-mismatch-12")

	const cookieState = "correct-state"
	const cookieNonce = "legit-session-nonce"

	// Issue a state cookie that binds state=correct-state, nonce=legit-session-nonce.
	loginReq := httptest.NewRequest(http.MethodGet, "/auth/login", nil)
	issueRR := httptest.NewRecorder()
	err := oidcpkg.IssueStateCookie(issueRR, loginReq, cookieState, cookieNonce, "pkce-verifier", "")
	if err != nil {
		t.Skipf("cannot issue state cookie (MCP_JWT_SECRET not set?): %v", err)
		return
	}

	// Build the callback request with the correct state parameter but pass the
	// cookie that has nonce=legit-session-nonce.
	callbackReq := httptest.NewRequest(
		http.MethodGet,
		"/auth/callback?code=authcode123&state="+cookieState,
		nil,
	)
	for _, c := range issueRR.Result().Cookies() {
		callbackReq.AddCookie(c)
	}

	// Stub exchange: returns an Identity with a DIFFERENT nonce than the cookie.
	// This simulates a replayed id_token whose nonce was set by an attacker.
	stubExchange := func(_ context.Context, _, _ string) (*oidcpkg.Identity, error) {
		return &oidcpkg.Identity{
			Sub:   "attacker-sub",
			Email: "attacker@example.com",
			Nonce: "attacker-replayed-nonce", // intentionally != cookieNonce
		}, nil
	}

	callbackRR := httptest.NewRecorder()
	h := &oidcCallbackHandler{
		pool:             nil,
		baseURL:          "https://mcp.test",
		exchangeOverride: stubExchange,
	}
	h.ServeHTTP(callbackRR, callbackReq)

	assert.GreaterOrEqual(t, callbackRR.Code, 400, "nonce mismatch must return 4xx")
	assert.Less(t, callbackRR.Code, 500, "nonce mismatch must return 4xx (not 5xx)")

	// Confirm no MCP session cookie was set (no token was minted).
	for _, c := range callbackRR.Result().Cookies() {
		if c.Name == "ch_session" {
			t.Errorf("nonce mismatch: ch_session cookie must NOT be set, but found one: %v", c)
		}
	}
}
