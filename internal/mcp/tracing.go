// Package mcp — tracing.go provides the OTel tracer and span attribute keys
// used by tool handlers in this package. Attribute names are drawn from the
// whitelist defined in the axiom-integration plan (01-plan.md §Architecture,
// "Span attributes recommended").
//
// Prohibited without exception: observation content, query strings, node names,
// user.email, SQL query parameter values.
package mcp

import (
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

const (
	// tracerName is the instrumentation scope for spans emitted by MCP handlers.
	tracerName = "github.com/mariogutierrez/context-harness-mcp/mcp"

	// Outcome values for mcp.tool_outcome.
	outcomeSuccess      = "success"
	outcomePolicyReject = "policy_reject"
	outcomeServerError  = "server_error"
)

// Attribute keys (whitelist only — see plan §Architecture "Span attributes recommended").
var (
	attrToolName         = attribute.Key("mcp.tool_name")
	attrToolOutcome      = attribute.Key("mcp.tool_outcome")
	attrErrorCode        = attribute.Key("mcp.error_code")
	attrLayer            = attribute.Key("mcp.layer")
	attrNodeCount        = attribute.Key("mcp.node_count")
	attrObservationCount = attribute.Key("mcp.observation_count")
	attrResultCount      = attribute.Key("mcp.result_count")
	attrQueryLength      = attribute.Key("mcp.query_length")
	attrHasEmbedding     = attribute.Key("mcp.has_embedding")
	attrUserID           = attribute.Key("user.id")
)

// toolTracer returns the global OTel tracer for MCP handlers.
// When observability is disabled the global TracerProvider is the no-op
// provider, so Start returns no-op spans with zero overhead.
func toolTracer() trace.Tracer {
	return otel.Tracer(tracerName)
}
