// Package metrics provides Prometheus instrumentation for the context-harness-mcp
// server. All metrics are registered on a package-private *prometheus.Registry
// (not the default global) so multiple test instances can coexist without
// "duplicate metrics" panics.
//
// Metrics exposed:
//   - mcp_tool_calls_total{tool, status}      counter — tool invocations
//   - mcp_tool_duration_seconds{tool}         histogram — handler latency
//   - mcp_content_filter_rejects_total{code, layer} counter — policy/* rejections
//   - mcp_embedder_duration_seconds            histogram — ONNX encode latency
//   - mcp_jwt_validations_total{result}        counter — token validation outcome
//   - mcp_jwt_validation_duration_seconds      histogram — token validation latency
package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var registry = prometheus.NewRegistry()

// ToolCalls counts MCP tool invocations by tool name and status.
// Status values: success | error | policy_reject | rate_limited.
var ToolCalls = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "mcp_tool_calls_total",
		Help: "Total number of MCP tool invocations, labeled by tool and status.",
	},
	[]string{"tool", "status"},
)

// ToolDuration measures MCP tool handler latency by tool name.
var ToolDuration = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Name:    "mcp_tool_duration_seconds",
		Help:    "MCP tool handler latency.",
		Buckets: prometheus.DefBuckets,
	},
	[]string{"tool"},
)

// ContentFilterRejects counts Content-Filter rejections by policy code and layer.
var ContentFilterRejects = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "mcp_content_filter_rejects_total",
		Help: "Content-Filter rejections by policy code and layer.",
	},
	[]string{"code", "layer"},
)

// EmbedderDuration measures ONNX embedder Encode wall-clock duration.
var EmbedderDuration = prometheus.NewHistogram(
	prometheus.HistogramOpts{
		Name:    "mcp_embedder_duration_seconds",
		Help:    "ONNX embedder Encode wall-clock duration.",
		Buckets: []float64{0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1.0, 2.5, 5.0},
	},
)

// JWTValidations counts JWT validation outcomes by result label.
// Result values: valid | invalid_signature | expired | revoked | malformed.
var JWTValidations = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "mcp_jwt_validations_total",
		Help: "JWT validation outcomes.",
	},
	[]string{"result"},
)

// JWTValidationDuration measures JWT validation latency (sign verify + revocation cache lookup).
var JWTValidationDuration = prometheus.NewHistogram(
	prometheus.HistogramOpts{
		Name:    "mcp_jwt_validation_duration_seconds",
		Help:    "JWT validation latency (sign verify + revocation cache lookup).",
		Buckets: []float64{0.0001, 0.0005, 0.001, 0.005, 0.01, 0.05, 0.1},
	},
)

func init() {
	registry.MustRegister(
		ToolCalls,
		ToolDuration,
		ContentFilterRejects,
		EmbedderDuration,
		JWTValidations,
		JWTValidationDuration,
	)
}

// Handler returns the http.Handler that exposes Prometheus metrics in text format.
// Mount at /metrics, gated by MCP_EXPOSE_METRICS=1 env var (see cmd/server/main.go).
func Handler() http.Handler {
	return promhttp.HandlerFor(registry, promhttp.HandlerOpts{
		EnableOpenMetrics: true,
	})
}
