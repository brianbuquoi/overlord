// Package api provides the HTTP REST and WebSocket server for pipeline
// observation, task submission, and real-time event streaming.
package api

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/brianbuquoi/overlord/internal/auth"
	"github.com/brianbuquoi/overlord/internal/broker"
	"github.com/brianbuquoi/overlord/internal/dashboard"
	"github.com/brianbuquoi/overlord/internal/deadletter"
	"github.com/brianbuquoi/overlord/internal/metrics"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// replayAllLimiter rate-limits replay-all calls to 1 per minute per pipeline.
// Entries are pruned on every call to prevent unbounded memory growth.
type replayAllRateLimiter struct {
	mu       sync.Mutex
	lastCall map[string]time.Time
}

// maxReplayAllEntries caps the map size as a safety net against pathological
// key cardinality (e.g. attacker sending random pipeline_id values). The
// pruning logic removes expired entries first; this cap only fires if there
// are more than this many *active* (non-expired) entries simultaneously.
const maxReplayAllEntries = 10000

func newReplayAllRateLimiter() *replayAllRateLimiter {
	return &replayAllRateLimiter{lastCall: make(map[string]time.Time)}
}

func (r *replayAllRateLimiter) allow(pipelineID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now()

	// Prune expired entries on every call to bound memory usage.
	for k, t := range r.lastCall {
		if now.Sub(t) >= time.Minute {
			delete(r.lastCall, k)
		}
	}

	if last, ok := r.lastCall[pipelineID]; ok {
		if now.Sub(last) < time.Minute {
			return false
		}
	}

	// Hard cap: reject if too many concurrent active entries.
	if len(r.lastCall) >= maxReplayAllEntries {
		return false
	}

	r.lastCall[pipelineID] = now
	return true
}

// Server is the HTTP/WebSocket server for the Overlord observation API.
type Server struct {
	broker           *broker.Broker
	hub              *wsHub
	limiter          *tokenBucket
	replayAllLimiter *replayAllRateLimiter
	logger           *slog.Logger
	srv              *http.Server
	metrics          *metrics.Metrics
	metricsPath      string
	authKeys         []auth.APIKey
	bfTracker        *auth.BruteForceTracker
	dashHandler      http.Handler
	dashPath         string
	dashEnabled      bool
	wsTokens         *wsTokenStore
	deadletter       *deadletter.Service
}

// NewServer creates a new API server. The metricsPath defaults to "/metrics"
// if empty. Pass nil for m to disable the /metrics endpoint. Pass nil for
// authKeys to disable authentication (backward compatible).
func NewServer(b *broker.Broker, logger *slog.Logger, m *metrics.Metrics, metricsPath string, authKeys ...[]auth.APIKey) *Server {
	return NewServerWithContext(context.Background(), b, logger, m, metricsPath, authKeys...)
}

// NewServerWithContext creates a new API server with an explicit context for
// background goroutine lifecycle (e.g. brute force tracker cleanup).
func NewServerWithContext(ctx context.Context, b *broker.Broker, logger *slog.Logger, m *metrics.Metrics, metricsPath string, authKeys ...[]auth.APIKey) *Server {
	if metricsPath == "" {
		metricsPath = "/metrics"
	}
	cfg := b.Config()
	dashEnabled := cfg.Dashboard.DashboardEnabled()
	dashPath := cfg.Dashboard.DashboardPath()

	s := &Server{
		broker:           b,
		limiter:          newTokenBucket(ctx, 100, 100), // 100 req/s per IP, burst of 100
		replayAllLimiter: newReplayAllRateLimiter(),
		logger:           logger,
		metrics:          m,
		metricsPath:      metricsPath,
		dashPath:         dashPath,
		dashEnabled:      dashEnabled,
		wsTokens:         newWSTokenStore(),
		deadletter:       deadletter.New(b.Store(), b, logger),
	}
	if dashEnabled {
		dh, err := dashboard.New()
		if err != nil {
			logger.Error("failed to load dashboard", "error", err)
		} else {
			s.dashHandler = dh
		}
	}
	if len(authKeys) > 0 && authKeys[0] != nil {
		s.authKeys = authKeys[0]
		s.bfTracker = auth.NewBruteForceTracker(10, 60*time.Second,
			auth.WithCleanup(ctx),
			auth.WithLogger(logger),
		)
	}
	s.hub = newWSHub(b.EventBus(), logger)
	s.srv = &http.Server{
		Handler: s.routes(),
	}
	return s
}

// routes builds the HTTP handler chain with middleware.
func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/v1/pipelines/", s.routePipelines)
	mux.HandleFunc("/v1/tasks/", s.routeTasks)
	mux.HandleFunc("/v1/tasks", s.handleListTasks)
	mux.HandleFunc("/v1/health", s.handleHealth)
	mux.HandleFunc("/v1/stream", s.handleStream)
	mux.HandleFunc("/v1/ws-token", s.handleIssueWSToken)
	mux.HandleFunc("/v1/dead-letter/", s.routeDeadLetter)
	mux.HandleFunc("/v1/dead-letter", s.routeDeadLetterRoot)

	if s.metrics != nil {
		mux.Handle(s.metricsPath, promhttp.HandlerFor(s.metrics.Registry, promhttp.HandlerOpts{}))
	}

	// Register dashboard routes (excluded from auth and rate limiting).
	// Note: we do NOT register "/" as a catch-all because it would intercept
	// the mux's automatic redirect from /v1/pipelines → /v1/pipelines/.
	// Instead, we handle "/" explicitly in the outer handler wrapper.
	if s.dashHandler != nil {
		mux.Handle(s.dashPath, s.dashHandler)
	}

	// isDashboardPath checks if a request path matches the dashboard routes.
	isDashboardPath := func(path string) bool {
		if s.dashHandler == nil {
			return false
		}
		return path == "/" || path == s.dashPath
	}

	// Wrap mux to serve dashboard at "/" without registering "/" on the mux
	// (which would intercept the mux's automatic trailing-slash redirects).
	var inner http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" && s.dashHandler != nil {
			s.dashHandler.ServeHTTP(w, r)
			return
		}
		mux.ServeHTTP(w, r)
	})

	// Apply middleware: rate limit → request ID → [auth] → inner.
	// /metrics is exempt from rate limiting so Prometheus scrapes are never
	// blocked by attacker traffic. SEC2-002.
	// /metrics is also exempt from auth per SEC2-NEW-002 (network-level
	// access control only).
	// Dashboard routes are exempt from both auth and rate limiting — the
	// dashboard serves static content and handles its own auth flow.
	var handler http.Handler = inner
	if s.authKeys != nil {
		handler = authMiddleware(s.authKeys, s.bfTracker, s.logger, endpointScope, s.wsTokens)(handler)
		// Wrap so /metrics and dashboard bypass auth.
		authed := handler
		handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == s.metricsPath || isDashboardPath(r.URL.Path) {
				inner.ServeHTTP(w, r)
				return
			}
			authed.ServeHTTP(w, r)
		})
	}
	handler = requestID(handler)
	handler = securityHeaders(handler)
	// Dashboard and /metrics are exempt from rate limiting.
	handler = rateLimitMiddleware(s.limiter, s.metricsPath, s.dashPath, "/")(handler)
	return handler
}

// routePipelines dispatches /v1/pipelines/* to the right handler based on
// method and path shape.
func (s *Server) routePipelines(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	// POST /v1/pipelines/{pipelineID}/tasks
	if r.Method == http.MethodPost && strings.HasSuffix(path, "/tasks") {
		s.handleSubmitTask(w, r)
		return
	}

	// GET /v1/pipelines
	if r.Method == http.MethodGet && (path == "/v1/pipelines" || path == "/v1/pipelines/") {
		s.handleListPipelines(w, r)
		return
	}

	writeError(w, http.StatusMethodNotAllowed, "method not allowed", "METHOD_NOT_ALLOWED")
}

// routeTasks dispatches /v1/tasks/* based on method and path shape.
func (s *Server) routeTasks(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	// POST /v1/tasks/{taskID}/recover
	if r.Method == http.MethodPost && strings.HasSuffix(path, "/recover") {
		s.handleRecoverTask(w, r)
		return
	}

	// GET /v1/tasks/{taskID}
	if r.Method == http.MethodGet {
		s.handleGetTask(w, r)
		return
	}

	writeError(w, http.StatusMethodNotAllowed, "method not allowed", "METHOD_NOT_ALLOWED")
}

// routeDeadLetterRoot handles GET /v1/dead-letter (no trailing path).
func (s *Server) routeDeadLetterRoot(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		s.handleListDeadLetter(w, r)
		return
	}
	writeError(w, http.StatusMethodNotAllowed, "method not allowed", "METHOD_NOT_ALLOWED")
}

// routeDeadLetter dispatches /v1/dead-letter/* to the appropriate handler.
func (s *Server) routeDeadLetter(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	// POST /v1/dead-letter/replay-all
	if r.Method == http.MethodPost && path == "/v1/dead-letter/replay-all" {
		s.handleReplayAllDeadLetter(w, r)
		return
	}

	// POST /v1/dead-letter/discard-all
	if r.Method == http.MethodPost && path == "/v1/dead-letter/discard-all" {
		s.handleDiscardAllDeadLetter(w, r)
		return
	}

	// POST /v1/dead-letter/{taskID}/replay
	if r.Method == http.MethodPost && strings.HasSuffix(path, "/replay") {
		s.handleReplayDeadLetter(w, r)
		return
	}

	// POST /v1/dead-letter/{taskID}/discard
	if r.Method == http.MethodPost && strings.HasSuffix(path, "/discard") {
		s.handleDiscardDeadLetter(w, r)
		return
	}

	// GET /v1/dead-letter/ (with trailing slash)
	if r.Method == http.MethodGet {
		s.handleListDeadLetter(w, r)
		return
	}

	writeError(w, http.StatusMethodNotAllowed, "method not allowed", "METHOD_NOT_ALLOWED")
}

// ListenAndServe starts the server on the given address.
func (s *Server) ListenAndServe(addr string) error {
	s.srv.Addr = addr
	return s.srv.ListenAndServe()
}

// Serve accepts connections on the given listener.
func (s *Server) Serve(ln net.Listener) error {
	return s.srv.Serve(ln)
}

// Shutdown gracefully shuts down the server: drains in-flight HTTP requests
// and sends close frames to WebSocket clients.
func (s *Server) Shutdown(ctx context.Context) error {
	// Shut down the WS hub first — sends close frames to all clients.
	s.hub.shutdown()

	// Then gracefully drain HTTP requests.
	return s.srv.Shutdown(ctx)
}

// Handler returns the server's HTTP handler for testing.
func (s *Server) Handler() http.Handler {
	return s.srv.Handler
}

// ShutdownTimeout is the default timeout for graceful shutdown.
const ShutdownTimeout = 10 * time.Second
