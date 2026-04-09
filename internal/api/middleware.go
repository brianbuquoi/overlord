package api

import (
	"net"
	"net/http"

	"github.com/google/uuid"
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

// clientIP extracts the client IP from the request, stripping the port.
//
// Security: X-Forwarded-For is NOT trusted because it can be trivially spoofed
// by any client. An attacker sending X-Forwarded-For: <random-ip> with each
// request would bypass per-IP rate limiting entirely. We always use RemoteAddr
// which reflects the actual TCP connection source.
//
// If Orcastrator is deployed behind a trusted reverse proxy (nginx, ALB, etc.),
// the proxy should set a trusted header (e.g. X-Real-IP) and this function
// should be updated to read that header only when the proxy is configured.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
