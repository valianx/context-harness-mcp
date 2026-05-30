// Command khctl (knowledge-harness control) is the operator CLI for
// context-harness-mcp. It provides three subcommands:
//
//	seed   — insert deterministic dev fixtures into a Postgres KG DB
//	export — export active KG rows to JSON
//	import — import a KG JSON file into Postgres (idempotent merge)
//
// Each subcommand reads --dsn or falls back to the DATABASE_URL env var
// (with a backward-compat fallback to SUPABASE_DB_URL for one release).
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
	case "sync-users":
		syncUsersCmd(args)
	case "backup":
		backupCmd(args)
	case "restore":
		restoreCmd(args)
	case "revoke":
		revokeCmd(args)
	case "reinstate":
		reinstateCmd(args)
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
  seed       [--dsn URL] [--reset]              Insert deterministic dev fixtures (idempotent).
                                                --reset truncates nodes/observations/relations first.
  export     [--dsn URL] [--out PATH]           Export active KG to JSON. --out - or omit → stdout.
  import     [INPUT] [--dsn URL]                Import KG JSON into Postgres (idempotent merge).
                                                INPUT is a file path; use - for stdin.
  sync-users [--dsn URL]                        Reconcile public.users against Supabase Admin API.
             [--supabase-service-role-key KEY]  Defaults to $SUPABASE_SERVICE_ROLE_KEY.
             [--supabase-project-url URL]       Defaults to $SUPABASE_PROJECT_URL.
  backup     [--dsn URL] --out PATH             Stream pg_dump custom-format archive to PATH.
             [--compress N]                     Compression level 0-9 (default 9).
  restore    FILE [--dsn URL] [--clean] [--yes] Restore pg_dump custom-format archive (created by 'khctl backup').
                                                --clean drops existing schema first (destructive). --yes skips
                                                the interactive confirmation prompt required with --clean.
  revoke     <user> [--dsn URL] [--by email|subject]
                                                Set users.revoked_at = now() for the matching user.
                                                Exit 0: updated. Exit 1: not found. Exit 2: DB error.
  reinstate  <user> [--dsn URL] [--by email|subject]
                                                Set users.revoked_at = NULL for the matching user (un-revoke).
                                                Exit 0: updated. Exit 1: not found. Exit 2: DB error.

Both 'backup' and 'restore' require the 'pg_dump' / 'pg_restore' binaries
on PATH (debian package: postgresql-client).

Flags (shared):
  --dsn URL   Postgres DSN. Defaults to $DATABASE_URL (falls back to $SUPABASE_DB_URL for one release).

Exit codes:
  0  success
  1  user error (bad flags, file not found, DSN missing)
  2  system error (DB failure, I/O error)
`)
}
