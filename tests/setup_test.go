// Package tests provides integration test fixtures for context-harness-mcp.
// Tests require a running Docker daemon — they spin up an ephemeral
// pgvector/pgvector:pg16 container via testcontainers-go and apply migrations
// using the goose library. If Docker is not available the suite skips cleanly.
package tests

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" driver for database/sql
	"github.com/pressly/goose/v3"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/mariogutierrez/context-harness-mcp/internal/embed"
	"github.com/mariogutierrez/context-harness-mcp/internal/store"
)

const (
	testDBName     = "context_harness_test"
	testDBUser     = "test"
	testDBPassword = "test"
	// pgvector image that matches the production target (Supabase Free runs pg16 + pgvector).
	pgvectorImage = "pgvector/pgvector:pg16"
	// migrationsDir is relative to the repo root, which is the working dir for `go test ./...`.
	migrationsDir = "../migrations"
)

// suite-level state shared across tests in this package.
var (
	testPool *pgxpool.Pool
	testDSN  string
)

// TestMain boots the ephemeral container, applies migrations, runs all tests,
// then tears the container down. If Docker is not available the entire suite
// skips with exit code 0 so non-Docker dev environments don't fail CI.
func TestMain(m *testing.M) {
	os.Exit(runSuite(m))
}

// runSuite separates the container lifecycle from os.Exit so defers fire.
func runSuite(m *testing.M) int {
	ctx := context.Background()

	// Detect Docker availability before attempting to spin up the container.
	// If the daemon is unreachable we skip the entire suite (exit 0).
	if err := checkDockerAvailable(ctx); err != nil {
		slog.Info("Docker daemon not available — skipping integration test suite",
			"reason", err.Error())
		return 0
	}

	pgCtr, err := tcpostgres.Run(ctx,
		pgvectorImage,
		tcpostgres.WithDatabase(testDBName),
		tcpostgres.WithUsername(testDBUser),
		tcpostgres.WithPassword(testDBPassword),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second),
		),
	)
	if err != nil {
		slog.Error("failed to start postgres container", "error", err)
		return 1
	}
	defer func() {
		if termErr := testcontainers.TerminateContainer(pgCtr); termErr != nil {
			slog.Warn("failed to terminate postgres container", "error", termErr)
		}
	}()

	dsn, err := pgCtr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		slog.Error("failed to get postgres connection string", "error", err)
		return 1
	}
	testDSN = dsn

	// Apply migrations via the goose library against a standard sql.DB handle.
	// This is the same migration path the production docker-compose migrate sidecar
	// and CI deploy.yml use — same files, same goose dialect, different target.
	if err := applyMigrations(dsn); err != nil {
		slog.Error("migration failed", "error", err)
		return 1
	}

	// Open the pgxpool (with pgvector type registration) for the test suite.
	pool, err := store.New(ctx, dsn)
	if err != nil {
		slog.Error("failed to open test pool", "error", err)
		return 1
	}
	defer pool.Close()
	testPool = pool

	// Default the embedder to a deterministic mock so the suite does not pay
	// the ONNX cold-start + per-encode cost. Tests that need semantic
	// validation opt back into real ONNX via requireRealEmbedder(t).
	restoreEmbed := embed.SetForTesting(embed.NewMockEncoder())
	defer restoreEmbed()

	return m.Run()
}

// applyMigrations opens a database/sql connection and calls goose.Up so the
// pgvector schema lands in the ephemeral container before any test runs.
func applyMigrations(dsn string) error {
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

// checkDockerAvailable probes the Docker daemon and returns a non-nil error
// when the daemon is unreachable. This lets TestMain skip gracefully rather
// than failing with a confusing connection-refused message.
func checkDockerAvailable(ctx context.Context) error {
	provider, err := testcontainers.ProviderDocker.GetProvider()
	if err != nil {
		return err
	}
	return provider.Health(ctx)
}

// NewTestPool returns the package-level *pgxpool.Pool with the pgvector schema
// already applied. Call this from any integration test in this package.
// It calls t.Skip if the pool is nil, which happens when Docker is unavailable.
func NewTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if testPool == nil {
		t.Skip("testPool is nil — Docker daemon was not available when the suite started")
	}
	return testPool
}

// CleanDB truncates the three core tables between tests so each test starts
// with a clean slate. CASCADE handles foreign-key ordering automatically.
func CleanDB(t *testing.T) {
	t.Helper()
	pool := NewTestPool(t)

	ctx := context.Background()
	_, err := pool.Exec(ctx,
		"truncate table relations, observations, nodes restart identity cascade",
	)
	if err != nil {
		t.Fatalf("CleanDB: truncate failed: %v", err)
	}
}
