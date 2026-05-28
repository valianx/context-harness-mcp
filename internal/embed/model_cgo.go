//go:build cgo

package embed

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	fastembed "github.com/anush008/fastembed-go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/mariogutierrez/context-harness-mcp/internal/metrics"
	"github.com/mariogutierrez/context-harness-mcp/internal/observability"
)

const embedTracerName = "github.com/mariogutierrez/context-harness-mcp/embed"

// defaultEmbedder is the package-level singleton used by encoder.go's Default()
// and RealEmbedder(). The ONNX session is initialized lazily on first Encode.
var defaultEmbedder = &FastEmbedder{}

// FastEmbedder wraps fastembed-go's FlagEmbedding configured for
// all-MiniLM-L6-v2 (384 dims). Thread-safe: the ONNX session is
// materialized exactly once via sync.Once; concurrent first calls block.
type FastEmbedder struct {
	once    sync.Once
	model   *fastembed.FlagEmbedding
	initErr error

	// warm is set to true after sync.Once fires successfully. A caller that
	// reads warm=false before calling Do has the cold start (AC-E.6).
	warm atomic.Bool
}

// Encode encodes texts in a single batch and returns one []float32 per input.
// Each returned slice has length EmbeddingDim (384).
//
// If the ONNX runtime is unavailable (missing shared library, missing model
// files) Encode returns a wrapped error — it does NOT panic. The caller
// surfaces the error as an MCP error so a broken embedder degrades cleanly
// per-call rather than crashing the server.
func (e *FastEmbedder) Encode(ctx context.Context, texts []string) ([][]float32, error) {
	start := time.Now()
	// cold=true when this is the first call (sync.Once has not fired yet).
	cold := !e.warm.Load()

	ctx, span := otel.Tracer(embedTracerName).Start(ctx, "embed.encode",
		trace.WithAttributes(attribute.Bool("mcp.embed_cold", cold)),
	)
	defer span.End()

	defer func() {
		dur := time.Since(start)
		metrics.EmbedderDuration.Observe(dur.Seconds())
		observability.RecordEmbedDuration(ctx, cold, dur)
	}()

	e.once.Do(func() {
		showProgress := false
		opts := &fastembed.InitOptions{
			Model:                fastembed.AllMiniLML6V2,
			MaxLength:            512,
			ShowDownloadProgress: &showProgress,
		}
		m, err := fastembed.NewFlagEmbedding(opts)
		if err != nil {
			e.initErr = fmt.Errorf("embed: ONNX session init failed: %w", err)
			return
		}
		e.model = m
		// Mark warm after successful init so subsequent calls see cold=false.
		e.warm.Store(true)
	})

	if e.initErr != nil {
		return nil, e.initErr
	}

	// Batch-encode in one call; batchSize=256 is the fastembed-go default.
	vecs, err := e.model.Embed(texts, 256)
	if err != nil {
		return nil, fmt.Errorf("embed: encode batch failed: %w", err)
	}
	return vecs, nil
}
