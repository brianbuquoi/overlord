package api

import (
	"testing"
	"time"
)

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
