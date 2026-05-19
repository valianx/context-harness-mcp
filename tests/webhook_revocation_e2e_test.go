// Package tests — webhook revocation end-to-end integration test for PR-4.
//
// Covers AC-5: Given a user with a valid MCP JWT and users.revoked_at IS NULL
// (cache primed via a prior request), when the webhook processes a DELETE event
// for that user, a follow-up request to /mcp within 2 seconds of the webhook
// returns HTTP 403 auth/revoked. This validates end-to-end revocation latency ≤2s.
//
// Steps exercised:
//  1. Insert a users row with revoked_at = NULL.
//  2. Issue a test MCP JWT for that user.
//  3. Send a POST /mcp request wrapped by auth.Middleware → expect 200 (primes
//     the revocation cache with "not revoked").
//  4. POST /auth/webhook with valid HMAC and a DELETE event for that user →
//     expect 200, users.revoked_at set in DB, cache entry invalidated.
//  5. Send another POST /mcp request WITHIN 2s of the webhook → expect 403
//     auth/revoked (cache miss forces DB re-check; DB now shows revoked).
//  6. Assert that elapsed time between step 4 and step 5's 403 < 2s.
//
// Uses the shared testcontainers pool from setup_test.go.
// Supabase is not called — all Supabase interactions are bypassed; only the
// /auth/webhook handler and auth.Middleware are exercised with a live DB.
package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariogutierrez/context-harness-mcp/internal/auth"
	"github.com/mariogutierrez/context-harness-mcp/internal/store"
	"github.com/mariogutierrez/context-harness-mcp/internal/web"
)

// ── constants ─────────────────────────────────────────────────────────────────

const (
	revE2EJWTSecret   = "revocation-e2e-test-jwt-secret-min-32-bytes!!"
	revE2EIssuer      = "context-harness-mcp"
	revE2EWebhookSec  = "revocation-e2e-webhook-secret-abc123"

	// The user whose revocation is tested end-to-end.
	revE2EUserSub   = "e2e00001-0001-0001-0001-000000000001"
	revE2EUserEmail = "revocation-e2e@example.com"

	// revocationLatencyBudget is the maximum allowed latency for the revocation
	// to propagate end-to-end (AC-5: ≤2s when webhook is intact).
	revocationLatencyBudget = 2 * time.Second
)

// ── revocationStoreForE2E ────────────────────────────────────────────────────

// revocationStoreE2E wraps *pgxpool.Pool to satisfy auth.RevocationStore.
// Identical to the adapter in auth_integration_test.go — reproduced here so
// this file is self-contained and does not create a cross-test dependency.
type revocationStoreE2E struct {
	pool *pgxpool.Pool
}

func (s *revocationStoreE2E) GetRevoked(sub string) (bool, error) {
	var revoked bool
	err := s.pool.QueryRow(
		context.Background(),
		"SELECT revoked_at IS NOT NULL FROM users WHERE supabase_user_id = $1",
		sub,
	).Scan(&revoked)
	if err != nil {
		// User not found → treat as not revoked (fail-open, mirrors production).
		return false, nil
	}
	return revoked, nil
}

// ── server wiring ─────────────────────────────────────────────────────────────

// buildRevocationE2EServer wires auth.Middleware + /mcp + web.RegisterWebhook
// onto a fresh ServeMux backed by the test pool and returns both the server and
// the shared RevocationCache so tests can inspect cache state.
//
// SUPABASE_WEBHOOK_SECRET and MCP_JWT_SECRET/MCP_JWT_ISSUER must already be set
// via t.Setenv before calling this function — they are read at handler
// construction time.
func buildRevocationE2EServer(t *testing.T, pool *pgxpool.Pool) (*httptest.Server, *auth.RevocationCache) {
	t.Helper()

	cache := auth.NewRevocationCache()
	revStore := &revocationStoreE2E{pool: pool}

	// Minimal /mcp handler — the auth middleware is the focus, not MCP dispatch.
	mcpHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"jsonrpc":"2.0","result":{"content":[{"type":"text","text":"ok"}]}}`)
	})

	wrapped := auth.Middleware(auth.ModeEnabled, revStore, cache, "https://test", mcpHandler)

	mux := http.NewServeMux()
	// Register webhook BEFORE wrapping /mcp — mirrors production wiring.
	web.RegisterWebhook(mux, pool, cache)
	mux.Handle("/mcp", wrapped)

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, cache
}

// ── JWT helpers ───────────────────────────────────────────────────────────────

// issueRevE2EToken signs a short-lived MCP JWT using revE2EJWTSecret.
// The token is valid for 1 hour — long enough for the test but uses the test
// secret so ValidateMCPToken in the middleware accepts it.
func issueRevE2EToken(t *testing.T, sub, email string) string {
	t.Helper()
	now := time.Now()
	claims := auth.Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   sub,
			Issuer:    revE2EIssuer,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour)),
		},
		Email: email,
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	s, err := tok.SignedString([]byte(revE2EJWTSecret))
	require.NoError(t, err, "issueRevE2EToken: sign must succeed")
	return s
}

// ── request helpers ───────────────────────────────────────────────────────────

// postMCPWithToken sends POST /mcp with the given Bearer token and returns the
// response. Caller is responsible for closing the body.
func postMCPWithToken(t *testing.T, serverURL, token string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, serverURL+"/mcp",
		bytes.NewBufferString(`{"jsonrpc":"2.0","method":"initialize","id":1}`))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	return resp
}

// postWebhookDeleteEvent sends POST /auth/webhook with a valid HMAC header and a
// DELETE event body for the given userID. Returns the response.
// Caller is responsible for closing the body.
func postWebhookDeleteEvent(t *testing.T, serverURL, webhookSecret, userID, email string) *http.Response {
	t.Helper()
	payload := map[string]any{
		"type":   "DELETE",
		"table":  "users",
		"schema": "auth",
		"record": nil,
		"old_record": map[string]any{
			"id":    userID,
			"email": email,
		},
	}
	data, err := json.Marshal(payload)
	require.NoError(t, err)

	req, err := http.NewRequest(http.MethodPost, serverURL+"/auth/webhook", bytes.NewReader(data))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Webhook-Secret", webhookSecret)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	return resp
}

// ── AC-5: end-to-end revocation latency ≤2s ──────────────────────────────────

// TestWebhookRevocation_E2E_LatencyUnder2s validates AC-5 end-to-end:
// after a webhook DELETE event, a subsequent /mcp request by the revoked user
// receives 403 auth/revoked within 2 seconds of the webhook call.
//
// This is the full stack test: testcontainers Postgres + auth.Middleware +
// RevocationCache + web.RegisterWebhook, all wired together.
func TestWebhookRevocation_E2E_LatencyUnder2s(t *testing.T) {
	pool := NewTestPool(t)
	ctx := context.Background()

	// Set env vars read by handlers at construction time.
	t.Setenv("MCP_JWT_SECRET", revE2EJWTSecret)
	t.Setenv("MCP_JWT_ISSUER", revE2EIssuer)
	t.Setenv("SUPABASE_WEBHOOK_SECRET", revE2EWebhookSec)

	// Step 1: Insert user with revoked_at = NULL.
	_, err := pool.Exec(ctx,
		`INSERT INTO users (supabase_user_id, email, revoked_at)
		 VALUES ($1, $2, NULL)
		 ON CONFLICT (supabase_user_id) DO UPDATE
		   SET email = EXCLUDED.email, revoked_at = NULL`,
		revE2EUserSub, revE2EUserEmail,
	)
	require.NoError(t, err, "Step 1: insert active user row must succeed")
	t.Cleanup(func() {
		pool.Exec(ctx, "DELETE FROM users WHERE supabase_user_id = $1", revE2EUserSub)
	})

	// Build the full server stack.
	srv, _ := buildRevocationE2EServer(t, pool)

	// Step 2: Issue a test MCP JWT for the user.
	token := issueRevE2EToken(t, revE2EUserSub, revE2EUserEmail)

	// Step 3: Send a /mcp request to prime the revocation cache with "not revoked".
	resp1 := postMCPWithToken(t, srv.URL, token)
	defer resp1.Body.Close()
	require.Equal(t, http.StatusOK, resp1.StatusCode,
		"Step 3: first /mcp request with valid token must return 200 (primes cache)")

	// Step 4: POST /auth/webhook with DELETE event → expect 200, DB updated, cache invalidated.
	webhookStart := time.Now()
	respWebhook := postWebhookDeleteEvent(t, srv.URL, revE2EWebhookSec, revE2EUserSub, revE2EUserEmail)
	defer respWebhook.Body.Close()

	require.Equal(t, http.StatusOK, respWebhook.StatusCode,
		"Step 4: webhook DELETE event must return 200")

	// Verify DB: users.revoked_at must now be set.
	userRow, err := store.GetUserByID(ctx, pool, revE2EUserSub)
	require.NoError(t, err, "Step 4: users row must still exist after webhook")
	assert.NotNil(t, userRow.RevokedAt,
		"Step 4: users.revoked_at must be set after webhook DELETE event (AC-5)")

	// Step 5: Send another /mcp request — must receive 403 auth/revoked.
	// The cache was invalidated by the webhook, so the middleware re-queries the
	// DB, sees revoked_at IS NOT NULL, and returns 403.
	resp2 := postMCPWithToken(t, srv.URL, token)
	defer resp2.Body.Close()

	elapsed := time.Since(webhookStart)

	require.Equal(t, http.StatusForbidden, resp2.StatusCode,
		"Step 5: /mcp request after webhook DELETE must return 403 auth/revoked (AC-5)")

	var errBody map[string]any
	require.NoError(t, json.NewDecoder(resp2.Body).Decode(&errBody),
		"Step 5: response body must be valid JSON")
	assert.Equal(t, auth.CodeRevoked, errBody["code"],
		"Step 5: error code must be auth/revoked (AC-5)")
	assert.Equal(t, "auth", errBody["layer"],
		"Step 5: error layer must be auth")

	// Step 6: Assert revocation latency < 2s.
	assert.Less(t, elapsed, revocationLatencyBudget,
		"Step 6: revocation latency must be ≤%s — got %s (AC-5)", revocationLatencyBudget, elapsed)
}

// TestWebhookRevocation_E2E_UpdateBan_LatencyUnder2s validates AC-5 via the
// UPDATE ban path: a webhook UPDATE event with a future banned_until timestamp
// revokes the user; the follow-up /mcp request within 2s returns 403.
//
// This exercises the handleUpdate code path (not DELETE) for completeness.
func TestWebhookRevocation_E2E_UpdateBan_LatencyUnder2s(t *testing.T) {
	pool := NewTestPool(t)
	ctx := context.Background()

	t.Setenv("MCP_JWT_SECRET", revE2EJWTSecret)
	t.Setenv("MCP_JWT_ISSUER", revE2EIssuer)
	t.Setenv("SUPABASE_WEBHOOK_SECRET", revE2EWebhookSec)

	// Use a distinct sub so this test is isolated from the DELETE test above.
	banSub := "e2e00002-0002-0002-0002-000000000002"
	banEmail := "revocation-ban-e2e@example.com"

	// Insert active user.
	_, err := pool.Exec(ctx,
		`INSERT INTO users (supabase_user_id, email, revoked_at)
		 VALUES ($1, $2, NULL)
		 ON CONFLICT (supabase_user_id) DO UPDATE
		   SET email = EXCLUDED.email, revoked_at = NULL`,
		banSub, banEmail,
	)
	require.NoError(t, err, "insert active user for ban test must succeed")
	t.Cleanup(func() {
		pool.Exec(ctx, "DELETE FROM users WHERE supabase_user_id = $1", banSub)
	})

	srv, _ := buildRevocationE2EServer(t, pool)

	token := issueRevE2EToken(t, banSub, banEmail)

	// Prime the cache: first request must succeed.
	resp1 := postMCPWithToken(t, srv.URL, token)
	defer resp1.Body.Close()
	require.Equal(t, http.StatusOK, resp1.StatusCode,
		"first /mcp request must return 200 (primes cache for ban test)")

	// Build an UPDATE ban webhook payload with a future banned_until.
	future := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
	banPayload := map[string]any{
		"type":   "UPDATE",
		"table":  "users",
		"schema": "auth",
		"record": map[string]any{
			"id":           banSub,
			"email":        banEmail,
			"banned_until": future,
			"deleted_at":   nil,
		},
	}
	banData, err := json.Marshal(banPayload)
	require.NoError(t, err)

	webhookStart := time.Now()
	reqWebhook, err := http.NewRequest(http.MethodPost, srv.URL+"/auth/webhook", bytes.NewReader(banData))
	require.NoError(t, err)
	reqWebhook.Header.Set("Content-Type", "application/json")
	reqWebhook.Header.Set("X-Webhook-Secret", revE2EWebhookSec)

	respWebhook, err := http.DefaultClient.Do(reqWebhook)
	require.NoError(t, err)
	defer respWebhook.Body.Close()

	require.Equal(t, http.StatusOK, respWebhook.StatusCode,
		"webhook UPDATE ban event must return 200")

	// Verify DB: revoked_at set.
	userRow, err := store.GetUserByID(ctx, pool, banSub)
	require.NoError(t, err)
	assert.NotNil(t, userRow.RevokedAt,
		"users.revoked_at must be set after webhook UPDATE ban event")

	// Follow-up request must return 403.
	resp2 := postMCPWithToken(t, srv.URL, token)
	defer resp2.Body.Close()
	elapsed := time.Since(webhookStart)

	require.Equal(t, http.StatusForbidden, resp2.StatusCode,
		"/mcp request after UPDATE ban webhook must return 403 auth/revoked (AC-5)")

	var errBody map[string]any
	require.NoError(t, json.NewDecoder(resp2.Body).Decode(&errBody))
	assert.Equal(t, auth.CodeRevoked, errBody["code"],
		"error code must be auth/revoked after ban webhook (AC-5)")

	assert.Less(t, elapsed, revocationLatencyBudget,
		"ban revocation latency must be ≤%s — got %s (AC-5)", revocationLatencyBudget, elapsed)
}
