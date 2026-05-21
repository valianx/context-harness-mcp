package web

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	sessionCookieName = "ch_session"
	sessionMaxAge     = 86400 // 24 hours in seconds

	// domainSeparator is appended to the HMAC input to prevent cross-protocol
	// confusion with MCP JWT signatures. Both credentials share MCP_JWT_SECRET,
	// so the domain suffix ensures a JWT signature cannot validate as a cookie HMAC.
	// This byte sequence was chosen to be invalid as a UTF-8 continuation and
	// distinct from any JWT separator (".").
	domainSeparator = "\x00ch_session_v1"
)

// ErrInvalidSession is returned by Read when the cookie is missing, expired,
// or has a bad HMAC. Callers should treat it as "unauthenticated" and redirect.
var ErrInvalidSession = errors.New("invalid or expired session")

// Session holds the decoded payload from the ch_session cookie.
type Session struct {
	Sub string `json:"sub"`
	IAT int64  `json:"iat"`
	Exp int64  `json:"exp"`
}

// sessionPayload mirrors Session for JSON encoding/decoding.
type sessionPayload struct {
	Sub string `json:"sub"`
	IAT int64  `json:"iat"`
	Exp int64  `json:"exp"`
}

// Issue creates a new session cookie for the given subject (Supabase user UUID)
// and writes it onto w. The cookie is valid for 24 hours.
// Returns an error only if MCP_JWT_SECRET is not set.
func Issue(w http.ResponseWriter, r *http.Request, sub string) error {
	secret, err := sessionSecret()
	if err != nil {
		return err
	}

	now := time.Now().Unix()
	payload := sessionPayload{
		Sub: sub,
		IAT: now,
		Exp: now + sessionMaxAge,
	}

	cookieValue, err := encodeSession(payload, secret)
	if err != nil {
		return fmt.Errorf("session: encode failed: %w", err)
	}

	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    cookieValue,
		Path:     "/",
		MaxAge:   sessionMaxAge,
		HttpOnly: true,
		Secure:   isHTTPS(r),
		SameSite: http.SameSiteStrictMode,
	})
	return nil
}

// Read extracts and validates the ch_session cookie from r.
// Returns (nil, ErrInvalidSession) when: cookie is absent, HMAC is wrong,
// payload is malformed, or the session has expired.
func Read(r *http.Request) (*Session, error) {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil {
		return nil, ErrInvalidSession
	}

	secret, err := sessionSecret()
	if err != nil {
		return nil, ErrInvalidSession
	}

	payload, err := decodeSession(cookie.Value, secret)
	if err != nil {
		return nil, ErrInvalidSession
	}

	if time.Now().Unix() > payload.Exp {
		return nil, ErrInvalidSession
	}

	return &Session{
		Sub: payload.Sub,
		IAT: payload.IAT,
		Exp: payload.Exp,
	}, nil
}

// Clear writes a Set-Cookie header that immediately expires the ch_session cookie.
// It does not return an error — clearing always succeeds from the HTTP layer's
// perspective even if the cookie was never set.
func Clear(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   0,
		HttpOnly: true,
		Secure:   isHTTPS(r),
		SameSite: http.SameSiteStrictMode,
	})
}

// encodeSession encodes payload as base64url(json).hex(hmac) — the cookie wire format.
func encodeSession(p sessionPayload, secret []byte) (string, error) {
	raw, err := json.Marshal(p)
	if err != nil {
		return "", err
	}

	encoded := base64.RawURLEncoding.EncodeToString(raw)
	mac := computeHMAC([]byte(encoded), secret)
	return encoded + "." + hex.EncodeToString(mac), nil
}

// decodeSession parses and verifies the cookie value produced by encodeSession.
// Returns an error for any structural or HMAC failure.
func decodeSession(value string, secret []byte) (sessionPayload, error) {
	dot := strings.LastIndex(value, ".")
	if dot < 0 {
		return sessionPayload{}, errors.New("session: missing separator")
	}

	encodedPayload := value[:dot]
	sigHex := value[dot+1:]

	sigBytes, err := hex.DecodeString(sigHex)
	if err != nil {
		return sessionPayload{}, fmt.Errorf("session: bad signature hex: %w", err)
	}

	expected := computeHMAC([]byte(encodedPayload), secret)
	// Constant-time compare prevents timing-based HMAC oracle attacks.
	if !hmac.Equal(expected, sigBytes) {
		return sessionPayload{}, errors.New("session: HMAC mismatch")
	}

	rawPayload, err := base64.RawURLEncoding.DecodeString(encodedPayload)
	if err != nil {
		return sessionPayload{}, fmt.Errorf("session: bad base64: %w", err)
	}

	var p sessionPayload
	if err := json.Unmarshal(rawPayload, &p); err != nil {
		return sessionPayload{}, fmt.Errorf("session: bad JSON: %w", err)
	}
	return p, nil
}

// computeHMAC computes HMAC-SHA256 over message with the domain-separation
// suffix appended. The suffix prevents a JWT signature (which uses the same
// MCP_JWT_SECRET) from validating as a session cookie HMAC.
func computeHMAC(message, secret []byte) []byte {
	h := hmac.New(sha256.New, secret)
	h.Write(message)
	h.Write([]byte(domainSeparator))
	return h.Sum(nil)
}

// isHTTPS returns true when the request is served over HTTPS. Checks in order:
// r.TLS (direct TLS termination), X-Forwarded-Proto header (reverse proxy),
// and MCP_PUBLIC_URL prefix (deploy-config driven). OR semantics: any single
// positive signal is sufficient. This avoids false negatives in prod.
func isHTTPS(r *http.Request) bool {
	if r != nil && r.TLS != nil {
		return true
	}
	if r != nil && r.Header.Get("X-Forwarded-Proto") == "https" {
		return true
	}
	return strings.HasPrefix(os.Getenv("MCP_PUBLIC_URL"), "https://")
}

// sessionSecret reads MCP_JWT_SECRET from the environment.
func sessionSecret() ([]byte, error) {
	s := os.Getenv("MCP_JWT_SECRET")
	if s == "" {
		return nil, fmt.Errorf("session: MCP_JWT_SECRET is not set")
	}
	return []byte(s), nil
}
