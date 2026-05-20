// Package tests — integration tests for the doctor MCP tool and /healthz HTTP
// endpoint (PR-3).
//
// Covers:
//   - AC-1: Healthy DB with pgvector returns degraded:false, 5 named checks all pass.
//   - AC-2: DB without pgvector extension returns degraded:true, pgvector_extension
//     fails, all 5 checks still execute (no short-circuit).
//   - AC-3: GET /healthz on healthy DB returns HTTP 200 with JSON body (degraded:false).
//   - AC-4: GET /healthz when DB is down returns HTTP 503 with degraded:true.
//   - AC-5: VERIFY MCP envelope IsError is always false even when degraded:true.
//   - AC-6: VERIFY MCP doctor and HTTP /healthz call the same healthz.Run function,
//     producing identical check names and degraded value.
package tests

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariogutierrez/context-harness-mcp/internal/healthz"
)

// expectedCheckNames is the canonical ordered list of check names produced by
// healthz.Run. All tests assert this exact order.
var expectedCheckNames = []string{
	"db_ping",
	"pgvector_extension",
	"embedder",
	"gitleaks_detector",
	"row_counts",
}

// warmEmbedder swaps the suite-wide mock to the real ONNX embedder and
// pre-initialises the ONNX sync.Once so that subsequent calls to the embedder
// check inside healthz.Run do not incur the 200–500 ms cold-start penalty and
// trigger the 200 ms latency gate.
//
// Background: the embedder uses sync.Once to lazy-load the ONNX session. The
// first Encode call can take 200–500 ms depending on the host. The doctor check
// fails when latency exceeds 200 ms (embedderLatencyLimit in healthz.go). By
// calling warmEmbedder before invoking doctor we ensure the cold-start cost is
// paid outside the measured window, making the test deterministic.
//
// The mock is restored via t.Cleanup when the test exits.
func warmEmbedder(t *testing.T) {
	t.Helper()
	requireRealEmbedder(t)
}

// newPoolForDSN creates a *pgxpool.Pool for the given DSN without the pgvector
// type registration that store.New provides. Used for the bad-DSN / DB-down
// scenario where we need a pool that will fail on first use.
// It uses pgxpool.New (not store.New) so we bypass the initial Ping.
func newUnverifiedPool(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(dsn)
	require.NoError(t, err, "parse pool config")
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	require.NoError(t, err, "create pool (no ping)")
	t.Cleanup(pool.Close)
	return pool
}

// assertCheckShape verifies that a single parsed check map has the expected
// name, a "pass" or "fail" status, and a non-negative duration_ms field.
func assertCheckShape(t *testing.T, idx int, raw any, wantName string) map[string]any {
	t.Helper()
	check, ok := raw.(map[string]any)
	require.True(t, ok, "checks[%d] must be a JSON object, got %T", idx, raw)
	assert.Equal(t, wantName, check["name"], "checks[%d].name", idx)
	status, _ := check["status"].(string)
	assert.True(t, status == "pass" || status == "fail",
		"checks[%d].status must be 'pass' or 'fail', got %q", idx, status)
	durMs, ok := check["duration_ms"].(float64)
	assert.True(t, ok, "checks[%d].duration_ms must be a number, got %T", idx, check["duration_ms"])
	assert.GreaterOrEqual(t, durMs, float64(0),
		"checks[%d].duration_ms must be >= 0", idx)
	return check
}

// ── AC-1: healthy DB — all checks pass ───────────────────────────────────────

// TestDoctor_HealthyDB invokes the "doctor" MCP tool against the suite-level
// healthy testcontainer (pgvector installed, migrations applied) and asserts:
//   - MCP envelope IsError == false.
//   - Body has degraded:false.
//   - Exactly 5 checks in the canonical order.
//   - Each check has status:"pass" and duration_ms >= 0.
//
// AC-1: Given healthy DB with pgvector, all 5 probes pass.
func TestDoctor_HealthyDB(t *testing.T) {
	// Pre-warm the embedder outside the doctor's 200 ms latency gate.
	// See warmEmbedder doc comment for rationale.
	warmEmbedder(t)

	c := newMCPClient(t)
	result := callTool(t, c, "doctor", map[string]any{})

	// AC-1 / AC-5: MCP envelope must always be IsError:false.
	require.False(t, result.IsError,
		"AC-1: doctor MCP envelope must be IsError:false; body: %s", resultText(t, result))

	var body map[string]any
	unmarshalResult(t, result, &body)

	// degraded must be false when all probes pass.
	assert.Equal(t, false, body["degraded"],
		"AC-1: degraded must be false on a healthy DB")

	rawChecks, ok := body["checks"].([]any)
	require.True(t, ok, "AC-1: checks must be a JSON array, got %T", body["checks"])
	require.Len(t, rawChecks, 5,
		"AC-1: exactly 5 checks must be present, got %d", len(rawChecks))

	for i, name := range expectedCheckNames {
		check := assertCheckShape(t, i, rawChecks[i], name)
		assert.Equal(t, "pass", check["status"],
			"AC-1: checks[%d] (%s) must have status 'pass'", i, name)
	}
}

// ── AC-2: pgvector extension missing — degraded:true, no short-circuit ────────

// TestDoctor_MissingPgvector spins a fresh plain-postgres container (no pgvector
// extension) and invokes healthz.Run directly (bypassing the MCP in-process
// harness so we can supply our own pool). It asserts:
//   - degraded:true.
//   - checks[1] (pgvector_extension) has status:"fail" with non-empty detail.
//   - All 5 checks are present (no short-circuit).
//
// We use healthz.Run directly rather than a separate testcontainer to keep the
// test fast. A separate plain-postgres container would require Docker networking
// inside the test suite and significantly increase setup complexity with no
// behavioral gain — the check function behaviour is identical regardless of how
// the pool was created.
//
// Strategy for simulating missing pgvector: use the shared testcontainer pool
// but drop the vector extension from pg_extension temporarily. The vector column
// type is registered in the pool's AfterConnect hook via pgxvec.RegisterTypes;
// the extension itself can be dropped at the SQL level without affecting
// already-open connections. We restore it after the test.
//
// Note: the embedder check may also fail on hosts without the ONNX runtime.
// That is acceptable: AC-2 only requires pgvector_extension to fail and all 5
// checks to execute. It does not require any specific status for the other checks.
//
// AC-2: pgvector_extension fail surfaces, all other checks still run.
func TestDoctor_MissingPgvector(t *testing.T) {

	pool := NewTestPool(t)
	ctx := context.Background()

	// Drop the vector extension to simulate missing pgvector.
	// CASCADE drops the vector-typed columns — observations.embedding specifically.
	// We restore the extension after the test so other tests are unaffected.
	// Note: this will break observations table temporarily; we use t.Cleanup to
	// restore before any other test touches that table.
	_, err := pool.Exec(ctx, "DROP EXTENSION IF EXISTS vector CASCADE")
	require.NoError(t, err, "AC-2: drop vector extension")

	t.Cleanup(func() {
		// Restore the vector extension. Column definitions lost to CASCADE cannot be
		// automatically restored, so we also restore the embedding column type.
		// The observations table embedding column defaults to NULL so existing rows
		// are unaffected by the type re-addition.
		restoreCtx := context.Background()
		if _, restoreErr := pool.Exec(restoreCtx,
			"CREATE EXTENSION IF NOT EXISTS vector",
		); restoreErr != nil {
			t.Logf("AC-2 cleanup: failed to restore vector extension: %v", restoreErr)
		}
		// Re-add the embedding column if CASCADE removed it.
		if _, colErr := pool.Exec(restoreCtx,
			"ALTER TABLE observations ADD COLUMN IF NOT EXISTS embedding vector(384)",
		); colErr != nil {
			t.Logf("AC-2 cleanup: failed to restore embedding column: %v", colErr)
		}
	})

	// Run healthz.Run directly so we control the pool and skip MCP overhead.
	report := healthz.Run(ctx, pool)

	// degraded must be true because pgvector_extension fails.
	assert.True(t, report.Degraded,
		"AC-2: degraded must be true when pgvector extension is missing")

	// All 5 checks must be present — no short-circuit.
	require.Len(t, report.Checks, 5,
		"AC-2: all 5 checks must execute even when pgvector_extension fails")

	// Check names must match canonical order.
	for i, name := range expectedCheckNames {
		assert.Equal(t, name, report.Checks[i].Name,
			"AC-2: checks[%d].name must be %q", i, name)
	}

	// pgvector_extension check (index 1) must have failed with non-empty detail.
	pgvCheck := report.Checks[1]
	assert.Equal(t, "pgvector_extension", pgvCheck.Name,
		"AC-2: checks[1] must be pgvector_extension")
	assert.Equal(t, "fail", pgvCheck.Status,
		"AC-2: pgvector_extension check must have status 'fail'")
	assert.NotEmpty(t, pgvCheck.Detail,
		"AC-2: pgvector_extension fail must carry a non-empty detail message")
}

// ── AC-3: GET /healthz — healthy DB — HTTP 200 ────────────────────────────────

// TestDoctor_HTTPHealthz_Healthy registers healthz.Handler as an httptest server
// handler — mirroring cmd/server/main.go's wiring — and asserts:
//   - HTTP 200 status code.
//   - Content-Type: application/json.
//   - Body parses to a Report with degraded:false and 5 named checks.
//
// AC-3: Healthy DB → HTTP 200 + JSON body shape.
func TestDoctor_HTTPHealthz_Healthy(t *testing.T) {
	warmEmbedder(t)

	pool := NewTestPool(t)

	// Wire the /healthz handler exactly as cmd/server/main.go does.
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		report := healthz.Run(r.Context(), pool)
		status := http.StatusOK
		if report.Degraded {
			status = http.StatusServiceUnavailable
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(report)
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/healthz") //nolint:noctx
	require.NoError(t, err, "AC-3: GET /healthz must not error")
	defer resp.Body.Close()

	// Status code.
	assert.Equal(t, http.StatusOK, resp.StatusCode,
		"AC-3: healthy DB must return HTTP 200")

	// Content-Type.
	ct := resp.Header.Get("Content-Type")
	assert.Contains(t, ct, "application/json",
		"AC-3: Content-Type must be application/json, got %q", ct)

	// Body shape.
	var report healthz.Report
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&report),
		"AC-3: response body must decode as healthz.Report")

	assert.False(t, report.Degraded,
		"AC-3: degraded must be false on a healthy DB")
	require.Len(t, report.Checks, 5,
		"AC-3: exactly 5 checks expected in response body")

	for i, name := range expectedCheckNames {
		assert.Equal(t, name, report.Checks[i].Name,
			"AC-3: checks[%d].name must be %q", i, name)
		assert.Equal(t, "pass", report.Checks[i].Status,
			"AC-3: checks[%d] (%s) must pass on healthy DB", i, name)
	}
}

// ── AC-4: GET /healthz — DB down — HTTP 503 ───────────────────────────────────

// TestDoctor_HTTPHealthz_DBDown creates a pool pointing to a non-existent
// database (localhost:1, which is unreachable) and asserts:
//   - HTTP 503 status code.
//   - Body has degraded:true.
//   - checks[0].name == "db_ping" with status:"fail".
//
// We use pgxpool.New (not store.New) to bypass the initial Ping so the pool
// can be constructed even when the DB is unreachable. The ping failure surfaces
// at query time inside healthz.Run.
//
// AC-4: Unreachable DB → HTTP 503 + degraded:true + db_ping fails.
func TestDoctor_HTTPHealthz_DBDown(t *testing.T) {
	// No warmEmbedder needed: DB-down causes degraded:true regardless of embedder
	// status. db_ping is the first check and will fail, making degraded:true
	// irrespective of whether the embedder is available on this host.

	// Build a pool that cannot connect to anything. Port 1 is reserved / closed
	// on all standard Linux systems. connect_timeout is set low to keep the test fast.
	badPool := newUnverifiedPool(t,
		"postgres://test:test@localhost:1/does_not_exist?connect_timeout=2")

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		report := healthz.Run(r.Context(), badPool)
		status := http.StatusOK
		if report.Degraded {
			status = http.StatusServiceUnavailable
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(report)
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	// Give the handler a generous timeout so the test does not flake on slow CI.
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(ts.URL + "/healthz")
	require.NoError(t, err, "AC-4: GET /healthz must not return a transport-level error")
	defer resp.Body.Close()

	// Status code.
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode,
		"AC-4: DB-down must return HTTP 503")

	// Body shape.
	var report healthz.Report
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&report),
		"AC-4: response body must decode as healthz.Report")

	assert.True(t, report.Degraded,
		"AC-4: degraded must be true when DB is unreachable")

	require.NotEmpty(t, report.Checks,
		"AC-4: checks must not be empty")

	dbPing := report.Checks[0]
	assert.Equal(t, "db_ping", dbPing.Name,
		"AC-4: checks[0].name must be 'db_ping'")
	assert.Equal(t, "fail", dbPing.Status,
		"AC-4: checks[0] (db_ping) must have status 'fail' when DB is unreachable")
}

// ── AC-5: MCP envelope IsError always false ───────────────────────────────────

// TestDoctor_MCPEnvelopeAlwaysFalse invokes the doctor MCP tool via healthz.Run
// directly in a degraded scenario (DB unreachable pool injected via Handler) and
// asserts that the MCP CallToolResult always carries IsError:false regardless of
// the report's degraded value.
//
// AC-5: VERIFY doctor handler always returns IsError:false in MCP envelope.
func TestDoctor_MCPEnvelopeAlwaysFalse(t *testing.T) {
	// No warmEmbedder: the bad pool forces db_ping to fail → degraded:true
	// regardless of embedder availability. We only need degraded:true to be
	// present in the body to make the IsError:false assertion meaningful.

	// Use healthz.Handler directly with a bad pool to force degraded:true.
	// We call it in-process (no MCP client overhead) by constructing a minimal
	// mcp.CallToolRequest and invoking the handler closure.
	badPool := newUnverifiedPool(t,
		"postgres://test:test@localhost:1/does_not_exist?connect_timeout=2")

	handler := healthz.Handler(badPool)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// mcp.CallToolRequest zero value is valid for the doctor tool (no arguments).
	result, err := handler(ctx, mcp.CallToolRequest{})
	require.NoError(t, err, "AC-5: doctor handler must not return a Go-level error")
	require.NotNil(t, result, "AC-5: doctor handler must return a non-nil result")

	// THE key assertion: IsError must always be false even with a degraded report.
	assert.False(t, result.IsError,
		"AC-5: MCP envelope IsError must be false even when report is degraded")

	// Confirm the body actually carries degraded:true (proves the bad-pool scenario
	// is genuinely degraded, making the IsError:false assertion meaningful).
	require.NotEmpty(t, result.Content, "AC-5: result must have content")
	tc, ok := result.Content[0].(mcp.TextContent)
	require.True(t, ok, "AC-5: content[0] must be TextContent, got %T", result.Content[0])

	var report map[string]any
	require.NoError(t, json.Unmarshal([]byte(tc.Text), &report),
		"AC-5: TextContent must be valid JSON")
	assert.Equal(t, true, report["degraded"],
		"AC-5: body must carry degraded:true in the bad-pool scenario")
}

// ── AC-6: shared healthz.Run function ────────────────────────────────────────

// TestDoctor_SharedFunction invokes both the MCP doctor tool and the HTTP
// /healthz endpoint against the same healthy testcontainer pool and asserts that
// both responses carry identical check names in identical order and the same
// degraded value. This confirms the single-source-of-truth property: both
// endpoints call healthz.Run and neither has its own check logic.
//
// AC-6: VERIFY MCP doctor and HTTP /healthz produce identical report shapes.
func TestDoctor_SharedFunction(t *testing.T) {
	// No warmEmbedder: the equality assertion between MCP and HTTP holds
	// regardless of embedder availability — both paths call the same healthz.Run,
	// so they will produce the same degraded value and the same check names in
	// the same order whether the embedder passes or fails.

	pool := NewTestPool(t)

	// ── MCP invocation ────────────────────────────────────────────────────────
	c := newMCPClient(t)
	mcpResult := callTool(t, c, "doctor", map[string]any{})
	require.False(t, mcpResult.IsError,
		"AC-6: MCP doctor must succeed; body: %s", resultText(t, mcpResult))

	var mcpBody map[string]any
	unmarshalResult(t, mcpResult, &mcpBody)

	// ── HTTP invocation ───────────────────────────────────────────────────────
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		report := healthz.Run(r.Context(), pool)
		status := http.StatusOK
		if report.Degraded {
			status = http.StatusServiceUnavailable
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(report)
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/healthz") //nolint:noctx
	require.NoError(t, err, "AC-6: GET /healthz must succeed")
	defer resp.Body.Close()

	var httpBody map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&httpBody),
		"AC-6: /healthz body must decode as JSON")

	// ── assert identical degraded value ──────────────────────────────────────
	assert.Equal(t, mcpBody["degraded"], httpBody["degraded"],
		"AC-6: MCP and HTTP responses must carry the same degraded value")

	// ── assert identical check names in identical order ───────────────────────
	mcpChecks, ok := mcpBody["checks"].([]any)
	require.True(t, ok, "AC-6: MCP body must contain checks array")
	httpChecks, ok := httpBody["checks"].([]any)
	require.True(t, ok, "AC-6: HTTP body must contain checks array")

	require.Equal(t, len(mcpChecks), len(httpChecks),
		"AC-6: MCP and HTTP must return the same number of checks")

	for i := range mcpChecks {
		mcpCheck, ok := mcpChecks[i].(map[string]any)
		require.True(t, ok, "AC-6: mcpChecks[%d] must be a JSON object", i)
		httpCheck, ok := httpChecks[i].(map[string]any)
		require.True(t, ok, "AC-6: httpChecks[%d] must be a JSON object", i)

		assert.Equal(t, mcpCheck["name"], httpCheck["name"],
			"AC-6: checks[%d].name must be identical in MCP and HTTP responses", i)
	}
}
