package oidc

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStateCookie_RoundTrip(t *testing.T) {
	t.Setenv("MCP_JWT_SECRET", "test-secret-oidc-state-roundtrip-key")

	state, nonce, pkce, err := GenerateState()
	require.NoError(t, err)
	assert.Len(t, state, 64) // 32 bytes → 64 hex chars
	assert.Len(t, nonce, 64)
	assert.Len(t, pkce, 64)

	req := httptest.NewRequest(http.MethodGet, "/auth/login", nil)
	rr := httptest.NewRecorder()

	require.NoError(t, IssueStateCookie(rr, req, state, nonce, pkce, "/dashboard"))

	// Extract cookie from response and add to a new request.
	resp := rr.Result()
	readReq := httptest.NewRequest(http.MethodGet, "/auth/callback", nil)
	for _, c := range resp.Cookies() {
		readReq.AddCookie(c)
	}

	gotState, gotNonce, gotPKCE, gotNext, err := ReadStateCookie(readReq)
	require.NoError(t, err)
	assert.Equal(t, state, gotState)
	assert.Equal(t, nonce, gotNonce)
	assert.Equal(t, pkce, gotPKCE)
	assert.Equal(t, "/dashboard", gotNext)
}

func TestStateCookie_TamperedHMAC_Rejected(t *testing.T) {
	// Threat #10: tampered cookie must be rejected.
	t.Setenv("MCP_JWT_SECRET", "test-secret-oidc-tamper-test-key1")

	state, nonce, pkce, err := GenerateState()
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, "/auth/login", nil)
	rr := httptest.NewRecorder()
	require.NoError(t, IssueStateCookie(rr, req, state, nonce, pkce, ""))

	resp := rr.Result()
	for _, c := range resp.Cookies() {
		if c.Name == oidcCookieName {
			// Flip a character in the signature portion.
			val := c.Value
			if len(val) > 5 {
				bytes := []byte(val)
				bytes[len(bytes)-1] ^= 0x01
				c.Value = string(bytes)
			}
			readReq := httptest.NewRequest(http.MethodGet, "/auth/callback", nil)
			readReq.AddCookie(c)
			_, _, _, _, err := ReadStateCookie(readReq)
			assert.ErrorIs(t, err, ErrInvalidOIDCState, "tampered cookie must be rejected")
			return
		}
	}
	t.Fatal("cookie not found in response")
}

func TestStateCookie_MissingCookie_Rejected(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/auth/callback", nil)
	_, _, _, _, err := ReadStateCookie(req)
	assert.ErrorIs(t, err, ErrInvalidOIDCState)
}

func TestStateCookie_ExpiredCookie_Rejected(t *testing.T) {
	t.Setenv("MCP_JWT_SECRET", "test-secret-oidc-expiry-test-key11")

	secret := []byte("test-secret-oidc-expiry-test-key11")

	// Build a bundle with an IssuedAt in the far past so it appears expired.
	expired := stateBundle{
		State:    "s",
		Nonce:    "n",
		PKCE:     "p",
		Next:     "",
		IssuedAt: time.Now().Add(-20 * time.Minute).Unix(),
	}
	val, err := encodeBundle(expired, secret)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, "/auth/callback", nil)
	req.AddCookie(&http.Cookie{Name: oidcCookieName, Value: val})

	_, _, _, _, err = ReadStateCookie(req)
	assert.ErrorIs(t, err, ErrInvalidOIDCState, "expired cookie must be rejected")
}

func TestStateCookie_WrongSecret_Rejected(t *testing.T) {
	// Cookie signed with key-A must be rejected when the server uses key-B.
	t.Setenv("MCP_JWT_SECRET", "key-a-secret-oidc-cross-key-test")

	state, nonce, pkce, err := GenerateState()
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, "/auth/login", nil)
	rr := httptest.NewRecorder()
	require.NoError(t, IssueStateCookie(rr, req, state, nonce, pkce, ""))

	resp := rr.Result()

	// Switch to a different secret.
	t.Setenv("MCP_JWT_SECRET", "key-b-secret-oidc-cross-key-test")

	readReq := httptest.NewRequest(http.MethodGet, "/auth/callback", nil)
	for _, c := range resp.Cookies() {
		readReq.AddCookie(c)
	}
	_, _, _, _, err = ReadStateCookie(readReq)
	assert.ErrorIs(t, err, ErrInvalidOIDCState, "cookie signed with different key must be rejected")
}

func TestClearStateCookie(t *testing.T) {
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/auth/callback", nil)
	ClearStateCookie(rr, req)

	resp := rr.Result()
	var found bool
	for _, c := range resp.Cookies() {
		if c.Name == oidcCookieName {
			assert.Equal(t, 0, c.MaxAge, "cleared cookie must have MaxAge=0")
			assert.Equal(t, "", c.Value, "cleared cookie must have empty value")
			found = true
		}
	}
	assert.True(t, found, "ClearStateCookie must write a Set-Cookie header")
}

// TestStateCookie_SecureFlagFromConfig verifies that the Secure flag on the
// state cookie is derived from the deployment configuration (MCP_PUBLIC_URL or
// OIDC_REDIRECT_URL), NOT from per-request TLS detection (fix: SEC-002, CWE-614).
//
// Behind a TLS-terminating proxy (Railway, Render, Fly), r.TLS is nil even for
// HTTPS deployments. The cookie must still be Secure=true in that scenario.
func TestStateCookie_SecureFlagFromConfig(t *testing.T) {
	t.Setenv("MCP_JWT_SECRET", "test-secret-cookie-secure-flag-abc")

	tests := []struct {
		name          string
		publicURL     string
		redirectURL   string
		wantSecure    bool
	}{
		{
			name:       "https public URL → Secure=true",
			publicURL:  "https://mcp.example.com",
			wantSecure: true,
		},
		{
			name:        "https redirect URL → Secure=true",
			publicURL:   "",
			redirectURL: "https://mcp.example.com/auth/callback",
			wantSecure:  true,
		},
		{
			name:        "localhost redirect URL → Secure=false",
			publicURL:   "",
			redirectURL: "http://localhost:7654/auth/callback",
			wantSecure:  false,
		},
		{
			name:       "empty config → Secure=false (local dev fallback)",
			publicURL:  "",
			wantSecure: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("MCP_PUBLIC_URL", tc.publicURL)
			t.Setenv("OIDC_REDIRECT_URL", tc.redirectURL)

			state, nonce, pkce, err := GenerateState()
			require.NoError(t, err)

			// Use a plain HTTP request without TLS. Behind a proxy r.TLS is nil
			// even when the upstream connection is HTTPS — this is the scenario
			// that the per-request isHTTPS() check fails on.
			req := httptest.NewRequest(http.MethodGet, "/auth/login", nil)
			// r.TLS is nil here — intentionally simulates a behind-proxy request.
			rr := httptest.NewRecorder()
			require.NoError(t, IssueStateCookie(rr, req, state, nonce, pkce, ""))

			resp := rr.Result()
			var cookie *http.Cookie
			for _, c := range resp.Cookies() {
				if c.Name == oidcCookieName {
					cookie = c
					break
				}
			}
			require.NotNil(t, cookie, "ch_oidc cookie must be set")
			assert.Equal(t, tc.wantSecure, cookie.Secure,
				"Secure flag must be %v for config: publicURL=%q redirectURL=%q",
				tc.wantSecure, tc.publicURL, tc.redirectURL)
		})
	}
}

func TestStateCookie_Attributes(t *testing.T) {
	t.Setenv("MCP_JWT_SECRET", "test-secret-cookie-attributes-abc")

	state, nonce, pkce, err := GenerateState()
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, "/auth/login", nil)
	rr := httptest.NewRecorder()
	require.NoError(t, IssueStateCookie(rr, req, state, nonce, pkce, ""))

	resp := rr.Result()
	var cookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == oidcCookieName {
			cookie = c
			break
		}
	}

	require.NotNil(t, cookie, "ch_oidc cookie must be set")
	assert.True(t, cookie.HttpOnly, "cookie must be HttpOnly")
	assert.Equal(t, oidcCookieMaxAge, cookie.MaxAge, "cookie must have 10-minute Max-Age")
	assert.Equal(t, http.SameSiteLaxMode, cookie.SameSite, "cookie must be SameSite=Lax")
	assert.Equal(t, "/", cookie.Path, "cookie path must be /")
}
