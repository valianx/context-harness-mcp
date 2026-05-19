package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// jsonUnmarshal is a local alias to avoid importing encoding/json in multiple places.
func jsonUnmarshal(data []byte, v any) error {
	return json.Unmarshal(data, v)
}

// ── fake RevocationStore ──────────────────────────────────────────────────────

// fakeRevocationStore is a test double for RevocationStore that records how many
// times GetRevoked was called (for cache-hit verification) and allows the caller
// to control the revoked/error state.
type fakeRevocationStore struct {
	callCount int
	revoked   bool
	err       error
}

func (f *fakeRevocationStore) GetRevoked(_ string) (bool, error) {
	f.callCount++
	return f.revoked, f.err
}

// ── token factories ───────────────────────────────────────────────────────────

// validToken issues a fresh HS256 token with the given sub and email using the
// test secret. The token is valid for 1 hour.
func validToken(t *testing.T, sub, email string) string {
	t.Helper()
	t.Setenv("MCP_JWT_SECRET", testSecret)
	t.Setenv("MCP_JWT_ISSUER", testIssuer)
	t.Setenv("MCP_JWT_EXPIRY", "1h")

	tok, err := IssueMCPToken(sub, email)
	require.NoError(t, err, "validToken helper: IssueMCPToken must succeed")
	return tok
}

// expiredToken returns a valid HS256 token whose exp is already in the past.
func expiredToken(t *testing.T) string {
	t.Helper()
	secret := []byte(testSecret)
	now := time.Now()
	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   testSub,
			Issuer:    testIssuer,
			IssuedAt:  jwt.NewNumericDate(now.Add(-2 * time.Second)),
			ExpiresAt: jwt.NewNumericDate(now.Add(-1 * time.Second)),
		},
		Email: testEmail,
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	s, err := tok.SignedString(secret)
	require.NoError(t, err)
	return s
}

// newMiddlewareRequest creates a GET request to /mcp optionally setting the
// Authorization header to "Bearer <token>".
func newMiddlewareRequest(token string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	return r
}

// echoHandler is a trivial handler that records what it received from ctx.
// It writes 200 OK and sets X-Sub and X-Email response headers for inspection.
func echoHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sub := UserIDFromContext(r.Context())
		email := EmailFromContext(r.Context())
		w.Header().Set("X-Sub", sub)
		w.Header().Set("X-Email", email)
		w.WriteHeader(http.StatusOK)
	}
}

// ── AC-5: valid request carries sub and email in ctx ─────────────────────────

// TestMiddleware_ValidToken verifies that a valid token causes the wrapped handler
// to be called with sub and email in context.
//
// AC-5: Given valid bearer, When Middleware runs, Then ctx carries sub and email.
func TestMiddleware_ValidToken(t *testing.T) {
	t.Setenv("MCP_JWT_SECRET", testSecret)
	t.Setenv("MCP_JWT_ISSUER", testIssuer)

	store := &fakeRevocationStore{revoked: false}
	cache := &RevocationCache{entries: make(map[string]*cacheEntry)}
	tok := validToken(t, testSub, testEmail)

	handler := Middleware(ModeEnabled, store, cache, "https://test", echoHandler())
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, newMiddlewareRequest(tok))

	assert.Equal(t, http.StatusOK, w.Code, "valid bearer must pass through")
	assert.Equal(t, testSub, w.Header().Get("X-Sub"), "ctx must carry sub")
	assert.Equal(t, testEmail, w.Header().Get("X-Email"), "ctx must carry email")
}

// ── AC-7: missing Authorization → 401 unauthenticated ────────────────────────

// TestMiddleware_MissingBearer verifies that a request without Authorization
// header is rejected with 401 auth/unauthenticated.
//
// AC-7: Given no Authorization header, When Middleware runs, Then 401 auth/unauthenticated.
func TestMiddleware_MissingBearer(t *testing.T) {
	t.Setenv("MCP_JWT_SECRET", testSecret)

	store := &fakeRevocationStore{}
	cache := &RevocationCache{entries: make(map[string]*cacheEntry)}

	handler := Middleware(ModeEnabled, store, cache, "https://test", echoHandler())
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, newMiddlewareRequest(""))

	assert.Equal(t, http.StatusUnauthorized, w.Code, "missing bearer must return 401")
	assertErrorBody(t, w, CodeUnauthenticated)
}

// TestMiddleware_MalformedBearer verifies that "Bearer " with no token is rejected.
func TestMiddleware_MalformedBearer(t *testing.T) {
	t.Setenv("MCP_JWT_SECRET", testSecret)

	store := &fakeRevocationStore{}
	cache := &RevocationCache{entries: make(map[string]*cacheEntry)}

	r := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	r.Header.Set("Authorization", "Bearer ") // space but no token

	handler := Middleware(ModeEnabled, store, cache, "https://test", echoHandler())
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// TestMiddleware_WrongScheme verifies "Token xyz" (not Bearer) is rejected.
func TestMiddleware_WrongScheme(t *testing.T) {
	t.Setenv("MCP_JWT_SECRET", testSecret)

	store := &fakeRevocationStore{}
	cache := &RevocationCache{entries: make(map[string]*cacheEntry)}

	r := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	r.Header.Set("Authorization", "Token some-value")

	handler := Middleware(ModeEnabled, store, cache, "https://test", echoHandler())
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// ── AC-8: expired JWT → 401 auth/expired ─────────────────────────────────────

// TestMiddleware_ExpiredToken verifies that an expired bearer is rejected with
// 401 auth/expired.
//
// AC-8: Given expired JWT, When Middleware runs, Then 401 auth/expired.
func TestMiddleware_ExpiredToken(t *testing.T) {
	t.Setenv("MCP_JWT_SECRET", testSecret)
	t.Setenv("MCP_JWT_ISSUER", testIssuer)

	store := &fakeRevocationStore{}
	cache := &RevocationCache{entries: make(map[string]*cacheEntry)}
	tok := expiredToken(t)

	handler := Middleware(ModeEnabled, store, cache, "https://test", echoHandler())
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, newMiddlewareRequest(tok))

	assert.Equal(t, http.StatusUnauthorized, w.Code, "expired token must return 401")
	assertErrorBody(t, w, CodeExpired)
}

// ── AC-6: revoked user → 403 ─────────────────────────────────────────────────

// TestMiddleware_RevokedUser verifies that a valid token for a revoked user
// is rejected with 403 auth/revoked.
//
// AC-6: Given valid JWT but user revoked, When Middleware runs, Then 403 auth/revoked.
func TestMiddleware_RevokedUser(t *testing.T) {
	t.Setenv("MCP_JWT_SECRET", testSecret)
	t.Setenv("MCP_JWT_ISSUER", testIssuer)

	store := &fakeRevocationStore{revoked: true}
	cache := &RevocationCache{entries: make(map[string]*cacheEntry)}
	tok := validToken(t, testSub, testEmail)

	handler := Middleware(ModeEnabled, store, cache, "https://test", echoHandler())
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, newMiddlewareRequest(tok))

	assert.Equal(t, http.StatusForbidden, w.Code, "revoked user must return 403")
	assertErrorBody(t, w, CodeRevoked)
}

// ── AC-6: cache hit — DB called exactly once for N requests within TTL ────────

// TestMiddleware_CacheHit verifies that 5 requests with the same valid token
// within the TTL window result in exactly 1 DB call (cache hit on calls 2-5).
//
// AC-6: Given 5 identical requests within 1h TTL, Then DB called exactly once.
func TestMiddleware_CacheHit(t *testing.T) {
	t.Setenv("MCP_JWT_SECRET", testSecret)
	t.Setenv("MCP_JWT_ISSUER", testIssuer)

	store := &fakeRevocationStore{revoked: false}
	cache := &RevocationCache{entries: make(map[string]*cacheEntry)}
	tok := validToken(t, testSub, testEmail)

	handler := Middleware(ModeEnabled, store, cache, "https://test", echoHandler())

	const numRequests = 5
	for i := 0; i < numRequests; i++ {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, newMiddlewareRequest(tok))
		assert.Equal(t, http.StatusOK, w.Code, "request %d must succeed", i+1)
	}

	assert.Equal(t, 1, store.callCount,
		"DB must be called exactly once across %d requests within TTL", numRequests)
}

// TestMiddleware_CacheHit_Revoked verifies that a revoked entry in cache also
// uses the cache (DB called once even for revoked + repeated requests).
func TestMiddleware_CacheHit_Revoked(t *testing.T) {
	t.Setenv("MCP_JWT_SECRET", testSecret)
	t.Setenv("MCP_JWT_ISSUER", testIssuer)

	store := &fakeRevocationStore{revoked: true}
	cache := &RevocationCache{entries: make(map[string]*cacheEntry)}
	tok := validToken(t, testSub, testEmail)

	handler := Middleware(ModeEnabled, store, cache, "https://test", echoHandler())

	for i := 0; i < 5; i++ {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, newMiddlewareRequest(tok))
		assert.Equal(t, http.StatusForbidden, w.Code)
	}

	assert.Equal(t, 1, store.callCount,
		"revoked state must also be cached; DB must not be called more than once")
}

// ── AC-11: ModeNone is a full pass-through ────────────────────────────────────

// TestMiddleware_ModeNone verifies that when mode is ModeNone, the middleware
// is a no-op: no 401, no 403, handler is called unconditionally.
//
// AC-11: Given MCP_AUTH=none, When request has no bearer, Then 200 (pass-through).
func TestMiddleware_ModeNone(t *testing.T) {
	store := &fakeRevocationStore{}
	cache := &RevocationCache{entries: make(map[string]*cacheEntry)}

	handler := Middleware(ModeNone, store, cache, "https://test", echoHandler())
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, newMiddlewareRequest(""))

	assert.Equal(t, http.StatusOK, w.Code,
		"ModeNone must let unauthenticated requests through")
	// DB must never be called in pass-through mode.
	assert.Equal(t, 0, store.callCount,
		"ModeNone must not query the revocation store")
}

// ── AC-12: invalid bearer + oversized payload → 401, not 413 ────────────────

// TestMiddleware_InvalidTokenBeforePayload verifies that the middleware rejects
// an invalid bearer before the handler (simulated payload) is invoked.
// This is the unit-level assertion for the ordering guarantee.
//
// AC-12: Given invalid bearer, Then 401 returned before handler runs.
func TestMiddleware_InvalidTokenBeforePayload(t *testing.T) {
	t.Setenv("MCP_JWT_SECRET", testSecret)

	store := &fakeRevocationStore{}
	cache := &RevocationCache{entries: make(map[string]*cacheEntry)}

	handlerCalled := false
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		handlerCalled = true
		w.WriteHeader(http.StatusRequestEntityTooLarge) // simulates Content Filter 413
	})

	handler := Middleware(ModeEnabled, store, cache, "https://test", inner)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, newMiddlewareRequest("not.a.valid.token"))

	assert.Equal(t, http.StatusUnauthorized, w.Code,
		"invalid token must return 401, not the handler's 413")
	assert.False(t, handlerCalled,
		"inner handler must NOT be called when auth rejects the request")
}

// ── helper assertions ─────────────────────────────────────────────────────────

// assertErrorBody decodes the response body and asserts the code field matches
// expectedCode and that layer == "auth".
func assertErrorBody(t *testing.T, w *httptest.ResponseRecorder, expectedCode string) {
	t.Helper()

	var body errorResponse
	require.NoError(t, jsonUnmarshal(w.Body.Bytes(), &body),
		"response body must be valid JSON")
	assert.Equal(t, expectedCode, body.Code,
		"error code must be %q", expectedCode)
	assert.Equal(t, "auth", body.Layer, "layer must always be 'auth'")
}
