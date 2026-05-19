package main

import (
	"errors"
	"os"
)

// resolveDSN returns the DSN from the --dsn flag value or the SUPABASE_DB_URL
// environment variable. Returns an error when neither is set.
func resolveDSN(flagVal string) (string, error) {
	if flagVal != "" {
		return flagVal, nil
	}
	if env := os.Getenv("SUPABASE_DB_URL"); env != "" {
		return env, nil
	}
	return "", errors.New("--dsn is required or set SUPABASE_DB_URL")
}
