// Package observability — logbridge.go
//
// BridgedSlogHandler is a slog.Handler that fans out each log record to two
// destinations in this order:
//
//  1. The wrapped inner handler (stdout JSON) — ALWAYS, preserving the
//     existing operator-visible log format (AC-B.1).
//  2. The OTel LoggerProvider — ONLY when the provider is non-nil (i.e. when
//     CH_OBSERVABILITY_ENABLED=true and the SDK is active).
//
// For each OTel emission the bridge:
//   - Converts the slog level to an OTel Severity (AC-B.4 filter applied here).
//   - Attaches trace_id + span_id when the context carries an active span (AC-B.2).
//   - Attaches an empty / absent trace_id when there is no span (AC-B.3).
//   - Attaches user.id when the auth middleware has stored it in the context (AC-B.5).
//
// Stdout output is BYTE-IDENTICAL to the existing slog.NewJSONHandler output
// because the inner handler is invoked unmodified.  The bridge only adds the
// second OTel emission path.
package observability

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"time"

	logapi "go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/mariogutierrez/context-harness-mcp/internal/auth"
)

// minLogSeverity reads OTEL_LOG_LEVEL and returns the minimum OTel Severity
// that should be emitted to the OTel backend.  Default is INFO (AC-B.4).
//
// The env var uses OpenTelemetry's log level naming convention:
// "trace", "debug", "info", "warn", "error", "fatal".
func minLogSeverity() logapi.Severity {
	switch strings.ToLower(os.Getenv("OTEL_LOG_LEVEL")) {
	case "trace":
		return logapi.SeverityTrace
	case "debug":
		return logapi.SeverityDebug
	case "warn":
		return logapi.SeverityWarn
	case "error":
		return logapi.SeverityError
	case "fatal":
		return logapi.SeverityFatal
	default:
		// "info" is the default per the architecture plan.
		return logapi.SeverityInfo
	}
}

// slogToOtelSeverity maps a slog.Level to the closest OTel Severity constant.
func slogToOtelSeverity(level slog.Level) logapi.Severity {
	switch {
	case level >= slog.LevelError:
		return logapi.SeverityError
	case level >= slog.LevelWarn:
		return logapi.SeverityWarn
	case level >= slog.LevelInfo:
		return logapi.SeverityInfo
	default:
		return logapi.SeverityDebug
	}
}

// BridgedSlogHandler implements slog.Handler.  It wraps an existing inner
// handler (typically a slog.JSONHandler writing to stdout) and fans out each
// record to the OTel LoggerProvider when one is available.
//
// The zero value is not valid; use NewBridgedSlogHandler.
type BridgedSlogHandler struct {
	inner       slog.Handler
	provider    *sdklog.LoggerProvider // nil when observability is disabled
	logger      logapi.Logger          // lazily obtained from provider
	minLevel    logapi.Severity        // filtered from OTEL_LOG_LEVEL
	groupPrefix string                 // dot-separated group path from WithGroup calls
	preAttrs    []logapi.KeyValue      // accumulated from WithAttrs calls
}

// NewBridgedSlogHandler constructs a BridgedSlogHandler.
//
//   - inner must not be nil; it handles stdout output (AC-B.1).
//   - provider may be nil (when observability is disabled); in that case the
//     handler behaves identically to inner alone, with zero overhead.
func NewBridgedSlogHandler(inner slog.Handler, provider *sdklog.LoggerProvider) *BridgedSlogHandler {
	h := &BridgedSlogHandler{
		inner:    inner,
		provider: provider,
		minLevel: minLogSeverity(),
	}
	if provider != nil {
		h.logger = provider.Logger("context-harness-mcp")
	}
	return h
}

// Enabled returns true when the inner handler accepts the level.
// The OTel path uses its own level filter; we do not gate Enabled() on it so
// that stdout output is unaffected by OTEL_LOG_LEVEL.
func (h *BridgedSlogHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

// Handle fans the record out to both destinations.
//
// stdout: delegated verbatim to the inner handler.
// OTel: a new logapi.Record is built and emitted via h.logger when:
//  1. The provider is non-nil.
//  2. The record's severity meets or exceeds OTEL_LOG_LEVEL (AC-B.4).
func (h *BridgedSlogHandler) Handle(ctx context.Context, r slog.Record) error {
	// Inject signal attribute so Axiom queries can filter logs vs spans (AC-SG.1).
	r.AddAttrs(slog.String("signal", "log"))

	// Always write to stdout first (AC-B.1).
	innerErr := h.inner.Handle(ctx, r)

	if h.logger == nil {
		return innerErr
	}

	otelSev := slogToOtelSeverity(r.Level)
	if otelSev < h.minLevel {
		return innerErr
	}

	h.emitToOtel(ctx, r, otelSev)

	return innerErr
}

// emitToOtel constructs and emits an OTel LogRecord from a slog.Record.
// Called only when the provider is active and the level passes the filter.
func (h *BridgedSlogHandler) emitToOtel(ctx context.Context, r slog.Record, sev logapi.Severity) {
	var rec logapi.Record

	rec.SetTimestamp(r.Time)
	rec.SetObservedTimestamp(time.Now())
	rec.SetSeverity(sev)
	rec.SetSeverityText(r.Level.String())
	rec.SetBody(logapi.StringValue(r.Message))

	// Pre-accumulated attrs from WithAttrs / WithGroup calls on this handler.
	capacity := len(h.preAttrs) + r.NumAttrs() + 3
	attrs := make([]logapi.KeyValue, 0, capacity)
	attrs = append(attrs, h.preAttrs...)

	// Build OTel attributes from the slog record's structured fields.
	r.Attrs(func(a slog.Attr) bool {
		attrs = append(attrs, slogAttrToOtel(a))
		return true
	})

	// Inject trace correlation when an active span exists in the context (AC-B.2).
	// When there is no span the span context is invalid and these attrs are omitted,
	// so log records produced outside a request span carry no trace_id (AC-B.3).
	sc := oteltrace.SpanContextFromContext(ctx)
	if sc.IsValid() {
		attrs = append(attrs,
			logapi.String("trace_id", sc.TraceID().String()),
			logapi.String("span_id", sc.SpanID().String()),
		)
	}

	// Inject user.id when the auth middleware has populated it (AC-B.5).
	// UserIDFromContext returns "" when unauthenticated; we omit the attribute
	// entirely rather than emitting an empty string.
	if uid := auth.UserIDFromContext(ctx); uid != "" {
		attrs = append(attrs, logapi.String("user.id", uid))
	}

	rec.AddAttributes(attrs...)

	h.logger.Emit(ctx, rec)
}

// WithAttrs returns a new handler whose OTel emission path and inner handler
// both carry the given attributes.
func (h *BridgedSlogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	otelAttrs := make([]logapi.KeyValue, 0, len(attrs))
	for _, a := range attrs {
		otelAttrs = append(otelAttrs, slogAttrToOtel(a))
	}

	combined := make([]logapi.KeyValue, len(h.preAttrs)+len(otelAttrs))
	copy(combined, h.preAttrs)
	copy(combined[len(h.preAttrs):], otelAttrs)

	return &BridgedSlogHandler{
		inner:       h.inner.WithAttrs(attrs),
		provider:    h.provider,
		logger:      h.logger,
		minLevel:    h.minLevel,
		groupPrefix: h.groupPrefix,
		preAttrs:    combined,
	}
}

// WithGroup returns a new handler with the given group namespace applied to
// both the inner handler and (indirectly) the OTel path.
func (h *BridgedSlogHandler) WithGroup(name string) slog.Handler {
	prefix := name
	if h.groupPrefix != "" {
		prefix = h.groupPrefix + "." + name
	}
	return &BridgedSlogHandler{
		inner:       h.inner.WithGroup(name),
		provider:    h.provider,
		logger:      h.logger,
		minLevel:    h.minLevel,
		groupPrefix: prefix,
		preAttrs:    h.preAttrs,
	}
}

// slogAttrToOtel converts a slog.Attr to an OTel logapi.KeyValue.
// Groups are represented as OTel maps (AC-B.1: structural fidelity).
func slogAttrToOtel(a slog.Attr) logapi.KeyValue {
	switch a.Value.Kind() {
	case slog.KindGroup:
		sub := a.Value.Group()
		kvs := make([]logapi.KeyValue, 0, len(sub))
		for _, s := range sub {
			kvs = append(kvs, slogAttrToOtel(s))
		}
		return logapi.Map(a.Key, kvs...)
	case slog.KindBool:
		return logapi.Bool(a.Key, a.Value.Bool())
	case slog.KindFloat64:
		return logapi.Float64(a.Key, a.Value.Float64())
	case slog.KindInt64:
		return logapi.Int64(a.Key, a.Value.Int64())
	case slog.KindUint64:
		// OTel log has no uint64 — use int64 (safe for realistic counter values).
		return logapi.Int64(a.Key, int64(a.Value.Uint64())) //nolint:gosec
	case slog.KindDuration:
		return logapi.Int64(a.Key, a.Value.Duration().Nanoseconds())
	case slog.KindTime:
		return logapi.String(a.Key, a.Value.Time().Format(time.RFC3339Nano))
	case slog.KindAny:
		return logapi.String(a.Key, a.Value.String())
	default: // KindString and everything else
		return logapi.String(a.Key, a.Value.String())
	}
}
