// Package store provides Postgres CRUD helpers for the context-harness-mcp
// entities, observations, and relations tables.
package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// EntityRow is a read-projection of an active entities row.
type EntityRow struct {
	ID         string // UUID stored as string
	Name       string
	EntityType string
}

// Create inserts a new entity row and returns its UUID. On a unique-name
// conflict it fetches and returns the existing active row's id, making the
// operation idempotent.
func Create(ctx context.Context, tx pgx.Tx, name, entityType string) (string, error) {
	var id string
	err := tx.QueryRow(ctx,
		`INSERT INTO entities (name, entity_type)
		 VALUES ($1, $2)
		 ON CONFLICT (name) DO NOTHING
		 RETURNING id`,
		name, entityType,
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
		`SELECT id FROM entities WHERE name = $1 AND deleted_at IS NULL`,
		name,
	).Scan(&id)
	return id, err
}

// FindByName returns the id and entity_type of the active entity with the
// given name. found=false when no active row exists (no error is returned in
// that case).
func FindByName(ctx context.Context, tx pgx.Tx, name string) (id, entityType string, found bool, err error) {
	err = tx.QueryRow(ctx,
		`SELECT id, entity_type FROM entities WHERE name = $1 AND deleted_at IS NULL`,
		name,
	).Scan(&id, &entityType)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, err
	}
	return id, entityType, true, nil
}

// MarkDeletedByNames soft-deletes all active entities whose name is in the
// provided slice. Returns the number of rows updated.
func MarkDeletedByNames(ctx context.Context, tx pgx.Tx, names []string) (int, error) {
	tag, err := tx.Exec(ctx,
		`UPDATE entities SET deleted_at = now()
		 WHERE name = ANY($1) AND deleted_at IS NULL`,
		names,
	)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// ListActive returns all active entity rows ordered by name. Used by read_graph.
func ListActive(ctx context.Context, pool *pgxpool.Pool) ([]EntityRow, error) {
	rows, err := pool.Query(ctx,
		`SELECT id, name, entity_type FROM entities WHERE deleted_at IS NULL ORDER BY name`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEntityRows(rows)
}

// SearchByTextSubstring returns active entities that have at least one active
// observation whose text contains the query string (case-insensitive ILIKE).
// This is the PR-4 substring fallback; PR-5 will replace it with pgvector cosine.
func SearchByTextSubstring(ctx context.Context, pool *pgxpool.Pool, query string) ([]EntityRow, error) {
	rows, err := pool.Query(ctx,
		`SELECT DISTINCT e.id, e.name, e.entity_type
		 FROM entities e
		 JOIN observations o ON o.entity_id = e.id
		 WHERE o.text ILIKE '%' || $1 || '%'
		   AND o.deleted_at IS NULL
		   AND e.deleted_at IS NULL
		 ORDER BY e.name`,
		query,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEntityRows(rows)
}

func scanEntityRows(rows pgx.Rows) ([]EntityRow, error) {
	var result []EntityRow
	for rows.Next() {
		var r EntityRow
		if err := rows.Scan(&r.ID, &r.Name, &r.EntityType); err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	return result, rows.Err()
}
