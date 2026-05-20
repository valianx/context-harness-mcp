package embed

// current is the active embedder. Production default: the real ONNX-backed
// FastEmbedder declared in model_cgo.go / model_stub.go. Tests swap this via
// SetForTesting to a deterministic mock to skip ONNX cost.
var current Embedder = defaultEmbedder

// Default returns the active embedder. All production callsites
// (internal/mcp, internal/healthz) use this.
func Default() Embedder {
	return current
}

// SetForTesting swaps the active embedder and returns a cleanup function that
// restores the previous one. Tests call SetForTesting in TestMain (or per-test)
// and defer the returned restore func — or pass it to t.Cleanup. Production
// code must never call this.
func SetForTesting(e Embedder) (restore func()) {
	prev := current
	current = e
	return func() { current = prev }
}

// RealEmbedder returns the package-level defaultEmbedder — the ONNX-backed
// FastEmbedder under CGO, or the stub under non-CGO builds. Tests that need
// semantic validation call this to get back to the real ONNX path after the
// suite-wide mock is installed.
func RealEmbedder() Embedder {
	return defaultEmbedder
}
