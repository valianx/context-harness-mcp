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

	mcpserver "github.com/mark3labs/mcp-go/server"

	internalmcp "github.com/mariogutierrez/context-harness-mcp/internal/mcp"
	"github.com/mariogutierrez/context-harness-mcp/internal/ratelimit"
	"github.com/mariogutierrez/context-harness-mcp/internal/store"
	"github.com/mariogutierrez/context-harness-mcp/internal/validate"
)

func main() {
	transport := flag.String("transport", "stdio", "MCP transport: stdio or http")
	addr := flag.String("addr", ":8080", "Listen address for http transport")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	if err := configureSecretMode(); err != nil {
		slog.Error("invalid SECRET_MODE value", "error", err)
		fmt.Fprintf(os.Stderr, "error: %s\n", err)
		os.Exit(1)
	}

	dsn := os.Getenv("SUPABASE_DB_URL")
	if dsn == "" {
		slog.Error("SUPABASE_DB_URL environment variable is required but not set")
		fmt.Fprintln(os.Stderr, "error: SUPABASE_DB_URL must be set — cannot start without DB access")
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
	// It is non-nil for both stdio and http transports — tool handlers skip
	// rate-limiting when the context carries no IP (stdio path).
	limiter := ratelimit.New()

	s := internalmcp.New(pool, limiter)

	switch *transport {
	case "stdio":
		runStdio(s)
	case "http":
		runHTTP(s, *addr, limiter)
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

func runHTTP(s *mcpserver.MCPServer, addr string, limiter *ratelimit.Limiter) {
	// WithHTTPContextFunc extracts the client IP from each incoming HTTP request
	// and injects it into the request context so tool handlers can read it for
	// rate-limit decisions without importing net/http directly.
	httpServer := mcpserver.NewStreamableHTTPServer(s,
		mcpserver.WithHTTPContextFunc(func(ctx context.Context, r *http.Request) context.Context {
			ip := ratelimit.ExtractClientIP(r)
			return ratelimit.ContextWithIP(ctx, ip)
		}),
	)

	mux := http.NewServeMux()
	// MCP streamable-http endpoint
	mux.Handle("/mcp", httpServer)
	// Plain HTTP health check consumed by Render and docker-compose healthchecks.
	// Intentionally returns "db":"not-configured" — a DB ping is not added here
	// per the anti-scope contract in PR-4.
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"status":"ok","db":"not-configured"}`)
	})

	srv := &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	slog.Info("starting MCP server", "transport", "http", "addr", addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		slog.Error("http server error", "error", err)
		os.Exit(1)
	}
}
