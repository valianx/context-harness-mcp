// Package healthz provides the MCP healthz tool handler.
package healthz

import (
	"context"
	"encoding/json"

	"github.com/mark3labs/mcp-go/mcp"
)

// response is the JSON payload returned by the healthz tool.
type response struct {
	Status string `json:"status"`
	DB     string `json:"db"`
}

// Handler is an mcp-go ToolHandlerFunc for the "healthz" tool.
// It returns {"status":"ok","db":"not-configured"} until a real DB pool
// is wired in PR-2.
func Handler(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	payload := response{
		Status: "ok",
		DB:     "not-configured",
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	return mcp.NewToolResultText(string(data)), nil
}
