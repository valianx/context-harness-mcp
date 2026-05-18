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
	serverVersion = "0.1.0"
)

// New returns a configured *server.MCPServer with all 6 MCP tools registered.
// pool must be non-nil — the server requires DB access for all write and read
// tool handlers. limiter enforces per-IP write-tool rate limits; pass a non-nil
// *ratelimit.Limiter for HTTP deployments.
func New(pool *pgxpool.Pool, limiter *ratelimit.Limiter) *server.MCPServer {
	s := server.NewMCPServer(serverName, serverVersion)
	RegisterHealthz(s)
	RegisterNodes(s, pool, limiter)
	RegisterRelations(s, pool, limiter)
	RegisterQuery(s, pool)
	return s
}

// RegisterHealthz registers the healthz tool on the given MCPServer.
// It is exported so main.go can also call it directly during bootstrap,
// and so future PRs can reuse it in tests without constructing a full server.
func RegisterHealthz(s *server.MCPServer) {
	tool := mcplib.NewTool(
		"healthz",
		mcplib.WithDescription("Returns the operational health of the MCP server and its dependencies."),
	)
	s.AddTool(tool, healthz.Handler)
}
