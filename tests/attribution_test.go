// Package tests — attribution integration tests for PR-5 (feat/auth-attribution).
//
// Covers:
//   - AC-1: create_nodes with an authenticated ctx → nodes row has correct
//     created_by_user_id + created_by_email.
//   - AC-2: create_nodes with no auth ctx (stdio/MCP_AUTH=none path) → nodes row has
//     NULL created_by_user_id AND NULL created_by_email.
//   - AC-3: add_observations and create_relations rows mirror the same attribution
//     behavior as nodes under both auth and no-auth contexts.
//   - AC-4: verified via grep — see sql_parameterized_grep in the status block.
//
// Design note: the mcp-go in-process transport threads the ctx passed to
// client.CallTool all the way into the server-side tool handler
// (transport.InProcessTransport.SendRequest → server.WithContext(ctx, session) →
// server.HandleMessage(ctx, ...)). This lets the attribution tests inject
// auth.WithUserID / auth.WithEmail values without an HTTP server or JWT.
//
// FK constraint: created_by_user_id references users.supabase_user_id. The tests
// pre-insert a synthetic users row before creating attributable rows, then clean
// up via t.Cleanup.
package tests

import (
	"context"
	"encoding/json"
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
	// attrUserSub is a synthetic Supabase UUID used across attribution tests.
	// Must be a valid UUID because created_by_user_id is typed uuid in Postgres.
	attrUserSub   = "aaaabbbb-cccc-dddd-eeee-000011112222"
	attrUserEmail = "attr-test@example.com"
)

// ── helpers ───────────────────────────────────────────────────────────────────

// insertAttrUser pre-inserts the synthetic users row that satisfies the
// created_by_user_id FK. Registered as a t.Cleanup so the row is removed after
// each test, preventing FK-violation cross-test pollution.
func insertAttrUser(t *testing.T) {
	t.Helper()
	pool := NewTestPool(t)
	ctx := context.Background()

	_, err := pool.Exec(ctx,
		`INSERT INTO users (supabase_user_id, email)
		 VALUES ($1, $2)
		 ON CONFLICT (supabase_user_id) DO NOTHING`,
		attrUserSub, attrUserEmail,
	)
	require.NoError(t, err, "insertAttrUser: must pre-insert synthetic users row")

	t.Cleanup(func() {
		pool.Exec(ctx, "DELETE FROM users WHERE supabase_user_id = $1", attrUserSub) //nolint:errcheck
	})
}

// newMCPClientForAttribution returns an in-process MCP client backed by a
// fresh server using the suite pool. Identical to newMCPClient but named
// separately to make attribution tests self-documenting.
func newMCPClientForAttribution(t *testing.T) *client.Client {
	t.Helper()
	pool := NewTestPool(t)
	srv := internalmcp.New(pool, nil)

	c, err := client.NewInProcessClient(srv)
	require.NoError(t, err, "create in-process client for attribution test")

	startCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	require.NoError(t, c.Start(startCtx), "start in-process client")

	initReq := mcp.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcp.Implementation{Name: "attr-test-client", Version: "0.0.1"}
	_, err = c.Initialize(startCtx, initReq)
	require.NoError(t, err, "initialize MCP session for attribution test")

	return c
}

// callToolWithCtx invokes a named MCP tool with the given context, allowing
// auth values to be threaded through to the server-side handler.
func callToolWithCtx(t *testing.T, c *client.Client, ctx context.Context, toolName string, args any) *mcp.CallToolResult {
	t.Helper()

	raw, err := json.Marshal(args)
	require.NoError(t, err, "marshal args for %s", toolName)

	req := mcp.CallToolRequest{}
	req.Params.Name = toolName
	req.Params.Arguments = json.RawMessage(raw)

	result, err := c.CallTool(ctx, req)
	require.NoError(t, err, "%s: CallTool must not return a protocol-level error", toolName)
	require.NotNil(t, result)
	return result
}

// queryNodeAttribution fetches (created_by_user_id, created_by_email) for the
// active node with the given name. Returns (*string, *string) so NULL is
// representable as nil.
func queryNodeAttribution(t *testing.T, name string) (userID *string, email *string) {
	t.Helper()
	pool := NewTestPool(t)
	err := pool.QueryRow(context.Background(),
		`SELECT created_by_user_id::text, created_by_email
		   FROM nodes
		  WHERE name = $1 AND deleted_at IS NULL`,
		name,
	).Scan(&userID, &email)
	require.NoError(t, err, "queryNodeAttribution: node %q must exist", name)
	return userID, email
}

// queryObservationAttribution fetches (created_by_user_id, created_by_email) for
// the active observation matching the given text on the named node.
func queryObservationAttribution(t *testing.T, nodeName, obsText string) (userID *string, email *string) {
	t.Helper()
	pool := NewTestPool(t)
	err := pool.QueryRow(context.Background(),
		`SELECT o.created_by_user_id::text, o.created_by_email
		   FROM observations o
		   JOIN nodes n ON n.id = o.node_id
		  WHERE n.name = $1 AND o.text = $2
		    AND o.deleted_at IS NULL AND n.deleted_at IS NULL`,
		nodeName, obsText,
	).Scan(&userID, &email)
	require.NoError(t, err,
		"queryObservationAttribution: observation %q on node %q must exist", obsText, nodeName)
	return userID, email
}

// queryRelationAttribution fetches (created_by_user_id, created_by_email) for
// the active relation identified by (fromName, toName, relationType).
func queryRelationAttribution(t *testing.T, fromName, toName, relationType string) (userID *string, email *string) {
	t.Helper()
	pool := NewTestPool(t)
	err := pool.QueryRow(context.Background(),
		`SELECT r.created_by_user_id::text, r.created_by_email
		   FROM relations r
		   JOIN nodes nf ON nf.id = r.from_node_id AND nf.deleted_at IS NULL
		   JOIN nodes nt ON nt.id = r.to_node_id   AND nt.deleted_at IS NULL
		  WHERE nf.name = $1 AND nt.name = $2 AND r.relation_type = $3
		    AND r.deleted_at IS NULL`,
		fromName, toName, relationType,
	).Scan(&userID, &email)
	require.NoError(t, err,
		"queryRelationAttribution: relation %s→%s (%s) must exist", fromName, toName, relationType)
	return userID, email
}

// ── AC-1: create_nodes with authenticated ctx ─────────────────────────────────

// TestAttribution_WithCtxUser verifies that when create_nodes is called with a
// ctx carrying user attribution (auth.WithUserID + auth.WithEmail), the persisted
// nodes and observations rows have the correct created_by_user_id and
// created_by_email values.
//
// AC-1: Given create_nodes called with auth ctx (sub=X, email=Y) → nodes row has
// created_by_user_id = X AND created_by_email = Y.
func TestAttribution_WithCtxUser(t *testing.T) {
	requireEmbedder(t)
	CleanDB(t)
	insertAttrUser(t)

	c := newMCPClientForAttribution(t)

	// Build a ctx carrying the synthetic user's sub + email.
	authCtx := auth.WithUserID(context.Background(), attrUserSub)
	authCtx = auth.WithEmail(authCtx, attrUserEmail)

	result := callToolWithCtx(t, c, authCtx, "create_nodes", map[string]any{
		"nodes": []map[string]any{
			{
				"name":         "attr-node-with-user",
				"nodeType":     "pattern",
				"observations": []string{"attributed observation"},
			},
		},
	})

	require.False(t, result.IsError,
		"create_nodes with auth ctx must succeed; error: %s", resultText(t, result))

	// Assert: node row has correct attribution.
	gotUserID, gotEmail := queryNodeAttribution(t, "attr-node-with-user")

	require.NotNil(t, gotUserID,
		"created_by_user_id must NOT be NULL when user is in ctx (AC-1)")
	assert.Equal(t, attrUserSub, *gotUserID,
		"created_by_user_id must equal ctx user sub (AC-1)")

	require.NotNil(t, gotEmail,
		"created_by_email must NOT be NULL when user is in ctx (AC-1)")
	assert.Equal(t, attrUserEmail, *gotEmail,
		"created_by_email must equal ctx user email (AC-1)")

	// Assert: observation row carries the same attribution.
	obsUserID, obsEmail := queryObservationAttribution(t, "attr-node-with-user", "attributed observation")

	require.NotNil(t, obsUserID,
		"observation created_by_user_id must NOT be NULL when user is in ctx (AC-1)")
	assert.Equal(t, attrUserSub, *obsUserID,
		"observation created_by_user_id must equal ctx user sub (AC-1)")

	require.NotNil(t, obsEmail,
		"observation created_by_email must NOT be NULL when user is in ctx (AC-1)")
	assert.Equal(t, attrUserEmail, *obsEmail,
		"observation created_by_email must equal ctx user email (AC-1)")
}

// ── AC-2: create_nodes with no auth ctx (stdio / MCP_AUTH=none) ──────────────

// TestAttribution_NoCtxUser verifies that when create_nodes is called with an
// empty context (no user in ctx — the stdio path or MCP_AUTH=none HTTP path),
// the persisted rows have NULL created_by_user_id AND NULL created_by_email.
//
// AC-2: Given create_nodes called with no auth ctx → nodes row has
// created_by_user_id IS NULL AND created_by_email IS NULL.
func TestAttribution_NoCtxUser(t *testing.T) {
	requireEmbedder(t)
	CleanDB(t)

	c := newMCPClientForAttribution(t)

	// Use a plain context with no auth values — simulates stdio / MCP_AUTH=none.
	plainCtx := context.Background()

	result := callToolWithCtx(t, c, plainCtx, "create_nodes", map[string]any{
		"nodes": []map[string]any{
			{
				"name":         "attr-node-no-user",
				"nodeType":     "pattern",
				"observations": []string{"unauthenticated observation"},
			},
		},
	})

	require.False(t, result.IsError,
		"create_nodes without auth ctx must succeed; error: %s", resultText(t, result))

	// Assert: node row has NULL attribution.
	gotUserID, gotEmail := queryNodeAttribution(t, "attr-node-no-user")

	assert.Nil(t, gotUserID,
		"created_by_user_id must be NULL when no user is in ctx (AC-2)")
	assert.Nil(t, gotEmail,
		"created_by_email must be NULL when no user is in ctx (AC-2)")

	// Assert: observation row also has NULL attribution.
	obsUserID, obsEmail := queryObservationAttribution(t, "attr-node-no-user", "unauthenticated observation")

	assert.Nil(t, obsUserID,
		"observation created_by_user_id must be NULL when no user is in ctx (AC-2)")
	assert.Nil(t, obsEmail,
		"observation created_by_email must be NULL when no user is in ctx (AC-2)")
}

// ── AC-3: add_observations attribution ───────────────────────────────────────

// TestAttribution_Observations verifies that add_observations persists
// created_by_user_id and created_by_email correctly on observation rows for
// both auth and no-auth contexts.
//
// AC-3 (observations): Given add_observations with/without auth ctx,
// Then observations rows mirror the same attribution behavior as nodes.
func TestAttribution_Observations(t *testing.T) {
	requireEmbedder(t)
	CleanDB(t)
	insertAttrUser(t)

	c := newMCPClientForAttribution(t)

	// Seed a node without attribution (stdio path — plain ctx).
	seedResult := callToolWithCtx(t, c, context.Background(), "create_nodes", map[string]any{
		"nodes": []map[string]any{
			{
				"name":         "obs-attr-node",
				"nodeType":     "pattern",
				"observations": []string{"seed observation"},
			},
		},
	})
	require.False(t, seedResult.IsError,
		"seed create_nodes must succeed; error: %s", resultText(t, seedResult))

	t.Run("with_auth_ctx", func(t *testing.T) {
		authCtx := auth.WithUserID(context.Background(), attrUserSub)
		authCtx = auth.WithEmail(authCtx, attrUserEmail)

		result := callToolWithCtx(t, c, authCtx, "add_observations", map[string]any{
			"observations": []map[string]any{
				{
					"nodeName": "obs-attr-node",
					"contents": []string{"authenticated additional observation"},
				},
			},
		})
		require.False(t, result.IsError,
			"add_observations with auth ctx must succeed; error: %s", resultText(t, result))

		userID, email := queryObservationAttribution(t, "obs-attr-node", "authenticated additional observation")

		require.NotNil(t, userID,
			"add_observations: created_by_user_id must NOT be NULL when user is in ctx (AC-3)")
		assert.Equal(t, attrUserSub, *userID,
			"add_observations: created_by_user_id must equal ctx user sub (AC-3)")

		require.NotNil(t, email,
			"add_observations: created_by_email must NOT be NULL when user is in ctx (AC-3)")
		assert.Equal(t, attrUserEmail, *email,
			"add_observations: created_by_email must equal ctx user email (AC-3)")
	})

	t.Run("no_auth_ctx", func(t *testing.T) {
		result := callToolWithCtx(t, c, context.Background(), "add_observations", map[string]any{
			"observations": []map[string]any{
				{
					"nodeName": "obs-attr-node",
					"contents": []string{"unauthenticated additional observation"},
				},
			},
		})
		require.False(t, result.IsError,
			"add_observations without auth ctx must succeed; error: %s", resultText(t, result))

		userID, email := queryObservationAttribution(t, "obs-attr-node", "unauthenticated additional observation")

		assert.Nil(t, userID,
			"add_observations: created_by_user_id must be NULL when no user is in ctx (AC-3)")
		assert.Nil(t, email,
			"add_observations: created_by_email must be NULL when no user is in ctx (AC-3)")
	})
}

// ── AC-3: create_relations attribution ───────────────────────────────────────

// TestAttribution_Relations verifies that create_relations persists
// created_by_user_id and created_by_email correctly on relation rows for
// both auth and no-auth contexts.
//
// AC-3 (relations): Given create_relations with/without auth ctx,
// Then relations rows mirror the same attribution behavior as nodes.
func TestAttribution_Relations(t *testing.T) {
	requireEmbedder(t)
	CleanDB(t)
	insertAttrUser(t)

	c := newMCPClientForAttribution(t)

	// Seed a pair of nodes without attribution. create_relations requires the
	// nodes to already exist (it calls store.FindByName under the hood).
	for _, name := range []string{"rel-from-node", "rel-to-node"} {
		seedResult := callToolWithCtx(t, c, context.Background(), "create_nodes", map[string]any{
			"nodes": []map[string]any{
				{
					"name":         name,
					"nodeType":     "pattern",
					"observations": []string{"seed obs for " + name},
				},
			},
		})
		require.False(t, seedResult.IsError,
			"seed create_nodes for %s must succeed; error: %s", name, resultText(t, seedResult))
	}

	t.Run("with_auth_ctx", func(t *testing.T) {
		authCtx := auth.WithUserID(context.Background(), attrUserSub)
		authCtx = auth.WithEmail(authCtx, attrUserEmail)

		result := callToolWithCtx(t, c, authCtx, "create_relations", map[string]any{
			"relations": []map[string]any{
				{
					"from":         "rel-from-node",
					"to":           "rel-to-node",
					"relationType": "relates_to",
				},
			},
		})
		require.False(t, result.IsError,
			"create_relations with auth ctx must succeed; error: %s", resultText(t, result))

		userID, email := queryRelationAttribution(t, "rel-from-node", "rel-to-node", "relates_to")

		require.NotNil(t, userID,
			"create_relations: created_by_user_id must NOT be NULL when user is in ctx (AC-3)")
		assert.Equal(t, attrUserSub, *userID,
			"create_relations: created_by_user_id must equal ctx user sub (AC-3)")

		require.NotNil(t, email,
			"create_relations: created_by_email must NOT be NULL when user is in ctx (AC-3)")
		assert.Equal(t, attrUserEmail, *email,
			"create_relations: created_by_email must equal ctx user email (AC-3)")
	})

	t.Run("no_auth_ctx", func(t *testing.T) {
		// Use a different relation type so the unique constraint doesn't block the insert.
		result := callToolWithCtx(t, c, context.Background(), "create_relations", map[string]any{
			"relations": []map[string]any{
				{
					"from":         "rel-from-node",
					"to":           "rel-to-node",
					"relationType": "depends-on",
				},
			},
		})
		require.False(t, result.IsError,
			"create_relations without auth ctx must succeed; error: %s", resultText(t, result))

		userID, email := queryRelationAttribution(t, "rel-from-node", "rel-to-node", "depends-on")

		assert.Nil(t, userID,
			"create_relations: created_by_user_id must be NULL when no user is in ctx (AC-3)")
		assert.Nil(t, email,
			"create_relations: created_by_email must be NULL when no user is in ctx (AC-3)")
	})
}

// ── AC-4: schema columns exist ────────────────────────────────────────────────

// TestAttribution_SchemaColumns verifies that all three tables have the
// created_by_user_id and created_by_email columns added by migration 00005.
// This is a pre-condition for the attribution feature.
//
// AC-4 (SQL parameterized): fmt.Sprintf-free SQL is verified by grep at CI time
// (see status block). This test focuses on schema presence.
func TestAttribution_SchemaColumns(t *testing.T) {
	pool := NewTestPool(t)
	ctx := context.Background()

	tables := []string{"nodes", "observations", "relations"}
	columns := []string{"created_by_user_id", "created_by_email"}

	for _, table := range tables {
		for _, col := range columns {
			t.Run(table+"_has_"+col, func(t *testing.T) {
				assertColumnExists(t, ctx, pool, table, col)
			})
		}
	}
}
