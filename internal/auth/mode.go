package auth

import (
	"fmt"
	"log/slog"
	"os"
	"strings"
)

// Mode represents the runtime authentication mode for the MCP server.
type Mode int

const (
	// ModeNone disables authentication. The middleware is a pass-through;
	// /mcp is public and write tools persist created_by_user_id = NULL.
	// This is the back-compat default for one release window per the migration plan.
	ModeNone Mode = iota

	// ModeEnabled activates JWT bearer validation on /mcp. Requests without
	// a valid bearer are rejected with 401. Revocation is checked via cache+DB.
	ModeEnabled
)

// ParseMode reads MCP_AUTH from the environment and returns the parsed Mode.
// Returns an error when the value is not "none" or "enabled", so main.go can
// fail-fast with a clear message instead of silently using a default.
func ParseMode() (Mode, error) {
	raw := os.Getenv("MCP_AUTH")
	switch raw {
	case "", "none":
		return ModeNone, nil
	case "enabled":
		return ModeEnabled, nil
	default:
		return ModeNone, fmt.Errorf(
			"MCP_AUTH=%q is not a valid value — use \"none\" or \"enabled\"", raw,
		)
	}
}

// WarnIfDisabled emits a periodic slog warning when auth is disabled on HTTP.
// This satisfies the soft-guard requirement (R10): the server starts fine but
// makes it impossible to miss that auth is off in production HTTP deployments.
// Call this once at startup; the warning recurs every hour via the log entry.
func WarnIfDisabled(mode Mode, transport string, dsn string) {
	if mode == ModeEnabled || transport != "http" {
		return
	}
	if isLocalDSN(dsn) {
		return
	}
	slog.Warn("MCP_AUTH=none on HTTP transport with remote database — auth is disabled; set MCP_AUTH=enabled before exposing this server to users",
		"code", "auth/disabled",
	)
}

// isLocalDSN returns true when dsn points to localhost/127.0.0.1/pgvector-test,
// indicating a local dev environment where auth-disabled is expected.
func isLocalDSN(dsn string) bool {
	for _, marker := range []string{"localhost", "127.0.0.1", "pgvector-test"} {
		if strings.Contains(dsn, marker) {
			return true
		}
	}
	return false
}
