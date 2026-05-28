package main

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"

	"github.com/mariogutierrez/context-harness-mcp/internal/observability"
)

// TestEnvOrDefault locks the precedence: env var (non-empty) > fallback.
// Regression test for the case where container hosting providers set
// MCP_TRANSPORT in env but the binary ignored it — the helper now serves
// only -transport (PORT handling is inlined for clarity).
func TestEnvOrDefault(t *testing.T) {
	const key = "CONTEXT_HARNESS_TEST_VAR"

	t.Run("env_unset_returns_fallback", func(t *testing.T) {
		t.Setenv(key, "") // ensure unset
		got := envOrDefault(key, "fallback-default")
		if got != "fallback-default" {
			t.Fatalf("want fallback-default, got %q", got)
		}
	})

	t.Run("env_set_returns_env_value", func(t *testing.T) {
		t.Setenv(key, "from-env")
		got := envOrDefault(key, "fallback-default")
		if got != "from-env" {
			t.Fatalf("want from-env, got %q", got)
		}
	})

	t.Run("env_empty_string_returns_fallback", func(t *testing.T) {
		// Empty-string env is treated as unset (matches the "if v != ''" check).
		t.Setenv(key, "")
		got := envOrDefault(key, "fallback-default")
		if got != "fallback-default" {
			t.Fatalf("want fallback-default for empty env, got %q", got)
		}
	})
}

// --- PR-H kill-switch tests ---

// TestApplyStdioGuard_SetsEnvVar covers AC-H.2: applyStdioGuard("stdio") must
// set OTEL_SDK_DISABLED=true so the subsequent observability.Init call always
// sees the kill-switch regardless of what CH_OBSERVABILITY_ENABLED is set to.
func TestApplyStdioGuard_SetsEnvVar(t *testing.T) {
	t.Setenv("OTEL_SDK_DISABLED", "") // ensure clean state

	applyStdioGuard("stdio")

	assert.Equal(t, "true", os.Getenv("OTEL_SDK_DISABLED"),
		"applyStdioGuard(stdio) must set OTEL_SDK_DISABLED=true")
}

// TestApplyStdioGuard_NoOp_ForHTTP covers AC-H.3: applyStdioGuard must NOT
// touch OTEL_SDK_DISABLED when transport is "http".
func TestApplyStdioGuard_NoOp_ForHTTP(t *testing.T) {
	t.Setenv("OTEL_SDK_DISABLED", "") // ensure clean state

	applyStdioGuard("http")

	assert.Empty(t, os.Getenv("OTEL_SDK_DISABLED"),
		"applyStdioGuard(http) must not set OTEL_SDK_DISABLED")
}

// TestApplyStdioGuard_PreservesExistingValue_ForHTTP verifies that when
// transport is "http" an already-set OTEL_SDK_DISABLED is left unchanged.
func TestApplyStdioGuard_PreservesExistingValue_ForHTTP(t *testing.T) {
	t.Setenv("OTEL_SDK_DISABLED", "true") // pre-set by operator

	applyStdioGuard("http")

	assert.Equal(t, "true", os.Getenv("OTEL_SDK_DISABLED"),
		"applyStdioGuard(http) must not clear an existing OTEL_SDK_DISABLED")
}

// TestStdioKillSwitch_InitProducesNoActiveSDKProvider covers AC-H.1: even when
// all observability env vars are set, the stdio guard ensures no real SDK
// TracerProvider is installed.
//
// Boot sequence simulated:
//  1. Set CH_OBSERVABILITY_ENABLED=true + OTLP endpoint + AXIOM_TOKEN.
//  2. Call applyStdioGuard("stdio") — sets OTEL_SDK_DISABLED=true.
//  3. Call observability.Init.
//  4. Assert the global TracerProvider produces only invalid (no-op) spans.
func TestStdioKillSwitch_InitProducesNoActiveSDKProvider(t *testing.T) {
	t.Setenv("CH_OBSERVABILITY_ENABLED", "true")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "https://api.axiom.co")
	t.Setenv("AXIOM_TOKEN", "xaat-test")
	t.Setenv("OTEL_SDK_DISABLED", "")

	// AC-H.2 ordering: guard runs BEFORE Init.
	applyStdioGuard("stdio")
	require.Equal(t, "true", os.Getenv("OTEL_SDK_DISABLED"),
		"pre-condition: guard must have fired before Init is called")

	shutdown, err := observability.Init(context.Background())

	// Two acceptable outcomes (both prove no OTLP connection):
	//   (a) err != nil with "scrubber" — boot guard fired, no exporter built.
	//   (b) err == nil — disabled path returned noopShutdown.
	// Any other error (network / TLS) indicates the guard failed.
	if err != nil {
		assert.Contains(t, err.Error(), "scrubber",
			"error must come from the boot guard, not from a network/TLS dial")
	} else {
		require.NotNil(t, shutdown)
		assert.NoError(t, shutdown(context.Background()))
	}

	// The global TracerProvider must not be a real SDK provider.
	// A real provider assigns a valid TraceID; the no-op provider returns an
	// invalid (zero) SpanContext.
	_, span := otel.Tracer("pr-h-test").Start(context.Background(), "probe")
	span.End()
	assert.False(t, span.SpanContext().IsValid(),
		"stdio mode: TracerProvider must be no-op; valid SpanContext means OTLP SDK was installed")
}

// TestHTTPTransport_GuardNotApplied covers AC-H.3 end-to-end: with
// transport=http, OTEL_SDK_DISABLED is absent after the guard — confirming
// observability.Init is free to activate providers when configured.
func TestHTTPTransport_GuardNotApplied(t *testing.T) {
	t.Setenv("CH_OBSERVABILITY_ENABLED", "true")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "https://api.axiom.co")
	t.Setenv("AXIOM_TOKEN", "xaat-test")
	t.Setenv("OTEL_SDK_DISABLED", "")

	applyStdioGuard("http")

	assert.Empty(t, os.Getenv("OTEL_SDK_DISABLED"),
		"HTTP transport: kill-switch must NOT be applied — OTEL_SDK_DISABLED must remain empty")
}

// TestShouldTraceHTTPPath covers AC-J.4: shouldTraceHTTPPath must return true
// for paths that carry real APM signal (/mcp, /viewer) and false for "no-work"
// paths that generate noise (healthchecks, static assets, OAuth flow, metrics).
//
// Default for unknown paths: the function falls through to `return true` so
// any path not explicitly excluded produces a span. This is intentional — an
// unrecognised route is never noisier than a real MCP call.
func TestShouldTraceHTTPPath(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		// Preserved paths — must produce spans.
		{"/mcp", true},
		{"/mcp/", true},
		{"/mcp/some/path", true},
		{"/viewer", true},
		{"/viewer/abc", true},

		// Excluded paths — must NOT produce spans.
		{"/", false},
		{"/favicon.ico", false},
		{"/auth/login", false},
		{"/auth/callback", false},
		{"/dashboard", false},
		{"/healthz", false},
		{"/debug/vars", false},
		{"/metrics", false},

		// Unknown paths fall through to the default (return true).
		{"/random/unknown", true},
	}

	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			got := shouldTraceHTTPPath(tc.path)
			assert.Equal(t, tc.want, got,
				"shouldTraceHTTPPath(%q) = %v, want %v", tc.path, got, tc.want)
		})
	}
}
