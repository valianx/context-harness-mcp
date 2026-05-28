package observability

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// newTestMeterProvider returns an in-memory MeterProvider and a function that
// collects all recorded metrics into a ResourceMetrics snapshot.
func newTestMeterProvider(t *testing.T) (*sdkmetric.MeterProvider, func() metricdata.ResourceMetrics) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	collect := func() metricdata.ResourceMetrics {
		var rm metricdata.ResourceMetrics
		require.NoError(t, reader.Collect(context.Background(), &rm))
		return rm
	}
	return mp, collect
}

// resetInstruments clears the sync.Once and instrument state so each test
// subtest gets a fresh registration.  Only safe to call from test code.
func resetInstruments() {
	initOnce = sync.Once{}
	inst = instruments{}
	// Reset gauge callbacks too.
	kgNodesMu.Lock()
	kgNodesCb = nil
	kgNodesMu.Unlock()
	kgObsMu.Lock()
	kgObsCb = nil
	kgObsMu.Unlock()
	nodesWarnAt.Store(0)
	obsWarnAt.Store(0)
}

// findMetric returns true when rm contains at least one metric with the given name.
func findMetric(rm metricdata.ResourceMetrics, name string) bool {
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == name {
				return true
			}
		}
	}
	return false
}

// TestAllNineMetricsRegistered validates AC-A.6: all 9 instruments are
// registered and visible to the MeterProvider after InitMetrics.
func TestAllNineMetricsRegistered(t *testing.T) {
	mp, collect := newTestMeterProvider(t)
	otel.SetMeterProvider(mp)
	t.Cleanup(func() {
		_ = mp.Shutdown(context.Background())
		otel.SetMeterProvider(nil)
		resetInstruments()
	})

	// Register gauge callbacks that return a value so the observable gauges
	// produce data points during collection.
	RegisterKGNodesCallback(func(_ context.Context) (float64, error) { return 5, nil })
	RegisterKGObservationsCallback(func(_ context.Context) (float64, error) { return 20, nil })

	require.NoError(t, InitMetrics())

	// Emit one data point for each non-observable instrument so the reader
	// has something to collect.
	ctx := context.Background()
	RecordRequest(ctx, "search_nodes", "success", 42*time.Millisecond)
	RecordValidationRejection(ctx, "secrets", "aws-key")
	RecordEmbedDuration(ctx, true, 300*time.Millisecond)
	RecordEmbedTokensTruncated(ctx)
	RecordDBPoolDelta(ctx, "active", 1)
	RecordAuthCacheResult(ctx, "miss")

	rm := collect()

	want := []string{
		"mcp.requests.total",
		"mcp.request.duration",
		"mcp.validate.rejections.total",
		"mcp.embed.duration",
		"mcp.embed.tokens_truncated.total",
		"db.pool.acquired",
		"auth.jwt.cache.total",
		"kg.nodes.active",
		"kg.observations.total",
	}
	for _, name := range want {
		assert.True(t, findMetric(rm, name), "metric %q not found in collected data", name)
	}
}

// TestGaugeSafeToFail_DBError validates AC-A.7: a failing gauge callback does
// not propagate an error or panic; subsequent collection cycles succeed.
func TestGaugeSafeToFail_DBError(t *testing.T) {
	mp, collect := newTestMeterProvider(t)
	otel.SetMeterProvider(mp)
	t.Cleanup(func() {
		_ = mp.Shutdown(context.Background())
		otel.SetMeterProvider(nil)
		resetInstruments()
	})

	require.NoError(t, InitMetrics())

	// Register a callback that always fails.
	RegisterKGNodesCallback(func(_ context.Context) (float64, error) {
		return 0, errors.New("db down")
	})

	assert.NotPanics(t, func() {
		collect() // triggers the gauge callback; should swallow the error
	})

	// Swap in a healthy callback and verify the gauge now emits a data point.
	RegisterKGNodesCallback(func(_ context.Context) (float64, error) {
		return 42, nil
	})

	rm := collect()
	assert.True(t, findMetric(rm, "kg.nodes.active"))
}

// TestGaugeSafeToFail_Panic validates AC-A.7: a panicking gauge callback is
// recovered, and the MeterProvider continues to work.
func TestGaugeSafeToFail_Panic(t *testing.T) {
	mp, collect := newTestMeterProvider(t)
	otel.SetMeterProvider(mp)
	t.Cleanup(func() {
		_ = mp.Shutdown(context.Background())
		otel.SetMeterProvider(nil)
		resetInstruments()
	})

	require.NoError(t, InitMetrics())

	RegisterKGNodesCallback(func(_ context.Context) (float64, error) {
		panic("simulated panic in gauge callback")
	})

	assert.NotPanics(t, func() {
		collect()
	})
}

// TestGaugeWarnRateLimit verifies that maybeWarn emits at most once per
// gaugeWarnInterval.
func TestGaugeWarnRateLimit(t *testing.T) {
	var lastWarn atomic.Int64

	// First call — lastWarnAt is zero, so the warn should fire and update.
	maybeWarn(&lastWarn, "test.gauge", errors.New("fail 1"))
	firstFire := lastWarn.Load()
	assert.Greater(t, firstFire, int64(0), "firstFire should be non-zero")

	// Immediate second call — throttled, lastWarnAt should not advance.
	maybeWarn(&lastWarn, "test.gauge", errors.New("fail 2"))
	assert.Equal(t, firstFire, lastWarn.Load(), "second call should be throttled")

	// Rewind clock to force the next call to fire.
	past := time.Now().Add(-2 * gaugeWarnInterval).UnixNano()
	lastWarn.Store(past)
	maybeWarn(&lastWarn, "test.gauge", errors.New("fail 3"))
	assert.Greater(t, lastWarn.Load(), past, "third call should advance lastWarnAt")
}

// TestGaugeNilCallback verifies that a nil callback produces no data point but
// also no error (valid before the store registers its callback).
func TestGaugeNilCallback(t *testing.T) {
	mp, collect := newTestMeterProvider(t)
	otel.SetMeterProvider(mp)
	t.Cleanup(func() {
		_ = mp.Shutdown(context.Background())
		otel.SetMeterProvider(nil)
		resetInstruments()
	})

	require.NoError(t, InitMetrics())
	// No RegisterKGNodesCallback call — the gauge should skip silently.
	assert.NotPanics(t, func() { collect() })
}

// TestInitMetricsIdempotent verifies the sync.Once guard: calling InitMetrics
// twice returns no error.
func TestInitMetricsIdempotent(t *testing.T) {
	mp, _ := newTestMeterProvider(t)
	otel.SetMeterProvider(mp)
	t.Cleanup(func() {
		_ = mp.Shutdown(context.Background())
		otel.SetMeterProvider(nil)
		resetInstruments()
	})

	require.NoError(t, InitMetrics())
	require.NoError(t, InitMetrics()) // second call must be a no-op
}
