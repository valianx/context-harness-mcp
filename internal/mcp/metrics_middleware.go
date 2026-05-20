package mcp

import (
	"context"
	"strings"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"

	"github.com/mariogutierrez/context-harness-mcp/internal/metrics"
)

// instrumentTool wraps an mcp-go ToolHandlerFunc with Prometheus instrumentation:
//   - records handler latency in mcp_tool_duration_seconds{tool}
//   - increments mcp_tool_calls_total{tool, status}
//
// Status label mapping:
//
//	IsError=false                        → "success"
//	IsError=true + body has "rate-limit" → "rate_limited"
//	IsError=true + body has "policy/"    → "policy_reject"
//	IsError=true otherwise               → "error"
//	err != nil                           → "error"
func instrumentTool(toolName string, handler func(context.Context, mcplib.CallToolRequest) (*mcplib.CallToolResult, error)) func(context.Context, mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
	return func(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
		start := time.Now()
		result, err := handler(ctx, req)
		elapsed := time.Since(start)

		metrics.ToolDuration.WithLabelValues(toolName).Observe(elapsed.Seconds())
		metrics.ToolCalls.WithLabelValues(toolName, resolveStatus(result, err)).Inc()

		return result, err
	}
}

// resolveStatus maps a tool result and error to a Prometheus status label value.
func resolveStatus(result *mcplib.CallToolResult, err error) string {
	if err != nil {
		return "error"
	}
	if result == nil || !result.IsError {
		return "success"
	}
	body := extractTextBody(result)
	switch {
	case strings.Contains(body, "rate-limit"):
		return "rate_limited"
	case strings.Contains(body, "policy/"):
		return "policy_reject"
	default:
		return "error"
	}
}

// extractTextBody returns the text content of the first TextContent item in
// the result, or an empty string when no text content is present.
func extractTextBody(result *mcplib.CallToolResult) string {
	for _, c := range result.Content {
		if t, ok := mcplib.AsTextContent(c); ok {
			return t.Text
		}
	}
	return ""
}
