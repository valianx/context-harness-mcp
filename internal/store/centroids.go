// Package store — centroids.go computes per-nodeType centroid vectors from the
// observations corpus. Used by the suggest_node_type MCP tool.
package store

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
	pgvector "github.com/pgvector/pgvector-go"
)

// CentroidsByType returns a map nodeType → centroid vector (mean of all
// observation embeddings on active nodes of that type). Types with no active
// observations with embeddings are absent from the map. An optional
// projectFilter scopes the query to one project; pass nil for all projects.
//
// Vectors are averaged in Go (not in SQL) because pgvector does not ship a
// VECTOR_AVG aggregate. Each type's centroid is the element-wise mean of its
// observation vectors; centroids are NOT re-normalized to unit length — cosine
// similarity normalizes both sides at query time.
func CentroidsByType(ctx context.Context, pool *pgxpool.Pool, projectFilter *string) (map[string][]float32, error) {
	const query = `
		SELECT n.node_type, o.embedding
		FROM observations o
		JOIN nodes n ON n.id = o.node_id
		WHERE n.deleted_at IS NULL
		  AND o.deleted_at IS NULL
		  AND o.embedding IS NOT NULL
		  AND ($1::text IS NULL OR n.project_id = $1)
	`
	rows, err := pool.Query(ctx, query, projectFilter)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// Accumulate in float64 to avoid precision drift across many additions.
	sums := make(map[string][]float64)
	counts := make(map[string]int)

	for rows.Next() {
		var nodeType string
		var vec pgvector.Vector
		if err := rows.Scan(&nodeType, &vec); err != nil {
			return nil, err
		}
		slice := vec.Slice()
		if len(slice) == 0 {
			continue
		}

		cur, exists := sums[nodeType]
		if !exists {
			cur = make([]float64, len(slice))
			sums[nodeType] = cur
		}
		for i, v := range slice {
			cur[i] += float64(v)
		}
		counts[nodeType]++
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make(map[string][]float32, len(sums))
	for t, s := range sums {
		n := float64(counts[t])
		centroid := make([]float32, len(s))
		for i := range s {
			centroid[i] = float32(s[i] / n)
		}
		out[t] = centroid
	}
	return out, nil
}
