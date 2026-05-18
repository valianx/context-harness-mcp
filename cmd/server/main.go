// Command server is the context-harness-mcp entry point.
// It starts the MCP server in stdio or streamable-http transport mode.
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"

	mcpserver "github.com/mark3labs/mcp-go/server"

	internalmcp "github.com/mariogutierrez/context-harness-mcp/internal/mcp"
)

func main() {
	transport := flag.String("transport", "stdio", "MCP transport: stdio or http")
	addr := flag.String("addr", ":8080", "Listen address for http transport")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	s := internalmcp.New()

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
	// Plain HTTP health check consumed by Render and docker-compose healthchecks
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
