package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// RelationRow is a read-projection of an active relations row, using node
// names rather than IDs so handlers can return it directly as JSON.
type RelationRow struct {
	From         string
	To           string
	RelationType string
}

// InsertRelation inserts a new relation. On a (from_node_id, to_node_id,
// relation_type) unique conflict the row is silently skipped and
// inserted=false is returned.
func InsertRelation(ctx context.Context, tx pgx.Tx, fromID, toID, relType string) (relationID string, inserted bool, err error) {
	err = tx.QueryRow(ctx,
		`INSERT INTO relations (from_node_id, to_node_id, relation_type)
		 VALUES ($1, $2, $3)
		 ON CONFLICT (from_node_id, to_node_id, relation_type) DO NOTHING
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
// (fromName, toName, relType) triple. Both node names are resolved to active
// ids inside the transaction. Returns the count of rows updated.
//
// Admin-script API only — not wired to any MCP tool.
func MarkRelationDeleted(ctx context.Context, tx pgx.Tx, fromName, toName, relType string) (int, error) {
	fromID, _, fromFound, err := FindByName(ctx, tx, fromName)
	if err != nil {
		return 0, err
	}
	if !fromFound {
		return 0, nil // node already gone — nothing to delete
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
		 WHERE from_node_id = $1
		   AND to_node_id   = $2
		   AND relation_type = $3
		   AND deleted_at IS NULL`,
		fromID, toID, relType,
	)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// ListActiveRelations returns all active relations, joining node names for
// both endpoints. Only rows where the relation AND both endpoint nodes are
// active (deleted_at IS NULL) are returned.
func ListActiveRelations(ctx context.Context, pool *pgxpool.Pool) ([]RelationRow, error) {
	rows, err := pool.Query(ctx,
		`SELECT nf.name AS "from", nt.name AS "to", r.relation_type
		 FROM relations r
		 JOIN nodes nf ON nf.id = r.from_node_id AND nf.deleted_at IS NULL
		 JOIN nodes nt ON nt.id = r.to_node_id   AND nt.deleted_at IS NULL
		 WHERE r.deleted_at IS NULL
		 ORDER BY nf.name, nt.name, r.relation_type`,
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

// ListActiveRelationsForNodeIDs returns active relations where both the from
// and to node ids are within the provided set. Used by open_nodes and
// search_nodes to filter relations to only those connecting the result set.
func ListActiveRelationsForNodeIDs(ctx context.Context, pool *pgxpool.Pool, nodeIDs []string) ([]RelationRow, error) {
	if len(nodeIDs) == 0 {
		return nil, nil
	}

	rows, err := pool.Query(ctx,
		`SELECT nf.name AS "from", nt.name AS "to", r.relation_type
		 FROM relations r
		 JOIN nodes nf ON nf.id = r.from_node_id AND nf.deleted_at IS NULL
		 JOIN nodes nt ON nt.id = r.to_node_id   AND nt.deleted_at IS NULL
		 WHERE r.from_node_id::text = ANY($1)
		   AND r.to_node_id::text   = ANY($1)
		   AND r.deleted_at IS NULL
		 ORDER BY nf.name, nt.name, r.relation_type`,
		nodeIDs,
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
