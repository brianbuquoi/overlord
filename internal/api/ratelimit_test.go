package api

import (
	"context"
	"runtime"
	"testing"
	"time"
)

// TestTokenBucket_CleanupStopsOnContextCancel is the SEC-014 regression.
// The prior shape started cleanupLoop with no stop mechanism, so every
// short-lived server or test that constructed a limiter leaked a
// background goroutine. Cancelling the context must release the
// goroutine deterministically.
func TestTokenBucket_CleanupStopsOnContextCancel(t *testing.T) {
	base := runtime.NumGoroutine()

	ctx, cancel := context.WithCancel(context.Background())
	tb := newTokenBucket(ctx, 10, 10)
	// Force a short cleanup interval so the loop is actually running
	// against the ticker rather than sitting on the first wake-up.
	tb.cleanup = 10 * time.Millisecond

	// Allow the goroutine to start and loop at least once.
	if !tb.allow("k") {
		t.Fatal("first allow must succeed")
	}
	time.Sleep(50 * time.Millisecond)

	peak := runtime.NumGoroutine()
	if peak <= base {
		t.Fatalf("expected goroutine count to grow; base=%d peak=%d", base, peak)
	}

	cancel()

	// Poll for the goroutine to exit rather than sleep a fixed duration.
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= base {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("cleanup goroutine did not exit after ctx cancel; base=%d current=%d", base, runtime.NumGoroutine())
}

// TestTokenBucket_AllowBasics confirms the core rate-limit behavior is
// unchanged by the context-aware cleanup refactor.
func TestTokenBucket_AllowBasics(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	tb := newTokenBucket(ctx, 1, 2)
	if !tb.allow("client") {
		t.Fatal("allow 1/2 must succeed")
	}
	if !tb.allow("client") {
		t.Fatal("allow 2/2 must succeed")
	}
	if tb.allow("client") {
		t.Fatal("allow 3/2 must fail (bucket empty)")
	}
}
