package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/mariogutierrez/context-harness-mcp/internal/khctl"
	"github.com/mariogutierrez/context-harness-mcp/internal/store"
)

// seedCmd implements the `khctl seed` subcommand.
func seedCmd(args []string) {
	fs := flag.NewFlagSet("seed", flag.ExitOnError)
	dsnFlag := fs.String("dsn", "", "Postgres DSN (defaults to $DATABASE_URL)")
	resetFlag := fs.Bool("reset", false, "TRUNCATE nodes, observations, and relations before seeding (dev only)")
	_ = fs.Parse(args)

	dsn, err := resolveDSN(*dsnFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "seed: %v\n", err)
		os.Exit(1)
	}

	ctx := getContext()
	pool, err := store.New(ctx, dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "seed: cannot connect to database: %v\n", err)
		os.Exit(2)
	}
	defer pool.Close()

	if *resetFlag {
		slog.Info("--reset: truncating nodes, observations, relations")
	}

	nodesIn, obsIn, relsIn, err := khctl.RunSeed(ctx, pool, *resetFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "seed: %v\n", err)
		os.Exit(2)
	}

	fmt.Printf("seed complete: nodes=%d inserted | observations=%d inserted | relations=%d inserted\n",
		nodesIn, obsIn, relsIn)
}
