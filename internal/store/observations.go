package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	pgvector "github.com/pgvector/pgvector-go"
)

// Insert adds a new observation text and its pre-computed embedding for the
// given entity. When the ONNX runtime is unavailable, callers may pass a
// zero-length pgvector.Vector{} — this is stored as SQL NULL (degraded mode).
// A non-zero vector must have exactly 384 dimensions. On a (entity_id, text)
// unique conflict the row is silently skipped and inserted=false is returned.
func Insert(ctx context.Context, tx pgx.Tx, entityID, text string, embedding pgvector.Vector) (observationID string, inserted bool, err error) {
	// Use a typed nil interface so pgx encodes a SQL NULL when no embedding is
	// available, rather than sending an invalid empty vector to Postgres.
	var embParam any
	if len(embedding.Slice()) > 0 {
		embParam = embedding
	}

	err = tx.QueryRow(ctx,
		`INSERT INTO observations (entity_id, text, embedding)
		 VALUES ($1, $2, $3)
		 ON CONFLICT (entity_id, text) DO NOTHING
		 RETURNING id`,
		entityID, text, embParam,
	).Scan(&observationID)

	if err == nil {
		return observationID, true, nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return "", false, err
}

// MarkDeletedByEntityAndTexts soft-deletes active observations belonging to
// entityID whose text matches any value in texts. Returns the count of rows
// updated.
func MarkDeletedByEntityAndTexts(ctx context.Context, tx pgx.Tx, entityID string, texts []string) (int, error) {
	tag, err := tx.Exec(ctx,
		`UPDATE observations SET deleted_at = now()
		 WHERE entity_id = $1
		   AND text = ANY($2)
		   AND deleted_at IS NULL`,
		entityID, texts,
	)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// ListByEntityIDs returns the active observation texts for each entity id in
// the provided slice. The result is a map of entity_id → []text, preserving
// insertion order via ORDER BY id. Entity ids with no active observations are
// omitted from the map.
func ListByEntityIDs(ctx context.Context, pool *pgxpool.Pool, entityIDs []string) (map[string][]string, error) {
	if len(entityIDs) == 0 {
		return map[string][]string{}, nil
	}

	rows, err := pool.Query(ctx,
		`SELECT entity_id::text, text
		 FROM observations
		 WHERE entity_id::text = ANY($1) AND deleted_at IS NULL
		 ORDER BY entity_id, id`,
		entityIDs,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[string][]string, len(entityIDs))
	for rows.Next() {
		var entityID, text string
		if err := rows.Scan(&entityID, &text); err != nil {
			return nil, err
		}
		result[entityID] = append(result[entityID], text)
	}
	return result, rows.Err()
}
