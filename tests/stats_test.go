// Package tests — integration tests for the stats MCP tool (PR-1).
//
// Covers:
//   - AC-1: Populated DB returns correct counts + by_type map + ordered extremes.
//   - AC-2: Empty DB (post-migrations, no seed) returns zero counts, empty
//     by_type map, and null oldest_node / newest_node.
//   - AC-3: Soft-deleted nodes are excluded from node_count and by_type.
//   - AC-4: stats handler does not call checkRateLimit — verified by issuing
//     100 successive calls in 1 second and asserting none return IsError=true.
//   - AC-5: "stats" tool is registered with no input-schema properties and no
//     required fields (read-only, no arguments).
package tests

import (
	"context"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	pgvector "github.com/pgvector/pgvector-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariogutierrez/context-harness-mcp/internal/store"
)

// ── seed helpers ──────────────────────────────────────────────────────────────

// insertNode inserts a single active node directly via the store (bypasses the
// MCP write path so we don't need the embedder). Returns the node UUID string.
func insertNode(t *testing.T, name, nodeType string) string {
	t.Helper()
	pool := NewTestPool(t)
	ctx := context.Background()

	tx, err := pool.Begin(ctx)
	require.NoError(t, err, "insertNode: begin tx")
	id, err := store.Create(ctx, tx, name, nodeType, "global", nil, nil)
	require.NoError(t, err, "insertNode: store.Create %q", name)
	require.NoError(t, tx.Commit(ctx), "insertNode: commit")
	return id
}

// insertObservation inserts a single observation (with NULL embedding) for the
// given node ID. Uses store.Insert directly so the embedder is not required.
// Passing pgvector.NewVector(nil) produces a zero-length slice, which
// store.Insert stores as SQL NULL (degraded / no-embedding mode).
func insertObservation(t *testing.T, nodeID, text string) {
	t.Helper()
	pool := NewTestPool(t)
	ctx := context.Background()

	tx, err := pool.Begin(ctx)
	require.NoError(t, err, "insertObservation: begin tx")
	_, _, err = store.Insert(ctx, tx, nodeID, text, pgvector.NewVector(nil), nil, nil)
	require.NoError(t, err, "insertObservation: store.Insert %q", text)
	require.NoError(t, tx.Commit(ctx), "insertObservation: commit")
}

// insertRelationByIDs inserts a single active relation directly via SQL.
// store.InsertRelation requires a pgx.Tx; we open one here.
func insertRelationByIDs(t *testing.T, fromID, toID, relType string) {
	t.Helper()
	pool := NewTestPool(t)
	ctx := context.Background()

	tx, err := pool.Begin(ctx)
	require.NoError(t, err, "insertRelationByIDs: begin tx")
	_, _, err = store.InsertRelation(ctx, tx, fromID, toID, relType, "global", nil, nil)
	require.NoError(t, err, "insertRelationByIDs: store.InsertRelation %s→%s", fromID, toID)
	require.NoError(t, tx.Commit(ctx), "insertRelationByIDs: commit")
}

// softDeleteNodeByName soft-deletes the active node with the given name via
// store.MarkDeletedByNames inside a transaction.
func softDeleteNodeByName(t *testing.T, name string) {
	t.Helper()
	pool := NewTestPool(t)
	ctx := context.Background()

	tx, err := pool.Begin(ctx)
	require.NoError(t, err, "softDeleteNodeByName: begin tx")
	n, err := store.MarkDeletedByNames(ctx, tx, []string{name})
	require.NoError(t, err, "softDeleteNodeByName: MarkDeletedByNames %q", name)
	require.Equal(t, 1, n, "softDeleteNodeByName: expected 1 row updated for %q", name)
	require.NoError(t, tx.Commit(ctx), "softDeleteNodeByName: commit")
}

// ── shared stats call helper ──────────────────────────────────────────────────

// callStats invokes the "stats" tool via the in-process MCP harness and returns
// the parsed JSON response body as a map. The call must succeed at the protocol
// level (IsError must be false); the test fails immediately otherwise.
func callStats(t *testing.T) map[string]any {
	t.Helper()
	c := newMCPClient(t)
	result := callTool(t, c, "stats", map[string]any{})
	require.False(t, result.IsError,
		"stats tool must succeed; got error body: %s", resultText(t, result))

	var body map[string]any
	unmarshalResult(t, result, &body)
	return body
}

// ── AC-1: happy path ─────────────────────────────────────────────────────────

// TestStats_HappyPath seeds 3 nodes (types: pattern, error, pattern), 5
// observations, and 2 relations, then asserts the exact wire-shape returned by
// the stats tool.
//
// AC-1: node_count=3, observation_count=5, relation_count=2,
// by_type={pattern:2,error:1}, oldest_node.created_at <= newest_node.created_at.
func TestStats_HappyPath(t *testing.T) {
	CleanDB(t)

	// Seed 3 nodes: two of type "pattern", one of type "error".
	nodeA := insertNode(t, "stats-node-pattern-a", "pattern")
	nodeB := insertNode(t, "stats-node-error-b", "error")
	nodeC := insertNode(t, "stats-node-pattern-c", "pattern")

	// Seed 5 observations spread across nodes (no embedder needed).
	insertObservation(t, nodeA, "obs-a1")
	insertObservation(t, nodeA, "obs-a2")
	insertObservation(t, nodeB, "obs-b1")
	insertObservation(t, nodeC, "obs-c1")
	insertObservation(t, nodeC, "obs-c2")

	// Seed 2 active relations.
	insertRelationByIDs(t, nodeA, nodeB, "relates_to")
	insertRelationByIDs(t, nodeB, nodeC, "relates_to")

	body := callStats(t)

	// ── counts ────────────────────────────────────────────────────────────────

	assert.Equal(t, float64(3), body["node_count"],
		"AC-1: node_count must be 3")
	assert.Equal(t, float64(5), body["observation_count"],
		"AC-1: observation_count must be 5")
	assert.Equal(t, float64(2), body["relation_count"],
		"AC-1: relation_count must be 2")

	// ── by_type exact map ─────────────────────────────────────────────────────

	byType, ok := body["by_type"].(map[string]any)
	require.True(t, ok, "AC-1: by_type must be a JSON object, got %T", body["by_type"])
	assert.Equal(t, float64(2), byType["pattern"],
		"AC-1: by_type.pattern must be 2")
	assert.Equal(t, float64(1), byType["error"],
		"AC-1: by_type.error must be 1")
	assert.Len(t, byType, 2,
		"AC-1: by_type must have exactly 2 entries (pattern, error)")

	// ── oldest_node / newest_node non-null and ordered ────────────────────────

	oldest, ok := body["oldest_node"].(map[string]any)
	require.True(t, ok, "AC-1: oldest_node must be a non-null JSON object, got %T", body["oldest_node"])
	require.NotEmpty(t, oldest["name"], "AC-1: oldest_node.name must be non-empty")
	require.NotEmpty(t, oldest["created_at"], "AC-1: oldest_node.created_at must be non-empty")

	newest, ok := body["newest_node"].(map[string]any)
	require.True(t, ok, "AC-1: newest_node must be a non-null JSON object, got %T", body["newest_node"])
	require.NotEmpty(t, newest["name"], "AC-1: newest_node.name must be non-empty")
	require.NotEmpty(t, newest["created_at"], "AC-1: newest_node.created_at must be non-empty")

	oldestAt, err := time.Parse(time.RFC3339Nano, oldest["created_at"].(string))
	require.NoError(t, err, "AC-1: oldest_node.created_at must parse as RFC3339")
	newestAt, err := time.Parse(time.RFC3339Nano, newest["created_at"].(string))
	require.NoError(t, err, "AC-1: newest_node.created_at must parse as RFC3339")

	assert.True(t, !oldestAt.After(newestAt),
		"AC-1: oldest_node.created_at (%s) must be <= newest_node.created_at (%s)",
		oldestAt, newestAt)
}

// ── AC-2: empty DB ────────────────────────────────────────────────────────────

// TestStats_EmptyDB calls stats on a fresh schema (no seed data) and asserts
// zero counts, an empty (non-nil) by_type map, and JSON-null extremes.
//
// AC-2: node_count=0, observation_count=0, relation_count=0, by_type={},
// oldest_node=null, newest_node=null.
func TestStats_EmptyDB(t *testing.T) {
	CleanDB(t)

	body := callStats(t)

	// ── counts must all be zero ───────────────────────────────────────────────

	assert.Equal(t, float64(0), body["node_count"],
		"AC-2: node_count must be 0 on empty DB")
	assert.Equal(t, float64(0), body["observation_count"],
		"AC-2: observation_count must be 0 on empty DB")
	assert.Equal(t, float64(0), body["relation_count"],
		"AC-2: relation_count must be 0 on empty DB")

	// ── by_type must be an empty map (not nil) ────────────────────────────────

	byType, ok := body["by_type"].(map[string]any)
	require.True(t, ok,
		"AC-2: by_type must be a JSON object ({}), got %T: %v", body["by_type"], body["by_type"])
	assert.Empty(t, byType,
		"AC-2: by_type must be empty ({}) when no nodes exist")

	// ── extremes must be JSON null ────────────────────────────────────────────

	// json.Unmarshal represents JSON null as Go nil (not a map).
	assert.Nil(t, body["oldest_node"],
		"AC-2: oldest_node must be JSON null on empty DB, got %v", body["oldest_node"])
	assert.Nil(t, body["newest_node"],
		"AC-2: newest_node must be JSON null on empty DB, got %v", body["newest_node"])
}

// ── AC-3: soft-deleted nodes excluded ────────────────────────────────────────

// TestStats_ExcludesSoftDeleted seeds 3 nodes and soft-deletes 1, then asserts
// the deleted node is absent from node_count and by_type.
//
// AC-3: Given node soft-deleted (deleted_at IS NOT NULL), stats must not count
// that node in node_count nor in by_type.
func TestStats_ExcludesSoftDeleted(t *testing.T) {
	CleanDB(t)

	// Seed 3 nodes: two "pattern", one "error".
	insertNode(t, "sd-node-a", "pattern")
	insertNode(t, "sd-node-b", "pattern")
	insertNode(t, "sd-node-c", "error")

	// Soft-delete the "error" node.
	softDeleteNodeByName(t, "sd-node-c")

	body := callStats(t)

	// Only the 2 remaining active nodes should be counted.
	assert.Equal(t, float64(2), body["node_count"],
		"AC-3: node_count must exclude the soft-deleted node")

	byType, ok := body["by_type"].(map[string]any)
	require.True(t, ok, "AC-3: by_type must be a JSON object")

	// "error" type had only one node and it was deleted — must not appear.
	_, hasError := byType["error"]
	assert.False(t, hasError,
		"AC-3: by_type must not contain 'error' when the only error node is soft-deleted")

	// "pattern" type still has 2 active nodes.
	assert.Equal(t, float64(2), byType["pattern"],
		"AC-3: by_type.pattern must be 2 (unaffected active nodes)")

	assert.Len(t, byType, 1,
		"AC-3: by_type must have exactly 1 entry after deleting the only error node")
}

// ── AC-4: no rate-limit check ─────────────────────────────────────────────────

// TestStats_NoRateLimitCheck verifies that the stats handler does not enforce a
// rate limit by making 100 successive calls within one second and asserting that
// none of them return IsError=true. Write tools are rate-limited; read-only
// tools (read_graph, open_nodes, stats) are not.
//
// AC-4: VERIFY: stats handler does not call checkRateLimit.
// Strategy: 100 in-process calls in <1 s — any rate-limit would surface as
// IsError=true at some call. If all 100 pass, the check is absent.
func TestStats_NoRateLimitCheck(t *testing.T) {
	CleanDB(t)
	// Seed one node so the response is non-trivial.
	insertNode(t, "rate-limit-probe-node", "pattern")

	c := newMCPClient(t)
	ctx := context.Background()

	const n = 100
	for i := range n {
		result := callToolWithCtx(t, c, ctx, "stats", map[string]any{})
		require.False(t, result.IsError,
			"AC-4: stats call %d/%d must not be rate-limited (IsError=true): %s",
			i+1, n, resultText(t, result))
	}
}

// ── AC-5: tool registered correctly ──────────────────────────────────────────

// TestStats_RegisteredCorrectly verifies that "stats" appears in the server's
// tools/list response with name "stats" and no declared input-schema properties
// (read-only tool, zero arguments).
//
// AC-5: VERIFY: mcplib.NewTool("stats", ...) without arg declarations produces
// an entry with an empty or absent "properties" map and no "required" fields.
func TestStats_RegisteredCorrectly(t *testing.T) {
	c := newMCPClient(t)
	ctx := context.Background()

	listResult, err := c.ListTools(ctx, mcp.ListToolsRequest{})
	require.NoError(t, err, "AC-5: ListTools must succeed")
	require.NotNil(t, listResult, "AC-5: ListTools result must not be nil")

	// Find the "stats" tool in the list.
	var statsTool *mcp.Tool
	for i := range listResult.Tools {
		if listResult.Tools[i].Name == "stats" {
			statsTool = &listResult.Tools[i]
			break
		}
	}
	require.NotNil(t, statsTool,
		"AC-5: 'stats' tool must be present in the server's tool list")

	// stats is a read-only, argument-free tool — its InputSchema must declare
	// no properties and no required fields.
	schema := statsTool.InputSchema
	assert.Empty(t, schema.Properties,
		"AC-5: stats tool must have no declared input-schema properties (no arguments)")
	assert.Empty(t, schema.Required,
		"AC-5: stats tool must have no required fields")
}
