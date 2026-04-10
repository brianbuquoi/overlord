package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/orcastrator/orcastrator/internal/auth"
	"github.com/orcastrator/orcastrator/internal/broker"
	"github.com/orcastrator/orcastrator/internal/config"
	"github.com/orcastrator/orcastrator/internal/contract"
	"github.com/orcastrator/orcastrator/internal/dashboard"
	"github.com/orcastrator/orcastrator/internal/metrics"
	"github.com/orcastrator/orcastrator/internal/store/memory"
	"golang.org/x/crypto/bcrypt"
)

// --- Dashboard route tests ---

func TestDashboard_ServedAtDashboardPath(t *testing.T) {
	srv, _ := newTestServer(t)
	defer srv.Shutdown(context.Background())

	req := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("GET /dashboard: expected 200, got %d", w.Code)
	}
	ct := w.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("GET /dashboard: expected text/html, got %q", ct)
	}
	body := w.Body.String()
	if !strings.Contains(body, "<html") {
		t.Fatal("GET /dashboard: body missing <html")
	}
}

func TestDashboard_ServedAtRoot(t *testing.T) {
	srv, _ := newTestServer(t)
	defer srv.Shutdown(context.Background())

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("GET /: expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "<html") {
		t.Fatal("GET /: body missing <html — should serve dashboard")
	}
}

func TestDashboard_ETag(t *testing.T) {
	srv, _ := newTestServer(t)
	defer srv.Shutdown(context.Background())

	// First request
	req := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	etag := w.Header().Get("ETag")
	if etag == "" {
		t.Fatal("expected ETag header on /dashboard")
	}

	// Second with If-None-Match
	req2 := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	req2.Header.Set("If-None-Match", etag)
	w2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w2, req2)

	if w2.Code != http.StatusNotModified {
		t.Fatalf("expected 304 with matching ETag, got %d", w2.Code)
	}
}

func TestDashboard_DoesNotAffectAPIRoutes(t *testing.T) {
	srv, _ := newTestServer(t)
	defer srv.Shutdown(context.Background())

	// /v1/pipelines still returns JSON
	req := httptest.NewRequest(http.MethodGet, "/v1/pipelines/", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("GET /v1/pipelines: expected 200, got %d", w.Code)
	}
	ct := w.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("GET /v1/pipelines: expected application/json, got %q", ct)
	}

	// /v1/tasks still returns JSON
	req2 := httptest.NewRequest(http.MethodGet, "/v1/tasks", nil)
	w2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w2, req2)

	if w2.Code != http.StatusOK {
		t.Fatalf("GET /v1/tasks: expected 200, got %d", w2.Code)
	}
	var tasks listTasksResponse
	if err := json.Unmarshal(w2.Body.Bytes(), &tasks); err != nil {
		t.Fatalf("GET /v1/tasks: not valid JSON: %v", err)
	}
}

func TestDashboard_AuthExclusion(t *testing.T) {
	cfg := testConfig()
	st := memory.New()
	agents := map[string]broker.Agent{
		"agent-1": &stubAgent{id: "agent-1", provider: "anthropic", healthy: true},
		"agent-2": &stubAgent{id: "agent-2", provider: "openai", healthy: true},
	}
	reg, _ := contract.NewRegistry(nil, "")
	logger := slog.Default()
	b := broker.New(cfg, st, agents, reg, logger, nil, nil)

	hashed, _ := bcrypt.GenerateFromPassword([]byte("test-key"), bcrypt.MinCost)
	keys := []auth.APIKey{
		{Name: "test", HashedKey: hashed, Scopes: auth.ScopeSet{auth.ScopeAdmin: true}},
	}

	srv := NewServerWithContext(context.Background(), b, logger, nil, "", keys)
	defer srv.Shutdown(context.Background())

	// Dashboard served without auth
	req := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("GET /dashboard without auth: expected 200, got %d", w.Code)
	}

	// API requires auth
	req2 := httptest.NewRequest(http.MethodGet, "/v1/pipelines", nil)
	w2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w2, req2)

	if w2.Code != http.StatusUnauthorized {
		t.Fatalf("GET /v1/pipelines without auth: expected 401, got %d", w2.Code)
	}

	// Root also served without auth
	req3 := httptest.NewRequest(http.MethodGet, "/", nil)
	w3 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w3, req3)

	if w3.Code != http.StatusOK {
		t.Fatalf("GET / without auth: expected 200, got %d", w3.Code)
	}
}

func TestDashboard_Disabled(t *testing.T) {
	cfg := testConfig()
	enabled := false
	cfg.Dashboard = config.DashboardConfig{Enabled: &enabled}

	st := memory.New()
	agents := map[string]broker.Agent{
		"agent-1": &stubAgent{id: "agent-1", provider: "anthropic", healthy: true},
		"agent-2": &stubAgent{id: "agent-2", provider: "openai", healthy: true},
	}
	reg, _ := contract.NewRegistry(nil, "")
	logger := slog.Default()
	b := broker.New(cfg, st, agents, reg, logger, nil, nil)
	srv := NewServer(b, logger, nil, "")
	defer srv.Shutdown(context.Background())

	// /dashboard returns 404
	req := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("GET /dashboard (disabled): expected 404, got %d", w.Code)
	}

	// / returns 404
	req2 := httptest.NewRequest(http.MethodGet, "/", nil)
	w2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w2, req2)

	if w2.Code != http.StatusNotFound {
		t.Fatalf("GET / (disabled): expected 404, got %d", w2.Code)
	}

	// API routes still work
	req3 := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	w3 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w3, req3)

	if w3.Code != http.StatusOK {
		t.Fatalf("GET /v1/health (dashboard disabled): expected 200, got %d", w3.Code)
	}
}

func TestDashboard_RateLimitExclusion(t *testing.T) {
	cfg := testConfig()
	st := memory.New()
	agents := map[string]broker.Agent{
		"agent-1": &stubAgent{id: "agent-1", provider: "anthropic", healthy: true},
		"agent-2": &stubAgent{id: "agent-2", provider: "openai", healthy: true},
	}
	reg, _ := contract.NewRegistry(nil, "")
	logger := slog.Default()
	b := broker.New(cfg, st, agents, reg, logger, nil, nil)

	// Create server with a very low rate limit to test exclusion.
	srv := NewServerWithContext(context.Background(), b, logger, nil, "")
	// Override the limiter to a tiny limit for testing.
	srv.limiter = newTokenBucket(1, 1) // 1 req/s, burst 1
	// Rebuild routes with new limiter.
	srv.srv.Handler = srv.routes()
	defer srv.Shutdown(context.Background())

	// Exhaust the rate limit with dashboard requests — should all succeed.
	for i := 0; i < 10; i++ {
		req := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
		req.RemoteAddr = "10.0.0.1:12345"
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		if w.Code == http.StatusTooManyRequests {
			t.Fatalf("GET /dashboard request %d: got 429, dashboard should be exempt", i+1)
		}
	}

	// The next API request should be rate limited since we already used the burst.
	// First consume the token with an API call.
	req := httptest.NewRequest(http.MethodGet, "/v1/tasks", nil)
	req.RemoteAddr = "10.0.0.1:12345"
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	// This might succeed (burst). Do another immediately.
	req2 := httptest.NewRequest(http.MethodGet, "/v1/tasks", nil)
	req2.RemoteAddr = "10.0.0.1:12345"
	w2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w2, req2)

	// At least one should be rate limited given burst=1 and rate=1/s
	if w.Code != http.StatusTooManyRequests && w2.Code != http.StatusTooManyRequests {
		t.Fatal("expected at least one API request to be rate limited")
	}
}

func TestDashboard_RateLimitExclusion_101Requests(t *testing.T) {
	srv, _ := newTestServer(t)
	defer srv.Shutdown(context.Background())

	// 101 dashboard requests — none should get 429
	for i := 0; i < 101; i++ {
		req := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
		req.RemoteAddr = "10.0.0.99:12345"
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		if w.Code == http.StatusTooManyRequests {
			t.Fatalf("GET /dashboard request %d: got 429, dashboard should be exempt from rate limiting", i+1)
		}
	}

	// Now exhaust the rate limit on API — the default is 100 burst.
	// Send 101 requests to /v1/tasks; the 101st should be 429.
	var got429 bool
	for i := 0; i < 101; i++ {
		req := httptest.NewRequest(http.MethodGet, "/v1/tasks", nil)
		req.RemoteAddr = "10.0.0.99:12345"
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		if w.Code == http.StatusTooManyRequests {
			got429 = true
			break
		}
	}
	if !got429 {
		t.Fatal("expected /v1/tasks to be rate limited after 101 requests")
	}
}

func TestDashboard_MetricsStillWork(t *testing.T) {
	srv, _ := newTestServer(t)
	defer srv.Shutdown(context.Background())

	// Metrics may return 404 if nil metrics (no prometheus registry).
	// But the route should not return HTML.
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	// With nil metrics, /metrics won't match any handler and falls through to mux default.
	// Just verify it doesn't return the dashboard HTML.
	if strings.Contains(w.Body.String(), "<html") {
		t.Fatal("GET /metrics returned dashboard HTML")
	}
}

// newTestServerWithMetrics creates a server with metrics enabled for testing.
func TestDashboard_MetricsRouteReturnsPrometheus(t *testing.T) {
	cfg := testConfig()
	st := memory.New()
	agents := map[string]broker.Agent{
		"agent-1": &stubAgent{id: "agent-1", provider: "anthropic", healthy: true},
		"agent-2": &stubAgent{id: "agent-2", provider: "openai", healthy: true},
	}
	reg, _ := contract.NewRegistry(nil, "")
	logger := slog.Default()
	m := metrics.New()
	b := broker.New(cfg, st, agents, reg, logger, m, nil)
	srv := NewServer(b, logger, m, "")
	defer srv.Shutdown(context.Background())

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("GET /metrics: expected 200, got %d", w.Code)
	}
	if strings.Contains(w.Body.String(), "<html") {
		t.Fatal("GET /metrics should return Prometheus text, not HTML")
	}
}

// --- Issue 1: WebSocket query-param auth for dashboard ---

func TestDashboard_WebSocketAuthViaQueryParam(t *testing.T) {
	// With auth enabled, a WebSocket upgrade request with ?token= and the
	// Upgrade: websocket header should authenticate successfully.
	cfg := testConfig()
	st := memory.New()
	agents := map[string]broker.Agent{
		"agent-1": &stubAgent{id: "agent-1", provider: "anthropic", healthy: true},
		"agent-2": &stubAgent{id: "agent-2", provider: "openai", healthy: true},
	}
	reg, _ := contract.NewRegistry(nil, "")
	logger := slog.Default()
	b := broker.New(cfg, st, agents, reg, logger, nil, nil)

	hashed, _ := bcrypt.GenerateFromPassword([]byte("ws-test-key"), bcrypt.MinCost)
	keys := []auth.APIKey{
		{Name: "ws-test", HashedKey: hashed, Scopes: auth.ScopeSet{auth.ScopeRead: true}},
	}

	srv := NewServerWithContext(context.Background(), b, logger, nil, "", keys)
	defer srv.Shutdown(context.Background())

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// Connect via WebSocket with token in query param.
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/v1/stream?token=ws-test-key"
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("WebSocket dial with query token failed: %v (status: %d)", err, resp.StatusCode)
	}
	conn.Close()
}

func TestDashboard_WebSocketAuthViaQueryParam_InvalidKey(t *testing.T) {
	cfg := testConfig()
	st := memory.New()
	agents := map[string]broker.Agent{
		"agent-1": &stubAgent{id: "agent-1", provider: "anthropic", healthy: true},
		"agent-2": &stubAgent{id: "agent-2", provider: "openai", healthy: true},
	}
	reg, _ := contract.NewRegistry(nil, "")
	logger := slog.Default()
	b := broker.New(cfg, st, agents, reg, logger, nil, nil)

	hashed, _ := bcrypt.GenerateFromPassword([]byte("real-key"), bcrypt.MinCost)
	keys := []auth.APIKey{
		{Name: "test", HashedKey: hashed, Scopes: auth.ScopeSet{auth.ScopeRead: true}},
	}

	srv := NewServerWithContext(context.Background(), b, logger, nil, "", keys)
	defer srv.Shutdown(context.Background())

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// Invalid token should be rejected.
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/v1/stream?token=wrong-key"
	_, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err == nil {
		t.Fatal("expected WebSocket dial to fail with invalid query token")
	}
	if resp != nil && resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
}

func TestDashboard_WebSocketQueryParam_NotAcceptedOnRegularHTTP(t *testing.T) {
	// Query param auth must NOT work for regular (non-WebSocket) HTTP requests.
	cfg := testConfig()
	st := memory.New()
	agents := map[string]broker.Agent{
		"agent-1": &stubAgent{id: "agent-1", provider: "anthropic", healthy: true},
		"agent-2": &stubAgent{id: "agent-2", provider: "openai", healthy: true},
	}
	reg, _ := contract.NewRegistry(nil, "")
	logger := slog.Default()
	b := broker.New(cfg, st, agents, reg, logger, nil, nil)

	hashed, _ := bcrypt.GenerateFromPassword([]byte("http-key"), bcrypt.MinCost)
	keys := []auth.APIKey{
		{Name: "test", HashedKey: hashed, Scopes: auth.ScopeSet{auth.ScopeRead: true}},
	}

	srv := NewServerWithContext(context.Background(), b, logger, nil, "", keys)
	defer srv.Shutdown(context.Background())

	// Regular GET with ?token= but no Upgrade header — should be 401.
	req := httptest.NewRequest(http.MethodGet, "/v1/tasks?token=http-key", nil)
	req.RemoteAddr = "10.0.0.50:12345"
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("query param on regular HTTP: expected 401, got %d", w.Code)
	}
}

func TestDashboard_WebSocketAuthViaHeader_StillWorks(t *testing.T) {
	// When auth is enabled, WebSocket with Authorization header should still work.
	cfg := testConfig()
	st := memory.New()
	agents := map[string]broker.Agent{
		"agent-1": &stubAgent{id: "agent-1", provider: "anthropic", healthy: true},
		"agent-2": &stubAgent{id: "agent-2", provider: "openai", healthy: true},
	}
	reg, _ := contract.NewRegistry(nil, "")
	logger := slog.Default()
	b := broker.New(cfg, st, agents, reg, logger, nil, nil)

	hashed, _ := bcrypt.GenerateFromPassword([]byte("header-key"), bcrypt.MinCost)
	keys := []auth.APIKey{
		{Name: "test", HashedKey: hashed, Scopes: auth.ScopeSet{auth.ScopeRead: true}},
	}

	srv := NewServerWithContext(context.Background(), b, logger, nil, "", keys)
	defer srv.Shutdown(context.Background())

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// Connect with Authorization header (preferred path).
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/v1/stream"
	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer header-key")
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, hdr)
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		t.Fatalf("WebSocket dial with header auth failed: %v (status: %d)", err, status)
	}
	conn.Close()
}

// --- Issue 2: Custom dashboard path ---

func TestDashboard_CustomPath(t *testing.T) {
	cfg := testConfig()
	cfg.Dashboard = config.DashboardConfig{Path: "/admin/console"}

	st := memory.New()
	agents := map[string]broker.Agent{
		"agent-1": &stubAgent{id: "agent-1", provider: "anthropic", healthy: true},
		"agent-2": &stubAgent{id: "agent-2", provider: "openai", healthy: true},
	}
	reg, _ := contract.NewRegistry(nil, "")
	logger := slog.Default()
	b := broker.New(cfg, st, agents, reg, logger, nil, nil)
	srv := NewServer(b, logger, nil, "")
	defer srv.Shutdown(context.Background())

	// Custom path serves dashboard
	req := httptest.NewRequest(http.MethodGet, "/admin/console", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("GET /admin/console: expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "<html") {
		t.Fatal("GET /admin/console: expected HTML dashboard")
	}

	// Default path should not serve dashboard
	req2 := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	w2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w2, req2)

	if w2.Code == http.StatusOK && strings.Contains(w2.Body.String(), "<html") {
		t.Fatal("GET /dashboard should not serve dashboard when custom path is configured")
	}

	// Root still serves dashboard
	req3 := httptest.NewRequest(http.MethodGet, "/", nil)
	w3 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w3, req3)

	if w3.Code != http.StatusOK {
		t.Fatalf("GET / with custom path: expected 200, got %d", w3.Code)
	}
}

func TestDashboard_CustomPath_TrailingSlashNormalized(t *testing.T) {
	cfg := testConfig()
	cfg.Dashboard = config.DashboardConfig{Path: "/admin/console/"}

	st := memory.New()
	agents := map[string]broker.Agent{
		"agent-1": &stubAgent{id: "agent-1", provider: "anthropic", healthy: true},
		"agent-2": &stubAgent{id: "agent-2", provider: "openai", healthy: true},
	}
	reg, _ := contract.NewRegistry(nil, "")
	logger := slog.Default()
	b := broker.New(cfg, st, agents, reg, logger, nil, nil)
	srv := NewServer(b, logger, nil, "")
	defer srv.Shutdown(context.Background())

	// Trailing slash is stripped — /admin/console should work
	req := httptest.NewRequest(http.MethodGet, "/admin/console", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("GET /admin/console (configured with trailing slash): expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "<html") {
		t.Fatal("expected dashboard HTML")
	}
}

func TestDashboard_CustomPath_AuthExclusion(t *testing.T) {
	cfg := testConfig()
	cfg.Dashboard = config.DashboardConfig{Path: "/ops/dashboard"}

	st := memory.New()
	agents := map[string]broker.Agent{
		"agent-1": &stubAgent{id: "agent-1", provider: "anthropic", healthy: true},
		"agent-2": &stubAgent{id: "agent-2", provider: "openai", healthy: true},
	}
	reg, _ := contract.NewRegistry(nil, "")
	logger := slog.Default()
	b := broker.New(cfg, st, agents, reg, logger, nil, nil)

	hashed, _ := bcrypt.GenerateFromPassword([]byte("auth-key"), bcrypt.MinCost)
	keys := []auth.APIKey{
		{Name: "test", HashedKey: hashed, Scopes: auth.ScopeSet{auth.ScopeAdmin: true}},
	}

	srv := NewServerWithContext(context.Background(), b, logger, nil, "", keys)
	defer srv.Shutdown(context.Background())

	// Custom dashboard path excluded from auth
	req := httptest.NewRequest(http.MethodGet, "/ops/dashboard", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("GET /ops/dashboard without auth: expected 200, got %d", w.Code)
	}

	// API still requires auth
	req2 := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	req2.RemoteAddr = "10.0.0.60:12345"
	w2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w2, req2)

	if w2.Code != http.StatusUnauthorized {
		t.Fatalf("GET /v1/health without auth: expected 401, got %d", w2.Code)
	}
}

func TestDashboard_CustomPath_RateLimitExclusion(t *testing.T) {
	cfg := testConfig()
	cfg.Dashboard = config.DashboardConfig{Path: "/ops/dash"}

	st := memory.New()
	agents := map[string]broker.Agent{
		"agent-1": &stubAgent{id: "agent-1", provider: "anthropic", healthy: true},
		"agent-2": &stubAgent{id: "agent-2", provider: "openai", healthy: true},
	}
	reg, _ := contract.NewRegistry(nil, "")
	logger := slog.Default()
	b := broker.New(cfg, st, agents, reg, logger, nil, nil)
	srv := NewServerWithContext(context.Background(), b, logger, nil, "")
	srv.limiter = newTokenBucket(1, 1) // very restrictive
	srv.srv.Handler = srv.routes()
	defer srv.Shutdown(context.Background())

	// Dashboard at custom path should be exempt from rate limiting
	for i := 0; i < 10; i++ {
		req := httptest.NewRequest(http.MethodGet, "/ops/dash", nil)
		req.RemoteAddr = "10.0.0.70:12345"
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		if w.Code == http.StatusTooManyRequests {
			t.Fatalf("GET /ops/dash request %d: got 429, custom dashboard path should be rate-limit exempt", i+1)
		}
	}
}

// --- Issue 3: Dashboard HTML correctness ---

func TestDashboard_HTML_NoQueryParamTokenLeak(t *testing.T) {
	// The dashboard must NOT send the API key as a query parameter on regular
	// HTTP fetches — only on WebSocket connections (where headers aren't available).
	h, err := dashboard.New()
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	body := w.Body.String()

	// apiFetch should use Authorization header, not query params
	if !strings.Contains(body, "'Authorization'") {
		t.Fatal("dashboard should use Authorization header for API calls")
	}

	// The ?token= pattern should only appear in the WebSocket URL construction
	tokenLines := 0
	for _, line := range strings.Split(body, "\n") {
		if strings.Contains(line, "?token=") || strings.Contains(line, "'token'") {
			tokenLines++
		}
	}
	if tokenLines > 1 {
		t.Fatalf("expected ?token= to appear only in WebSocket URL construction, found %d occurrences", tokenLines)
	}
}

func TestDashboard_HTML_HasIntervalCleanup(t *testing.T) {
	// The dashboard must store interval IDs and clear them to prevent accumulation.
	h, err := dashboard.New()
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	body := w.Body.String()

	if !strings.Contains(body, "clearInterval") {
		t.Fatal("dashboard must call clearInterval to prevent interval accumulation")
	}
	if !strings.Contains(body, "healthInterval") {
		t.Fatal("dashboard must track health polling interval ID")
	}
	if !strings.Contains(body, "queueInterval") {
		t.Fatal("dashboard must track queue depth polling interval ID")
	}
}

func TestDashboard_HTML_HasIdempotencyGuard(t *testing.T) {
	// showDashboard must be idempotent — guard against double invocation.
	h, err := dashboard.New()
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	body := w.Body.String()

	if !strings.Contains(body, "dashboardActive") {
		t.Fatal("dashboard must have a dashboardActive guard to prevent double initialization")
	}
}

func TestDashboard_HTML_HandlesAuthExpiry(t *testing.T) {
	// The dashboard must detect 401 responses during operation and redirect to login.
	h, err := dashboard.New()
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	body := w.Body.String()

	// apiFetch must check for 401 and trigger re-authentication
	if !strings.Contains(body, "resp.status === 401") || !strings.Contains(body, "showLogin") {
		t.Fatal("apiFetch must detect 401 responses and redirect to login")
	}
}

// --- Config defaults ---

func TestDashboardConfig_PathNormalization(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"", "/dashboard"},
		{"/dashboard", "/dashboard"},
		{"/dashboard/", "/dashboard"},
		{"dashboard", "/dashboard"},
		{"/admin/console/", "/admin/console"},
		{"/", "/"},
	}
	for _, tt := range tests {
		cfg := config.DashboardConfig{Path: tt.input}
		got := cfg.DashboardPath()
		if got != tt.want {
			t.Errorf("DashboardPath(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

// SEC4-002: Verify dashboard response includes Content-Security-Policy header.
func TestDashboard_ContentSecurityPolicy(t *testing.T) {
	srv, _ := newTestServer(t)
	defer srv.Shutdown(context.Background())

	req := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	w := httptest.NewRecorder()
	srv.routes().ServeHTTP(w, req)

	csp := w.Header().Get("Content-Security-Policy")
	if csp == "" {
		t.Fatal("Content-Security-Policy header not set on dashboard response")
	}
	if !strings.Contains(csp, "script-src") {
		t.Errorf("CSP missing script-src directive: %s", csp)
	}
	if !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Errorf("CSP missing frame-ancestors 'none': %s", csp)
	}
	if !strings.Contains(csp, "cdnjs.cloudflare.com") {
		t.Errorf("CSP should allow D3 CDN: %s", csp)
	}
}

// Dummy reference for unused import suppression.
var _ = time.Second
