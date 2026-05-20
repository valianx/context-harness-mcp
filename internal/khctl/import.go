// Package khctl provides the core import, export, and seed logic for the
// khctl operator CLI. Separating the logic from package main allows it to be
// called directly from integration tests without os/exec.
package khctl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	pgvector "github.com/pgvector/pgvector-go"
)

const ExpectedEmbeddingDims = 384

// ImportNode is the JSON shape for a node in the import payload.
// It accepts both the new "nodeType" vocabulary and the legacy "entityType"
// field (normalized at parse time). ProjectID defaults to "global" when
// absent from the JSON (back-compat with pre-Phase-2 exports).
type ImportNode struct {
	Name         string      `json:"name"`
	NodeType     string      `json:"nodeType"`
	ProjectID    string      `json:"project_id"`
	Observations []string    `json:"observations"`
	Embeddings   [][]float32 `json:"embeddings"`
	DeletedAt    interface{} `json:"deleted_at"`
}

// ImportRelation is the JSON shape for a relation in the import payload.
// ProjectID defaults to "global" when absent (back-compat with pre-Phase-2 exports).
type ImportRelation struct {
	From         string      `json:"from"`
	To           string      `json:"to"`
	RelationType string      `json:"relationType"`
	ProjectID    string      `json:"project_id"`
	DeletedAt    interface{} `json:"deleted_at"`
}

// rawPayload is used for initial JSON decode before normalization.
type rawPayload struct {
	Nodes     []json.RawMessage `json:"nodes"`
	Entities  []json.RawMessage `json:"entities"`
	Relations []ImportRelation  `json:"relations"`
}

// ReadInput reads raw bytes from the given path, or from stdin when path is "-".
func ReadInput(path string) ([]byte, error) {
	if path == "-" {
		return io.ReadAll(os.Stdin)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return data, nil
}

// ParseImportPayload decodes the raw JSON and normalizes the node list.
// It accepts both the preferred {"nodes": [...]} shape and the legacy
// {"entities": [...]} shape (with a deprecation notice on stderr).
// The "entityType" field within nodes is also normalized to "nodeType".
func ParseImportPayload(data []byte) ([]ImportNode, []ImportRelation, error) {
	var raw rawPayload
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, nil, fmt.Errorf("parse JSON: %w", err)
	}

	var nodeRaws []json.RawMessage
	if len(raw.Nodes) > 0 {
		nodeRaws = raw.Nodes
	} else if len(raw.Entities) > 0 {
		fmt.Fprintln(os.Stderr,
			"Warning: input uses legacy {\"entities\": [...]} shape. "+
				"Regenerate exports with khctl export to retire this fallback path.")
		nodeRaws = raw.Entities
	}

	nodes, err := decodeNodes(nodeRaws)
	if err != nil {
		return nil, nil, err
	}
	relations := defaultRelationProjectIDs(raw.Relations)
	return nodes, relations, nil
}

// defaultRelationProjectIDs sets ProjectID to "global" on any relation that
// omits the field (back-compat with pre-Phase-2 exports).
func defaultRelationProjectIDs(rels []ImportRelation) []ImportRelation {
	for i := range rels {
		if rels[i].ProjectID == "" {
			rels[i].ProjectID = "global"
		}
	}
	return rels
}

// decodeNodes decodes raw node JSON messages and normalizes legacy field names.
func decodeNodes(raws []json.RawMessage) ([]ImportNode, error) {
	nodes := make([]ImportNode, 0, len(raws))
	for i, raw := range raws {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, fmt.Errorf("node[%d]: %w", i, err)
		}
		// Normalize entityType → nodeType for the legacy shape.
		if _, hasNodeType := m["nodeType"]; !hasNodeType {
			if et, hasEntityType := m["entityType"]; hasEntityType {
				m["nodeType"] = et
				delete(m, "entityType")
			}
		}
		// Default project_id to "global" for pre-Phase-2 exports that omit the field.
		if _, hasProjectID := m["project_id"]; !hasProjectID {
			m["project_id"] = json.RawMessage(`"global"`)
		}
		normalized, err := json.Marshal(m)
		if err != nil {
			return nil, fmt.Errorf("node[%d]: re-encode: %w", i, err)
		}
		var n ImportNode
		if err := json.Unmarshal(normalized, &n); err != nil {
			return nil, fmt.Errorf("node[%d]: decode: %w", i, err)
		}
		nodes = append(nodes, n)
	}
	return nodes, nil
}

// RunImport executes the full import inside a single transaction.
// Returns (nodesInserted, obsInserted, obsDeduped, relsInserted, relsDeduped, error).
func RunImport(
	ctx context.Context,
	pool *pgxpool.Pool,
	nodes []ImportNode,
	relations []ImportRelation,
) (nodesIn, obsIn, obsDedup, relsIn, relsDedup int, err error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, 0, 0, 0, 0, fmt.Errorf("begin transaction: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback(ctx)
		}
	}()

	nodesIn, err = importNodes(ctx, tx, nodes)
	if err != nil {
		return
	}
	obsIn, obsDedup, err = importObservations(ctx, tx, nodes)
	if err != nil {
		return
	}
	relsIn, relsDedup, err = importRelations(ctx, tx, relations)
	if err != nil {
		return
	}

	err = tx.Commit(ctx)
	return
}

// importNodes inserts nodes using ON CONFLICT DO NOTHING against the composite
// unique constraint nodes_project_name_key (project_id, name) added by 00007.
func importNodes(ctx context.Context, tx pgx.Tx, nodes []ImportNode) (int, error) {
	inserted := 0
	for _, n := range nodes {
		if n.DeletedAt != nil {
			continue
		}
		var id string
		err := tx.QueryRow(ctx,
			`INSERT INTO nodes (name, node_type, project_id)
			 VALUES ($1, $2, $3)
			 ON CONFLICT ON CONSTRAINT nodes_project_name_key DO NOTHING
			 RETURNING id`,
			n.Name, n.NodeType, n.ProjectID,
		).Scan(&id)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				continue
			}
			return inserted, fmt.Errorf("insert node %q (project %q): %w", n.Name, n.ProjectID, err)
		}
		inserted++
	}
	return inserted, nil
}

// importObservations inserts observations for all nodes.
func importObservations(ctx context.Context, tx pgx.Tx, nodes []ImportNode) (int, int, error) {
	inserted, deduped := 0, 0
	for _, n := range nodes {
		if n.DeletedAt != nil {
			continue
		}

		var nodeID string
		if err := tx.QueryRow(ctx,
			`SELECT id FROM nodes WHERE project_id = $1 AND name = $2 AND deleted_at IS NULL`,
			n.ProjectID, n.Name,
		).Scan(&nodeID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				continue
			}
			return inserted, deduped, fmt.Errorf("resolve node %q (project %q): %w", n.Name, n.ProjectID, err)
		}

		for idx, text := range n.Observations {
			embParam, err := embeddingParam(n, idx)
			if err != nil {
				return inserted, deduped, fmt.Errorf("node %q obs %d: %w", n.Name, idx, err)
			}

			var id string
			scanErr := tx.QueryRow(ctx,
				`INSERT INTO observations (node_id, text, embedding)
				 VALUES ($1, $2, $3)
				 ON CONFLICT (node_id, text) DO NOTHING
				 RETURNING id`,
				nodeID, text, embParam,
			).Scan(&id)
			if scanErr != nil {
				if errors.Is(scanErr, pgx.ErrNoRows) {
					deduped++
					continue
				}
				return inserted, deduped, fmt.Errorf("insert observation for node %q: %w", n.Name, scanErr)
			}
			inserted++
		}
	}
	return inserted, deduped, nil
}

// embeddingParam returns the pgvector.Vector for the embedding at obs index idx,
// or nil if no embedding is present. Returns an error when dimensionality is wrong.
func embeddingParam(n ImportNode, idx int) (any, error) {
	if idx >= len(n.Embeddings) || n.Embeddings[idx] == nil {
		return nil, nil
	}
	emb := n.Embeddings[idx]
	if len(emb) != ExpectedEmbeddingDims {
		return nil, fmt.Errorf("expected %d-dim embedding, got %d", ExpectedEmbeddingDims, len(emb))
	}
	return pgvector.NewVector(emb), nil
}

// importRelations inserts relations by resolving node names to IDs.
func importRelations(ctx context.Context, tx pgx.Tx, relations []ImportRelation) (int, int, error) {
	inserted, deduped := 0, 0
	for _, r := range relations {
		if r.DeletedAt != nil {
			continue
		}
		if r.From == "" || r.To == "" || r.RelationType == "" {
			continue
		}

		var fromID, toID string
		if err := tx.QueryRow(ctx,
			`SELECT id FROM nodes WHERE project_id = $1 AND name = $2 AND deleted_at IS NULL`,
			r.ProjectID, r.From,
		).Scan(&fromID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				continue
			}
			return inserted, deduped, fmt.Errorf("resolve from-node %q (project %q): %w", r.From, r.ProjectID, err)
		}
		if err := tx.QueryRow(ctx,
			`SELECT id FROM nodes WHERE project_id = $1 AND name = $2 AND deleted_at IS NULL`,
			r.ProjectID, r.To,
		).Scan(&toID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				continue
			}
			return inserted, deduped, fmt.Errorf("resolve to-node %q (project %q): %w", r.To, r.ProjectID, err)
		}

		var id string
		err := tx.QueryRow(ctx,
			`INSERT INTO relations (from_node_id, to_node_id, relation_type)
			 VALUES ($1, $2, $3)
			 ON CONFLICT (from_node_id, to_node_id, relation_type) DO NOTHING
			 RETURNING id`,
			fromID, toID, r.RelationType,
		).Scan(&id)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				deduped++
				continue
			}
			return inserted, deduped, fmt.Errorf("insert relation %q→%q: %w", r.From, r.To, err)
		}
		inserted++
	}
	return inserted, deduped, nil
}
