// Package store_test validates that the pgxpool is correctly instrumented with
// otelpgx, satisfying the three observability requirements for PR-D:
//
//   - AC-D.1: every SQL query produces a child span with db.system=postgresql
//     and db.statement=<parameterized SQL>, parented to the active HTTP span.
//   - AC-D.2: query parameter values are NOT included in db.statement; only
//     the $1, $2 placeholders appear.
//   - AC-D.3: SQL errors mark the span with status=Error; the error message
//     does not contain query parameter values.
//
// Tests require a running Docker daemon. If Docker is unavailable the suite
// skips gracefully (exit 0) following the same pattern as tests/setup_test.go.
package store_test

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"

	"github.com/mariogutierrez/context-harness-mcp/internal/store"
)

const (
	otelpgxTestDBName     = "otelpgx_test"
	otelpgxTestDBUser     = "test"
	otelpgxTestDBPassword = "test"
	otelpgxPGImage        = "pgvector/pgvector:pg16"
	// otelpgxMigrationsDir is relative to the repo root (working dir for go test ./...).
	otelpgxMigrationsDir = "../../migrations"
)

var testDSN string

// TestMain manages the ephemeral container lifecycle for Docker-dependent tests.
// If Docker is not available, testDSN stays empty and newTracedPool skips those
// tests individually — allowing Docker-free tests (e.g. TestCollapsedSQL_AC_S2
// in collapsed_sql_test.go) to run normally in any environment.
func TestMain(m *testing.M) {
	os.Exit(runSuite(m))
}

func runSuite(m *testing.M) int {
	ctx := context.Background()

	if err := checkDockerAvailable(ctx); err != nil {
		slog.Info("Docker daemon not available — Docker-dependent tests will be skipped individually",
			"reason", err.Error())
		// Run remaining (non-Docker) tests instead of bailing out entirely.
		return m.Run()
	}

	pgCtr, err := tcpostgres.Run(ctx,
		otelpgxPGImage,
		tcpostgres.WithDatabase(otelpgxTestDBName),
		tcpostgres.WithUsername(otelpgxTestDBUser),
		tcpostgres.WithPassword(otelpgxTestDBPassword),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second),
		),
	)
	if err != nil {
		slog.Error("failed to start postgres container", "error", err)
		return 1
	}
	defer func() {
		if termErr := testcontainers.TerminateContainer(pgCtr); termErr != nil {
			slog.Warn("failed to terminate postgres container", "error", termErr)
		}
	}()

	dsn, err := pgCtr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		slog.Error("failed to get postgres connection string", "error", err)
		return 1
	}

	if err := applyMigrations(dsn); err != nil {
		slog.Error("migration failed", "error", err)
		return 1
	}

	testDSN = dsn
	return m.Run()
}

func applyMigrations(dsn string) error {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return fmt.Errorf("sql.Open: %w", err)
	}
	defer db.Close()

	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("goose.SetDialect: %w", err)
	}
	if err := goose.Up(db, otelpgxMigrationsDir); err != nil {
		return fmt.Errorf("goose.Up: %w", err)
	}
	return nil
}

// checkDockerAvailable probes the Docker daemon and returns a non-nil error
// when the daemon is unreachable. Uses recover to handle the panic that
// testcontainers emits on Windows when the Docker socket path is not found
// (testcontainers v0.35 calls MustExtractDockerHost which panics instead of
// returning an error on unsupported configurations).
func checkDockerAvailable(ctx context.Context) (dockerErr error) {
	defer func() {
		if r := recover(); r != nil {
			dockerErr = fmt.Errorf("testcontainers panic: %v", r)
		}
	}()

	provider, err := testcontainers.ProviderDocker.GetProvider()
	if err != nil {
		return err
	}
	return provider.Health(ctx)
}

// newTracedPool builds a pgxpool backed by an in-memory span recorder and
// returns the pool, the shared TracerProvider, the recorder, and a cleanup
// func the caller must defer.
//
// otelpgx.NewTracer() captures the OTel global TracerProvider at construction
// time. We install our in-memory provider as the global BEFORE creating the
// pool, so every span produced by the pool — and every parent span created by
// tests using the same provider — land in the same recorder.
func newTracedPool(t *testing.T) (*pgxpool.Pool, *sdktrace.TracerProvider, *tracetest.SpanRecorder, func()) {
	t.Helper()

	if testDSN == "" {
		t.Skip("Docker not available")
	}

	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))

	// Swap the global provider so otelpgx.NewTracer() inside store.New picks
	// up our recorder. Restore the previous provider on cleanup.
	prevTP := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)

	pool, err := store.New(context.Background(), testDSN)
	require.NoError(t, err, "store.New should succeed against the test container")

	return pool, tp, rec, func() {
		pool.Close()
		_ = tp.Shutdown(context.Background())
		otel.SetTracerProvider(prevTP)
	}
}

// ---------------------------------------------------------------------------
// AC-D.1 — each query produces a child span with db.system=postgresql,
//           db.statement containing the parameterized SQL, and the span is
//           parented to the active HTTP span.
// ---------------------------------------------------------------------------

func TestOtelpgx_AC_D1_QueryProducesSpanWithDBSystemAndStatement(t *testing.T) {
	pool, tp, rec, cleanup := newTracedPool(t)
	defer cleanup()

	// Build a root "HTTP" parent span using the same provider that was
	// installed as global — so pool spans and parent spans share the recorder.
	ctx, parentSpan := tp.Tracer("test").Start(context.Background(), "http.request")
	defer parentSpan.End()

	// Run a parameterized query. SELECT count(*) always returns a row, so no
	// ErrNoRows; we only care that a span was emitted.
	var count int
	_ = pool.QueryRow(ctx, "SELECT count(*) FROM nodes WHERE name = $1", "test-node").Scan(&count)

	spans := rec.Ended()
	querySpan := findSpanContaining(spans, "SELECT")
	require.NotNil(t, querySpan,
		"expected a span whose name contains 'SELECT'; recorded spans: %v", spanNames(spans))

	// db.system = postgresql
	assertAttrString(t, querySpan, semconv.DBSystemKey, "postgresql")

	// db.statement contains the SQL text with the $1 placeholder.
	stmt := attrString(querySpan, semconv.DBQueryTextKey)
	assert.Contains(t, stmt, "SELECT", "db.statement should contain SELECT keyword")
	assert.Contains(t, stmt, "$1", "db.statement should contain the $1 placeholder")

	// The query span shares the trace ID of the parent HTTP span — confirming
	// parent-child relationship across the trace context propagation.
	assert.Equal(t,
		parentSpan.SpanContext().TraceID(),
		querySpan.SpanContext().TraceID(),
		"query span must share the trace ID of the HTTP parent span",
	)
	assert.Equal(t,
		parentSpan.SpanContext().SpanID(),
		querySpan.Parent().SpanID(),
		"query span's parent must be the HTTP span",
	)
}

// ---------------------------------------------------------------------------
// AC-D.2 — parameter VALUES are not included in db.statement or any attribute.
// ---------------------------------------------------------------------------

func TestOtelpgx_AC_D2_QueryParameterValuesAbsentFromSpan(t *testing.T) {
	pool, tp, rec, cleanup := newTracedPool(t)
	defer cleanup()

	ctx, parentSpan := tp.Tracer("test").Start(context.Background(), "http.request")
	defer parentSpan.End()

	secretValue := "super-secret-parameter-value-must-not-appear"
	var count int
	_ = pool.QueryRow(ctx,
		"SELECT count(*) FROM nodes WHERE name = $1",
		secretValue,
	).Scan(&count)

	spans := rec.Ended()
	querySpan := findSpanContaining(spans, "SELECT")
	require.NotNil(t, querySpan,
		"expected a SELECT span; recorded spans: %v", spanNames(spans))

	// No string attribute on the span may contain the parameter value.
	for _, attr := range querySpan.Attributes() {
		if attr.Value.Type() == attribute.STRING {
			assert.NotContains(t, attr.Value.AsString(), secretValue,
				"parameter value must not appear in span attribute %q", attr.Key)
		}
	}

	// The pgx.query.parameters attribute must be absent — otelpgx.NewTracer()
	// does not enable WithIncludeQueryParameters, so values stay out of spans.
	for _, attr := range querySpan.Attributes() {
		assert.NotEqual(t, "pgx.query.parameters", string(attr.Key),
			"pgx.query.parameters must be absent when parameter inclusion is disabled")
	}
}

// ---------------------------------------------------------------------------
// AC-D.3 — SQL errors mark the span status=Error; the error message contains
//           no query parameter values.
// ---------------------------------------------------------------------------

func TestOtelpgx_AC_D3_SQLErrorMarksSpanWithError(t *testing.T) {
	pool, tp, rec, cleanup := newTracedPool(t)
	defer cleanup()

	ctx, parentSpan := tp.Tracer("test").Start(context.Background(), "http.request")
	defer parentSpan.End()

	secretParam := "should-not-appear-in-error-span"

	// Force a DB error by querying a non-existent table.
	_, err := pool.Exec(ctx,
		"SELECT * FROM nonexistent_table_for_otelpgx_test WHERE id = $1",
		secretParam,
	)
	require.Error(t, err, "expected an error querying a missing table")

	spans := rec.Ended()
	errorSpan := findSpanContaining(spans, "SELECT")
	require.NotNil(t, errorSpan,
		"expected a SELECT span even on DB error; recorded spans: %v", spanNames(spans))

	// Span status must be Error.
	assert.Equal(t, codes.Error, errorSpan.Status().Code,
		"span status must be Error for a failed SQL query")

	// The status description (error message) must not contain parameter values.
	assert.NotContains(t, errorSpan.Status().Description, secretParam,
		"span error description must not contain query parameter values")

	// At least one exception event must have been recorded.
	hasException := false
	for _, ev := range errorSpan.Events() {
		if ev.Name == "exception" {
			hasException = true
			for _, attr := range ev.Attributes {
				if strings.Contains(strings.ToLower(string(attr.Key)), "message") {
					assert.NotContains(t, attr.Value.AsString(), secretParam,
						"exception event message attribute must not contain parameter values")
				}
			}
		}
	}
	assert.True(t, hasException, "span must record an exception event on SQL error")
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// findSpanContaining returns the first ended span whose name contains keyword.
func findSpanContaining(spans []sdktrace.ReadOnlySpan, keyword string) sdktrace.ReadOnlySpan {
	for _, s := range spans {
		if strings.Contains(s.Name(), keyword) {
			return s
		}
	}
	return nil
}

// spanNames extracts span names for use in assertion messages.
func spanNames(spans []sdktrace.ReadOnlySpan) []string {
	names := make([]string, len(spans))
	for i, s := range spans {
		names[i] = s.Name()
	}
	return names
}

// assertAttrString asserts that span has an attribute with key and the given string value.
func assertAttrString(t *testing.T, span sdktrace.ReadOnlySpan, key attribute.Key, want string) {
	t.Helper()
	for _, attr := range span.Attributes() {
		if attr.Key == key {
			assert.Equal(t, want, attr.Value.AsString(), "attribute %q", key)
			return
		}
	}
	t.Errorf("attribute %q not found on span %q", key, span.Name())
}

// attrString returns the string value of an attribute by key, or "" if absent.
func attrString(span sdktrace.ReadOnlySpan, key attribute.Key) string {
	for _, attr := range span.Attributes() {
		if attr.Key == key {
			return attr.Value.AsString()
		}
	}
	return ""
}
