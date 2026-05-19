package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/mariogutierrez/context-harness-mcp/internal/khctl"
	"github.com/mariogutierrez/context-harness-mcp/internal/store"
)

const (
	khctlTestDBName     = "khctl_test"
	khctlTestDBUser     = "test"
	khctlTestDBPassword = "test"
	pgvectorImage       = "pgvector/pgvector:pg16"
	// migrationsDir is relative to the package directory (cmd/khctl/).
	migrationsDir = "../../migrations"
)

// suite-level state shared across khctl tests.
var (
	khctlPool *pgxpool.Pool
	khctlDSN  string
)

func TestMain(m *testing.M) {
	os.Exit(runKhctlSuite(m))
}

func runKhctlSuite(m *testing.M) int {
	ctx := context.Background()

	provider, err := testcontainers.ProviderDocker.GetProvider()
	if err != nil {
		fmt.Fprintln(os.Stderr, "Docker daemon not available — skipping khctl integration tests:", err)
		return 0
	}
	if err := provider.Health(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "Docker daemon not healthy — skipping khctl integration tests:", err)
		return 0
	}

	pgCtr, err := tcpostgres.Run(ctx,
		pgvectorImage,
		tcpostgres.WithDatabase(khctlTestDBName),
		tcpostgres.WithUsername(khctlTestDBUser),
		tcpostgres.WithPassword(khctlTestDBPassword),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second),
		),
	)
	if err != nil {
		fmt.Fprintln(os.Stderr, "failed to start postgres container:", err)
		return 1
	}
	defer func() { _ = testcontainers.TerminateContainer(pgCtr) }()

	dsn, err := pgCtr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		fmt.Fprintln(os.Stderr, "failed to get connection string:", err)
		return 1
	}
	khctlDSN = dsn

	if err := applyKhctlMigrations(dsn); err != nil {
		fmt.Fprintln(os.Stderr, "migration failed:", err)
		return 1
	}

	pool, err := store.New(ctx, dsn)
	if err != nil {
		fmt.Fprintln(os.Stderr, "failed to open pool:", err)
		return 1
	}
	defer pool.Close()
	khctlPool = pool

	return m.Run()
}

func applyKhctlMigrations(dsn string) error {
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

func cleanDB(t *testing.T) {
	t.Helper()
	if khctlPool == nil {
		t.Skip("khctlPool is nil — Docker daemon was not available")
	}
	_, err := khctlPool.Exec(context.Background(),
		"TRUNCATE TABLE relations, observations, nodes RESTART IDENTITY CASCADE")
	require.NoError(t, err, "cleanDB: truncate failed")
}

// TestImport_NewShape verifies that importing a {"nodes": [...]} payload
// inserts the expected rows.
func TestImport_NewShape(t *testing.T) {
	if khctlPool == nil {
		t.Skip("Docker daemon not available")
	}
	t.Cleanup(func() { cleanDB(t) })

	payload := map[string]any{
		"format_version": "2",
		"nodes": []map[string]any{
			{
				"name":         "import-test-pattern",
				"nodeType":     "pattern",
				"observations": []string{"First observation.", "Second observation."},
			},
		},
		"relations": []map[string]any{},
	}
	data, err := json.Marshal(payload)
	require.NoError(t, err)

	nodes, relations, err := khctl.ParseImportPayload(data)
	require.NoError(t, err)

	nodesIn, obsIn, _, relsIn, _, err := khctl.RunImport(context.Background(), khctlPool, nodes, relations)
	require.NoError(t, err)
	assert.Equal(t, 1, nodesIn, "expected 1 node inserted")
	assert.Equal(t, 2, obsIn, "expected 2 observations inserted")
	assert.Equal(t, 0, relsIn, "expected 0 relations inserted")

	var count int
	err = khctlPool.QueryRow(context.Background(),
		"SELECT COUNT(*) FROM nodes WHERE name = 'import-test-pattern' AND deleted_at IS NULL",
	).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 1, count, "node must be present in DB")
}

// TestImport_LegacyEntitiesShape verifies that the legacy {"entities": [...]}
// shape is accepted and correctly imported.
func TestImport_LegacyEntitiesShape(t *testing.T) {
	if khctlPool == nil {
		t.Skip("Docker daemon not available")
	}
	t.Cleanup(func() { cleanDB(t) })

	payload := map[string]any{
		"format_version": "0.1.0",
		"entities": []map[string]any{
			{
				"name":         "legacy-entity-node",
				"entityType":   "decision",
				"observations": []string{"A legacy observation."},
			},
		},
		"relations": []map[string]any{},
	}
	data, err := json.Marshal(payload)
	require.NoError(t, err)

	nodes, relations, err := khctl.ParseImportPayload(data)
	require.NoError(t, err)
	require.Len(t, nodes, 1, "expected 1 node parsed from legacy shape")
	assert.Equal(t, "decision", nodes[0].NodeType, "entityType must be normalized to nodeType")

	nodesIn, _, _, _, _, err := khctl.RunImport(context.Background(), khctlPool, nodes, relations)
	require.NoError(t, err)
	assert.Equal(t, 1, nodesIn, "expected 1 node inserted from legacy shape")
}

// TestImport_Idempotent verifies that re-importing the same payload adds zero new rows.
func TestImport_Idempotent(t *testing.T) {
	if khctlPool == nil {
		t.Skip("Docker daemon not available")
	}
	t.Cleanup(func() { cleanDB(t) })

	payload := map[string]any{
		"nodes": []map[string]any{
			{
				"name":         "idempotent-node",
				"nodeType":     "pattern",
				"observations": []string{"Idempotent observation."},
			},
		},
		"relations": []map[string]any{},
	}
	data, err := json.Marshal(payload)
	require.NoError(t, err)

	nodes, relations, err := khctl.ParseImportPayload(data)
	require.NoError(t, err)

	// First import.
	nodesIn, obsIn, _, _, _, err := khctl.RunImport(context.Background(), khctlPool, nodes, relations)
	require.NoError(t, err)
	assert.Equal(t, 1, nodesIn)
	assert.Equal(t, 1, obsIn)

	// Second import — must be all-dedup.
	nodesIn2, obsIn2, obsDedup, _, _, err := khctl.RunImport(context.Background(), khctlPool, nodes, relations)
	require.NoError(t, err)
	assert.Equal(t, 0, nodesIn2, "second import: no new nodes")
	assert.Equal(t, 0, obsIn2, "second import: no new observations")
	assert.Equal(t, 1, obsDedup, "second import: observation deduped")
}

// TestImport_EmbeddingValidation verifies that embeddings with wrong
// dimensionality are rejected.
func TestImport_EmbeddingValidation(t *testing.T) {
	if khctlPool == nil {
		t.Skip("Docker daemon not available")
	}
	t.Cleanup(func() { cleanDB(t) })

	payload := map[string]any{
		"nodes": []map[string]any{
			{
				"name":         "emb-validation-node",
				"nodeType":     "pattern",
				"observations": []string{"obs with wrong dim embedding"},
				"embeddings":   [][]float32{{1.0, 2.0, 3.0}}, // only 3 dims instead of 384
			},
		},
		"relations": []map[string]any{},
	}
	data, err := json.Marshal(payload)
	require.NoError(t, err)

	nodes, relations, err := khctl.ParseImportPayload(data)
	require.NoError(t, err)

	_, _, _, _, _, err = khctl.RunImport(context.Background(), khctlPool, nodes, relations)
	require.Error(t, err, "import with wrong embedding dims must fail")
	assert.Contains(t, err.Error(), "expected 384-dim embedding, got 3")
}

// TestImport_RelationsSkipMissingNodes verifies that relations whose from/to
// nodes are absent are silently skipped.
func TestImport_RelationsSkipMissingNodes(t *testing.T) {
	if khctlPool == nil {
		t.Skip("Docker daemon not available")
	}
	t.Cleanup(func() { cleanDB(t) })

	payload := map[string]any{
		"nodes": []map[string]any{
			{
				"name":         "present-node",
				"nodeType":     "pattern",
				"observations": []string{"present"},
			},
		},
		"relations": []map[string]any{
			{"from": "present-node", "to": "absent-node", "relationType": "relates_to"},
		},
	}
	data, err := json.Marshal(payload)
	require.NoError(t, err)

	nodes, relations, err := khctl.ParseImportPayload(data)
	require.NoError(t, err)

	_, _, _, relsIn, _, err := khctl.RunImport(context.Background(), khctlPool, nodes, relations)
	require.NoError(t, err)
	assert.Equal(t, 0, relsIn, "relation referencing absent node must be skipped")
}

// unused keeps khctlDSN referenced to avoid compiler error if tests don't use it.
var _ = khctlDSN
