//go:build cgo

// cold_start_test.go verifies AC-E.6: the embed.encode span carries
// mcp.embed_cold=true on the first Encode call and mcp.embed_cold=false on
// all subsequent calls, using the atomic.Bool on FastEmbedder.
//
// This test is gated by the cgo build tag because FastEmbedder only exists
// in model_cgo.go.  On non-CGO builds the stub never sets mcp.embed_cold
// (no OTel span is emitted by the stub), and the test is not applicable.
package embed

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// setupEmbedTraceRecorder installs an in-memory span recorder as the global
// TracerProvider and returns it, plus a cleanup func.
func setupEmbedTraceRecorder(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { otel.SetTracerProvider(prev) })
	return rec
}

// findEmbedSpan returns the nth span named "embed.encode" from the recorder
// (0-indexed).
func findEmbedSpan(rec *tracetest.SpanRecorder, idx int) (tracetest.SpanStub, bool) {
	count := 0
	for _, ro := range rec.Ended() {
		s := tracetest.SpanStubFromReadOnlySpan(ro)
		if s.Name == "embed.encode" {
			if count == idx {
				return s, true
			}
			count++
		}
	}
	return tracetest.SpanStub{}, false
}

// boolAttr returns the bool value of the named attribute, and whether it was
// found.
func boolAttr(s tracetest.SpanStub, key string) (bool, bool) {
	for _, a := range s.Attributes {
		if string(a.Key) == key {
			return a.Value.AsBool(), true
		}
	}
	return false, false
}

// TestEmbedColdStart_first_call_is_cold verifies AC-E.6: the first Encode
// call on a fresh FastEmbedder emits a span with mcp.embed_cold=true.
// The ONNX model init will fail (model files not present in CI), but the span
// is started before sync.Once.Do so the attribute is already recorded.
func TestEmbedColdStart_first_call_is_cold(t *testing.T) {
	rec := setupEmbedTraceRecorder(t)

	// Create a fresh embedder so sync.Once has not fired.
	e := &FastEmbedder{}

	// Encode will fail (no ONNX model in unit test env), but that is fine — we
	// only care that the span was emitted with the correct cold attribute.
	_, _ = e.Encode(context.Background(), []string{"hello"})

	span, ok := findEmbedSpan(rec, 0)
	if !ok {
		t.Fatal("expected span 'embed.encode' to be emitted on first call")
	}

	cold, found := boolAttr(span, "mcp.embed_cold")
	if !found {
		t.Fatal("expected 'mcp.embed_cold' attribute to be present on embed.encode span")
	}
	if !cold {
		t.Errorf("first call: mcp.embed_cold = false, want true")
	}
}

// TestEmbedColdStart_warm_flag_reflects_sync_once_state verifies that a
// FastEmbedder reports cold=false when warm.Load() is true — simulating the
// state after a successful init.  We directly manipulate warm to avoid
// requiring ONNX model files.
func TestEmbedColdStart_warm_flag_reflects_sync_once_state(t *testing.T) {
	rec := setupEmbedTraceRecorder(t)

	e := &FastEmbedder{}
	// Simulate a previously successful init by marking the embedder warm.
	e.warm.Store(true)
	// Also force sync.Once to be "done" by running an empty Do.
	e.once.Do(func() {})
	// initErr remains nil, model remains nil — Embed will panic/fail but span fires.

	_, _ = e.Encode(context.Background(), []string{"hello"})

	span, ok := findEmbedSpan(rec, 0)
	if !ok {
		t.Fatal("expected span 'embed.encode' to be emitted")
	}

	cold, found := boolAttr(span, "mcp.embed_cold")
	if !found {
		t.Fatal("expected 'mcp.embed_cold' attribute to be present")
	}
	if cold {
		t.Errorf("warm embedder: mcp.embed_cold = true, want false")
	}
}
