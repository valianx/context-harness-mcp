package tests

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariogutierrez/context-harness-mcp/internal/viewer"
)

// ── TestViewerIndex ───────────────────────────────────────────────────────────

// TestViewerIndex verifies that GET /viewer/ returns 200 with the HTML page.
func TestViewerIndex(t *testing.T) {
	pool := NewTestPool(t)

	mux := http.NewServeMux()
	viewer.Register(mux, pool)

	req := httptest.NewRequest(http.MethodGet, "/viewer/", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	resp := w.Result()
	assert.Equal(t, http.StatusOK, resp.StatusCode,
		"GET /viewer/ must return 200")
	assert.Contains(t, resp.Header.Get("Content-Type"), "text/html",
		"Content-Type must be text/html")
	assert.Contains(t, w.Body.String(), "Context Harness MCP",
		"body must contain the page title")
}

// ── TestViewerSearchAPI_Empty ─────────────────────────────────────────────────

// TestViewerSearchAPI_Empty verifies that GET /viewer/api/search (no q param)
// returns 200 with a valid JSON body and an array nodes field.
func TestViewerSearchAPI_Empty(t *testing.T) {
	pool := NewTestPool(t)
	CleanDB(t)

	mux := http.NewServeMux()
	viewer.Register(mux, pool)

	req := httptest.NewRequest(http.MethodGet, "/viewer/api/search", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	resp := w.Result()
	assert.Equal(t, http.StatusOK, resp.StatusCode,
		"GET /viewer/api/search must return 200")
	assert.Contains(t, resp.Header.Get("Content-Type"), "application/json",
		"Content-Type must be application/json")

	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body),
		"response body must be valid JSON")

	nodes, ok := body["nodes"]
	require.True(t, ok, "response must have a 'nodes' field")
	_, isSlice := nodes.([]any)
	assert.True(t, isSlice, "'nodes' must be a JSON array")
}

// ── TestViewerSearchAPI_WithQuery ─────────────────────────────────────────────

// TestViewerSearchAPI_WithQuery seeds two nodes and verifies that a semantic
// search query surfaces the matching node in the results. Skips when the ONNX
// embedder is unavailable (CGO-disabled environments / Windows dev boxes).
func TestViewerSearchAPI_WithQuery(t *testing.T) {
	pool := NewTestPool(t)
	CleanDB(t)

	// Skip when embedder is unavailable — semantic search requires ONNX.
	requireEmbedder(t)

	ctx := context.Background()

	// Seed two nodes directly via SQL so we avoid coupling to MCP tool internals.
	// The observations need embeddings for cosine search; we insert them via the
	// MCP server's in-process client which runs the full embedding pipeline.
	mux := http.NewServeMux()
	viewer.Register(mux, pool)

	// Insert nodes using the store-adjacent approach: raw SQL for nodes +
	// observations table. We use the seeded MCP client from tools_test helpers
	// to ensure embeddings are computed properly.
	c := newMCPClient(t)

	callTool(t, c, "create_nodes", map[string]any{
		"nodes": []map[string]any{
			{
				"name":         "viewer-test-node-alpha",
				"nodeType":     "pattern",
				"observations": []string{"authentication via OAuth2 bearer tokens"},
			},
			{
				"name":         "viewer-test-node-beta",
				"nodeType":     "decision",
				"observations": []string{"use PostgreSQL for persistent storage"},
			},
		},
	})

	// Verify seed worked.
	var nodeCount int
	err := pool.QueryRow(ctx, "SELECT COUNT(*) FROM nodes WHERE deleted_at IS NULL").Scan(&nodeCount)
	require.NoError(t, err)
	require.Equal(t, 2, nodeCount, "two nodes must be seeded before the search test")

	// Search for "oauth authentication" — should surface viewer-test-node-alpha.
	req := httptest.NewRequest(http.MethodGet, "/viewer/api/search?q=oauth+authentication", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	resp := w.Result()
	assert.Equal(t, http.StatusOK, resp.StatusCode,
		"GET /viewer/api/search?q= must return 200")

	var body struct {
		Query     string `json:"query"`
		NodeCount int    `json:"node_count"`
		Nodes     []struct {
			Name     string `json:"name"`
			NodeType string `json:"node_type"`
		} `json:"nodes"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body),
		"response must be valid JSON")

	assert.Equal(t, "oauth authentication", body.Query,
		"query field must echo the search term")
	require.NotEmpty(t, body.Nodes, "semantic search must return at least one node")

	// The most relevant node for "oauth authentication" must be in the results.
	names := make([]string, len(body.Nodes))
	for i, n := range body.Nodes {
		names[i] = n.Name
	}
	assert.True(t, containsName(names, "viewer-test-node-alpha"),
		"viewer-test-node-alpha (oauth observation) must appear in results; got: %s",
		strings.Join(names, ", "))
}

func containsName(names []string, target string) bool {
	for _, n := range names {
		if n == target {
			return true
		}
	}
	return false
}
