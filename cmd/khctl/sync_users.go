package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/mariogutierrez/context-harness-mcp/internal/khctl"
)

// syncUsersCmd implements the `khctl sync-users` subcommand.
// It connects to the local DB and reconciles public.users against the Supabase
// Admin API. Intended to be run as a scheduled GitHub Action (every 6h) as a
// fallback when webhooks are unavailable.
//
// Exit codes:
//
//	0 — success or non-fatal reconciliation errors (cron resilience)
//	1 — missing required flags or env vars
func syncUsersCmd(args []string) {
	fs := flag.NewFlagSet("sync-users", flag.ExitOnError)
	dsnFlag := fs.String("dsn", "", "Postgres DSN (defaults to $DATABASE_URL)")
	serviceRoleKeyFlag := fs.String("supabase-service-role-key", "", "Supabase service role key (defaults to $SUPABASE_SERVICE_ROLE_KEY)")
	projectURLFlag := fs.String("supabase-project-url", "", "Supabase project URL (defaults to $SUPABASE_PROJECT_URL)")
	_ = fs.Parse(args)

	dsn, err := resolveDSN(*dsnFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sync-users: %v\n", err)
		os.Exit(1)
	}

	serviceRoleKey := resolveEnvFlag(*serviceRoleKeyFlag, "SUPABASE_SERVICE_ROLE_KEY")
	if serviceRoleKey == "" {
		fmt.Fprintln(os.Stderr, "sync-users: --supabase-service-role-key or $SUPABASE_SERVICE_ROLE_KEY is required")
		os.Exit(1)
	}

	projectURL := resolveEnvFlag(*projectURLFlag, "SUPABASE_PROJECT_URL")
	if projectURL == "" {
		fmt.Fprintln(os.Stderr, "sync-users: --supabase-project-url or $SUPABASE_PROJECT_URL is required")
		os.Exit(1)
	}

	ctx := getContext()
	if err := khctl.SyncUsers(ctx, dsn, serviceRoleKey, projectURL); err != nil {
		// Log and exit 0 per cron resilience contract — the GH Action workflow
		// must not fail on reconciliation errors (see users_sync.yml).
		slog.Warn("sync-users: reconciliation completed with errors", "error", err)
		fmt.Fprintf(os.Stderr, "warning: sync-users encountered errors: %v\n", err)
	} else {
		slog.Info("sync-users: reconciliation complete")
		fmt.Println("sync-users: complete")
	}
}

// resolveEnvFlag returns flagVal when non-empty; otherwise looks up the env var.
func resolveEnvFlag(flagVal, envKey string) string {
	if flagVal != "" {
		return flagVal
	}
	return os.Getenv(envKey)
}
