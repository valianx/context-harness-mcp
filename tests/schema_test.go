package tests

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestSchemaApplied verifies that all three migrations applied cleanly:
//   - the vector extension is enabled (AC-3)
//   - the nodes, observations, and relations tables exist with the expected
//     columns, including deleted_at and the renamed FK columns (AC-2)
//   - the v_active_nodes view from migration 00003 exists
func TestSchemaApplied(t *testing.T) {
	pool := NewTestPool(t)
	CleanDB(t)
	ctx := context.Background()

	t.Run("vector_extension_enabled", func(t *testing.T) {
		var extname string
		err := pool.QueryRow(ctx,
			"select extname from pg_extension where extname = 'vector'",
		).Scan(&extname)
		if err != nil {
			t.Fatalf("vector extension not found: %v", err)
		}
		if extname != "vector" {
			t.Fatalf("expected extname = 'vector', got %q", extname)
		}
	})

	t.Run("nodes_table_has_deleted_at", func(t *testing.T) {
		assertColumnExists(t, ctx, pool, "nodes", "deleted_at")
	})

	t.Run("nodes_table_has_node_type", func(t *testing.T) {
		assertColumnExists(t, ctx, pool, "nodes", "node_type")
	})

	t.Run("observations_table_has_deleted_at", func(t *testing.T) {
		assertColumnExists(t, ctx, pool, "observations", "deleted_at")
	})

	t.Run("observations_table_has_node_id", func(t *testing.T) {
		assertColumnExists(t, ctx, pool, "observations", "node_id")
	})

	t.Run("relations_table_has_deleted_at", func(t *testing.T) {
		assertColumnExists(t, ctx, pool, "relations", "deleted_at")
	})

	t.Run("relations_table_has_from_node_id", func(t *testing.T) {
		assertColumnExists(t, ctx, pool, "relations", "from_node_id")
	})

	t.Run("relations_table_has_to_node_id", func(t *testing.T) {
		assertColumnExists(t, ctx, pool, "relations", "to_node_id")
	})

	t.Run("observations_has_embedding_column", func(t *testing.T) {
		assertColumnExists(t, ctx, pool, "observations", "embedding")
	})

	t.Run("v_active_nodes_view_exists", func(t *testing.T) {
		var viewname string
		err := pool.QueryRow(ctx,
			"select viewname from pg_views where schemaname = 'public' and viewname = 'v_active_nodes'",
		).Scan(&viewname)
		if err != nil {
			t.Fatalf("v_active_nodes view not found: %v", err)
		}
	})

	// Verify the old names no longer exist (migration success check).
	t.Run("entities_table_does_not_exist", func(t *testing.T) {
		var tablename string
		err := pool.QueryRow(ctx,
			"select tablename from pg_tables where schemaname = 'public' and tablename = 'entities'",
		).Scan(&tablename)
		if err == nil {
			t.Fatal("entities table still exists — migration 00003 may not have applied")
		}
	})

	t.Run("v_active_entities_view_does_not_exist", func(t *testing.T) {
		var viewname string
		err := pool.QueryRow(ctx,
			"select viewname from pg_views where schemaname = 'public' and viewname = 'v_active_entities'",
		).Scan(&viewname)
		if err == nil {
			t.Fatal("v_active_entities view still exists — migration 00003 may not have applied")
		}
	})
}

// TestNodeTypeCheckConstraint verifies that the node_type CHECK constraint
// allows all 9 valid values and rejects an invalid one.
func TestNodeTypeCheckConstraint(t *testing.T) {
	pool := NewTestPool(t)
	ctx := context.Background()

	validTypes := []string{
		"pattern", "error", "constraint", "decision",
		"tool-gotcha", "process-insight",
		"project", "service", "stack-profile",
	}

	for _, nt := range validTypes {
		CleanDB(t)
		t.Run("allows_"+nt, func(t *testing.T) {
			_, err := pool.Exec(ctx,
				"insert into nodes (name, node_type) values ($1, $2)",
				"test-node-"+nt, nt,
			)
			if err != nil {
				t.Fatalf("expected insert to succeed for node_type=%q, got: %v", nt, err)
			}
		})
	}

	CleanDB(t)
	t.Run("rejects_invalid_node_type", func(t *testing.T) {
		_, err := pool.Exec(ctx,
			"insert into nodes (name, node_type) values ($1, $2)",
			"bad-node", "not-a-valid-type",
		)
		if err == nil {
			t.Fatal("expected insert to fail for invalid node_type, but it succeeded")
		}
	})
}

// TestRelationTypeCheckConstraint verifies the relation_type CHECK constraint.
func TestRelationTypeCheckConstraint(t *testing.T) {
	pool := NewTestPool(t)
	ctx := context.Background()

	// Insert a pair of nodes to satisfy FK constraints.
	CleanDB(t)
	_, err := pool.Exec(ctx,
		"insert into nodes (name, node_type) values ('from-node', 'pattern'), ('to-node', 'pattern')",
	)
	if err != nil {
		t.Fatalf("setup: insert nodes: %v", err)
	}

	var fromID, toID string
	if err := pool.QueryRow(ctx, "select id from nodes where name = 'from-node'").Scan(&fromID); err != nil {
		t.Fatalf("setup: get from_node_id: %v", err)
	}
	if err := pool.QueryRow(ctx, "select id from nodes where name = 'to-node'").Scan(&toID); err != nil {
		t.Fatalf("setup: get to_node_id: %v", err)
	}

	validTypes := []string{"relates_to", "belongs-to", "calls", "uses-stack", "depends-on"}

	for _, rt := range validTypes {
		t.Run("allows_"+rt, func(t *testing.T) {
			_, err := pool.Exec(ctx,
				"insert into relations (from_node_id, to_node_id, relation_type) values ($1, $2, $3)",
				fromID, toID, rt,
			)
			if err != nil {
				t.Fatalf("expected insert to succeed for relation_type=%q, got: %v", rt, err)
			}
			// Remove so the unique constraint doesn't block the next iteration.
			pool.Exec(ctx, "delete from relations where relation_type = $1", rt) //nolint:errcheck
		})
	}

	t.Run("rejects_invalid_relation_type", func(t *testing.T) {
		_, err := pool.Exec(ctx,
			"insert into relations (from_node_id, to_node_id, relation_type) values ($1, $2, $3)",
			fromID, toID, "is-called-by",
		)
		if err == nil {
			t.Fatal("expected insert to fail for invalid relation_type, but it succeeded")
		}
	})
}

// TestSoftDeletePartialIndexes verifies that soft-deleted rows are excluded
// from the v_active_nodes view.
func TestSoftDeletePartialIndexes(t *testing.T) {
	pool := NewTestPool(t)
	CleanDB(t)
	ctx := context.Background()

	_, err := pool.Exec(ctx,
		"insert into nodes (name, node_type) values ('active-node', 'pattern'), ('deleted-node', 'pattern')",
	)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	_, err = pool.Exec(ctx, "update nodes set deleted_at = now() where name = 'deleted-node'")
	if err != nil {
		t.Fatalf("soft delete: %v", err)
	}

	var count int
	if err := pool.QueryRow(ctx, "select count(*) from v_active_nodes").Scan(&count); err != nil {
		t.Fatalf("query v_active_nodes: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 active node, got %d", count)
	}
}

// TestUniqueConstraints verifies that the three unique constraints defined in
// 00001_init.sql prevent duplicate rows on: nodes.name, the
// (node_id, text) pair in observations, and the
// (from_node_id, to_node_id, relation_type) triple in relations.
func TestUniqueConstraints(t *testing.T) {
	pool := NewTestPool(t)
	ctx := context.Background()

	t.Run("nodes_name_unique", func(t *testing.T) {
		CleanDB(t)
		_, err := pool.Exec(ctx,
			"insert into nodes (name, node_type) values ('unique-node', 'pattern')",
		)
		if err != nil {
			t.Fatalf("first insert: %v", err)
		}
		_, err = pool.Exec(ctx,
			"insert into nodes (name, node_type) values ('unique-node', 'decision')",
		)
		if err == nil {
			t.Fatal("expected duplicate-name insert to fail, but it succeeded")
		}
	})

	t.Run("observations_node_text_unique", func(t *testing.T) {
		CleanDB(t)
		_, err := pool.Exec(ctx,
			"insert into nodes (name, node_type) values ('obs-dedup-node', 'pattern')",
		)
		if err != nil {
			t.Fatalf("setup node: %v", err)
		}
		var nodeID string
		if err := pool.QueryRow(ctx, "select id from nodes where name = 'obs-dedup-node'").Scan(&nodeID); err != nil {
			t.Fatalf("get node id: %v", err)
		}
		_, err = pool.Exec(ctx,
			"insert into observations (node_id, text) values ($1, 'same observation text')", nodeID,
		)
		if err != nil {
			t.Fatalf("first observation insert: %v", err)
		}
		_, err = pool.Exec(ctx,
			"insert into observations (node_id, text) values ($1, 'same observation text')", nodeID,
		)
		if err == nil {
			t.Fatal("expected duplicate (node_id, text) insert to fail, but it succeeded")
		}
	})

	t.Run("relations_triple_unique", func(t *testing.T) {
		CleanDB(t)
		_, err := pool.Exec(ctx,
			"insert into nodes (name, node_type) values ('rel-from', 'pattern'), ('rel-to', 'pattern')",
		)
		if err != nil {
			t.Fatalf("setup nodes: %v", err)
		}
		var fromID, toID string
		if err := pool.QueryRow(ctx, "select id from nodes where name = 'rel-from'").Scan(&fromID); err != nil {
			t.Fatalf("get from id: %v", err)
		}
		if err := pool.QueryRow(ctx, "select id from nodes where name = 'rel-to'").Scan(&toID); err != nil {
			t.Fatalf("get to id: %v", err)
		}
		_, err = pool.Exec(ctx,
			"insert into relations (from_node_id, to_node_id, relation_type) values ($1, $2, 'relates_to')",
			fromID, toID,
		)
		if err != nil {
			t.Fatalf("first relation insert: %v", err)
		}
		_, err = pool.Exec(ctx,
			"insert into relations (from_node_id, to_node_id, relation_type) values ($1, $2, 'relates_to')",
			fromID, toID,
		)
		if err == nil {
			t.Fatal("expected duplicate (from_node_id, to_node_id, relation_type) insert to fail, but it succeeded")
		}
	})
}

// assertColumnExists fails the test if the given column is not present on the
// given table in the public schema.
func assertColumnExists(t *testing.T, ctx context.Context, pool *pgxpool.Pool, table, column string) {
	t.Helper()
	var colname string
	err := pool.QueryRow(ctx,
		"select column_name from information_schema.columns where table_schema = 'public' and table_name = $1 and column_name = $2",
		table, column,
	).Scan(&colname)
	if err != nil {
		t.Fatalf("column %q not found on table %q: %v", column, table, err)
	}
}
