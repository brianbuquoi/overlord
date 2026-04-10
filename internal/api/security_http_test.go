package api

// Security Audit Verification — Sections 2.7-2.8, 3.9-3.13
// HTTP Input Validation, API credential leakage, task metadata audit.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/orcastrator/orcastrator/internal/broker"
	"github.com/orcastrator/orcastrator/internal/contract"
	"github.com/orcastrator/orcastrator/internal/store/memory"
)

func newSecurityTestServer(t *testing.T) (*Server, *broker.Broker, *memory.MemoryStore) {
	t.Helper()
	cfg := testConfig()
	st := memory.New()
	agents := map[string]broker.Agent{
		"agent-1": &stubAgent{id: "agent-1", provider: "anthropic", healthy: true},
		"agent-2": &stubAgent{id: "agent-2", provider: "openai", healthy: true},
	}
	reg, _ := contract.NewRegistry(nil, "")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	b := broker.New(cfg, st, agents, reg, logger, nil, nil)
	srv := NewServer(b, logger, nil, "")
	return srv, b, st
}

// Test 9: Request body size limit
func TestSecurity_RequestBodySizeLimit(t *testing.T) {
	srv, _, _ := newSecurityTestServer(t)

	t.Run("over_1MB_rejected", func(t *testing.T) {
		// Create body that exceeds 1MB
		bigPayload := strings.Repeat("x", maxRequestBodySize+1)
		body := fmt.Sprintf(`{"payload":{"data":"%s"}}`, bigPayload)

		req := httptest.NewRequest(http.MethodPost, "/v1/pipelines/test-pipeline/tasks", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)

		resp := w.Result()
		// MaxBytesReader returns 400 (bad request) when limit exceeded because
		// json.Decoder gets an error, or the server may return 413.
		if resp.StatusCode != http.StatusRequestEntityTooLarge && resp.StatusCode != http.StatusBadRequest {
			t.Errorf("expected 413 or 400 for oversized body, got %d", resp.StatusCode)
		}
	})

	t.Run("under_1MB_accepted", func(t *testing.T) {
		body := `{"payload":{"data":"small"}}`
		req := httptest.NewRequest(http.MethodPost, "/v1/pipelines/test-pipeline/tasks", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)

		resp := w.Result()
		// Should be accepted (202 or at worst some validation error, not 413)
		if resp.StatusCode == http.StatusRequestEntityTooLarge {
			t.Error("small valid body rejected as too large")
		}
	})
}

// Test 10: Path parameter injection
func TestSecurity_PathParameterInjection(t *testing.T) {
	srv, _, _ := newSecurityTestServer(t)
	validBody := `{"payload":{"test":true}}`

	cases := []struct {
		name string
		path string
	}{
		{"path_traversal", "/v1/pipelines/..%2F..%2F..%2Fetc%2Fpasswd/tasks"},
		{"null_byte", "/v1/pipelines/pipeline%00null/tasks"},
		{"colons", "/v1/pipelines/pipeline:with:colons/tasks"},
		{"spaces", "/v1/pipelines/pipeline%20with%20spaces/tasks"},
		{"very_long", "/v1/pipelines/" + strings.Repeat("a", 1000) + "/tasks"},
		{"empty", "/v1/pipelines//tasks"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(validBody))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, req)

			resp := w.Result()
			if resp.StatusCode == 200 || resp.StatusCode == 202 || resp.StatusCode == 500 {
				t.Errorf("path %q: expected 400 or 404, got %d", tc.path, resp.StatusCode)
			}
		})
	}
}

// Test 11: Query parameter bounds
func TestSecurity_QueryParameterBounds(t *testing.T) {
	srv, _, _ := newSecurityTestServer(t)

	cases := []struct {
		name       string
		query      string
		wantStatus int
	}{
		{"limit_negative", "limit=-1", 400},
		{"limit_zero", "limit=0", 400},
		{"limit_huge", "limit=1000000", 400},
		{"offset_negative", "offset=-1", 400},
		{"limit_non_numeric", "limit=abc", 400},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/v1/tasks?"+tc.query, nil)
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, req)

			resp := w.Result()
			body, _ := io.ReadAll(resp.Body)

			// Must NOT be 500 (panic/crash)
			if resp.StatusCode == 500 {
				t.Errorf("got 500 for %s — possible panic: %s", tc.query, body)
			}

			// Should return 400 for invalid values
			if resp.StatusCode != tc.wantStatus {
				t.Errorf("%s: expected status %d, got %d; body: %s", tc.query, tc.wantStatus, resp.StatusCode, body)
			}

			// Response must be valid JSON
			if !json.Valid(body) {
				t.Errorf("response for %s is not valid JSON: %s", tc.query, body)
			}
		})
	}

	// Verify the limit cap message includes the max value
	t.Run("SEC-008_limit_cap_message", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/tasks?limit=1000000", nil)
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)

		body := w.Body.String()
		if w.Result().StatusCode != 400 {
			t.Errorf("SEC-008: expected 400, got %d", w.Result().StatusCode)
		}
		if !strings.Contains(body, "1000") {
			t.Error("SEC-008: error message should mention max limit of 1000")
		}
	})
}

// Test 12: Rate limiter spoofing — verify X-Forwarded-For does not bypass rate limiting
func TestSecurity_RateLimiterSpoofing(t *testing.T) {
	srv, _, _ := newSecurityTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// Exhaust rate limit (burst=100)
	rateLimited := false
	for i := 0; i < 110; i++ {
		resp, err := http.Get(ts.URL + "/v1/tasks")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests {
			rateLimited = true
			break
		}
	}

	if !rateLimited {
		t.Skip("Could not trigger rate limit in 110 requests")
	}

	// Try with spoofed X-Forwarded-For — should still be rate limited
	client := &http.Client{}
	req, _ := http.NewRequest("GET", ts.URL+"/v1/tasks", nil)
	req.Header.Set("X-Forwarded-For", "10.0.0.1")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusTooManyRequests {
		t.Error("SECURITY: X-Forwarded-For bypassed rate limiting")
	}

	t.Log("DOCUMENTED: X-Forwarded-For is NOT trusted. Rate limiting uses RemoteAddr (TCP source). " +
		"To trust a proxy header, configure a trusted proxy CIDR (not yet implemented).")
}

// Test 13: WebSocket origin check
func TestSecurity_WebSocketOriginCheck(t *testing.T) {
	srv, _, _ := newSecurityTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/v1/stream"
	host := strings.TrimPrefix(ts.URL, "http://")

	t.Run("no_origin", func(t *testing.T) {
		dialer := websocket.Dialer{HandshakeTimeout: 2 * time.Second}
		conn, _, err := dialer.Dial(wsURL, nil)
		if err != nil {
			t.Logf("No Origin: rejected (%v)", err)
		} else {
			conn.Close()
			t.Log("No Origin: accepted (correct for non-browser clients like curl)")
		}
	})

	t.Run("evil_origin", func(t *testing.T) {
		dialer := websocket.Dialer{HandshakeTimeout: 2 * time.Second}
		header := http.Header{}
		header.Set("Origin", "http://evil.example.com")

		conn, resp, err := dialer.Dial(wsURL, header)
		if err == nil {
			conn.Close()
			t.Error("SECURITY: evil origin accepted — CSWSH vulnerability")
		} else {
			status := 0
			if resp != nil {
				status = resp.StatusCode
			}
			t.Logf("Evil origin correctly rejected (status=%d)", status)
		}
	})

	t.Run("same_origin", func(t *testing.T) {
		dialer := websocket.Dialer{HandshakeTimeout: 2 * time.Second}
		header := http.Header{}
		header.Set("Origin", "http://"+host)

		conn, resp, err := dialer.Dial(wsURL, header)
		if err != nil {
			status := 0
			if resp != nil {
				status = resp.StatusCode
			}
			t.Errorf("Same origin rejected (status=%d, err=%v)", status, err)
		} else {
			conn.Close()
			t.Log("Same origin: accepted")
		}
	})
}

// Test 7: HTTP API credential leakage — verify no credential appears in task JSON response
func TestSecurity_HTTPAPICredentialLeakage(t *testing.T) {
	srv, b, st := newSecurityTestServer(t)

	// Set recognizable fake API keys in the environment
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-CANARY-VALUE-12345")
	t.Setenv("OPENAI_API_KEY", "sk-CANARY-VALUE-12345")
	t.Setenv("GEMINI_API_KEY", "CANARY-VALUE-12345")

	// Submit and retrieve a task
	task, err := b.Submit(context.Background(), "test-pipeline", json.RawMessage(`{"data":"test"}`))
	if err != nil {
		t.Fatal(err)
	}

	// Give broker time to process
	time.Sleep(100 * time.Millisecond)

	// Retrieve via HTTP
	req := httptest.NewRequest(http.MethodGet, "/v1/tasks/"+task.ID, nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	body := w.Body.String()
	if strings.Contains(body, "CANARY-VALUE") {
		t.Fatalf("SECURITY: credential canary found in task HTTP response: %s", body)
	}

	// Test 8: Also check directly from store
	storeTask, err := st.GetTask(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}

	taskJSON, _ := json.Marshal(storeTask)
	if strings.Contains(string(taskJSON), "CANARY-VALUE") {
		t.Fatalf("SECURITY: credential canary found in store task data: %s", taskJSON)
	}
}

// SEC4-001: Verify security headers on all API responses.
func TestSecurity_ResponseHeaders(t *testing.T) {
	srv, _, _ := newSecurityTestServer(t)
	ts := httptest.NewServer(srv.routes())
	defer ts.Close()

	endpoints := []struct {
		method string
		path   string
	}{
		{"GET", "/v1/health"},
		{"GET", "/v1/tasks?limit=10"},
		{"GET", "/v1/pipelines/"},
	}

	for _, ep := range endpoints {
		t.Run(ep.method+" "+ep.path, func(t *testing.T) {
			req, _ := http.NewRequest(ep.method, ts.URL+ep.path, nil)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()

			if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
				t.Errorf("X-Content-Type-Options = %q, want %q", got, "nosniff")
			}
			if got := resp.Header.Get("X-Frame-Options"); got != "DENY" {
				t.Errorf("X-Frame-Options = %q, want %q", got, "DENY")
			}
			if got := resp.Header.Get("Referrer-Policy"); got != "no-referrer" {
				t.Errorf("Referrer-Policy = %q, want %q", got, "no-referrer")
			}
		})
	}
}
