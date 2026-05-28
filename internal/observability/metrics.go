// Package observability — metrics.go defines the 9 instruments listed in the
// Functional Outcomes section of the axiom-integration plan.
//
// Instrument summary:
//   - mcp.requests.total          Counter       tool, outcome
//   - mcp.request.duration        Histogram     tool
//   - mcp.validate.rejections.total Counter     layer, pattern
//   - mcp.embed.duration          Histogram     cold (bool-as-string)
//   - mcp.embed.tokens_truncated.total Counter  (no attrs)
//   - db.pool.acquired            UpDownCounter state (active|idle)
//   - auth.jwt.cache.total        Counter       result (hit|miss)
//   - kg.nodes.active             Float64ObservableGauge
//   - kg.observations.total       Float64ObservableGauge
//
// Observable gauges (kg.*) accept callbacks registered via
// RegisterKGStatsProvider.  If no callback is registered the gauge silently
// skips the observation.  The store wires the callback at boot in a later PR.
package observability

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

const (
	// meterName is the instrumentation scope for all metrics.
	meterName = "github.com/mariogutierrez/context-harness-mcp"

	// gaugeWarnInterval throttles the WARN log emitted when a gauge callback
	// fails, to at most once per minute (AC-A.7).
	gaugeWarnInterval = time.Minute
)

// KGStatsFunc is the type of callback registered by the store.
// Returns (value, nil) on success, or (0, error) when DB is unavailable.
type KGStatsFunc func(ctx context.Context) (float64, error)

// instruments holds the initialised metric instruments.
// Populated once by initMetrics(); safe to use concurrently after that.
type instruments struct {
	requestsTotal         metric.Int64Counter
	requestDuration       metric.Float64Histogram
	validateRejectTotal   metric.Int64Counter
	embedDuration         metric.Float64Histogram
	embedTokensTruncated  metric.Int64Counter
	dbPoolAcquired        metric.Int64UpDownCounter
	authJWTCacheTotal     metric.Int64Counter
}

var (
	// initOnce ensures metrics are initialised at most once.
	initOnce sync.Once
	inst     instruments

	// kgNodesCb and kgObsCb are the observable gauge callbacks registered by
	// the store.  Nil means "skip" — valid before the store wires them.
	kgNodesMu  sync.RWMutex
	kgNodesCb  KGStatsFunc

	kgObsMu sync.RWMutex
	kgObsCb KGStatsFunc

	// gaugeWarnThrottle tracks when each gauge last emitted a WARN log.
	nodesWarnAt atomic.Int64
	obsWarnAt   atomic.Int64
)

// RegisterKGNodesCallback registers the callback that returns the current
// count of active nodes.  Called once at boot by the store package (future PR).
func RegisterKGNodesCallback(fn KGStatsFunc) {
	kgNodesMu.Lock()
	defer kgNodesMu.Unlock()
	kgNodesCb = fn
}

// RegisterKGObservationsCallback registers the callback that returns the total
// observation count.
func RegisterKGObservationsCallback(fn KGStatsFunc) {
	kgObsMu.Lock()
	defer kgObsMu.Unlock()
	kgObsCb = fn
}

// InitMetrics registers all 9 instruments with the global MeterProvider.
// It is called lazily on first use — the MeterProvider may not be ready at
// package init time.  Repeated calls are safe (sync.Once).
func InitMetrics() error {
	var retErr error
	initOnce.Do(func() {
		retErr = registerInstruments()
	})
	return retErr
}

func registerInstruments() error {
	m := otel.Meter(meterName)

	var err error

	inst.requestsTotal, err = m.Int64Counter(
		"mcp.requests.total",
		metric.WithDescription("Total MCP tool invocations by tool name and outcome"),
	)
	if err != nil {
		return fmt.Errorf("mcp.requests.total: %w", err)
	}

	inst.requestDuration, err = m.Float64Histogram(
		"mcp.request.duration",
		metric.WithDescription("MCP tool latency in milliseconds"),
		metric.WithUnit("ms"),
	)
	if err != nil {
		return fmt.Errorf("mcp.request.duration: %w", err)
	}

	inst.validateRejectTotal, err = m.Int64Counter(
		"mcp.validate.rejections.total",
		metric.WithDescription("Content Filter rejections by layer and pattern"),
	)
	if err != nil {
		return fmt.Errorf("mcp.validate.rejections.total: %w", err)
	}

	inst.embedDuration, err = m.Float64Histogram(
		"mcp.embed.duration",
		metric.WithDescription("Embedding latency in milliseconds, split by cold/warm start"),
		metric.WithUnit("ms"),
	)
	if err != nil {
		return fmt.Errorf("mcp.embed.duration: %w", err)
	}

	inst.embedTokensTruncated, err = m.Int64Counter(
		"mcp.embed.tokens_truncated.total",
		metric.WithDescription("Observations that exceeded the 256-token embed window and were truncated"),
	)
	if err != nil {
		return fmt.Errorf("mcp.embed.tokens_truncated.total: %w", err)
	}

	inst.dbPoolAcquired, err = m.Int64UpDownCounter(
		"db.pool.acquired",
		metric.WithDescription("pgx pool connections in use (state=active|idle)"),
	)
	if err != nil {
		return fmt.Errorf("db.pool.acquired: %w", err)
	}

	inst.authJWTCacheTotal, err = m.Int64Counter(
		"auth.jwt.cache.total",
		metric.WithDescription("JWT revocation LRU cache lookups by result"),
	)
	if err != nil {
		return fmt.Errorf("auth.jwt.cache.total: %w", err)
	}

	// Observable gauges — periodic reader fires every 60 s (configured in initMeter).
	if _, err = m.Float64ObservableGauge(
		"kg.nodes.active",
		metric.WithDescription("Current count of active (non-deleted) nodes"),
		metric.WithFloat64Callback(func(ctx context.Context, o metric.Float64Observer) error {
			return observeGauge(ctx, o, &kgNodesMu, &kgNodesCb, &nodesWarnAt, "kg.nodes.active")
		}),
	); err != nil {
		return fmt.Errorf("kg.nodes.active: %w", err)
	}

	if _, err = m.Float64ObservableGauge(
		"kg.observations.total",
		metric.WithDescription("Total active observation rows (embedding count proxy)"),
		metric.WithFloat64Callback(func(ctx context.Context, o metric.Float64Observer) error {
			return observeGauge(ctx, o, &kgObsMu, &kgObsCb, &obsWarnAt, "kg.observations.total")
		}),
	); err != nil {
		return fmt.Errorf("kg.observations.total: %w", err)
	}

	return nil
}

// observeGauge is the shared implementation for observable gauge callbacks.
// It is safe-to-fail (AC-A.7): panics are recovered, DB errors are swallowed
// with a rate-limited WARN, and the observation is simply skipped for that
// collection cycle.
func observeGauge(
	ctx context.Context,
	o metric.Float64Observer,
	mu *sync.RWMutex,
	cbPtr *KGStatsFunc,
	lastWarnAt *atomic.Int64,
	name string,
) (retErr error) {
	// Panic recovery — a panicking callback must not crash the MeterProvider.
	defer func() {
		if r := recover(); r != nil {
			maybeWarn(lastWarnAt, name, fmt.Errorf("panic: %v", r))
		}
	}()

	mu.RLock()
	cb := *cbPtr
	mu.RUnlock()

	if cb == nil {
		return nil // no callback registered yet — skip silently
	}

	v, err := cb(ctx)
	if err != nil {
		maybeWarn(lastWarnAt, name, err)
		return nil // swallow — do not surface DB errors through the gauge path
	}

	o.Observe(v)
	return nil
}

// maybeWarn emits a WARN log at most once per gaugeWarnInterval (AC-A.7).
func maybeWarn(lastWarnAt *atomic.Int64, name string, err error) {
	now := time.Now().UnixNano()
	prev := lastWarnAt.Load()
	if now-prev < gaugeWarnInterval.Nanoseconds() {
		return
	}
	if lastWarnAt.CompareAndSwap(prev, now) {
		slog.Warn("observable gauge callback failed (rate-limited)",
			"gauge", name, "error", err)
	}
}

// ---------------------------------------------------------------------------
// Public helpers — called by tool handlers and middleware (PR-E wires these).
// ---------------------------------------------------------------------------

// RecordRequest records a single MCP tool invocation counter and histogram.
// tool is the tool name (e.g. "search_nodes"), outcome is "success",
// "policy_reject", or "server_error".
func RecordRequest(ctx context.Context, tool, outcome string, dur time.Duration) {
	if err := InitMetrics(); err != nil {
		return
	}
	attrs := metric.WithAttributes(
		attribute.String("tool", tool),
		attribute.String("outcome", outcome),
	)
	inst.requestsTotal.Add(ctx, 1, attrs)
	inst.requestDuration.Record(ctx, float64(dur.Milliseconds()),
		metric.WithAttributes(attribute.String("tool", tool)))
}

// RecordValidationRejection records a Content Filter rejection.
// layer is "syntactic", "secrets", or "taxonomy"; pattern is the matched rule.
func RecordValidationRejection(ctx context.Context, layer, pattern string) {
	if err := InitMetrics(); err != nil {
		return
	}
	inst.validateRejectTotal.Add(ctx, 1, metric.WithAttributes(
		attribute.String("layer", layer),
		attribute.String("pattern", pattern),
	))
}

// RecordEmbedDuration records embedding latency. cold indicates whether this
// was a cold-start invocation of the ONNX session.
func RecordEmbedDuration(ctx context.Context, cold bool, dur time.Duration) {
	if err := InitMetrics(); err != nil {
		return
	}
	inst.embedDuration.Record(ctx, float64(dur.Milliseconds()), metric.WithAttributes(
		attribute.Bool("cold", cold),
	))
}

// RecordEmbedTokensTruncated increments the truncation counter.
func RecordEmbedTokensTruncated(ctx context.Context) {
	if err := InitMetrics(); err != nil {
		return
	}
	inst.embedTokensTruncated.Add(ctx, 1)
}

// RecordDBPoolDelta adjusts the db.pool.acquired UpDownCounter.
// state is "active" or "idle"; delta is +1 or -1.
func RecordDBPoolDelta(ctx context.Context, state string, delta int64) {
	if err := InitMetrics(); err != nil {
		return
	}
	inst.dbPoolAcquired.Add(ctx, delta, metric.WithAttributes(
		attribute.String("state", state),
	))
}

// RecordAuthCacheResult increments the JWT revocation cache counter.
// result is "hit" or "miss".
func RecordAuthCacheResult(ctx context.Context, result string) {
	if err := InitMetrics(); err != nil {
		return
	}
	inst.authJWTCacheTotal.Add(ctx, 1, metric.WithAttributes(
		attribute.String("result", result),
	))
}
