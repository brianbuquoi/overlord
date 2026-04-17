package api

import (
	"context"
	"sync"
	"time"
)

// tokenBucket implements a per-key token bucket rate limiter using stdlib only.
type tokenBucket struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	rate    float64       // tokens per second
	burst   int           // max tokens (bucket capacity)
	cleanup time.Duration // how often to sweep stale buckets
	now     func() time.Time
}

type bucket struct {
	tokens   float64
	lastSeen time.Time
}

// newTokenBucket constructs a rate limiter. The background cleanup
// goroutine runs until ctx is cancelled — short-lived servers or tests
// must cancel ctx to release it (the audit flagged the prior
// no-stop-channel shape as a goroutine leak).
func newTokenBucket(ctx context.Context, rate float64, burst int) *tokenBucket {
	tb := &tokenBucket{
		buckets: make(map[string]*bucket),
		rate:    rate,
		burst:   burst,
		cleanup: 5 * time.Minute,
		now:     time.Now,
	}
	go tb.cleanupLoop(ctx)
	return tb
}

// allow returns true if the key has tokens remaining.
func (tb *tokenBucket) allow(key string) bool {
	tb.mu.Lock()
	defer tb.mu.Unlock()

	now := tb.now()
	b, ok := tb.buckets[key]
	if !ok {
		b = &bucket{tokens: float64(tb.burst), lastSeen: now}
		tb.buckets[key] = b
	}

	// Refill tokens based on elapsed time.
	elapsed := now.Sub(b.lastSeen).Seconds()
	b.tokens += elapsed * tb.rate
	if b.tokens > float64(tb.burst) {
		b.tokens = float64(tb.burst)
	}
	b.lastSeen = now

	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// cleanupLoop periodically removes stale entries. Exits cleanly when
// ctx is cancelled so tests and short-lived servers do not leak the
// goroutine.
func (tb *tokenBucket) cleanupLoop(ctx context.Context) {
	ticker := time.NewTicker(tb.cleanup)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			tb.mu.Lock()
			cutoff := time.Now().Add(-tb.cleanup)
			for k, b := range tb.buckets {
				if b.lastSeen.Before(cutoff) {
					delete(tb.buckets, k)
				}
			}
			tb.mu.Unlock()
		case <-ctx.Done():
			return
		}
	}
}
