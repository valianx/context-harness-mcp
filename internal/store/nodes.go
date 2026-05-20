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
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	pgvector "github.com/pgvector/pgvector-go"
)

// NodeSummary is the wire-shape for oldest_node / newest_node in StatsResult.
// A nil pointer means the graph is empty (zero active nodes).
type NodeSummary struct {
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

// StatsResult holds the aggregated counts returned by the stats MCP tool.
type StatsResult struct {
	NodeCount        int             `json:"node_count"`
	ObservationCount int             `json:"observation_count"`
	RelationCount    int             `json:"relation_count"`
	ByType           map[string]int  `json:"by_type"`
	OldestNode       *NodeSummary    `json:"oldest_node"`
	NewestNode       *NodeSummary    `json:"newest_node"`
}

// Stats returns aggregated counts for the active knowledge graph in a single
// round-trip using a CTE query. Soft-deleted rows (deleted_at IS NOT NULL) are
// excluded from all counts. When the graph is empty, OldestNode and NewestNode
// are nil (marshalled as JSON null) and ByType is an empty map.
func Stats(ctx context.Context, pool *pgxpool.Pool) (StatsResult, error) {
	result := StatsResult{ByType: map[string]int{}}

	// Single CTE round-trip: counts + by_type + extremes.
	// The outer SELECT returns one row even when nodes is empty (counts are
	// correlated subqueries and always return a value; the extremes subqueries
	// return NULL when the table is empty).
	const q = `
WITH counts AS (
  SELECT
    (SELECT count(*)::int FROM nodes       WHERE deleted_at IS NULL) AS node_count,
    (SELECT count(*)::int FROM observations WHERE deleted_at IS NULL) AS observation_count,
    (SELECT count(*)::int FROM relations    WHERE deleted_at IS NULL) AS relation_count
),
extremes AS (
  SELECT
    (SELECT name       FROM nodes WHERE deleted_at IS NULL ORDER BY created_at ASC  NULLS LAST LIMIT 1) AS oldest_name,
    (SELECT created_at FROM nodes WHERE deleted_at IS NULL ORDER BY created_at ASC  NULLS LAST LIMIT 1) AS oldest_at,
    (SELECT name       FROM nodes WHERE deleted_at IS NULL ORDER BY created_at DESC NULLS LAST LIMIT 1) AS newest_name,
    (SELECT created_at FROM nodes WHERE deleted_at IS NULL ORDER BY created_at DESC NULLS LAST LIMIT 1) AS newest_at
)
SELECT
  c.node_count, c.observation_count, c.relation_count,
  e.oldest_name, e.oldest_at, e.newest_name, e.newest_at
FROM counts c, extremes e`

	var (
		oldestName *string
		oldestAt   *time.Time
		newestName *string
		newestAt   *time.Time
	)

	err := pool.QueryRow(ctx, q).Scan(
		&result.NodeCount,
		&result.ObservationCount,
		&result.RelationCount,
		&oldestName, &oldestAt,
		&newestName, &newestAt,
	)
	if err != nil {
		return StatsResult{}, err
	}

	if oldestName != nil && oldestAt != nil {
		result.OldestNode = &NodeSummary{Name: *oldestName, CreatedAt: *oldestAt}
	}
	if newestName != nil && newestAt != nil {
		result.NewestNode = &NodeSummary{Name: *newestName, CreatedAt: *newestAt}
	}

	// Fetch per-type counts in a second query (cannot be combined with the
	// scalar CTE above without a GROUP BY that breaks the extremes subqueries).
	byTypeRows, err := pool.Query(ctx,
		`SELECT node_type, count(*)::int FROM nodes WHERE deleted_at IS NULL GROUP BY node_type`,
	)
	if err != nil {
		return StatsResult{}, err
	}
	defer byTypeRows.Close()

	for byTypeRows.Next() {
		var nodeType string
		var cnt int
		if err := byTypeRows.Scan(&nodeType, &cnt); err != nil {
			return StatsResult{}, err
		}
		result.ByType[nodeType] = cnt
	}
	if err := byTypeRows.Err(); err != nil {
		return StatsResult{}, err
	}

	return result, nil
}

// NodeRow is a read-projection of an active nodes row.
type NodeRow struct {
	ID       string // UUID stored as string
	Name     string
	NodeType string
}

// Create inserts a new node row and returns its UUID. On a unique-name
// conflict it fetches and returns the existing active row's id, making the
// operation idempotent.
//
// userID and email carry optional attribution from the authenticated request
// context. Pass nil for both when the caller has no auth context (stdio path,
// MCP_AUTH=none) — the nullable columns accept NULL safely.
func Create(ctx context.Context, tx pgx.Tx, name, nodeType string, userID, email *string) (string, error) {
	var id string
	err := tx.QueryRow(ctx,
		`INSERT INTO nodes (name, node_type, created_by_user_id, created_by_email)
		 VALUES ($1, $2, $3::uuid, $4)
		 ON CONFLICT (name) DO NOTHING
		 RETURNING id`,
		name, nodeType, userID, email,
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

// ListByCreatedAt returns active nodes ordered by created_at DESC, id DESC for
// stable pagination. Optional since/until bounds narrow the result window; pass
// nil to skip a bound. limit controls page size; offset skips rows. Returns the
// page of nodes plus a hasMore flag (true when additional rows exist beyond the
// requested limit). The LIMIT N+1 strategy avoids a second COUNT(*) query.
func ListByCreatedAt(
	ctx context.Context,
	pool *pgxpool.Pool,
	since, until *time.Time,
	limit, offset int,
) (rows []NodeRow, hasMore bool, err error) {
	// Fetch one extra row to detect whether a next page exists.
	dbRows, err := pool.Query(ctx,
		`SELECT id, name, node_type
		 FROM nodes
		 WHERE deleted_at IS NULL
		   AND ($1::timestamptz IS NULL OR created_at >= $1)
		   AND ($2::timestamptz IS NULL OR created_at <= $2)
		 ORDER BY created_at DESC, id DESC
		 LIMIT $3 OFFSET $4`,
		since, until, limit+1, offset,
	)
	if err != nil {
		return nil, false, err
	}
	defer dbRows.Close()

	nodeRows, err := scanNodeRows(dbRows)
	if err != nil {
		return nil, false, err
	}

	if len(nodeRows) > limit {
		return nodeRows[:limit], true, nil
	}
	return nodeRows, false, nil
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
