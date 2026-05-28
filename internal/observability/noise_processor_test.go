package observability

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// setupNoiseFilterTP builds a TracerProvider that routes through
// noiseFilterProcessor → InMemoryExporter, returning the exporter and a cleanup func.
func setupNoiseFilterTP(t *testing.T) (*tracetest.InMemoryExporter, func()) {
	t.Helper()
	mem := tracetest.NewInMemoryExporter()
	batchProc := sdktrace.NewSimpleSpanProcessor(mem) // simple for deterministic test flush
	noiseProc := NewNoiseFilterProcessor(batchProc)

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(noiseProc),
	)
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	cleanup := func() {
		otel.SetTracerProvider(prev)
		_ = tp.Shutdown(context.Background())
	}
	return mem, cleanup
}

// TestNoiseFilterProcessor_DropsPrepareStar verifies AC-J.2:
// spans with names matching "prepare *" are dropped and never reach the exporter.
func TestNoiseFilterProcessor_DropsPrepareStar(t *testing.T) {
	mem, cleanup := setupNoiseFilterTP(t)
	defer cleanup()

	tracer := otel.Tracer("test")

	// Emit a "prepare *" span.
	_, prepareSpan := tracer.Start(context.Background(), "prepare SELECT * FROM nodes")
	prepareSpan.End()

	// Emit another common otelpgx noise span variant.
	_, prepare2 := tracer.Start(context.Background(), "prepare INSERT INTO observations")
	prepare2.End()

	spans := mem.GetSpans()
	for _, s := range spans {
		assert.False(t,
			isNoiseSpan(s.Name),
			"span %q must not reach exporter — it is a noise span", s.Name,
		)
	}
}

// TestNoiseFilterProcessor_PassesQuerySpans verifies AC-J.3:
// spans NOT matching the noise pattern pass through to the exporter intact.
func TestNoiseFilterProcessor_PassesQuerySpans(t *testing.T) {
	mem, cleanup := setupNoiseFilterTP(t)
	defer cleanup()

	tracer := otel.Tracer("test")

	_, qSpan := tracer.Start(context.Background(), "query SELECT id FROM nodes WHERE ...")
	qSpan.End()

	_, mcpSpan := tracer.Start(context.Background(), "mcp.search_nodes")
	mcpSpan.End()

	spans := mem.GetSpans()
	names := make([]string, len(spans))
	for i, s := range spans {
		names[i] = s.Name
	}

	require.Contains(t, names, "query SELECT id FROM nodes WHERE ...",
		"query span must not be dropped")
	require.Contains(t, names, "mcp.search_nodes",
		"MCP span must not be dropped")
}

// TestNoiseFilterProcessor_ExactPrefixMatch verifies that "prepare" without
// a trailing space is NOT dropped — only "prepare " (with space) matches.
func TestNoiseFilterProcessor_ExactPrefixMatch(t *testing.T) {
	mem, cleanup := setupNoiseFilterTP(t)
	defer cleanup()

	tracer := otel.Tracer("test")

	// "prepare" alone (no space) is not a prepare-statement span name.
	_, span := tracer.Start(context.Background(), "prepare")
	span.End()

	spans := mem.GetSpans()
	names := make([]string, len(spans))
	for i, s := range spans {
		names[i] = s.Name
	}
	require.Contains(t, names, "prepare",
		`span named "prepare" (no space) must not be dropped`)
}

// TestNoiseFilterProcessor_DropsPoolAcquire verifies AC-J.1:
// pool.acquire spans are dropped because otelpgx v0.9.4 has no native option
// to disable them; the processor-level drop is the only available mechanism.
func TestNoiseFilterProcessor_DropsPoolAcquire(t *testing.T) {
	mem, cleanup := setupNoiseFilterTP(t)
	defer cleanup()

	tracer := otel.Tracer("test")

	// Simulate what otelpgx emits for connection checkout.
	_, acqSpan := tracer.Start(context.Background(), "pool.acquire")
	acqSpan.End()

	// Emit a real query span to verify it still passes.
	_, qSpan := tracer.Start(context.Background(), "query SELECT 1")
	qSpan.End()

	spans := mem.GetSpans()
	names := make([]string, len(spans))
	for i, s := range spans {
		names[i] = s.Name
	}

	assert.NotContains(t, names, "pool.acquire",
		"pool.acquire span must be dropped by noiseFilterProcessor (AC-J.1)")
	assert.Contains(t, names, "query SELECT 1",
		"query span must not be dropped")
}

// TestIsNoiseSpan verifies the predicate directly.
func TestIsNoiseSpan(t *testing.T) {
	cases := []struct {
		name  string
		noise bool
	}{
		{"prepare SELECT * FROM nodes", true},
		{"prepare INSERT INTO observations (node_id, text) VALUES ($1, $2)", true},
		{"prepare ", true},    // bare "prepare " with trailing space
		{"pool.acquire", true}, // AC-J.1 — otelpgx v0.9.4 has no native disable option
		{"query SELECT id FROM nodes", false},
		{"mcp.search_nodes", false},
		{"prepare", false}, // no trailing space
		{"", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := isNoiseSpan(tc.name)
			assert.Equal(t, tc.noise, got, "isNoiseSpan(%q)", tc.name)
		})
	}
}
