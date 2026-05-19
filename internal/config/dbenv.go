// Package config provides environment-variable resolution helpers shared
// across server and operator (khctl) entry points.
package config

import (
	"fmt"
	"os"
)

// ResolveDatabaseURL reads the database connection string from the environment.
// It prefers DATABASE_URL (industry-standard name, auto-populated by Railway,
// Render, Fly, and Heroku when they provision Postgres). For one release it
// also falls back to SUPABASE_DB_URL with a deprecation warning on stderr so
// operators who have not yet renamed their secret keep working without
// interruption.
//
// Returns the empty string when neither variable is set. Callers are
// responsible for treating an empty return value as a fatal startup error.
func ResolveDatabaseURL() string {
	if v := os.Getenv("DATABASE_URL"); v != "" {
		return v
	}
	if v := os.Getenv("SUPABASE_DB_URL"); v != "" {
		fmt.Fprintln(os.Stderr, "warning: SUPABASE_DB_URL is deprecated; rename to DATABASE_URL (legacy name will be removed in v2.0)")
		return v
	}
	return ""
}
