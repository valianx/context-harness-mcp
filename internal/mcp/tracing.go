// Package mcp — tracing.go provides the OTel tracer and span attribute keys
// used by tool handlers in this package. Attribute names are drawn from the
// whitelist defined in the axiom-integration plan (01-plan.md §Architecture,
// "Span attributes recommended"). All keys in the allowed set are declared
// here; any key outside this set must not appear in exported spans (AC-I.8).
//
// Full-payload mode (operator-approved override of original prohibition):
// mcp.request_body and mcp.response_body carry the complete JSON of each
// tool request and response. All values pass through ScrubSpanExporter before
// export so secrets are redacted before leaving the process.
package mcp

import (
	"encoding/json"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/mariogutierrez/context-harness-mcp/internal/observability"
)

const (
	// tracerName is the instrumentation scope for spans emitted by MCP handlers.
	tracerName = "github.com/mariogutierrez/context-harness-mcp/mcp"

	// Outcome values for mcp.tool_outcome.
	outcomeSuccess      = "success"
	outcomePolicyReject = "policy_reject"
	outcomeServerError  = "server_error"

	// Cap constants for tiered content attrs (AC-I plan §Whitelist diff).
	// All new string attrs pass through ScrubSpanExporter before export (AC-F boundary).
	queryAttrCap      = 500  // max chars for mcp.query
	snippetCap        = 1000 // max chars per observation snippet (bumped from 100 for full-payload mode)
	snippetMaxCount   = 10   // max number of snippets in mcp.observation_snippets
	nameSliceMaxCount = 20   // max elements in mcp.node_names / mcp.node_types
	relationsMaxCount = 20   // max elements in mcp.relations_summary
	rejectedInputCap  = 500  // max chars for mcp.rejected_input
)

// Attribute keys — complete whitelist (10 existing + 8 tiered-content from PR-I + 6 full-payload).
// See plan §Whitelist diff for the full list.
var (
	// --- 10 pre-existing keys ---
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

	// --- 8 keys added by PR-I (tiered content) ---
	attrQuery               = attribute.Key("mcp.query")
	attrResultNames         = attribute.Key("mcp.result_names")
	attrNodeNames           = attribute.Key("mcp.node_names")
	attrNodeTypes           = attribute.Key("mcp.node_types")
	attrNodeName            = attribute.Key("mcp.node_name")
	attrObservationSnippets = attribute.Key("mcp.observation_snippets")
	attrRelationsSummary    = attribute.Key("mcp.relations_summary")
	attrRejectedInput       = attribute.Key("mcp.rejected_input")

	// --- 6 keys added by full-payload mode (operator-approved) ---
	// All values pass through ScrubSpanExporter before export — secrets are
	// redacted by the existing boundary; no local scrub is applied here.
	attrRequestBody  = attribute.Key("mcp.request_body")
	attrResponseBody = attribute.Key("mcp.response_body")
	attrRowText      = attribute.Key("mcp.row_text")
	attrRowNodeID    = attribute.Key("mcp.row_node_id")
	attrRowName      = attribute.Key("mcp.row_name")
	attrRowNodeType  = attribute.Key("mcp.row_node_type")
)

// toolTracer returns the global OTel tracer for MCP handlers.
// When observability is disabled the global TracerProvider is the no-op
// provider, so Start returns no-op spans with zero overhead.
func toolTracer() trace.Tracer {
	return otel.Tracer(tracerName)
}

// setRejectedInputAttr records a truncated, scrubbed fragment of the rejected
// payload on the span. It is called only when outcome is policy_reject or
// server_error, so the attribute never appears on successful spans (AC-I.6).
//
// The value passes through ScrubSpanExporter on export — this call does NOT
// apply scrubbing itself; it relies on the existing PR-F boundary.
func setRejectedInputAttr(span trace.Span, fragment string) {
	if fragment == "" {
		return
	}
	span.SetAttributes(attrRejectedInput.String(observability.SnippetTruncate(fragment, rejectedInputCap)))
}

// marshalBody serialises v to a JSON string for span/log body fields.
// Returns "<marshal error>" when json.Marshal fails (e.g. non-marshalable types)
// so callers never need to guard on the error — the handler continues normally.
func marshalBody(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "<marshal error>"
	}
	return string(b)
}
