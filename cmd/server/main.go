// Command server is the context-harness-mcp entry point.
// It starts the MCP server in stdio or streamable-http transport mode.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/mariogutierrez/context-harness-mcp/internal/auth"
	"github.com/mariogutierrez/context-harness-mcp/internal/config"
	internalmcp "github.com/mariogutierrez/context-harness-mcp/internal/mcp"
	"github.com/mariogutierrez/context-harness-mcp/internal/ratelimit"
	"github.com/mariogutierrez/context-harness-mcp/internal/store"
	"github.com/mariogutierrez/context-harness-mcp/internal/validate"
	"github.com/mariogutierrez/context-harness-mcp/internal/viewer"
)

// envOrDefault returns the value of env var key when non-empty, otherwise
// the fallback. Used to seed flag defaults so CLI flags > env var > hard
// default precedence holds. Without this, the binary ignored MCP_TRANSPORT
// and MCP_HTTP_ADDR set in hosting platforms (Render, Railway, etc.) — the
// flag defaults always won, forcing operators to override the Dockerfile
// CMD just to change the transport.
func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	transport := flag.String("transport", envOrDefault("MCP_TRANSPORT", "stdio"),
		"MCP transport: stdio or http (env: MCP_TRANSPORT)")
	// Fallback chain: MCP_HTTP_ADDR > PORT (Railway / Heroku convention) > :7654.
	// Railway sets PORT to its assigned port; honoring it lets the app deploy
	// without an explicit Target Port override in the Railway UI.
	rawAddr := envOrDefault("MCP_HTTP_ADDR", "")
	if rawAddr == "" {
		if port := os.Getenv("PORT"); port != "" {
			rawAddr = ":" + port
		} else {
			rawAddr = ":7654"
		}
	}
	addr := flag.String("addr", rawAddr,
		"Listen address for http transport (env: MCP_HTTP_ADDR, or PORT for hosting platforms)")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	if err := configureSecretMode(); err != nil {
		slog.Error("invalid SECRET_MODE value", "error", err)
		fmt.Fprintf(os.Stderr, "error: %s\n", err)
		os.Exit(1)
	}

	// Parse MCP_AUTH before doing any other work so a misconfigured value
	// fails fast at boot with a clear message (AC-10).
	authMode, err := auth.ParseMode()
	if err != nil {
		slog.Error("invalid MCP_AUTH value", "error", err)
		fmt.Fprintf(os.Stderr, "error: %s\n", err)
		os.Exit(1)
	}

	// Fail fast when MCP_AUTH=enabled but MCP_JWT_SECRET is absent.
	// Never allow the server to boot in auth-enabled mode without a secret.
	if authMode == auth.ModeEnabled {
		if os.Getenv("MCP_JWT_SECRET") == "" {
			slog.Error("MCP_JWT_SECRET must be set when MCP_AUTH=enabled")
			fmt.Fprintln(os.Stderr, "error: MCP_JWT_SECRET is required when MCP_AUTH=enabled — generate a 32+ byte hex secret and set it")
			os.Exit(1)
		}
	}

	dsn := config.ResolveDatabaseURL()
	if dsn == "" {
		slog.Error("DATABASE_URL environment variable is required but not set")
		fmt.Fprintln(os.Stderr, "error: DATABASE_URL must be set — cannot start without DB access")
		os.Exit(1)
	}

	ctx := context.Background()
	pool, err := store.New(ctx, dsn)
	if err != nil {
		slog.Error("failed to open database pool", "error", err)
		fmt.Fprintf(os.Stderr, "error: cannot connect to database: %s\n", err)
		os.Exit(1)
	}
	defer pool.Close()

	// The rate limiter is shared across all write-tool registrations.
	// It is non-nil for both stdio and http transports.
	limiter := ratelimit.New()

	// Initialize the process-wide stdio token bucket from MCP_STDIO_RATE_LIMIT.
	// Must happen before any tool handler runs so the bucket is ready on first call.
	ratelimit.InitStdio()

	s := internalmcp.New(pool, limiter)

	switch *transport {
	case "stdio":
		runStdio(s)
	case "http":
		// Emit a soft-guard warning when running HTTP with auth disabled against
		// a remote DB (R10 — makes it impossible to miss auth being off in prod).
		auth.WarnIfDisabled(authMode, "http", dsn)
		runHTTP(s, *addr, pool, limiter, authMode)
	default:
		slog.Error("unknown transport", "transport", *transport)
		fmt.Fprintf(os.Stderr, "error: unknown transport %q — use stdio or http\n", *transport)
		os.Exit(1)
	}
}

func runStdio(s *mcpserver.MCPServer) {
	slog.Info("starting MCP server", "transport", "stdio")
	if err := mcpserver.ServeStdio(s); err != nil {
		slog.Error("stdio server error", "error", err)
		os.Exit(1)
	}
}

// configureSecretMode reads SECRET_MODE from the environment and configures the
// validate package accordingly. Valid values are "reject" (default) and
// "redact". Any other non-empty value is rejected with an error so a typo
// (e.g. "rdeact") surfaces immediately at startup rather than silently falling
// back to reject mode.
func configureSecretMode() error {
	raw := os.Getenv("SECRET_MODE")
	switch raw {
	case "", "reject":
		validate.SetSecretMode(validate.SecretModeReject)
		slog.Info("secret mode configured", "mode", "reject")
	case "redact":
		validate.SetSecretMode(validate.SecretModeRedact)
		slog.Info("secret mode configured", "mode", "redact")
	default:
		return fmt.Errorf("SECRET_MODE=%q is not a valid value — use \"reject\" or \"redact\"", raw)
	}
	return nil
}

// revocationStoreAdapter wraps *pgxpool.Pool to satisfy auth.RevocationStore.
// It queries public.users for the revoked_at column using a parameterized query.
type revocationStoreAdapter struct {
	pool *pgxpool.Pool
}

// GetRevoked returns true when the user identified by sub has a non-null
// revoked_at timestamp in the public.users table.
func (a *revocationStoreAdapter) GetRevoked(sub string) (bool, error) {
	var revoked bool
	err := a.pool.QueryRow(
		context.Background(),
		"SELECT revoked_at IS NOT NULL FROM users WHERE supabase_user_id = $1",
		sub,
	).Scan(&revoked)
	if err != nil {
		// User not found in local users table → treat as not revoked.
		// This can happen for valid tokens issued before the user row was created,
		// which should not occur post-PR-3, but we fail open to avoid DOS.
		return false, nil
	}
	return revoked, nil
}

func runHTTP(s *mcpserver.MCPServer, addr string, pool *pgxpool.Pool, limiter *ratelimit.Limiter, authMode auth.Mode) {
	// WithHTTPContextFunc extracts the client IP from each incoming HTTP request
	// and injects it into the request context so tool handlers can read it for
	// rate-limit decisions without importing net/http directly.
	httpServer := mcpserver.NewStreamableHTTPServer(s,
		mcpserver.WithHTTPContextFunc(func(ctx context.Context, r *http.Request) context.Context {
			ip := ratelimit.ExtractClientIP(r)
			ctx = ratelimit.ContextWithIP(ctx, ip)
			// Propagate authenticated sub into the MCP tool context so the
			// rate limiter can key by sub rather than IP when auth is enabled.
			if sub := auth.UserIDFromContext(r.Context()); sub != "" {
				ctx = ratelimit.ContextWithSub(ctx, sub)
			}
			return ctx
		}),
	)

	revocationCache := auth.NewRevocationCache()
	revStore := &revocationStoreAdapter{pool: pool}
	baseURL := os.Getenv("MCP_PUBLIC_URL")

	mux := http.NewServeMux()
	// /mcp is wrapped by auth.Middleware — when ModeNone it's a no-op pass-through.
	// Ordering: auth.Middleware → httpServer (MCP handler → Content Filter → DB write).
	mux.Handle("/mcp", auth.Middleware(authMode, revStore, revocationCache, baseURL, httpServer))
	// Plain HTTP health check consumed by Render and docker-compose healthchecks.
	// Intentionally returns "db":"not-configured" — a DB ping is not added here
	// per the anti-scope contract in PR-4.
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"status":"ok","db":"not-configured"}`)
	})
	viewer.Register(mux, pool)

	srv := &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	slog.Info("starting MCP server", "transport", "http", "addr", addr, "auth_mode", authMode)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		slog.Error("http server error", "error", err)
		os.Exit(1)
	}
}
