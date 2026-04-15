package api

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The auth middleware must only admit the first caller that races to
// consume a ws-token. Subsequent callers using the same token must be
// rejected.
func TestWSToken_ConcurrentUpgrade(t *testing.T) {
	s := newWSTokenStore()
	tok, _, err := s.issue()
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	const N = 20
	var admitted, rejected atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if s.consume(tok) {
				admitted.Add(1)
			} else {
				rejected.Add(1)
			}
		}()
	}
	wg.Wait()

	if admitted.Load() != 1 {
		t.Fatalf("admitted: got %d, want 1", admitted.Load())
	}
	if rejected.Load() != N-1 {
		t.Fatalf("rejected: got %d, want %d", rejected.Load(), N-1)
	}
	// Token should now be fully consumed.
	if s.consume(tok) {
		t.Fatal("token should be fully consumed after the winner")
	}
}

func TestWSTokenStore_IssueAndConsumeOnce(t *testing.T) {
	s := newWSTokenStore()
	tok, ttl, err := s.issue()
	if err != nil {
		t.Fatal(err)
	}
	if tok == "" {
		t.Fatal("empty token")
	}
	if ttl <= 0 {
		t.Fatalf("ttl: got %d, want > 0", ttl)
	}
	if !s.consume(tok) {
		t.Fatal("first consume should succeed")
	}
	if s.consume(tok) {
		t.Fatal("second consume must fail — token is single-use")
	}
}

func TestWSTokenStore_RejectsUnknownToken(t *testing.T) {
	s := newWSTokenStore()
	if s.consume("not-a-real-token") {
		t.Fatal("unknown token must not be accepted")
	}
	if s.consume("") {
		t.Fatal("empty token must not be accepted")
	}
}

func TestWSTokenStore_ExpiredTokenRejected(t *testing.T) {
	s := newWSTokenStore()
	now := time.Now()
	s.now = func() time.Time { return now }
	tok, _, err := s.issue()
	if err != nil {
		t.Fatal(err)
	}
	// Jump well past the TTL.
	now = now.Add(s.ttl + time.Second)
	if s.consume(tok) {
		t.Fatal("expired token must be rejected")
	}
}

func TestWSTokenStore_PrunesExpiredOnIssue(t *testing.T) {
	s := newWSTokenStore()
	now := time.Now()
	s.now = func() time.Time { return now }
	for i := 0; i < 10; i++ {
		if _, _, err := s.issue(); err != nil {
			t.Fatal(err)
		}
	}
	now = now.Add(s.ttl + time.Second)
	// Next issue should prune the 10 expired tokens.
	if _, _, err := s.issue(); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	remaining := len(s.tokens)
	s.mu.Unlock()
	if remaining != 1 {
		t.Fatalf("expected 1 live token after prune, got %d", remaining)
	}
}
