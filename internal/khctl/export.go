package khctl

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	pgvector "github.com/pgvector/pgvector-go"
)

const ExportFormatVersion = "2"

// ExportNode is the JSON shape for a single exported node.
type ExportNode struct {
	Name         string      `json:"name"`
	NodeType     string      `json:"nodeType"`
	ProjectID    string      `json:"project_id"`
	Observations []string    `json:"observations"`
	Embeddings   [][]float32 `json:"embeddings,omitempty"`
}

// ExportRelation is the JSON shape for a single exported relation.
type ExportRelation struct {
	From         string `json:"from"`
	To           string `json:"to"`
	RelationType string `json:"relationType"`
	ProjectID    string `json:"project_id"`
}

// ExportPayload is the top-level JSON shape emitted by khctl export.
type ExportPayload struct {
	FormatVersion string           `json:"format_version"`
	ExportedAt    string           `json:"exported_at"`
	NodeCount     int              `json:"node_count"`
	RelationCount int              `json:"relation_count"`
	Nodes         []ExportNode     `json:"nodes"`
	Relations     []ExportRelation `json:"relations"`
}

// BuildExportPayload queries the DB and assembles the export JSON structure.
func BuildExportPayload(ctx context.Context, pool *pgxpool.Pool) (*ExportPayload, error) {
	nodes, err := fetchExportNodes(ctx, pool)
	if err != nil {
		return nil, fmt.Errorf("fetch nodes: %w", err)
	}
	relations, err := fetchExportRelations(ctx, pool)
	if err != nil {
		return nil, fmt.Errorf("fetch relations: %w", err)
	}

	return &ExportPayload{
		FormatVersion: ExportFormatVersion,
		ExportedAt:    time.Now().UTC().Format(time.RFC3339),
		NodeCount:     len(nodes),
		RelationCount: len(relations),
		Nodes:         nodes,
		Relations:     relations,
	}, nil
}

// fetchExportNodes returns all active nodes with their observations and embeddings.
func fetchExportNodes(ctx context.Context, pool *pgxpool.Pool) ([]ExportNode, error) {
	rows, err := pool.Query(ctx,
		`SELECT id, name, node_type, project_id
		 FROM nodes
		 WHERE deleted_at IS NULL
		 ORDER BY project_id, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var nodes []ExportNode
	for rows.Next() {
		var id, name, nodeType, projectID string
		if err := rows.Scan(&id, &name, &nodeType, &projectID); err != nil {
			return nil, err
		}
		obs, embs, err := fetchNodeObservations(ctx, pool, id)
		if err != nil {
			return nil, fmt.Errorf("observations for node %q: %w", name, err)
		}
		n := ExportNode{Name: name, NodeType: nodeType, ProjectID: projectID, Observations: obs}
		for _, e := range embs {
			if e != nil {
				n.Embeddings = embs
				break
			}
		}
		nodes = append(nodes, n)
	}
	return nodes, rows.Err()
}

// fetchNodeObservations returns the ordered (text, embedding) pairs for one node.
func fetchNodeObservations(ctx context.Context, pool *pgxpool.Pool, nodeID string) ([]string, [][]float32, error) {
	rows, err := pool.Query(ctx,
		`SELECT text, embedding
		 FROM observations
		 WHERE node_id = $1 AND deleted_at IS NULL
		 ORDER BY created_at`,
		nodeID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	var texts []string
	var embeddings [][]float32
	for rows.Next() {
		var text string
		var emb *pgvector.Vector
		if err := rows.Scan(&text, &emb); err != nil {
			return nil, nil, err
		}
		texts = append(texts, text)
		if emb != nil {
			embeddings = append(embeddings, emb.Slice())
		} else {
			embeddings = append(embeddings, nil)
		}
	}
	return texts, embeddings, rows.Err()
}

// fetchExportRelations returns all active relations using node names.
func fetchExportRelations(ctx context.Context, pool *pgxpool.Pool) ([]ExportRelation, error) {
	rows, err := pool.Query(ctx,
		`SELECT fn.name, tn.name, r.relation_type, r.project_id
		 FROM relations r
		 JOIN nodes fn ON fn.id = r.from_node_id
		 JOIN nodes tn ON tn.id = r.to_node_id
		 WHERE r.deleted_at IS NULL
		   AND fn.deleted_at IS NULL
		   AND tn.deleted_at IS NULL
		 ORDER BY r.project_id, fn.name, tn.name, r.relation_type`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var relations []ExportRelation
	for rows.Next() {
		var from, to, relType, projectID string
		if err := rows.Scan(&from, &to, &relType, &projectID); err != nil {
			return nil, err
		}
		relations = append(relations, ExportRelation{From: from, To: to, RelationType: relType, ProjectID: projectID})
	}
	return relations, rows.Err()
}
