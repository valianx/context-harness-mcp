package ratelimit_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariogutierrez/context-harness-mcp/internal/ratelimit"
)

// ── AC-14: stdio count-based burst + refill scenario ─────────────────────────

// TestStdioBucket_DefaultBurst verifies that with MCP_STDIO_RATE_LIMIT=1000,
// exactly 1000 calls pass (burst) and the 1001st is rejected.
// This is a count-based assertion — no wall-clock timing involved.
//
// AC-14: 1000 burst pass, remaining ≥100 get rate-limited.
func TestStdioBucket_DefaultBurst(t *testing.T) {
	t.Setenv("MCP_STDIO_RATE_LIMIT", "1000")

	bucket := ratelimit.NewStdioBucket()

	const burst = 1000

	passed := 0
	for i := 0; i < burst; i++ {
		allowed, _ := bucket.Allow()
		if allowed {
			passed++
		}
	}

	assert.Equal(t, burst, passed,
		"all %d burst calls must pass when bucket starts full", burst)

	// The 1001st call must be rejected (bucket exhausted).
	allowed, retryAfter := bucket.Allow()
	assert.False(t, allowed, "call after burst must be rejected")
	assert.Positive(t, retryAfter, "retryAfter must be positive when rate-limited")
}

// TestStdioBucket_BurstThen100Rejected verifies that after exhausting the burst
// (1000 tokens), the next N calls (≥100 consecutive) are all rejected.
// Count-based — no wall-clock assertion.
//
// AC-14: After burst exhaustion, ≥100 calls get rate-limited consecutively.
func TestStdioBucket_BurstThen100Rejected(t *testing.T) {
	t.Setenv("MCP_STDIO_RATE_LIMIT", "1000")

	bucket := ratelimit.NewStdioBucket()

	// Exhaust the burst.
	for i := 0; i < 1000; i++ {
		allowed, _ := bucket.Allow()
		require.True(t, allowed, "burst call %d must be allowed", i+1)
	}

	// The next 100 calls must ALL be rejected (refill is 100/s but we do not sleep).
	const numRejected = 100
	for i := 0; i < numRejected; i++ {
		allowed, _ := bucket.Allow()
		assert.False(t, allowed,
			"call %d after burst exhaustion must be rejected (AC-14)", i+1)
	}
}

// TestStdioBucket_Disabled verifies that MCP_STDIO_RATE_LIMIT=0 disables the
// bucket (all calls pass).
func TestStdioBucket_Disabled(t *testing.T) {
	t.Setenv("MCP_STDIO_RATE_LIMIT", "0")

	bucket := ratelimit.NewStdioBucket()

	const callCount = 5000
	for i := 0; i < callCount; i++ {
		allowed, _ := bucket.Allow()
		assert.True(t, allowed, "all calls must pass when rate-limit is disabled")
	}
}

// TestStdioBucket_UnsetDefaultsTo1000 verifies that an unset
// MCP_STDIO_RATE_LIMIT defaults to the 1000-token burst.
func TestStdioBucket_UnsetDefaultsTo1000(t *testing.T) {
	t.Setenv("MCP_STDIO_RATE_LIMIT", "")

	bucket := ratelimit.NewStdioBucket()

	// All 1000 burst calls must pass.
	passed := 0
	for i := 0; i < 1000; i++ {
		allowed, _ := bucket.Allow()
		if allowed {
			passed++
		}
	}
	assert.Equal(t, 1000, passed,
		"default burst (unset env) must be 1000")

	// 1001st must fail.
	allowed, _ := bucket.Allow()
	assert.False(t, allowed, "burst exhausted — next call must be rejected")
}

// TestStdioBucket_CustomBurst verifies that a custom burst (e.g. 50) is respected.
func TestStdioBucket_CustomBurst(t *testing.T) {
	t.Setenv("MCP_STDIO_RATE_LIMIT", "50")

	bucket := ratelimit.NewStdioBucket()

	passed := 0
	for i := 0; i < 50; i++ {
		allowed, _ := bucket.Allow()
		if allowed {
			passed++
		}
	}
	assert.Equal(t, 50, passed, "all 50 burst tokens must pass")

	allowed, _ := bucket.Allow()
	assert.False(t, allowed, "51st call must be rejected")
}

// TestNewStdioBucket_IndependentFromSingleton verifies that NewStdioBucket
// creates a fresh bucket each time, independent of the process singleton from
// InitStdio. This ensures tests are isolated from each other.
func TestNewStdioBucket_IndependentFromSingleton(t *testing.T) {
	t.Setenv("MCP_STDIO_RATE_LIMIT", "10")

	b1 := ratelimit.NewStdioBucket()
	b2 := ratelimit.NewStdioBucket()

	// Exhaust b1.
	for i := 0; i < 10; i++ {
		b1.Allow()
	}

	// b2 must still have full capacity.
	allowed, _ := b2.Allow()
	assert.True(t, allowed, "b2 must be independent of b1")
}
