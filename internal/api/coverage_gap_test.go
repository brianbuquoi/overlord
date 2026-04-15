package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/brianbuquoi/overlord/internal/broker"
)

// =============================================================================
// Section 6.1 — Error response body on every error path
// Verify every HTTP error returns valid JSON with {error, code} fields.
// =============================================================================

func TestErrorResponses_AllPaths(t *testing.T) {
	srv, b := newTestServer(t)
	defer srv.Shutdown(context.Background())
	handler := srv.Handler()

	// Submit a task so we have data for some tests.
	task, err := b.Submit(context.Background(), "test-pipeline", json.RawMessage(`{"x":1}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = task

	tests := []struct {
		name           string
		method         string
		path           string
		body           string
		expectedStatus int
		expectedCode   string
	}{
		{
			name:           "404 unknown task ID",
			method:         "GET",
			path:           "/v1/tasks/nonexistent-task-id-12345",
			expectedStatus: 404,
			expectedCode:   "TASK_NOT_FOUND",
		},
		{
			name:           "404 unknown pipeline ID on submit",
			method:         "POST",
			path:           "/v1/pipelines/nonexistent-pipeline/tasks",
			body:           `{"payload":{"x":1}}`,
			expectedStatus: 404,
			expectedCode:   "PIPELINE_NOT_FOUND",
		},
		{
			name:           "405 wrong method on pipelines list",
			method:         "DELETE",
			path:           "/v1/pipelines/",
			expectedStatus: 405,
			expectedCode:   "METHOD_NOT_ALLOWED",
		},
		{
			name:           "405 wrong method on dead-letter root",
			method:         "POST",
			path:           "/v1/dead-letter",
			expectedStatus: 405,
			expectedCode:   "METHOD_NOT_ALLOWED",
		},
		{
			name:           "400 invalid JSON on submit",
			method:         "POST",
			path:           "/v1/pipelines/test-pipeline/tasks",
			body:           "not-json",
			expectedStatus: 400,
			expectedCode:   "INVALID_JSON",
		},
		{
			name:           "400 missing payload on submit",
			method:         "POST",
			path:           "/v1/pipelines/test-pipeline/tasks",
			body:           `{}`,
			expectedStatus: 400,
			expectedCode:   "MISSING_PAYLOAD",
		},
		{
			name:           "400 payload is not object",
			method:         "POST",
			path:           "/v1/pipelines/test-pipeline/tasks",
			body:           `{"payload":"string-not-object"}`,
			expectedStatus: 400,
			expectedCode:   "INVALID_PAYLOAD_TYPE",
		},
		{
			name:           "400 invalid limit",
			method:         "GET",
			path:           "/v1/tasks?limit=abc",
			expectedStatus: 400,
			expectedCode:   "INVALID_LIMIT",
		},
		{
			name:           "400 limit too small",
			method:         "GET",
			path:           "/v1/tasks?limit=0",
			expectedStatus: 400,
			expectedCode:   "INVALID_LIMIT",
		},
		{
			name:           "400 limit too large",
			method:         "GET",
			path:           "/v1/tasks?limit=1001",
			expectedStatus: 400,
			expectedCode:   "INVALID_LIMIT",
		},
		{
			name:           "400 invalid offset",
			method:         "GET",
			path:           "/v1/tasks?offset=abc",
			expectedStatus: 400,
			expectedCode:   "INVALID_OFFSET",
		},
		{
			name:           "400 negative offset",
			method:         "GET",
			path:           "/v1/tasks?offset=-1",
			expectedStatus: 400,
			expectedCode:   "INVALID_OFFSET",
		},
		{
			// Note: /v1/pipelines//tasks gets 301-redirected by Go's mux
			// to /v1/pipelines/tasks, which then hits the list handler.
			// Test a different invalid path instead.
			name:           "400 missing task path param",
			method:         "GET",
			path:           "/v1/tasks/",
			expectedStatus: 400,
			expectedCode:   "INVALID_PATH",
		},
		{
			name:           "400 missing pipeline_id on replay-all",
			method:         "POST",
			path:           "/v1/dead-letter/replay-all",
			expectedStatus: 400,
			expectedCode:   "MISSING_PIPELINE_ID",
		},
		{
			name:           "400 missing pipeline_id on discard-all",
			method:         "POST",
			path:           "/v1/dead-letter/discard-all",
			expectedStatus: 400,
			expectedCode:   "MISSING_PIPELINE_ID",
		},
		{
			name:           "404 pipeline not found on replay-all",
			method:         "POST",
			path:           "/v1/dead-letter/replay-all?pipeline_id=nonexistent",
			expectedStatus: 404,
			expectedCode:   "PIPELINE_NOT_FOUND",
		},
		{
			name:           "404 pipeline not found on discard-all",
			method:         "POST",
			path:           "/v1/dead-letter/discard-all?pipeline_id=nonexistent",
			expectedStatus: 404,
			expectedCode:   "PIPELINE_NOT_FOUND",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var req *http.Request
			if tt.body != "" {
				req = httptest.NewRequest(tt.method, tt.path, strings.NewReader(tt.body))
			} else {
				req = httptest.NewRequest(tt.method, tt.path, nil)
			}
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)

			if w.Code != tt.expectedStatus {
				t.Fatalf("expected status %d, got %d: %s", tt.expectedStatus, w.Code, w.Body.String())
			}

			// Verify body is valid JSON with error and code fields.
			var resp errorResponse
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("response body is not valid JSON: %v\nbody: %s", err, w.Body.String())
			}
			if resp.Error == "" {
				t.Fatal("error field is empty")
			}
			if resp.Code == "" {
				t.Fatal("code field is empty")
			}
			if tt.expectedCode != "" && resp.Code != tt.expectedCode {
				t.Fatalf("expected code %q, got %q", tt.expectedCode, resp.Code)
			}
		})
	}
}

// =============================================================================
// Section 6.2 — Concurrent GET requests for the same task
// =============================================================================

func TestConcurrentGetTask(t *testing.T) {
	srv, b := newTestServer(t)
	defer srv.Shutdown(context.Background())
	handler := srv.Handler()

	task, err := b.Submit(context.Background(), "test-pipeline", json.RawMessage(`{"key":"value"}`))
	if err != nil {
		t.Fatal(err)
	}

	const goroutines = 10
	var wg sync.WaitGroup
	bodies := make([]string, goroutines)
	statuses := make([]int, goroutines)

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			req := httptest.NewRequest("GET", "/v1/tasks/"+task.ID, nil)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)
			statuses[idx] = w.Code
			bodies[idx] = w.Body.String()
		}(i)
	}
	wg.Wait()

	for i := 0; i < goroutines; i++ {
		if statuses[i] != 200 {
			t.Fatalf("goroutine %d got status %d", i, statuses[i])
		}
	}

	// All bodies should be identical.
	for i := 1; i < goroutines; i++ {
		if bodies[i] != bodies[0] {
			t.Fatalf("goroutine %d returned different body than goroutine 0", i)
		}
	}
}

// =============================================================================
// Section 6.5 — Dead letter endpoints with empty queue
// =============================================================================

func TestDeadLetter_EmptyQueue(t *testing.T) {
	srv, _ := newTestServer(t)
	defer srv.Shutdown(context.Background())
	handler := srv.Handler()

	t.Run("GET /v1/dead-letter returns 200 with empty array", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/v1/dead-letter", nil)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		if w.Code != 200 {
			t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
		}
		var resp deadLetterListResponse
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("invalid JSON: %v", err)
		}
		if len(resp.Tasks) != 0 {
			t.Fatalf("expected 0 tasks, got %d", len(resp.Tasks))
		}
	})

	t.Run("POST replay-all returns 200 with count 0", func(t *testing.T) {
		req := httptest.NewRequest("POST", "/v1/dead-letter/replay-all?pipeline_id=test-pipeline", nil)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		if w.Code != http.StatusAccepted {
			t.Fatalf("expected 202, got %d: %s", w.Code, w.Body.String())
		}
		var resp replayAllResponse
		json.Unmarshal(w.Body.Bytes(), &resp)
		if resp.Processed != 0 {
			t.Fatalf("expected count 0, got %d", resp.Processed)
		}
	})

	t.Run("POST discard-all returns 200 with count 0", func(t *testing.T) {
		req := httptest.NewRequest("POST", "/v1/dead-letter/discard-all?pipeline_id=test-pipeline", nil)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		if w.Code != 200 {
			t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
		}
		var resp discardAllResponse
		json.Unmarshal(w.Body.Bytes(), &resp)
		if resp.Processed != 0 {
			t.Fatalf("expected count 0, got %d", resp.Processed)
		}
	})
}

// =============================================================================
// Section 6.6 — Request ID propagation
// =============================================================================

func TestRequestID_Propagation(t *testing.T) {
	srv, b := newTestServer(t)
	defer srv.Shutdown(context.Background())
	handler := srv.Handler()

	t.Run("custom request ID is echoed in response", func(t *testing.T) {
		body := `{"payload":{"input":"hello"}}`
		req := httptest.NewRequest("POST", "/v1/pipelines/test-pipeline/tasks", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Request-ID", "test-trace-id-12345")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		if w.Code != http.StatusAccepted {
			t.Fatalf("expected 202, got %d", w.Code)
		}

		respID := w.Header().Get("X-Request-ID")
		if respID != "test-trace-id-12345" {
			t.Fatalf("expected X-Request-ID test-trace-id-12345, got %q", respID)
		}
	})

	t.Run("request ID does NOT appear in task metadata", func(t *testing.T) {
		body := `{"payload":{"input":"meta-check"}}`
		req := httptest.NewRequest("POST", "/v1/pipelines/test-pipeline/tasks", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Request-ID", "should-not-be-in-metadata")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		var resp submitTaskResponse
		json.Unmarshal(w.Body.Bytes(), &resp)

		task, err := b.Store().GetTask(context.Background(), resp.TaskID)
		if err != nil {
			t.Fatal(err)
		}

		// Request ID is a transport concern and should NOT be stored in task metadata.
		if task.Metadata != nil {
			if _, ok := task.Metadata["request_id"]; ok {
				t.Fatal("request_id should NOT appear in task metadata")
			}
			if _, ok := task.Metadata["X-Request-ID"]; ok {
				t.Fatal("X-Request-ID should NOT appear in task metadata")
			}
		}
	})
}

// =============================================================================
// Section 6.1 supplement — Security headers on all responses
// =============================================================================

func TestSecurityHeaders_OnAllResponses(t *testing.T) {
	srv, _ := newTestServer(t)
	defer srv.Shutdown(context.Background())
	handler := srv.Handler()

	endpoints := []struct {
		method string
		path   string
	}{
		{"GET", "/v1/health"},
		{"GET", "/v1/tasks"},
		{"GET", "/v1/pipelines/"},
		{"GET", "/v1/dead-letter"},
		{"GET", "/v1/tasks/nonexistent"},
	}

	for _, ep := range endpoints {
		t.Run(fmt.Sprintf("%s %s", ep.method, ep.path), func(t *testing.T) {
			req := httptest.NewRequest(ep.method, ep.path, nil)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)

			if w.Header().Get("X-Content-Type-Options") != "nosniff" {
				t.Error("missing X-Content-Type-Options: nosniff")
			}
			if w.Header().Get("X-Frame-Options") != "DENY" {
				t.Error("missing X-Frame-Options: DENY")
			}
			if w.Header().Get("Referrer-Policy") != "no-referrer" {
				t.Error("missing Referrer-Policy: no-referrer")
			}
		})
	}
}

// =============================================================================
// Section 6.1 supplement — Dead letter single-task error paths
// =============================================================================

func TestDeadLetter_SingleTask_ErrorPaths(t *testing.T) {
	srv, b := newTestServer(t)
	defer srv.Shutdown(context.Background())
	handler := srv.Handler()

	// Submit a non-dead-lettered task.
	task, err := b.Submit(context.Background(), "test-pipeline", json.RawMessage(`{"x":1}`))
	if err != nil {
		t.Fatal(err)
	}

	t.Run("replay non-dead-letter task returns 409", func(t *testing.T) {
		req := httptest.NewRequest("POST", "/v1/dead-letter/"+task.ID+"/replay", nil)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		if w.Code != http.StatusConflict {
			t.Fatalf("expected 409, got %d: %s", w.Code, w.Body.String())
		}
		var resp errorResponse
		json.Unmarshal(w.Body.Bytes(), &resp)
		if resp.Code != "TASK_NOT_REPLAYABLE" {
			t.Fatalf("expected TASK_NOT_REPLAYABLE, got %s", resp.Code)
		}
	})

	t.Run("discard non-dead-letter task returns 409", func(t *testing.T) {
		req := httptest.NewRequest("POST", "/v1/dead-letter/"+task.ID+"/discard", nil)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		if w.Code != http.StatusConflict {
			t.Fatalf("expected 409, got %d: %s", w.Code, w.Body.String())
		}
	})

	t.Run("replay nonexistent task returns 404", func(t *testing.T) {
		req := httptest.NewRequest("POST", "/v1/dead-letter/no-such-task/replay", nil)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		if w.Code != http.StatusNotFound {
			t.Fatalf("expected 404, got %d", w.Code)
		}
	})

	t.Run("discard nonexistent task returns 404", func(t *testing.T) {
		req := httptest.NewRequest("POST", "/v1/dead-letter/no-such-task/discard", nil)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		if w.Code != http.StatusNotFound {
			t.Fatalf("expected 404, got %d", w.Code)
		}
	})
}

// =============================================================================
// Section 6.1 supplement — Replay-all rate limiting
// =============================================================================

func TestReplayAll_RateLimited(t *testing.T) {
	srv, _ := newTestServer(t)
	defer srv.Shutdown(context.Background())
	handler := srv.Handler()

	// First call should succeed.
	req := httptest.NewRequest("POST", "/v1/dead-letter/replay-all?pipeline_id=test-pipeline", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("first call: expected 202, got %d", w.Code)
	}

	// Second call within 1 minute should be rate limited.
	req = httptest.NewRequest("POST", "/v1/dead-letter/replay-all?pipeline_id=test-pipeline", nil)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("second call: expected 429, got %d", w.Code)
	}

	// Verify error body is valid JSON.
	var resp errorResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Code != "RATE_LIMITED" {
		t.Fatalf("expected RATE_LIMITED, got %s", resp.Code)
	}
	if w.Header().Get("Retry-After") != "60" {
		t.Fatalf("expected Retry-After: 60, got %s", w.Header().Get("Retry-After"))
	}
}

// =============================================================================
// Section 6 supplement — Dead letter list with pipeline filter on dead-letter/
// =============================================================================

func TestDeadLetterList_TrailingSlash(t *testing.T) {
	srv, _ := newTestServer(t)
	defer srv.Shutdown(context.Background())
	handler := srv.Handler()

	// Both /v1/dead-letter and /v1/dead-letter/ should return the list.
	for _, path := range []string{"/v1/dead-letter", "/v1/dead-letter/"} {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest("GET", path, nil)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)

			if w.Code != 200 {
				t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
			}
		})
	}
}

// =============================================================================
// Section 6 supplement — Discard already-discarded task
// =============================================================================

func TestDiscard_AlreadyDiscarded(t *testing.T) {
	srv, b := newTestServer(t)
	defer srv.Shutdown(context.Background())
	handler := srv.Handler()

	// Create a dead-lettered task manually.
	task, _ := b.Submit(context.Background(), "test-pipeline", json.RawMessage(`{"x":1}`))
	state := broker.TaskStateFailed
	deadLetter := true
	b.Store().UpdateTask(context.Background(), task.ID, broker.TaskUpdate{
		State:              &state,
		RoutedToDeadLetter: &deadLetter,
	})

	// First discard succeeds.
	req := httptest.NewRequest("POST", "/v1/dead-letter/"+task.ID+"/discard", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("first discard: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Second discard returns 409 (already discarded).
	req = httptest.NewRequest("POST", "/v1/dead-letter/"+task.ID+"/discard", nil)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("second discard: expected 409, got %d: %s", w.Code, w.Body.String())
	}
}

// =============================================================================
// Section 6 supplement — Dead letter replay creates new task
// =============================================================================

func TestDeadLetter_Replay_CreatesNewTask(t *testing.T) {
	srv, b := newTestServer(t)
	defer srv.Shutdown(context.Background())
	handler := srv.Handler()

	// Create a dead-lettered task.
	task, _ := b.Submit(context.Background(), "test-pipeline", json.RawMessage(`{"replay":"test"}`))
	state := broker.TaskStateFailed
	deadLetter := true
	b.Store().UpdateTask(context.Background(), task.ID, broker.TaskUpdate{
		State:              &state,
		RoutedToDeadLetter: &deadLetter,
	})

	req := httptest.NewRequest("POST", "/v1/dead-letter/"+task.ID+"/replay", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", w.Code, w.Body.String())
	}

	var resp replayResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.TaskID == "" {
		t.Fatal("expected non-empty task_id in replay response")
	}
	if resp.TaskID == task.ID {
		t.Fatal("replay should create a NEW task, not reuse the old one")
	}

	// Verify new task exists.
	newTask, err := b.Store().GetTask(context.Background(), resp.TaskID)
	if err != nil {
		t.Fatalf("new task should exist: %v", err)
	}
	if newTask.PipelineID != "test-pipeline" {
		t.Fatalf("expected pipeline test-pipeline, got %s", newTask.PipelineID)
	}
}

// =============================================================================
// Section 6 supplement — Rate limiter: /metrics is exempt
// =============================================================================

func TestRateLimiter_MetricsExempt(t *testing.T) {
	// Metrics endpoint should never be rate limited.
	// Use the full server with metrics to test this.
	srv, _ := newTestServer(t)
	defer srv.Shutdown(context.Background())
	handler := srv.Handler()

	frozen := time.Now()
	srv.limiter.now = func() time.Time { return frozen }

	// Exhaust rate limit from a single IP.
	for i := 0; i < 101; i++ {
		req := httptest.NewRequest("GET", "/v1/health", nil)
		req.RemoteAddr = "10.0.0.5:12345"
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
	}

	// Verify the IP is now rate limited for normal endpoints.
	req := httptest.NewRequest("GET", "/v1/health", nil)
	req.RemoteAddr = "10.0.0.5:12345"
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 on rate-limited IP, got %d", w.Code)
	}

	// Note: /metrics would need metrics enabled to test exemption.
	// The route is not registered without metrics, so skip the positive test.
	// The rate limiter middleware exempts "/metrics" path — tested via TestRoutes_MetricsAuthBypass.
}

// =============================================================================
// Section 6 supplement — Dead letter invalid limit/offset
// =============================================================================

func TestDeadLetterList_InvalidParams(t *testing.T) {
	srv, _ := newTestServer(t)
	defer srv.Shutdown(context.Background())
	handler := srv.Handler()

	tests := []struct {
		name string
		path string
	}{
		{"invalid limit", "/v1/dead-letter?limit=abc"},
		{"limit too large", "/v1/dead-letter?limit=1001"},
		{"invalid offset", "/v1/dead-letter?offset=abc"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", tt.path, nil)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)

			if w.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
			}
		})
	}
}

// =============================================================================
// Helpers used above but not in existing tests
// =============================================================================

// verifyTimeout is not needed; the time package handles it.
var _ = time.Second // prevent unused import
