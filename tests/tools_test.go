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

	"github.com/mariogutierrez/context-harness-mcp/internal/embed"
	internalmcp "github.com/mariogutierrez/context-harness-mcp/internal/mcp"
)

// ── embedder guard ────────────────────────────────────────────────────────────

// requireEmbedder skips the test when the ONNX embedder is unavailable (e.g.
// CGO-disabled Windows dev boxes). Tests that exercise the write path must call
// this before any create_entities or add_observations invocation — batchEmbed
// now returns an error on model failure rather than degrading silently to NULL.
func requireEmbedder(t *testing.T) {
	t.Helper()
	if _, err := embed.Default().Encode(context.Background(), []string{"probe"}); err != nil {
		t.Skipf("embedder unavailable, skipping write-path test: %v", err)
	}
}

// ── test client helpers ────────────────────────────────────────────────────────

// newMCPClient builds an in-process MCP client wired to a fresh server backed
// by the suite-level pool. The Initialize handshake is performed so callers can
// invoke tools immediately.
func newMCPClient(t *testing.T) *client.Client {
	t.Helper()
	pool := NewTestPool(t)
	srv := internalmcp.New(pool)

	c, err := client.NewInProcessClient(srv)
	require.NoError(t, err, "create in-process client")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	require.NoError(t, c.Start(ctx), "start in-process client")

	req := mcp.InitializeRequest{}
	req.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	req.Params.ClientInfo = mcp.Implementation{Name: "test-client", Version: "0.0.1"}
	_, err = c.Initialize(ctx, req)
	require.NoError(t, err, "initialize MCP session")

	return c
}

// callTool invokes a named tool via the in-process MCP client. args is marshalled
// to JSON before dispatch. The Go-level error is always fatal — tool-level errors
// are surfaced through result.IsError instead.
func callTool(t *testing.T, c *client.Client, toolName string, args any) *mcp.CallToolResult {
	t.Helper()
	ctx := context.Background()

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

// resultText extracts the first TextContent string from a CallToolResult.
func resultText(t *testing.T, result *mcp.CallToolResult) string {
	t.Helper()
	require.NotEmpty(t, result.Content, "result must have content")
	tc, ok := result.Content[0].(mcp.TextContent)
	require.True(t, ok, "content[0] must be TextContent, got %T", result.Content[0])
	return tc.Text
}

// unmarshalResult unmarshals the first TextContent of a result into target.
func unmarshalResult(t *testing.T, result *mcp.CallToolResult, target any) {
	t.Helper()
	require.NoError(t, json.Unmarshal([]byte(resultText(t, result)), target))
}

// countActiveRows returns the count of rows where deleted_at IS NULL.
func countActiveRows(t *testing.T, table string) int {
	t.Helper()
	pool := NewTestPool(t)
	var n int
	err := pool.QueryRow(context.Background(),
		"SELECT COUNT(*) FROM "+table+" WHERE deleted_at IS NULL",
	).Scan(&n)
	require.NoError(t, err)
	return n
}

// countAllRows returns the total row count in table (including soft-deleted rows).
func countAllRows(t *testing.T, table string) int {
	t.Helper()
	pool := NewTestPool(t)
	var n int
	err := pool.QueryRow(context.Background(), "SELECT COUNT(*) FROM "+table).Scan(&n)
	require.NoError(t, err)
	return n
}

// ── AC-1: create_entities ─────────────────────────────────────────────────────

// TestCreateEntities_SingleEntity verifies that a clean DB insert of one entity
// with one observation produces exactly one entities row and one observations
// row (AC-1).
func TestCreateEntities_SingleEntity(t *testing.T) {
	requireEmbedder(t)
	CleanDB(t)
	c := newMCPClient(t)

	result := callTool(t, c, "create_entities", map[string]any{
		"entities": []map[string]any{
			{
				"name":         "test-entity-ac1",
				"entityType":   "pattern",
				"observations": []string{"first observation"},
			},
		},
	})

	assert.False(t, result.IsError, "expected success, got error: %s", resultText(t, result))

	var resp map[string]any
	unmarshalResult(t, result, &resp)
	assert.Equal(t, float64(1), resp["created_entities"])
	assert.Equal(t, float64(1), resp["created_observations"])

	// DB-level assertions (AC-1).
	assert.Equal(t, 1, countActiveRows(t, "entities"), "exactly one active entity row")
	assert.Equal(t, 1, countActiveRows(t, "observations"), "exactly one active observation row")
}

// ── AC-2: add_observations dedup ─────────────────────────────────────────────

// TestAddObservations_Dedup verifies that duplicate observations are deduped at
// the DB level via the (entity_id, text) unique constraint (AC-2).
func TestAddObservations_Dedup(t *testing.T) {
	requireEmbedder(t)
	CleanDB(t)
	c := newMCPClient(t)

	// Seed the entity.
	callTool(t, c, "create_entities", map[string]any{
		"entities": []map[string]any{
			{
				"name":         "dedup-entity",
				"entityType":   "pattern",
				"observations": []string{"original observation"},
			},
		},
	})

	// Add the same observation text that already exists.
	result := callTool(t, c, "add_observations", map[string]any{
		"observations": []map[string]any{
			{
				"entityName": "dedup-entity",
				"contents":   []string{"original observation", "original observation"},
			},
		},
	})

	assert.False(t, result.IsError, "expected success: %s", resultText(t, result))

	// The DB must still have exactly ONE active observation (AC-2).
	assert.Equal(t, 1, countActiveRows(t, "observations"),
		"duplicate observations must be deduped by unique constraint (AC-2)")
}

// ── AC-3: delete_entities soft-delete ────────────────────────────────────────

// TestDeleteEntities_SoftDelete verifies that soft-delete sets deleted_at and
// that read_graph excludes the deleted entity (AC-3).
func TestDeleteEntities_SoftDelete(t *testing.T) {
	requireEmbedder(t)
	CleanDB(t)
	c := newMCPClient(t)

	callTool(t, c, "create_entities", map[string]any{
		"entities": []map[string]any{
			{
				"name":         "entity-to-delete",
				"entityType":   "decision",
				"observations": []string{"some observation"},
			},
		},
	})

	require.Equal(t, 1, countActiveRows(t, "entities"), "entity must exist before delete")

	// Soft-delete the entity.
	result := callTool(t, c, "delete_entities", map[string]any{
		"entityNames": []string{"entity-to-delete"},
	})
	assert.False(t, result.IsError, "expected success: %s", resultText(t, result))

	var resp map[string]any
	unmarshalResult(t, result, &resp)
	assert.Equal(t, float64(1), resp["deleted"])

	// Physical row must still exist (soft delete does not hard-delete).
	assert.Equal(t, 1, countAllRows(t, "entities"),
		"physical row must remain after soft delete (AC-3)")

	// Active-row count must be zero.
	assert.Equal(t, 0, countActiveRows(t, "entities"),
		"active entity count must be 0 after soft delete (AC-3)")

	// read_graph must not include the deleted entity.
	graphResult := callTool(t, c, "read_graph", map[string]any{})
	assert.False(t, graphResult.IsError, "read_graph must succeed")

	var graph map[string]any
	unmarshalResult(t, graphResult, &graph)
	entities, _ := graph["entities"].([]any)
	assert.Empty(t, entities,
		"read_graph must not include soft-deleted entity (AC-3)")
}

// ── AC-4: validate.Run called before any pgx.Tx ──────────────────────────────

// TestValidation_CalledBeforeTx uses the row-count snapshot approach: a payload
// rejected by the Content Filter must leave all three tables unchanged, proving
// no Tx was committed (AC-4). The snapshot is equivalent to verifying "zero
// BEGIN statements committed" — if a transaction committed, rows would appear.
func TestValidation_CalledBeforeTx(t *testing.T) {
	CleanDB(t)
	c := newMCPClient(t)

	entitiesBefore := countAllRows(t, "entities")
	obsBefore := countAllRows(t, "observations")
	relBefore := countAllRows(t, "relations")

	// Invalid entity type triggers taxonomy rejection (Layer 3) before any Tx.
	result := callTool(t, c, "create_entities", map[string]any{
		"entities": []map[string]any{
			{
				"name":         "bad-type-entity",
				"entityType":   "invalid-not-in-enum",
				"observations": []string{"some valid observation text"},
			},
		},
	})

	assert.True(t, result.IsError, "rejected payload must produce IsError=true (AC-4)")

	var errPayload map[string]any
	unmarshalResult(t, result, &errPayload)

	code, _ := errPayload["code"].(string)
	assert.Contains(t, code, "policy/", "error code must be a policy code (AC-4)")

	// Snapshot check — no Tx was committed (AC-4).
	assert.Equal(t, entitiesBefore, countAllRows(t, "entities"),
		"entities table must be unchanged after policy rejection (AC-4)")
	assert.Equal(t, obsBefore, countAllRows(t, "observations"),
		"observations table must be unchanged after policy rejection (AC-4)")
	assert.Equal(t, relBefore, countAllRows(t, "relations"),
		"relations table must be unchanged after policy rejection (AC-4)")
}

// ── AC-5: policy/secret-detected end-to-end ──────────────────────────────────

// TestSecretDetected_E2E verifies that a payload containing a secret triggers
// the structured error with code="policy/secret-detected" AND leaves the DB
// unchanged (AC-5).
func TestSecretDetected_E2E(t *testing.T) {
	CleanDB(t)
	c := newMCPClient(t)

	entitiesBefore := countAllRows(t, "entities")
	obsBefore := countAllRows(t, "observations")
	relBefore := countAllRows(t, "relations")

	// RSA private key header — XOR+base64 encoded in source as in validator_test.go
	// to prevent GitHub push-protection from flagging this source file.
	rsaObs := decodeTestSecret("b29vb28ABwULDGIQEQNiEhALFAMWB2IJBxtvb29vb0gPCwsHLTULAAMDCQEDEwcDbGxsSG9vb29vBwwGYhARA2ISEAsUAxYHYgkHG29vb29v")

	result := callTool(t, c, "create_entities", map[string]any{
		"entities": []map[string]any{
			{
				"name":         "secret-entity",
				"entityType":   "pattern",
				"observations": []string{rsaObs},
			},
		},
	})

	assert.True(t, result.IsError, "secret payload must be rejected (AC-5)")

	var errPayload map[string]any
	unmarshalResult(t, result, &errPayload)

	assert.Equal(t, "policy/secret-detected", errPayload["code"],
		"error code must be policy/secret-detected (AC-5)")

	// DB must be unchanged — atomic rejection.
	assert.Equal(t, entitiesBefore, countAllRows(t, "entities"),
		"no entity row must be inserted (AC-5)")
	assert.Equal(t, obsBefore, countAllRows(t, "observations"),
		"no observation row must be inserted (AC-5)")
	assert.Equal(t, relBefore, countAllRows(t, "relations"),
		"no relation row must be inserted (AC-5)")
}

// ── AC-6: multi-entity atomic reject-everything-or-nothing ───────────────────

// TestAtomicReject_MultiEntity verifies that when entity[2].observations[1]
// fails Layer 2 (secrets), the error carries rejected_observation_index=1 AND
// none of the 5 entities appear in the DB (AC-6).
func TestAtomicReject_MultiEntity(t *testing.T) {
	CleanDB(t)
	c := newMCPClient(t)

	entitiesBefore := countAllRows(t, "entities")

	// AWS access key ID — XOR+base64 encoded as in validator_test.go.
	// Decoded value is synthetic: AKIA + 16 sequential uppercase chars.
	awsKey := decodeTestSecret("AwkLA3NwcXZ3dHV6e3IDAAEGBwQ=")

	result := callTool(t, c, "create_entities", map[string]any{
		"entities": []map[string]any{
			{
				"name":         "entity-zero",
				"entityType":   "pattern",
				"observations": []string{"clean observation 0"},
			},
			{
				"name":         "entity-one",
				"entityType":   "decision",
				"observations": []string{"clean observation 1"},
			},
			{
				// entity index 2 — observation index 1 carries the AWS key.
				"name":       "entity-two",
				"entityType": "service",
				"observations": []string{
					"clean first obs",
					"Found credential " + awsKey + " in config", // index 1
				},
			},
			{
				"name":         "entity-three",
				"entityType":   "error",
				"observations": []string{"clean observation 3"},
			},
			{
				"name":         "entity-four",
				"entityType":   "constraint",
				"observations": []string{"clean observation 4"},
			},
		},
	})

	assert.True(t, result.IsError,
		"payload with secret in entity[2].obs[1] must be rejected (AC-6)")

	var errPayload map[string]any
	unmarshalResult(t, result, &errPayload)

	assert.Equal(t, "policy/secret-detected", errPayload["code"],
		"error code must be policy/secret-detected (AC-6)")

	// rejected_observation_index must be 1 (zero-indexed within entity[2].observations).
	rejObs, ok := errPayload["rejected_observation_index"].(float64)
	require.True(t, ok,
		"rejected_observation_index must be a number, got %T: %v",
		errPayload["rejected_observation_index"], errPayload["rejected_observation_index"])
	assert.Equal(t, float64(1), rejObs,
		"rejected_observation_index must be 1 (second obs of the offending entity) (AC-6)")

	// rejected_entity_index must be 2 (entity #3, zero-indexed).
	rejEnt, ok := errPayload["rejected_entity_index"].(float64)
	require.True(t, ok,
		"rejected_entity_index must be a number, got %T", errPayload["rejected_entity_index"])
	assert.Equal(t, float64(2), rejEnt,
		"rejected_entity_index must be 2 (third entity, zero-indexed) (AC-6)")

	// None of the 5 entities must appear in the DB — atomic reject-everything-or-nothing.
	assert.Equal(t, entitiesBefore, countAllRows(t, "entities"),
		"none of the 5 entities must appear in the DB after atomic rejection (AC-6)")
}
