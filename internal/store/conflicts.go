package store

import (
	"context"
	"sort"

	"github.com/jackc/pgx/v5/pgxpool"
	pgvector "github.com/pgvector/pgvector-go"
)

// ConflictCandidate is a node that is semantically similar to the target node.
// Similarity is the maximum cosine similarity observed across any observation
// pair between the candidate and the target. OwnObs and OtherObs are the
// observation texts from the target and the candidate respectively that
// produced the maximum similarity score.
type ConflictCandidate struct {
	NodeID     string
	Name       string
	NodeType   string
	Similarity float32
	OwnObs     string // target node's observation that produced max similarity
	OtherObs   string // candidate node's observation that produced max similarity
}

// FindSimilarTo returns up to topK nodes in the same project whose observations
// are semantically similar to the target node's observations.
//
// Strategy: loop-N-queries — one pgvector cosine query per target observation.
// For each query, fetch up to topK*4 candidates above minSim. Aggregate
// client-side by candidate node, taking MAX(similarity) and remembering the
// matching observation pair. Sort descending, return top topK results.
//
// This loop strategy (vs CROSS JOIN single query) preserves HNSW index
// efficiency for the typical case of N≤10 observations per node.
//
// Returns nil (not an error) when the target node has no non-null embeddings —
// the handler surfaces this as an empty candidates list.
func FindSimilarTo(
	ctx context.Context,
	pool *pgxpool.Pool,
	targetNodeID, projectID string,
	topK int,
	minSim float32,
) ([]ConflictCandidate, error) {
	targetTexts, targetVecs, err := fetchTargetEmbeddings(ctx, pool, targetNodeID)
	if err != nil {
		return nil, err
	}
	if len(targetVecs) == 0 {
		return nil, nil
	}

	// candidates maps nodeID → best-match candidate seen so far.
	candidates := make(map[string]*ConflictCandidate)

	for i, vec := range targetVecs {
		if err := queryAndAggregate(ctx, pool, vec, targetTexts[i], targetNodeID, projectID, topK*4, minSim, candidates); err != nil {
			return nil, err
		}
	}

	return sortAndTruncate(candidates, topK), nil
}

// fetchTargetEmbeddings returns the observation texts and their embeddings for
// the target node. Only active observations with a non-null embedding are
// returned; the two slices are parallel (same index = same row).
func fetchTargetEmbeddings(ctx context.Context, pool *pgxpool.Pool, targetNodeID string) ([]string, []pgvector.Vector, error) {
	rows, err := pool.Query(ctx,
		`SELECT text, embedding
		 FROM observations
		 WHERE node_id = $1
		   AND deleted_at IS NULL
		   AND embedding IS NOT NULL`,
		targetNodeID,
	)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	var texts []string
	var vecs []pgvector.Vector
	for rows.Next() {
		var text string
		var vec pgvector.Vector
		if err := rows.Scan(&text, &vec); err != nil {
			return nil, nil, err
		}
		texts = append(texts, text)
		vecs = append(vecs, vec)
	}
	return texts, vecs, rows.Err()
}

// queryAndAggregate runs one cosine similarity query for a single target
// observation embedding and merges the results into the candidates map.
// The inflated limit (topK*4) compensates for HNSW post-filter selectivity
// when the project filter is highly selective.
func queryAndAggregate(
	ctx context.Context,
	pool *pgxpool.Pool,
	vec pgvector.Vector,
	ownObs string,
	targetNodeID, projectID string,
	inflatedLimit int,
	minSim float32,
	candidates map[string]*ConflictCandidate,
) error {
	rows, err := pool.Query(ctx,
		`SELECT n.id::text, n.name, n.node_type, o.text AS other_obs,
		        (1 - (o.embedding <=> $1::vector))::real AS sim
		 FROM nodes n
		 JOIN observations o ON o.node_id = n.id
		 WHERE n.id::text <> $2
		   AND n.project_id = $3
		   AND n.deleted_at IS NULL
		   AND o.deleted_at IS NULL
		   AND o.embedding IS NOT NULL
		   AND (1 - (o.embedding <=> $1::vector))::real >= $4
		 ORDER BY sim DESC
		 LIMIT $5`,
		vec, targetNodeID, projectID, minSim, inflatedLimit,
	)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var c ConflictCandidate
		if err := rows.Scan(&c.NodeID, &c.Name, &c.NodeType, &c.OtherObs, &c.Similarity); err != nil {
			return err
		}

		existing, ok := candidates[c.NodeID]
		if !ok || c.Similarity > existing.Similarity {
			c.OwnObs = ownObs
			candidates[c.NodeID] = &c
		}
	}
	return rows.Err()
}

// sortAndTruncate converts the candidates map into a sorted slice (descending
// by similarity) and returns the top topK entries.
func sortAndTruncate(candidates map[string]*ConflictCandidate, topK int) []ConflictCandidate {
	result := make([]ConflictCandidate, 0, len(candidates))
	for _, c := range candidates {
		result = append(result, *c)
	}

	sort.Slice(result, func(i, j int) bool {
		return result[i].Similarity > result[j].Similarity
	})

	if len(result) > topK {
		result = result[:topK]
	}
	return result
}
