// Package khctl — unit tests for SyncUsers reconciliation logic.
//
// The Supabase Admin API is mocked via httptest.Server so no real Supabase is
// called. The local DB is a testcontainers-go ephemeral Postgres instance
// provisioned through the same pattern as tests/setup_test.go — but because
// this file lives in internal/khctl we cannot share TestMain from tests/.
// Instead we spin up the container here via TestMain in this package.
//
// Four reconciliation scenarios (A–D) per the PR-4 testing strategy line:
//   "4 scenarios: Supabase=revoked/local=active, Supabase=active/local=revoked,
//    both same, user not in local DB → revoke."
//
// Note: the package does NOT export reconcile/fetchAllLocalUsers, so the tests
// drive SyncUsers end-to-end (httptest.Server replaces the real Supabase URL).
// This is valid because SyncUsers is the public entry point and all four rules
// flow through it.
package khctl

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/mariogutierrez/context-harness-mcp/internal/store"
)

// ── package-level state ──────────────────────────────────────────────────────

const (
	syncTestDBName     = "khctl_sync_test"
	syncTestDBUser     = "test"
	syncTestDBPassword = "test"
	pgvectorImage      = "pgvector/pgvector:pg16"
	// migrationsDir is relative to the internal/khctl/ directory (repo root is
	// two levels up since go test ./... runs from repo root as CWD).
	migrationsDir = "../../migrations"

	// fake service role key — only used to satisfy the Authorization header
	// check; the mock server never validates it.
	fakeServiceRoleKey = "fake-service-role-key-for-testing"
)

var (
	syncTestPool *pgxpool.Pool
	syncTestDSN  string
)

// TestMain boots an ephemeral Postgres container, applies migrations, and runs
// the suite. Skips gracefully when Docker is unavailable.
func TestMain(m *testing.M) {
	os.Exit(runSyncSuite(m))
}

func runSyncSuite(m *testing.M) int {
	ctx := context.Background()

	if err := checkDockerAvailable(ctx); err != nil {
		slog.Info("Docker daemon not available — skipping khctl sync test suite", "reason", err)
		return 0
	}

	pgCtr, err := tcpostgres.Run(ctx,
		pgvectorImage,
		tcpostgres.WithDatabase(syncTestDBName),
		tcpostgres.WithUsername(syncTestDBUser),
		tcpostgres.WithPassword(syncTestDBPassword),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second),
		),
	)
	if err != nil {
		slog.Error("failed to start postgres container for khctl sync tests", "error", err)
		return 1
	}
	defer func() {
		if termErr := testcontainers.TerminateContainer(pgCtr); termErr != nil {
			slog.Warn("failed to terminate postgres container", "error", termErr)
		}
	}()

	dsn, err := pgCtr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		slog.Error("failed to get DSN", "error", err)
		return 1
	}
	syncTestDSN = dsn

	if err := applySyncMigrations(dsn); err != nil {
		slog.Error("migration failed", "error", err)
		return 1
	}

	pool, err := store.New(ctx, dsn)
	if err != nil {
		slog.Error("failed to open test pool", "error", err)
		return 1
	}
	defer pool.Close()
	syncTestPool = pool

	return m.Run()
}

func applySyncMigrations(dsn string) error {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return fmt.Errorf("sql.Open: %w", err)
	}
	defer db.Close()
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("goose.SetDialect: %w", err)
	}
	if err := goose.Up(db, migrationsDir); err != nil {
		return fmt.Errorf("goose.Up: %w", err)
	}
	return nil
}

func checkDockerAvailable(ctx context.Context) error {
	provider, err := testcontainers.ProviderDocker.GetProvider()
	if err != nil {
		return err
	}
	return provider.Health(ctx)
}

// ── helpers ──────────────────────────────────────────────────────────────────

// getTestPool returns the package-level pool or skips the test when Docker was
// unavailable at suite startup.
func getTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if syncTestPool == nil {
		t.Skip("syncTestPool is nil — Docker daemon was not available")
	}
	return syncTestPool
}

// cleanUsers truncates the users table so each test starts from a clean slate.
func cleanUsers(t *testing.T) {
	t.Helper()
	pool := getTestPool(t)
	_, err := pool.Exec(context.Background(), "DELETE FROM users")
	require.NoError(t, err, "cleanUsers: DELETE FROM users must succeed")
}

// insertUser pre-seeds a users row. Pass a non-nil revokedAt to seed an already-
// revoked row, or nil to seed an active row.
func insertUser(t *testing.T, id, email string, revokedAt *time.Time) {
	t.Helper()
	pool := getTestPool(t)
	_, err := pool.Exec(context.Background(),
		`INSERT INTO users (supabase_user_id, email, revoked_at)
		 VALUES ($1, $2, $3)
		 ON CONFLICT (supabase_user_id) DO UPDATE
		   SET email = EXCLUDED.email, revoked_at = EXCLUDED.revoked_at`,
		id, email, revokedAt,
	)
	require.NoError(t, err, "insertUser: INSERT must succeed for id=%s", id)
}

// getUser fetches the users row for id. Returns (nil, nil) when the row is not
// found so callers can assert absence without require.Error.
func getUser(t *testing.T, id string) *store.UserRow {
	t.Helper()
	pool := getTestPool(t)
	u, err := store.GetUserByID(context.Background(), pool, id)
	if err != nil {
		return nil
	}
	return u
}

// ── Admin API mock factory ────────────────────────────────────────────────────

// adminUserJSON constructs the Supabase Admin API JSON for a single user.
// bannedUntil and deletedAt are both optional — pass nil to omit them
// (which represents an active user).
func adminUserJSON(id, email string, bannedUntil, deletedAt *string) map[string]any {
	u := map[string]any{
		"id":    id,
		"email": email,
	}
	if bannedUntil != nil {
		u["banned_until"] = *bannedUntil
	} else {
		u["banned_until"] = nil
	}
	if deletedAt != nil {
		u["deleted_at"] = *deletedAt
	} else {
		u["deleted_at"] = nil
	}
	return u
}

// newFakeAdminServer returns an httptest.Server that serves GET /auth/v1/admin/users
// with the given users on page 1 and an empty page 2 (so the pagination loop
// terminates). All other paths return 404.
//
// The service role key is not validated — the mock accepts any Authorization header.
func newFakeAdminServer(t *testing.T, users []map[string]any) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/auth/v1/admin/users" {
			http.NotFound(w, r)
			return
		}
		page := r.URL.Query().Get("page")
		var responseUsers []map[string]any
		if page == "" || page == "1" {
			responseUsers = users
		} else {
			// Any page > 1 returns empty — terminates pagination.
			responseUsers = []map[string]any{}
		}
		resp := map[string]any{"users": responseUsers}
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(resp))
	}))
	t.Cleanup(ts.Close)
	return ts
}

// futureTimestamp returns an RFC3339 timestamp 24h in the future.
func futureTimestamp() string {
	return time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
}

// ── Test cases ────────────────────────────────────────────────────────────────

// TestSyncUsers_CaseA verifies that when the local user already has revoked_at
// set AND Supabase also shows the user as banned, the row is left unchanged
// (no redundant update — revoked_at stays non-null).
//
// Local=revoked, Supabase=banned → no state change (both already match).
func TestSyncUsers_CaseA(t *testing.T) {
	cleanUsers(t)

	id := "aaaaaaaa-0001-0001-0001-000000000001"
	email := "case-a@example.com"

	// Pre-seed: local user is already revoked.
	now := time.Now()
	insertUser(t, id, email, &now)

	// Admin API: Supabase also shows this user as banned.
	future := futureTimestamp()
	ts := newFakeAdminServer(t, []map[string]any{
		adminUserJSON(id, email, &future, nil),
	})

	// Run sync.
	err := SyncUsers(context.Background(), syncTestDSN, fakeServiceRoleKey, ts.URL)
	require.NoError(t, err, "SyncUsers must not return an error for Case A")

	// Assert: row still revoked — revoked_at must remain non-null.
	u := getUser(t, id)
	require.NotNil(t, u, "users row must still exist after sync")
	assert.NotNil(t, u.RevokedAt,
		"Case A: local=revoked + Supabase=banned → revoked_at must remain non-null (no change)")
}

// TestSyncUsers_CaseB verifies that when the local user is active (revoked_at IS
// NULL) but Supabase shows the user as banned, the sync sets revoked_at.
//
// Local=active, Supabase=banned → revoke (set revoked_at = now()).
func TestSyncUsers_CaseB(t *testing.T) {
	cleanUsers(t)

	id := "bbbbbbbb-0002-0002-0002-000000000002"
	email := "case-b@example.com"

	// Pre-seed: local user is active.
	insertUser(t, id, email, nil)

	// Admin API: Supabase shows user as banned.
	future := futureTimestamp()
	ts := newFakeAdminServer(t, []map[string]any{
		adminUserJSON(id, email, &future, nil),
	})

	// Run sync.
	err := SyncUsers(context.Background(), syncTestDSN, fakeServiceRoleKey, ts.URL)
	require.NoError(t, err, "SyncUsers must not return an error for Case B")

	// Assert: row is now revoked.
	u := getUser(t, id)
	require.NotNil(t, u, "users row must exist after sync")
	assert.NotNil(t, u.RevokedAt,
		"Case B: local=active + Supabase=banned → revoked_at must be set")
}

// TestSyncUsers_CaseC verifies that when the local user is revoked (revoked_at IS
// NOT NULL) but Supabase shows the user as active (no ban, no deletion), the sync
// un-revokes the user by setting revoked_at = NULL.
//
// Local=revoked, Supabase=active → un-revoke (set revoked_at = NULL).
func TestSyncUsers_CaseC(t *testing.T) {
	cleanUsers(t)

	id := "cccccccc-0003-0003-0003-000000000003"
	email := "case-c@example.com"

	// Pre-seed: local user is revoked.
	now := time.Now()
	insertUser(t, id, email, &now)

	// Admin API: Supabase shows user as active (both nil).
	ts := newFakeAdminServer(t, []map[string]any{
		adminUserJSON(id, email, nil, nil),
	})

	// Run sync.
	err := SyncUsers(context.Background(), syncTestDSN, fakeServiceRoleKey, ts.URL)
	require.NoError(t, err, "SyncUsers must not return an error for Case C")

	// Assert: row is now un-revoked.
	u := getUser(t, id)
	require.NotNil(t, u, "users row must exist after sync")
	assert.Nil(t, u.RevokedAt,
		"Case C: local=revoked + Supabase=active → revoked_at must be NULL (un-revoked)")
}

// TestSyncUsers_CaseD verifies that when a local user is NOT present in the
// Supabase Admin API response across any page (i.e., the user was deleted from
// Supabase), the sync revokes them locally.
//
// Local=exists, Supabase=missing → revoke (user deleted from Supabase).
func TestSyncUsers_CaseD(t *testing.T) {
	cleanUsers(t)

	missingID := "dddddddd-0004-0004-0004-000000000004"
	missingEmail := "case-d-missing@example.com"

	otherID := "dddddddd-0004-0004-0004-000000000099"
	otherEmail := "case-d-other@example.com"

	// Pre-seed: the "missing" user exists locally but is not in Supabase list.
	// The "other" user is present in both, so it should be left alone.
	insertUser(t, missingID, missingEmail, nil)
	insertUser(t, otherID, otherEmail, nil)

	// Admin API: only the "other" user is returned — missingID is absent.
	ts := newFakeAdminServer(t, []map[string]any{
		adminUserJSON(otherID, otherEmail, nil, nil),
	})

	// Run sync.
	err := SyncUsers(context.Background(), syncTestDSN, fakeServiceRoleKey, ts.URL)
	require.NoError(t, err, "SyncUsers must not return an error for Case D")

	// Assert: missing user is now revoked.
	missing := getUser(t, missingID)
	require.NotNil(t, missing, "users row for missing user must still exist after sync")
	assert.NotNil(t, missing.RevokedAt,
		"Case D: local exists but Supabase missing → revoked_at must be set")

	// Assert: other user is untouched.
	other := getUser(t, otherID)
	require.NotNil(t, other, "users row for other user must still exist after sync")
	assert.Nil(t, other.RevokedAt,
		"Case D: other user (present in Supabase, active) must not be revoked")
}
