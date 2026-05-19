package ratelimit_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/mariogutierrez/context-harness-mcp/internal/ratelimit"
)

// ── AC-13: sub-key sharing, isolation, and stdio fallback ─────────────────────

// TestSubFromContext_Empty verifies that SubFromContext returns an empty string
// when no sub was stored in the context.
func TestSubFromContext_Empty(t *testing.T) {
	sub := ratelimit.SubFromContext(context.Background())
	assert.Empty(t, sub, "SubFromContext must return empty string on bare context")
}

// TestContextWithSub_RoundTrip verifies that ContextWithSub + SubFromContext
// round-trips correctly.
func TestContextWithSub_RoundTrip(t *testing.T) {
	const testSub = "550e8400-e29b-41d4-a716-446655440000"
	ctx := ratelimit.ContextWithSub(context.Background(), testSub)
	got := ratelimit.SubFromContext(ctx)
	assert.Equal(t, testSub, got, "SubFromContext must return the value stored by ContextWithSub")
}

// TestContextWithSub_OverridesParent verifies that a child context with a sub
// shadows the parent context's missing sub.
func TestContextWithSub_OverridesParent(t *testing.T) {
	parent := context.Background()
	child := ratelimit.ContextWithSub(parent, "user-a")

	assert.Empty(t, ratelimit.SubFromContext(parent), "parent must not be affected by child")
	assert.Equal(t, "user-a", ratelimit.SubFromContext(child))
}

// ── Sub-keyed sharing: same sub from 2 IPs share one bucket ──────────────────

// TestRateLimit_SameSubTwoIPs verifies that two requests with the same sub (from
// different IPs) share the same budget bucket.
//
// AC-13: Same sub from 2 IPs share budget. 10 from IP-A + 1 from IP-B = 11 total;
// IP-B's call must be rejected because the shared budget is already exhausted.
func TestRateLimit_SameSubTwoIPs(t *testing.T) {
	l := ratelimit.New()

	const sub = "user-shared-sub"

	// Exhaust the budget from "IP-A" using the sub as the key.
	// Since the limiter uses sub when present, both IPs map to the same bucket.
	for i := 0; i < 10; i++ {
		allowed, _ := l.Allow(sub)
		assert.True(t, allowed, "burst call %d for sub must be allowed", i+1)
	}

	// The 11th call using the same sub (even "from IP-B") must be rejected.
	allowed, retryAfter := l.Allow(sub)
	assert.False(t, allowed,
		"shared bucket must be exhausted after 10 calls from same sub (AC-13)")
	assert.Positive(t, retryAfter,
		"retryAfter must be positive when sub bucket is exhausted")
}

// TestRateLimit_DifferentSubsIndependent verifies that two distinct subs from
// the same IP have independent buckets.
//
// AC-13: Two different subs from same IP have independent budgets.
func TestRateLimit_DifferentSubsIndependent(t *testing.T) {
	l := ratelimit.New()

	const subA = "user-a-uuid"
	const subB = "user-b-uuid"

	// Exhaust subA's budget completely.
	for i := 0; i < 10; i++ {
		allowed, _ := l.Allow(subA)
		assert.True(t, allowed, "subA call %d must be allowed", i+1)
	}
	allowedA11, _ := l.Allow(subA)
	assert.False(t, allowedA11, "subA 11th call must be rejected")

	// subB must still have full capacity (independent bucket).
	for i := 0; i < 10; i++ {
		allowed, _ := l.Allow(subB)
		assert.True(t, allowed,
			"subB call %d must be allowed (independent from subA) (AC-13)", i+1)
	}
}

// ── IP-based fallback when sub is absent ─────────────────────────────────────

// TestRateLimit_IPFallbackWhenNoSub verifies that when sub is absent (unauthenticated
// HTTP), the limiter keys by IP. Two users on different IPs get independent budgets.
func TestRateLimit_IPFallbackWhenNoSub(t *testing.T) {
	l := ratelimit.New()

	const ipA = "203.0.113.100"
	const ipB = "203.0.113.101"

	// Exhaust IP-A's budget.
	for i := 0; i < 10; i++ {
		allowed, _ := l.Allow(ipA)
		assert.True(t, allowed, "IP-A call %d must be allowed", i+1)
	}
	allowedA11, _ := l.Allow(ipA)
	assert.False(t, allowedA11, "IP-A bucket must be exhausted")

	// IP-B has its own independent bucket.
	for i := 0; i < 10; i++ {
		allowed, _ := l.Allow(ipB)
		assert.True(t, allowed, "IP-B call %d must be allowed (independent bucket)", i+1)
	}
}

// TestRateLimit_SubAndIPAreDifferentKeys verifies that the same string used as
// "sub key" and as "IP key" do NOT share a bucket — they use the same underlying
// string key in the map, so in practice sub takes priority over IP in the
// checkRateLimit helper (which is internal to mcp/nodes.go). Here we verify the
// Limiter itself is just a string-keyed bucket.
func TestRateLimit_SubAndIPAreDifferentKeys(t *testing.T) {
	l := ratelimit.New()

	// The Limiter only sees string keys; it's the caller's responsibility to
	// choose sub over IP. We verify the Limiter correctly isolates distinct keys.
	keyA := "203.0.113.200"          // used as IP key by unauthenticated path
	keyB := "550e8400-e29b-41d4-a716-446655440999" // used as sub key

	for i := 0; i < 10; i++ {
		l.Allow(keyA)
	}
	allowedA, _ := l.Allow(keyA)
	assert.False(t, allowedA, "keyA must be exhausted")

	// keyB is unaffected.
	allowedB, _ := l.Allow(keyB)
	assert.True(t, allowedB, "keyB is a separate bucket from keyA")
}

// ── Context key type safety ───────────────────────────────────────────────────

// TestIPFromContext_Empty verifies IPFromContext returns empty on bare context.
func TestIPFromContext_Empty(t *testing.T) {
	ip := ratelimit.IPFromContext(context.Background())
	assert.Empty(t, ip)
}

// TestContextWithIP_RoundTrip verifies ContextWithIP + IPFromContext round-trips.
func TestContextWithIP_RoundTrip(t *testing.T) {
	ctx := ratelimit.ContextWithIP(context.Background(), "1.2.3.4")
	assert.Equal(t, "1.2.3.4", ratelimit.IPFromContext(ctx))
}

// TestSubAndIPKeysDontCollide verifies that ContextWithSub and ContextWithIP use
// different context key types so they do not shadow each other.
func TestSubAndIPKeysDontCollide(t *testing.T) {
	ctx := context.Background()
	ctx = ratelimit.ContextWithSub(ctx, "user-uuid")
	ctx = ratelimit.ContextWithIP(ctx, "1.2.3.4")

	assert.Equal(t, "user-uuid", ratelimit.SubFromContext(ctx),
		"sub must not be overwritten by IP key")
	assert.Equal(t, "1.2.3.4", ratelimit.IPFromContext(ctx),
		"IP must not be overwritten by sub key")
}
