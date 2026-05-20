package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// backupCmd implements the `khctl backup` subcommand.
//
// It shells out to pg_dump in custom format (--format=custom) with compression
// level 9, schema-only restricted to `public`, no-owner / no-privileges so the
// dump restores cleanly into a different DB role.
//
// pg_dump must be available on PATH. The binary is part of the postgresql-client
// debian package; operators running khctl outside the official Docker image must
// install it themselves.
//
// Output: custom-format archive that pg_restore reads. This is denser than
// plain SQL and preserves index/constraint ordering for safe restore.
func backupCmd(args []string) {
	fs := flag.NewFlagSet("backup", flag.ExitOnError)
	dsnFlag := fs.String("dsn", "", "Postgres DSN (defaults to $DATABASE_URL)")
	outFlag := fs.String("out", "", "Output file path (required — pg_dump custom format is binary)")
	compressFlag := fs.Int("compress", 9, "Compression level (0-9; 9 = max)")
	_ = fs.Parse(args)

	if *outFlag == "" || *outFlag == "-" {
		fmt.Fprintln(os.Stderr, "backup: --out is required (pg_dump custom format is binary, stdout is not supported)")
		os.Exit(1)
	}

	dsn, err := resolveDSN(*dsnFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "backup: %v\n", err)
		os.Exit(1)
	}

	if _, err := exec.LookPath("pg_dump"); err != nil {
		fmt.Fprintln(os.Stderr, "backup: pg_dump not found on PATH — install postgresql-client")
		os.Exit(1)
	}

	cmd := exec.Command("pg_dump",
		"--format=custom",
		fmt.Sprintf("--compress=%d", *compressFlag),
		"--schema=public",
		"--no-owner",
		"--no-privileges",
		"--file="+*outFlag,
		dsn,
	)
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "backup: pg_dump failed: %v\n", err)
		os.Exit(2)
	}

	info, err := os.Stat(*outFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "backup: stat output: %v\n", err)
		os.Exit(2)
	}
	fmt.Fprintf(os.Stderr, "Backup complete: %s (%s)\n", *outFlag, humanBytes(info.Size()))
}

// restoreCmd implements the `khctl restore` subcommand.
//
// It shells out to pg_restore with the custom-format archive produced by
// `khctl backup`. By default the restore is additive (will fail on
// pre-existing tables). With --clean, pg_restore drops the existing schema
// objects before restoring.
//
// pg_restore must be available on PATH.
//
// SAFETY: --clean is destructive. The handler requires the operator to
// either pass --yes or answer interactively on stdin. CI / scripted runs
// must pass --yes explicitly.
func restoreCmd(args []string) {
	fs := flag.NewFlagSet("restore", flag.ExitOnError)
	dsnFlag := fs.String("dsn", "", "Postgres DSN (defaults to $DATABASE_URL)")
	cleanFlag := fs.Bool("clean", false, "Drop existing schema objects before restore (destructive)")
	yesFlag := fs.Bool("yes", false, "Skip interactive confirmation (required with --clean in non-interactive contexts)")
	_ = fs.Parse(args)

	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "restore: exactly one input file path is required")
		fmt.Fprintln(os.Stderr, "usage: khctl restore <FILE> [--dsn URL] [--clean] [--yes]")
		os.Exit(1)
	}
	input := fs.Arg(0)

	if _, err := os.Stat(input); err != nil {
		fmt.Fprintf(os.Stderr, "restore: input file: %v\n", err)
		os.Exit(1)
	}

	dsn, err := resolveDSN(*dsnFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "restore: %v\n", err)
		os.Exit(1)
	}

	if _, err := exec.LookPath("pg_restore"); err != nil {
		fmt.Fprintln(os.Stderr, "restore: pg_restore not found on PATH — install postgresql-client")
		os.Exit(1)
	}

	if *cleanFlag && !*yesFlag {
		if !confirmInteractive("--clean will DROP existing tables in the target DB. Continue?") {
			fmt.Fprintln(os.Stderr, "restore: aborted by operator")
			os.Exit(1)
		}
	}

	restoreArgs := []string{
		"--no-owner",
		"--no-privileges",
		"--exit-on-error",
	}
	if *cleanFlag {
		restoreArgs = append(restoreArgs, "--clean", "--if-exists")
	}
	restoreArgs = append(restoreArgs, "--dbname="+dsn, input)

	cmd := exec.Command("pg_restore", restoreArgs...)
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "restore: pg_restore failed: %v\n", err)
		os.Exit(2)
	}

	fmt.Fprintf(os.Stderr, "Restore complete: %s\n", input)
}

// confirmInteractive prompts the operator on stderr and reads y/N from stdin.
// Returns true only on an explicit "y" or "yes" (case-insensitive).
func confirmInteractive(prompt string) bool {
	fmt.Fprintf(os.Stderr, "%s [y/N]: ", prompt)
	var resp string
	if _, err := fmt.Fscanln(os.Stdin, &resp); err != nil {
		return false
	}
	resp = strings.ToLower(strings.TrimSpace(resp))
	return resp == "y" || resp == "yes"
}

// humanBytes formats a byte count as KB / MB / GB with one decimal place.
func humanBytes(n int64) string {
	const (
		kb = 1024
		mb = kb * 1024
		gb = mb * 1024
	)
	switch {
	case n >= gb:
		return fmt.Sprintf("%.1f GB", float64(n)/gb)
	case n >= mb:
		return fmt.Sprintf("%.1f MB", float64(n)/mb)
	case n >= kb:
		return fmt.Sprintf("%.1f KB", float64(n)/kb)
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// Compile-time guard to keep "io" import live in case future work adds streaming
// (e.g. pg_dump to stdout for piping); harmless if unused for now.
var _ = io.Copy
