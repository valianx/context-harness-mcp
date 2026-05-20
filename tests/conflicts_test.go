// Package tests — integration tests for the find_conflicts and mark_superseded
// MCP tools (PR-3: feat/mcp-conflicts).
//
// Covers:
//   - AC-1: find_conflicts returns semantically similar candidates above threshold.
//   - AC-2: find_conflicts scopes results to the target node's project only.
//   - AC-3: find_conflicts returns policy/node-not-found when target is absent.
//   - AC-4: mark_superseded happy path creates supersedes relation with correct DB state.
//   - AC-5: mark_superseded is idempotent — returns policy/relation-already-exists.
//   - AC-6: mark_superseded with archive_old_observations=true soft-deletes old obs.
//   - AC-7: mark_superseded rejects cross-project pairs with policy/cross-project-relation.
//   - AC-8: supersedes is descriptive-only — old node still searchable; edge visible in read_graph.
//   - AC-9: mark_superseded emits a structured slog JSON line with the 10 required keys.
//
// ONNX-gated tests: AC-1, AC-2, AC-6, AC-8 — call requireRealEmbedder(t).
// Non-ONNX tests:   AC-3, AC-4, AC-5, AC-7, AC-9.
//
// Design notes:
//   - All tests use the shared testPool; CleanDB clears state before each test.
//   - mark_superseded tests asserting DB attribution use insertConflictsUser /
//     conflictsAuthCtx to satisfy the created_by_user_id FK constraint.
//   - AC-9 slog capture replaces the default logger with a JSONHandler writing to
//     a bytes.Buffer, restoring the original logger in t.Cleanup.
package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	pgvector "github.com/pgvector/pgvector-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariogutierrez/context-harness-mcp/internal/auth"
	"github.com/mariogutierrez/context-harness-mcp/internal/embed"
	internalmcp "github.com/mariogutierrez/context-harness-mcp/internal/mcp"
	"github.com/mariogutierrez/context-harness-mcp/internal/store"
)

// ── constants ─────────────────────────────────────────────────────────────────

const (
	// conflictsUserSub is a synthetic Supabase UUID used across conflicts tests
	// that exercise the mark_superseded write path with attribution.
	conflictsUserSub   = "ccccdddd-eeee-ffff-0000-111122223333"
	conflictsUserEmail = "conflicts-test@example.com"
)

// ── helpers ───────────────────────────────────────────────────────────────────

// insertConflictsUser pre-inserts the synthetic users row required by the
// created_by_user_id FK. Cleanup is registered via t.Cleanup.
func insertConflictsUser(t *testing.T) {
	t.Helper()
	pool := NewTestPool(t)
	ctx := context.Background()

	_, err := pool.Exec(ctx,
		`INSERT INTO users (supabase_user_id, email)
		 VALUES ($1, $2)
		 ON CONFLICT (supabase_user_id) DO NOTHING`,
		conflictsUserSub, conflictsUserEmail,
	)
	require.NoError(t, err, "insertConflictsUser: must pre-insert synthetic users row")

	t.Cleanup(func() {
		pool.Exec(ctx, "DELETE FROM users WHERE supabase_user_id = $1", conflictsUserSub) //nolint:errcheck
	})
}

// conflictsAuthCtx returns a context carrying the conflicts test user's
// attribution values (sub + email).
func conflictsAuthCtx() context.Context {
	ctx := auth.WithUserID(context.Background(), conflictsUserSub)
	return auth.WithEmail(ctx, conflictsUserEmail)
}

// newMCPClientForConflicts returns an in-process MCP client backed by a fresh
// server using the suite pool. Named separately from newMCPClient for clarity.
func newMCPClientForConflicts(t *testing.T) *client.Client {
	t.Helper()
	pool := NewTestPool(t)
	srv := internalmcp.New(pool, nil)

	c, err := client.NewInProcessClient(srv)
	require.NoError(t, err, "create in-process client for conflicts test")

	startCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	require.NoError(t, c.Start(startCtx), "start in-process client for conflicts test")

	initReq := mcp.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcp.Implementation{Name: "conflicts-test-client", Version: "0.0.1"}
	_, err = c.Initialize(startCtx, initReq)
	require.NoError(t, err, "initialize MCP session for conflicts test")

	return c
}

// insertNodeInProject inserts a single active node in the given project directly
// via the store (bypasses the MCP write path, no embedder required).
// Returns the node UUID string.
func insertNodeInProject(t *testing.T, name, nodeType, projectID string) string {
	t.Helper()
	pool := NewTestPool(t)
	ctx := context.Background()

	tx, err := pool.Begin(ctx)
	require.NoError(t, err, "insertNodeInProject: begin tx")
	id, err := store.Create(ctx, tx, name, nodeType, projectID, nil, nil)
	require.NoError(t, err, "insertNodeInProject: store.Create %q in project %q", name, projectID)
	require.NoError(t, tx.Commit(ctx), "insertNodeInProject: commit")
	return id
}

// insertObsWithVec inserts an observation with a real embedding vector for the
// given node ID. The caller must have already called requireRealEmbedder(t).
func insertObsWithVec(t *testing.T, nodeID, text string, vec []float32) {
	t.Helper()
	pool := NewTestPool(t)
	ctx := context.Background()

	tx, err := pool.Begin(ctx)
	require.NoError(t, err, "insertObsWithVec: begin tx")
	_, _, err = store.Insert(ctx, tx, nodeID, text, pgvector.NewVector(vec), nil, nil)
	require.NoError(t, err, "insertObsWithVec: store.Insert %q", text)
	require.NoError(t, tx.Commit(ctx), "insertObsWithVec: commit")
}

// encodeText encodes a single text string using the default ONNX embedder and
// returns the float32 vector. The caller must have already called requireRealEmbedder(t).
func encodeText(t *testing.T, text string) []float32 {
	t.Helper()
	ctx := context.Background()
	vecs, err := embed.Default().Encode(ctx, []string{text})
	require.NoError(t, err, "encodeText: Encode %q", text)
	require.Len(t, vecs, 1, "encodeText: expected 1 vector")
	return vecs[0]
}

// countActiveRelations returns the count of active (non-soft-deleted) relations.
func countActiveRelations(t *testing.T) int {
	t.Helper()
	return countActiveRows(t, "relations")
}

// querySupersededRelation fetches the project_id, relation_type, and
// created_by_user_id for the active supersedes relation identified by
// (fromName → toName). Fails the test if the relation does not exist.
func querySupersededRelation(t *testing.T, fromName, toName string) (projectID, relType string, createdByUserID *string) {
	t.Helper()
	pool := NewTestPool(t)
	err := pool.QueryRow(context.Background(),
		`SELECT r.project_id, r.relation_type, r.created_by_user_id::text
		   FROM relations r
		   JOIN nodes nf ON nf.id = r.from_node_id AND nf.deleted_at IS NULL
		   JOIN nodes nt ON nt.id = r.to_node_id   AND nt.deleted_at IS NULL
		  WHERE nf.name = $1 AND nt.name = $2 AND r.relation_type = 'supersedes'
		    AND r.deleted_at IS NULL`,
		fromName, toName,
	).Scan(&projectID, &relType, &createdByUserID)
	require.NoError(t, err,
		"querySupersededRelation: supersedes(%s → %s) must exist in DB", fromName, toName)
	return projectID, relType, createdByUserID
}

// countActiveObservationsForNode returns the count of non-soft-deleted
// observations for the given node ID.
func countActiveObservationsForNode(t *testing.T, nodeID string) int {
	t.Helper()
	pool := NewTestPool(t)
	var n int
	err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM observations WHERE node_id = $1 AND deleted_at IS NULL`,
		nodeID,
	).Scan(&n)
	require.NoError(t, err, "countActiveObservationsForNode: query failed for node %s", nodeID)
	return n
}

// conflictsResultCode unmarshals the first TextContent of a result and returns
// the "code" field. Returns "" when the field is absent.
func conflictsResultCode(t *testing.T, result *mcp.CallToolResult) string {
	t.Helper()
	var payload map[string]any
	require.NoError(t, json.Unmarshal([]byte(resultText(t, result)), &payload),
		"conflictsResultCode: must unmarshal result JSON")
	code, _ := payload["code"].(string)
	return code
}

// splitLines splits a string into individual non-empty lines.
func splitLines(s string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			if i > start {
				lines = append(lines, s[start:i])
			}
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}

// mapKeys returns the keys of a map for use in diagnostic messages.
func mapKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// ── AC-1: find_conflicts — semantically similar candidates ───────────────────

// TestFindConflicts_SemanticallySimilar seeds two nodes in project "global" with
// observations about OAuth key rotation and token refresh strategy (the exact
// fixture pair from the task spec, calibrated for >0.85 with MiniLM-L6-v2),
// then asserts find_conflicts surfaces the other node with similarity >= 0.85.
//
// AC-1: find_conflicts returns at least 1 candidate with similarity >= 0.85 and
// non-empty matching_observations_pair.own_obs / other_obs.
func TestFindConflicts_SemanticallySimilar(t *testing.T) {
	requireRealEmbedder(t)
	CleanDB(t)

	// Seed node A with a real embedding.
	nodeA := insertNodeInProject(t, "rotacion-claves-oauth", "pattern", "global")
	obsA := "Rotación de claves OAuth cada 90 días vía Vault"
	insertObsWithVec(t, nodeA, obsA, encodeText(t, obsA))

	// Seed node B with a semantically similar observation.
	nodeB := insertNodeInProject(t, "token-refresh-strategy", "pattern", "global")
	_ = nodeB
	obsB := "Token refresh strategy: 90-day rotation via Vault, MiniLM embed"
	insertObsWithVec(t, nodeB, obsB, encodeText(t, obsB))

	c := newMCPClientForConflicts(t)

	result := callToolWithCtx(t, c, context.Background(), "find_conflicts", map[string]any{
		"nodeName":       "rotacion-claves-oauth",
		"top_k":          5,
		"min_similarity": 0.85,
	})

	require.False(t, result.IsError,
		"AC-1: find_conflicts must succeed; got: %s", resultText(t, result))

	var resp struct {
		Candidates []struct {
			Name       string  `json:"name"`
			NodeType   string  `json:"node_type"`
			Similarity float64 `json:"similarity"`
			MatchingObservationPair struct {
				OwnObs   string `json:"own_obs"`
				OtherObs string `json:"other_obs"`
			} `json:"matching_observations_pair"`
		} `json:"candidates"`
	}
	unmarshalResult(t, result, &resp)

	require.NotEmpty(t, resp.Candidates,
		"AC-1: find_conflicts must return at least 1 candidate for semantically similar nodes")

	top := resp.Candidates[0]
	assert.Equal(t, "token-refresh-strategy", top.Name,
		"AC-1: top candidate must be token-refresh-strategy")
	assert.GreaterOrEqual(t, top.Similarity, 0.85,
		"AC-1: candidate similarity must be >= 0.85, got %f", top.Similarity)
	assert.NotEmpty(t, top.MatchingObservationPair.OwnObs,
		"AC-1: matching_observations_pair.own_obs must be non-empty")
	assert.NotEmpty(t, top.MatchingObservationPair.OtherObs,
		"AC-1: matching_observations_pair.other_obs must be non-empty")
}

// ── AC-2: find_conflicts — same-project isolation ────────────────────────────

// TestFindConflicts_SameProjectIsolation seeds a node with a unique observation
// in project "foo" and a semantically similar node in project "bar". Invokes
// find_conflicts for the "foo" node and asserts zero candidates come from "bar".
//
// AC-2: find_conflicts scopes the search to the target node's own project only.
func TestFindConflicts_SameProjectIsolation(t *testing.T) {
	requireRealEmbedder(t)
	CleanDB(t)

	const obsText = "Rotación de claves OAuth cada 90 días vía Vault"
	const similarText = "Token refresh strategy: 90-day rotation via Vault"

	// Node A in project "foo".
	nodeA := insertNodeInProject(t, "rotacion-claves-oauth", "pattern", "foo")
	insertObsWithVec(t, nodeA, obsText, encodeText(t, obsText))

	// Node B in project "bar" with a semantically similar observation.
	nodeB := insertNodeInProject(t, "token-refresh-strategy", "pattern", "bar")
	insertObsWithVec(t, nodeB, similarText, encodeText(t, similarText))

	c := newMCPClientForConflicts(t)

	result := callToolWithCtx(t, c, context.Background(), "find_conflicts", map[string]any{
		"nodeName": "rotacion-claves-oauth",
		"project":  "foo",
		"top_k":    10,
	})

	require.False(t, result.IsError,
		"AC-2: find_conflicts must succeed; got: %s", resultText(t, result))

	var resp struct {
		Candidates []struct {
			Name string `json:"name"`
		} `json:"candidates"`
	}
	unmarshalResult(t, result, &resp)

	for _, candidate := range resp.Candidates {
		assert.NotEqual(t, "token-refresh-strategy", candidate.Name,
			"AC-2: find_conflicts must not return the node from project 'bar'")
	}
}

// ── AC-3: find_conflicts — node not found ────────────────────────────────────

// TestFindConflicts_NodeNotFound invokes find_conflicts with a non-existent node
// name and asserts IsError=true with code "policy/node-not-found". The error
// fires before any embedding is attempted.
//
// AC-3: find_conflicts returns policy/node-not-found when the target is absent.
func TestFindConflicts_NodeNotFound(t *testing.T) {
	CleanDB(t)

	c := newMCPClientForConflicts(t)

	result := callToolWithCtx(t, c, context.Background(), "find_conflicts", map[string]any{
		"nodeName": "INEXISTE",
	})

	require.True(t, result.IsError,
		"AC-3: find_conflicts for unknown node must return IsError=true")

	code := conflictsResultCode(t, result)
	assert.Equal(t, "policy/node-not-found", code,
		"AC-3: error code must be policy/node-not-found, got %q", code)
}

// ── AC-4: mark_superseded — happy path ───────────────────────────────────────

// TestMarkSuperseded_HappyPath seeds nodes "ms-old" and "ms-new" in project "foo",
// invokes mark_superseded with an auth ctx, and verifies:
//   - response: {relation_created: true, observations_archived: 0}
//   - DB: supersedes relation with from_node="ms-new", to_node="ms-old",
//     project_id="foo", created_by_user_id populated from ctx.
//
// AC-4: mark_superseded creates the supersedes relation with the correct wire and DB state.
func TestMarkSuperseded_HappyPath(t *testing.T) {
	CleanDB(t)
	insertConflictsUser(t)

	insertNodeInProject(t, "ms-old", "pattern", "foo")
	insertNodeInProject(t, "ms-new", "pattern", "foo")

	c := newMCPClientForConflicts(t)

	result := callToolWithCtx(t, c, conflictsAuthCtx(), "mark_superseded", map[string]any{
		"old":    "ms-old",
		"new":    "ms-new",
		"reason": "refactored",
	})

	require.False(t, result.IsError,
		"AC-4: mark_superseded must succeed; got: %s", resultText(t, result))

	var resp map[string]any
	unmarshalResult(t, result, &resp)
	assert.Equal(t, true, resp["relation_created"],
		"AC-4: relation_created must be true")
	assert.Equal(t, float64(0), resp["observations_archived"],
		"AC-4: observations_archived must be 0 when archive_old_observations is false")

	// DB assertions: the supersedes relation must run from ms-new → ms-old.
	projectID, relType, createdByUserID := querySupersededRelation(t, "ms-new", "ms-old")
	assert.Equal(t, "foo", projectID,
		"AC-4: relation project_id must be 'foo'")
	assert.Equal(t, "supersedes", relType,
		"AC-4: relation_type must be 'supersedes'")
	require.NotNil(t, createdByUserID,
		"AC-4: created_by_user_id must NOT be NULL when auth ctx is set")
	assert.Equal(t, conflictsUserSub, *createdByUserID,
		"AC-4: created_by_user_id must equal ctx user sub")
}

// ── AC-5: mark_superseded — idempotent ───────────────────────────────────────

// TestMarkSuperseded_Idempotent re-invokes mark_superseded with the same node
// pair and asserts the second call returns policy/relation-already-exists.
//
// AC-5: mark_superseded returns policy/relation-already-exists on a duplicate call.
func TestMarkSuperseded_Idempotent(t *testing.T) {
	CleanDB(t)
	insertConflictsUser(t)

	insertNodeInProject(t, "idem-old", "pattern", "foo")
	insertNodeInProject(t, "idem-new", "pattern", "foo")

	c := newMCPClientForConflicts(t)

	// First call — must succeed.
	first := callToolWithCtx(t, c, conflictsAuthCtx(), "mark_superseded", map[string]any{
		"old": "idem-old",
		"new": "idem-new",
	})
	require.False(t, first.IsError,
		"AC-5: first mark_superseded must succeed; got: %s", resultText(t, first))

	// Second call with the same pair — must return idempotency error.
	second := callToolWithCtx(t, c, conflictsAuthCtx(), "mark_superseded", map[string]any{
		"old": "idem-old",
		"new": "idem-new",
	})
	require.True(t, second.IsError,
		"AC-5: second mark_superseded must return IsError=true (idempotency check)")

	code := conflictsResultCode(t, second)
	assert.Equal(t, "policy/relation-already-exists", code,
		"AC-5: error code must be policy/relation-already-exists, got %q", code)
}

// ── AC-6: mark_superseded — archives observations ─────────────────────────────

// TestMarkSuperseded_ArchivesObservations seeds "arch-old" with 3 active
// observations (real embeddings), invokes mark_superseded with
// archive_old_observations=true, and asserts all 3 are soft-deleted.
//
// AC-6: mark_superseded with archive_old_observations=true soft-deletes all
// active observations of the old node and reports the correct count.
func TestMarkSuperseded_ArchivesObservations(t *testing.T) {
	requireRealEmbedder(t)
	CleanDB(t)
	insertConflictsUser(t)

	nodeOld := insertNodeInProject(t, "arch-old", "pattern", "foo")
	insertNodeInProject(t, "arch-new", "pattern", "foo")

	// Insert 3 active observations with real embeddings.
	obsTexts := []string{
		"First observation about archival strategy",
		"Second observation about legacy protocol",
		"Third observation about deprecation plan",
	}
	for _, obs := range obsTexts {
		insertObsWithVec(t, nodeOld, obs, encodeText(t, obs))
	}

	require.Equal(t, 3, countActiveObservationsForNode(t, nodeOld),
		"AC-6: pre-condition: arch-old must have 3 active observations before mark_superseded")

	c := newMCPClientForConflicts(t)

	result := callToolWithCtx(t, c, conflictsAuthCtx(), "mark_superseded", map[string]any{
		"old":                      "arch-old",
		"new":                      "arch-new",
		"archive_old_observations": true,
	})

	require.False(t, result.IsError,
		"AC-6: mark_superseded with archive must succeed; got: %s", resultText(t, result))

	var resp map[string]any
	unmarshalResult(t, result, &resp)
	assert.Equal(t, true, resp["relation_created"],
		"AC-6: relation_created must be true")
	assert.Equal(t, float64(3), resp["observations_archived"],
		"AC-6: observations_archived must be 3")

	// DB: all 3 observations must now be soft-deleted (deleted_at IS NOT NULL).
	remaining := countActiveObservationsForNode(t, nodeOld)
	assert.Equal(t, 0, remaining,
		"AC-6: all 3 observations of arch-old must be soft-deleted after archive_old_observations=true")
}

// ── AC-7: mark_superseded — cross-project rejected ───────────────────────────

// TestMarkSuperseded_CrossProjectRejected seeds "xp-old" in project "foo" and
// "xp-new" in project "bar", then asserts the handler rejects the call with
// policy/cross-project-relation and inserts no relation row.
//
// AC-7: mark_superseded rejects cross-project pairs at the handler level.
func TestMarkSuperseded_CrossProjectRejected(t *testing.T) {
	CleanDB(t)

	insertNodeInProject(t, "xp-old", "pattern", "foo")
	insertNodeInProject(t, "xp-new", "pattern", "bar")

	relsBefore := countActiveRelations(t)

	c := newMCPClientForConflicts(t)

	result := callToolWithCtx(t, c, context.Background(), "mark_superseded", map[string]any{
		"old": "xp-old",
		"new": "xp-new",
	})

	require.True(t, result.IsError,
		"AC-7: cross-project mark_superseded must return IsError=true")

	code := conflictsResultCode(t, result)
	assert.Equal(t, "policy/cross-project-relation", code,
		"AC-7: error code must be policy/cross-project-relation, got %q", code)

	// DB unchanged: no relation was inserted.
	relsAfter := countActiveRelations(t)
	assert.Equal(t, relsBefore, relsAfter,
		"AC-7: no relation must be inserted after cross-project rejection")
}

// ── AC-8: supersedes is descriptive-only ─────────────────────────────────────

// TestSupersedes_IsDescriptiveOnly seeds two nodes in project "foo", marks one
// as superseded by the other, then:
//  1. Calls search_nodes for the old node's text — asserts it still appears.
//  2. Calls read_graph for project "foo" — asserts the supersedes edge is present
//     with from_node="desc-new" and to_node="desc-old".
//
// AC-8: supersedes is descriptive-only — old node remains searchable; the
// edge appears in read_graph.relations.
func TestSupersedes_IsDescriptiveOnly(t *testing.T) {
	requireRealEmbedder(t)
	CleanDB(t)

	const uniqueText = "Distributed tracing with Jaeger and OpenTelemetry spans"

	nodeOld := insertNodeInProject(t, "desc-old", "pattern", "foo")
	insertNodeInProject(t, "desc-new", "pattern", "foo")

	// Insert a uniquely-worded observation so search_nodes can surface it.
	insertObsWithVec(t, nodeOld, uniqueText, encodeText(t, uniqueText))

	c := newMCPClientForConflicts(t)

	// Mark desc-new as superseding desc-old (direction: new → old).
	msResult := callToolWithCtx(t, c, context.Background(), "mark_superseded", map[string]any{
		"old": "desc-old",
		"new": "desc-new",
	})
	require.False(t, msResult.IsError,
		"AC-8: mark_superseded must succeed; got: %s", resultText(t, msResult))

	// 1. search_nodes must still return desc-old (supersedes is descriptive-only).
	searchResult := callToolWithCtx(t, c, context.Background(), "search_nodes", map[string]any{
		"query":   uniqueText,
		"project": "foo",
	})
	require.False(t, searchResult.IsError,
		"AC-8: search_nodes must succeed; got: %s", resultText(t, searchResult))

	assert.Contains(t, resultText(t, searchResult), "desc-old",
		"AC-8: desc-old must still appear in search_nodes — supersedes is descriptive-only")

	// 2. read_graph must include the supersedes edge.
	graphResult := callToolWithCtx(t, c, context.Background(), "read_graph", map[string]any{
		"project": "foo",
	})
	require.False(t, graphResult.IsError,
		"AC-8: read_graph must succeed; got: %s", resultText(t, graphResult))

	var graph struct {
		Relations []struct {
			From         string `json:"from"`
			To           string `json:"to"`
			RelationType string `json:"relationType"`
		} `json:"relations"`
	}
	unmarshalResult(t, graphResult, &graph)

	foundEdge := false
	for _, rel := range graph.Relations {
		if rel.RelationType == "supersedes" && rel.From == "desc-new" && rel.To == "desc-old" {
			foundEdge = true
			break
		}
	}
	assert.True(t, foundEdge,
		"AC-8: read_graph must include supersedes(from=desc-new, to=desc-old); got relations: %+v",
		graph.Relations)
}

// ── AC-9: mark_superseded — structured log assertion ─────────────────────────

// TestMarkSuperseded_StructuredLog captures the slog JSON output produced by
// mark_superseded via slog.SetDefault and asserts the presence of all 10
// required log keys: old, new, old_node_id, new_node_id, project_id, reason,
// archive_observations, observations_archived, user_id, email.
//
// AC-9: mark_superseded emits exactly the slog.Info("mark_superseded", ...) call
// with the 10 documented keys — reason is logged but NOT persisted in DB.
func TestMarkSuperseded_StructuredLog(t *testing.T) {
	CleanDB(t)
	insertConflictsUser(t)

	insertNodeInProject(t, "log-old", "pattern", "foo")
	insertNodeInProject(t, "log-new", "pattern", "foo")

	// Capture slog output: replace the default logger with a JSON handler writing
	// to a buffer. Restore the original logger when the test exits.
	var buf bytes.Buffer
	captureHandler := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})
	captureLogger := slog.New(captureHandler)
	originalLogger := slog.Default()
	slog.SetDefault(captureLogger)
	t.Cleanup(func() {
		slog.SetDefault(originalLogger)
	})

	c := newMCPClientForConflicts(t)

	result := callToolWithCtx(t, c, conflictsAuthCtx(), "mark_superseded", map[string]any{
		"old":    "log-old",
		"new":    "log-new",
		"reason": "test-structured-log-reason",
	})
	require.False(t, result.IsError,
		"AC-9: mark_superseded must succeed to produce a log line; got: %s", resultText(t, result))

	// Scan the captured output for the mark_superseded JSON log line.
	rawLog := buf.String()
	require.NotEmpty(t, rawLog,
		"AC-9: slog buffer must not be empty after mark_superseded call")

	var logEntry map[string]any
	found := false
	for _, line := range splitLines(rawLog) {
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}
		if msg, _ := entry["msg"].(string); msg == "mark_superseded" {
			logEntry = entry
			found = true
			break
		}
	}
	require.True(t, found,
		"AC-9: must find a slog JSON line with msg='mark_superseded' in captured output:\n%s", rawLog)

	// Assert all 10 required keys are present.
	requiredKeys := []string{
		"old", "new", "old_node_id", "new_node_id",
		"project_id", "reason", "archive_observations",
		"observations_archived", "user_id", "email",
	}
	for _, key := range requiredKeys {
		_, ok := logEntry[key]
		assert.True(t, ok,
			"AC-9: log entry must contain key %q; got keys: %v", key, mapKeys(logEntry))
	}
}
