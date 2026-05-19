package auth

import (
	"sync"
	"time"
)

const (
	// revocationTTL is how long a cache entry is considered fresh.
	// After 1h, the middleware re-queries the DB on the next request.
	// Locked decision: 1h (raised from 30s by user).
	revocationTTL = time.Hour

	// cacheJanitorInterval controls how often the background goroutine sweeps
	// the map and removes entries whose TTL has elapsed.
	cacheJanitorInterval = 10 * time.Minute
)

// cacheEntry holds the revocation state and the time it was cached.
type cacheEntry struct {
	revoked   bool
	cachedAt  time.Time
}

// RevocationCache is a thread-safe TTL cache for user revocation state.
// It follows a cache-aside pattern: the middleware queries the cache first;
// on a miss it queries the DB and populates the cache. The webhook handler
// calls Invalidate(sub) to drop a specific entry immediately, which reduces
// effective revocation latency from 1h to ~1s in the normal webhook path.
//
// The zero value is not usable; construct with NewRevocationCache.
type RevocationCache struct {
	mu      sync.Mutex
	entries map[string]*cacheEntry
}

// NewRevocationCache creates a RevocationCache and starts the background
// janitor goroutine that evicts expired entries.
func NewRevocationCache() *RevocationCache {
	c := &RevocationCache{entries: make(map[string]*cacheEntry)}
	go c.janitor()
	return c
}

// Get returns (revoked, true) when a fresh entry exists for sub, or
// (false, false) when the entry is absent or expired (cache miss).
func (c *RevocationCache) Get(sub string) (revoked bool, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	e, exists := c.entries[sub]
	if !exists {
		return false, false
	}
	if time.Since(e.cachedAt) > revocationTTL {
		delete(c.entries, sub)
		return false, false
	}
	return e.revoked, true
}

// Set stores the revocation state for sub with the current time as the
// cache timestamp. Subsequent Get calls within 1h will return this value
// without querying the DB.
func (c *RevocationCache) Set(sub string, revoked bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[sub] = &cacheEntry{revoked: revoked, cachedAt: time.Now()}
}

// Invalidate removes the cache entry for sub immediately, forcing the next
// middleware check to re-query the DB. Called by the webhook handler after
// updating users.revoked_at, reducing effective revocation latency to ~1s.
func (c *RevocationCache) Invalidate(sub string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, sub)
}

// janitor runs in a background goroutine and removes entries whose TTL has
// elapsed, preventing unbounded memory growth over time.
func (c *RevocationCache) janitor() {
	ticker := time.NewTicker(cacheJanitorInterval)
	defer ticker.Stop()
	for range ticker.C {
		c.evictExpired()
	}
}

// evictExpired removes all entries older than revocationTTL.
func (c *RevocationCache) evictExpired() {
	cutoff := time.Now().Add(-revocationTTL)
	c.mu.Lock()
	defer c.mu.Unlock()
	for sub, e := range c.entries {
		if e.cachedAt.Before(cutoff) {
			delete(c.entries, sub)
		}
	}
}
