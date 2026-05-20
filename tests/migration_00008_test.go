// Package tests — integration tests for migration 00008_taxonomy_extend.
// Covers AC-3 and AC-8 of PR-1 (feat/db-project-scope).
package tests

import (
	"context"
	"fmt"
	"testing"

	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── AC-3: TestMigration00008_AppliesCleanly ───────────────────────────────────

// TestMigration00008_AppliesCleanly applies migrations 00001–00008 and verifies
// that the relations_relation_type_check constraint now lists all 7 values.
//
// AC-3: Given a DB with 00001-00007 applied, When goose up runs 00008,
// Then the CHECK constraint contains all 7 relation type literals.
func TestMigration00008_AppliesCleanly(t *testing.T) {
	db, cleanup := newEphemeralContainer(t)
	defer cleanup()
	ctx := context.Background()

	require.NoError(t, goose.SetDialect("postgres"))
	require.NoError(t, goose.UpTo(db, migrationsDir, 8),
		"goose.UpTo(8) debe aplicar 00001..00008 sin error")

	// Verify the CHECK constraint definition contains all 7 relation types.
	t.Run("check_constraint_has_7_values", func(t *testing.T) {
		var constraintDef string
		err := db.QueryRowContext(ctx,
			`select pg_get_constraintdef(oid)
			   from pg_constraint
			  where conname = 'relations_relation_type_check'`,
		).Scan(&constraintDef)
		require.NoError(t, err,
			"constraint relations_relation_type_check debe existir tras 00008")

		expectedValues := []string{
			"relates_to",
			"belongs-to",
			"calls",
			"uses-stack",
			"depends-on",
			"supersedes",
			"conflicts_with",
		}
		for _, v := range expectedValues {
			assert.Contains(t, constraintDef, v,
				"el CHECK debe incluir el valor %q", v)
		}
	})

	// AC-8: Verify all 7 relation types are accepted by inserting a relation with
	// each type. We seed a pair of nodes first, then attempt one INSERT per type.
	t.Run("all_7_types_accepted_by_insert", func(t *testing.T) {
		// Seed a pair of nodes to use as from/to for all relation inserts.
		_, err := db.ExecContext(ctx,
			`insert into nodes (name, node_type, project_id)
			 values ('src-node', 'pattern', 'global'),
			        ('dst-node', 'pattern', 'global')`,
		)
		require.NoError(t, err, "insertar par de nodos de prueba")

		var fromID, toID string
		require.NoError(t, db.QueryRowContext(ctx,
			`select id::text from nodes where name = 'src-node'`,
		).Scan(&fromID))
		require.NoError(t, db.QueryRowContext(ctx,
			`select id::text from nodes where name = 'dst-node'`,
		).Scan(&toID))

		allTypes := []string{
			"relates_to",
			"belongs-to",
			"calls",
			"uses-stack",
			"depends-on",
			"supersedes",
			"conflicts_with",
		}

		for _, rt := range allTypes {
			rt := rt
			t.Run(fmt.Sprintf("type_%s", rt), func(t *testing.T) {
				_, err := db.ExecContext(ctx,
					`insert into relations
					        (from_node_id, to_node_id, relation_type, project_id)
					 values ($1::uuid, $2::uuid, $3, 'global')`,
					fromID, toID, rt,
				)
				assert.NoError(t, err,
					"INSERT con relation_type=%q debe tener éxito", rt)

				// Clean up for the next subtest (UNIQUE constraint on the triple).
				_, _ = db.ExecContext(ctx,
					`delete from relations
					  where from_node_id = $1::uuid
					    and to_node_id   = $2::uuid
					    and relation_type = $3`,
					fromID, toID, rt,
				)
			})
		}
	})
}

// ── AC-8 (negative): TestMigration00008_RejectsInvalidType ───────────────────

// TestMigration00008_RejectsInvalidType verifies that inserting a relation with
// an unsupported relation_type is rejected by the CHECK constraint.
//
// AC-8 (negative path): INSERT with relation_type='invalid_type' must fail.
func TestMigration00008_RejectsInvalidType(t *testing.T) {
	db, cleanup := newEphemeralContainer(t)
	defer cleanup()
	ctx := context.Background()

	require.NoError(t, goose.SetDialect("postgres"))
	require.NoError(t, goose.UpTo(db, migrationsDir, 8),
		"goose.UpTo(8) debe aplicar 00001..00008 sin error")

	// Seed a pair of nodes.
	_, err := db.ExecContext(ctx,
		`insert into nodes (name, node_type, project_id)
		 values ('from-node', 'pattern', 'global'),
		        ('to-node', 'pattern', 'global')`,
	)
	require.NoError(t, err, "insertar nodos de prueba")

	var fromID, toID string
	require.NoError(t, db.QueryRowContext(ctx,
		`select id::text from nodes where name = 'from-node'`,
	).Scan(&fromID))
	require.NoError(t, db.QueryRowContext(ctx,
		`select id::text from nodes where name = 'to-node'`,
	).Scan(&toID))

	_, err = db.ExecContext(ctx,
		`insert into relations
		        (from_node_id, to_node_id, relation_type, project_id)
		 values ($1::uuid, $2::uuid, 'invalid_type', 'global')`,
		fromID, toID,
	)
	require.Error(t, err,
		"INSERT con relation_type='invalid_type' debe fallar con violación de CHECK")
	assert.Contains(t, err.Error(), "check",
		"el error debe mencionar la violación del constraint CHECK")
}
