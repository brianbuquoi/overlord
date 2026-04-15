package api

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/brianbuquoi/overlord/internal/auth"
)

const requestIDHeader = "X-Request-ID"

// requestID reads X-Request-ID from the request or generates a new UUID.
// The ID is set on both the request header (for downstream handlers) and
// the response header.
func requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(requestIDHeader)
		if id == "" {
			id = uuid.New().String()
		}
		r.Header.Set(requestIDHeader, id)
		w.Header().Set(requestIDHeader, id)
		next.ServeHTTP(w, r)
	})
}

// rateLimitMiddleware returns middleware that rate-limits by client IP.
// Paths in exempt are skipped — this prevents Prometheus /metrics scrapes
// from being blocked by attacker traffic (SEC2-002).
func rateLimitMiddleware(limiter *tokenBucket, exempt ...string) func(http.Handler) http.Handler {
	exemptSet := make(map[string]struct{}, len(exempt))
	for _, p := range exempt {
		exemptSet[p] = struct{}{}
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if _, ok := exemptSet[r.URL.Path]; ok {
				next.ServeHTTP(w, r)
				return
			}
			ip := clientIP(r)
			if !limiter.allow(ip) {
				w.Header().Set("Retry-After", "1")
				writeError(w, http.StatusTooManyRequests, "rate limit exceeded", "RATE_LIMIT_EXCEEDED")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// securityHeaders sets standard security response headers on every response.
// SEC4-001: Prevents MIME-type sniffing, clickjacking, and referrer leakage.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

// clientIP extracts the client IP from the request, stripping the port.
//
// Security: X-Forwarded-For is NOT trusted because it can be trivially spoofed
// by any client. An attacker sending X-Forwarded-For: <random-ip> with each
// request would bypass per-IP rate limiting entirely. We always use RemoteAddr
// which reflects the actual TCP connection source.
//
// If Overlord is deployed behind a trusted reverse proxy (nginx, ALB, etc.),
// the proxy should set a trusted header (e.g. X-Real-IP) and this function
// should be updated to read that header only when the proxy is configured.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// contextKey is an unexported type for context keys in this package.
type contextKey int

const authKeyContextKey contextKey = iota

// AuthKeyFromContext returns the authenticated APIKey from the request context,
// or nil if the request was not authenticated.
func AuthKeyFromContext(ctx context.Context) *auth.APIKey {
	key, _ := ctx.Value(authKeyContextKey).(*auth.APIKey)
	return key
}

// authMiddleware returns middleware that enforces Bearer token authentication
// with scope checking and brute force protection. The required scope is
// determined per-request by the scopeFn callback.
//
// For WebSocket upgrade requests, the Authorization header is preferred. If
// absent, the middleware consumes a short-lived single-use WebSocket session
// token from ?token= (minted via POST /v1/ws-token). Full API keys are
// NEVER accepted via the query string — a leaked short-lived token expires
// in seconds and cannot be replayed, while a long-lived API key can.
func authMiddleware(keys []auth.APIKey, tracker *auth.BruteForceTracker, logger *slog.Logger, scopeFn func(r *http.Request) auth.Scope, wsTokens *wsTokenStore) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip := clientIP(r)

			// Brute force protection: reject if IP is blocked.
			if tracker.IsBlocked(ip) {
				windowEnd := tracker.WindowEnd(ip)
				retryAfter := int(math.Ceil(time.Until(windowEnd).Seconds()))
				if retryAfter < 1 {
					retryAfter = 1
				}
				w.Header().Set("Retry-After", fmt.Sprintf("%d", retryAfter))
				w.Header().Set("X-RateLimit-Reset", fmt.Sprintf("%d", windowEnd.Unix()))
				writeError(w, http.StatusTooManyRequests, "too many authentication failures", "AUTH_RATE_LIMITED")
				logger.Warn("auth rate limited",
					"ip", ip,
					"endpoint", r.URL.Path,
					"reason", "brute_force_blocked",
				)
				return
			}

			// For WebSocket upgrades without an Authorization header, try a
			// short-lived session token from the query string. On success,
			// authenticate as a synthetic read-scoped session; no brute-force
			// credit/debit because the token itself is single-use and random.
			if isWebSocketUpgrade(r) && r.Header.Get("Authorization") == "" && wsTokens != nil {
				qToken := r.URL.Query().Get("token")
				if qToken != "" && wsTokens.consume(qToken) {
					ctx := context.WithValue(r.Context(), authKeyContextKey, &auth.APIKey{
						Name:   "ws-session",
						Scopes: auth.ScopeSet{auth.ScopeRead: true},
					})
					next.ServeHTTP(w, r.WithContext(ctx))
					return
				}
				// Token missing or invalid: fall through to standard auth
				// handling below, which will reject with a uniform 401.
			}

			// Extract Bearer token from Authorization header.
			authHeader := r.Header.Get("Authorization")
			if authHeader == "" {
				tracker.RecordFailure(ip)
				w.Header().Set("WWW-Authenticate", "Bearer")
				// SEC3-002: Uniform 401 body — do not reveal failure mode.
				writeError(w, http.StatusUnauthorized, "unauthorized", "UNAUTHORIZED")
				logger.Warn("auth failed",
					"ip", ip,
					"endpoint", r.URL.Path,
					"reason", "missing",
				)
				return
			}

			if !strings.HasPrefix(authHeader, "Bearer ") {
				tracker.RecordFailure(ip)
				w.Header().Set("WWW-Authenticate", "Bearer")
				// SEC3-002: Uniform 401 body — do not reveal failure mode.
				writeError(w, http.StatusUnauthorized, "unauthorized", "UNAUTHORIZED")
				logger.Warn("auth failed",
					"ip", ip,
					"endpoint", r.URL.Path,
					"reason", "invalid_scheme",
				)
				return
			}

			token := strings.TrimPrefix(authHeader, "Bearer ")

			key, err := auth.Authenticate(keys, token)
			if err != nil {
				tracker.RecordFailure(ip)
				// SEC3-002: Uniform 401 body — do not reveal failure mode.
				writeError(w, http.StatusUnauthorized, "unauthorized", "UNAUTHORIZED")
				logger.Warn("auth failed",
					"ip", ip,
					"endpoint", r.URL.Path,
					"reason", "invalid_key",
				)
				return
			}

			// Check scope.
			required := scopeFn(r)
			if !key.Scopes.HasScope(required) {
				// SEC3-003: Uniform 403 body — do not reveal scope details.
				writeError(w, http.StatusForbidden, "forbidden", "FORBIDDEN")
				logger.Warn("auth failed",
					"ip", ip,
					"endpoint", r.URL.Path,
					"key_name", key.Name,
					"reason", "insufficient_scope",
				)
				return
			}

			// SEC3-001: RecordSuccess is a no-op — do not call it.
			// Failures expire naturally via the sliding window.

			// Attach key to context for downstream handlers.
			ctx := context.WithValue(r.Context(), authKeyContextKey, key)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// isWebSocketUpgrade returns true if the request is a WebSocket upgrade.
func isWebSocketUpgrade(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
}

// endpointScope returns the required scope for a given request based on
// method and path.
func endpointScope(r *http.Request) auth.Scope {
	path := r.URL.Path

	// replay-all and discard-all require admin scope.
	if path == "/v1/dead-letter/replay-all" || path == "/v1/dead-letter/discard-all" {
		return auth.ScopeAdmin
	}

	// /v1/ws-token mints a read-only WebSocket session token; any
	// authenticated caller (even read-scoped) should be able to stream.
	if path == "/v1/ws-token" {
		return auth.ScopeRead
	}

	if r.Method == http.MethodPost {
		return auth.ScopeWrite
	}
	return auth.ScopeRead
}
