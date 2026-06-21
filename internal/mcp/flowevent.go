package mcp

import (
	"context"
	"encoding/json"
	"log/slog"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mariogutierrez/context-harness-mcp/internal/ratelimit"
	"github.com/mariogutierrez/context-harness-mcp/internal/validate"
)

// RegisterFlowEvent registers the record_flow_event MCP tool on the server.
// The tool validates a metadata-only flow-event payload, runs it through the
// existing Content Filter, and relays a scrubbed structured log line with
// signal="flow_event" to the LoggerProvider→ScrubLogProcessor→OTLP/HTTP→Axiom
// pipeline when observability is enabled.
//
// The tool NEVER opens a pgx.Tx — flow events are not persisted to Postgres.
// pool is accepted for API consistency with other Register* helpers but is not used.
func RegisterFlowEvent(s *server.MCPServer, _ *pgxpool.Pool, limiter *ratelimit.Limiter) {
	s.AddTool(
		mcplib.NewTool("record_flow_event",
			mcplib.WithDescription("Record a metadata-only pipeline friction event (gate fail, guard block, verify reject, iteration loop, blocked, scope collapse, MCP unavailable, or abandon). Validated against the closed flow-event schema and relayed to the observability backend as a scrubbed structured log. Never persisted to the knowledge graph."),
			mcplib.WithObject("event",
				mcplib.Required(),
				mcplib.Description("Flow event payload. Must conform to the shared flow-event schema: common fields (event, ts, project, task_type, th_version) plus per-event fields for the specific event type."),
			),
		),
		instrumentTool("record_flow_event", recordFlowEventHandler(limiter)),
	)
}

// recordFlowEventHandler returns the ToolHandlerFunc for record_flow_event.
func recordFlowEventHandler(limiter *ratelimit.Limiter) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
		// 1. Rate-limit guard — same as all other write tools.
		if result := checkRateLimit(ctx, limiter); result != nil {
			return result, nil
		}

		// 2. Bind arguments — the payload arrives as a JSON object under "event".
		var rawEvent map[string]any
		if err := req.BindArguments(&rawEvent); err != nil {
			return errorResult("invalid arguments: " + err.Error()), nil
		}

		payload, err := unmarshalFlowEventPayload(rawEvent["event"])
		if err != nil {
			return errorResult("invalid event payload: " + err.Error()), nil
		}

		// 3. Validate against the closed flow-event schema (common + per-event
		//    fields) + secrets + user-path layers of the Content Filter.
		if verr := validate.ValidateFlowEvent(payload); verr != nil {
			// AC-1.9: on unknown-type rejection, also relay a drift-visibility log
			// so a drifted or outdated TH client surfaces in Axiom.
			if verr.Code == validate.CodeFlowEventUnknownType {
				relayFlowEventRejected(ctx, payload.Event)
			}
			return verr.ToMCPResult(), nil
		}

		// 4. Relay as a scrubbed structured log line.
		// The log goes through slog → BridgedSlogHandler → LoggerProvider →
		// ScrubLogProcessor → OTLP/HTTP → Axiom.  When CH_OBSERVABILITY_ENABLED
		// is not "true" the LoggerProvider is nil and the OTel emission path is a
		// no-op, but the slog line still writes to stdout (AC-1.8).
		// NEVER pass any flow-event field to a metric call site — metric labels
		// are unscrubbed by SDK design (AC-1.5).
		relayFlowEvent(ctx, payload)

		return jsonResult(map[string]any{"recorded": true}), nil
	}
}

// unmarshalFlowEventPayload converts the raw "event" argument value to a
// FlowEventPayload.  The MCP framework delivers arguments as map[string]any;
// we re-marshal to JSON and then unmarshal into the typed struct.
func unmarshalFlowEventPayload(raw any) (validate.FlowEventPayload, error) {
	b, err := json.Marshal(raw)
	if err != nil {
		return validate.FlowEventPayload{}, err
	}
	var p validate.FlowEventPayload
	if err := json.Unmarshal(b, &p); err != nil {
		return validate.FlowEventPayload{}, err
	}
	return p, nil
}

// relayFlowEvent emits exactly one structured log line with signal="flow_event"
// and the validated metadata fields.  The slog bridge fans this out to both
// stdout and the OTel LoggerProvider (when observability is enabled).
//
// No metric call sites appear in this function — that is the AC-1.5 invariant
// enforced by TestFlowEventHandler_NoMetricCallSites.
func relayFlowEvent(ctx context.Context, p validate.FlowEventPayload) {
	attrs := []any{
		"signal", "flow_event",
		"event", p.Event,
		"ts", p.Ts,
		"project", p.Project,
		"task_type", p.TaskType,
		"th_version", p.ThVersion,
	}
	attrs = appendPerEventAttrs(attrs, p)
	slog.InfoContext(ctx, "flow_event", attrs...)
}

// maxRejectedEventLen is the maximum number of bytes relayed for the offending
// event token in a flow_event_rejected log line. Values that exceed this cap or
// contain an absolute path / secret are replaced with a placeholder so the
// rejection log cannot become a content-leak side channel.
// fix(sec-001): bound + sanitize attacker-controlled offendingEvent before relay.
const maxRejectedEventLen = 64

// relayFlowEventRejected emits a reason-coded log line when the closed enum
// rejects an unknown event type (AC-1.9 — runtime drift visibility).
//
// The offending event value is attacker-controlled (any non-enum string), so it
// is sanitized before relay:
//   - If it exceeds maxRejectedEventLen bytes it is replaced with "<non-enum>".
//   - If validate.CheckEventToken finds a path or secret pattern it is replaced
//     with "<non-enum>" to avoid leaking content through the rejection log.
func relayFlowEventRejected(ctx context.Context, offendingEvent string) {
	safe := sanitizeRejectedEvent(offendingEvent)
	slog.InfoContext(ctx, "flow_event_rejected",
		"signal", "flow_event_rejected",
		"reason", "unknown-type",
		"event", safe,
	)
}

// sanitizeRejectedEvent returns the event string if it is safe to relay, or the
// placeholder "<non-enum>" when the value exceeds the length cap or matches a
// path/secret pattern.
func sanitizeRejectedEvent(s string) string {
	if len(s) > maxRejectedEventLen {
		return "<non-enum>"
	}
	if validate.ContainsPathOrSecret(s) {
		return "<non-enum>"
	}
	return s
}

// appendPerEventAttrs appends the per-event metadata fields to the attrs slice
// for the given event type.  Only non-zero/non-nil fields are included.
func appendPerEventAttrs(attrs []any, p validate.FlowEventPayload) []any {
	switch p.Event {
	case "guard.block":
		attrs = append(attrs,
			"hook", p.Hook,
			"reason", p.Reason,
			"resolved", p.Resolved != nil && *p.Resolved,
		)
	case "gate.fail":
		attrs = append(attrs,
			"gate", p.Gate,
			"verdict", p.Verdict,
		)
	case "verify.reject":
		attrs = append(attrs,
			"agent", p.Agent,
			"verdict", p.Verdict,
		)
	case "iteration.loop":
		if p.Stage != nil {
			attrs = append(attrs, "stage", *p.Stage)
		}
		if p.Iterations != nil {
			attrs = append(attrs, "iterations", *p.Iterations)
		}
	case "blocked":
		attrs = append(attrs, "reason", p.Reason)
	case "scope.collapse":
		if p.ItemsDropped != nil {
			attrs = append(attrs, "items_dropped", *p.ItemsDropped)
		}
	case "mcp.unavailable":
		attrs = append(attrs, "op", p.Op)
	case "abandon":
		if p.LastStage != nil {
			attrs = append(attrs, "last_stage", *p.LastStage)
		}
	}
	return attrs
}
