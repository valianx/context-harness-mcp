// Package embed provides a lazy-loaded ONNX embedding model for the
// context-harness-mcp server. The embedder is initialized on the first call to
// Encode and reused for all subsequent calls (sync.Once semantics).
//
// Two implementations exist:
//   - model_cgo.go  — real fastembed-go ONNX session; compiled when CGO is enabled.
//   - model_stub.go — returns a clear error; compiled when CGO is disabled (Windows
//     dev boxes, pure-Go CI stages). Tests skip cleanly on the error pattern.
package embed

import "context"

// Embedder encodes a batch of texts into float32 vectors.
// The returned slice has the same length as the input slice.
type Embedder interface {
	Encode(ctx context.Context, texts []string) ([][]float32, error)
}

// EmbeddingDim is the output dimensionality of all-MiniLM-L6-v2.
const EmbeddingDim = 384
