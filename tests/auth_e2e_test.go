// Package tests — auth exchange E2E integration tests for PR-3 / auth-ux-redesign.
//
// Covers:
//   - AC-1 (auth-ux-redesign PR-1): happy path POST /auth/exchange now also issues
//     a ch_session cookie alongside the MCP JWT. The "Set-Cookie must be absent"
//     assertion was updated to reflect the STAGE-GATE-1 ratified change.
//   - AC-2: happy path POST /auth/exchange → 200, token+expires_at+snippet,
//     users row created, token validates with auth.ValidateMCPToken.
//   - AC-5: atomicity — JWT issuance failure → ROLLBACK → no orphan row (new user),
//     and existing user row is UNCHANGED after failure.
//
// All tests use the shared testcontainers pool from setup_test.go.
// Supabase is mocked via httptest.Server — no real Supabase is called.
//
// [SCOPE-DRIFT: web.exchangeHandler and its tokenIssuer field are unexported.
// The implementer did NOT expose a constructor accepting a custom tokenIssuer
// for external-package injection (task description assumed this existed).
// AC-5 is tested by omitting MCP_JWT_SECRET so auth.IssueMCPToken returns an
// error naturally — this exercises the real production rollback path without
// modifying implementation files.]
package tests

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
	"github.com/mariogutierrez/context-harness-mcp/internal/store"
	"github.com/mariogutierrez/context-harness-mcp/internal/web"
)

// ── constants ────────────────────────────────────────────────────────────────

const (
	e2eJWTSecret = "e2e-test-jwt-secret-minimum-32bytes-long!!"
	e2eIssuer    = "context-harness-mcp"

	// fake Supabase user returned by the mock httptest.Server for AC-2 happy path.
	e2eSupabaseSub   = "aaaabbbb-cccc-dddd-eeee-ffff00001111"
	e2eSupabaseEmail = "e2e-happy@example.com"

	// sub used for the AC-5 "new user rollback" scenario.
	e2eRollbackSub   = "11112222-3333-4444-5555-666677778888"
	e2eRollbackEmail = "e2e-rollback@example.com"

	// sub used for the AC-5 "existing user unchanged" scenario.
	e2eExistingUserSub      = "aaaabbbb-1111-2222-3333-444455556666"
	e2eExistingUserOldEmail = "old@example.com"
	e2eExistingUserNewEmail = "new@example.com"
)

// ── fake Supabase factory ────────────────────────────────────────────────────

// fakeSupabaseUserJSON builds the JSON that the mock Supabase httptest.Server
// returns for GET /auth/v1/user. email_confirmed_at is set to a past timestamp.
func fakeSupabaseUserJSON(sub, email string) []byte {
	confirmedAt := time.Now().Add(-24 * time.Hour).UTC().Format(time.RFC3339)
	payload := map[string]any{
		"id":                 sub,
		"email":              email,
		"email_confirmed_at": confirmedAt,
	}
	data, _ := json.Marshal(payload)
	return data
}

// newFakeSupabaseServer starts an httptest.Server that responds to
// GET /auth/v1/user with 200 + userJSON. All other requests get 404.
// Cleanup is registered automatically.
func newFakeSupabaseServer(t *testing.T, userJSON []byte) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/auth/v1/user" && r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write(userJSON) //nolint:errcheck
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(ts.Close)
	return ts
}

// ── exchange handler wiring ──────────────────────────────────────────────────

// buildE2EServer registers POST /auth/exchange on a fresh mux, wired to the
// shared test pool and the given fakeSupabaseURL, then wraps it in an
// httptest.Server. Environment variables (MCP_JWT_SECRET, MCP_JWT_ISSUER,
// SUPABASE_PROJECT_URL, SUPABASE_ANON_KEY, MCP_PUBLIC_URL) must be set by
// the caller using t.Setenv before calling this function.
//
// RegisterExchange uses newExchangeHandler which reads env vars at call time,
// so the Setenv calls must precede this helper.
func buildE2EServer(t *testing.T, fakeSupabaseURL string) *httptest.Server {
	t.Helper()
	pool := NewTestPool(t)

	// Point SUPABASE_PROJECT_URL at the fake Supabase server so that
	// auth.NewSupabaseClient inside newExchangeHandler uses the mock URL.
	t.Setenv("SUPABASE_PROJECT_URL", fakeSupabaseURL)
	t.Setenv("SUPABASE_ANON_KEY", "fake-anon-key-for-test")
	t.Setenv("MCP_PUBLIC_URL", "https://mcp.test")
	t.Setenv("MCP_SNIPPET_SERVER_NAME", "context-harness")

	mux := http.NewServeMux()
	web.RegisterExchange(mux, pool)

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// postExchange sends POST /auth/exchange with the given access_token and
// returns the *http.Response. Caller is responsible for closing the body.
func postExchange(t *testing.T, serverURL, accessToken string) *http.Response {
	t.Helper()
	body, err := json.Marshal(map[string]string{"access_token": accessToken})
	require.NoError(t, err)

	resp, err := http.Post(serverURL+"/auth/exchange", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	return resp
}

// cleanupUsers deletes the users rows for the given subs after the test.
func cleanupUsers(t *testing.T, subs ...string) {
	t.Helper()
	pool := NewTestPool(t)
	t.Cleanup(func() {
		ctx := context.Background()
		for _, sub := range subs {
			pool.Exec(ctx, "DELETE FROM users WHERE supabase_user_id = $1", sub)
		}
	})
}

// exchangeResponseBody is the parsed 200 OK body from POST /auth/exchange.
type exchangeResponseBody struct {
	Token     string `json:"token"`
	ExpiresAt string `json:"expires_at"`
	Snippet   string `json:"snippet"`
}

// ── AC-2: happy path E2E ─────────────────────────────────────────────────────

// TestAuthE2E_HappyPath exercises the full POST /auth/exchange happy path
// using a real test-container Postgres DB and a mock Supabase httptest.Server.
//
// Assertions:
//  1. Response is HTTP 200.
//  2. Body has token, expires_at, snippet fields.
//  3. No Set-Cookie header present (viewer public read-only, locked decision).
//  4. users row exists in DB with correct supabase_user_id and email.
//  5. Token validates with auth.ValidateMCPToken using the same secret.
//
// AC-2: POST /auth/exchange with valid Supabase token → 200 + row created + no cookie.
func TestAuthE2E_HappyPath(t *testing.T) {
	pool := NewTestPool(t)
	ctx := context.Background()

	// Set JWT env vars so IssueMCPToken and ValidateMCPToken agree.
	t.Setenv("MCP_JWT_SECRET", e2eJWTSecret)
	t.Setenv("MCP_JWT_ISSUER", e2eIssuer)

	// Ensure any row created by this test is cleaned up.
	cleanupUsers(t, e2eSupabaseSub)

	// Start fake Supabase.
	fakeSupabase := newFakeSupabaseServer(t, fakeSupabaseUserJSON(e2eSupabaseSub, e2eSupabaseEmail))

	// Build the exchange server (RegisterExchange reads env vars set above).
	srv := buildE2EServer(t, fakeSupabase.URL)

	// POST /auth/exchange.
	resp := postExchange(t, srv.URL, "fake-supabase-access-token")
	defer resp.Body.Close()

	// 1. HTTP 200.
	require.Equal(t, http.StatusOK, resp.StatusCode,
		"happy path must return 200 (AC-2)")

	// 2. Set-Cookie: ch_session must be present (AC-1 — session cookie issued alongside JWT).
	// The "viewer public read-only" locked decision was overridden at STAGE-GATE-1 of
	// auth-ux-redesign. The session cookie is now issued unconditionally on success.
	assert.Contains(t, resp.Header.Get("Set-Cookie"), "ch_session=",
		"Set-Cookie must include ch_session cookie (AC-1)")

	// 3. Response body has token, expires_at, snippet.
	var body exchangeResponseBody
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body),
		"response body must be valid JSON")
	assert.NotEmpty(t, body.Token,
		"token field must be present and non-empty in 200 response (AC-2)")
	assert.NotEmpty(t, body.ExpiresAt,
		"expires_at field must be present and non-empty in 200 response (AC-2)")
	assert.Contains(t, body.Snippet, "mcpServers",
		"snippet must contain mcpServers key (AC-2)")
	assert.Contains(t, body.Snippet, "Bearer "+body.Token,
		"snippet must embed the issued Bearer token (AC-2)")

	// 4. users row exists in DB.
	userRow, err := store.GetUserByID(ctx, pool, e2eSupabaseSub)
	require.NoError(t, err,
		"users row must exist in DB after successful exchange (AC-2)")
	assert.Equal(t, e2eSupabaseSub, userRow.SupabaseUserID,
		"users.supabase_user_id must match the Supabase user ID (AC-2)")
	assert.Equal(t, e2eSupabaseEmail, userRow.Email,
		"users.email must match the email from the Supabase response (AC-2)")
	assert.Nil(t, userRow.RevokedAt,
		"users.revoked_at must be NULL for a freshly exchanged token (AC-2)")

	// 5. Token validates with ValidateMCPToken.
	claims, err := auth.ValidateMCPToken(body.Token)
	require.NoError(t, err,
		"issued token must pass ValidateMCPToken with the same secret (AC-2)")
	assert.Equal(t, e2eSupabaseSub, claims.Subject,
		"token sub must equal supabase_user_id (AC-2)")
	assert.Equal(t, e2eSupabaseEmail, claims.Email,
		"token email claim must match Supabase user email (AC-2)")
	assert.Equal(t, e2eIssuer, claims.Issuer,
		"token iss must match configured issuer (AC-2)")
	assert.True(t, claims.ExpiresAt.After(time.Now()),
		"token must not be immediately expired after issuance (AC-2)")
}

// ── AC-5: atomicity — new user rollback ──────────────────────────────────────

// TestAuthE2E_Atomicity_NewUser_Rollback verifies that when IssueMCPToken fails
// (triggered here by deliberately not setting MCP_JWT_SECRET) for a brand-new
// user, the DB transaction is rolled back and no users row remains.
//
// Production rollback path: exchangeWithinTx calls h.issueToken; on error, the
// deferred tx.Rollback fires before the handler returns 500.
//
// AC-5(b): JWT issuance failure for new user → 500 auth/jwt-issuance-failed
// AND no users row exists for that supabase_user_id post-call.
func TestAuthE2E_Atomicity_NewUser_Rollback(t *testing.T) {
	pool := NewTestPool(t)
	ctx := context.Background()

	// Do NOT set MCP_JWT_SECRET — auth.IssueMCPToken will fail with
	// "MCP_JWT_SECRET is not set", triggering the rollback path.
	t.Setenv("MCP_JWT_SECRET", "")

	// Verify no pre-existing row for this sub (pre-condition).
	_, preErr := store.GetUserByID(ctx, pool, e2eRollbackSub)
	require.Error(t, preErr,
		"pre-condition: no users row must exist for rollback sub before the call (AC-5)")

	// Cleanup in case rollback fails and the test leaks a row.
	cleanupUsers(t, e2eRollbackSub)

	// Start fake Supabase returning the rollback user.
	fakeSupabase := newFakeSupabaseServer(t, fakeSupabaseUserJSON(e2eRollbackSub, e2eRollbackEmail))

	// Build server — MCP_JWT_SECRET is empty, so IssueMCPToken will fail.
	srv := buildE2EServer(t, fakeSupabase.URL)

	// POST /auth/exchange.
	resp := postExchange(t, srv.URL, "supabase-token-for-new-user")
	defer resp.Body.Close()

	// Assert: HTTP 500 with auth/jwt-issuance-failed.
	require.Equal(t, http.StatusInternalServerError, resp.StatusCode,
		"JWT issuance failure must return 500 (AC-5)")

	var errBody map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&errBody))
	assert.Equal(t, auth.CodeJWTIssuanceFailed, errBody["code"],
		"error code must be auth/jwt-issuance-failed (AC-5)")
	assert.Equal(t, "auth", errBody["layer"],
		"layer must be auth (AC-5)")

	// Assert: NO users row for this sub — rollback must have undone the upsert.
	_, postErr := store.GetUserByID(ctx, pool, e2eRollbackSub)
	require.Error(t, postErr,
		"users row must NOT exist after JWT issuance failure — transaction must have rolled back (AC-5)")
}

// ── AC-5: atomicity — existing user row UNCHANGED on rollback ────────────────

// TestAuthE2E_Atomicity_ExistingUser_Unchanged verifies that when IssueMCPToken
// fails for a user who already has a row in public.users, the transaction is
// rolled back and the pre-call row state is preserved unchanged.
//
// The upsert would normally update email to the new value; rollback must undo it.
//
// AC-5(c): JWT issuance failure for existing user → 500 AND the pre-call row
// (email=old@example.com, revoked_at=NULL) is preserved verbatim.
func TestAuthE2E_Atomicity_ExistingUser_Unchanged(t *testing.T) {
	pool := NewTestPool(t)
	ctx := context.Background()

	// Do NOT set MCP_JWT_SECRET — IssueMCPToken will fail, triggering rollback.
	t.Setenv("MCP_JWT_SECRET", "")

	// Pre-insert an existing user row with a known email.
	_, err := pool.Exec(ctx,
		`INSERT INTO users (supabase_user_id, email, revoked_at)
		 VALUES ($1, $2, NULL)
		 ON CONFLICT (supabase_user_id) DO UPDATE
		   SET email = EXCLUDED.email, revoked_at = NULL`,
		e2eExistingUserSub, e2eExistingUserOldEmail,
	)
	require.NoError(t, err, "pre-condition: insert existing user row")
	t.Cleanup(func() {
		pool.Exec(ctx, "DELETE FROM users WHERE supabase_user_id = $1", e2eExistingUserSub)
	})

	// Confirm pre-call state.
	preCond, err := store.GetUserByID(ctx, pool, e2eExistingUserSub)
	require.NoError(t, err, "pre-condition: existing user row must be readable")
	require.Equal(t, e2eExistingUserOldEmail, preCond.Email,
		"pre-condition: email must be the old value before the call")
	require.Nil(t, preCond.RevokedAt,
		"pre-condition: revoked_at must be NULL before the call")

	// Fake Supabase returns the SAME sub but a NEW email — the upsert would
	// normally overwrite the email, but rollback must undo that change.
	fakeSupabase := newFakeSupabaseServer(t, fakeSupabaseUserJSON(e2eExistingUserSub, e2eExistingUserNewEmail))

	// Build server — MCP_JWT_SECRET is empty → IssueMCPToken fails → rollback.
	srv := buildE2EServer(t, fakeSupabase.URL)

	// POST /auth/exchange with the new email in the Supabase mock.
	resp := postExchange(t, srv.URL, "supabase-token-for-existing-user")
	defer resp.Body.Close()

	// Assert: HTTP 500 with auth/jwt-issuance-failed.
	require.Equal(t, http.StatusInternalServerError, resp.StatusCode,
		"JWT issuance failure for existing user must return 500 (AC-5)")

	var errBody map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&errBody))
	assert.Equal(t, auth.CodeJWTIssuanceFailed, errBody["code"],
		"error code must be auth/jwt-issuance-failed (AC-5)")

	// Assert: existing row is UNCHANGED — rollback preserved old state.
	postCond, err := store.GetUserByID(ctx, pool, e2eExistingUserSub)
	require.NoError(t, err,
		"existing user row must still exist after failed exchange (AC-5)")
	assert.Equal(t, e2eExistingUserOldEmail, postCond.Email,
		"email must be UNCHANGED (old@example.com) — rollback must have undone the upsert (AC-5)")
	assert.Nil(t, postCond.RevokedAt,
		"revoked_at must remain NULL after rollback (AC-5)")
}
