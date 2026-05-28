// Command server is the context-harness-mcp entry point.
// It starts the MCP server in stdio or streamable-http transport mode.
package main

import (
	"context"
	"encoding/json"
	"expvar"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/mariogutierrez/context-harness-mcp/internal/auth"
	"github.com/mariogutierrez/context-harness-mcp/internal/config"
	"github.com/mariogutierrez/context-harness-mcp/internal/embed"
	"github.com/mariogutierrez/context-harness-mcp/internal/healthz"
	internalmcp "github.com/mariogutierrez/context-harness-mcp/internal/mcp"
	"github.com/mariogutierrez/context-harness-mcp/internal/metrics"
	"github.com/mariogutierrez/context-harness-mcp/internal/observability"
	"github.com/mariogutierrez/context-harness-mcp/internal/ratelimit"
	"github.com/mariogutierrez/context-harness-mcp/internal/store"
	"github.com/mariogutierrez/context-harness-mcp/internal/validate"
	"github.com/mariogutierrez/context-harness-mcp/internal/viewer"
	"github.com/mariogutierrez/context-harness-mcp/internal/web"
)

// envOrDefault returns the value of env var key when non-empty, otherwise
// the fallback. Used to seed the -transport flag default so MCP_TRANSPORT
// set by container hosting providers wins over the stdio default without
// requiring operators to override the Dockerfile CMD.
func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	transport := flag.String("transport", envOrDefault("MCP_TRANSPORT", "stdio"),
		"MCP transport: stdio or http (env: MCP_TRANSPORT)")
	// Address resolution: CLI -addr flag > PORT env > :7654 hard default.
	// PORT is the universal PaaS convention (Railway / Heroku / Fly / Render
	// all set it dynamically and route their healthcheck to the same port).
	// Local dev sets -addr or relies on the :7654 default.
	//
	// There is NO separate MCP_HTTP_ADDR env var. The previous version had
	// one and its higher priority over PORT was a footgun: operators who
	// set both got the MCP_HTTP_ADDR value silently, even though Railway's
	// healthcheck went to PORT, producing service-unavailable on deploy.
	// PaaS users: leave PORT alone, the platform sets it. Local users: use
	// -addr.
	rawAddr := ""
	if port := os.Getenv("PORT"); port != "" {
		rawAddr = ":" + port
	} else {
		rawAddr = ":7654"
	}
	addr := flag.String("addr", rawAddr,
		"Listen address for http transport (env: PORT — set automatically by Railway / Heroku / Fly / Render)")
	flag.Parse()

	// Bootstrap stdout logger first so boot errors are always visible.
	stdoutHandler := slog.NewJSONHandler(os.Stdout, nil)
	slog.SetDefault(slog.New(stdoutHandler))

	// Stdio transport never sends telemetry — apply the kill-switch before
	// observability.Init so the SDK cannot open an OTLP connection in stdio
	// mode regardless of what CH_OBSERVABILITY_ENABLED / AXIOM_TOKEN are set to.
	applyStdioGuard(*transport)

	ctx := context.Background()

	obsShutdown, err := observability.Init(ctx)
	if err != nil {
		slog.Error("observability init failed", "error", err)
		fmt.Fprintf(os.Stderr, "error: observability init: %s\n", err)
		os.Exit(1)
	}

	// If a real LoggerProvider was initialised, replace the default logger with
	// the bridge handler so every slog call also emits to the OTel backend.
	// Stdout JSON output is UNCHANGED — the bridge wraps the same stdoutHandler.
	// When observability is disabled LoggerProvider() returns nil and this is a
	// no-op, preserving the existing operator-visible log format (AC-B.1).
	if lp := observability.LoggerProvider(); lp != nil {
		bridged := observability.NewBridgedSlogHandler(stdoutHandler, lp)
		slog.SetDefault(slog.New(bridged))
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if shutdownErr := obsShutdown(shutdownCtx); shutdownErr != nil {
			slog.Warn("observability shutdown error", "error", shutdownErr)
		}
	}()

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

	ctx = context.Background()
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

// applyStdioGuard sets OTEL_SDK_DISABLED=true when transport is "stdio",
// ensuring no OTLP connection is ever opened in stdio mode.  Must be called
// before observability.Init — the ordering is enforced structurally in main().
//
// For any transport other than "stdio" this function is a no-op, leaving the
// env var unchanged so HTTP mode can still activate full observability.
func applyStdioGuard(transport string) {
	if transport == "stdio" {
		os.Setenv("OTEL_SDK_DISABLED", "true") //nolint:errcheck
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

	// Unauthenticated routes — registered BEFORE /mcp so they are never gated
	// by auth.Middleware. Order matters: these must be reachable without a
	// bearer token (login flow + HMAC-verified webhook).
	web.RegisterCallback(mux)
	web.RegisterExchange(mux, pool)
	web.RegisterLogin(mux)
	// Webhook is HMAC-verified inside the handler; no bearer token required.
	// The same revocationCache used by auth.Middleware is passed here so that
	// a webhook event immediately invalidates the cached revocation state,
	// reducing effective revocation latency from 1h (TTL) to ~1s.
	web.RegisterWebhook(mux, pool, revocationCache)
	// Session-authenticated routes: /dashboard and /auth/logout.
	// These use ch_session cookie auth, not bearer JWT — they sit outside the
	// auth.Middleware wrap on /mcp deliberately.
	web.RegisterDashboard(mux, pool)
	web.RegisterLogout(mux)
	// Landing page at GET / — fully static, presents both products in the
	// agent stack (team-harness + context-harness-mcp). Registered LAST so
	// it doesn't shadow any earlier exact route; mux's longest-prefix match
	// means /mcp, /auth/*, /healthz, /viewer/* are all hit before "/".
	web.RegisterLanding(mux)

	// /mcp is wrapped by auth.Middleware — when ModeNone it's a no-op pass-through.
	// Ordering: auth.Middleware → httpServer (MCP handler → Content Filter → DB write).
	// Register both exact "/mcp" and subtree "/mcp/" — Go's ServeMux distinguishes
	// trailing-slash paths and Claude Code's MCP HTTP transport calls "/mcp/".
	mcpHandler := auth.Middleware(authMode, revStore, revocationCache, baseURL, httpServer)
	mux.Handle("/mcp", mcpHandler)
	mux.Handle("/mcp/", mcpHandler)
	// Plain HTTP health check consumed by container-host healthchecks (any platform).
	// Returns HTTP 200 when all checks pass, HTTP 503 when any check fails (degraded).
	// Container hosts (Railway, Render, Fly, Docker compose) interpret 5xx as unhealthy.
	// Body shape is identical to the "doctor" MCP tool response.
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		report := healthz.Run(r.Context(), pool)
		status := http.StatusOK
		if report.Degraded {
			status = http.StatusServiceUnavailable
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(report)
	})
	viewer.Register(mux, pool)

	// /debug/vars exposes the in-memory expvar counters (e.g., webhook metrics).
	// Disabled by default; enabled when MCP_EXPOSE_EXPVAR=1.
	// Never expose this on public-facing servers without additional auth.
	if os.Getenv("MCP_EXPOSE_EXPVAR") == "1" {
		mux.Handle("/debug/vars", expvar.Handler())
		slog.Info("expvar endpoint enabled", "path", "/debug/vars")
	}

	// /metrics exposes Prometheus-format metrics for scraping by a Prometheus
	// server. Disabled by default; enabled when MCP_EXPOSE_METRICS=1.
	// Gate at the network layer (firewall/internal-only) — no bearer auth here
	// per Prometheus convention. The env var is the safety net.
	if os.Getenv("MCP_EXPOSE_METRICS") == "1" {
		mux.Handle("/metrics", metrics.Handler())
		slog.Info("metrics endpoint enabled", "path", "/metrics")
	}

	srv := &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	// Pre-warm the ONNX embedder before accepting traffic so the first
	// /healthz probe (and the first real write request) sees a warm session
	// rather than paying the ~300-500 ms sync.Once cold-start. Platform
	// healthchecks (Railway, Render, Fly) probe within seconds of boot;
	// without this, the embedder check used to fail intermittently on the
	// very first attempt. Errors here are logged but non-fatal — the server
	// still boots; the embedder check itself will surface any real failure
	// via /healthz on the next probe.
	warmCtx, warmCancel := context.WithTimeout(context.Background(), 10*time.Second)
	warmStart := time.Now()
	if _, err := embed.Default().Encode(warmCtx, []string{"_warmup"}); err != nil {
		slog.Warn("embedder warmup failed (non-fatal)", "error", err, "duration_ms", time.Since(warmStart).Milliseconds())
	} else {
		slog.Info("embedder warmup complete", "duration_ms", time.Since(warmStart).Milliseconds())
	}
	warmCancel()

	slog.Info("starting MCP server", "transport", "http", "addr", addr, "auth_mode", authMode)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		slog.Error("http server error", "error", err)
		os.Exit(1)
	}
}
