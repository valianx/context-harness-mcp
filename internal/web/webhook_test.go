package web

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariogutierrez/context-harness-mcp/internal/auth"
)

const testWebhookSecret = "test-webhook-secret-abc123"

// stubDB satisfies revocationUpdater without a live pool.
// It records the last call for assertion in tests.
type stubDB struct {
	calls []struct {
		id        string
		revokedAt *time.Time
	}
	errToReturn error
}

func (s *stubDB) SetUserRevoked(_ context.Context, id string, revokedAt *time.Time) error {
	s.calls = append(s.calls, struct {
		id        string
		revokedAt *time.Time
	}{id: id, revokedAt: revokedAt})
	return s.errToReturn
}

// newTestHandler returns a webhookHandler wired with a stubDB and the given
// RevocationCache. The secret is the package-level test constant.
func newTestHandler(db *stubDB, cache *auth.RevocationCache) *webhookHandler {
	return &webhookHandler{
		db:              db,
		revocationCache: cache,
		secret:          testWebhookSecret,
	}
}

// postWebhook sends a POST /auth/webhook request to the handler with the given
// secret header and JSON body. Returns the recorded response.
func postWebhook(t *testing.T, h *webhookHandler, secret string, body any) *httptest.ResponseRecorder {
	t.Helper()
	data, err := json.Marshal(body)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost, "/auth/webhook", bytes.NewReader(data))
	if secret != "" {
		req.Header.Set("X-Webhook-Secret", secret)
	}
	req.Header.Set("Content-Type", "application/json")

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

// TestWebhookInvalidSecret covers AC-2: invalid HMAC → 401 with correct error body.
func TestWebhookInvalidSecret(t *testing.T) {
	db := &stubDB{}
	cache := auth.NewRevocationCache()
	h := newTestHandler(db, cache)

	rr := postWebhook(t, h, "wrong-secret", map[string]string{"type": "DELETE"})

	assert.Equal(t, http.StatusUnauthorized, rr.Code)

	var body map[string]any
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
	assert.Equal(t, auth.CodeWebhookInvalidSignature, body["code"])
	assert.Equal(t, "auth", body["layer"])
	// auth/webhook-invalid-signature must NOT include auth_login_url (§F, AC-2).
	_, hasLoginURL := body["auth_login_url"]
	assert.False(t, hasLoginURL, "auth_login_url must be absent for webhook-invalid-signature")

	// No DB write on invalid secret.
	assert.Empty(t, db.calls)
}

// TestWebhookDeleteRevokesUser covers AC-1: valid DELETE → revoked_at set.
func TestWebhookDeleteRevokesUser(t *testing.T) {
	db := &stubDB{}
	cache := auth.NewRevocationCache()
	userID := "00000000-0000-0000-0000-000000000001"

	// Prime the cache so we can verify invalidation.
	cache.Set(userID, false)
	_, cached := cache.Get(userID)
	require.True(t, cached, "precondition: cache must have entry")

	h := newTestHandler(db, cache)

	payload := map[string]any{
		"type":       "DELETE",
		"table":      "users",
		"schema":     "auth",
		"record":     nil,
		"old_record": map[string]any{"id": userID, "email": "test@example.com"},
	}
	rr := postWebhook(t, h, testWebhookSecret, payload)

	assert.Equal(t, http.StatusOK, rr.Code)

	// DB must have been called with a non-nil revokedAt.
	require.Len(t, db.calls, 1)
	assert.Equal(t, userID, db.calls[0].id)
	assert.NotNil(t, db.calls[0].revokedAt, "revokedAt must be set for DELETE")

	// Cache entry must be invalidated after DELETE.
	_, cached = cache.Get(userID)
	assert.False(t, cached, "cache must be invalidated after DELETE webhook")
}

// TestWebhookUpdateBanRevokesUser covers AC-3 (ban path): UPDATE with future
// banned_until → revoked_at set.
func TestWebhookUpdateBanRevokesUser(t *testing.T) {
	db := &stubDB{}
	cache := auth.NewRevocationCache()
	userID := "00000000-0000-0000-0000-000000000002"
	future := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)

	h := newTestHandler(db, cache)

	payload := map[string]any{
		"type":   "UPDATE",
		"table":  "users",
		"schema": "auth",
		"record": map[string]any{
			"id":           userID,
			"banned_until": future,
			"deleted_at":   nil,
		},
	}
	rr := postWebhook(t, h, testWebhookSecret, payload)

	assert.Equal(t, http.StatusOK, rr.Code)
	require.Len(t, db.calls, 1)
	assert.Equal(t, userID, db.calls[0].id)
	assert.NotNil(t, db.calls[0].revokedAt, "revokedAt must be set for ban UPDATE")
}

// TestWebhookUpdateUnbanClearsRevocation covers AC-3 (unban path): UPDATE with
// banned_until=null and deleted_at=null → revoked_at set to NULL.
func TestWebhookUpdateUnbanClearsRevocation(t *testing.T) {
	db := &stubDB{}
	cache := auth.NewRevocationCache()
	userID := "00000000-0000-0000-0000-000000000003"

	// Prime the cache with a revoked entry.
	cache.Set(userID, true)
	_, cached := cache.Get(userID)
	require.True(t, cached, "precondition: cache must have entry")

	h := newTestHandler(db, cache)

	payload := map[string]any{
		"type":   "UPDATE",
		"table":  "users",
		"schema": "auth",
		"record": map[string]any{
			"id":           userID,
			"banned_until": nil,
			"deleted_at":   nil,
		},
	}
	rr := postWebhook(t, h, testWebhookSecret, payload)

	assert.Equal(t, http.StatusOK, rr.Code)
	require.Len(t, db.calls, 1)
	assert.Equal(t, userID, db.calls[0].id)
	assert.Nil(t, db.calls[0].revokedAt, "revokedAt must be NULL for unban UPDATE")

	// Cache must be invalidated.
	_, cached = cache.Get(userID)
	assert.False(t, cached, "cache must be invalidated after unban UPDATE webhook")
}

// TestWebhookIgnoredType covers AC-4: valid HMAC but INSERT type → 200 no-op,
// no DB call.
func TestWebhookIgnoredType(t *testing.T) {
	db := &stubDB{}
	cache := auth.NewRevocationCache()
	h := newTestHandler(db, cache)

	payload := map[string]any{
		"type":   "INSERT",
		"table":  "users",
		"schema": "auth",
		"record": map[string]string{"id": "abc123"},
	}
	rr := postWebhook(t, h, testWebhookSecret, payload)

	assert.Equal(t, http.StatusOK, rr.Code)
	assert.Empty(t, db.calls, "INSERT type must produce no DB call")
}

// TestWebhookIgnoredTable covers AC-4: valid HMAC but unknown table → 200 no-op.
func TestWebhookIgnoredTable(t *testing.T) {
	db := &stubDB{}
	cache := auth.NewRevocationCache()
	h := newTestHandler(db, cache)

	payload := map[string]any{
		"type":       "DELETE",
		"table":      "sessions",
		"schema":     "auth",
		"record":     nil,
		"old_record": map[string]string{"id": "abc123"},
	}
	rr := postWebhook(t, h, testWebhookSecret, payload)

	assert.Equal(t, http.StatusOK, rr.Code)
	assert.Empty(t, db.calls, "non-users table must produce no DB call")
}

// TestWebhookMalformedBody covers the tolerant-parsing path: malformed JSON →
// 200 (avoid pg_net retries) and no panic.
func TestWebhookMalformedBody(t *testing.T) {
	db := &stubDB{}
	cache := auth.NewRevocationCache()
	h := newTestHandler(db, cache)

	req := httptest.NewRequest(http.MethodPost, "/auth/webhook", bytes.NewBufferString("{not valid json"))
	req.Header.Set("X-Webhook-Secret", testWebhookSecret)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusOK, rr.Code)
	assert.Empty(t, db.calls)
}

// TestIsFutureBannedUntil verifies the timestamp parsing helper.
func TestIsFutureBannedUntil(t *testing.T) {
	future := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
	past := time.Now().Add(-24 * time.Hour).UTC().Format(time.RFC3339)

	assert.True(t, isFutureBannedUntil(&future))
	assert.False(t, isFutureBannedUntil(&past))
	assert.False(t, isFutureBannedUntil(nil))

	garbage := "not-a-timestamp"
	assert.False(t, isFutureBannedUntil(&garbage))
}
