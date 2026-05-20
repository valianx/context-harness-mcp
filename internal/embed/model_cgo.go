//go:build cgo

package embed

import (
	"context"
	"fmt"
	"sync"
	"time"

	fastembed "github.com/anush008/fastembed-go"

	"github.com/mariogutierrez/context-harness-mcp/internal/metrics"
)

// defaultEmbedder is the package-level singleton.
var defaultEmbedder = &FastEmbedder{}

// Default returns the package-level *FastEmbedder singleton.
// The ONNX session is initialized lazily on the first Encode call.
func Default() *FastEmbedder {
	return defaultEmbedder
}

// FastEmbedder wraps fastembed-go's FlagEmbedding configured for
// all-MiniLM-L6-v2 (384 dims). Thread-safe: the ONNX session is
// materialized exactly once via sync.Once; concurrent first calls block.
type FastEmbedder struct {
	once    sync.Once
	model   *fastembed.FlagEmbedding
	initErr error
}

// Encode encodes texts in a single batch and returns one []float32 per input.
// Each returned slice has length EmbeddingDim (384).
//
// If the ONNX runtime is unavailable (missing shared library, missing model
// files) Encode returns a wrapped error — it does NOT panic. The caller
// surfaces the error as an MCP error so a broken embedder degrades cleanly
// per-call rather than crashing the server.
func (e *FastEmbedder) Encode(_ context.Context, texts []string) ([][]float32, error) {
	start := time.Now()
	defer func() { metrics.EmbedderDuration.Observe(time.Since(start).Seconds()) }()

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
