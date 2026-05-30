package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"

	internalkh "github.com/mariogutierrez/context-harness-mcp/internal/khctl"
)

// revokeCmd implements the `khctl revoke` subcommand.
// It sets users.revoked_at = now() for a user matched by email or subject.
//
// Exit codes:
//
//	0 — user found and revoked
//	1 — user not found
//	2 — DB or configuration error
func revokeCmd(args []string) {
	fs := flag.NewFlagSet("revoke", flag.ExitOnError)
	dsn := fs.String("dsn", "", "Postgres DSN (defaults to $DATABASE_URL)")
	by := fs.String("by", "email", "Match field: email (default) or subject")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `Usage: khctl revoke <user> [--dsn URL] [--by email|subject]

Sets users.revoked_at = now() for the matching user.
<user> matches by email (default) or by external_subject (--by subject).

Exit codes:
  0  success (1 row updated)
  1  user not found
  2  DB or configuration error
`)
	}
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}

	if fs.NArg() < 1 {
		fs.Usage()
		os.Exit(2)
	}
	user := fs.Arg(0)

	runRevokeOp(user, *dsn, *by, false)
}

// reinstateCmd implements the `khctl reinstate` subcommand.
// It sets users.revoked_at = NULL for a user matched by email or subject.
func reinstateCmd(args []string) {
	fs := flag.NewFlagSet("reinstate", flag.ExitOnError)
	dsn := fs.String("dsn", "", "Postgres DSN (defaults to $DATABASE_URL)")
	by := fs.String("by", "email", "Match field: email (default) or subject")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `Usage: khctl reinstate <user> [--dsn URL] [--by email|subject]

Sets users.revoked_at = NULL for the matching user (un-revokes).
<user> matches by email (default) or by external_subject (--by subject).

Exit codes:
  0  success (1 row updated)
  1  user not found
  2  DB or configuration error
`)
	}
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}

	if fs.NArg() < 1 {
		fs.Usage()
		os.Exit(2)
	}
	user := fs.Arg(0)

	runRevokeOp(user, *dsn, *by, true)
}

// runRevokeOp is the shared implementation for revoke and reinstate.
func runRevokeOp(user, dsnFlag, by string, reinstate bool) {
	resolvedDSN, err := resolveDSN(dsnFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %s\n", err)
		os.Exit(2)
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, resolvedDSN)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: cannot connect to database: %s\n", err)
		os.Exit(2)
	}
	defer pool.Close()

	matcher := internalkh.MatchByEmail
	if by == "subject" {
		matcher = internalkh.MatchBySubject
	}

	var found bool
	var opErr error
	if reinstate {
		found, opErr = internalkh.Reinstate(ctx, pool, matcher, user)
	} else {
		found, opErr = internalkh.Revoke(ctx, pool, matcher, user)
	}

	if opErr != nil {
		fmt.Fprintf(os.Stderr, "error: DB operation failed: %s\n", opErr)
		os.Exit(2)
	}

	if !found {
		action := "revoke"
		if reinstate {
			action = "reinstate"
		}
		fmt.Fprintf(os.Stderr, "error: user not found for %s (by %s): %s\n", action, by, user)
		os.Exit(1)
	}

	action := "revoked"
	if reinstate {
		action = "reinstated"
	}
	fmt.Printf("user %s: %s\n", action, user)
}
