package metrics_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariogutierrez/context-harness-mcp/internal/metrics"
)

// TestHandler_returns_200_with_text_plain verifies that Handler() returns HTTP 200
// with a Content-Type that starts with "text/plain" (Prometheus text format).
func TestHandler_returns_200_with_text_plain(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rr := httptest.NewRecorder()

	metrics.Handler().ServeHTTP(rr, req)

	assert.Equal(t, http.StatusOK, rr.Code)
	contentType := rr.Header().Get("Content-Type")
	assert.True(t,
		strings.HasPrefix(contentType, "text/plain") || strings.HasPrefix(contentType, "application/openmetrics-text"),
		"Content-Type must be text/plain or openmetrics, got: %s", contentType,
	)
}

// TestHandler_reflects_tool_calls_counter verifies that after incrementing
// ToolCalls the response body contains the expected Prometheus counter line.
func TestHandler_reflects_tool_calls_counter(t *testing.T) {
	metrics.ToolCalls.WithLabelValues("foo", "success").Inc()

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rr := httptest.NewRecorder()
	metrics.Handler().ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	body := rr.Body.String()

	// The Prometheus text format emits labels in sorted order: status before tool.
	assert.Contains(t, body, `mcp_tool_calls_total`,
		"body must contain the counter name")
	assert.Contains(t, body, `tool="foo"`,
		"body must contain the tool label")
	assert.Contains(t, body, `status="success"`,
		"body must contain the status label")
}

// TestHandler_reflects_embedder_duration_histogram verifies that after observing
// a value on EmbedderDuration the response body contains histogram lines for
// mcp_embedder_duration_seconds.
func TestHandler_reflects_embedder_duration_histogram(t *testing.T) {
	metrics.EmbedderDuration.Observe(0.05)

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rr := httptest.NewRecorder()
	metrics.Handler().ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	body := rr.Body.String()

	assert.Contains(t, body, "mcp_embedder_duration_seconds",
		"body must contain the embedder histogram name")
	// OpenMetrics format emits _bucket, _sum, _count (or suffixed with _total for counters).
	assert.True(t,
		strings.Contains(body, "mcp_embedder_duration_seconds_bucket") ||
			strings.Contains(body, "mcp_embedder_duration_seconds{"),
		"body must contain histogram bucket lines")
}
