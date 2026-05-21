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

// ── TestViewer_NodeTypesAPI ───────────────────────────────────────────────────

// TestViewer_NodeTypesAPI verifies that GET /viewer/api/node-types with a valid
// session returns 200 with a JSON body containing exactly the 9 canonical node
// types in alphabetical order. The list is static (no DB required).
func TestViewer_NodeTypesAPI(t *testing.T) {
	t.Setenv("MCP_JWT_SECRET", viewerTestSecret)
	pool := NewTestPool(t)

	mux := http.NewServeMux()
	viewer.Register(mux, pool)

	req := httptest.NewRequest(http.MethodGet, "/viewer/api/node-types", nil)
	req = withSession(t, req)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	resp := w.Result()
	assert.Equal(t, http.StatusOK, resp.StatusCode,
		"GET /viewer/api/node-types with session must return 200")
	assert.Contains(t, resp.Header.Get("Content-Type"), "application/json",
		"Content-Type must be application/json")

	var body struct {
		NodeTypes []string `json:"nodeTypes"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body),
		"response body must be valid JSON")

	// Exactly 9 node types as defined in internal/validate/taxonomy.go.
	assert.Equal(t, 9, len(body.NodeTypes),
		"must return exactly 9 node types; got %v", body.NodeTypes)

	// Types must be alphabetically sorted.
	for i := 1; i < len(body.NodeTypes); i++ {
		assert.LessOrEqual(t, body.NodeTypes[i-1], body.NodeTypes[i],
			"node types must be in alphabetical order: %v", body.NodeTypes)
	}

	// Spot-check known values.
	assert.Contains(t, body.NodeTypes, "constraint", "must include 'constraint'")
	assert.Contains(t, body.NodeTypes, "decision", "must include 'decision'")
	assert.Contains(t, body.NodeTypes, "pattern", "must include 'pattern'")
}

// ── TestViewer_SearchWithNodeTypeFilter ───────────────────────────────────────

// TestViewer_SearchWithNodeTypeFilter seeds nodes of different types and verifies
// that ?nodeType=pattern returns only pattern-typed nodes.
func TestViewer_SearchWithNodeTypeFilter(t *testing.T) {
	t.Setenv("MCP_JWT_SECRET", viewerTestSecret)
	pool := NewTestPool(t)
	CleanDB(t)

	ctx := context.Background()

	// Seed three nodes of three different types via raw SQL (no embedding
	// needed for the list-all path that this test exercises).
	_, err := pool.Exec(ctx, `
		INSERT INTO nodes (name, node_type, project_id) VALUES
		  ('nt-filter-alpha',   'pattern',  'global'),
		  ('nt-filter-beta',    'decision', 'global'),
		  ('nt-filter-gamma',   'pattern',  'global')
	`)
	require.NoError(t, err, "seed nodes for nodeType filter test")

	mux := http.NewServeMux()
	viewer.Register(mux, pool)

	req := httptest.NewRequest(http.MethodGet, "/viewer/api/search?nodeType=pattern", nil)
	req = withSession(t, req)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	resp := w.Result()
	assert.Equal(t, http.StatusOK, resp.StatusCode,
		"GET /viewer/api/search?nodeType=pattern with session must return 200")

	var body struct {
		NodeCount int `json:"node_count"`
		Nodes     []struct {
			Name     string `json:"name"`
			NodeType string `json:"node_type"`
		} `json:"nodes"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body),
		"response must be valid JSON")

	assert.Equal(t, 2, body.NodeCount,
		"nodeType=pattern filter must return exactly 2 nodes; got %d", body.NodeCount)

	for _, n := range body.Nodes {
		assert.Equal(t, "pattern", n.NodeType,
			"all returned nodes must have node_type=pattern; got %q for %q", n.NodeType, n.Name)
	}

	// The decision node must not appear.
	names := make([]string, len(body.Nodes))
	for i, n := range body.Nodes {
		names[i] = n.Name
	}
	assert.False(t, containsName(names, "nt-filter-beta"),
		"decision-typed node must not appear in pattern-only results")
}

// ── TestViewer_SearchIncludesRelations ────────────────────────────────────────

// TestViewer_SearchIncludesRelations seeds nodes and a relation, then verifies
// that the search response includes the relation on the relevant node.
func TestViewer_SearchIncludesRelations(t *testing.T) {
	t.Setenv("MCP_JWT_SECRET", viewerTestSecret)
	pool := NewTestPool(t)
	CleanDB(t)

	ctx := context.Background()

	// Seed two nodes and one relation between them.
	var fromID, toID string
	err := pool.QueryRow(ctx,
		`INSERT INTO nodes (name, node_type, project_id) VALUES ('rel-source', 'pattern', 'global') RETURNING id`,
	).Scan(&fromID)
	require.NoError(t, err, "seed from-node")

	err = pool.QueryRow(ctx,
		`INSERT INTO nodes (name, node_type, project_id) VALUES ('rel-target', 'decision', 'global') RETURNING id`,
	).Scan(&toID)
	require.NoError(t, err, "seed to-node")

	_, err = pool.Exec(ctx,
		`INSERT INTO relations (from_node_id, to_node_id, relation_type, project_id)
		 VALUES ($1, $2, 'relates_to', 'global')`,
		fromID, toID,
	)
	require.NoError(t, err, "seed relation")

	mux := http.NewServeMux()
	viewer.Register(mux, pool)

	req := httptest.NewRequest(http.MethodGet, "/viewer/api/search", nil)
	req = withSession(t, req)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	resp := w.Result()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var body struct {
		Nodes []struct {
			Name      string `json:"name"`
			Relations []struct {
				FromName     string `json:"from_name"`
				ToName       string `json:"to_name"`
				RelationType string `json:"relation_type"`
			} `json:"relations"`
		} `json:"nodes"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body),
		"response must be valid JSON")

	require.NotEmpty(t, body.Nodes, "must return at least one node")

	// Find rel-source in the results and verify it carries the relation.
	var sourceNode *struct {
		Name      string `json:"name"`
		Relations []struct {
			FromName     string `json:"from_name"`
			ToName       string `json:"to_name"`
			RelationType string `json:"relation_type"`
		} `json:"relations"`
	}
	for i := range body.Nodes {
		if body.Nodes[i].Name == "rel-source" {
			sourceNode = &body.Nodes[i]
			break
		}
	}
	require.NotNil(t, sourceNode,
		"'rel-source' node must appear in search results; got: %v",
		nodeNames(body.Nodes))

	require.NotEmpty(t, sourceNode.Relations,
		"'rel-source' must have at least one relation in the response")

	rel := sourceNode.Relations[0]
	assert.Equal(t, "rel-source", rel.FromName, "from_name must be rel-source")
	assert.Equal(t, "rel-target", rel.ToName, "to_name must be rel-target")
	assert.Equal(t, "relates_to", rel.RelationType, "relation_type must be relates_to")
}

// ── TestViewer_RendersUpdatedHTML ─────────────────────────────────────────────

// TestViewer_RendersUpdatedHTML verifies that GET /viewer/ with a valid session
// returns HTML that contains the nodetype-filter select element and the
// relations-related CSS class names introduced in Phase 5 Item D.
func TestViewer_RendersUpdatedHTML(t *testing.T) {
	t.Setenv("MCP_JWT_SECRET", viewerTestSecret)
	pool := NewTestPool(t)

	mux := http.NewServeMux()
	viewer.Register(mux, pool)

	req := httptest.NewRequest(http.MethodGet, "/viewer/", nil)
	req = withSession(t, req)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	resp := w.Result()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, resp.Header.Get("Content-Type"), "text/html")

	body := w.Body.String()

	// Node-type filter dropdown must be present.
	assert.True(t, strings.Contains(body, `id="nodetype-filter"`),
		`body must contain <select id="nodetype-filter">`)
	assert.True(t, strings.Contains(body, `aria-label="filter by node type"`),
		"nodetype-filter must have an aria-label for accessibility")

	// Relations CSS tokens must be present.
	assert.True(t, strings.Contains(body, "ch-link-relation"),
		"body must define .ch-link-relation CSS class")
	assert.True(t, strings.Contains(body, "ch-link-target"),
		"body must define .ch-link-target CSS class")
	assert.True(t, strings.Contains(body, "ch-badge-superseded"),
		"body must define .ch-badge-superseded CSS class")

	// The node-types API endpoint must be referenced in JS.
	assert.True(t, strings.Contains(body, "/viewer/api/node-types"),
		"JS must reference /viewer/api/node-types endpoint")
}

// ── helpers ───────────────────────────────────────────────────────────────────

// nodeNames extracts node names from the anonymous struct slice used in
// TestViewer_SearchIncludesRelations for readable assertion messages.
func nodeNames(nodes []struct {
	Name      string `json:"name"`
	Relations []struct {
		FromName     string `json:"from_name"`
		ToName       string `json:"to_name"`
		RelationType string `json:"relation_type"`
	} `json:"relations"`
}) []string {
	names := make([]string, len(nodes))
	for i, n := range nodes {
		names[i] = n.Name
	}
	return names
}
