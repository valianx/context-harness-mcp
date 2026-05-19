package auth

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── AC-6 (cache path): RevocationCache unit tests ────────────────────────────

// TestRevocationCache_GetMiss verifies that a cache miss returns (false, false).
func TestRevocationCache_GetMiss(t *testing.T) {
	c := &RevocationCache{entries: make(map[string]*cacheEntry)}

	revoked, ok := c.Get("unknown-sub")
	assert.False(t, revoked)
	assert.False(t, ok, "get on missing entry must return ok=false")
}

// TestRevocationCache_SetAndGet verifies that Set followed by Get within TTL
// returns the stored value.
func TestRevocationCache_SetAndGet(t *testing.T) {
	c := &RevocationCache{entries: make(map[string]*cacheEntry)}

	const sub = "test-user-uuid-1"
	c.Set(sub, true)

	revoked, ok := c.Get(sub)
	assert.True(t, ok, "entry must be found immediately after Set")
	assert.True(t, revoked, "revocation state must match what was Set")
}

// TestRevocationCache_SetAndGet_NotRevoked verifies Set(sub, false) is also cached.
func TestRevocationCache_SetAndGet_NotRevoked(t *testing.T) {
	c := &RevocationCache{entries: make(map[string]*cacheEntry)}

	const sub = "test-user-uuid-2"
	c.Set(sub, false)

	revoked, ok := c.Get(sub)
	assert.True(t, ok, "entry must be found immediately after Set")
	assert.False(t, revoked, "revocation=false must be stored and retrieved correctly")
}

// TestRevocationCache_TTLExpiry verifies that a cache entry older than 1h is
// treated as a miss without sleeping for 1h.
//
// Implementation note: since the test is in package auth (same package), it can
// directly set cachedAt to a time in the past to simulate TTL expiry.
// This avoids any real sleep and is deterministic.
func TestRevocationCache_TTLExpiry(t *testing.T) {
	c := &RevocationCache{entries: make(map[string]*cacheEntry)}

	const sub = "test-user-expired"

	// Plant an entry with cachedAt more than 1h ago, bypassing Set's time.Now().
	c.mu.Lock()
	c.entries[sub] = &cacheEntry{
		revoked:  true,
		cachedAt: time.Now().Add(-revocationTTL - time.Second), // 1h+1s ago
	}
	c.mu.Unlock()

	revoked, ok := c.Get(sub)
	assert.False(t, ok, "entry older than TTL must be a cache miss")
	assert.False(t, revoked, "expired entry must not return revoked=true")

	// The entry must be deleted when the TTL expires (Get does lazy eviction).
	c.mu.Lock()
	_, stillExists := c.entries[sub]
	c.mu.Unlock()
	assert.False(t, stillExists, "Get must delete the expired entry from the map")
}

// TestRevocationCache_WithinTTL verifies that an entry just under the TTL boundary
// is still returned as a hit.
func TestRevocationCache_WithinTTL(t *testing.T) {
	c := &RevocationCache{entries: make(map[string]*cacheEntry)}

	const sub = "test-user-fresh"

	// 59 minutes ago — within 1h TTL.
	c.mu.Lock()
	c.entries[sub] = &cacheEntry{
		revoked:  true,
		cachedAt: time.Now().Add(-59 * time.Minute),
	}
	c.mu.Unlock()

	revoked, ok := c.Get(sub)
	assert.True(t, ok, "entry younger than TTL must be a cache hit")
	assert.True(t, revoked, "revocation state must be preserved within TTL")
}

// TestRevocationCache_Invalidate verifies that Invalidate removes the entry
// immediately, forcing the next Get to be a miss.
func TestRevocationCache_Invalidate(t *testing.T) {
	c := &RevocationCache{entries: make(map[string]*cacheEntry)}

	const sub = "test-user-invalidate"
	c.Set(sub, true)

	// Confirm it was cached.
	_, ok := c.Get(sub)
	require.True(t, ok, "entry must exist before Invalidate")

	c.Invalidate(sub)

	revoked, ok := c.Get(sub)
	assert.False(t, ok, "Invalidate must cause the next Get to be a miss")
	assert.False(t, revoked)
}

// TestRevocationCache_InvalidateNonExistent verifies Invalidate on a
// non-existent key does not panic.
func TestRevocationCache_InvalidateNonExistent(t *testing.T) {
	c := &RevocationCache{entries: make(map[string]*cacheEntry)}

	assert.NotPanics(t, func() {
		c.Invalidate("sub-that-was-never-cached")
	})
}

// TestRevocationCache_ConcurrentSafe verifies that concurrent reads and writes
// do not trigger the race detector. Run with: go test -race ./internal/auth/...
func TestRevocationCache_ConcurrentSafe(t *testing.T) {
	c := NewRevocationCache()

	const (
		numGoroutines = 50
		opsPerRoutine = 20
	)

	var wg sync.WaitGroup
	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			sub := "concurrent-sub"
			for j := 0; j < opsPerRoutine; j++ {
				switch j % 3 {
				case 0:
					c.Set(sub, j%2 == 0)
				case 1:
					c.Get(sub)
				case 2:
					c.Invalidate(sub)
				}
			}
		}(i)
	}
	wg.Wait()
	// If the race detector fires, the test binary will abort with a message.
	// Reaching here means concurrent access was safe.
}

// TestRevocationCache_EvictExpired verifies the evictExpired helper removes
// stale entries and preserves fresh ones.
func TestRevocationCache_EvictExpired(t *testing.T) {
	c := &RevocationCache{entries: make(map[string]*cacheEntry)}

	const staleSub = "stale-user"
	const freshSub = "fresh-user"

	// Plant stale entry (older than TTL).
	c.mu.Lock()
	c.entries[staleSub] = &cacheEntry{
		revoked:  true,
		cachedAt: time.Now().Add(-revocationTTL - time.Second),
	}
	// Plant fresh entry (within TTL).
	c.entries[freshSub] = &cacheEntry{
		revoked:  false,
		cachedAt: time.Now().Add(-30 * time.Minute),
	}
	c.mu.Unlock()

	c.evictExpired()

	c.mu.Lock()
	_, staleExists := c.entries[staleSub]
	_, freshExists := c.entries[freshSub]
	c.mu.Unlock()

	assert.False(t, staleExists, "stale entry must be removed by evictExpired")
	assert.True(t, freshExists, "fresh entry must survive evictExpired")
}
