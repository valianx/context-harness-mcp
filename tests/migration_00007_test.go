// Package tests — integration tests for migration 00007_project_scope.
// Covers AC-1 and AC-2 of PR-1 (feat/db-project-scope).
//
// Each test that requires a fresh schema state uses an ephemeral testcontainer
// so it is fully isolated from the shared testPool (which has all migrations).
package tests

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/pressly/goose/v3"
)

// ── helpers local to this file ────────────────────────────────────────────────

// newEphemeralContainer spins up a fresh pgvector container and returns a
// *sql.DB connected to it plus a cleanup func. The caller should defer cleanup.
func newEphemeralContainer(t *testing.T) (db *sql.DB, cleanup func()) {
	t.Helper()
	ctx := context.Background()

	if err := checkDockerAvailable(ctx); err != nil {
		t.Skipf("Docker daemon unavailable: %v", err)
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
	require.NoError(t, err, "iniciar contenedor postgres efímero")

	dsn, err := pgCtr.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err, "obtener connection string")

	db, err = sql.Open("pgx", dsn)
	require.NoError(t, err, "abrir sql.DB")

	cleanup = func() {
		db.Close()
		if termErr := testcontainers.TerminateContainer(pgCtr); termErr != nil {
			t.Logf("advertencia: fallo al terminar contenedor: %v", termErr)
		}
	}
	return db, cleanup
}

// ── AC-1: TestMigration00007_AppliesCleanly ───────────────────────────────────

// TestMigration00007_AppliesCleanly applies migrations 00001–00007 to a fresh
// container and asserts that:
//   - nodes.project_id is text NOT NULL DEFAULT 'global'
//   - relations.project_id is text NOT NULL DEFAULT 'global'
//   - the legacy constraint entities_name_key is gone
//   - the composite unique nodes_project_name_key exists
//   - indexes nodes_project_name_active_idx, nodes_project_id_active_idx, and
//     relations_project_id_active_idx are present
//
// AC-1: Given a DB with migrations 00001-00006 applied, When goose up runs
// 00007, Then the schema matches the spec.
func TestMigration00007_AppliesCleanly(t *testing.T) {
	db, cleanup := newEphemeralContainer(t)
	defer cleanup()
	ctx := context.Background()

	require.NoError(t, goose.SetDialect("postgres"))
	require.NoError(t, goose.UpTo(db, migrationsDir, 7),
		"goose.UpTo(7) debe aplicar 00001..00007 sin error")

	// ── nodes.project_id column ──────────────────────────────────────────────
	t.Run("nodes_project_id_column", func(t *testing.T) {
		var dataType, isNullable, colDefault string
		err := db.QueryRowContext(ctx,
			`select data_type, is_nullable,
			        coalesce(column_default, '')
			   from information_schema.columns
			  where table_schema = 'public'
			    and table_name   = 'nodes'
			    and column_name  = 'project_id'`,
		).Scan(&dataType, &isNullable, &colDefault)
		require.NoError(t, err, "nodes.project_id debe existir")
		assert.Equal(t, "text", dataType, "nodes.project_id debe ser text")
		assert.Equal(t, "NO", isNullable, "nodes.project_id debe ser NOT NULL")
		assert.Contains(t, colDefault, "global", "nodes.project_id debe tener DEFAULT 'global'")
	})

	// ── relations.project_id column ──────────────────────────────────────────
	t.Run("relations_project_id_column", func(t *testing.T) {
		var dataType, isNullable, colDefault string
		err := db.QueryRowContext(ctx,
			`select data_type, is_nullable,
			        coalesce(column_default, '')
			   from information_schema.columns
			  where table_schema = 'public'
			    and table_name   = 'relations'
			    and column_name  = 'project_id'`,
		).Scan(&dataType, &isNullable, &colDefault)
		require.NoError(t, err, "relations.project_id debe existir")
		assert.Equal(t, "text", dataType, "relations.project_id debe ser text")
		assert.Equal(t, "NO", isNullable, "relations.project_id debe ser NOT NULL")
		assert.Contains(t, colDefault, "global", "relations.project_id debe tener DEFAULT 'global'")
	})

	// ── legacy constraint entities_name_key is dropped ───────────────────────
	t.Run("legacy_constraint_dropped", func(t *testing.T) {
		var count int
		err := db.QueryRowContext(ctx,
			`select count(*) from pg_constraint
			  where conname = 'entities_name_key'
			    and conrelid = 'public.nodes'::regclass`,
		).Scan(&count)
		require.NoError(t, err)
		assert.Equal(t, 0, count,
			"constraint entities_name_key debe estar eliminado tras 00007")
	})

	// ── composite unique nodes_project_name_key exists ───────────────────────
	t.Run("composite_unique_nodes_project_name_key", func(t *testing.T) {
		var constraintDef string
		err := db.QueryRowContext(ctx,
			`select pg_get_constraintdef(oid)
			   from pg_constraint
			  where conname    = 'nodes_project_name_key'
			    and conrelid   = 'public.nodes'::regclass`,
		).Scan(&constraintDef)
		require.NoError(t, err,
			"constraint nodes_project_name_key debe existir")
		assert.Contains(t, constraintDef, "project_id",
			"el constraint debe incluir project_id")
		assert.Contains(t, constraintDef, "name",
			"el constraint debe incluir name")
	})

	// ── new indexes exist ─────────────────────────────────────────────────────
	expectedIndexes := []struct {
		table string
		index string
	}{
		{"nodes", "nodes_project_name_active_idx"},
		{"nodes", "nodes_project_id_active_idx"},
		{"relations", "relations_project_id_active_idx"},
	}

	for _, idx := range expectedIndexes {
		idx := idx
		t.Run(fmt.Sprintf("index_%s", idx.index), func(t *testing.T) {
			var indexName string
			err := db.QueryRowContext(ctx,
				`select indexname from pg_indexes
				  where schemaname = 'public'
				    and tablename  = $1
				    and indexname  = $2`,
				idx.table, idx.index,
			).Scan(&indexName)
			require.NoError(t, err,
				"índice %s debe existir en tabla %s", idx.index, idx.table)
			assert.Equal(t, idx.index, indexName)
		})
	}
}

// ── AC-2: TestMigration00007_BackfillsExistingRowsToGlobal ───────────────────

// TestMigration00007_BackfillsExistingRowsToGlobal seeds nodes before applying
// 00007, then applies it and asserts all rows have project_id = 'global'.
//
// AC-2: Given existing rows, When migration 00007 runs, Then all rows have
// project_id = 'global'.
func TestMigration00007_BackfillsExistingRowsToGlobal(t *testing.T) {
	db, cleanup := newEphemeralContainer(t)
	defer cleanup()
	ctx := context.Background()

	require.NoError(t, goose.SetDialect("postgres"))

	// Apply 00001..00006 only — no project_id column yet.
	require.NoError(t, goose.UpTo(db, migrationsDir, 6),
		"goose.UpTo(6) debe aplicar 00001..00006 sin error")

	// Seed 5 nodes using the pre-00007 schema (node_type column exists after
	// the 00003 rename migration).
	nodeNames := []string{"node-alpha", "node-beta", "node-gamma", "node-delta", "node-epsilon"}
	for _, name := range nodeNames {
		_, err := db.ExecContext(ctx,
			`insert into nodes (name, node_type) values ($1, 'pattern')`,
			name,
		)
		require.NoError(t, err, "insertar nodo pre-existente: %s", name)
	}

	// Seed 3 relations between the first 3 nodes.
	// First we need node IDs.
	type nodeRow struct{ id, name string }
	var seededNodes []nodeRow
	rows, err := db.QueryContext(ctx,
		`select id::text, name from nodes where name = any($1) order by name`,
		nodeNames[:3],
	)
	require.NoError(t, err, "query node IDs para relaciones")
	for rows.Next() {
		var nr nodeRow
		require.NoError(t, rows.Scan(&nr.id, &nr.name))
		seededNodes = append(seededNodes, nr)
	}
	require.NoError(t, rows.Err())
	require.Len(t, seededNodes, 3, "deben existir 3 nodos en la DB antes de 00007")

	for i := 0; i < 2; i++ {
		_, err := db.ExecContext(ctx,
			`insert into relations (from_node_id, to_node_id, relation_type)
			 values ($1::uuid, $2::uuid, 'relates_to')`,
			seededNodes[i].id, seededNodes[i+1].id,
		)
		require.NoError(t, err, "insertar relación pre-existente %d→%d", i, i+1)
	}

	// Now apply 00007 — the backfill happens via DEFAULT 'global' (fast-path).
	require.NoError(t, goose.UpTo(db, migrationsDir, 7),
		"goose.UpTo(7) debe aplicar 00007 sin error")

	// Verify: zero nodes with project_id != 'global'.
	t.Run("nodes_backfilled_to_global", func(t *testing.T) {
		var nonGlobalCount int
		require.NoError(t, db.QueryRowContext(ctx,
			`select count(*) from nodes where project_id != 'global'`,
		).Scan(&nonGlobalCount))
		assert.Equal(t, 0, nonGlobalCount,
			"todos los nodos pre-existentes deben tener project_id = 'global'")
	})

	// Verify: zero relations with project_id != 'global'.
	t.Run("relations_backfilled_to_global", func(t *testing.T) {
		var nonGlobalCount int
		require.NoError(t, db.QueryRowContext(ctx,
			`select count(*) from relations where project_id != 'global'`,
		).Scan(&nonGlobalCount))
		assert.Equal(t, 0, nonGlobalCount,
			"todas las relaciones pre-existentes deben tener project_id = 'global'")
	})
}

// ── AC-1 (behavior): TestMigration00007_AllowsSameNameAcrossProjects ─────────

// TestMigration00007_AllowsSameNameAcrossProjects verifies that the new
// composite UNIQUE(project_id, name) allows homonyms in different projects but
// rejects duplicates within the same project.
//
// This exercises the behavioral guarantee from AC-1 (the constraint shape) and
// the same-project uniqueness rule documented in the migration.
func TestMigration00007_AllowsSameNameAcrossProjects(t *testing.T) {
	db, cleanup := newEphemeralContainer(t)
	defer cleanup()
	ctx := context.Background()

	require.NoError(t, goose.SetDialect("postgres"))
	require.NoError(t, goose.UpTo(db, migrationsDir, 7),
		"goose.UpTo(7) debe aplicar 00001..00007 sin error")

	// Insert node 'foo' in project 'global'.
	_, err := db.ExecContext(ctx,
		`insert into nodes (name, node_type, project_id) values ('foo', 'pattern', 'global')`,
	)
	require.NoError(t, err, "INSERT nodo 'foo' en project 'global' debe tener éxito")

	// Insert node 'foo' in project 'team-a' — same name, different project: must succeed.
	t.Run("same_name_different_project_allowed", func(t *testing.T) {
		_, err := db.ExecContext(ctx,
			`insert into nodes (name, node_type, project_id) values ('foo', 'pattern', 'team-a')`,
		)
		assert.NoError(t, err,
			"INSERT 'foo' en project 'team-a' debe ser permitido (homónimos cross-project)")
	})

	// Insert node 'foo' in project 'global' again — same (project_id, name): must fail.
	t.Run("same_name_same_project_rejected", func(t *testing.T) {
		_, err := db.ExecContext(ctx,
			`insert into nodes (name, node_type, project_id) values ('foo', 'pattern', 'global')`,
		)
		require.Error(t, err,
			"INSERT duplicado ('global', 'foo') debe fallar con violación de UNIQUE")
		assert.Contains(t, err.Error(), "unique",
			"el error debe mencionar la violación de constraint unique")
	})
}
