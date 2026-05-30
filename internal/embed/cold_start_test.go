//go:build cgo

// cold_start_test.go verifies AC-E.6: the embed.encode span carries
// mcp.embed_cold=true on the first Encode call and mcp.embed_cold=false on
// all subsequent calls, using the atomic.Bool on FastEmbedder.
//
// This test is gated by the cgo build tag because FastEmbedder only exists
// in model_cgo.go.  On non-CGO builds the stub never sets mcp.embed_cold
// (no OTel span is emitted by the stub), and the test is not applicable.
//
// ONNX is mocked via the loadModel seam on FastEmbedder so these tests run
// without the native ONNX library present (unit-test environment, CI).
package embed

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// fakeModel is a minimal embedModel implementation that returns deterministic
// fake vectors. It satisfies the embedModel interface without requiring ONNX.
type fakeModel struct{}

func (f *fakeModel) Embed(texts []string, _ int) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = make([]float32, EmbeddingDim) // zero vector; shape is what matters
	}
	return out, nil
}

// fakeLoader returns an immediately-available fakeModel. Inject this into
// FastEmbedder.loadModel to bypass real ONNX initialization in tests.
func fakeLoader() (embedModel, error) {
	return &fakeModel{}, nil
}

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
// The ONNX loader is replaced by fakeLoader so no native library is needed.
func TestEmbedColdStart_first_call_is_cold(t *testing.T) {
	rec := setupEmbedTraceRecorder(t)

	// Inject the fake loader — no ONNX required.
	e := &FastEmbedder{loadModel: fakeLoader}

	if _, err := e.Encode(context.Background(), []string{"hello"}); err != nil {
		t.Fatalf("Encode returned unexpected error: %v", err)
	}

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
// FastEmbedder reports cold=false after a successful init has already run
// (i.e., sync.Once has fired and warm=true).
//
// Two Encode calls are made on the same embedder instance.  The first is the
// cold call (warm=false at entry); the second is the warm call (warm=true at
// entry because the first call's sync.Once set it).
func TestEmbedColdStart_warm_flag_reflects_sync_once_state(t *testing.T) {
	rec := setupEmbedTraceRecorder(t)

	// Inject the fake loader — no ONNX required.
	e := &FastEmbedder{loadModel: fakeLoader}

	// First call: cold start — sync.Once fires, warm becomes true.
	if _, err := e.Encode(context.Background(), []string{"first"}); err != nil {
		t.Fatalf("first Encode returned unexpected error: %v", err)
	}

	// Second call: warm path — warm.Load() returns true before span starts.
	if _, err := e.Encode(context.Background(), []string{"second"}); err != nil {
		t.Fatalf("second Encode returned unexpected error: %v", err)
	}

	// The second span (index 1) must have mcp.embed_cold=false.
	span, ok := findEmbedSpan(rec, 1)
	if !ok {
		t.Fatal("expected a second 'embed.encode' span to be emitted")
	}

	cold, found := boolAttr(span, "mcp.embed_cold")
	if !found {
		t.Fatal("expected 'mcp.embed_cold' attribute to be present on second embed.encode span")
	}
	if cold {
		t.Errorf("warm embedder: mcp.embed_cold = true, want false")
	}
}
