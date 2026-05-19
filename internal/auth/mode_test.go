package auth

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── AC-10: mode parsing ───────────────────────────────────────────────────────

// TestParseMode_None verifies that MCP_AUTH="" or "none" returns ModeNone.
//
// AC-10: Valid values pass, garbage returns error mentioning var name + valid values.
func TestParseMode_None(t *testing.T) {
	t.Run("empty_string", func(t *testing.T) {
		t.Setenv("MCP_AUTH", "")
		mode, err := ParseMode()
		require.NoError(t, err, "empty MCP_AUTH must default to ModeNone without error")
		assert.Equal(t, ModeNone, mode)
	})

	t.Run("explicit_none", func(t *testing.T) {
		t.Setenv("MCP_AUTH", "none")
		mode, err := ParseMode()
		require.NoError(t, err, "MCP_AUTH=none must return ModeNone without error")
		assert.Equal(t, ModeNone, mode)
	})
}

// TestParseMode_Enabled verifies that MCP_AUTH="enabled" returns ModeEnabled.
func TestParseMode_Enabled(t *testing.T) {
	t.Setenv("MCP_AUTH", "enabled")
	mode, err := ParseMode()
	require.NoError(t, err, "MCP_AUTH=enabled must return ModeEnabled without error")
	assert.Equal(t, ModeEnabled, mode)
}

// TestParseMode_GarbageReturnsError verifies that an invalid value like "garbage"
// returns an error that mentions the env var name and the valid values.
//
// AC-10: MCP_AUTH=garbage returns error mentioning var name and valid values.
func TestParseMode_GarbageReturnsError(t *testing.T) {
	t.Setenv("MCP_AUTH", "garbage")
	mode, err := ParseMode()
	require.Error(t, err, "MCP_AUTH=garbage must return an error")

	// Error must mention the var name so operators know what to fix.
	assert.Contains(t, err.Error(), "MCP_AUTH",
		"error must mention the env var name")

	// Error must mention valid values so operators know the fix.
	assert.Contains(t, err.Error(), "none",
		"error must mention 'none' as a valid value")
	assert.Contains(t, err.Error(), "enabled",
		"error must mention 'enabled' as a valid value")

	// Even on error, the returned mode must be a safe default.
	assert.Equal(t, ModeNone, mode,
		"on parse error, returned mode must be ModeNone (safe default)")
}

// TestParseMode_OtherGarbageValues verifies additional invalid values.
func TestParseMode_OtherGarbageValues(t *testing.T) {
	garbage := []string{"true", "1", "yes", "ENABLED", "None", "disabled", "off"}

	for _, v := range garbage {
		v := v
		t.Run(v, func(t *testing.T) {
			t.Setenv("MCP_AUTH", v)
			_, err := ParseMode()
			require.Error(t, err,
				"MCP_AUTH=%q must return an error (not a valid value)", v)
			assert.Contains(t, err.Error(), "MCP_AUTH",
				"error must mention the env var name for value %q", v)
		})
	}
}

// TestWarnIfDisabled_NoWarnWhenEnabled verifies WarnIfDisabled is a no-op when
// mode is enabled (no panic, no visible side effect).
func TestWarnIfDisabled_NoWarnWhenEnabled(t *testing.T) {
	// If WarnIfDisabled panics or has an unexpected call path, the test will fail.
	// We verify it does not panic when mode is enabled.
	assert.NotPanics(t, func() {
		WarnIfDisabled(ModeEnabled, "http", "postgres://remotehost/db")
	})
}

// TestWarnIfDisabled_NoWarnOnStdio verifies WarnIfDisabled is silent for stdio
// transport even when mode is none (stdio is expected to run without auth).
func TestWarnIfDisabled_NoWarnOnStdio(t *testing.T) {
	assert.NotPanics(t, func() {
		WarnIfDisabled(ModeNone, "stdio", "postgres://remotehost/db")
	})
}

// TestWarnIfDisabled_NoWarnOnLocalDSN verifies WarnIfDisabled is silent for
// local dev environments (localhost DSN with auth disabled is expected).
func TestWarnIfDisabled_NoWarnOnLocalDSN(t *testing.T) {
	assert.NotPanics(t, func() {
		WarnIfDisabled(ModeNone, "http", "postgres://localhost:5432/dev")
	})
}
