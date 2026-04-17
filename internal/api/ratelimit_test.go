package api

import (
	"context"
	"testing"
	"time"
)

// TestTokenBucket_CleanupStopsOnContextCancel is the SEC-014 regression.
// The prior shape started cleanupLoop with no stop mechanism, so every
// short-lived server or test that constructed a limiter leaked a
// background goroutine. Cancelling the context must release the
// goroutine deterministically.
//
// The goroutine's select is woken immediately by ctx.Done() regardless
// of the cleanup ticker interval, so we don't need to shrink tb.cleanup
// from the default 5-minute value. Waiting on tb.done is race-free:
// cleanupLoop closes it before returning, so the receive blocks until
// after every field access is complete.
func TestTokenBucket_CleanupStopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	tb := newTokenBucket(ctx, 10, 10)

	// Touch the limiter so we know the goroutine has had a chance to
	// start — this is not required for correctness (close(done) happens
	// from the goroutine itself), but it keeps the test's intent clear.
	if !tb.allow("k") {
		t.Fatal("first allow must succeed")
	}

	cancel()

	select {
	case <-tb.done:
		// The goroutine exited cleanly on ctx.Done(). SEC-014 verified.
	case <-time.After(1 * time.Second):
		t.Fatal("cleanup goroutine did not exit within 1s of ctx cancel")
	}
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
