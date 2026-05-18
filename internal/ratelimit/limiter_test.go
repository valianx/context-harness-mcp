package ratelimit_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariogutierrez/context-harness-mcp/internal/ratelimit"
)

// TestAllow_FirstTenPass verifies that the first 10 calls from the same IP
// are all allowed (the bucket starts full).
func TestAllow_FirstTenPass(t *testing.T) {
	l := ratelimit.New()
	ip := "203.0.113.10" // RFC 5737 TEST-NET — never a real IP

	for i := 0; i < 10; i++ {
		allowed, _ := l.Allow(ip)
		assert.True(t, allowed, "call %d of 10 from the same IP must be allowed", i+1)
	}
}

// TestAllow_EleventhRejected verifies that the 11th consecutive call from the
// same IP is rejected and that retryAfter is positive.
func TestAllow_EleventhRejected(t *testing.T) {
	l := ratelimit.New()
	ip := "203.0.113.11"

	for i := 0; i < 10; i++ {
		allowed, _ := l.Allow(ip)
		require.True(t, allowed, "first 10 calls must be allowed (call %d)", i+1)
	}

	allowed, retryAfter := l.Allow(ip)
	assert.False(t, allowed, "11th call must be rejected")
	assert.Positive(t, retryAfter, "retryAfter must be > 0 when rejected")
}

// TestAllow_DifferentIPsIndependent verifies that two distinct IPs each get
// their own 10-token bucket — 10 calls from IP-A plus 10 calls from IP-B
// = 20 total, all allowed.
func TestAllow_DifferentIPsIndependent(t *testing.T) {
	l := ratelimit.New()
	ipA := "203.0.113.20"
	ipB := "203.0.113.21"

	for i := 0; i < 10; i++ {
		allowedA, _ := l.Allow(ipA)
		assert.True(t, allowedA, "IP-A call %d must be allowed", i+1)

		allowedB, _ := l.Allow(ipB)
		assert.True(t, allowedB, "IP-B call %d must be allowed", i+1)
	}
}

// TestAllow_RecoversOverTime verifies that after exhausting the bucket (10
// calls), waiting 1.1 seconds allows at least one more call (the refill rate
// is 1 token/second).
func TestAllow_RecoversOverTime(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping timing-sensitive test in -short mode")
	}

	l := ratelimit.New()
	ip := "203.0.113.30"

	// Exhaust the bucket.
	for i := 0; i < 10; i++ {
		allowed, _ := l.Allow(ip)
		require.True(t, allowed, "first 10 calls must be allowed")
	}

	// 11th call must be rejected.
	allowed, _ := l.Allow(ip)
	require.False(t, allowed, "11th call must be rejected immediately after exhaustion")

	// Wait 1.1 s — at least 1 token should have refilled.
	time.Sleep(1100 * time.Millisecond)

	allowed, _ = l.Allow(ip)
	assert.True(t, allowed, "after 1.1s wait, one refilled token must allow the next call")
}
