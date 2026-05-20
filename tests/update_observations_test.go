// Package tests — update_observations integration tests for PR-4 (feat/tool-update-observations).
//
// Covers:
//   - AC-1: update_observations with valid input soft-deletes old observation and
//     inserts new one with embedding + attribution.
//   - AC-2: After AC-1, search_nodes and read_graph no longer surface old text.
//   - AC-3: Transaction rollback when any update targets a non-existent node.
//   - AC-4: Transaction rollback when old_text does not match any active observation.
//   - AC-5: Identical old_text/new_text rejected before any Tx is opened.
//   - AC-6: new_text containing a secret is rejected by the Content Filter (policy/secret-detected)
//     before any Tx is opened.
//
// Design note: tests that exercise the write path (embedding required) call
// requireRealEmbedder(t) first so they skip cleanly when the ONNX runtime is
// unavailable (CGO-disabled environments, Windows dev boxes, etc.).
//
// FK constraint: created_by_user_id references users.supabase_user_id. Where
// attribution is asserted, insertAttrUser(t) pre-inserts the synthetic users row
// (same helper as attribution_test.go). Cleanup is registered via t.Cleanup.
package tests

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariogutierrez/context-harness-mcp/internal/auth"
	internalmcp "github.com/mariogutierrez/context-harness-mcp/internal/mcp"
)

// ── constants ─────────────────────────────────────────────────────────────────

const (
	// updUserSub is a synthetic Supabase UUID used across update_observations tests.
	updUserSub   = "bbbbcccc-dddd-eeee-ffff-111122223333"
	updUserEmail = "upd-test@example.com"
)

// ── helpers ───────────────────────────────────────────────────────────────────

// insertUpdUser pre-inserts the synthetic users row required to satisfy the
// created_by_user_id FK. Registered as a t.Cleanup so the row is removed after
// each test, preventing cross-test FK violations.
func insertUpdUser(t *testing.T) {
	t.Helper()
	pool := NewTestPool(t)
	ctx := context.Background()

	_, err := pool.Exec(ctx,
		`INSERT INTO users (supabase_user_id, email)
		 VALUES ($1, $2)
		 ON CONFLICT (supabase_user_id) DO NOTHING`,
		updUserSub, updUserEmail,
	)
	require.NoError(t, err, "insertUpdUser: must pre-insert synthetic users row")

	t.Cleanup(func() {
		pool.Exec(ctx, "DELETE FROM users WHERE supabase_user_id = $1", updUserSub) //nolint:errcheck
	})
}

// newMCPClientForUpdate returns an in-process MCP client backed by a fresh
// server using the suite pool. Named separately from newMCPClient for clarity.
func newMCPClientForUpdate(t *testing.T) *client.Client {
	t.Helper()
	pool := NewTestPool(t)
	srv := internalmcp.New(pool, nil)

	c, err := client.NewInProcessClient(srv)
	require.NoError(t, err, "create in-process client for update_observations test")

	startCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	require.NoError(t, c.Start(startCtx), "start in-process client")

	initReq := mcp.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcp.Implementation{Name: "upd-test-client", Version: "0.0.1"}
	_, err = c.Initialize(startCtx, initReq)
	require.NoError(t, err, "initialize MCP session for update_observations test")

	return c
}

// seedNodeWithObs creates a node with a single observation via the MCP tool and
// returns the client that was used. The node is seeded with a plain context
// (no auth) so attribution columns remain NULL on the seeded rows — auth is
// injected only on the update call we're testing.
func seedNodeWithObs(t *testing.T, c *client.Client, nodeName, obsText string) {
	t.Helper()
	result := callToolWithCtx(t, c, context.Background(), "create_nodes", map[string]any{
		"nodes": []map[string]any{
			{
				"name":         nodeName,
				"nodeType":     "pattern",
				"observations": []string{obsText},
			},
		},
	})
	require.False(t, result.IsError,
		"seed create_nodes must succeed; got: %s", resultText(t, result))
}

// queryObsDeletedAt returns the deleted_at value (as *time.Time, nil when NULL)
// for the observation matching (nodeName, obsText). Finds both active and
// soft-deleted rows so tests can assert deleted_at != NULL after an update.
func queryObsDeletedAt(t *testing.T, nodeName, obsText string) *time.Time {
	t.Helper()
	pool := NewTestPool(t)
	var deletedAt *time.Time
	err := pool.QueryRow(context.Background(),
		`SELECT o.deleted_at
		   FROM observations o
		   JOIN nodes n ON n.id = o.node_id
		  WHERE n.name = $1 AND o.text = $2
		    AND n.deleted_at IS NULL`,
		nodeName, obsText,
	).Scan(&deletedAt)
	require.NoError(t, err,
		"queryObsDeletedAt: observation %q on node %q must exist (including soft-deleted)", obsText, nodeName)
	return deletedAt
}

// queryObsEmbeddingNull returns true when the embedding column is NULL for the
// active observation matching (nodeName, obsText).
func queryObsEmbeddingNull(t *testing.T, nodeName, obsText string) bool {
	t.Helper()
	pool := NewTestPool(t)
	var isNull bool
	err := pool.QueryRow(context.Background(),
		`SELECT (o.embedding IS NULL)
		   FROM observations o
		   JOIN nodes n ON n.id = o.node_id
		  WHERE n.name = $1 AND o.text = $2
		    AND o.deleted_at IS NULL AND n.deleted_at IS NULL`,
		nodeName, obsText,
	).Scan(&isNull)
	require.NoError(t, err,
		"queryObsEmbeddingNull: active observation %q on node %q must exist", obsText, nodeName)
	return isNull
}

// countObsRows returns the total rows (including soft-deleted) in the
// observations table. Used for before/after snapshot comparisons.
func countObsRows(t *testing.T) int {
	t.Helper()
	return countAllRows(t, "observations")
}

// resultPolicyCode unmarshals the first TextContent of a result and returns the
// "code" field. Returns an empty string when the field is absent.
func resultPolicyCode(t *testing.T, result *mcp.CallToolResult) string {
	t.Helper()
	var payload map[string]any
	require.NoError(t, json.Unmarshal([]byte(resultText(t, result)), &payload),
		"resultPolicyCode: must unmarshal result JSON")
	code, _ := payload["code"].(string)
	return code
}

// ── AC-1: happy path ─────────────────────────────────────────────────────────

// TestUpdateObservations_HappyPath verifies that a valid update_observations call:
//   - returns {"updated": 1}
//   - soft-deletes the old observation (deleted_at != NULL)
//   - inserts the new observation with a non-NULL embedding
//   - persists attribution (created_by_user_id, created_by_email) from the ctx
//
// AC-1: Given node N with active observation O1="text-A", When update_observations
// is invoked with auth ctx, Then response={"updated":1}, O1.deleted_at!=NULL,
// O2="text-B" active with embedding and correct attribution.
func TestUpdateObservations_HappyPath(t *testing.T) {
	requireRealEmbedder(t)
	CleanDB(t)
	insertUpdUser(t)

	c := newMCPClientForUpdate(t)
	const nodeName = "upd-happy-node"
	const oldText = "text-A"
	const newText = "text-B"

	seedNodeWithObs(t, c, nodeName, oldText)

	authCtx := auth.WithUserID(context.Background(), updUserSub)
	authCtx = auth.WithEmail(authCtx, updUserEmail)

	result := callToolWithCtx(t, c, authCtx, "update_observations", map[string]any{
		"updates": []map[string]any{
			{
				"nodeName": nodeName,
				"old_text": oldText,
				"new_text": newText,
			},
		},
	})

	require.False(t, result.IsError,
		"update_observations happy-path must succeed; got: %s", resultText(t, result))

	// Assert response shape: {"updated": 1}.
	var resp map[string]any
	unmarshalResult(t, result, &resp)
	assert.Equal(t, float64(1), resp["updated"],
		"response must carry updated=1 (AC-1)")

	// Assert DB: O1 must now be soft-deleted.
	deletedAt := queryObsDeletedAt(t, nodeName, oldText)
	require.NotNil(t, deletedAt,
		"old observation O1 must have deleted_at != NULL after update (AC-1)")

	// Assert DB: O2 must be active with a non-NULL embedding.
	embNull := queryObsEmbeddingNull(t, nodeName, newText)
	assert.False(t, embNull,
		"new observation O2 must have a non-NULL embedding vector(384) (AC-1)")

	// Assert DB: O2 must carry correct attribution.
	userID, email := queryObservationAttribution(t, nodeName, newText)
	require.NotNil(t, userID,
		"new observation created_by_user_id must NOT be NULL when auth ctx is set (AC-1)")
	assert.Equal(t, updUserSub, *userID,
		"new observation created_by_user_id must equal ctx user sub (AC-1)")
	require.NotNil(t, email,
		"new observation created_by_email must NOT be NULL when auth ctx is set (AC-1)")
	assert.Equal(t, updUserEmail, *email,
		"new observation created_by_email must equal ctx user email (AC-1)")
}

// ── AC-2: soft-deleted observation hidden from search and read_graph ──────────

// TestUpdateObservations_HidesFromSearchAndReadGraph verifies that after an
// update, the replaced observation text no longer appears in search_nodes or
// read_graph responses.
//
// AC-2: Given the AC-1 scenario, When search_nodes(query="text-A") and
// read_graph(), Then O1 text does NOT appear in either response.
func TestUpdateObservations_HidesFromSearchAndReadGraph(t *testing.T) {
	requireRealEmbedder(t)
	CleanDB(t)
	insertUpdUser(t)

	c := newMCPClientForUpdate(t)
	const nodeName = "upd-hidden-node"
	const oldText = "text-A-hidden"
	const newText = "text-B-visible"

	seedNodeWithObs(t, c, nodeName, oldText)

	authCtx := auth.WithUserID(context.Background(), updUserSub)
	authCtx = auth.WithEmail(authCtx, updUserEmail)

	// Perform the update.
	upd := callToolWithCtx(t, c, authCtx, "update_observations", map[string]any{
		"updates": []map[string]any{
			{
				"nodeName": nodeName,
				"old_text": oldText,
				"new_text": newText,
			},
		},
	})
	require.False(t, upd.IsError,
		"update must succeed before visibility checks; got: %s", resultText(t, upd))

	// Assert: search_nodes does not return old text.
	searchResult := callTool(t, c, "search_nodes", map[string]any{
		"query": oldText,
	})
	require.False(t, searchResult.IsError,
		"search_nodes must not error; got: %s", resultText(t, searchResult))

	searchText := resultText(t, searchResult)
	assert.NotContains(t, searchText, oldText,
		"search_nodes must not return old observation text after soft-delete (AC-2)")

	// Assert: read_graph does not include old text in the node's observations.
	graphResult := callTool(t, c, "read_graph", map[string]any{})
	require.False(t, graphResult.IsError,
		"read_graph must not error; got: %s", resultText(t, graphResult))

	graphText := resultText(t, graphResult)
	assert.NotContains(t, graphText, oldText,
		"read_graph must not include soft-deleted observation text (AC-2)")
}

// ── AC-3: transaction rollback on missing node ─────────────────────────────────

// TestUpdateObservations_TxRollbackOnMissingNode verifies that when one update
// targets a non-existent node, the entire Tx rolls back: O1 stays active and no
// new observation is inserted.
//
// AC-3: Given node N with O1, When updates contain [NODE-NO-EXISTE, N], Then
// IsError=true with "node not found", DB unchanged.
func TestUpdateObservations_TxRollbackOnMissingNode(t *testing.T) {
	requireRealEmbedder(t)
	CleanDB(t)

	c := newMCPClientForUpdate(t)
	const nodeName = "upd-rollback-node"
	const obsText = "rollback-obs-text"

	seedNodeWithObs(t, c, nodeName, obsText)

	obsBefore := countObsRows(t)

	result := callToolWithCtx(t, c, context.Background(), "update_observations", map[string]any{
		"updates": []map[string]any{
			{
				"nodeName": "NODE-NO-EXISTE",
				"old_text": "x",
				"new_text": "y",
			},
			{
				"nodeName": nodeName,
				"old_text": obsText,
				"new_text": "text-new",
			},
		},
	})

	// Assert IsError=true with correct message.
	require.True(t, result.IsError,
		"update with missing node must return IsError=true (AC-3)")

	errText := resultText(t, result)
	assert.Contains(t, errText, "node not found",
		"error must mention 'node not found' (AC-3)")
	assert.Contains(t, errText, "NODE-NO-EXISTE",
		"error must identify the missing node name (AC-3)")

	// Assert DB unchanged: O1 still active, no new observation inserted.
	deletedAt := queryObsDeletedAt(t, nodeName, obsText)
	assert.Nil(t, deletedAt,
		"O1 must still be active after rollback — deleted_at must be NULL (AC-3)")

	obsAfter := countObsRows(t)
	assert.Equal(t, obsBefore, obsAfter,
		"total observation row count must be unchanged after rollback (AC-3)")
}

// ── AC-4: transaction rollback on missing observation ──────────────────────────

// TestUpdateObservations_TxRollbackOnMissingObservation verifies that when
// old_text does not match any active observation, the call fails with
// "observation not found" and the DB stays unchanged.
//
// AC-4: Given node N with O1, When old_text="INEXISTENTE", Then
// IsError=true with "observation not found", DB unchanged.
func TestUpdateObservations_TxRollbackOnMissingObservation(t *testing.T) {
	requireRealEmbedder(t)
	CleanDB(t)

	c := newMCPClientForUpdate(t)
	const nodeName = "upd-missing-obs-node"
	const obsText = "real-observation-text"

	seedNodeWithObs(t, c, nodeName, obsText)

	obsBefore := countObsRows(t)

	result := callToolWithCtx(t, c, context.Background(), "update_observations", map[string]any{
		"updates": []map[string]any{
			{
				"nodeName": nodeName,
				"old_text": "INEXISTENTE",
				"new_text": "text-B",
			},
		},
	})

	require.True(t, result.IsError,
		"update with non-matching old_text must return IsError=true (AC-4)")

	errText := resultText(t, result)
	assert.Contains(t, errText, "observation not found",
		"error must mention 'observation not found' (AC-4)")

	// Assert DB unchanged.
	deletedAt := queryObsDeletedAt(t, nodeName, obsText)
	assert.Nil(t, deletedAt,
		"O1 must remain active — deleted_at must be NULL after failed update (AC-4)")

	obsAfter := countObsRows(t)
	assert.Equal(t, obsBefore, obsAfter,
		"total observation row count must be unchanged after failure (AC-4)")
}

// ── AC-5: identical old_text/new_text rejected before any Tx ──────────────────

// TestUpdateObservations_IdenticalTextRejected verifies that when old_text and
// new_text are equal, the call is rejected with "new_text identical to old_text"
// before any transaction is opened (row counts unchanged).
//
// AC-5: When old_text == new_text == "same", Then IsError=true with correct
// message and row counts before == after.
func TestUpdateObservations_IdenticalTextRejected(t *testing.T) {
	CleanDB(t)

	c := newMCPClientForUpdate(t)

	nodesBefore := countAllRows(t, "nodes")
	obsBefore := countObsRows(t)

	result := callToolWithCtx(t, c, context.Background(), "update_observations", map[string]any{
		"updates": []map[string]any{
			{
				"nodeName": "any-node",
				"old_text": "same",
				"new_text": "same",
			},
		},
	})

	require.True(t, result.IsError,
		"identical old/new text must return IsError=true (AC-5)")

	errText := resultText(t, result)
	assert.Contains(t, errText, "new_text identical to old_text",
		"error must carry the guard message (AC-5)")

	// Verify no Tx was opened: row counts must be unchanged.
	assert.Equal(t, nodesBefore, countAllRows(t, "nodes"),
		"nodes row count must be unchanged — no Tx was opened (AC-5)")
	assert.Equal(t, obsBefore, countObsRows(t),
		"observations row count must be unchanged — no Tx was opened (AC-5)")
}

// ── AC-6: policy/secret-detected rejects before any Tx ────────────────────────

// TestUpdateObservations_PolicySecretDetected verifies that a new_text containing
// a known AWS access-key pattern is rejected by the Content Filter with
// policy/secret-detected BEFORE any transaction is opened.
//
// AC-6: Given node N with O1, When new_text contains an AWS access key,
// Then IsError=true, code="policy/secret-detected", DB unchanged.
//
// Design note: seeding via create_nodes requires the embedder. However, the
// Content Filter runs before batchEmbed and before any node lookup — so the
// policy error fires regardless of whether a real node exists. This test does
// NOT call requireRealEmbedder(t) so it executes in all environments. The DB
// unchanged assertion uses the total row count which is zero in a clean DB.
func TestUpdateObservations_PolicySecretDetected(t *testing.T) {
	CleanDB(t)

	c := newMCPClientForUpdate(t)

	obsBefore := countObsRows(t)

	// AKIAIOSFODNN7EXAMPLE is AWS's own well-known canary key — safe to use in
	// test source because it is publicly documented and not a real credential.
	// The content filter detects it via the aws-access-key-id inline pattern.
	awsCanary := "AKIAIOSFODNN7EXAMPLE"
	newTextWithSecret := "Found credential " + awsCanary + " in config file"

	result := callToolWithCtx(t, c, context.Background(), "update_observations", map[string]any{
		"updates": []map[string]any{
			{
				"nodeName": "any-node",
				"old_text": "original text",
				"new_text": newTextWithSecret,
			},
		},
	})

	require.True(t, result.IsError,
		"new_text with AWS key must return IsError=true (AC-6)")

	// Assert policy error code via the JSON payload.
	code := resultPolicyCode(t, result)
	assert.Equal(t, "policy/secret-detected", code,
		"error code must be policy/secret-detected (AC-6)")

	// Assert DB unchanged: Content Filter fired before any Tx was opened.
	obsAfter := countObsRows(t)
	assert.Equal(t, obsBefore, obsAfter,
		"observation row count must be unchanged after policy rejection (AC-6)")

	// Verify the secret guard fires even when old_text != new_text (the
	// identical-text guard is a separate early-exit that must not have fired).
	assert.False(t, strings.Contains(resultText(t, result), "identical"),
		"rejection must come from the policy layer, not the identical-text guard (AC-6)")
}
