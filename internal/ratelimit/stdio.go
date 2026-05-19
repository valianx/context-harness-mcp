package ratelimit

import (
	"os"
	"strconv"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

const (
	// stdioDefaultBurst is the maximum burst for the stdio process-wide bucket.
	// Locked decision: 1000 writes/s burst per MCP_STDIO_RATE_LIMIT default.
	stdioDefaultBurst = 1000

	// stdioRefillPerBurst is the ratio used to derive the refill rate.
	// Refill rate = burst / 10 per second → default 100/s.
	stdioRefillPerBurst = 10
)

// StdioBucket is a process-wide token bucket for the stdio transport.
// All stdio write-tool calls share this single bucket regardless of tool type.
// HTTP transport uses the per-IP or per-sub Limiter instead.
//
// The zero value is not usable; construct with NewStdioBucket.
type StdioBucket struct {
	mu      sync.Mutex
	limiter *rate.Limiter
	// disabled is set to true when MCP_STDIO_RATE_LIMIT=0.
	disabled bool
}

// globalStdioBucket is the process-level singleton initialized by InitStdio.
var globalStdioBucket *StdioBucket

// once guards InitStdio against duplicate initialization.
var once sync.Once

// InitStdio initializes the process-wide stdio rate-limit bucket from the
// MCP_STDIO_RATE_LIMIT environment variable.
//
// MCP_STDIO_RATE_LIMIT=1000 → burst 1000, refill 100/s.
// MCP_STDIO_RATE_LIMIT=0    → disabled (all requests pass).
// Unset                      → same as 1000.
//
// This must be called once at server startup before any tool handler runs.
// Subsequent calls are no-ops (safe for test isolation use NewStdioBucket directly).
func InitStdio() *StdioBucket {
	once.Do(func() {
		globalStdioBucket = NewStdioBucket()
	})
	return globalStdioBucket
}

// NewStdioBucket creates a fresh StdioBucket reading MCP_STDIO_RATE_LIMIT.
// Use this in tests to get a clean bucket without the singleton.
func NewStdioBucket() *StdioBucket {
	raw := os.Getenv("MCP_STDIO_RATE_LIMIT")
	burst := stdioDefaultBurst
	if raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			burst = n
		}
	}

	if burst == 0 {
		return &StdioBucket{disabled: true}
	}

	refill := rate.Limit(float64(burst) / float64(stdioRefillPerBurst))
	return &StdioBucket{
		limiter: rate.NewLimiter(refill, burst),
	}
}

// Allow returns (true, 0) when the stdio bucket has capacity, or (false, delay)
// when the bucket is exhausted. When disabled, it always returns (true, 0).
func (b *StdioBucket) Allow() (allowed bool, retryAfter time.Duration) {
	if b.disabled || b.limiter == nil {
		return true, 0
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	r := b.limiter.Reserve()
	if r.OK() && r.Delay() == 0 {
		return true, 0
	}
	delay := r.Delay()
	r.Cancel()
	return false, delay
}
