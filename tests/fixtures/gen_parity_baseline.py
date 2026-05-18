# /// script
# requires-python = ">=3.11"
# dependencies = [
#   "sentence-transformers>=3.0",
# ]
# ///
#
# Generates the parity baseline JSON for context-harness-mcp embeddings tests.
# Run once to regenerate parity_baseline.json:
#   uv run tests/fixtures/gen_parity_baseline.py > tests/fixtures/parity_baseline.json
#
# The output is an array of 20 records:
#   {"text": "...", "embedding": [384 floats]}
#
# Uses sentence-transformers with all-MiniLM-L6-v2 — the same model family
# that fastembed-go's AllMiniLML6V2 ("fast-all-MiniLM-L6-v2") is based on.
# The two model variants are quantised/optimised differently but produce vectors
# with cosine similarity >= 0.999 for the same input text.

import json
import sys
from sentence_transformers import SentenceTransformer

TEXTS = [
    "Stack profile: Go 1.23 + pgx/v5 + pgvector for the MCP server.",
    "Decision: no auth on the MCP endpoint, locked in intake.",
    "Pattern: every write handler calls validate.Run before opening pgx.Tx.",
    "Constraint: fastembed-go requires CGO and ONNX runtime shared library.",
    "Error: ONNX session init failed when LD_LIBRARY_PATH is not set correctly.",
    "Decision: observations.embedding is vector(384) NULL until PR-5 fills it.",
    "Pattern: sync.Once used for lazy ONNX session initialization in embed package.",
    "Stack: testcontainers-go spins up pgvector/pgvector:pg16 for integration tests.",
    "Decision: soft-delete via deleted_at column; no hard deletes in the schema.",
    "Pattern: cosine distance operator <=> used for semantic nearest-neighbor search.",
    "Constraint: pgvector HNSW partial index covers only non-null embedding rows.",
    "Decision: session-docs are git-ignored; agents communicate through shared files.",
    "Pattern: goose v3 manages all schema migrations; no manual SQL applied to DB.",
    "Service: context-harness-mcp replaces ChromaDB-based knowledge graph backend.",
    "Decision: entity names must be unique; idempotent create resolves conflicts.",
    "Pattern: batch encode all observation texts before opening the pgx transaction.",
    "Constraint: observation text truncated to 256 approximate tokens before embedding.",
    "Decision: search_nodes returns top-10 entities ranked by minimum cosine distance.",
    "Pattern: build tags isolate CGO-dependent code so the server compiles without CGO.",
    "Stack: mcp-go v0.47.1 provides the stdio and streamable-HTTP transport layers.",
]

def main():
    model = SentenceTransformer("all-MiniLM-L6-v2")
    embeddings = model.encode(TEXTS, normalize_embeddings=True)

    records = [
        {"text": text, "embedding": emb.tolist()}
        for text, emb in zip(TEXTS, embeddings)
    ]

    json.dump(records, sys.stdout, separators=(",", ":"))
    sys.stdout.write("\n")

if __name__ == "__main__":
    main()
