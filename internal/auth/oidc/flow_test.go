package oidc

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── RSA key helpers ────────────────────────────────────────────────────────────

// testKey generates a fresh RSA-2048 key for signing test ID tokens.
func testKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	return key
}

// makeIDToken creates a minimal JWT id_token signed with the given RSA key.
// iss, aud, sub, nonce, exp are all set so go-oidc's verifier can validate them.
func makeIDToken(t *testing.T, key *rsa.PrivateKey, issuer, audience, sub, nonce string, exp time.Time) string {
	t.Helper()
	type customClaims struct {
		jwt.RegisteredClaims
		Nonce         string `json:"nonce,omitempty"`
		Email         string `json:"email,omitempty"`
		EmailVerified bool   `json:"email_verified,omitempty"`
	}
	claims := customClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    issuer,
			Subject:   sub,
			Audience:  jwt.ClaimStrings{audience},
			ExpiresAt: jwt.NewNumericDate(exp),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
		Nonce:         nonce,
		Email:         sub + "@example.com",
		EmailVerified: true,
	}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	raw, err := token.SignedString(key)
	require.NoError(t, err)
	return raw
}

// buildVerifier constructs an oidc.IDTokenVerifier using a StaticKeySet so
// verification runs offline (no network, no JWKS endpoint).
// This is the pattern for security-negative unit tests.
func buildVerifier(issuer, clientID string, key *rsa.PrivateKey, algs []string) *gooidc.IDTokenVerifier {
	keySet := &gooidc.StaticKeySet{PublicKeys: []crypto.PublicKey{&key.PublicKey}}
	cfg := &gooidc.Config{
		ClientID:             clientID,
		SupportedSigningAlgs: algs,
	}
	return gooidc.NewVerifier(issuer, keySet, cfg)
}

// ── Security-negative tests ───────────────────────────────────────────────────

// TestFlow_BadSignature_Rejected verifies threat #4 (AC-4): an id_token signed
// with a key not in the JWKS is rejected by the verifier.
func TestFlow_BadSignature_Rejected(t *testing.T) {
	goodKey := testKey(t)
	badKey := testKey(t)

	const issuer = "https://test.example.com"
	const clientID = "test-client"

	// Token signed with badKey, verified against goodKey.
	raw := makeIDToken(t, badKey, issuer, clientID, "sub1", "nonce1", time.Now().Add(time.Hour))

	verifier := buildVerifier(issuer, clientID, goodKey, []string{"RS256"})
	_, err := verifier.Verify(context.Background(), raw)
	require.Error(t, err, "id_token with bad signature must be rejected")
	assert.Contains(t, err.Error(), "failed to verify")
}

// TestFlow_WrongAudience_Rejected verifies threat #6 (AC-5): an id_token with
// an audience that does not match the configured client_id is rejected.
func TestFlow_WrongAudience_Rejected(t *testing.T) {
	key := testKey(t)
	const issuer = "https://test.example.com"
	const realClientID = "real-client"
	const otherClientID = "alien-client"

	// Token issued for otherClientID, verified against realClientID.
	raw := makeIDToken(t, key, issuer, otherClientID, "sub1", "nonce1", time.Now().Add(time.Hour))

	verifier := buildVerifier(issuer, realClientID, key, []string{"RS256"})
	_, err := verifier.Verify(context.Background(), raw)
	require.Error(t, err, "id_token with wrong audience must be rejected")
}

// TestFlow_ExpiredToken_Rejected verifies that an expired id_token is rejected.
func TestFlow_ExpiredToken_Rejected(t *testing.T) {
	key := testKey(t)
	const issuer = "https://test.example.com"
	const clientID = "test-client"

	// Token expired 1 minute ago.
	raw := makeIDToken(t, key, issuer, clientID, "sub1", "nonce1", time.Now().Add(-time.Minute))

	verifier := buildVerifier(issuer, clientID, key, []string{"RS256"})
	_, err := verifier.Verify(context.Background(), raw)
	require.Error(t, err, "expired id_token must be rejected")
}

// TestFlow_AlgNone_Rejected verifies threat #5 (AC-5): go-oidc rejects alg:none
// by default when SupportedSigningAlgs is pinned to RS256.
//
// go-oidc parses the header alg before consulting the keyset. If the alg is
// not in SupportedSigningAlgs, Verify returns an error before any key material
// is needed — so this test passes even without a real key lookup.
func TestFlow_AlgNone_Rejected(t *testing.T) {
	const issuer = "https://test.example.com"
	const clientID = "test-client"

	// Build an alg:none JWT manually using standard library base64url encoding.
	// encoding/base64.RawURLEncoding produces unpadded base64url — exactly what
	// JWT header/payload segments use.
	b64url := func(s string) string {
		return base64.RawURLEncoding.EncodeToString([]byte(s))
	}

	header := b64url(`{"alg":"none","typ":"JWT"}`)
	payload := b64url(`{"iss":"https://test.example.com","aud":"test-client","sub":"s","exp":9999999999}`)
	rawToken := header + "." + payload + "." // empty signature

	key := testKey(t)
	keySet := &gooidc.StaticKeySet{PublicKeys: []crypto.PublicKey{&key.PublicKey}}
	verifier := gooidc.NewVerifier(issuer, keySet, &gooidc.Config{
		ClientID:             clientID,
		SupportedSigningAlgs: []string{"RS256"},
	})

	_, err := verifier.Verify(context.Background(), rawToken)
	require.Error(t, err, "alg:none id_token must be rejected")
}

// TestFlow_ValidToken_ExtractsClaims verifies the happy path: a properly signed
// token with correct iss/aud/sub/nonce passes verification and claims are extracted.
func TestFlow_ValidToken_ExtractsClaims(t *testing.T) {
	key := testKey(t)
	const issuer = "https://test.example.com"
	const clientID = "test-client"
	const sub = "user-123"
	const nonce = "test-nonce-value"

	raw := makeIDToken(t, key, issuer, clientID, sub, nonce, time.Now().Add(time.Hour))

	verifier := buildVerifier(issuer, clientID, key, []string{"RS256"})
	idToken, err := verifier.Verify(context.Background(), raw)
	require.NoError(t, err)

	var claims struct {
		Sub   string `json:"sub"`
		Nonce string `json:"nonce"`
	}
	require.NoError(t, idToken.Claims(&claims))
	assert.Equal(t, sub, claims.Sub)
	// Nonce is also accessible via idToken.Nonce.
	assert.Equal(t, nonce, idToken.Nonce)
}

// ── Mock OIDC issuer integration tests ────────────────────────────────────────
// These tests use an httptest.Server that serves discovery + JWKS + token
// endpoints to exercise the full go-oidc discovery → NewProvider flow.

// mockIssuer bundles the mock server and the signing key.
type mockIssuer struct {
	server    *httptest.Server
	key       *rsa.PrivateKey
	issuerURL string
	clientID  string
}

// newMockIssuer starts a mock OIDC issuer that supports discovery and a minimal
// JWKS endpoint. The token endpoint is left to individual tests.
func newMockIssuer(t *testing.T, clientID string) *mockIssuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	mi := &mockIssuer{key: key, clientID: clientID}

	mux := http.NewServeMux()
	ts := httptest.NewServer(mux)
	mi.server = ts
	mi.issuerURL = ts.URL

	// Discovery endpoint: points all OIDC endpoints to this server.
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		doc := map[string]any{
			"issuer":                 ts.URL,
			"authorization_endpoint": ts.URL + "/authorize",
			"token_endpoint":         ts.URL + "/token",
			"jwks_uri":               ts.URL + "/jwks",
			"response_types_supported": []string{"code"},
			"subject_types_supported": []string{"public"},
			"id_token_signing_alg_values_supported": []string{"RS256"},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(doc)
	})

	// JWKS endpoint: exposes the RSA public key as a minimal JWK set.
	// go-oidc's NewRemoteKeySet fetches this to validate token signatures.
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		n := key.PublicKey.N.Bytes()
		e := []byte{0x01, 0x00, 0x01} // big-endian 65537

		jwks := map[string]any{
			"keys": []map[string]any{
				{
					"kty": "RSA",
					"use": "sig",
					"alg": "RS256",
					"kid": "test-key-1",
					"n":   base64.RawURLEncoding.EncodeToString(n),
					"e":   base64.RawURLEncoding.EncodeToString(e),
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(jwks)
	})

	t.Cleanup(ts.Close)
	return mi
}

// issueIDToken generates a signed ID token from the mock issuer.
func (mi *mockIssuer) issueIDToken(t *testing.T, sub, nonce string, exp time.Time) string {
	t.Helper()
	return makeIDToken(t, mi.key, mi.issuerURL, mi.clientID, sub, nonce, exp)
}

// TestProvider_Discovery_BuildsVerifier confirms that NewProvider can discover
// a mock issuer and build a functional verifier.
func TestProvider_Discovery_BuildsVerifier(t *testing.T) {
	const clientID = "test-client-disc"
	mi := newMockIssuer(t, clientID)

	// Reset singleton so this test's config is used.
	resetForTest()

	t.Setenv("OIDC_ISSUER_URL", mi.issuerURL)
	t.Setenv("OIDC_CLIENT_ID", clientID)
	t.Setenv("OIDC_CLIENT_SECRET", "secret")
	t.Setenv("OIDC_REDIRECT_URL", "http://localhost:7654/auth/callback")
	t.Setenv("OIDC_ID_TOKEN_SIGNING_ALGS", "RS256")
	t.Setenv("MCP_JWT_SECRET", "test-secret-for-disc-test-abc123")
	t.Cleanup(resetForTest)

	p, err := NewProvider(context.Background())
	require.NoError(t, err, "NewProvider must succeed against mock issuer")
	require.NotNil(t, p)

	// Verify a valid token to confirm the verifier is functional.
	raw := mi.issueIDToken(t, "user-1", "n1", time.Now().Add(time.Hour))
	id, err := p.verifier.Verify(context.Background(), raw)
	require.NoError(t, err)
	assert.Equal(t, "user-1", id.Subject)
}
