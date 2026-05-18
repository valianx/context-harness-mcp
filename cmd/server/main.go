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
	"github.com/mariogutierrez/context-harness-mcp/internal/store"
)

func main() {
	transport := flag.String("transport", "stdio", "MCP transport: stdio or http")
	addr := flag.String("addr", ":8080", "Listen address for http transport")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

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

	s := internalmcp.New(pool)

	switch *transport {
	case "stdio":
		runStdio(s)
	case "http":
		runHTTP(s, *addr)
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

func runHTTP(s *mcpserver.MCPServer, addr string) {
	httpServer := mcpserver.NewStreamableHTTPServer(s)

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
