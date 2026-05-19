package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/mariogutierrez/context-harness-mcp/internal/khctl"
	"github.com/mariogutierrez/context-harness-mcp/internal/store"
)

// importCmd implements the `khctl import` subcommand.
func importCmd(args []string) {
	fs := flag.NewFlagSet("import", flag.ExitOnError)
	dsnFlag := fs.String("dsn", "", "Postgres DSN (defaults to $DATABASE_URL)")
	_ = fs.Parse(args)

	// Positional arg: input file path. "-" means stdin; default to stdin when absent.
	inputPath := "-"
	if fs.NArg() > 0 {
		inputPath = fs.Arg(0)
	}

	dsn, err := resolveDSN(*dsnFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "import: %v\n", err)
		os.Exit(1)
	}

	data, err := khctl.ReadInput(inputPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "import: %v\n", err)
		os.Exit(1)
	}

	nodes, relations, err := khctl.ParseImportPayload(data)
	if err != nil {
		fmt.Fprintf(os.Stderr, "import: %v\n", err)
		os.Exit(1)
	}

	ctx := getContext()
	pool, err := store.New(ctx, dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "import: cannot connect to database: %v\n", err)
		os.Exit(2)
	}
	defer pool.Close()

	nodesIn, obsIn, obsDedup, relsIn, relsDedup, err := khctl.RunImport(ctx, pool, nodes, relations)
	if err != nil {
		fmt.Fprintf(os.Stderr, "import: %v\n", err)
		os.Exit(2)
	}

	nodeDedup := len(nodes) - nodesIn
	fmt.Printf("imported nodes=%d observations=%d relations=%d"+
		" (deduped: nodes=%d observations=%d relations=%d)\n",
		nodesIn, obsIn, relsIn,
		nodeDedup, obsDedup, relsDedup)
}
