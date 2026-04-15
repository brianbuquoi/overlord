package api

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"
)

// wsTokenTTL is the lifetime of a short-lived WebSocket session token.
// 30s is long enough for the browser to receive the response and open the
// socket, but short enough that a token leaking into a proxy log or trace
// is expired before an attacker can replay it.
const wsTokenTTL = 30 * time.Second

// wsTokenStore holds issued short-lived WebSocket session tokens. Tokens are
// consumed on use — a successful WebSocket upgrade deletes the token from
// the store so it cannot be reused. Expired tokens are pruned lazily on
// every issue/consume call plus a periodic background sweep.
//
// Tokens are ephemeral and never persisted; a restart invalidates all
// outstanding tokens, which is acceptable behaviour — browsers reconnect
// via the /v1/ws-token handshake.
type wsTokenStore struct {
	mu     sync.Mutex
	tokens map[string]time.Time // token → expiresAt
	ttl    time.Duration
	now    func() time.Time
}

func newWSTokenStore() *wsTokenStore {
	return &wsTokenStore{
		tokens: make(map[string]time.Time),
		ttl:    wsTokenTTL,
		now:    time.Now,
	}
}

// issue generates a new token with the store's TTL and returns it alongside
// the TTL in seconds.
func (s *wsTokenStore) issue() (token string, ttlSeconds int, err error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", 0, err
	}
	token = hex.EncodeToString(raw)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked()
	s.tokens[token] = s.now().Add(s.ttl)
	return token, int(s.ttl / time.Second), nil
}

// consume atomically validates and removes a token. Returns true only on the
// one call that successfully consumes a non-expired token.
func (s *wsTokenStore) consume(token string) bool {
	if token == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	expiresAt, ok := s.tokens[token]
	if !ok {
		return false
	}
	delete(s.tokens, token)
	return !s.now().After(expiresAt)
}

func (s *wsTokenStore) pruneLocked() {
	now := s.now()
	for tok, exp := range s.tokens {
		if now.After(exp) {
			delete(s.tokens, tok)
		}
	}
}
