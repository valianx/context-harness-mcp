// Package tests — integration tests for Phase 4: Sessions.
//
// Covers:
//   - TestSessionStart_HappyPath: session_start returns UUID + started_at, DB row exists.
//   - TestSessionStart_DefaultProject: omitting project → project_id = "global".
//   - TestSessionStart_BadProject: uppercase project → policy/project-naming-violation.
//   - TestSessionEnd_HappyPath: session_end stores ended_at and summary.
//   - TestSessionEnd_Idempotent: second session_end returns same ended_at.
//   - TestSessionEnd_NotFound: bogus session_id → policy/session-not-found.
//   - TestSessionEnd_TooLongSummary: 4097-char summary → MCP error.
//   - TestSessionSummary_HappyPath: 3 nodes attached, summary returns them in order.
//   - TestSessionSummary_IncludesSoftDeleted: soft-deleted nodes appear in summary.
//   - TestSessionSummary_NotFound: bogus session_id → policy/session-not-found.
//   - TestCreateNodesWithSession_HappyPath: nodes.session_id set in DB.
//   - TestCreateNodesWithSession_NoSession: ended session_id → policy/session-already-ended.
//   - TestCreateNodesWithSession_BadSessionID: non-UUID → MCP error.
//   - TestFullSequence: session_start → 5 create_nodes → session_end → session_summary.
package tests

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── helpers ───────────────────────────────────────────────────────────────────

// startSession calls session_start and returns the session_id UUID string.
func startSession(t *testing.T, project, workingDir string) string {
	t.Helper()
	c := newMCPClient(t)
	args := map[string]any{}
	if project != "" {
		args["project"] = project
	}
	if workingDir != "" {
		args["working_dir"] = workingDir
	}
	result := callTool(t, c, "session_start", args)
	require.False(t, result.IsError,
		"session_start must succeed; got: %s", resultText(t, result))
	var resp map[string]any
	unmarshalResult(t, result, &resp)
	sessionID, ok := resp["session_id"].(string)
	require.True(t, ok && sessionID != "", "session_id must be a non-empty string")
	return sessionID
}

// endSession calls session_end and returns the parsed response map.
func endSession(t *testing.T, sessionID, summary string) map[string]any {
	t.Helper()
	c := newMCPClient(t)
	args := map[string]any{"session_id": sessionID}
	if summary != "" {
		args["summary"] = summary
	}
	result := callTool(t, c, "session_end", args)
	require.False(t, result.IsError,
		"session_end must succeed; got: %s", resultText(t, result))
	var resp map[string]any
	unmarshalResult(t, result, &resp)
	return resp
}

// querySessionRow fetches (ended_at, summary) for the session from DB.
func querySessionRow(t *testing.T, sessionID string) (endedAt *string, summary *string) {
	t.Helper()
	pool := NewTestPool(t)
	err := pool.QueryRow(context.Background(),
		`SELECT ended_at::text, summary FROM sessions WHERE id = $1`,
		sessionID,
	).Scan(&endedAt, &summary)
	require.NoError(t, err, "querySessionRow: session %q must exist", sessionID)
	return endedAt, summary
}

// queryNodeSessionID fetches the session_id (as *string) for the active node
// with the given name.
func queryNodeSessionID(t *testing.T, name string) *string {
	t.Helper()
	pool := NewTestPool(t)
	var sid *string
	err := pool.QueryRow(context.Background(),
		`SELECT session_id::text FROM nodes WHERE name = $1 AND deleted_at IS NULL`,
		name,
	).Scan(&sid)
	require.NoError(t, err, "queryNodeSessionID: node %q must exist", name)
	return sid
}

// ── TestSessionStart_HappyPath ────────────────────────────────────────────────

func TestSessionStart_HappyPath(t *testing.T) {
	CleanDB(t)
	c := newMCPClient(t)

	result := callTool(t, c, "session_start", map[string]any{
		"project":     "foo",
		"working_dir": "/tmp/x",
	})

	assert.False(t, result.IsError, "session_start must succeed: %s", resultText(t, result))

	var resp map[string]any
	unmarshalResult(t, result, &resp)

	sessionID, ok := resp["session_id"].(string)
	require.True(t, ok && len(sessionID) == 36,
		"session_id must be a 36-char UUID, got: %v", resp["session_id"])

	startedAt, ok := resp["started_at"].(string)
	require.True(t, ok && startedAt != "", "started_at must be a non-empty string")

	// Verify DB row exists with correct project_id and working_dir.
	pool := NewTestPool(t)
	var projectID, workingDir string
	err := pool.QueryRow(context.Background(),
		`SELECT project_id, working_dir FROM sessions WHERE id = $1`,
		sessionID,
	).Scan(&projectID, &workingDir)
	require.NoError(t, err, "sessions row must exist after session_start")
	assert.Equal(t, "foo", projectID)
	assert.Equal(t, "/tmp/x", workingDir)
}

// ── TestSessionStart_DefaultProject ──────────────────────────────────────────

func TestSessionStart_DefaultProject(t *testing.T) {
	CleanDB(t)
	c := newMCPClient(t)

	result := callTool(t, c, "session_start", map[string]any{})
	assert.False(t, result.IsError, "session_start without project must succeed")

	var resp map[string]any
	unmarshalResult(t, result, &resp)
	sessionID := resp["session_id"].(string)

	pool := NewTestPool(t)
	var projectID string
	err := pool.QueryRow(context.Background(),
		`SELECT project_id FROM sessions WHERE id = $1`,
		sessionID,
	).Scan(&projectID)
	require.NoError(t, err)
	assert.Equal(t, "global", projectID,
		"project_id must default to 'global' when not provided")
}

// ── TestSessionStart_BadProject ───────────────────────────────────────────────

func TestSessionStart_BadProject(t *testing.T) {
	CleanDB(t)
	c := newMCPClient(t)

	// Uppercase project name must be rejected.
	result := callTool(t, c, "session_start", map[string]any{"project": "Foo"})

	assert.True(t, result.IsError, "session_start with invalid project must fail")

	var resp map[string]any
	unmarshalResult(t, result, &resp)
	assert.Equal(t, "policy/project-naming-violation", resp["code"],
		"error code must be policy/project-naming-violation")
}

// ── TestSessionEnd_HappyPath ──────────────────────────────────────────────────

func TestSessionEnd_HappyPath(t *testing.T) {
	CleanDB(t)

	sessionID := startSession(t, "foo", "")

	c := newMCPClient(t)
	result := callTool(t, c, "session_end", map[string]any{
		"session_id": sessionID,
		"summary":    "all done",
	})

	assert.False(t, result.IsError, "session_end must succeed: %s", resultText(t, result))

	var resp map[string]any
	unmarshalResult(t, result, &resp)
	assert.Equal(t, sessionID, resp["session_id"])
	assert.NotEmpty(t, resp["ended_at"], "ended_at must be set")

	// Verify DB row has ended_at and summary persisted.
	endedAt, summary := querySessionRow(t, sessionID)
	require.NotNil(t, endedAt, "ended_at must be set in DB")
	require.NotNil(t, summary, "summary must be set in DB")
	assert.Equal(t, "all done", *summary)
}

// ── TestSessionEnd_Idempotent ─────────────────────────────────────────────────

func TestSessionEnd_Idempotent(t *testing.T) {
	CleanDB(t)

	sessionID := startSession(t, "", "")

	// First end.
	resp1 := endSession(t, sessionID, "first end")
	endedAt1, ok := resp1["ended_at"].(string)
	require.True(t, ok && endedAt1 != "", "first ended_at must be a non-empty string")

	// Give a moment to ensure clock advances if nanosecond precision is truncated.
	time.Sleep(10 * time.Millisecond)

	// Second end must return the same ended_at and must not overwrite summary.
	c := newMCPClient(t)
	result2 := callTool(t, c, "session_end", map[string]any{
		"session_id": sessionID,
		"summary":    "second end attempt — must not overwrite",
	})
	assert.False(t, result2.IsError, "second session_end must succeed (idempotent)")

	var resp2 map[string]any
	unmarshalResult(t, result2, &resp2)
	endedAt2, ok := resp2["ended_at"].(string)
	require.True(t, ok, "second ended_at must be a string")

	// Truncate to seconds for comparison — RFC3339 may vary in sub-second detail.
	assert.Equal(t, endedAt1[:19], endedAt2[:19],
		"second session_end must return the same ended_at (idempotent)")

	// Summary must still be the first one.
	_, summary := querySessionRow(t, sessionID)
	require.NotNil(t, summary)
	assert.Equal(t, "first end", *summary,
		"summary must not be overwritten by second session_end")
}

// ── TestSessionEnd_NotFound ───────────────────────────────────────────────────

func TestSessionEnd_NotFound(t *testing.T) {
	CleanDB(t)
	c := newMCPClient(t)

	result := callTool(t, c, "session_end", map[string]any{
		"session_id": "00000000-0000-0000-0000-000000000000",
	})

	assert.True(t, result.IsError, "session_end on non-existent session must return error")

	var resp map[string]any
	unmarshalResult(t, result, &resp)
	assert.Equal(t, "policy/session-not-found", resp["code"])
}

// ── TestSessionEnd_TooLongSummary ─────────────────────────────────────────────

func TestSessionEnd_TooLongSummary(t *testing.T) {
	CleanDB(t)

	sessionID := startSession(t, "", "")

	c := newMCPClient(t)
	longSummary := strings.Repeat("x", 4097)
	result := callTool(t, c, "session_end", map[string]any{
		"session_id": sessionID,
		"summary":    longSummary,
	})

	assert.True(t, result.IsError,
		"session_end with 4097-char summary must return error")
}

// ── TestSessionSummary_HappyPath ──────────────────────────────────────────────

func TestSessionSummary_HappyPath(t *testing.T) {
	requireRealEmbedder(t)
	CleanDB(t)

	sessionID := startSession(t, "foo", "/tmp/test")
	c := newMCPClient(t)

	// Create 3 nodes attached to the session.
	nodeNames := []string{"sess-node-a", "sess-node-b", "sess-node-c"}
	for _, name := range nodeNames {
		r := callTool(t, c, "create_nodes", map[string]any{
			"nodes": []map[string]any{
				{
					"name":         name,
					"nodeType":     "pattern",
					"observations": []string{"observation for " + name},
					"session_id":   sessionID,
				},
			},
		})
		require.False(t, r.IsError, "create_nodes for %s must succeed: %s", name, resultText(t, r))
	}

	// End the session.
	endSession(t, sessionID, "summary text")

	// Call session_summary.
	result := callTool(t, c, "session_summary", map[string]any{
		"session_id": sessionID,
	})
	assert.False(t, result.IsError, "session_summary must succeed: %s", resultText(t, result))

	var resp map[string]any
	unmarshalResult(t, result, &resp)

	assert.Equal(t, sessionID, resp["session_id"])
	assert.Equal(t, "foo", resp["project_id"])
	assert.Equal(t, "/tmp/test", resp["working_dir"])
	assert.Equal(t, "summary text", resp["summary"])
	assert.NotNil(t, resp["ended_at"], "ended_at must be set after session_end")

	nodes, ok := resp["nodes"].([]any)
	require.True(t, ok, "nodes must be an array")
	assert.Len(t, nodes, 3, "session_summary must return exactly 3 nodes")

	// Verify chronological order (a < b < c) and observation_count.
	for i, name := range nodeNames {
		n := nodes[i].(map[string]any)
		assert.Equal(t, name, n["name"],
			"node[%d] must be %q in chronological order", i, name)
		assert.Equal(t, float64(1), n["observation_count"],
			"node[%d] must have observation_count=1", i)
		assert.Nil(t, n["deleted_at"],
			"active node deleted_at must be null")
	}
}

// ── TestSessionSummary_IncludesSoftDeleted ────────────────────────────────────

func TestSessionSummary_IncludesSoftDeleted(t *testing.T) {
	requireRealEmbedder(t)
	CleanDB(t)

	sessionID := startSession(t, "", "")
	c := newMCPClient(t)

	// Create 3 nodes attached to the session.
	for i := 1; i <= 3; i++ {
		name := fmt.Sprintf("sd-node-%d", i)
		r := callTool(t, c, "create_nodes", map[string]any{
			"nodes": []map[string]any{
				{
					"name":         name,
					"nodeType":     "pattern",
					"observations": []string{"obs for " + name},
					"session_id":   sessionID,
				},
			},
		})
		require.False(t, r.IsError, "create_nodes for %s must succeed", name)
	}

	// Soft-delete the second node directly via SQL.
	pool := NewTestPool(t)
	_, err := pool.Exec(context.Background(),
		`UPDATE nodes SET deleted_at = now() WHERE name = 'sd-node-2' AND deleted_at IS NULL`,
	)
	require.NoError(t, err, "soft-delete sd-node-2 must succeed")

	// session_summary must still return all 3 nodes.
	result := callTool(t, c, "session_summary", map[string]any{
		"session_id": sessionID,
	})
	assert.False(t, result.IsError, "session_summary must succeed")

	var resp map[string]any
	unmarshalResult(t, result, &resp)
	nodes, ok := resp["nodes"].([]any)
	require.True(t, ok)
	assert.Len(t, nodes, 3,
		"session_summary must include soft-deleted nodes for full audit trail")

	// The second node must have a non-null deleted_at.
	node2 := nodes[1].(map[string]any)
	assert.Equal(t, "sd-node-2", node2["name"])
	assert.NotNil(t, node2["deleted_at"],
		"soft-deleted node must have non-null deleted_at in session_summary")
}

// ── TestSessionSummary_NotFound ───────────────────────────────────────────────

func TestSessionSummary_NotFound(t *testing.T) {
	CleanDB(t)
	c := newMCPClient(t)

	result := callTool(t, c, "session_summary", map[string]any{
		"session_id": "00000000-0000-0000-0000-000000000000",
	})

	assert.True(t, result.IsError, "session_summary on non-existent session must return error")

	var resp map[string]any
	unmarshalResult(t, result, &resp)
	assert.Equal(t, "policy/session-not-found", resp["code"])
}

// ── TestCreateNodesWithSession_HappyPath ──────────────────────────────────────

func TestCreateNodesWithSession_HappyPath(t *testing.T) {
	requireRealEmbedder(t)
	CleanDB(t)

	sessionID := startSession(t, "", "")
	c := newMCPClient(t)

	result := callTool(t, c, "create_nodes", map[string]any{
		"nodes": []map[string]any{
			{
				"name":         "session-node-test",
				"nodeType":     "pattern",
				"observations": []string{"some observation"},
				"session_id":   sessionID,
			},
		},
	})
	require.False(t, result.IsError, "create_nodes with session_id must succeed: %s", resultText(t, result))

	// Verify nodes.session_id is set in DB.
	sid := queryNodeSessionID(t, "session-node-test")
	require.NotNil(t, sid, "nodes.session_id must be set in DB")
	assert.Equal(t, sessionID, *sid)
}

// ── TestCreateNodesWithSession_NoSession ──────────────────────────────────────

func TestCreateNodesWithSession_NoSession(t *testing.T) {
	CleanDB(t)

	sessionID := startSession(t, "", "")

	// End the session.
	endSession(t, sessionID, "")

	// Attempt to create a node with the ended session_id.
	c := newMCPClient(t)
	result := callTool(t, c, "create_nodes", map[string]any{
		"nodes": []map[string]any{
			{
				"name":         "node-after-ended-session",
				"nodeType":     "pattern",
				"observations": []string{"should be rejected"},
				"session_id":   sessionID,
			},
		},
	})

	assert.True(t, result.IsError,
		"create_nodes with ended session_id must be rejected")

	var resp map[string]any
	unmarshalResult(t, result, &resp)
	assert.Equal(t, "policy/session-already-ended", resp["code"])
}

// ── TestCreateNodesWithSession_BadSessionID ───────────────────────────────────

func TestCreateNodesWithSession_BadSessionID(t *testing.T) {
	CleanDB(t)
	c := newMCPClient(t)

	result := callTool(t, c, "create_nodes", map[string]any{
		"nodes": []map[string]any{
			{
				"name":         "node-bad-session",
				"nodeType":     "pattern",
				"observations": []string{"some obs"},
				"session_id":   "not-a-uuid",
			},
		},
	})

	assert.True(t, result.IsError,
		"create_nodes with invalid session_id must return error")
}

// ── TestFullSequence ──────────────────────────────────────────────────────────

// TestFullSequence is the AC headline test: session_start → 5 create_nodes →
// session_end → session_summary returns the 5 nodes in chronological order.
func TestFullSequence(t *testing.T) {
	requireRealEmbedder(t)
	CleanDB(t)

	sessionID := startSession(t, "foo", "/home/user/project")
	c := newMCPClient(t)

	// Create 5 nodes in sequence, each attached to the session.
	names := []string{
		"full-seq-node-1",
		"full-seq-node-2",
		"full-seq-node-3",
		"full-seq-node-4",
		"full-seq-node-5",
	}
	for _, name := range names {
		r := callTool(t, c, "create_nodes", map[string]any{
			"nodes": []map[string]any{
				{
					"name":         name,
					"nodeType":     "pattern",
					"observations": []string{"observation for " + name},
					"session_id":   sessionID,
				},
			},
		})
		require.False(t, r.IsError, "create_nodes for %s must succeed: %s", name, resultText(t, r))
	}

	// End the session.
	endResp := endSession(t, sessionID, "full sequence complete")
	assert.Equal(t, float64(5), endResp["node_count"],
		"session_end node_count must be 5")

	// Retrieve summary.
	result := callTool(t, c, "session_summary", map[string]any{
		"session_id": sessionID,
	})
	require.False(t, result.IsError, "session_summary must succeed: %s", resultText(t, result))

	var resp map[string]any
	unmarshalResult(t, result, &resp)

	nodes, ok := resp["nodes"].([]any)
	require.True(t, ok, "nodes must be an array")
	require.Len(t, nodes, 5,
		"session_summary must return exactly 5 nodes (the headline AC)")

	// Verify chronological order.
	for i, name := range names {
		n := nodes[i].(map[string]any)
		assert.Equal(t, name, n["name"],
			"node[%d] must be %q in creation order", i, name)
	}

	// Verify ended_at and summary are present.
	assert.NotNil(t, resp["ended_at"])
	assert.Equal(t, "full sequence complete", resp["summary"])
}
