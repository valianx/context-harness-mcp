// Package observability — signal_processor.go
//
// SignalSpanProcessor is an OTel SpanProcessor that stamps every span with
// attribute signal="trace" on OnStart.  This lets Axiom queries distinguish
// spans from log records with a simple:
//
//	where attributes.signal == 'trace'
//
// The processor wraps a downstream SpanProcessor and is inserted at the head
// of the pipeline in initTracer so ALL spans — including auto-instrumentation
// spans from otelhttp and otelpgx — carry the attribute (AC-SG.2, AC-SG.3).
package observability

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// signalAttr is the attribute stamped on every span.
// Using a package-level var avoids re-allocating the attribute on each OnStart.
var signalAttr = attribute.String("signal", "trace")

// SignalSpanProcessor wraps a downstream SpanProcessor and sets
// attribute signal="trace" on every span's OnStart.
type SignalSpanProcessor struct {
	next sdktrace.SpanProcessor
}

// NewSignalSpanProcessor wraps next with signal-attribute injection.
// next must not be nil.
func NewSignalSpanProcessor(next sdktrace.SpanProcessor) sdktrace.SpanProcessor {
	return &SignalSpanProcessor{next: next}
}

// OnStart sets signal="trace" on the span and forwards to next.
// All spans receive the attribute unconditionally (AC-SG.2, AC-SG.3).
func (p *SignalSpanProcessor) OnStart(parent context.Context, s sdktrace.ReadWriteSpan) {
	s.SetAttributes(signalAttr)
	p.next.OnStart(parent, s)
}

// OnEnd forwards to next unchanged.
func (p *SignalSpanProcessor) OnEnd(s sdktrace.ReadOnlySpan) {
	p.next.OnEnd(s)
}

// Shutdown forwards to next.
func (p *SignalSpanProcessor) Shutdown(ctx context.Context) error {
	return p.next.Shutdown(ctx)
}

// ForceFlush forwards to next.
func (p *SignalSpanProcessor) ForceFlush(ctx context.Context) error {
	return p.next.ForceFlush(ctx)
}
