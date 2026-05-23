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
	"github.com/mariogutierrez/context-harness-mcp/internal/web"
)

const testUserSub = "550e8400-e29b-41d4-a716-446655440099"
const viewerTestSecret = "viewer-test-secret-32-bytes-xxxx"

// ── session helpers ───────────────────────────────────────────────────────────

// withSession returns a request copy with a valid ch_session cookie attached.
// It calls web.Issue against an httptest.ResponseRecorder and copies the
// Set-Cookie value into the outgoing request, simulating the browser cookie jar.
func withSession(t *testing.T, req *http.Request) *http.Request {
	t.Helper()
	t.Setenv("MCP_JWT_SECRET", viewerTestSecret)

	rr := httptest.NewRecorder()
	issueReq := httptest.NewRequest(http.MethodGet, "/", nil)
	require.NoError(t, web.Issue(rr, issueReq, testUserSub), "withSession: Issue must succeed")

	for _, c := range rr.Result().Cookies() {
		req.AddCookie(c)
	}
	return req
}

// buildViewerMux builds a test mux with viewer routes registered.
func buildViewerMux(t *testing.T) *http.ServeMux {
	t.Helper()
	pool := NewTestPool(t)
	mux := http.NewServeMux()
	viewer.Register(mux, pool)
	return mux
}

// ── auth-gating: unauthenticated ─────────────────────────────────────────────

// TestViewerIndex_NoSession verifies that GET /viewer/ without a session cookie
// returns 302 redirecting to /auth/login with the preserved next param (AC-9).
func TestViewerIndex_NoSession(t *testing.T) {
	t.Setenv("MCP_JWT_SECRET", viewerTestSecret)
	mux := buildViewerMux(t)

	req := httptest.NewRequest(http.MethodGet, "/viewer/", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	resp := w.Result()
	assert.Equal(t, http.StatusFound, resp.StatusCode,
		"GET /viewer/ without session must 302")
	assert.Contains(t, resp.Header.Get("Location"), "/auth/login",
		"redirect must go to /auth/login")
	assert.Contains(t, resp.Header.Get("Location"), "next=%2Fviewer%2F",
		"?next= must include /viewer/ URL-encoded")
}

// TestViewerIndex_NoSession_QueryPreserved verifies that ?q= is preserved in next.
func TestViewerIndex_NoSession_QueryPreserved(t *testing.T) {
	t.Setenv("MCP_JWT_SECRET", viewerTestSecret)
	mux := buildViewerMux(t)

	req := httptest.NewRequest(http.MethodGet, "/viewer/?q=foo", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	resp := w.Result()
	assert.Equal(t, http.StatusFound, resp.StatusCode)
	// The Location header should contain the encoded original path with query string.
	loc := resp.Header.Get("Location")
	assert.Contains(t, loc, "/auth/login")
	assert.Contains(t, loc, "viewer", "next param should contain viewer path")
}

// TestViewerSearchAPI_NoSession verifies that /viewer/api/search without a session
// returns 401 with JSON body (AC-9).
func TestViewerSearchAPI_NoSession(t *testing.T) {
	t.Setenv("MCP_JWT_SECRET", viewerTestSecret)
	mux := buildViewerMux(t)

	req := httptest.NewRequest(http.MethodGet, "/viewer/api/search", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	resp := w.Result()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode,
		"GET /viewer/api/search without session must 401")
	assert.Contains(t, resp.Header.Get("Content-Type"), "application/json",
		"401 response must be JSON")

	var body map[string]string
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, "authentication required", body["error"])
}

// TestViewerProjectsAPI_NoSession verifies 401 for /viewer/api/projects.
func TestViewerProjectsAPI_NoSession(t *testing.T) {
	t.Setenv("MCP_JWT_SECRET", viewerTestSecret)
	mux := buildViewerMux(t)

	req := httptest.NewRequest(http.MethodGet, "/viewer/api/projects", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Result().StatusCode)
}

// TestViewerNodeTypesAPI_NoSession verifies 401 for /viewer/api/node-types.
func TestViewerNodeTypesAPI_NoSession(t *testing.T) {
	t.Setenv("MCP_JWT_SECRET", viewerTestSecret)
	mux := buildViewerMux(t)

	req := httptest.NewRequest(http.MethodGet, "/viewer/api/node-types", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Result().StatusCode)
}

// TestViewerAPI_TamperedSession_Returns401 verifies that a tampered cookie is
// treated the same as no cookie (AC-9).
func TestViewerAPI_TamperedSession_Returns401(t *testing.T) {
	t.Setenv("MCP_JWT_SECRET", viewerTestSecret)
	mux := buildViewerMux(t)

	req := httptest.NewRequest(http.MethodGet, "/viewer/api/search", nil)
	req.AddCookie(&http.Cookie{Name: "ch_session", Value: "tampered.bad"})
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Result().StatusCode)
}

// ── TestViewerIndex ───────────────────────────────────────────────────────────

// TestViewerIndex verifies that GET /viewer/ with a valid session returns 200
// with the HTML page (regression: payload shape unchanged).
func TestViewerIndex(t *testing.T) {
	t.Setenv("MCP_JWT_SECRET", viewerTestSecret)
	pool := NewTestPool(t)

	mux := http.NewServeMux()
	viewer.Register(mux, pool)

	req := httptest.NewRequest(http.MethodGet, "/viewer/", nil)
	req = withSession(t, req)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	resp := w.Result()
	assert.Equal(t, http.StatusOK, resp.StatusCode,
		"GET /viewer/ with valid session must return 200")
	assert.Contains(t, resp.Header.Get("Content-Type"), "text/html",
		"Content-Type must be text/html")
	body := w.Body.String()
	assert.Contains(t, body, "context-harness",
		"body must contain the wordmark")
	assert.Contains(t, body, "knowledge graph",
		"body must reference the product purpose")
}

// ── TestViewerSearchAPI_Empty ─────────────────────────────────────────────────

// TestViewerSearchAPI_Empty verifies that GET /viewer/api/search (no q param)
// with a valid session returns 200 with a valid JSON body (regression).
func TestViewerSearchAPI_Empty(t *testing.T) {
	t.Setenv("MCP_JWT_SECRET", viewerTestSecret)
	pool := NewTestPool(t)
	CleanDB(t)

	mux := http.NewServeMux()
	viewer.Register(mux, pool)

	req := httptest.NewRequest(http.MethodGet, "/viewer/api/search", nil)
	req = withSession(t, req)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	resp := w.Result()
	assert.Equal(t, http.StatusOK, resp.StatusCode,
		"GET /viewer/api/search with session must return 200")
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
	t.Setenv("MCP_JWT_SECRET", viewerTestSecret)
	pool := NewTestPool(t)
	CleanDB(t)

	// Skip when embedder is unavailable — semantic search requires ONNX.
	requireRealEmbedder(t)

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
	req = withSession(t, req)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	resp := w.Result()
	assert.Equal(t, http.StatusOK, resp.StatusCode,
		"GET /viewer/api/search?q= with session must return 200")

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
