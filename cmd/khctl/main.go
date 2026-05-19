// Command khctl (knowledge-harness control) is the operator CLI for
// context-harness-mcp. It provides three subcommands:
//
//	seed   — insert deterministic dev fixtures into a Postgres KG DB
//	export — export active KG rows to JSON
//	import — import a KG JSON file into Postgres (idempotent merge)
//
// Each subcommand reads --dsn or falls back to the SUPABASE_DB_URL env var.
// khctl is compiled with CGO_ENABLED=0 (no ONNX bindings needed) and ships
// as a static binary alongside `server` in the Docker image.
package main

import (
	"context"
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	sub := os.Args[1]
	args := os.Args[2:]

	switch sub {
	case "seed":
		seedCmd(args)
	case "export":
		exportCmd(args)
	case "import":
		importCmd(args)
	case "help", "--help", "-h":
		printUsage()
		os.Exit(0)
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand: %s\n", sub)
		printUsage()
		os.Exit(1)
	}
}

// getContext returns a background context for subcommand use.
func getContext() context.Context {
	return context.Background()
}

func printUsage() {
	fmt.Fprint(os.Stderr, `khctl — knowledge-harness control

Usage:
  khctl <subcommand> [flags]

Subcommands:
  seed   [--dsn URL] [--reset]    Insert deterministic dev fixtures (idempotent).
                                   --reset truncates nodes/observations/relations first.
  export [--dsn URL] [--out PATH] Export active KG to JSON. --out - or omit → stdout.
  import [INPUT] [--dsn URL]      Import KG JSON into Postgres (idempotent merge).
                                   INPUT is a file path; use - for stdin.

Flags (shared):
  --dsn URL   Postgres DSN. Defaults to $SUPABASE_DB_URL.

Exit codes:
  0  success
  1  user error (bad flags, file not found, DSN missing)
  2  system error (DB failure, I/O error)
`)
}
