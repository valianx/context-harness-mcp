package observability

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	logapi "go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	oteltrace "go.opentelemetry.io/otel/trace"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/mariogutierrez/context-harness-mcp/internal/auth"
)

// recordCapture is a minimal sdklog.Processor that records every emitted
// log record for assertion in tests.  Safe for concurrent use.
// Named differently from scrub_test.go's captureProcessor to avoid redeclaration.
type recordCapture struct {
	mu      sync.Mutex
	records []sdklog.Record
}

func (c *recordCapture) OnEmit(_ context.Context, r *sdklog.Record) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	// Clone so the caller can mutate r after OnEmit returns.
	c.records = append(c.records, r.Clone())
	return nil
}

func (c *recordCapture) Shutdown(_ context.Context) error   { return nil }
func (c *recordCapture) ForceFlush(_ context.Context) error { return nil }

func (c *recordCapture) Emitted() []sdklog.Record {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]sdklog.Record, len(c.records))
	copy(out, c.records)
	return out
}

// newTestProvider creates an sdklog.LoggerProvider backed by a recordCapture.
func newTestProvider(proc *recordCapture) *sdklog.LoggerProvider {
	return sdklog.NewLoggerProvider(
		sdklog.WithProcessor(proc),
	)
}

// findAttr walks a log record's attributes and returns the value for the given
// key, plus a boolean indicating whether the key was present.
func findAttr(t *testing.T, r sdklog.Record, key string) (logapi.Value, bool) {
	t.Helper()
	var found logapi.Value
	var ok bool
	r.WalkAttributes(func(kv logapi.KeyValue) bool {
		if kv.Key == key {
			found = kv.Value
			ok = true
			return false // stop iteration
		}
		return true
	})
	return found, ok
}

// ── AC-B.1: logs siguen yendo a stdout ────────────────────────────────────────

func TestBridgedSlogHandler_AC_B1_StdoutAlwaysReceivesOutput(t *testing.T) {
	var buf bytes.Buffer
	inner := slog.NewJSONHandler(&buf, nil)

	// provider is nil → bridge is stdout-only path.
	h := NewBridgedSlogHandler(inner, nil)
	logger := slog.New(h)

	logger.Info("hello stdout", "key", "value")

	line := buf.String()
	assert.Contains(t, line, `"hello stdout"`, "message must appear in stdout")
	assert.Contains(t, line, `"key"`, "structured attr key must appear in stdout")
	assert.Contains(t, line, `"value"`, "structured attr value must appear in stdout")
}

func TestBridgedSlogHandler_AC_B1_StdoutReceivedEvenWhenOtelActive(t *testing.T) {
	// Ensure stdout output is not suppressed when OTel is active.
	var buf bytes.Buffer
	inner := slog.NewJSONHandler(&buf, nil)
	proc := &recordCapture{}
	provider := newTestProvider(proc)

	h := NewBridgedSlogHandler(inner, provider)
	logger := slog.New(h)

	logger.Info("both paths", "x", 1)

	assert.NotEmpty(t, buf.String(), "stdout must receive the log line")
	require.Len(t, proc.Emitted(), 1, "OTel must also receive the record")
}

// ── AC-B.2: trace_id + span_id cuando hay span activa ─────────────────────────

func TestBridgedSlogHandler_AC_B2_TraceIDAndSpanIDWhenSpanActive(t *testing.T) {
	var buf bytes.Buffer
	proc := &recordCapture{}
	provider := newTestProvider(proc)
	h := NewBridgedSlogHandler(slog.NewJSONHandler(&buf, nil), provider)
	logger := slog.New(h)

	// Create a real OTel span so the context carries valid trace + span IDs.
	tp := sdktrace.NewTracerProvider()
	tracer := tp.Tracer("test")
	ctx, span := tracer.Start(context.Background(), "test-span")
	defer span.End()

	sc := oteltrace.SpanContextFromContext(ctx)
	require.True(t, sc.IsValid(), "span context must be valid")

	logger.InfoContext(ctx, "with span")

	records := proc.Emitted()
	require.Len(t, records, 1)

	traceIDVal, hasTraceID := findAttr(t, records[0], "trace_id")
	spanIDVal, hasSpanID := findAttr(t, records[0], "span_id")

	assert.True(t, hasTraceID, "trace_id attribute must be present")
	assert.True(t, hasSpanID, "span_id attribute must be present")
	assert.Equal(t, sc.TraceID().String(), traceIDVal.AsString())
	assert.Equal(t, sc.SpanID().String(), spanIDVal.AsString())
}

// ── AC-B.3: logs sin context no incluyen trace_id ─────────────────────────────

func TestBridgedSlogHandler_AC_B3_NoTraceIDWhenNoSpan(t *testing.T) {
	var buf bytes.Buffer
	proc := &recordCapture{}
	provider := newTestProvider(proc)
	h := NewBridgedSlogHandler(slog.NewJSONHandler(&buf, nil), provider)
	logger := slog.New(h)

	// Background context has no span.
	logger.InfoContext(context.Background(), "no span here")

	records := proc.Emitted()
	require.Len(t, records, 1)

	_, hasTraceID := findAttr(t, records[0], "trace_id")
	_, hasSpanID := findAttr(t, records[0], "span_id")

	assert.False(t, hasTraceID, "trace_id must be absent when there is no active span")
	assert.False(t, hasSpanID, "span_id must be absent when there is no active span")
}

// ── AC-B.4: logs respetan el filtro de nivel OTEL_LOG_LEVEL ───────────────────

func TestBridgedSlogHandler_AC_B4_OtelLevelFilterHonored(t *testing.T) {
	// Set OTEL_LOG_LEVEL=warn → DEBUG and INFO records must NOT reach OTel.
	t.Setenv("OTEL_LOG_LEVEL", "warn")

	var buf bytes.Buffer
	// Allow all levels on the inner handler to confirm stdout is unaffected.
	inner := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	proc := &recordCapture{}
	provider := newTestProvider(proc)
	h := NewBridgedSlogHandler(inner, provider)
	logger := slog.New(h)

	logger.Debug("debug message")
	logger.Info("info message")
	logger.Warn("warn message")
	logger.Error("error message")

	// Stdout must have all four (inner handler is LevelDebug).
	stdout := buf.String()
	assert.Contains(t, stdout, "debug message")
	assert.Contains(t, stdout, "info message")
	assert.Contains(t, stdout, "warn message")
	assert.Contains(t, stdout, "error message")

	// OTel must only have warn and error.
	otelRecords := proc.Emitted()
	require.Len(t, otelRecords, 2, "only warn+ records should reach OTel")

	assert.Equal(t, logapi.SeverityWarn, otelRecords[0].Severity())
	assert.Equal(t, logapi.SeverityError, otelRecords[1].Severity())
}

func TestBridgedSlogHandler_AC_B4_DefaultLevelIsInfo(t *testing.T) {
	// With no OTEL_LOG_LEVEL set, default is INFO.
	// DEBUG records must NOT reach OTel; INFO+ must.
	os.Unsetenv("OTEL_LOG_LEVEL") //nolint:errcheck

	var buf bytes.Buffer
	inner := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	proc := &recordCapture{}
	provider := newTestProvider(proc)
	h := NewBridgedSlogHandler(inner, provider)
	logger := slog.New(h)

	logger.Debug("should not go to otel")
	logger.Info("should go to otel")

	otelRecords := proc.Emitted()
	require.Len(t, otelRecords, 1, "only INFO+ should reach OTel by default")
	assert.Equal(t, logapi.SeverityInfo, otelRecords[0].Severity())
}

// ── AC-B.5: user.id cuando el request está autenticado ────────────────────────

func TestBridgedSlogHandler_AC_B5_UserIDWhenAuthenticated(t *testing.T) {
	var buf bytes.Buffer
	proc := &recordCapture{}
	provider := newTestProvider(proc)
	h := NewBridgedSlogHandler(slog.NewJSONHandler(&buf, nil), provider)
	logger := slog.New(h)

	userID := "550e8400-e29b-41d4-a716-446655440000"
	ctx := auth.WithUserID(context.Background(), userID)

	logger.InfoContext(ctx, "authenticated request")

	records := proc.Emitted()
	require.Len(t, records, 1)

	val, hasUserID := findAttr(t, records[0], "user.id")
	assert.True(t, hasUserID, "user.id attribute must be present for authenticated requests")
	assert.Equal(t, userID, val.AsString())
}

func TestBridgedSlogHandler_AC_B5_UserIDAbsentWhenUnauthenticated(t *testing.T) {
	var buf bytes.Buffer
	proc := &recordCapture{}
	provider := newTestProvider(proc)
	h := NewBridgedSlogHandler(slog.NewJSONHandler(&buf, nil), provider)
	logger := slog.New(h)

	// No user in context.
	logger.InfoContext(context.Background(), "unauthenticated request")

	records := proc.Emitted()
	require.Len(t, records, 1)

	_, hasUserID := findAttr(t, records[0], "user.id")
	assert.False(t, hasUserID, "user.id must be absent when not authenticated")
}

// ── Structural tests ──────────────────────────────────────────────────────────

func TestBridgedSlogHandler_WithAttrsPropagatesToBothPaths(t *testing.T) {
	var buf bytes.Buffer
	proc := &recordCapture{}
	provider := newTestProvider(proc)

	base := NewBridgedSlogHandler(slog.NewJSONHandler(&buf, nil), provider)
	h := base.WithAttrs([]slog.Attr{slog.String("component", "test")})
	logger := slog.New(h)

	logger.Info("with attrs")

	// stdout must carry the attribute.
	assert.Contains(t, buf.String(), `"component"`)

	// OTel must also carry it.
	records := proc.Emitted()
	require.Len(t, records, 1)
	val, ok := findAttr(t, records[0], "component")
	assert.True(t, ok, "component attr must be in OTel record")
	assert.Equal(t, "test", val.AsString())
}

func TestBridgedSlogHandler_NilProvider_NoOtelEmission(t *testing.T) {
	var buf bytes.Buffer
	h := NewBridgedSlogHandler(slog.NewJSONHandler(&buf, nil), nil)
	logger := slog.New(h)

	// Must not panic when provider is nil.
	logger.Info("nil provider")
	assert.Contains(t, buf.String(), "nil provider")
}

func TestSlogAttrToOtel_TypeMapping(t *testing.T) {
	cases := []struct {
		attr     slog.Attr
		expected logapi.Value
	}{
		{slog.Bool("b", true), logapi.BoolValue(true)},
		{slog.Float64("f", 3.14), logapi.Float64Value(3.14)},
		{slog.Int64("i", -42), logapi.Int64Value(-42)},
		{slog.String("s", "hello"), logapi.StringValue("hello")},
	}

	for _, tc := range cases {
		kv := slogAttrToOtel(tc.attr)
		assert.Equal(t, tc.expected, kv.Value, "type mapping for attr %s", tc.attr.Key)
	}
}
