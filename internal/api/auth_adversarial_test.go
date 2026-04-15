package api

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/brianbuquoi/overlord/internal/auth"
	"github.com/brianbuquoi/overlord/internal/config"
)

// --- Test 3: Key in logs (middleware-level) ---

func TestAdversarial_KeyNotInLogs(t *testing.T) {
	secretKey := "adversarial-log-test-key-12345"
	t.Setenv("ADV_LOG_KEY", secretKey)

	keys, err := auth.LoadKeys([]config.AuthKeyConfig{
		{Name: "logcheck", KeyEnv: "ADV_LOG_KEY", Scopes: []string{"read"}},
	})
	if err != nil {
		t.Fatalf("LoadKeys: %v", err)
	}

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	tracker := auth.NewBruteForceTracker(100, time.Minute)

	handler := authMiddleware(keys, tracker, logger, endpointScope, nil)(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(200)
		}),
	)

	// Scenarios: valid, invalid, missing.
	tokens := []string{
		secretKey,
		"wrong-key-value",
		"",
	}

	for _, token := range tokens {
		req := httptest.NewRequest("GET", "/v1/tasks", nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		req.RemoteAddr = "127.0.0.1:12345"
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
	}

	logOutput := logBuf.String()
	if strings.Contains(logOutput, secretKey) {
		t.Error("CRITICAL: API key value found in log output")
	}
	if strings.Contains(logOutput, "wrong-key-value") {
		t.Error("CRITICAL: attempted key value found in log output")
	}
}

// --- Test 4: Key not in error responses ---

func TestAdversarial_KeyNotInErrorResponse(t *testing.T) {
	t.Setenv("ADV_ERR_KEY", "error-response-secret-key")

	keys, err := auth.LoadKeys([]config.AuthKeyConfig{
		{Name: "errcheck", KeyEnv: "ADV_ERR_KEY", Scopes: []string{"read"}},
	})
	if err != nil {
		t.Fatalf("LoadKeys: %v", err)
	}

	tracker := auth.NewBruteForceTracker(100, time.Minute)
	logger := slog.Default()

	handler := authMiddleware(keys, tracker, logger, endpointScope, nil)(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(200)
		}),
	)

	invalidTokens := []string{
		"invalid-key-attempt-12345",
		"error-response-secret-key-almost",
		"Bearer error-response-secret-key", // double Bearer
	}

	for _, token := range invalidTokens {
		req := httptest.NewRequest("GET", "/v1/tasks", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		req.RemoteAddr = "127.0.0.1:12345"
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		body := rec.Body.String()
		if strings.Contains(body, token) {
			t.Errorf("CRITICAL: submitted token %q echoed in response body", token)
		}
	}
}

// --- Test 6: Full scope boundary matrix ---

func TestAdversarial_ScopeBoundaryMatrix(t *testing.T) {
	t.Setenv("ADV_READ_KEY", "adv-read-key")
	t.Setenv("ADV_WRITE_KEY", "adv-write-key")
	t.Setenv("ADV_ADMIN_KEY", "adv-admin-key")

	keys, err := auth.LoadKeys([]config.AuthKeyConfig{
		{Name: "reader", KeyEnv: "ADV_READ_KEY", Scopes: []string{"read"}},
		{Name: "writer", KeyEnv: "ADV_WRITE_KEY", Scopes: []string{"write"}},
		{Name: "admin", KeyEnv: "ADV_ADMIN_KEY", Scopes: []string{"read", "write", "admin"}},
	})
	if err != nil {
		t.Fatalf("LoadKeys: %v", err)
	}

	tracker := auth.NewBruteForceTracker(10000, time.Hour)
	logger := slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil)) // Suppress output.

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			w.WriteHeader(202)
		} else {
			w.WriteHeader(200)
		}
	})
	handler := authMiddleware(keys, tracker, logger, endpointScope, nil)(inner)

	// Full matrix as specified in test prompt.
	type cell struct {
		method string
		path   string
		scope  string // "read", "write", "admin", "none"
		want   int
	}

	matrix := []cell{
		// GET /v1/tasks
		{"GET", "/v1/tasks", "read", 200},
		{"GET", "/v1/tasks", "write", 200},
		{"GET", "/v1/tasks", "admin", 200},
		{"GET", "/v1/tasks", "none", 401},
		// GET /v1/tasks/{id}
		{"GET", "/v1/tasks/abc123", "read", 200},
		{"GET", "/v1/tasks/abc123", "write", 200},
		{"GET", "/v1/tasks/abc123", "admin", 200},
		{"GET", "/v1/tasks/abc123", "none", 401},
		// POST /v1/pipelines/{id}/tasks
		{"POST", "/v1/pipelines/test/tasks", "read", 403},
		{"POST", "/v1/pipelines/test/tasks", "write", 202},
		{"POST", "/v1/pipelines/test/tasks", "admin", 202},
		{"POST", "/v1/pipelines/test/tasks", "none", 401},
		// GET /v1/pipelines
		{"GET", "/v1/pipelines", "read", 200},
		{"GET", "/v1/pipelines", "write", 200},
		{"GET", "/v1/pipelines", "admin", 200},
		{"GET", "/v1/pipelines", "none", 401},
		// GET /v1/health
		{"GET", "/v1/health", "read", 200},
		{"GET", "/v1/health", "write", 200},
		{"GET", "/v1/health", "admin", 200},
		{"GET", "/v1/health", "none", 401},
	}

	secrets := map[string]string{
		"read":  "adv-read-key",
		"write": "adv-write-key",
		"admin": "adv-admin-key",
	}

	for _, c := range matrix {
		name := c.method + " " + c.path + " " + c.scope
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(c.method, c.path, nil)
			req.RemoteAddr = "127.0.0.1:12345"

			if c.scope != "none" {
				req.Header.Set("Authorization", "Bearer "+secrets[c.scope])
			}

			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != c.want {
				t.Errorf("got %d, want %d", rec.Code, c.want)
			}
		})
	}
}

// --- Test 7: Scope escalation attempt ---

func TestAdversarial_ScopeEscalation(t *testing.T) {
	t.Setenv("ADV_ESC_KEY", "escalation-read-key")

	keys, err := auth.LoadKeys([]config.AuthKeyConfig{
		{Name: "reader", KeyEnv: "ADV_ESC_KEY", Scopes: []string{"read"}},
	})
	if err != nil {
		t.Fatalf("LoadKeys: %v", err)
	}

	tracker := auth.NewBruteForceTracker(1000, time.Hour)
	logger := slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil))

	handler := authMiddleware(keys, tracker, logger, endpointScope, nil)(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(202)
		}),
	)

	// Try various spoofed scope headers.
	spoofHeaders := map[string]string{
		"X-Scope":      "admin",
		"X-Override":   "write",
		"X-Auth-Scope": "write",
		"X-Role":       "admin",
		"X-Privilege":  "elevated",
	}

	for header, value := range spoofHeaders {
		t.Run(header, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/v1/pipelines/test/tasks", nil)
			req.Header.Set("Authorization", "Bearer escalation-read-key")
			req.Header.Set(header, value)
			req.RemoteAddr = "127.0.0.1:12345"
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusForbidden {
				t.Errorf("header %s=%s: expected 403, got %d", header, value, rec.Code)
			}
		})
	}
}

// --- Test 11-14: WebSocket auth ---

func TestAdversarial_WebSocket_NoToken(t *testing.T) {
	t.Setenv("ADV_WS_KEY", "websocket-test-key")

	keys, err := auth.LoadKeys([]config.AuthKeyConfig{
		{Name: "wskey", KeyEnv: "ADV_WS_KEY", Scopes: []string{"read"}},
	})
	if err != nil {
		t.Fatalf("LoadKeys: %v", err)
	}

	tracker := auth.NewBruteForceTracker(100, time.Minute)
	logger := slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil))

	handler := authMiddleware(keys, tracker, logger, endpointScope, nil)(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Simulate WebSocket upgrade handler — in real code this would
			// be the WS upgrader, but auth middleware runs BEFORE upgrade.
			w.WriteHeader(101)
		}),
	)

	// No token → 401, connection NOT upgraded.
	req := httptest.NewRequest("GET", "/v1/stream", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("WebSocket without token: expected 401, got %d", rec.Code)
	}
}

func TestAdversarial_WebSocket_ReadToken(t *testing.T) {
	t.Setenv("ADV_WS_KEY2", "websocket-read-key")

	keys, err := auth.LoadKeys([]config.AuthKeyConfig{
		{Name: "wsread", KeyEnv: "ADV_WS_KEY2", Scopes: []string{"read"}},
	})
	if err != nil {
		t.Fatalf("LoadKeys: %v", err)
	}

	tracker := auth.NewBruteForceTracker(100, time.Minute)
	logger := slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil))

	handler := authMiddleware(keys, tracker, logger, endpointScope, nil)(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(200) // Would be 101 with real upgrader.
		}),
	)

	// Read-scoped token on /v1/stream (GET, requires read) → should pass.
	req := httptest.NewRequest("GET", "/v1/stream", nil)
	req.Header.Set("Authorization", "Bearer websocket-read-key")
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("WebSocket with read token: expected 200, got %d", rec.Code)
	}
}

func TestAdversarial_WebSocket_InvalidToken(t *testing.T) {
	t.Setenv("ADV_WS_KEY3", "websocket-valid-key")

	keys, err := auth.LoadKeys([]config.AuthKeyConfig{
		{Name: "wsinvalid", KeyEnv: "ADV_WS_KEY3", Scopes: []string{"read"}},
	})
	if err != nil {
		t.Fatalf("LoadKeys: %v", err)
	}

	tracker := auth.NewBruteForceTracker(100, time.Minute)
	logger := slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil))

	handler := authMiddleware(keys, tracker, logger, endpointScope, nil)(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(200)
		}),
	)

	req := httptest.NewRequest("GET", "/v1/stream", nil)
	req.Header.Set("Authorization", "Bearer wrong-websocket-key")
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("WebSocket with invalid token: expected 401, got %d", rec.Code)
	}
}

func TestAdversarial_WebSocket_QueryParamToken(t *testing.T) {
	// Test 14: Token via query param — document behavior.
	// Query param auth is NOT supported. The endpoint must reject it
	// (return 401) rather than silently allowing unauthenticated access.

	t.Setenv("ADV_WS_KEY4", "websocket-query-key")

	keys, err := auth.LoadKeys([]config.AuthKeyConfig{
		{Name: "wsquery", KeyEnv: "ADV_WS_KEY4", Scopes: []string{"read"}},
	})
	if err != nil {
		t.Fatalf("LoadKeys: %v", err)
	}

	tracker := auth.NewBruteForceTracker(100, time.Minute)
	logger := slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil))

	handler := authMiddleware(keys, tracker, logger, endpointScope, nil)(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(200)
		}),
	)

	// Token only in query param, no Authorization header.
	req := httptest.NewRequest("GET", "/v1/stream?token=websocket-query-key", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("Query param token should NOT be accepted: expected 401, got %d", rec.Code)
	}

	t.Log("DOCUMENTED: Query parameter token (?token=...) is NOT supported. " +
		"Only the Authorization: Bearer header is accepted. The endpoint " +
		"correctly rejects query-param-only auth with 401.")
}

// --- Test 17: Race detector (brute force counter is concurrent-safe) ---

func TestAdversarial_BruteForce_RaceSafe(t *testing.T) {
	// This test is primarily for -race flag detection.
	tracker := auth.NewBruteForceTracker(100, time.Minute)

	done := make(chan struct{})
	for i := 0; i < 10; i++ {
		go func(ip string) {
			defer func() { done <- struct{}{} }()
			for j := 0; j < 100; j++ {
				tracker.RecordFailure(ip)
				tracker.IsBlocked(ip)
				if j%10 == 0 {
					tracker.RecordSuccess(ip)
				}
			}
		}(strings.Repeat("x", i+1))
	}

	for i := 0; i < 10; i++ {
		<-done
	}
}

// TestAdversarial_MetricsNoAuth was a t.Log() stub. It has been replaced by
// TestRoutes_MetricsAuthBypass in server_test.go which tests the actual
// routes() code path with a full server instance.
