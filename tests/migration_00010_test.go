// Package tests — integration tests for migration 00010_oidc_provider.
// Covers AC-7 of the ch-oidc-agnostic-auth feature: +goose Down reverses cleanly.
//
// These tests require a running Docker daemon (they use testcontainers-go).
// When Docker is unavailable each test calls t.Skipf and the suite exits 0 — the
// tests are written and exercisable in CI with Docker; they are NOT claimed to
// have passed in environments where Docker is absent.
package tests

import (
	"context"
	"testing"

	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMigration00010_Down_ReversesCleanly verifies AC-7: running goose down
// from version 10 to version 9 drops the auth_provider and external_subject
// columns and their indexes without error, leaving the users table in its
// pre-00010 state.
//
// The +goose Down block of 00010_oidc_provider.sql drops both new columns and
// both new indexes. This test confirms the block executes without error and that
// the columns are absent afterwards.
func TestMigration00010_Down_ReversesCleanly(t *testing.T) {
	ctx := context.Background()
	if err := checkDockerAvailable(ctx); err != nil {
		t.Skipf("Docker daemon unavailable — test written but cannot run: %v", err)
	}

	// newEphemeralContainer is defined in migration_00007_test.go and handles
	// container lifecycle + sql.DB setup.
	db, cleanup := newEphemeralContainer(t)
	defer cleanup()

	require.NoError(t, goose.SetDialect("postgres"))

	// Apply all migrations (00001..00010).
	require.NoError(t, goose.Up(db, migrationsDir),
		"goose.Up must apply all migrations without error before down test")

	version, err := goose.GetDBVersion(db)
	require.NoError(t, err)
	require.Equal(t, int64(10), version,
		"schema must be at version 10 before running down")

	// Confirm both OIDC columns exist before running down.
	t.Run("pre_down_columns_present", func(t *testing.T) {
		for _, col := range []string{"auth_provider", "external_subject"} {
			var count int
			require.NoError(t, db.QueryRowContext(ctx,
				`select count(*) from information_schema.columns
				  where table_schema = 'public'
				    and table_name   = 'users'
				    and column_name  = $1`,
				col,
			).Scan(&count))
			assert.Equal(t, 1, count,
				"column users.%s must exist before goose down", col)
		}
	})

	// Run goose down exactly one step (10 → 9).
	require.NoError(t, goose.Down(db, migrationsDir),
		"goose down (10→9) must succeed without error")

	version, err = goose.GetDBVersion(db)
	require.NoError(t, err)
	assert.Equal(t, int64(9), version,
		"schema must be at version 9 after goose down one step")

	// Confirm both OIDC columns are gone.
	t.Run("post_down_columns_absent", func(t *testing.T) {
		for _, col := range []string{"auth_provider", "external_subject"} {
			var count int
			require.NoError(t, db.QueryRowContext(ctx,
				`select count(*) from information_schema.columns
				  where table_schema = 'public'
				    and table_name   = 'users'
				    and column_name  = $1`,
				col,
			).Scan(&count))
			assert.Equal(t, 0, count,
				"column users.%s must be absent after goose down", col)
		}
	})

	// Confirm the two new indexes are gone.
	t.Run("post_down_indexes_absent", func(t *testing.T) {
		for _, idx := range []string{"users_provider_subject_key", "users_external_subject_idx"} {
			var count int
			require.NoError(t, db.QueryRowContext(ctx,
				`select count(*) from pg_indexes
				  where schemaname = 'public'
				    and tablename  = 'users'
				    and indexname  = $1`,
				idx,
			).Scan(&count))
			assert.Equal(t, 0, count,
				"index %s must be absent after goose down", idx)
		}
	})

	// Re-apply migration 00010 (up) to confirm the round-trip is idempotent.
	t.Run("re_up_succeeds", func(t *testing.T) {
		require.NoError(t, goose.Up(db, migrationsDir),
			"goose up after down must succeed without error (idempotent round-trip)")

		version, err := goose.GetDBVersion(db)
		require.NoError(t, err)
		assert.Equal(t, int64(10), version,
			"schema must be back at version 10 after re-up")
	})
}
