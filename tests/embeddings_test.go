package tests

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariogutierrez/context-harness-mcp/internal/embed"
)

// ── helpers ───────────────────────────────────────────────────────────────────

// parityRecord is a single entry from parity_baseline.json.
type parityRecord struct {
	Text      string    `json:"text"`
	Embedding []float32 `json:"embedding"`
}

// cosineSimilarity computes the cosine similarity between two equal-length
// float32 vectors. Panics if lengths differ.
func cosineSimilarity(a, b []float32) float64 {
	var dot, normA, normB float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		normA += float64(a[i]) * float64(a[i])
		normB += float64(b[i]) * float64(b[i])
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return dot / (math.Sqrt(normA) * math.Sqrt(normB))
}

// skipIfONNXUnavailable calls t.Skipf if the embed.Default().Encode method
// returns an error that indicates the ONNX runtime is not available. This
// mirrors the Docker-daemon skip pattern used in setup_test.go.
func skipIfONNXUnavailable(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		return
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "onnx") ||
		strings.Contains(msg, "cgo") ||
		strings.Contains(msg, "shared lib") ||
		strings.Contains(msg, "model") {
		t.Skipf("ONNX runtime unavailable — skipping embedding test: %v", err)
	}
}

// loadParityBaseline reads tests/fixtures/parity_baseline.json relative to the
// repo root (which is the working directory for `go test ./...`).
func loadParityBaseline(t *testing.T) []parityRecord {
	t.Helper()
	data, err := os.ReadFile("../tests/fixtures/parity_baseline.json")
	require.NoError(t, err, "parity_baseline.json must exist at tests/fixtures/")

	var records []parityRecord
	require.NoError(t, json.Unmarshal(data, &records))
	require.Len(t, records, 20, "baseline must have exactly 20 records")
	return records
}

// ── AC-3: parity with ChromaDB/sentence-transformers baseline ────────────────

// TestParityWithChromaDB encodes each text via embed.Default() and asserts
// cosine similarity >= 0.999 against the sentence-transformers baseline vectors
// in parity_baseline.json (AC-3). Requires the real ONNX embedder — mock
// vectors are deterministic but have no relationship to the sentence-transformer
// baseline.
func TestParityWithChromaDB(t *testing.T) {
	requireRealEmbedder(t)

	records := loadParityBaseline(t)
	ctx := context.Background()

	texts := make([]string, len(records))
	for i, r := range records {
		texts[i] = r.Text
	}

	vecs, err := embed.Default().Encode(ctx, texts)
	require.NoError(t, err, "Encode must succeed when ONNX runtime is available")
	require.Len(t, vecs, len(records), "one vector per input text")

	for i, r := range records {
		require.Len(t, vecs[i], embed.EmbeddingDim,
			"vector[%d] must have %d dims", i, embed.EmbeddingDim)

		sim := cosineSimilarity(vecs[i], r.Embedding)
		assert.GreaterOrEqual(t, sim, 0.999,
			"cosine similarity for text[%d] %q must be >= 0.999, got %.6f",
			i, r.Text[:min(40, len(r.Text))], sim)
	}
}

// ── AC-4: cold-start budget ───────────────────────────────────────────────────

// TestColdStartBudget measures wall-clock time from a fresh FastEmbedder to
// the first successful Encode call and asserts <= 5 seconds (AC-4).
// Uses a dedicated FastEmbedder instance (not the package singleton) so the
// test is independent of any prior warm-up done by TestParityWithChromaDB.
func TestColdStartBudget(t *testing.T) {
	ctx := context.Background()
	probe := &embed.FastEmbedder{}

	start := time.Now()
	_, err := probe.Encode(ctx, []string{"cold-start probe"})
	elapsed := time.Since(start)

	skipIfONNXUnavailable(t, err)
	require.NoError(t, err)

	assert.LessOrEqual(t, elapsed, 5*time.Second,
		"cold-start to first embedding must be <= 5 s, got %s", elapsed)
}

// ── AC-2: semantic top-hit ────────────────────────────────────────────────────

// TestSearchSemanticTopHit seeds 10 nodes via create_nodes, queries for
// "authentication patterns", and asserts that the node with auth-related
// observations is the top-1 result (AC-2). Requires Docker + ONNX runtime
// because mock vectors have no semantic structure — only the real embedder
// produces vectors where "authentication patterns" is closer to "OAuth"
// than to "Redis" or "Kubernetes".
func TestSearchSemanticTopHit(t *testing.T) {
	// Guard: skip if DB container is unavailable.
	pool := NewTestPool(t)
	_ = pool

	// Swap to real ONNX (default is mock); cleanup restores. Skip if ONNX
	// unavailable in this environment.
	requireRealEmbedder(t)

	CleanDB(t)
	c := newMCPClient(t)

	// Seed 10 nodes. Only "auth-entity" has auth-related observations;
	// the others cover unrelated topics to make the semantic ranking meaningful.
	nodes := []map[string]any{
		{
			"name":         "auth-entity",
			"nodeType":   "pattern",
			"observations": []string{"OAuth refresh tokens and JWT rotation for authentication", "bearer token validation and session management"},
		},
		{
			"name":         "db-entity",
			"nodeType":   "pattern",
			"observations": []string{"pgvector cosine similarity search for semantic retrieval", "database indexing with HNSW for approximate nearest neighbor"},
		},
		{
			"name":         "deploy-entity",
			"nodeType":   "decision",
			"observations": []string{"Docker container orchestration and Kubernetes deployment", "Render platform for serverless container hosting"},
		},
		{
			"name":         "testing-entity",
			"nodeType":   "pattern",
			"observations": []string{"testcontainers-go for ephemeral database integration tests", "table-driven tests with subtests in Go testing package"},
		},
		{
			"name":         "storage-entity",
			"nodeType":   "service",
			"observations": []string{"Amazon S3 object storage for file persistence", "bucket lifecycle policies and versioning configuration"},
		},
		{
			"name":         "network-entity",
			"nodeType":   "constraint",
			"observations": []string{"TCP connection pooling and keep-alive configuration", "HTTP/2 multiplexing and request pipelining"},
		},
		{
			"name":         "cache-entity",
			"nodeType":   "pattern",
			"observations": []string{"Redis distributed cache with TTL-based eviction", "LRU cache invalidation strategy for frequently accessed data"},
		},
		{
			"name":         "logging-entity",
			"nodeType":   "pattern",
			"observations": []string{"structured JSON logging with log levels and correlation IDs", "centralized log aggregation with search and alerting"},
		},
		{
			"name":         "schema-entity",
			"nodeType":   "decision",
			"observations": []string{"goose migration versioning and rollback strategy", "foreign key constraints with cascade delete for referential integrity"},
		},
		{
			"name":         "mcp-entity",
			"nodeType":   "service",
			"observations": []string{"Model Context Protocol stdio transport for Claude Code integration", "MCP tool registration and handler dispatch pattern"},
		},
	}

	result := callTool(t, c, "create_nodes", map[string]any{
		"nodes": nodes,
	})
	require.False(t, result.IsError,
		"create_nodes must succeed: %s", resultText(t, result))

	// Query for authentication — "auth-entity" must be the top result.
	searchResult := callTool(t, c, "search_nodes", map[string]any{
		"query": "authentication patterns",
	})
	require.False(t, searchResult.IsError,
		"search_nodes must succeed: %s", resultText(t, searchResult))

	var resp struct {
		Nodes []struct {
			Name string `json:"name"`
		} `json:"nodes"`
	}
	unmarshalResult(t, searchResult, &resp)

	require.NotEmpty(t, resp.Nodes, "search must return at least one node")
	assert.Equal(t, "auth-entity", resp.Nodes[0].Name,
		"top result must be auth-entity (AC-2); got %v", resp.Nodes)
}

