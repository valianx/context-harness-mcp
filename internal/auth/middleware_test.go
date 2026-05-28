package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
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

// ── AC-C.3: authenticated span carries user.id ────────────────────────────────

// TestMiddleware_SpanAttributes_UserID verifies that after successful auth the
// active span receives a user.id attribute equal to the JWT subject (AC-C.3).
// When mode is ModeNone (unauthenticated path) the span must NOT have user.id.
func TestMiddleware_SpanAttributes_UserID(t *testing.T) {
	t.Setenv("MCP_JWT_SECRET", testSecret)
	t.Setenv("MCP_JWT_ISSUER", testIssuer)

	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))

	store := &fakeRevocationStore{revoked: false}
	cache := &RevocationCache{entries: make(map[string]*cacheEntry)}
	tok := validToken(t, testSub, testEmail)

	handler := Middleware(ModeEnabled, store, cache, "https://test", echoHandler())

	// Simulate otelhttp wrapping: start a parent span, inject into ctx, call middleware.
	ctx, parentSpan := tp.Tracer("test").Start(context.Background(), "http.request")
	defer parentSpan.End()

	r := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	r = r.WithContext(ctx)
	r.Header.Set("Authorization", "Bearer "+tok)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	require.Equal(t, http.StatusOK, w.Code)

	// The middleware sets attributes on the span from context — which is parentSpan.
	parentSpan.End()
	spans := sr.Ended()
	require.Len(t, spans, 1, "exactly one span must have been recorded")

	attrMap := spanAttrMap(spans[0])
	assert.Equal(t, testSub, attrMap["user.id"],
		"AC-C.3: authenticated span must carry user.id equal to JWT sub")
}

// TestMiddleware_SpanAttributes_UserID_ModeNone verifies that when Middleware is
// in ModeNone (no auth), no user.id attribute is set on the span (AC-C.3).
func TestMiddleware_SpanAttributes_UserID_ModeNone(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))

	store := &fakeRevocationStore{}
	cache := &RevocationCache{entries: make(map[string]*cacheEntry)}

	handler := Middleware(ModeNone, store, cache, "https://test", echoHandler())

	ctx, parentSpan := tp.Tracer("test").Start(context.Background(), "http.request")
	defer parentSpan.End()

	r := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	r = r.WithContext(ctx)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	require.Equal(t, http.StatusOK, w.Code)

	parentSpan.End()
	spans := sr.Ended()
	require.Len(t, spans, 1)

	attrMap := spanAttrMap(spans[0])
	_, hasUserID := attrMap["user.id"]
	assert.False(t, hasUserID,
		"AC-C.3: unauthenticated (ModeNone) span must NOT carry user.id")
}

// ── AC-C.4: auth.cache_result attribute reflects cache hit or miss ─────────────

// TestMiddleware_SpanAttributes_CacheResult_Miss verifies that the first request
// for a sub (DB lookup) sets auth.cache_result=miss on the span (AC-C.4).
func TestMiddleware_SpanAttributes_CacheResult_Miss(t *testing.T) {
	t.Setenv("MCP_JWT_SECRET", testSecret)
	t.Setenv("MCP_JWT_ISSUER", testIssuer)

	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))

	store := &fakeRevocationStore{revoked: false}
	cache := &RevocationCache{entries: make(map[string]*cacheEntry)}
	tok := validToken(t, testSub, testEmail)

	handler := Middleware(ModeEnabled, store, cache, "https://test", echoHandler())

	ctx, parentSpan := tp.Tracer("test").Start(context.Background(), "http.request")
	r := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	r = r.WithContext(ctx)
	r.Header.Set("Authorization", "Bearer "+tok)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	parentSpan.End()

	require.Equal(t, http.StatusOK, w.Code)

	spans := sr.Ended()
	require.Len(t, spans, 1)

	attrMap := spanAttrMap(spans[0])
	assert.Equal(t, "miss", attrMap["auth.cache_result"],
		"AC-C.4: first request (DB lookup) must set auth.cache_result=miss")
}

// TestMiddleware_SpanAttributes_CacheResult_Hit verifies that a second request
// for the same sub (cache warm) sets auth.cache_result=hit on the span (AC-C.4).
func TestMiddleware_SpanAttributes_CacheResult_Hit(t *testing.T) {
	t.Setenv("MCP_JWT_SECRET", testSecret)
	t.Setenv("MCP_JWT_ISSUER", testIssuer)

	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))

	store := &fakeRevocationStore{revoked: false}
	cache := &RevocationCache{entries: make(map[string]*cacheEntry)}
	tok := validToken(t, testSub, testEmail)

	handler := Middleware(ModeEnabled, store, cache, "https://test", echoHandler())

	// First request — populates the cache (miss).
	r1 := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	ctx1, span1 := tp.Tracer("test").Start(context.Background(), "http.request.1")
	r1 = r1.WithContext(ctx1)
	r1.Header.Set("Authorization", "Bearer "+tok)
	handler.ServeHTTP(httptest.NewRecorder(), r1)
	span1.End()

	// Second request — cache is warm; should be a hit.
	ctx2, span2 := tp.Tracer("test").Start(context.Background(), "http.request.2")
	r2 := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	r2 = r2.WithContext(ctx2)
	r2.Header.Set("Authorization", "Bearer "+tok)
	w2 := httptest.NewRecorder()
	handler.ServeHTTP(w2, r2)
	span2.End()

	require.Equal(t, http.StatusOK, w2.Code)

	spans := sr.Ended()
	require.Len(t, spans, 2, "exactly two spans must have been recorded")

	// The second span (index 1) corresponds to the second request.
	attrMap := spanAttrMap(spans[1])
	assert.Equal(t, "hit", attrMap["auth.cache_result"],
		"AC-C.4: second request (cache warm) must set auth.cache_result=hit")
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

// spanAttrMap converts the recorded span attributes to a string map for easy
// assertion in tests. Only string-typed attributes are included; numeric and
// boolean attributes are skipped (not needed by the AC-C tests).
func spanAttrMap(s sdktrace.ReadOnlySpan) map[string]string {
	attrs := s.Attributes()
	m := make(map[string]string, len(attrs))
	for _, kv := range attrs {
		m[string(kv.Key)] = kv.Value.AsString()
	}
	return m
}

// Compile-time assertion: trace package imported for use in test bodies above.
var _ = trace.SpanFromContext
