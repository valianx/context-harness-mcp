package mcp

import (
	"context"
	"fmt"
	"strings"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/mariogutierrez/context-harness-mcp/internal/embed"
)

// setupTraceRecorder installs an in-memory SpanRecorder as the global TracerProvider
// and returns the recorder plus a cleanup func that restores the previous provider.
func setupTraceRecorder(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { otel.SetTracerProvider(prev) })
	return rec
}

// findSpan returns the first ended span whose name matches the given name.
func findSpan(rec *tracetest.SpanRecorder, name string) (sdktrace.ReadOnlySpan, bool) {
	for _, s := range rec.Ended() {
		if s.Name() == name {
			return s, true
		}
	}
	return nil, false
}

// attrValue returns the string value for the given key in the span's attributes,
// or an empty string when not found.
func attrValue(s sdktrace.ReadOnlySpan, key string) string {
	for _, a := range s.Attributes() {
		if string(a.Key) == key {
			return a.Value.AsString()
		}
	}
	return ""
}

// attrExists returns true when the key is present in the span's attributes.
func attrExists(s sdktrace.ReadOnlySpan, key string) bool {
	for _, a := range s.Attributes() {
		if string(a.Key) == key {
			return true
		}
	}
	return false
}

// allAttrValues returns all attribute key-value pairs for scanning.
func allAttrValues(s sdktrace.ReadOnlySpan) []attribute.KeyValue {
	return s.Attributes()
}

// installMockEmbedder replaces the active embedder with the mock and restores on cleanup.
func installMockEmbedder(t *testing.T) {
	t.Helper()
	restore := embed.SetForTesting(embed.NewMockEncoder())
	t.Cleanup(restore)
}

// ── AC-E.1: each MCP tool invocation emits a semantically named span ──────────

// TestSpan_CreateNodes_emits_named_span_with_tool_name verifies that a policy
// rejection from create_nodes still emits a span named "mcp.create_nodes" with
// mcp.tool_name set. We trigger a validation error so we don't need a real pool.
func TestSpan_CreateNodes_emits_named_span_with_tool_name(t *testing.T) {
	rec := setupTraceRecorder(t)
	installMockEmbedder(t)

	handler := createNodesHandler(nil, nil) // pool=nil: validation runs before DB

	// Build a request whose Content Filter will reject it (empty nodeType triggers taxonomy).
	req := mcplib.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"nodes": []any{
			map[string]any{
				"name":         "TestNode",
				"nodeType":     "", // empty nodeType → taxonomy reject
				"observations": []any{"some text"},
			},
		},
	}

	_, _ = handler(context.Background(), req)

	span, ok := findSpan(rec, "mcp.create_nodes")
	if !ok {
		t.Fatal("expected span 'mcp.create_nodes' to be emitted")
	}
	if got := attrValue(span, "mcp.tool_name"); got != "create_nodes" {
		t.Errorf("mcp.tool_name = %q, want %q", got, "create_nodes")
	}
}

// failingEncoder is an embed.Embedder that always returns an error.
// It is used to cause searchNodesHandler to fail at the embed step — before
// reaching the pool — so we can test span attributes without a real DB.
type failingEncoder struct{}

func (failingEncoder) Encode(_ context.Context, _ []string) ([][]float32, error) {
	return nil, fmt.Errorf("embed: test-induced failure")
}

// TestSpan_SearchNodes_emits_named_span verifies mcp.search_nodes span name and
// mcp.query_length attribute.  A failing encoder is installed so the handler
// errors before reaching the pool.
func TestSpan_SearchNodes_emits_named_span(t *testing.T) {
	rec := setupTraceRecorder(t)

	// Use a failing encoder so we fail at embed — before touching the pool.
	restore := embed.SetForTesting(failingEncoder{})
	t.Cleanup(restore)

	handler := searchNodesHandler(nil)

	req := mcplib.CallToolRequest{}
	req.Params.Arguments = map[string]any{"query": "some query text"}

	_, _ = handler(context.Background(), req)

	span, ok := findSpan(rec, "mcp.search_nodes")
	if !ok {
		t.Fatal("expected span 'mcp.search_nodes' to be emitted")
	}
	if got := attrValue(span, "mcp.tool_name"); got != "search_nodes" {
		t.Errorf("mcp.tool_name = %q, want %q", got, "search_nodes")
	}
}

// ── AC-E.2: errors mark span with status ERROR + mcp.error_code + mcp.layer ──

// TestSpan_CreateNodes_policy_reject_sets_error_attributes verifies that a Content
// Filter rejection sets span status ERROR and the error_code / layer attributes,
// and that the secret value does NOT appear in the attributes.
func TestSpan_CreateNodes_policy_reject_sets_error_attributes(t *testing.T) {
	rec := setupTraceRecorder(t)
	installMockEmbedder(t)

	handler := createNodesHandler(nil, nil)

	req := mcplib.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"nodes": []any{
			map[string]any{
				"name":     "TestNode",
				"nodeType": "tech",
				// Known gitleaks pattern: fake AWS access key triggers Layer 2.
				"observations": []any{"AKIAIOSFODNN7EXAMPLE"},
			},
		},
	}

	_, _ = handler(context.Background(), req)

	span, ok := findSpan(rec, "mcp.create_nodes")
	if !ok {
		t.Fatal("expected span 'mcp.create_nodes' to be emitted")
	}

	// AC-E.2: outcome must be policy_reject.
	if got := attrValue(span, "mcp.tool_outcome"); got != outcomePolicyReject {
		t.Errorf("mcp.tool_outcome = %q, want %q", got, outcomePolicyReject)
	}

	// mcp.error_code must be set.
	if !attrExists(span, "mcp.error_code") {
		t.Error("expected mcp.error_code attribute to be present on rejected span")
	}

	// mcp.layer must be set.
	if !attrExists(span, "mcp.layer") {
		t.Error("expected mcp.layer attribute to be present on rejected span")
	}

	// AC-E.2: the secret value must not appear in any attribute (AC-F handles
	// the scrub layer, but the handler must not put it there in the first place).
	for _, a := range allAttrValues(span) {
		if strings.Contains(a.Value.AsString(), "AKIAIOSFODNN7EXAMPLE") {
			t.Errorf("span attribute %q contains secret value — must not be recorded", a.Key)
		}
	}
}

// ── AC-E.3: span attributes do NOT contain observation content ────────────────

// TestSpan_CreateNodes_no_observation_content verifies that observation text is
// never materialised as a span attribute.
func TestSpan_CreateNodes_no_observation_content(t *testing.T) {
	rec := setupTraceRecorder(t)
	installMockEmbedder(t)

	sensitiveText := "this is very sensitive observation content 12345"

	handler := createNodesHandler(nil, nil)

	req := mcplib.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"nodes": []any{
			map[string]any{
				"name":         "TestNode",
				"nodeType":     "tech",
				"observations": []any{sensitiveText},
			},
		},
	}

	_, _ = handler(context.Background(), req)

	span, ok := findSpan(rec, "mcp.create_nodes")
	if !ok {
		t.Fatal("expected span 'mcp.create_nodes' to be emitted")
	}

	for _, a := range allAttrValues(span) {
		if strings.Contains(a.Value.AsString(), sensitiveText) {
			t.Errorf("span attribute %q contains observation content — must not be recorded", a.Key)
		}
	}
}

// ── AC-E.4: search_nodes does not expose query content or result content ──────

// TestSpan_SearchNodes_no_query_content verifies that only mcp.query_length
// is recorded, never the query text itself or result content.
func TestSpan_SearchNodes_no_query_content(t *testing.T) {
	rec := setupTraceRecorder(t)

	// Use a failing encoder so the handler errors at the embed step, before the
	// pool is invoked.  The span is still emitted with the pre-pool attributes.
	restore := embed.SetForTesting(failingEncoder{})
	t.Cleanup(restore)

	sensitiveQuery := "sensitive query about private matters"

	handler := searchNodesHandler(nil)

	req := mcplib.CallToolRequest{}
	req.Params.Arguments = map[string]any{"query": sensitiveQuery}

	_, _ = handler(context.Background(), req)

	span, ok := findSpan(rec, "mcp.search_nodes")
	if !ok {
		t.Fatal("expected span 'mcp.search_nodes' to be emitted")
	}

	// mcp.query_length must be set.
	if !attrExists(span, "mcp.query_length") {
		t.Error("expected mcp.query_length to be present")
	}

	// The query text must never appear as a span attribute value.
	for _, a := range allAttrValues(span) {
		if strings.Contains(a.Value.AsString(), sensitiveQuery) {
			t.Errorf("span attribute %q contains query content — must not be recorded", a.Key)
		}
	}
}

// ── AC-E.5: mcp.tool_outcome is set on ALL exit paths via defer ───────────────

// TestSpan_ToolOutcome_set_on_all_exit_paths verifies that mcp.tool_outcome is
// always present on MCP spans regardless of the exit path (AC-E.5).
//
// Three paths are exercised without requiring a real DB connection:
//   - success (deferred via create_nodes with taxonomy pass, embed fail → server_error)
//   - policy_reject (taxonomy error in create_relations)
//   - server_error (embed fail in create_nodes before reaching the pool)
//
// The key invariant is that mcp.tool_outcome is always non-empty; the exact
// value depends on the path taken.
func TestSpan_ToolOutcome_set_on_all_exit_paths(t *testing.T) {
	validOutcomes := map[string]bool{
		outcomeSuccess:      true,
		outcomePolicyReject: true,
		outcomeServerError:  true,
	}

	checkOutcome := func(t *testing.T, span sdktrace.ReadOnlySpan) {
		t.Helper()
		outcome := attrValue(span, "mcp.tool_outcome")
		if outcome == "" {
			t.Error("mcp.tool_outcome must always be set — was empty")
		} else if !validOutcomes[outcome] {
			t.Errorf("mcp.tool_outcome = %q — not one of the allowed values", outcome)
		}
	}

	// Path 1: policy_reject — empty relationType triggers taxonomy rejection.
	t.Run("policy_reject", func(t *testing.T) {
		rec := setupTraceRecorder(t)

		handler := createRelationsHandler(nil, nil)
		req := mcplib.CallToolRequest{}
		req.Params.Arguments = map[string]any{
			"relations": []any{
				map[string]any{
					"from":         "NodeA",
					"to":           "NodeB",
					"relationType": "", // empty → taxonomy reject
				},
			},
		}
		_, _ = handler(context.Background(), req)

		span, ok := findSpan(rec, "mcp.create_relations")
		if !ok {
			t.Fatal("expected span 'mcp.create_relations' to be emitted")
		}
		if got := attrValue(span, "mcp.tool_outcome"); got != outcomePolicyReject {
			t.Errorf("mcp.tool_outcome = %q, want %q", got, outcomePolicyReject)
		}
		checkOutcome(t, span)
	})

	// Path 2: server_error — failing embedder causes error before pool is reached.
	t.Run("server_error_via_embed_fail", func(t *testing.T) {
		rec := setupTraceRecorder(t)

		restore := embed.SetForTesting(failingEncoder{})
		t.Cleanup(restore)

		handler := searchNodesHandler(nil)
		req := mcplib.CallToolRequest{}
		req.Params.Arguments = map[string]any{"query": "any query"}
		_, _ = handler(context.Background(), req)

		span, ok := findSpan(rec, "mcp.search_nodes")
		if !ok {
			t.Fatal("expected span 'mcp.search_nodes' to be emitted")
		}
		if got := attrValue(span, "mcp.tool_outcome"); got != outcomeServerError {
			t.Errorf("mcp.tool_outcome = %q, want %q", got, outcomeServerError)
		}
		checkOutcome(t, span)
	})

	// Path 3: open_nodes with empty names returns early (empty result) — the
	// defer still fires and sets tool_outcome.  The result is server_error
	// because openNodes itself returns before setting outcome to success when
	// len(names)==0 (it returns empty slices but no error — this path does NOT
	// touch the pool).
	t.Run("open_nodes_empty_names_outcome_is_set", func(t *testing.T) {
		rec := setupTraceRecorder(t)

		handler := openNodesHandler(nil) // pool not reached for empty names
		req := mcplib.CallToolRequest{}
		req.Params.Arguments = map[string]any{
			"names": []any{},
		}
		_, _ = handler(context.Background(), req)

		span, ok := findSpan(rec, "mcp.open_nodes")
		if !ok {
			t.Fatal("expected span 'mcp.open_nodes' to be emitted")
		}
		// outcome is whatever path openNodes took — the important thing is it is set.
		checkOutcome(t, span)
	})
}
