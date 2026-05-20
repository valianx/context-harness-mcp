// Package healthz provides deep operational health checks for the MCP server.
// The Run function is the single source of truth shared by the MCP "doctor"
// tool and the HTTP GET /healthz endpoint.
package healthz

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mark3labs/mcp-go/mcp"

	"github.com/mariogutierrez/context-harness-mcp/internal/embed"
	"github.com/mariogutierrez/context-harness-mcp/internal/validate"
)

// Check represents the result of a single operational probe.
type Check struct {
	Name       string `json:"name"`
	Status     string `json:"status"`     // "pass" or "fail"
	DurationMs int64  `json:"duration_ms"` // wall-clock time for this check
	Detail     string `json:"detail"`     // version string, error message, or metric summary
}

// Report is the aggregate result of all health checks.
type Report struct {
	Checks  []Check `json:"checks"`
	Degraded bool   `json:"degraded"` // true when any check has status "fail"
}

// perCheckTimeout is the per-check context deadline applied by runCheck.
// The embedder cold-start can take up to ~500 ms; warm calls are <40 ms.
// 5 s covers both with comfortable margin and matches the historical timeout.
const perCheckTimeout = 5 * time.Second

// Run executes the 5 operational probes in order and returns a Report.
// Checks run sequentially (deterministic order) and never short-circuit —
// all 5 always run regardless of earlier failures.
func Run(ctx context.Context, pool *pgxpool.Pool) Report {
	checks := []Check{
		runCheck(ctx, "db_ping", func(ctx context.Context) (string, error) {
			return checkDBPing(ctx, pool)
		}),
		runCheck(ctx, "pgvector_extension", func(ctx context.Context) (string, error) {
			return checkPgvectorExtension(ctx, pool)
		}),
		runCheck(ctx, "embedder", func(ctx context.Context) (string, error) {
			return checkEmbedder(ctx)
		}),
		runCheck(ctx, "gitleaks_detector", func(ctx context.Context) (string, error) {
			return checkGitleaksDetector()
		}),
		runCheck(ctx, "row_counts", func(ctx context.Context) (string, error) {
			return checkRowCounts(ctx, pool)
		}),
	}

	degraded := false
	for _, c := range checks {
		if c.Status == "fail" {
			degraded = true
			break
		}
	}

	return Report{Checks: checks, Degraded: degraded}
}

// runCheck executes fn within a 5-second timeout and wraps the result in a
// Check. If fn returns an error, Status is set to "fail" and Detail carries
// the error message. Otherwise Status is "pass" and Detail is fn's return value.
func runCheck(parent context.Context, name string, fn func(context.Context) (string, error)) Check {
	ctx, cancel := context.WithTimeout(parent, perCheckTimeout)
	defer cancel()

	start := time.Now()
	detail, err := fn(ctx)
	dur := time.Since(start)

	status := "pass"
	if err != nil {
		status = "fail"
		detail = err.Error()
	}

	return Check{
		Name:       name,
		Status:     status,
		DurationMs: dur.Milliseconds(),
		Detail:     detail,
	}
}

// checkDBPing pings the connection pool.
func checkDBPing(ctx context.Context, pool *pgxpool.Pool) (string, error) {
	return "", pool.Ping(ctx)
}

// checkPgvectorExtension queries pg_extension for the vector extension and
// returns its version string as detail.
func checkPgvectorExtension(ctx context.Context, pool *pgxpool.Pool) (string, error) {
	var extversion string
	err := pool.QueryRow(ctx,
		"SELECT extversion FROM pg_extension WHERE extname = 'vector'",
	).Scan(&extversion)
	if err != nil {
		return "", fmt.Errorf("extension vector not found in pg_extension: %w", err)
	}
	return extversion, nil
}

// checkEmbedder runs a single test encode through the ONNX pipeline. Fails
// only when Encode returns an error — latency is reported in Detail but not
// treated as a failure. The 200 ms threshold that previously gated this
// check would systematically fail on the first post-boot /healthz request
// because the ONNX sync.Once cold-start takes ~300-500 ms, while warm calls
// are <40 ms. Platform healthchecks (Railway, Render, Fly) probe within
// seconds of container boot and would mark the deployment unhealthy on what
// is actually a transient initialization cost, not a health problem.
func checkEmbedder(ctx context.Context) (string, error) {
	start := time.Now()
	vecs, err := embed.Default().Encode(ctx, []string{"healthcheck"})
	latency := time.Since(start)

	if err != nil {
		return "", fmt.Errorf("embedder error: %w", err)
	}

	dims := 0
	if len(vecs) > 0 {
		dims = len(vecs[0])
	}
	return fmt.Sprintf("all-MiniLM-L6-v2 %d dims (%dms)", dims, latency.Milliseconds()), nil
}

// checkGitleaksDetector fires the gitleaks sync.Once and reports the rule count.
// ctx is unused because the detector init is synchronous (no network I/O).
func checkGitleaksDetector() (string, error) {
	rules, err := validate.InitDetector()
	if err != nil {
		return "", fmt.Errorf("gitleaks init failed: %w", err)
	}
	return fmt.Sprintf("%d rules loaded", rules), nil
}

// checkRowCounts runs SELECT count(*) against nodes, observations, and relations.
// Pass if all 3 queries succeed; counts of 0 are valid (fresh deploy).
func checkRowCounts(ctx context.Context, pool *pgxpool.Pool) (string, error) {
	var nodes, obs, rel int64

	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM nodes WHERE deleted_at IS NULL",
	).Scan(&nodes); err != nil {
		return "", fmt.Errorf("nodes count failed: %w", err)
	}

	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM observations WHERE deleted_at IS NULL",
	).Scan(&obs); err != nil {
		return "", fmt.Errorf("observations count failed: %w", err)
	}

	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM relations WHERE deleted_at IS NULL",
	).Scan(&rel); err != nil {
		return "", fmt.Errorf("relations count failed: %w", err)
	}

	return fmt.Sprintf("nodes=%d obs=%d rel=%d", nodes, obs, rel), nil
}

// Handler is an mcp-go ToolHandlerFunc for the "doctor" tool.
// It always returns IsError:false — degradation is reported in the JSON body,
// not in the MCP envelope, so the agent caller reads the "degraded" field.
func Handler(pool *pgxpool.Pool) func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		report := Run(ctx, pool)
		data, err := json.Marshal(report)
		if err != nil {
			return nil, err
		}
		return mcp.NewToolResultText(string(data)), nil
	}
}
