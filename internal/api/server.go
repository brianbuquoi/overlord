// Package api provides the HTTP REST and WebSocket server for pipeline
// observation, task submission, and real-time event streaming.
package api

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/orcastrator/orcastrator/internal/auth"
	"github.com/orcastrator/orcastrator/internal/broker"
	"github.com/orcastrator/orcastrator/internal/metrics"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Server is the HTTP/WebSocket server for the Orcastrator observation API.
type Server struct {
	broker      *broker.Broker
	hub         *wsHub
	limiter     *tokenBucket
	logger      *slog.Logger
	srv         *http.Server
	metrics     *metrics.Metrics
	metricsPath string
	authKeys    []auth.APIKey
	bfTracker   *auth.BruteForceTracker
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
	s := &Server{
		broker:      b,
		limiter:     newTokenBucket(100, 100), // 100 req/s per IP, burst of 100
		logger:      logger,
		metrics:     m,
		metricsPath: metricsPath,
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
	mux.HandleFunc("/v1/tasks/", s.handleGetTask)
	mux.HandleFunc("/v1/tasks", s.handleListTasks)
	mux.HandleFunc("/v1/health", s.handleHealth)
	mux.HandleFunc("/v1/stream", s.handleStream)

	if s.metrics != nil {
		mux.Handle(s.metricsPath, promhttp.HandlerFor(s.metrics.Registry, promhttp.HandlerOpts{}))
	}

	// Apply middleware: rate limit → request ID → [auth] → mux.
	// /metrics is exempt from rate limiting so Prometheus scrapes are never
	// blocked by attacker traffic. SEC2-002.
	// /metrics is also exempt from auth per SEC2-NEW-002 (network-level
	// access control only).
	var handler http.Handler = mux
	if s.authKeys != nil {
		handler = authMiddleware(s.authKeys, s.bfTracker, s.logger, endpointScope)(handler)
		// Wrap so /metrics bypasses auth.
		authed := handler
		handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == s.metricsPath {
				mux.ServeHTTP(w, r)
				return
			}
			authed.ServeHTTP(w, r)
		})
	}
	handler = requestID(handler)
	handler = rateLimitMiddleware(s.limiter, s.metricsPath)(handler)
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
