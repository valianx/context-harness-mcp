package embed

import (
	"context"
	"hash/fnv"
)

// MockEncoder produces deterministic 384-dim vectors from FNV-64 hashes of
// input texts. The output has no semantic meaning — two semantically similar
// inputs produce uncorrelated vectors. Tests that need semantic validation must
// opt back into the real embedder via SetForTesting(RealEmbedder()).
//
// Each input text maps to a fixed vector: per-dimension, the text + dimension
// index are hashed with FNV-64a and the low 16 bits are normalized to [-1, 1].
// Two identical texts always produce identical vectors (deterministic across
// runs, platform-independent).
type MockEncoder struct{}

// NewMockEncoder returns a stateless deterministic encoder.
func NewMockEncoder() *MockEncoder { return &MockEncoder{} }

// Encode returns one 384-dim vector per input text. Never errors.
func (m *MockEncoder) Encode(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		out[i] = mockVector(t)
	}
	return out, nil
}

// mockVector hashes text and expands the hash into a 384-dim float32 slice.
// Each dimension is normalized to [-1, 1]. Deterministic for a given input.
func mockVector(text string) []float32 {
	vec := make([]float32, EmbeddingDim)
	h := fnv.New64a()
	var idxBuf [4]byte
	for i := 0; i < EmbeddingDim; i++ {
		h.Reset()
		// Mix text + dimension index so each dimension has a distinct value
		// even when the input texts are identical.
		h.Write([]byte(text))
		idxBuf[0] = byte(i)
		idxBuf[1] = byte(i >> 8)
		idxBuf[2] = byte(i >> 16)
		idxBuf[3] = byte(i >> 24)
		h.Write(idxBuf[:])
		sum := h.Sum64()
		// Normalize low 16 bits to [-1, 1].
		vec[i] = (float32(sum&0xFFFF)/32768.0) - 1.0
	}
	return vec
}
