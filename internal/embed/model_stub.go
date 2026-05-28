//go:build !cgo

// model_stub.go is compiled when CGO is disabled (e.g. Windows dev boxes
// without a C toolchain, pure-Go CI stages). It returns a clear error from
// Encode so callers and tests can detect the unavailability and skip/degrade
// cleanly — no panic, no silent wrong results.

package embed

import (
	"context"
	"errors"
	"time"

	"github.com/mariogutierrez/context-harness-mcp/internal/metrics"
)

// FastEmbedder is a no-op stub used when the ONNX runtime is unavailable.
type FastEmbedder struct{}

// defaultEmbedder is the no-op stub singleton used by encoder.go's Default()
// and RealEmbedder() when CGO is disabled.
var defaultEmbedder = &FastEmbedder{}

// errONXUnavailable is returned by the stub Encode to signal that the ONNX
// runtime is not available in this build (CGO disabled).
var errONXUnavailable = errors.New("embed: ONNX runtime unavailable (CGO disabled in this build)")

// Encode always returns errONXUnavailable when compiled without CGO.
// The stub does not emit OTel spans — there is no real work to trace.
func (e *FastEmbedder) Encode(_ context.Context, _ []string) ([][]float32, error) {
	start := time.Now()
	defer func() { metrics.EmbedderDuration.Observe(time.Since(start).Seconds()) }()
	return nil, errONXUnavailable
}
