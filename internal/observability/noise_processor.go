// Package observability — noise_processor.go
//
// noiseFilterProcessor is an OTel SpanProcessor that drops low-value spans
// before they reach the downstream batch processor. It is positioned upstream
// of the BatchSpanProcessor in the pipeline so dropped spans incur zero export
// cost (no serialization, no HTTP call to Axiom).
//
// Dropped span names (AC-J.2):
//   - "prepare *"  — otelpgx emits one prepare span per unique SQL statement
//     on first execution; pgx caches prepared statements so these appear only
//     once per connection but add noise. otelpgx does not expose a native option
//     to disable prepare tracing, so the processor is the correct interception point.
//
// Non-dropped spans pass through to next unchanged (AC-J.3).
//
// The pool.acquire noise is handled separately via otelpgx.WithDisableAcquireTracer()
// in internal/store/pool.go — no processor logic needed for that.
package observability

import (
	"context"
	"strings"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// noiseFilterProcessor wraps a downstream SpanProcessor and drops spans whose
// names match known noise patterns. It implements sdktrace.SpanProcessor.
type noiseFilterProcessor struct {
	next sdktrace.SpanProcessor
}

// NewNoiseFilterProcessor wraps next with noise filtering.
// next must not be nil.
func NewNoiseFilterProcessor(next sdktrace.SpanProcessor) sdktrace.SpanProcessor {
	return &noiseFilterProcessor{next: next}
}

// OnStart drops spans matching noise patterns by not propagating to next.
// Non-matching spans are forwarded unchanged. Dropping here avoids the cost
// of attribute population on spans that will never be exported.
func (p *noiseFilterProcessor) OnStart(parent context.Context, s sdktrace.ReadWriteSpan) {
	if isNoiseSpan(s.Name()) {
		// Drop: do not propagate to downstream processor.
		return
	}
	p.next.OnStart(parent, s)
}

// OnEnd forwards to next for all spans. Spans that were dropped in OnStart
// still have OnEnd called by the SDK but will have no downstream effect since
// the batch processor never saw them in OnStart.
func (p *noiseFilterProcessor) OnEnd(s sdktrace.ReadOnlySpan) {
	if isNoiseSpan(s.Name()) {
		return
	}
	p.next.OnEnd(s)
}

// Shutdown forwards to next.
func (p *noiseFilterProcessor) Shutdown(ctx context.Context) error {
	return p.next.Shutdown(ctx)
}

// ForceFlush forwards to next.
func (p *noiseFilterProcessor) ForceFlush(ctx context.Context) error {
	return p.next.ForceFlush(ctx)
}

// isNoiseSpan returns true for span names that should be dropped.
//
// Dropped patterns:
//   - "prepare *"  — otelpgx statement-preparation spans (no native option to disable
//     in otelpgx v0.9.4 — only a SpanProcessor-level drop is available).
//   - "pool.acquire" — connection-checkout span. otelpgx v0.9.4 does not expose
//     WithDisableAcquireTracer; we drop at the processor level instead (AC-J.1).
func isNoiseSpan(name string) bool {
	return strings.HasPrefix(name, "prepare ") || name == "pool.acquire"
}
