package oidc

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	// oidcCookieName is the name of the ephemeral state cookie.
	oidcCookieName = "ch_oidc"

	// oidcCookieMaxAge is the lifetime of the ephemeral state cookie in seconds.
	// 10 minutes is sufficient for a human to complete the IdP authentication flow.
	oidcCookieMaxAge = 600

	// oidcDomainSeparator is appended to HMAC inputs to prevent cross-protocol
	// confusion with the MCP JWT and ch_session signatures (all share MCP_JWT_SECRET).
	// The \x00 prefix is intentionally invalid UTF-8 to prevent collision with
	// any legitimate string suffix.
	oidcDomainSeparator = "\x00ch_oidc_v1"

	// nonceLen is the number of random bytes for state and nonce values.
	// 32 bytes = 256 bits of entropy — sufficient per RFC 9700 §2.1.
	nonceLen = 32
)

// ErrInvalidOIDCState is returned when the state cookie is absent,
// tampered, expired, or structurally invalid.
var ErrInvalidOIDCState = errors.New("invalid or expired OIDC state cookie")

// stateBundle holds the values bound to the ephemeral OIDC state cookie.
type stateBundle struct {
	State    string `json:"state"`
	Nonce    string `json:"nonce"`
	PKCE     string `json:"pkce"`
	Next     string `json:"next,omitempty"`
	IssuedAt int64  `json:"iat"`
}

// GenerateState creates cryptographically random state, nonce, and PKCE verifier
// values. All three are 32 random bytes encoded as hex strings.
func GenerateState() (state, nonce, pkce string, err error) {
	state, err = randomHex(nonceLen)
	if err != nil {
		return "", "", "", fmt.Errorf("oidc: generate state: %w", err)
	}
	nonce, err = randomHex(nonceLen)
	if err != nil {
		return "", "", "", fmt.Errorf("oidc: generate nonce: %w", err)
	}
	pkce, err = randomHex(nonceLen)
	if err != nil {
		return "", "", "", fmt.Errorf("oidc: generate pkce verifier: %w", err)
	}
	return state, nonce, pkce, nil
}

// IssueStateCookie binds state, nonce, pkce, and next into an HMAC-signed
// cookie set on w.
//
// Security (threat #10 — CWE-384):
//   - HttpOnly: cookie not readable by JavaScript.
//   - SameSite=Lax: allows the cross-site top-level redirect from the IdP callback.
//     (Strict would block the callback redirect because it is cross-site navigation.)
//   - Secure: conditional on HTTPS (same logic as ch_session).
//   - Max-Age=600: expires in 10 minutes — single use.
//   - HMAC-signed: prevents tampering without the server's secret.
//   - Domain separator \x00ch_oidc_v1: prevents cross-protocol confusion with
//     ch_session (\x00ch_session_v1) and MCP JWT signatures.
func IssueStateCookie(w http.ResponseWriter, r *http.Request, state, nonce, pkce, next string) error {
	secret, err := oidcSecret()
	if err != nil {
		return err
	}

	bundle := stateBundle{
		State:    state,
		Nonce:    nonce,
		PKCE:     pkce,
		Next:     next,
		IssuedAt: time.Now().Unix(),
	}

	value, err := encodeBundle(bundle, secret)
	if err != nil {
		return fmt.Errorf("oidc: encode state cookie: %w", err)
	}

	http.SetCookie(w, &http.Cookie{
		Name:     oidcCookieName,
		Value:    value,
		Path:     "/",
		MaxAge:   oidcCookieMaxAge,
		HttpOnly: true,
		Secure:   isHTTPS(r),
		SameSite: http.SameSiteLaxMode,
	})
	return nil
}

// ReadStateCookie reads and verifies the ephemeral state cookie from r.
// Returns ErrInvalidOIDCState for any missing, tampered, or expired cookie.
func ReadStateCookie(r *http.Request) (state, nonce, pkce, next string, err error) {
	cookie, err := r.Cookie(oidcCookieName)
	if err != nil {
		return "", "", "", "", ErrInvalidOIDCState
	}

	secret, err := oidcSecret()
	if err != nil {
		return "", "", "", "", ErrInvalidOIDCState
	}

	bundle, err := decodeBundle(cookie.Value, secret)
	if err != nil {
		return "", "", "", "", ErrInvalidOIDCState
	}

	if time.Now().Unix() > bundle.IssuedAt+oidcCookieMaxAge {
		return "", "", "", "", ErrInvalidOIDCState
	}

	return bundle.State, bundle.Nonce, bundle.PKCE, bundle.Next, nil
}

// ClearStateCookie writes a Set-Cookie header that immediately expires the
// ch_oidc cookie. Called after a successful or failed callback to enforce
// single-use semantics (threat #10).
func ClearStateCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     oidcCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   0,
		HttpOnly: true,
		Secure:   isHTTPS(r),
		SameSite: http.SameSiteLaxMode,
	})
}

// encodeBundle serializes bundle as base64url(json).hex(hmac).
func encodeBundle(b stateBundle, secret []byte) (string, error) {
	raw, err := json.Marshal(b)
	if err != nil {
		return "", err
	}

	encoded := base64.RawURLEncoding.EncodeToString(raw)
	mac := computeOIDCHMAC([]byte(encoded), secret)
	return encoded + "." + hex.EncodeToString(mac), nil
}

// decodeBundle parses and verifies a cookie value produced by encodeBundle.
func decodeBundle(value string, secret []byte) (stateBundle, error) {
	dot := strings.LastIndex(value, ".")
	if dot < 0 {
		return stateBundle{}, errors.New("oidc: missing separator in state cookie")
	}

	encodedPayload := value[:dot]
	sigHex := value[dot+1:]

	sigBytes, err := hex.DecodeString(sigHex)
	if err != nil {
		return stateBundle{}, fmt.Errorf("oidc: bad signature hex: %w", err)
	}

	expected := computeOIDCHMAC([]byte(encodedPayload), secret)
	// Constant-time compare prevents timing-based HMAC oracle attacks.
	if !hmac.Equal(expected, sigBytes) {
		return stateBundle{}, errors.New("oidc: HMAC mismatch in state cookie")
	}

	rawPayload, err := base64.RawURLEncoding.DecodeString(encodedPayload)
	if err != nil {
		return stateBundle{}, fmt.Errorf("oidc: bad base64 in state cookie: %w", err)
	}

	var b stateBundle
	if err := json.Unmarshal(rawPayload, &b); err != nil {
		return stateBundle{}, fmt.Errorf("oidc: bad JSON in state cookie: %w", err)
	}
	return b, nil
}

// computeOIDCHMAC computes HMAC-SHA256 over message with the OIDC domain
// separator appended. The separator prevents cross-protocol confusion with the
// ch_session cookie HMAC (\x00ch_session_v1) and MCP JWT signatures.
func computeOIDCHMAC(message, secret []byte) []byte {
	h := hmac.New(sha256.New, secret)
	h.Write(message)
	h.Write([]byte(oidcDomainSeparator))
	return h.Sum(nil)
}

// randomHex generates n random bytes and returns them as a lowercase hex string.
func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// oidcSecret reads MCP_JWT_SECRET from the environment for use as the OIDC
// cookie HMAC key. The domain separator ensures the same key material cannot
// be used across protocol boundaries.
func oidcSecret() ([]byte, error) {
	s := os.Getenv("MCP_JWT_SECRET")
	if s == "" {
		return nil, fmt.Errorf("oidc: MCP_JWT_SECRET is not set")
	}
	return []byte(s), nil
}

// isHTTPS returns true for non-localhost deployments where cookies must carry
// the Secure flag. The decision is based on static config signals (the public
// URL and the OIDC redirect URL) rather than per-request TLS state, which is
// nil behind TLS-terminating proxies (Railway, Render, Fly) and would
// incorrectly omit the Secure flag in production (fix: SEC-002, CWE-614).
//
// Rule: Secure=true whenever the deployment is NOT local development, i.e.
// when MCP_PUBLIC_URL starts with https:// or the OIDC redirect URL has a
// non-localhost hostname. This mirrors the signal that OIDCConfig.Validate()
// already uses to enforce HTTPS on the redirect URI.
func isHTTPS(_ *http.Request) bool {
	if strings.HasPrefix(os.Getenv("MCP_PUBLIC_URL"), "https://") {
		return true
	}
	// Fall back to the OIDC redirect URL if MCP_PUBLIC_URL is not set.
	redirectURL := os.Getenv("OIDC_REDIRECT_URL")
	if redirectURL == "" {
		publicURL := strings.TrimRight(os.Getenv("MCP_PUBLIC_URL"), "/")
		if publicURL != "" {
			redirectURL = publicURL + "/auth/callback"
		}
	}
	if redirectURL != "" && !isLocalhostURL(redirectURL) {
		return true
	}
	return false
}

// isLocalhostURL returns true when the URL's hostname is exactly "localhost",
// "127.0.0.1", or "::1". Duplicated from config.go to keep packages
// independent (state.go must not import config.go types to avoid cycles).
func isLocalhostURL(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	host := u.Hostname()
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}
