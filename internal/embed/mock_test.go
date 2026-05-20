package embed

import (
	"context"
	"testing"
)

func TestMockEncoder_ReturnsDimPerInput(t *testing.T) {
	enc := NewMockEncoder()
	texts := []string{"hello", "world", "test"}
	vecs, err := enc.Encode(context.Background(), texts)
	if err != nil {
		t.Fatalf("Encode must never error, got: %v", err)
	}
	if len(vecs) != len(texts) {
		t.Fatalf("expected %d vectors, got %d", len(texts), len(vecs))
	}
	for i, v := range vecs {
		if len(v) != EmbeddingDim {
			t.Errorf("vecs[%d]: expected dim %d, got %d", i, EmbeddingDim, len(v))
		}
	}
}

func TestMockEncoder_Deterministic(t *testing.T) {
	enc := NewMockEncoder()
	ctx := context.Background()
	first, _ := enc.Encode(ctx, []string{"deterministic"})
	second, _ := enc.Encode(ctx, []string{"deterministic"})
	for i := range first[0] {
		if first[0][i] != second[0][i] {
			t.Errorf("dim %d: first=%f second=%f — must be identical", i, first[0][i], second[0][i])
		}
	}
}

func TestMockEncoder_DifferentTextsProduceDifferentVectors(t *testing.T) {
	enc := NewMockEncoder()
	vecs, _ := enc.Encode(context.Background(), []string{"alpha", "beta"})
	identical := true
	for i := range vecs[0] {
		if vecs[0][i] != vecs[1][i] {
			identical = false
			break
		}
	}
	if identical {
		t.Error("different texts must produce different vectors")
	}
}

func TestMockEncoder_EmptyBatchReturnsEmptySlice(t *testing.T) {
	enc := NewMockEncoder()
	vecs, err := enc.Encode(context.Background(), []string{})
	if err != nil {
		t.Fatalf("Encode must never error, got: %v", err)
	}
	if len(vecs) != 0 {
		t.Errorf("empty batch: expected 0 vectors, got %d", len(vecs))
	}
}

func TestMockEncoder_NeverErrors(t *testing.T) {
	enc := NewMockEncoder()
	texts := []string{"a", "b", "c", "d", "e"}
	_, err := enc.Encode(context.Background(), texts)
	if err != nil {
		t.Errorf("MockEncoder.Encode must never return an error, got: %v", err)
	}
}
