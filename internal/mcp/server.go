// Package mcp provides the mcp-go server factory and tool registration helpers.
package mcp

import (
	"github.com/jackc/pgx/v5/pgxpool"
	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/mariogutierrez/context-harness-mcp/internal/healthz"
	"github.com/mariogutierrez/context-harness-mcp/internal/ratelimit"
)

const (
	serverName    = "context-harness-mcp"
	serverVersion = "1.1.0"
)

// New returns a configured *server.MCPServer with all 16 MCP tools registered.
// pool must be non-nil — the server requires DB access for all write and read
// tool handlers. limiter enforces per-IP write-tool rate limits; pass a non-nil
// *ratelimit.Limiter for HTTP deployments.
func New(pool *pgxpool.Pool, limiter *ratelimit.Limiter) *server.MCPServer {
	s := server.NewMCPServer(serverName, serverVersion)
	RegisterDoctor(s, pool)
	RegisterNodes(s, pool, limiter)
	RegisterRelations(s, pool, limiter)
	RegisterQuery(s, pool)
	RegisterConflicts(s, pool, limiter)
	RegisterSuggestNodeType(s, pool)
	RegisterSessions(s, pool, limiter)
	return s
}

// RegisterDoctor registers the "doctor" MCP tool on the given MCPServer.
// The tool runs 5 deep operational probes (db_ping, pgvector_extension,
// embedder, gitleaks_detector, row_counts) and always returns IsError:false —
// degradation is reported in the JSON body via the "degraded" field.
//
// It is exported so tests can register it in isolation without a full server.
func RegisterDoctor(s *server.MCPServer, pool *pgxpool.Pool) {
	tool := mcplib.NewTool(
		"doctor",
		mcplib.WithDescription("Runs deep operational health probes (db, pgvector, embedder, gitleaks, row counts) and returns a structured report. Always returns IsError:false — read the 'degraded' field in the body."),
	)
	s.AddTool(tool, instrumentTool("doctor", healthz.Handler(pool)))
}
