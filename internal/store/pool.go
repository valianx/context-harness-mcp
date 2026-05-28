// Package store provides the Postgres connection pool and type registrations
// required by the context-harness-mcp server.
package store

import (
	"context"
	"time"

	"github.com/exaring/otelpgx"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	pgxvec "github.com/pgvector/pgvector-go/pgx"
)

const (
	maxConns            = 10
	healthCheckPeriod   = 1 * time.Minute
	maxConnIdleTime     = 30 * time.Minute
	maxConnLifetime     = 1 * time.Hour
	connectTimeout      = 10 * time.Second
)

// New builds and returns a *pgxpool.Pool configured for the context-harness-mcp
// server. It registers the pgvector `vector` type in the AfterConnect hook so
// that every connection in the pool can decode vector columns without an
// explicit cast. Returns an error if the DSN is invalid or the pool cannot
// acquire its first connection.
func New(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}

	cfg.MaxConns = maxConns
	cfg.HealthCheckPeriod = healthCheckPeriod
	cfg.MaxConnIdleTime = maxConnIdleTime
	cfg.MaxConnLifetime = maxConnLifetime
	cfg.ConnConfig.ConnectTimeout = connectTimeout

	// Instrument every pgx query with an OTel child span. Parameters are
	// intentionally excluded (default) so that $1, $2 placeholders appear in
	// db.statement but the bound values never do — satisfying AC-D.2.
	//
	// pool.acquire and prepare * spans are dropped upstream by noiseFilterProcessor
	// in internal/observability/init.go — otelpgx v0.9.4 has no native option to
	// disable these span types, so the processor-level drop is the correct approach.
	cfg.ConnConfig.Tracer = otelpgx.NewTracer()

	// Register the pgvector `vector` type on every new connection so that
	// pgx can encode/decode vector(384) columns natively.
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		return pgxvec.RegisterTypes(ctx, conn)
	}

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}

	// Verify connectivity immediately so callers learn about a bad DSN at
	// startup rather than on the first query.
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}

	return pool, nil
}
