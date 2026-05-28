package observability

import (
	"sync/atomic"

	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
)

// globalLoggerProvider stores the active LoggerProvider.
// Atomic pointer avoids locking in the read path.
var globalLoggerProvider atomic.Pointer[sdklog.LoggerProvider]

// globalMeterProvider stores the active MeterProvider.
var globalMeterProvider atomic.Pointer[sdkmetric.MeterProvider]

// LoggerProvider returns the active LoggerProvider, or nil when observability
// is disabled.  Used by PR-B (slog bridge).
func LoggerProvider() *sdklog.LoggerProvider {
	return globalLoggerProvider.Load()
}

// MeterProvider returns the active MeterProvider, or nil when observability
// is disabled.  Used by metrics.go internally.
func MeterProvider() *sdkmetric.MeterProvider {
	return globalMeterProvider.Load()
}
