// Package tests — integration tests for the suggest_node_type MCP tool.
//
// Covers:
//   - TestSuggestNodeType_HappyPath         (ONNX-gated): top suggestion is "pattern" for auth text.
//   - TestSuggestNodeType_EmptyDB:           empty corpus → empty suggestions, all 9 types in types_unseen.
//   - TestSuggestNodeType_TextEmpty:         empty text → MCP error.
//   - TestSuggestNodeType_TextTooLong:       8193-char text → MCP error.
//   - TestSuggestNodeType_BadProject:        uppercase project → policy/project-naming-violation.
//   - TestSuggestNodeType_ProjectFilter      (ONNX-gated): project-scoped centroids match only that project.
package tests

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	internalmcp "github.com/mariogutierrez/context-harness-mcp/internal/mcp"
	"github.com/mariogutierrez/context-harness-mcp/internal/validate"
)

// newMCPClientForSuggest returns an in-process MCP client backed by the suite pool.
func newMCPClientForSuggest(t *testing.T) *client.Client {
	t.Helper()
	pool := NewTestPool(t)
	srv := internalmcp.New(pool, nil)

	c, err := client.NewInProcessClient(srv)
	require.NoError(t, err, "create in-process client for suggest test")

	startCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	require.NoError(t, c.Start(startCtx), "start in-process client for suggest test")

	initReq := mcp.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcp.Implementation{Name: "suggest-test-client", Version: "0.0.1"}
	_, err = c.Initialize(startCtx, initReq)
	require.NoError(t, err, "initialize MCP session for suggest test")

	return c
}

// suggestResponse is the wire shape of a successful suggest_node_type response.
type suggestResponse struct {
	Suggestions []struct {
		NodeType string  `json:"node_type"`
		Score    float64 `json:"score"`
	} `json:"suggestions"`
	Stats struct {
		CentroidsComputed int      `json:"centroids_computed"`
		TypesUnseen       []string `json:"types_unseen"`
	} `json:"stats"`
}

// ── TestSuggestNodeType_HappyPath ────────────────────────────────────────────

// TestSuggestNodeType_HappyPath seeds 6 nodes across 3 types (pattern, decision,
// constraint), each with thematically distinct auth / deployment / rate-limit
// observations. Invokes suggest_node_type with text about JWT token rotation and
// asserts the top suggestion is "pattern" with centroids_computed == 3.
//
// ONNX-gated: embeddings are required for both seeding and ranking.
func TestSuggestNodeType_HappyPath(t *testing.T) {
	requireEmbedder(t)
	CleanDB(t)

	// Seed 2 pattern nodes with auth-related observations.
	authObs1 := "JWT bearer token validation and rotation policy"
	authObs2 := "OAuth2 authorization code flow with PKCE and refresh tokens"
	patNode1 := insertNodeInProject(t, "auth-pattern-1", "pattern", "global")
	patNode2 := insertNodeInProject(t, "auth-pattern-2", "pattern", "global")
	insertObsWithVec(t, patNode1, authObs1, encodeText(t, authObs1))
	insertObsWithVec(t, patNode2, authObs2, encodeText(t, authObs2))

	// Seed 2 decision nodes with deployment-related observations.
	deployObs1 := "Deploy to Render using Docker container with environment variables"
	deployObs2 := "Blue-green deployment strategy for zero-downtime releases"
	decNode1 := insertNodeInProject(t, "deploy-decision-1", "decision", "global")
	decNode2 := insertNodeInProject(t, "deploy-decision-2", "decision", "global")
	insertObsWithVec(t, decNode1, deployObs1, encodeText(t, deployObs1))
	insertObsWithVec(t, decNode2, deployObs2, encodeText(t, deployObs2))

	// Seed 2 constraint nodes with rate-limit-related observations.
	rateObs1 := "API rate limit: 100 requests per 10 seconds per IP address"
	rateObs2 := "Throttle write operations to prevent abuse of the knowledge graph"
	conNode1 := insertNodeInProject(t, "rate-constraint-1", "constraint", "global")
	conNode2 := insertNodeInProject(t, "rate-constraint-2", "constraint", "global")
	insertObsWithVec(t, conNode1, rateObs1, encodeText(t, rateObs1))
	insertObsWithVec(t, conNode2, rateObs2, encodeText(t, rateObs2))

	c := newMCPClientForSuggest(t)

	result := callToolWithCtx(t, c, context.Background(), "suggest_node_type", map[string]any{
		"text": "JWT bearer token rotation policy",
	})

	require.False(t, result.IsError,
		"suggest_node_type must succeed; got: %s", resultText(t, result))

	var resp suggestResponse
	unmarshalResult(t, result, &resp)

	assert.Equal(t, 3, resp.Stats.CentroidsComputed,
		"centroids_computed must be 3 (pattern, decision, constraint)")

	require.NotEmpty(t, resp.Suggestions,
		"suggestions must be non-empty")
	assert.Equal(t, "pattern", resp.Suggestions[0].NodeType,
		"top suggestion must be 'pattern' for JWT auth text; got %v", resp.Suggestions)

	// types_unseen must include the 6 types not seeded.
	for _, seeded := range []string{"pattern", "decision", "constraint"} {
		assert.NotContains(t, resp.Stats.TypesUnseen, seeded,
			"seeded type %q must NOT appear in types_unseen", seeded)
	}
	assert.Len(t, resp.Stats.TypesUnseen, 9-3,
		"types_unseen must have 6 entries (9 total minus 3 seeded)")
}

// ── TestSuggestNodeType_EmptyDB ───────────────────────────────────────────────

// TestSuggestNodeType_EmptyDB invokes suggest_node_type against an empty corpus
// and asserts the response carries empty suggestions, centroids_computed == 0,
// and types_unseen containing all 9 nodeType values.
func TestSuggestNodeType_EmptyDB(t *testing.T) {
	CleanDB(t)

	c := newMCPClientForSuggest(t)

	result := callToolWithCtx(t, c, context.Background(), "suggest_node_type", map[string]any{
		"text": "some query text",
	})

	require.False(t, result.IsError,
		"suggest_node_type on empty DB must succeed (not an error); got: %s", resultText(t, result))

	var resp suggestResponse
	unmarshalResult(t, result, &resp)

	assert.Empty(t, resp.Suggestions,
		"suggestions must be empty when DB has no observations")
	assert.Equal(t, 0, resp.Stats.CentroidsComputed,
		"centroids_computed must be 0 for empty DB")
	assert.Len(t, resp.Stats.TypesUnseen, len(validate.SortedNodeTypes()),
		"types_unseen must contain all 9 nodeType values; got %v", resp.Stats.TypesUnseen)
}

// ── TestSuggestNodeType_TextEmpty ─────────────────────────────────────────────

// TestSuggestNodeType_TextEmpty asserts that an empty text field triggers an MCP
// error without hitting the DB.
func TestSuggestNodeType_TextEmpty(t *testing.T) {
	c := newMCPClientForSuggest(t)

	result := callToolWithCtx(t, c, context.Background(), "suggest_node_type", map[string]any{
		"text": "",
	})

	require.True(t, result.IsError,
		"empty text must produce IsError=true")
}

// ── TestSuggestNodeType_TextTooLong ───────────────────────────────────────────

// TestSuggestNodeType_TextTooLong asserts that text longer than 8192 characters
// triggers an MCP error without hitting the DB.
func TestSuggestNodeType_TextTooLong(t *testing.T) {
	c := newMCPClientForSuggest(t)

	overlong := strings.Repeat("x", 8193)
	result := callToolWithCtx(t, c, context.Background(), "suggest_node_type", map[string]any{
		"text": overlong,
	})

	require.True(t, result.IsError,
		"text longer than 8192 chars must produce IsError=true")
}

// ── TestSuggestNodeType_BadProject ────────────────────────────────────────────

// TestSuggestNodeType_BadProject asserts that a project value that fails the
// naming regex returns policy/project-naming-violation without touching the DB.
func TestSuggestNodeType_BadProject(t *testing.T) {
	c := newMCPClientForSuggest(t)

	result := callToolWithCtx(t, c, context.Background(), "suggest_node_type", map[string]any{
		"text":    "some query text",
		"project": "BadName", // uppercase forbidden by naming policy
	})

	require.True(t, result.IsError,
		"invalid project name must produce IsError=true")

	var payload map[string]any
	unmarshalResult(t, result, &payload)
	code, _ := payload["code"].(string)
	assert.Equal(t, validate.CodeProjectNamingViolation, code,
		"error code must be policy/project-naming-violation; got %q", code)
}

// ── TestSuggestNodeType_ProjectFilter ────────────────────────────────────────

// TestSuggestNodeType_ProjectFilter seeds nodes in two projects with opposite
// thematic content, then calls suggest_node_type scoped to one project and
// verifies the top suggestion reflects only that project's centroid.
//
// ONNX-gated: embeddings are required for both seeding and ranking.
func TestSuggestNodeType_ProjectFilter(t *testing.T) {
	requireEmbedder(t)
	CleanDB(t)

	// Project "auth-proj": pattern nodes about authentication.
	authObs := "JWT bearer token validation and OAuth2 refresh cycle"
	authNode := insertNodeInProject(t, "auth-node", "pattern", "auth-proj")
	insertObsWithVec(t, authNode, authObs, encodeText(t, authObs))

	// Project "infra-proj": decision nodes about infrastructure deployment.
	infraObs := "Kubernetes cluster autoscaling and rolling update strategy"
	infraNode := insertNodeInProject(t, "infra-node", "decision", "infra-proj")
	insertObsWithVec(t, infraNode, infraObs, encodeText(t, infraObs))

	c := newMCPClientForSuggest(t)

	// Scoped to "auth-proj": only the pattern centroid should be available.
	result := callToolWithCtx(t, c, context.Background(), "suggest_node_type", map[string]any{
		"text":    "JWT bearer token rotation policy",
		"project": "auth-proj",
	})

	require.False(t, result.IsError,
		"project-filtered suggest_node_type must succeed; got: %s", resultText(t, result))

	var resp suggestResponse
	unmarshalResult(t, result, &resp)

	// Only "auth-proj" observations contribute: centroids_computed must be 1
	// (only "pattern" has data in that project).
	assert.Equal(t, 1, resp.Stats.CentroidsComputed,
		"centroids_computed must be 1 when scoped to auth-proj (only pattern seeded)")

	require.Len(t, resp.Suggestions, 1,
		"must return exactly 1 suggestion when only 1 centroid exists")
	assert.Equal(t, "pattern", resp.Suggestions[0].NodeType,
		"suggestion must be 'pattern' for the auth-proj centroid")

	// types_unseen must include all 8 types that are NOT "pattern".
	assert.Len(t, resp.Stats.TypesUnseen, 8,
		"types_unseen must have 8 entries (9 total minus 'pattern'); got %v", resp.Stats.TypesUnseen)
	assert.NotContains(t, resp.Stats.TypesUnseen, "pattern",
		"'pattern' must not appear in types_unseen")
}
