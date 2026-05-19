package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/mariogutierrez/context-harness-mcp/internal/khctl"
	"github.com/mariogutierrez/context-harness-mcp/internal/store"
)

// exportCmd implements the `khctl export` subcommand.
func exportCmd(args []string) {
	fs := flag.NewFlagSet("export", flag.ExitOnError)
	dsnFlag := fs.String("dsn", "", "Postgres DSN (defaults to $SUPABASE_DB_URL)")
	outFlag := fs.String("out", "-", "Output file path (- or omit → stdout)")
	_ = fs.Parse(args)

	dsn, err := resolveDSN(*dsnFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "export: %v\n", err)
		os.Exit(1)
	}

	ctx := getContext()
	pool, err := store.New(ctx, dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "export: cannot connect to database: %v\n", err)
		os.Exit(2)
	}
	defer pool.Close()

	payload, err := khctl.BuildExportPayload(ctx, pool)
	if err != nil {
		fmt.Fprintf(os.Stderr, "export: %v\n", err)
		os.Exit(2)
	}

	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "export: marshal JSON: %v\n", err)
		os.Exit(2)
	}
	data = append(data, '\n')

	if err := writeOutput(*outFlag, data); err != nil {
		fmt.Fprintf(os.Stderr, "export: %v\n", err)
		os.Exit(2)
	}

	if *outFlag != "-" && *outFlag != "" {
		fmt.Fprintf(os.Stderr, "Exported %d nodes, %d relations → %s\n",
			payload.NodeCount, payload.RelationCount, *outFlag)
	}
}

// writeOutput writes data to the given path, or to stdout when path is "-" or empty.
func writeOutput(path string, data []byte) error {
	if path == "-" || path == "" {
		_, err := os.Stdout.Write(data)
		return err
	}
	return os.WriteFile(path, data, 0o644)
}
