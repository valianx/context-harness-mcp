// Package store provides Postgres CRUD helpers for the context-harness-mcp
// nodes, observations, and relations tables.
//
// MarkDeletedByNames and MarkDeletedByNodeAndTexts are admin-script API only —
// they are not wired to any MCP tool. Use Supabase Studio or a dedicated admin
// script for cleanup operations.
package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	pgvector "github.com/pgvector/pgvector-go"
)

// NodeRow is a read-projection of an active nodes row.
type NodeRow struct {
	ID       string // UUID stored as string
	Name     string
	NodeType string
}

// Create inserts a new node row and returns its UUID. On a unique-name
// conflict it fetches and returns the existing active row's id, making the
// operation idempotent.
func Create(ctx context.Context, tx pgx.Tx, name, nodeType string) (string, error) {
	var id string
	err := tx.QueryRow(ctx,
		`INSERT INTO nodes (name, node_type)
		 VALUES ($1, $2)
		 ON CONFLICT (name) DO NOTHING
		 RETURNING id`,
		name, nodeType,
	).Scan(&id)

	if err == nil {
		return id, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}

	// Conflict path — the row already exists; fetch its active id.
	return findIDByName(ctx, tx, name)
}

// findIDByName is an unexported helper used only by Create on the conflict path.
func findIDByName(ctx context.Context, tx pgx.Tx, name string) (string, error) {
	var id string
	err := tx.QueryRow(ctx,
		`SELECT id FROM nodes WHERE name = $1 AND deleted_at IS NULL`,
		name,
	).Scan(&id)
	return id, err
}

// FindByName returns the id and node_type of the active node with the given
// name. found=false when no active row exists (no error is returned in that
// case).
func FindByName(ctx context.Context, tx pgx.Tx, name string) (id, nodeType string, found bool, err error) {
	err = tx.QueryRow(ctx,
		`SELECT id, node_type FROM nodes WHERE name = $1 AND deleted_at IS NULL`,
		name,
	).Scan(&id, &nodeType)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, err
	}
	return id, nodeType, true, nil
}

// MarkDeletedByNames soft-deletes all active nodes whose name is in the
// provided slice. Returns the number of rows updated.
//
// Admin-script API only — not wired to any MCP tool.
func MarkDeletedByNames(ctx context.Context, tx pgx.Tx, names []string) (int, error) {
	tag, err := tx.Exec(ctx,
		`UPDATE nodes SET deleted_at = now()
		 WHERE name = ANY($1) AND deleted_at IS NULL`,
		names,
	)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// ListActive returns all active node rows ordered by name. Used by read_graph.
func ListActive(ctx context.Context, pool *pgxpool.Pool) ([]NodeRow, error) {
	rows, err := pool.Query(ctx,
		`SELECT id, name, node_type FROM nodes WHERE deleted_at IS NULL ORDER BY name`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanNodeRows(rows)
}

// SearchByCosine returns up to 10 active nodes ranked by the minimum cosine
// distance between the query vector and each node's active observation
// embeddings. Only nodes that have at least one non-null embedding are
// considered. Results are ordered closest-first (ASC distance = most similar).
func SearchByCosine(ctx context.Context, pool *pgxpool.Pool, queryVec pgvector.Vector) ([]NodeRow, error) {
	rows, err := pool.Query(ctx,
		`SELECT n.id, n.name, n.node_type
		 FROM nodes n
		 JOIN observations o ON o.node_id = n.id
		 WHERE n.deleted_at IS NULL
		   AND o.deleted_at IS NULL
		   AND o.embedding IS NOT NULL
		 GROUP BY n.id, n.name, n.node_type
		 ORDER BY MIN(o.embedding <=> $1::vector) ASC
		 LIMIT 10`,
		queryVec,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanNodeRows(rows)
}

// SearchByTextSubstring returns active nodes that have at least one active
// observation whose text contains the query string (case-insensitive ILIKE).
// This is the substring fallback; no longer wired into search_nodes —
// kept for potential future use (e.g. ?fallback=substring query param).
func SearchByTextSubstring(ctx context.Context, pool *pgxpool.Pool, query string) ([]NodeRow, error) {
	rows, err := pool.Query(ctx,
		`SELECT DISTINCT n.id, n.name, n.node_type
		 FROM nodes n
		 JOIN observations o ON o.node_id = n.id
		 WHERE o.text ILIKE '%' || $1 || '%'
		   AND o.deleted_at IS NULL
		   AND n.deleted_at IS NULL
		 ORDER BY n.name`,
		query,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanNodeRows(rows)
}

func scanNodeRows(rows pgx.Rows) ([]NodeRow, error) {
	var result []NodeRow
	for rows.Next() {
		var r NodeRow
		if err := rows.Scan(&r.ID, &r.Name, &r.NodeType); err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	return result, rows.Err()
}
