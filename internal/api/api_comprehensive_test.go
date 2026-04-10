package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/orcastrator/orcastrator/internal/broker"
	"github.com/orcastrator/orcastrator/internal/config"
	"github.com/orcastrator/orcastrator/internal/contract"
	"github.com/orcastrator/orcastrator/internal/store/memory"
)

// --- Helpers ---

// slowAgent simulates an agent whose HealthCheck blocks for a configurable duration.
type slowAgent struct {
	id       string
	provider string
	delay    time.Duration
}

func (a *slowAgent) ID() string       { return a.id }
func (a *slowAgent) Provider() string { return a.provider }
func (a *slowAgent) Execute(ctx context.Context, task *broker.Task) (*broker.TaskResult, error) {
	return &broker.TaskResult{TaskID: task.ID, Payload: json.RawMessage(`{"ok":true}`)}, nil
}
func (a *slowAgent) HealthCheck(ctx context.Context) error {
	select {
	case <-time.After(a.delay):
		return fmt.Errorf("health check timed out after %v", a.delay)
	case <-ctx.Done():
		return ctx.Err()
	}
}

func testConfigMultiPipeline() *config.Config {
	return &config.Config{
		Version: "1",
		Pipelines: []config.Pipeline{
			{
				Name:        "pipeline-a",
				Concurrency: 1,
				Store:       "memory",
				Stages: []config.Stage{
					{
						ID:    "intake",
						Agent: "agent-1",
						InputSchema: config.StageSchemaRef{
							Name: "intake_input", Version: "v1",
						},
						OutputSchema: config.StageSchemaRef{
							Name: "intake_output", Version: "v1",
						},
						OnSuccess: config.StaticOnSuccess("done"),
						OnFailure: "dead-letter",
					},
				},
			},
			{
				Name:        "pipeline-b",
				Concurrency: 1,
				Store:       "memory",
				Stages: []config.Stage{
					{
						ID:    "intake-b",
						Agent: "agent-2",
						InputSchema: config.StageSchemaRef{
							Name: "intake_input", Version: "v1",
						},
						OutputSchema: config.StageSchemaRef{
							Name: "intake_output", Version: "v1",
						},
						OnSuccess: config.StaticOnSuccess("done"),
						OnFailure: "dead-letter",
					},
				},
			},
		},
		Agents: []config.Agent{
			{ID: "agent-1", Provider: "anthropic", Model: "claude-opus-4-5"},
			{ID: "agent-2", Provider: "openai", Model: "gpt-4o"},
			{ID: "agent-3", Provider: "google", Model: "gemini-pro"},
		},
	}
}

func newMultiPipelineTestServer(t *testing.T) (*Server, *broker.Broker) {
	t.Helper()
	cfg := testConfigMultiPipeline()
	st := memory.New()
	agents := map[string]broker.Agent{
		"agent-1": &stubAgent{id: "agent-1", provider: "anthropic", healthy: true},
		"agent-2": &stubAgent{id: "agent-2", provider: "openai", healthy: true},
		"agent-3": &stubAgent{id: "agent-3", provider: "google", healthy: true},
	}
	reg, _ := contract.NewRegistry(nil, "")
	logger := slog.Default()
	b := broker.New(cfg, st, agents, reg, logger, nil, nil)
	srv := NewServer(b, logger, nil, "")
	return srv, b
}

// submitTask is a helper that POSTs a task and returns the response.
func submitTask(t *testing.T, handler http.Handler, pipelineID string, payload string) *httptest.ResponseRecorder {
	t.Helper()
	body := fmt.Sprintf(`{"payload":%s}`, payload)
	req := httptest.NewRequest(http.MethodPost, "/v1/pipelines/"+pipelineID+"/tasks", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	return w
}

// =====================================================================
// 1. Task Submission Validation
// =====================================================================

func TestSubmitTask_InvalidJSON_StructuredError(t *testing.T) {
	srv, _ := newTestServer(t)
	defer srv.Shutdown(context.Background())

	req := httptest.NewRequest(http.MethodPost, "/v1/pipelines/test-pipeline/tasks", strings.NewReader("not valid json {"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}

	var errResp errorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	if errResp.Error == "" {
		t.Fatal("expected non-empty error message")
	}
	if errResp.Code == "" {
		t.Fatal("expected non-empty error code")
	}
}

func TestSubmitTask_StringPayload_Rejected(t *testing.T) {
	srv, _ := newTestServer(t)
	defer srv.Shutdown(context.Background())

	// Payload is a valid JSON string, but not an object.
	body := `{"payload":"hello world"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/pipelines/test-pipeline/tasks", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for string payload, got %d: %s", w.Code, w.Body.String())
	}

	var errResp errorResponse
	json.Unmarshal(w.Body.Bytes(), &errResp)
	if errResp.Code != "INVALID_PAYLOAD_TYPE" {
		t.Fatalf("expected code INVALID_PAYLOAD_TYPE, got %s", errResp.Code)
	}
}

func TestSubmitTask_ArrayPayload_Rejected(t *testing.T) {
	srv, _ := newTestServer(t)
	defer srv.Shutdown(context.Background())

	body := `{"payload":[1,2,3]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/pipelines/test-pipeline/tasks", strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for array payload, got %d: %s", w.Code, w.Body.String())
	}
}

func TestSubmitTask_NumberPayload_Rejected(t *testing.T) {
	srv, _ := newTestServer(t)
	defer srv.Shutdown(context.Background())

	body := `{"payload":42}`
	req := httptest.NewRequest(http.MethodPost, "/v1/pipelines/test-pipeline/tasks", strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for number payload, got %d: %s", w.Code, w.Body.String())
	}
}

func TestSubmitTask_BoolPayload_Rejected(t *testing.T) {
	srv, _ := newTestServer(t)
	defer srv.Shutdown(context.Background())

	body := `{"payload":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/pipelines/test-pipeline/tasks", strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for bool payload, got %d: %s", w.Code, w.Body.String())
	}
}

func TestSubmitTask_PipelineNotFound_404(t *testing.T) {
	srv, _ := newTestServer(t)
	defer srv.Shutdown(context.Background())

	body := `{"payload":{"input":"hello"}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/pipelines/nonexistent-pipeline/tasks", strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}

	var errResp errorResponse
	json.Unmarshal(w.Body.Bytes(), &errResp)
	if errResp.Code != "PIPELINE_NOT_FOUND" {
		t.Fatalf("expected code PIPELINE_NOT_FOUND, got %s", errResp.Code)
	}
}

func TestSubmitTask_MissingContentType_StillWorks(t *testing.T) {
	srv, _ := newTestServer(t)
	defer srv.Shutdown(context.Background())

	body := `{"payload":{"input":"hello"}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/pipelines/test-pipeline/tasks", strings.NewReader(body))
	// Deliberately do NOT set Content-Type header.
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202 even without Content-Type, got %d: %s", w.Code, w.Body.String())
	}
}

// =====================================================================
// 2. Task State Consistency — concurrent submit+get, no 404s
// =====================================================================

func TestTaskStateConsistency_ConcurrentSubmitGet(t *testing.T) {
	srv, _ := newTestServer(t)
	defer srv.Shutdown(context.Background())

	const concurrent = 100
	var wg sync.WaitGroup
	errors := make(chan string, concurrent)

	for i := 0; i < concurrent; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()

			// Submit a task. Use unique IPs to avoid rate limiting.
			body := fmt.Sprintf(`{"payload":{"n":%d}}`, n)
			req := httptest.NewRequest(http.MethodPost, "/v1/pipelines/test-pipeline/tasks", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.RemoteAddr = fmt.Sprintf("10.%d.%d.%d:12345", n/65536%256, n/256%256, n%256)
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, req)

			if w.Code != http.StatusAccepted {
				errors <- fmt.Sprintf("submit %d: expected 202, got %d", n, w.Code)
				return
			}

			var resp submitTaskResponse
			json.Unmarshal(w.Body.Bytes(), &resp)

			// Immediately GET the task — must NOT be 404.
			getReq := httptest.NewRequest(http.MethodGet, "/v1/tasks/"+resp.TaskID, nil)
			getReq.RemoteAddr = fmt.Sprintf("10.%d.%d.%d:12345", n/65536%256, n/256%256, n%256)
			getW := httptest.NewRecorder()
			srv.Handler().ServeHTTP(getW, getReq)

			if getW.Code == http.StatusNotFound {
				errors <- fmt.Sprintf("task %s: 404 immediately after submission (race condition)", resp.TaskID)
				return
			}
			if getW.Code != http.StatusOK {
				errors <- fmt.Sprintf("task %s: expected 200, got %d", resp.TaskID, getW.Code)
				return
			}

			var task broker.Task
			json.Unmarshal(getW.Body.Bytes(), &task)
			if task.State != broker.TaskStatePending &&
				task.State != broker.TaskStateRouting &&
				task.State != broker.TaskStateExecuting {
				// Task must be in PENDING or a later state.
				errors <- fmt.Sprintf("task %s: unexpected state %s", resp.TaskID, task.State)
			}
		}(i)
	}

	wg.Wait()
	close(errors)

	for err := range errors {
		t.Error(err)
	}
}

// =====================================================================
// 3. Pagination Correctness
// =====================================================================

func TestPagination_Correctness(t *testing.T) {
	srv, _ := newTestServer(t)
	defer srv.Shutdown(context.Background())

	// Submit 25 tasks.
	for i := 0; i < 25; i++ {
		w := submitTask(t, srv.Handler(), "test-pipeline", fmt.Sprintf(`{"n":%d}`, i))
		if w.Code != http.StatusAccepted {
			t.Fatalf("submit %d: expected 202, got %d", i, w.Code)
		}
	}

	type pageTest struct {
		limit       int
		offset      int
		wantCount   int
		wantTotal   int
		description string
	}

	tests := []pageTest{
		{10, 0, 10, 25, "first page"},
		{10, 10, 10, 25, "second page"},
		{10, 20, 5, 25, "third page (partial)"},
		{10, 30, 0, 25, "past end (empty)"},
	}

	for _, tc := range tests {
		t.Run(tc.description, func(t *testing.T) {
			url := fmt.Sprintf("/v1/tasks?pipeline_id=test-pipeline&limit=%d&offset=%d", tc.limit, tc.offset)
			req := httptest.NewRequest(http.MethodGet, url, nil)
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, req)

			if w.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
			}

			var resp listTasksResponse
			json.Unmarshal(w.Body.Bytes(), &resp)

			if len(resp.Tasks) != tc.wantCount {
				t.Errorf("tasks count: got %d, want %d", len(resp.Tasks), tc.wantCount)
			}
			if resp.Total != tc.wantTotal {
				t.Errorf("total: got %d, want %d (must be unfiltered total, not page count)", resp.Total, tc.wantTotal)
			}
		})
	}

	// Verify all pages together contain 25 unique tasks.
	allIDs := make(map[string]bool)
	for offset := 0; offset < 30; offset += 10 {
		url := fmt.Sprintf("/v1/tasks?pipeline_id=test-pipeline&limit=10&offset=%d", offset)
		req := httptest.NewRequest(http.MethodGet, url, nil)
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)

		var resp listTasksResponse
		json.Unmarshal(w.Body.Bytes(), &resp)
		for _, task := range resp.Tasks {
			if allIDs[task.ID] {
				t.Errorf("task %s appears in multiple pages (offset=%d)", task.ID, offset)
			}
			allIDs[task.ID] = true
		}
	}
	if len(allIDs) != 25 {
		t.Errorf("total unique tasks across pages: got %d, want 25", len(allIDs))
	}
}

// =====================================================================
// 4. Health Endpoint Under Partial Failure
// =====================================================================

func TestHealth_PartialFailure_TimedOutAgent(t *testing.T) {
	cfg := testConfigMultiPipeline()
	st := memory.New()
	agents := map[string]broker.Agent{
		"agent-1": &stubAgent{id: "agent-1", provider: "anthropic", healthy: true},
		"agent-2": &stubAgent{id: "agent-2", provider: "openai", healthy: true},
		// agent-3 blocks for 30s — will time out under the 5s health check deadline.
		"agent-3": &slowAgent{id: "agent-3", provider: "google", delay: 30 * time.Second},
	}
	reg, _ := contract.NewRegistry(nil, "")
	logger := slog.Default()
	b := broker.New(cfg, st, agents, reg, logger, nil, nil)
	srv := NewServer(b, logger, nil, "")
	defer srv.Shutdown(context.Background())

	start := time.Now()
	req := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	elapsed := time.Since(start)

	// 1. Response is 200 — the endpoint itself is up.
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// 2. Status is "degraded", not "ok".
	var resp healthResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Status != "degraded" {
		t.Fatalf("expected status degraded, got %s", resp.Status)
	}

	// 3. Response arrives within 6s (5s timeout + 1s buffer).
	if elapsed > 6*time.Second {
		t.Fatalf("health check took %v, expected <6s", elapsed)
	}

	// 4. Healthy agents report "ok".
	if resp.Agents["agent-1"] != "ok" {
		t.Errorf("agent-1: expected ok, got %s", resp.Agents["agent-1"])
	}
	if resp.Agents["agent-2"] != "ok" {
		t.Errorf("agent-2: expected ok, got %s", resp.Agents["agent-2"])
	}

	// 5. The timed-out agent's entry contains an error string, not null/empty.
	agent3Status := resp.Agents["agent-3"]
	if agent3Status == "" || agent3Status == "ok" {
		t.Fatalf("agent-3: expected error string, got %q", agent3Status)
	}
	if !strings.Contains(agent3Status, "error:") {
		t.Fatalf("agent-3: expected error prefix, got %q", agent3Status)
	}
}

// =====================================================================
// 5. X-Request-ID Propagation
// =====================================================================

func TestRequestID_Propagation_WithHeader(t *testing.T) {
	srv, _ := newTestServer(t)
	defer srv.Shutdown(context.Background())

	req := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	req.Header.Set("X-Request-ID", "my-trace-id-123")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	got := w.Header().Get("X-Request-ID")
	if got != "my-trace-id-123" {
		t.Fatalf("expected X-Request-ID my-trace-id-123, got %q", got)
	}
}

func TestRequestID_Propagation_Generated(t *testing.T) {
	srv, _ := newTestServer(t)
	defer srv.Shutdown(context.Background())

	req := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	// No X-Request-ID header set.
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	got := w.Header().Get("X-Request-ID")
	if got == "" {
		t.Fatal("expected non-empty generated X-Request-ID")
	}
	// Verify it looks like a UUID (36 chars with dashes).
	if len(got) != 36 {
		t.Fatalf("expected UUID-format X-Request-ID (36 chars), got %q (%d chars)", got, len(got))
	}
}

// =====================================================================
// 6. WebSocket Event Delivery Ordering
// =====================================================================

func TestWebSocket_EventTimestampOrdering(t *testing.T) {
	srv, b := newTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	defer srv.Shutdown(context.Background())

	// Connect WebSocket.
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/v1/stream"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	time.Sleep(50 * time.Millisecond)

	// Publish 5 events sequentially with incrementing timestamps.
	for i := 0; i < 5; i++ {
		b.EventBus().Publish(broker.TaskEvent{
			Event:      "state_change",
			TaskID:     fmt.Sprintf("task-%d", i),
			PipelineID: "test-pipeline",
			StageID:    "intake",
			From:       broker.TaskStatePending,
			To:         broker.TaskStateRouting,
			Timestamp:  time.Now(),
		})
		time.Sleep(time.Millisecond) // Ensure distinct timestamps.
	}

	// Read all 5 events.
	var events []broker.TaskEvent
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	for i := 0; i < 5; i++ {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read event %d: %v", i, err)
		}
		var ev broker.TaskEvent
		json.Unmarshal(msg, &ev)
		events = append(events, ev)
	}

	// Verify non-decreasing timestamp order.
	for i := 1; i < len(events); i++ {
		if events[i].Timestamp.Before(events[i-1].Timestamp) {
			t.Errorf("event %d timestamp %v is before event %d timestamp %v",
				i, events[i].Timestamp, i-1, events[i-1].Timestamp)
		}
	}
}

// =====================================================================
// 7. WebSocket Filter Correctness
// =====================================================================

func TestWebSocket_FilterByPipelineID_Strict(t *testing.T) {
	srv, b := newMultiPipelineTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	defer srv.Shutdown(context.Background())

	// Connect filtered to pipeline-a only.
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/v1/stream?pipeline_id=pipeline-a"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	time.Sleep(50 * time.Millisecond)

	// Publish event for pipeline-b — should NOT arrive.
	b.EventBus().Publish(broker.TaskEvent{
		Event:      "state_change",
		TaskID:     "task-b-1",
		PipelineID: "pipeline-b",
		StageID:    "intake-b",
		From:       broker.TaskStatePending,
		To:         broker.TaskStateRouting,
		Timestamp:  time.Now(),
	})

	// Publish event for pipeline-a — SHOULD arrive.
	b.EventBus().Publish(broker.TaskEvent{
		Event:      "state_change",
		TaskID:     "task-a-1",
		PipelineID: "pipeline-a",
		StageID:    "intake",
		From:       broker.TaskStatePending,
		To:         broker.TaskStateDone,
		Timestamp:  time.Now(),
	})

	// Read the first event — should be from pipeline-a.
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, msg, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	var ev broker.TaskEvent
	json.Unmarshal(msg, &ev)
	if ev.PipelineID != "pipeline-a" {
		t.Fatalf("expected pipeline-a event, got pipeline %s", ev.PipelineID)
	}
	if ev.TaskID != "task-a-1" {
		t.Fatalf("expected task-a-1, got %s", ev.TaskID)
	}

	// Try to read another message — should timeout (no pipeline-b events).
	conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	_, _, err = conn.ReadMessage()
	if err == nil {
		t.Fatal("expected timeout reading (no pipeline-b events should arrive), but got a message")
	}
}

// =====================================================================
// 8. Subscriber Cleanup on Disconnect
// =====================================================================

func TestWebSocket_SubscriberCleanupOnDisconnect(t *testing.T) {
	srv, b := newTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	defer srv.Shutdown(context.Background())

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/v1/stream"

	// Record baseline goroutine count.
	runtime.GC()
	time.Sleep(50 * time.Millisecond)
	baseline := runtime.NumGoroutine()

	// Connect 10 WebSocket clients.
	conns := make([]*websocket.Conn, 10)
	for i := 0; i < 10; i++ {
		conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
		if err != nil {
			t.Fatalf("dial %d: %v", i, err)
		}
		conns[i] = conn
	}

	time.Sleep(100 * time.Millisecond)

	// Disconnect all 10 abruptly (close underlying TCP without handshake).
	for _, conn := range conns {
		conn.UnderlyingConn().Close()
	}

	// Wait for goroutines to settle.
	time.Sleep(500 * time.Millisecond)

	// Publish an event — should not block on dead subscribers.
	done := make(chan struct{})
	go func() {
		b.EventBus().Publish(broker.TaskEvent{
			Event:      "state_change",
			TaskID:     "after-disconnect",
			PipelineID: "test-pipeline",
			StageID:    "intake",
			From:       broker.TaskStatePending,
			To:         broker.TaskStateDone,
			Timestamp:  time.Now(),
		})
		close(done)
	}()

	select {
	case <-done:
		// Good — publish did not block.
	case <-time.After(2 * time.Second):
		t.Fatal("EventBus.Publish blocked on dead subscriber channels")
	}

	// Verify goroutine count settles back near baseline.
	time.Sleep(500 * time.Millisecond)
	current := runtime.NumGoroutine()
	// Allow some slack for test infrastructure goroutines.
	if current > baseline+5 {
		t.Errorf("goroutine leak: baseline=%d, current=%d (delta=%d)", baseline, current, current-baseline)
	}
}

// =====================================================================
// 9. Graceful Shutdown During Active WebSocket
// =====================================================================

func TestGracefulShutdown_WebSocket_CloseFrame(t *testing.T) {
	srv, _ := newTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/v1/stream"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	time.Sleep(50 * time.Millisecond)

	// Track if we receive a close frame.
	closeReceived := make(chan struct{}, 1)
	conn.SetCloseHandler(func(code int, text string) error {
		closeReceived <- struct{}{}
		return nil
	})

	// Trigger graceful shutdown in a goroutine.
	shutdownDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		shutdownDone <- srv.Shutdown(ctx)
	}()

	// Read from the connection — should get a close frame or error.
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, _, readErr := conn.ReadMessage()

	// Either the close handler fired, or we got an error from reading
	// (both indicate the server closed the connection properly).
	if readErr == nil {
		t.Fatal("expected error or close after shutdown")
	}

	// Verify server shutdown completes within 5 seconds.
	select {
	case err := <-shutdownDone:
		if err != nil {
			t.Logf("shutdown error (acceptable): %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not shut down within 5 seconds")
	}
}

// =====================================================================
// 10. Token Bucket Correctness
// =====================================================================

func TestRateLimiter_TokenBucket_Correctness(t *testing.T) {
	srv, _ := newTestServer(t)
	defer srv.Shutdown(context.Background())

	ip := "10.0.0.99:12345"

	// Send 100 requests — all should succeed.
	for i := 0; i < 100; i++ {
		req := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
		req.RemoteAddr = ip
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		if w.Code == http.StatusTooManyRequests {
			t.Fatalf("rate limited too early at request %d", i+1)
		}
	}

	// 101st request should be 429.
	req := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	req.RemoteAddr = ip
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 on 101st request, got %d", w.Code)
	}

	// Verify Retry-After header is present.
	retryAfter := w.Header().Get("Retry-After")
	if retryAfter == "" {
		t.Fatal("expected Retry-After header on 429 response")
	}

	// Wait 1 second for token refill (100 tokens/sec rate).
	time.Sleep(1100 * time.Millisecond)

	// Should now be allowed again.
	req = httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	req.RemoteAddr = ip
	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 after bucket refill, got %d", w.Code)
	}
}

// =====================================================================
// 11. Rate Limit Per-IP Isolation
// =====================================================================

func TestRateLimiter_PerIPIsolation(t *testing.T) {
	srv, _ := newTestServer(t)
	defer srv.Shutdown(context.Background())

	// Exhaust the bucket for IP 10.0.0.1.
	for i := 0; i < 100; i++ {
		req := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
		req.RemoteAddr = "10.0.0.1:12345"
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
	}

	// Verify 10.0.0.1 is rate limited.
	req := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	req.RemoteAddr = "10.0.0.1:12345"
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("10.0.0.1 should be rate limited, got %d", w.Code)
	}

	// SEC-004: X-Forwarded-For is NOT trusted for rate limiting.
	// A spoofed X-Forwarded-For must NOT bypass the rate limit on the real IP.
	req = httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	req.RemoteAddr = "10.0.0.1:12345" // same RemoteAddr (still rate limited)
	req.Header.Set("X-Forwarded-For", "10.0.0.2")
	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("10.0.0.1 with spoofed X-Forwarded-For should still be rate limited, got %d", w.Code)
	}

	// Request from a genuinely different RemoteAddr should succeed.
	req = httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	req.RemoteAddr = "10.0.0.3:54321"
	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("10.0.0.3 should not be rate limited, got %d", w.Code)
	}
}
