package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	pgvector "github.com/pgvector/pgvector-go"
)

// Insert adds a new observation text and its pre-computed embedding for the
// given node. When the ONNX runtime is unavailable, callers may pass a
// zero-length pgvector.Vector{} — this is stored as SQL NULL (degraded mode).
// A non-zero vector must have exactly 384 dimensions. On a (node_id, text)
// unique conflict the row is silently skipped and inserted=false is returned.
func Insert(ctx context.Context, tx pgx.Tx, nodeID, text string, embedding pgvector.Vector) (observationID string, inserted bool, err error) {
	// Use a typed nil interface so pgx encodes a SQL NULL when no embedding is
	// available, rather than sending an invalid empty vector to Postgres.
	var embParam any
	if len(embedding.Slice()) > 0 {
		embParam = embedding
	}

	err = tx.QueryRow(ctx,
		`INSERT INTO observations (node_id, text, embedding)
		 VALUES ($1, $2, $3)
		 ON CONFLICT (node_id, text) DO NOTHING
		 RETURNING id`,
		nodeID, text, embParam,
	).Scan(&observationID)

	if err == nil {
		return observationID, true, nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return "", false, err
}

// MarkDeletedByNodeAndTexts soft-deletes active observations belonging to
// nodeID whose text matches any value in texts. Returns the count of rows
// updated.
//
// Admin-script API only — not wired to any MCP tool.
func MarkDeletedByNodeAndTexts(ctx context.Context, tx pgx.Tx, nodeID string, texts []string) (int, error) {
	tag, err := tx.Exec(ctx,
		`UPDATE observations SET deleted_at = now()
		 WHERE node_id = $1
		   AND text = ANY($2)
		   AND deleted_at IS NULL`,
		nodeID, texts,
	)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// ListByNodeIDs returns the active observation texts for each node id in the
// provided slice. The result is a map of node_id → []text, preserving
// insertion order via ORDER BY id. Node ids with no active observations are
// omitted from the map.
func ListByNodeIDs(ctx context.Context, pool *pgxpool.Pool, nodeIDs []string) (map[string][]string, error) {
	if len(nodeIDs) == 0 {
		return map[string][]string{}, nil
	}

	rows, err := pool.Query(ctx,
		`SELECT node_id::text, text
		 FROM observations
		 WHERE node_id::text = ANY($1) AND deleted_at IS NULL
		 ORDER BY node_id, id`,
		nodeIDs,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[string][]string, len(nodeIDs))
	for rows.Next() {
		var nodeID, text string
		if err := rows.Scan(&nodeID, &text); err != nil {
			return nil, err
		}
		result[nodeID] = append(result[nodeID], text)
	}
	return result, rows.Err()
}
