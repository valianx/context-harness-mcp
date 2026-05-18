// Package mcp provides the mcp-go server factory and tool registration helpers.
package mcp

import (
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/mariogutierrez/context-harness-mcp/internal/healthz"
)

const (
	serverName    = "context-harness-mcp"
	serverVersion = "0.1.0"
)

// New returns a configured *server.MCPServer ready for transport wiring.
// PR-3 through PR-5 extend this function by adding additional tools via
// RegisterXxx helpers.
func New() *server.MCPServer {
	s := server.NewMCPServer(serverName, serverVersion)
	RegisterHealthz(s)
	return s
}

// RegisterHealthz registers the healthz tool on the given MCPServer.
// It is exported so main.go can also call it directly during bootstrap,
// and so future PRs can reuse it in tests without constructing a full server.
func RegisterHealthz(s *server.MCPServer) {
	tool := mcp.NewTool(
		"healthz",
		mcp.WithDescription("Returns the operational health of the MCP server and its dependencies."),
	)
	s.AddTool(tool, healthz.Handler)
}
