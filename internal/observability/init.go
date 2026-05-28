// Package observability initialises the OpenTelemetry SDK for
// context-harness-mcp.  All three signals (traces, logs, metrics) share a
// single OTLP/HTTP exporter pointed at OTEL_EXPORTER_OTLP_ENDPOINT.
//
// Axiom-specific credentials are never embedded in code.  observability.Init
// constructs the OTEL_EXPORTER_OTLP_HEADERS value at runtime from AXIOM_TOKEN
// and AXIOM_DATASET, then delegates header injection to the standard OTel SDK
// env-var contract.
//
// Kill-switch behaviour (AC-A.1):
//   - CH_OBSERVABILITY_ENABLED != "true"  → no-op, log disabled message, return nil.
//   - OTEL_SDK_DISABLED = "true"          → SDK's own kill-switch; Init respects it.
//
// Fail-fast on inconsistent config (AC-A.2):
//   - CH_OBSERVABILITY_ENABLED=true but OTEL_EXPORTER_OTLP_ENDPOINT empty → error.
//
// Boot guard (AC-A.8):
//   - CH_OBSERVABILITY_ENABLED=true but scrubber is still noopScrubber → error.
//   - PR-F resolves this by calling RegisterScrubber(realScrubber) in its init().
package observability

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.34.0"
)

const (
	// shutdownTimeout is the hard deadline applied to each provider shutdown.
	// Per AC-A.4, if the exporter does not flush within 10s the shutdown returns
	// anyway, losing queued batches but not blocking process exit.
	shutdownTimeout = 10 * time.Second

	// serviceName is the default when OTEL_SERVICE_NAME is not set.
	serviceName = "context-harness-mcp"
)

// Scrubber is implemented by any type that can redact PII and secrets from
// a string before it leaves the process boundary.  PR-F registers a real
// implementation.  Until then only noopScrubber is registered, and
// observability.Init refuses to start when CH_OBSERVABILITY_ENABLED=true.
type Scrubber interface {
	Redact(text string) string
}

// noopScrubber is the default.  Its presence signals that PR-F has not merged.
type noopScrubber struct{}

func (noopScrubber) Redact(text string) string { return text }

// scrubberMu protects the global scrubber slot.
var (
	scrubberMu      sync.RWMutex
	activeScrubber  Scrubber = noopScrubber{}
	isNoopScrubber           = true
)

// RegisterScrubber replaces the active scrubber.  Called from PR-F's package
// init() so the real scrubber is in place before main() runs.
func RegisterScrubber(s Scrubber) {
	scrubberMu.Lock()
	defer scrubberMu.Unlock()
	activeScrubber = s
	_, isNoopScrubber = s.(noopScrubber)
}

// ActiveScrubber returns the currently registered scrubber.
func ActiveScrubber() Scrubber {
	scrubberMu.RLock()
	defer scrubberMu.RUnlock()
	return activeScrubber
}

// ShutdownFunc is returned by Init.  Callers defer it with a 10-second timeout
// context to flush any in-flight batches before the process exits.
type ShutdownFunc func(context.Context) error

// Init bootstraps TracerProvider, LoggerProvider, and MeterProvider.
// It returns a shutdown function and nil error on success.
//
// No-op path (AC-A.1): returns (noopShutdown, nil) when disabled.
// Error path (AC-A.2, AC-A.8): returns (nil, non-nil error) on bad config.
func Init(ctx context.Context) (ShutdownFunc, error) {
	if !isObservabilityEnabled() {
		slog.Info("observability disabled", "reason", "CH_OBSERVABILITY_ENABLED not true")
		return noopShutdown, nil
	}

	// Honor OTEL_SDK_DISABLED — the stdio kill-switch in main.go sets this
	// before invoking Init. Even with CH_OBSERVABILITY_ENABLED=true and a
	// valid endpoint, OTEL_SDK_DISABLED=true must short-circuit to noop.
	if os.Getenv("OTEL_SDK_DISABLED") == "true" {
		slog.Info("observability disabled", "reason", "OTEL_SDK_DISABLED=true")
		return noopShutdown, nil
	}

	endpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	if endpoint == "" {
		return nil, errors.New("CH_OBSERVABILITY_ENABLED=true requires OTEL_EXPORTER_OTLP_ENDPOINT")
	}

	// Boot guard (AC-A.8): refuse to start with the noop scrubber active.
	scrubberMu.RLock()
	noop := isNoopScrubber
	scrubberMu.RUnlock()
	if noop {
		return nil, errors.New(
			"observability enabled but no real scrubber registered — " +
				"link PR-F before activating in production",
		)
	}

	if err := buildAxiomHeaders(); err != nil {
		return nil, fmt.Errorf("axiom header construction: %w", err)
	}

	res, err := buildResource(ctx)
	if err != nil {
		return nil, fmt.Errorf("build otel resource: %w", err)
	}

	sampler := buildSampler()

	traceShutdown, err := initTracer(ctx, res, sampler)
	if err != nil {
		return nil, fmt.Errorf("init tracer: %w", err)
	}

	logShutdown, err := initLogger(ctx, res)
	if err != nil {
		_ = traceShutdown(ctx)
		return nil, fmt.Errorf("init logger: %w", err)
	}

	meterShutdown, err := initMeter(ctx, res)
	if err != nil {
		_ = traceShutdown(ctx)
		_ = logShutdown(ctx)
		return nil, fmt.Errorf("init meter: %w", err)
	}

	// W3C TraceContext propagation — explicit default (AC-A.3).
	// Setting this prevents any future dep from silently overriding to B3.
	otel.SetTextMapPropagator(
		propagation.NewCompositeTextMapPropagator(
			propagation.TraceContext{},
			propagation.Baggage{},
		),
	)

	slog.Info("observability initialised",
		"endpoint", endpoint,
		"service", os.Getenv("OTEL_SERVICE_NAME"),
	)

	return combineShutdowns(traceShutdown, logShutdown, meterShutdown), nil
}

// isObservabilityEnabled returns true only when CH_OBSERVABILITY_ENABLED is
// the exact string "true".  OTEL_SDK_DISABLED=true is a separate SDK-level
// kill-switch honoured downstream.
func isObservabilityEnabled() bool {
	return os.Getenv("CH_OBSERVABILITY_ENABLED") == "true"
}

// buildAxiomHeaders sets OTEL_EXPORTER_OTLP_HEADERS from AXIOM_TOKEN +
// AXIOM_DATASET when those vars are present.  If OTEL_EXPORTER_OTLP_HEADERS
// is already set the caller wins.
func buildAxiomHeaders() error {
	if os.Getenv("OTEL_EXPORTER_OTLP_HEADERS") != "" {
		return nil // caller already set headers
	}
	token := os.Getenv("AXIOM_TOKEN")
	dataset := os.Getenv("AXIOM_DATASET")
	if token == "" || dataset == "" {
		// Neither token nor dataset provided — allowed if the operator is
		// pointing at a collector that needs no auth.
		return nil
	}
	headers := fmt.Sprintf("Authorization=Bearer %s,X-Axiom-Dataset=%s", token, dataset)
	return os.Setenv("OTEL_EXPORTER_OTLP_HEADERS", headers)
}

// buildResource constructs the OTEL resource for this service.
func buildResource(ctx context.Context) (*resource.Resource, error) {
	name := os.Getenv("OTEL_SERVICE_NAME")
	if name == "" {
		name = serviceName
	}
	return resource.Merge(
		resource.Default(),
		resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceName(name),
		),
	)
}

// buildSampler reads OTEL_TRACES_SAMPLER and OTEL_TRACES_SAMPLER_ARG to
// construct the sampler (AC-A.9).  Default: ParentBased(1.0).
func buildSampler() sdktrace.Sampler {
	samplerName := os.Getenv("OTEL_TRACES_SAMPLER")
	if samplerName == "" {
		samplerName = "parentbased_traceidratio"
	}

	samplerArg := os.Getenv("OTEL_TRACES_SAMPLER_ARG")
	ratio := 1.0
	if samplerArg != "" {
		if v, err := strconv.ParseFloat(samplerArg, 64); err == nil {
			ratio = v
		}
	}

	switch samplerName {
	case "traceidratio":
		return sdktrace.TraceIDRatioBased(ratio)
	case "parentbased_traceidratio":
		return sdktrace.ParentBased(sdktrace.TraceIDRatioBased(ratio))
	case "always_on":
		return sdktrace.AlwaysSample()
	case "always_off":
		return sdktrace.NeverSample()
	case "parentbased_always_on":
		return sdktrace.ParentBased(sdktrace.AlwaysSample())
	case "parentbased_always_off":
		return sdktrace.ParentBased(sdktrace.NeverSample())
	default:
		slog.Warn("unknown OTEL_TRACES_SAMPLER, using parentbased_traceidratio",
			"sampler", samplerName)
		return sdktrace.ParentBased(sdktrace.TraceIDRatioBased(ratio))
	}
}

// initTracer creates and registers the global TracerProvider.
//
// Pipeline order (innermost to outermost):
//   OTLP exporter → ScrubSpanExporter → BatchSpanProcessor → noiseFilterProcessor
//
// noiseFilterProcessor sits upstream of the batch processor so dropped spans
// (e.g. "prepare *") are discarded before batch allocation (AC-J.2, AC-J.3).
// ScrubSpanExporter wraps the OTLP exporter so all exported string attrs are
// scrubbed before leaving the process (PR-F boundary, unchanged).
func initTracer(ctx context.Context, res *resource.Resource, sampler sdktrace.Sampler) (ShutdownFunc, error) {
	exporter, err := otlptracehttp.New(ctx)
	if err != nil {
		return nil, err
	}

	// Wrap the OTLP exporter with the scrubber (PR-F, unchanged).
	scrubExporter := NewScrubSpanExporter(exporter)

	// Materialise the batch processor explicitly so we can wrap it with the
	// noise filter. WithBatcher() is a shorthand that does not allow insertion
	// of a processor upstream of the batch — we replicate what it does here.
	batchProc := sdktrace.NewBatchSpanProcessor(scrubExporter)
	noiseProc := NewNoiseFilterProcessor(batchProc)

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(noiseProc),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sampler),
	)
	otel.SetTracerProvider(tp)
	return tp.Shutdown, nil
}

// initLogger creates and registers the global LoggerProvider.
//
// Pipeline order (innermost to outermost):
//
//	OTLP exporter → BatchProcessor → ScrubLogProcessor → LoggerProvider
//
// ScrubLogProcessor wraps the BatchProcessor so all log records have PII and
// secrets redacted before they reach the batch queue and leave the process
// boundary (fix SEC-001, mirrors the ScrubSpanExporter pattern in initTracer).
func initLogger(ctx context.Context, res *resource.Resource) (ShutdownFunc, error) {
	exporter, err := otlploghttp.New(ctx)
	if err != nil {
		return nil, err
	}

	// fix(observability-SEC-001): wrap the batch processor with the scrub layer
	// so secrets/PII are redacted before log records are queued for export.
	batchProc := sdklog.NewBatchProcessor(exporter)
	scrubbedProc := NewScrubLogProcessor(batchProc)

	lp := sdklog.NewLoggerProvider(
		sdklog.WithResource(res),
		sdklog.WithProcessor(scrubbedProc),
	)
	// PR-B will wire the global LoggerProvider into slog.
	// For now we store it so other packages can retrieve it.
	globalLoggerProvider.Store(lp)
	return lp.Shutdown, nil
}

// initMeter creates and registers the global MeterProvider.
func initMeter(ctx context.Context, res *resource.Resource) (ShutdownFunc, error) {
	exporter, err := otlpmetrichttp.New(ctx)
	if err != nil {
		return nil, err
	}

	reader := sdkmetric.NewPeriodicReader(exporter,
		sdkmetric.WithInterval(60*time.Second),
	)

	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(reader),
		sdkmetric.WithResource(res),
	)
	otel.SetMeterProvider(mp)
	globalMeterProvider.Store(mp)
	return mp.Shutdown, nil
}

// combineShutdowns returns a single ShutdownFunc that invokes each shutdown
// in order, collects errors, and honours the context deadline.  Each provider
// gets at most shutdownTimeout to complete (AC-A.4).
func combineShutdowns(fns ...ShutdownFunc) ShutdownFunc {
	return func(parent context.Context) error {
		var errs []error
		for _, fn := range fns {
			ctx, cancel := context.WithTimeout(parent, shutdownTimeout)
			if err := fn(ctx); err != nil {
				errs = append(errs, err)
			}
			cancel()
		}
		return errors.Join(errs...)
	}
}

// noopShutdown is returned when observability is disabled.
func noopShutdown(_ context.Context) error { return nil }
