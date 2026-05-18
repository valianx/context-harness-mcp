// Package ratelimit implements a per-IP token-bucket rate limiter for the
// MCP write tools (create_entities, add_observations, create_relations).
// Reads and deletes are not rate-limited.
package ratelimit

import (
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

const (
	// writesPerWindow is the maximum number of write-tool calls allowed per IP
	// within any sliding window of writeWindow duration. The bucket holds this
	// many tokens, and each refill adds one token every second (steady-state
	// ≈ 10 writes / 10 s with burst of 10).
	writesPerWindow = 10

	// idleThreshold is how long a limiter must be unused before the janitor
	// removes it from the map. 5 minutes covers any reasonable client pause
	// between writes without letting the map grow unboundedly.
	idleThreshold = 5 * time.Minute

	// janitorInterval controls how often the background goroutine sweeps the
	// per-IP map and evicts idle limiters. 60 seconds is cheap and keeps memory
	// bounded even under sustained traffic from many distinct IPs.
	janitorInterval = 60 * time.Second
)

// entry pairs a rate.Limiter with a lastUsed timestamp so the janitor can
// evict entries that have been idle for more than idleThreshold.
type entry struct {
	limiter  *rate.Limiter
	lastUsed time.Time
}

// Limiter is a per-IP token-bucket rate limiter. The zero value is not usable;
// construct with New.
type Limiter struct {
	mu      sync.Mutex
	entries map[string]*entry
}

// New creates a Limiter and starts the background janitor goroutine.
func New() *Limiter {
	l := &Limiter{entries: make(map[string]*entry)}
	go l.janitor()
	return l
}

// Allow checks whether the given IP may perform a write. It returns (true, 0)
// when a token is available, or (false, retryAfter) when the bucket is empty.
// retryAfter is the approximate time the caller should wait before retrying —
// it is non-zero only when allowed is false.
func (l *Limiter) Allow(ip string) (allowed bool, retryAfter time.Duration) {
	e := l.getOrCreate(ip)
	r := e.limiter.Reserve()
	if r.OK() && r.Delay() == 0 {
		return true, 0
	}
	// The reservation would require waiting — cancel it and report the delay.
	delay := r.Delay()
	r.Cancel()
	return false, delay
}

// getOrCreate returns the entry for ip, creating one if it does not exist.
func (l *Limiter) getOrCreate(ip string) *entry {
	l.mu.Lock()
	defer l.mu.Unlock()

	e, ok := l.entries[ip]
	if !ok {
		// rate.Every(time.Second) = 1 token/second refill rate.
		// writesPerWindow = 10 token burst capacity.
		e = &entry{limiter: rate.NewLimiter(rate.Every(time.Second), writesPerWindow)}
		l.entries[ip] = e
	}
	e.lastUsed = time.Now()
	return e
}

// janitor runs in a background goroutine and removes entries that have not
// been touched for idleThreshold, preventing unbounded memory growth from
// one-off IPs that never return.
func (l *Limiter) janitor() {
	ticker := time.NewTicker(janitorInterval)
	defer ticker.Stop()
	for range ticker.C {
		l.evictIdle()
	}
}

// evictIdle removes all entries whose lastUsed is older than idleThreshold.
func (l *Limiter) evictIdle() {
	cutoff := time.Now().Add(-idleThreshold)
	l.mu.Lock()
	defer l.mu.Unlock()
	for ip, e := range l.entries {
		if e.lastUsed.Before(cutoff) {
			delete(l.entries, ip)
		}
	}
}

// ExtractClientIP reads the client's real IP from the request.
// It prefers the leftmost public IP from X-Forwarded-For (Render passes the
// original client IP there, possibly with a proxy chain). Falls back to
// r.RemoteAddr (with port stripped) for direct connections where XFF is absent.
func ExtractClientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		// XFF is a comma-separated list: "client, proxy1, proxy2".
		// The leftmost entry is the original client IP.
		first := strings.SplitN(xff, ",", 2)[0]
		return strings.TrimSpace(first)
	}

	// Strip port from RemoteAddr ("1.2.3.4:port" or "[::1]:port").
	addr := r.RemoteAddr
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		return addr[:i]
	}
	return addr
}
