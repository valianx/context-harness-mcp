package main

import (
	"testing"
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
