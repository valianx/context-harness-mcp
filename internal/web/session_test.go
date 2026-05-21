package web

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testSessionSecret = "test-secret-32-bytes-for-session"
	testSessionSub    = "550e8400-e29b-41d4-a716-446655440000"
)

// ── helpers ───────────────────────────────────────────────────────────────────

func setSessionSecret(t *testing.T) {
	t.Helper()
	t.Setenv("MCP_JWT_SECRET", testSessionSecret)
}

// issueToRequest issues a session cookie and returns a request that carries it,
// simulating the browser's cookie-jar behaviour after a Set-Cookie response.
func issueToRequest(t *testing.T, sub string) *http.Request {
	t.Helper()
	rr := httptest.NewRecorder()
	issueReq := httptest.NewRequest(http.MethodGet, "/", nil)
	require.NoError(t, Issue(rr, issueReq, sub), "Issue must succeed")

	cookies := rr.Result().Cookies()
	require.NotEmpty(t, cookies, "Issue must set at least one cookie")

	readReq := httptest.NewRequest(http.MethodGet, "/", nil)
	for _, c := range cookies {
		readReq.AddCookie(c)
	}
	return readReq
}

// signWithoutSeparator computes HMAC-SHA256 over message WITHOUT the
// domain-separation suffix. Used in TestSession_DomainSeparation to prove
// the suffix is required for validation.
func signWithoutSeparator(message, secret []byte) []byte {
	h := hmac.New(sha256.New, secret)
	h.Write(message)
	return h.Sum(nil)
}

// ── round-trip ────────────────────────────────────────────────────────────────

func TestSession_Roundtrip(t *testing.T) {
	setSessionSecret(t)

	readReq := issueToRequest(t, testSessionSub)

	sess, err := Read(readReq)
	require.NoError(t, err)
	assert.Equal(t, testSessionSub, sess.Sub, "Sub must round-trip unchanged")
	assert.WithinDuration(t, time.Now(), time.Unix(sess.IAT, 0), 5*time.Second)
	assert.WithinDuration(t, time.Now().Add(24*time.Hour), time.Unix(sess.Exp, 0), 5*time.Second)
}

// ── cookie attributes ─────────────────────────────────────────────────────────

func TestSession_CookieAttributes(t *testing.T) {
	setSessionSecret(t)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	require.NoError(t, Issue(rr, req, testSessionSub))

	cookies := rr.Result().Cookies()
	require.Len(t, cookies, 1)

	c := cookies[0]
	assert.Equal(t, sessionCookieName, c.Name)
	assert.True(t, c.HttpOnly, "HttpOnly must be set")
	assert.Equal(t, "/", c.Path)
	assert.Equal(t, sessionMaxAge, c.MaxAge)

	// SameSite=Strict is encoded in the raw Set-Cookie header.
	assert.Contains(t, rr.Header().Get("Set-Cookie"), "SameSite=Strict")
}

// ── missing cookie ────────────────────────────────────────────────────────────

func TestSession_MissingCookie(t *testing.T) {
	setSessionSecret(t)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	sess, err := Read(req)
	assert.Nil(t, sess)
	assert.ErrorIs(t, err, ErrInvalidSession)
}

// ── expired session ───────────────────────────────────────────────────────────

func TestSession_Expired(t *testing.T) {
	setSessionSecret(t)

	secret := []byte(testSessionSecret)
	p := sessionPayload{
		Sub: testSessionSub,
		IAT: time.Now().Add(-48 * time.Hour).Unix(),
		Exp: time.Now().Add(-1 * time.Second).Unix(),
	}
	value, err := encodeSession(p, secret)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: value})

	sess, err := Read(req)
	assert.Nil(t, sess)
	assert.ErrorIs(t, err, ErrInvalidSession, "expired session must return ErrInvalidSession")
}

// ── tampered payload ──────────────────────────────────────────────────────────

func TestSession_TamperedPayload(t *testing.T) {
	setSessionSecret(t)

	// Issue a valid session.
	rr := httptest.NewRecorder()
	reqOut := httptest.NewRequest(http.MethodGet, "/", nil)
	require.NoError(t, Issue(rr, reqOut, testSessionSub))

	cookieVal := rr.Result().Cookies()[0].Value
	dot := strings.LastIndex(cookieVal, ".")
	require.Greater(t, dot, 0)

	sig := cookieVal[dot:] // includes the leading "."

	// Build an alternate payload with a different sub.
	altPayload := sessionPayload{
		Sub: "evil-sub-aaaaaaaaaaaaaaaa",
		IAT: time.Now().Unix(),
		Exp: time.Now().Add(24 * time.Hour).Unix(),
	}
	altRaw, _ := json.Marshal(altPayload)
	altEncoded := base64.RawURLEncoding.EncodeToString(altRaw)

	// Reuse the original signature — HMAC check must reject this.
	tampered := altEncoded + sig

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: tampered})

	sess, err := Read(req)
	assert.Nil(t, sess)
	assert.ErrorIs(t, err, ErrInvalidSession, "tampered payload must be rejected")
}

// ── malformed cookie ──────────────────────────────────────────────────────────

func TestSession_Malformed_NoDot(t *testing.T) {
	setSessionSecret(t)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "nodothere"})

	sess, err := Read(req)
	assert.Nil(t, sess)
	assert.ErrorIs(t, err, ErrInvalidSession)
}

func TestSession_Malformed_BadBase64(t *testing.T) {
	setSessionSecret(t)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{
		Name:  sessionCookieName,
		Value: "!notbase64!." + hex.EncodeToString(make([]byte, 32)),
	})

	sess, err := Read(req)
	assert.Nil(t, sess)
	assert.ErrorIs(t, err, ErrInvalidSession)
}

func TestSession_Malformed_BadJSON(t *testing.T) {
	setSessionSecret(t)

	secret := []byte(testSessionSecret)
	notJSON := []byte("this-is-not-json")
	encoded := base64.RawURLEncoding.EncodeToString(notJSON)
	mac := computeHMAC([]byte(encoded), secret)
	value := encoded + "." + hex.EncodeToString(mac)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: value})

	sess, err := Read(req)
	assert.Nil(t, sess)
	assert.ErrorIs(t, err, ErrInvalidSession)
}

// ── domain-separation safety ──────────────────────────────────────────────────

// TestSession_DomainSeparation proves that a signature computed over the same
// payload WITHOUT the \x00ch_session_v1 suffix is rejected. This prevents a
// future maintainer from accidentally accepting a JWT HMAC as a cookie HMAC —
// both credentials share MCP_JWT_SECRET, so the suffix is the only structural
// guard against cross-protocol confusion.
func TestSession_DomainSeparation(t *testing.T) {
	setSessionSecret(t)

	secret := []byte(testSessionSecret)
	p := sessionPayload{
		Sub: testSessionSub,
		IAT: time.Now().Unix(),
		Exp: time.Now().Add(24 * time.Hour).Unix(),
	}
	raw, _ := json.Marshal(p)
	encoded := base64.RawURLEncoding.EncodeToString(raw)

	// Sign WITHOUT the domain-separation suffix.
	badSig := signWithoutSeparator([]byte(encoded), secret)
	valueBadSig := encoded + "." + hex.EncodeToString(badSig)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: valueBadSig})

	sess, err := Read(req)
	assert.Nil(t, sess, "signature without domain separator must be rejected")
	assert.ErrorIs(t, err, ErrInvalidSession)
}

// ── no secret ────────────────────────────────────────────────────────────────

func TestSession_NoSecret_IssueErrors(t *testing.T) {
	t.Setenv("MCP_JWT_SECRET", "")

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	err := Issue(rr, req, testSessionSub)
	assert.Error(t, err, "Issue must fail when MCP_JWT_SECRET is empty")
}

func TestSession_NoSecret_ReadErrors(t *testing.T) {
	// Issue with a valid secret.
	t.Setenv("MCP_JWT_SECRET", testSessionSecret)
	rr := httptest.NewRecorder()
	reqIssue := httptest.NewRequest(http.MethodGet, "/", nil)
	require.NoError(t, Issue(rr, reqIssue, testSessionSub))

	cookies := rr.Result().Cookies()
	require.NotEmpty(t, cookies)

	// Clear the secret — Read must now fail gracefully.
	t.Setenv("MCP_JWT_SECRET", "")

	readReq := httptest.NewRequest(http.MethodGet, "/", nil)
	for _, c := range cookies {
		readReq.AddCookie(c)
	}

	sess, err := Read(readReq)
	assert.Nil(t, sess)
	assert.ErrorIs(t, err, ErrInvalidSession)
}

// ── clear ─────────────────────────────────────────────────────────────────────

func TestSession_Clear(t *testing.T) {
	setSessionSecret(t)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	Clear(rr, req)

	cookies := rr.Result().Cookies()
	require.Len(t, cookies, 1, "Clear must write exactly one cookie")

	c := cookies[0]
	assert.Equal(t, sessionCookieName, c.Name)
	assert.Equal(t, "", c.Value)
	assert.Equal(t, 0, c.MaxAge, "MaxAge=0 instructs the browser to delete the cookie")
}

// ── TLS detection ─────────────────────────────────────────────────────────────

func TestIsHTTPS_ForwardedProto(t *testing.T) {
	t.Setenv("MCP_PUBLIC_URL", "")
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	assert.True(t, isHTTPS(req))
}

func TestIsHTTPS_EnvVar(t *testing.T) {
	t.Setenv("MCP_PUBLIC_URL", "https://myapp.example.com")
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	assert.True(t, isHTTPS(req))
}

func TestIsHTTPS_HTTP_EnvVar(t *testing.T) {
	t.Setenv("MCP_PUBLIC_URL", "http://localhost:7654")
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	assert.False(t, isHTTPS(req))
}

func TestIsHTTPS_NoSignal(t *testing.T) {
	t.Setenv("MCP_PUBLIC_URL", "")
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	assert.False(t, isHTTPS(req))
}
