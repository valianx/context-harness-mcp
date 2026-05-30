// Package khctl — unit tests for the revoke/reinstate operations.
//
// These tests require a running Postgres container (testcontainers-go).
// They are skipped automatically when Docker is unavailable.
//
// Because internal/khctl already has a TestMain in sync_test.go that boots
// the container, we reuse the syncTestPool and syncTestDSN package variables.
package khctl

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRevoke_ByEmail revokes a user by email and then reinstates them.
func TestRevoke_ByEmail(t *testing.T) {
	pool := getTestPool(t)
	cleanUsers(t)

	id := "ee000001-0001-4001-a001-000000000001"
	email := "revoke-email@example.com"
	insertUser(t, id, email, nil)

	ctx := context.Background()

	// Revoke.
	found, err := Revoke(ctx, pool, MatchByEmail, email)
	require.NoError(t, err)
	assert.True(t, found, "user must be found")

	u := getUser(t, id)
	require.NotNil(t, u)
	assert.NotNil(t, u.RevokedAt, "revoked_at must be set after Revoke")

	// Reinstate.
	found, err = Reinstate(ctx, pool, MatchByEmail, email)
	require.NoError(t, err)
	assert.True(t, found, "user must be found for reinstate")

	u = getUser(t, id)
	require.NotNil(t, u)
	assert.Nil(t, u.RevokedAt, "revoked_at must be NULL after Reinstate")
}

// TestRevoke_BySubject revokes a user by external_subject.
func TestRevoke_BySubject(t *testing.T) {
	pool := getTestPool(t)
	cleanUsers(t)

	id := "ee000002-0002-4002-a002-000000000002"
	email := "revoke-subject@example.com"
	subject := "google-subject-12345"
	insertUserWithSubject(t, id, email, "google", subject)

	ctx := context.Background()

	found, err := Revoke(ctx, pool, MatchBySubject, subject)
	require.NoError(t, err)
	assert.True(t, found, "user must be found by subject")

	u := getUser(t, id)
	require.NotNil(t, u)
	assert.NotNil(t, u.RevokedAt, "revoked_at must be set after Revoke by subject")

	// Reinstate by subject.
	found, err = Reinstate(ctx, pool, MatchBySubject, subject)
	require.NoError(t, err)
	assert.True(t, found)
	u = getUser(t, id)
	assert.Nil(t, u.RevokedAt, "revoked_at must be NULL after Reinstate by subject")
}

// TestRevoke_NotFound returns (false, nil) when no user matches.
func TestRevoke_NotFound(t *testing.T) {
	pool := getTestPool(t)
	cleanUsers(t)

	found, err := Revoke(context.Background(), pool, MatchByEmail, "nonexistent@example.com")
	require.NoError(t, err)
	assert.False(t, found, "Revoke of nonexistent user must return found=false")
}

// TestReinstate_NotFound returns (false, nil) when no user matches.
func TestReinstate_NotFound(t *testing.T) {
	pool := getTestPool(t)
	cleanUsers(t)

	found, err := Reinstate(context.Background(), pool, MatchByEmail, "ghost@example.com")
	require.NoError(t, err)
	assert.False(t, found, "Reinstate of nonexistent user must return found=false")
}

// TestRevoke_AlreadyRevoked is idempotent: revoking again updates revoked_at timestamp.
func TestRevoke_AlreadyRevoked(t *testing.T) {
	pool := getTestPool(t)
	cleanUsers(t)

	id := "ee000003-0003-4003-a003-000000000003"
	email := "revoke-idempotent@example.com"
	past := time.Now().Add(-time.Hour)
	insertUser(t, id, email, &past)

	ctx := context.Background()

	found, err := Revoke(ctx, pool, MatchByEmail, email)
	require.NoError(t, err)
	assert.True(t, found)

	u := getUser(t, id)
	require.NotNil(t, u)
	require.NotNil(t, u.RevokedAt)
	// New revoked_at should be after the old one.
	assert.True(t, u.RevokedAt.After(past), "second Revoke must update the timestamp")
}

// ── helper ────────────────────────────────────────────────────────────────────

// insertUserWithSubject pre-seeds a users row with auth_provider and external_subject.
// This exercises the migration 00010 columns.
func insertUserWithSubject(t *testing.T, id, email, provider, subject string) {
	t.Helper()
	pool := getTestPool(t)
	_, err := pool.Exec(context.Background(),
		`INSERT INTO users (supabase_user_id, email, auth_provider, external_subject)
		 VALUES ($1, $2, $3, $4)
		 ON CONFLICT (supabase_user_id) DO UPDATE
		   SET email = EXCLUDED.email,
		       auth_provider = EXCLUDED.auth_provider,
		       external_subject = EXCLUDED.external_subject`,
		id, email, provider, subject,
	)
	require.NoError(t, err, "insertUserWithSubject must succeed for id=%s", id)
}
