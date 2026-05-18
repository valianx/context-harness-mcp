package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// RelationRow is a read-projection of an active relations row, using entity
// names rather than IDs so handlers can return it directly as JSON.
type RelationRow struct {
	From         string
	To           string
	RelationType string
}

// InsertRelation inserts a new relation. On a (from_entity_id, to_entity_id,
// relation_type) unique conflict the row is silently skipped and
// inserted=false is returned.
func InsertRelation(ctx context.Context, tx pgx.Tx, fromID, toID, relType string) (relationID string, inserted bool, err error) {
	err = tx.QueryRow(ctx,
		`INSERT INTO relations (from_entity_id, to_entity_id, relation_type)
		 VALUES ($1, $2, $3)
		 ON CONFLICT (from_entity_id, to_entity_id, relation_type) DO NOTHING
		 RETURNING id`,
		fromID, toID, relType,
	).Scan(&relationID)

	if err == nil {
		return relationID, true, nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return "", false, err
}

// MarkRelationDeleted soft-deletes the active relation identified by the
// (fromName, toName, relType) triple. Both entity names are resolved to
// active ids inside the transaction. Returns the count of rows updated.
func MarkRelationDeleted(ctx context.Context, tx pgx.Tx, fromName, toName, relType string) (int, error) {
	fromID, _, fromFound, err := FindByName(ctx, tx, fromName)
	if err != nil {
		return 0, err
	}
	if !fromFound {
		return 0, nil // entity already gone — nothing to delete
	}

	toID, _, toFound, err := FindByName(ctx, tx, toName)
	if err != nil {
		return 0, err
	}
	if !toFound {
		return 0, nil
	}

	tag, err := tx.Exec(ctx,
		`UPDATE relations SET deleted_at = now()
		 WHERE from_entity_id = $1
		   AND to_entity_id   = $2
		   AND relation_type  = $3
		   AND deleted_at IS NULL`,
		fromID, toID, relType,
	)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// ListActiveRelations returns all active relations, joining entity names for
// both endpoints. Only rows where the relation AND both endpoint entities are
// active (deleted_at IS NULL) are returned.
func ListActiveRelations(ctx context.Context, pool *pgxpool.Pool) ([]RelationRow, error) {
	rows, err := pool.Query(ctx,
		`SELECT ef.name AS "from", et.name AS "to", r.relation_type
		 FROM relations r
		 JOIN entities ef ON ef.id = r.from_entity_id AND ef.deleted_at IS NULL
		 JOIN entities et ON et.id = r.to_entity_id   AND et.deleted_at IS NULL
		 WHERE r.deleted_at IS NULL
		 ORDER BY ef.name, et.name, r.relation_type`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []RelationRow
	for rows.Next() {
		var r RelationRow
		if err := rows.Scan(&r.From, &r.To, &r.RelationType); err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	return result, rows.Err()
}

// ListActiveRelationsForEntityIDs returns active relations where both the from
// and to entity ids are within the provided set. Used by open_nodes and
// search_nodes to filter relations to only those connecting the result set.
func ListActiveRelationsForEntityIDs(ctx context.Context, pool *pgxpool.Pool, entityIDs []string) ([]RelationRow, error) {
	if len(entityIDs) == 0 {
		return nil, nil
	}

	rows, err := pool.Query(ctx,
		`SELECT ef.name AS "from", et.name AS "to", r.relation_type
		 FROM relations r
		 JOIN entities ef ON ef.id = r.from_entity_id AND ef.deleted_at IS NULL
		 JOIN entities et ON et.id = r.to_entity_id   AND et.deleted_at IS NULL
		 WHERE r.from_entity_id::text = ANY($1)
		   AND r.to_entity_id::text   = ANY($1)
		   AND r.deleted_at IS NULL
		 ORDER BY ef.name, et.name, r.relation_type`,
		entityIDs,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []RelationRow
	for rows.Next() {
		var r RelationRow
		if err := rows.Scan(&r.From, &r.To, &r.RelationType); err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	return result, rows.Err()
}
