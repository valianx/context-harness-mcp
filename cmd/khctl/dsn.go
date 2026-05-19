package main

import (
	"errors"

	"github.com/mariogutierrez/context-harness-mcp/internal/config"
)

// resolveDSN returns the DSN from the --dsn flag value or the DATABASE_URL
// environment variable (with a backward-compat fallback to SUPABASE_DB_URL for
// one release). Returns an error when neither is set.
func resolveDSN(flagVal string) (string, error) {
	if flagVal != "" {
		return flagVal, nil
	}
	if dsn := config.ResolveDatabaseURL(); dsn != "" {
		return dsn, nil
	}
	return "", errors.New("--dsn is required or set DATABASE_URL")
}
