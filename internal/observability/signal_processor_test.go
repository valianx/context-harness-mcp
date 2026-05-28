package observability

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// setupSignalTP builds a TracerProvider routed through
// SignalSpanProcessor → InMemoryExporter, returning the exporter and cleanup.
func setupSignalTP(t *testing.T) (*tracetest.InMemoryExporter, func()) {
	t.Helper()
	mem := tracetest.NewInMemoryExporter()
	simpleProc := sdktrace.NewSimpleSpanProcessor(mem)
	signalProc := NewSignalSpanProcessor(simpleProc)

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(signalProc),
	)
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	cleanup := func() {
		otel.SetTracerProvider(prev)
		_ = tp.Shutdown(context.Background())
	}
	return mem, cleanup
}

// findSpanAttr searches an exported span's attributes for the given key and
// returns the value plus a presence flag.
func findSpanAttr(span tracetest.SpanStub, key attribute.Key) (attribute.Value, bool) {
	for _, kv := range span.Attributes {
		if kv.Key == key {
			return kv.Value, true
		}
	}
	return attribute.Value{}, false
}

// TestSignalSpanProcessor_AC_SG2_SpanCarriesSignalTrace verifies AC-SG.2:
// spans emitted via otel.Tracer carry attribute signal="trace".
func TestSignalSpanProcessor_AC_SG2_SpanCarriesSignalTrace(t *testing.T) {
	mem, cleanup := setupSignalTP(t)
	defer cleanup()

	tracer := otel.Tracer("test")
	_, span := tracer.Start(context.Background(), "mcp.search_nodes")
	span.End()

	spans := mem.GetSpans()
	require.Len(t, spans, 1, "exactly one span must be exported")

	val, ok := findSpanAttr(spans[0], attribute.Key("signal"))
	assert.True(t, ok, "signal attribute must be present on exported span")
	assert.Equal(t, "trace", val.AsString(), "signal attribute value must be 'trace'")
}

// TestSignalSpanProcessor_AC_SG3_AllSpansCarrySignal verifies AC-SG.3:
// every span — regardless of name or instrumentation source — receives signal="trace".
func TestSignalSpanProcessor_AC_SG3_AllSpansCarrySignal(t *testing.T) {
	mem, cleanup := setupSignalTP(t)
	defer cleanup()

	tracer := otel.Tracer("test")

	spanNames := []string{
		"POST /mcp",
		"query SELECT id FROM nodes WHERE ...",
		"mcp.create_nodes",
		"mcp.add_observations",
	}
	for _, name := range spanNames {
		_, s := tracer.Start(context.Background(), name)
		s.End()
	}

	spans := mem.GetSpans()
	require.Len(t, spans, len(spanNames), "all spans must be exported")

	for _, s := range spans {
		val, ok := findSpanAttr(s, attribute.Key("signal"))
		assert.True(t, ok, "span %q must have signal attribute", s.Name)
		assert.Equal(t, "trace", val.AsString(), "span %q signal value must be 'trace'", s.Name)
	}
}

// TestSignalSpanProcessor_PreservesExistingAttributes verifies that the
// processor does not remove or overwrite other span attributes (AC-SG.5).
func TestSignalSpanProcessor_PreservesExistingAttributes(t *testing.T) {
	mem, cleanup := setupSignalTP(t)
	defer cleanup()

	tracer := otel.Tracer("test")
	_, span := tracer.Start(context.Background(), "mcp.open_nodes")
	span.SetAttributes(
		attribute.String("tool", "open_nodes"),
		attribute.Int("result_count", 3),
	)
	span.End()

	spans := mem.GetSpans()
	require.Len(t, spans, 1)

	signalVal, hasSignal := findSpanAttr(spans[0], attribute.Key("signal"))
	assert.True(t, hasSignal, "signal attribute must be present")
	assert.Equal(t, "trace", signalVal.AsString())

	toolVal, hasTool := findSpanAttr(spans[0], attribute.Key("tool"))
	assert.True(t, hasTool, "pre-existing tool attribute must be preserved")
	assert.Equal(t, "open_nodes", toolVal.AsString())

	countVal, hasCount := findSpanAttr(spans[0], attribute.Key("result_count"))
	assert.True(t, hasCount, "pre-existing result_count attribute must be preserved")
	assert.Equal(t, int64(3), countVal.AsInt64())
}

// TestSignalSpanProcessor_ForwardsToDownstream verifies the processor
// propagates spans to the downstream processor (no silent drop).
func TestSignalSpanProcessor_ForwardsToDownstream(t *testing.T) {
	mem, cleanup := setupSignalTP(t)
	defer cleanup()

	tracer := otel.Tracer("test")

	for i := 0; i < 5; i++ {
		_, s := tracer.Start(context.Background(), "batch-span")
		s.End()
	}

	spans := mem.GetSpans()
	assert.Len(t, spans, 5, "all spans must be forwarded to downstream processor")
}
