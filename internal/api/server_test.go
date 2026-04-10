package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/orcastrator/orcastrator/internal/auth"
	"github.com/orcastrator/orcastrator/internal/broker"
	"github.com/orcastrator/orcastrator/internal/config"
	"github.com/orcastrator/orcastrator/internal/contract"
	"github.com/orcastrator/orcastrator/internal/metrics"
	"github.com/orcastrator/orcastrator/internal/store/memory"
)

// --- Test helpers ---

// stubAgent implements broker.Agent for testing.
type stubAgent struct {
	id        string
	provider  string
	healthy   bool
	healthErr string
}

func (a *stubAgent) ID() string       { return a.id }
func (a *stubAgent) Provider() string { return a.provider }
func (a *stubAgent) Execute(ctx context.Context, task *broker.Task) (*broker.TaskResult, error) {
	return &broker.TaskResult{
		TaskID:  task.ID,
		Payload: json.RawMessage(`{"result":"ok"}`),
	}, nil
}
func (a *stubAgent) HealthCheck(ctx context.Context) error {
	if !a.healthy {
		return fmt.Errorf("%s", a.healthErr)
	}
	return nil
}

func testConfig() *config.Config {
	return &config.Config{
		Version: "1",
		Pipelines: []config.Pipeline{
			{
				Name:        "test-pipeline",
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
		},
		Agents: []config.Agent{
			{ID: "agent-1", Provider: "anthropic", Model: "claude-opus-4-5"},
			{ID: "agent-2", Provider: "openai", Model: "gpt-4o"},
		},
	}
}

func newTestServer(t *testing.T) (*Server, *broker.Broker) {
	t.Helper()
	cfg := testConfig()
	st := memory.New()
	agents := map[string]broker.Agent{
		"agent-1": &stubAgent{id: "agent-1", provider: "anthropic", healthy: true},
		"agent-2": &stubAgent{id: "agent-2", provider: "openai", healthy: true},
	}
	reg, _ := contract.NewRegistry(nil, "")
	logger := slog.Default()
	b := broker.New(cfg, st, agents, reg, logger, nil, nil)
	srv := NewServer(b, logger, nil, "")
	return srv, b
}

// --- POST /v1/pipelines/{pipelineID}/tasks ---

func TestSubmitTask_Success(t *testing.T) {
	srv, _ := newTestServer(t)
	defer srv.Shutdown(context.Background())

	body := `{"payload":{"input":"hello"}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/pipelines/test-pipeline/tasks", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", w.Code, w.Body.String())
	}

	var resp submitTaskResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.TaskID == "" {
		t.Fatal("expected non-empty task_id")
	}
	if resp.State != "PENDING" {
		t.Fatalf("expected state PENDING, got %s", resp.State)
	}

	// Verify task exists in store.
	task, err := srv.broker.Store().GetTask(context.Background(), resp.TaskID)
	if err != nil {
		t.Fatalf("get task from store: %v", err)
	}
	if task.PipelineID != "test-pipeline" {
		t.Fatalf("expected pipeline_id test-pipeline, got %s", task.PipelineID)
	}
}

func TestSubmitTask_PipelineNotFound(t *testing.T) {
	srv, _ := newTestServer(t)
	defer srv.Shutdown(context.Background())

	body := `{"payload":{"input":"hello"}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/pipelines/nonexistent/tasks", strings.NewReader(body))
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
}

func TestSubmitTask_InvalidJSON(t *testing.T) {
	srv, _ := newTestServer(t)
	defer srv.Shutdown(context.Background())

	req := httptest.NewRequest(http.MethodPost, "/v1/pipelines/test-pipeline/tasks", strings.NewReader("not json"))
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestSubmitTask_MissingPayload(t *testing.T) {
	srv, _ := newTestServer(t)
	defer srv.Shutdown(context.Background())

	req := httptest.NewRequest(http.MethodPost, "/v1/pipelines/test-pipeline/tasks", strings.NewReader(`{}`))
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

// --- GET /v1/tasks/{taskID} ---

func TestGetTask_Success(t *testing.T) {
	srv, b := newTestServer(t)
	defer srv.Shutdown(context.Background())

	// Submit a task first.
	task, err := b.Submit(context.Background(), "test-pipeline", json.RawMessage(`{"x":1}`))
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/tasks/"+task.ID, nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var got broker.Task
	json.Unmarshal(w.Body.Bytes(), &got)
	if got.ID != task.ID {
		t.Fatalf("expected task ID %s, got %s", task.ID, got.ID)
	}
}

func TestGetTask_NotFound(t *testing.T) {
	srv, _ := newTestServer(t)
	defer srv.Shutdown(context.Background())

	req := httptest.NewRequest(http.MethodGet, "/v1/tasks/nonexistent-id", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

// --- GET /v1/tasks ---

func TestListTasks_Success(t *testing.T) {
	srv, b := newTestServer(t)
	defer srv.Shutdown(context.Background())

	// Submit two tasks.
	b.Submit(context.Background(), "test-pipeline", json.RawMessage(`{"x":1}`))
	b.Submit(context.Background(), "test-pipeline", json.RawMessage(`{"x":2}`))

	req := httptest.NewRequest(http.MethodGet, "/v1/tasks?pipeline_id=test-pipeline&limit=10", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp listTasksResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Tasks) != 2 {
		t.Fatalf("expected 2 tasks, got %d", len(resp.Tasks))
	}
}

func TestListTasks_FilterByState(t *testing.T) {
	srv, b := newTestServer(t)
	defer srv.Shutdown(context.Background())

	b.Submit(context.Background(), "test-pipeline", json.RawMessage(`{"x":1}`))

	req := httptest.NewRequest(http.MethodGet, "/v1/tasks?state=DONE", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var resp listTasksResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Tasks) != 0 {
		t.Fatalf("expected 0 tasks with DONE state, got %d", len(resp.Tasks))
	}
}

// --- GET /v1/pipelines ---

func TestListPipelines(t *testing.T) {
	srv, _ := newTestServer(t)
	defer srv.Shutdown(context.Background())

	req := httptest.NewRequest(http.MethodGet, "/v1/pipelines/", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var pipelines []pipelineSummary
	json.Unmarshal(w.Body.Bytes(), &pipelines)
	if len(pipelines) != 1 {
		t.Fatalf("expected 1 pipeline, got %d", len(pipelines))
	}
	if pipelines[0].Name != "test-pipeline" {
		t.Fatalf("expected pipeline name test-pipeline, got %s", pipelines[0].Name)
	}
	if len(pipelines[0].Stages) != 1 {
		t.Fatalf("expected 1 stage, got %d", len(pipelines[0].Stages))
	}
}

// --- GET /v1/health ---

func TestHealth_AllHealthy(t *testing.T) {
	srv, _ := newTestServer(t)
	defer srv.Shutdown(context.Background())

	req := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var resp healthResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Status != "ok" {
		t.Fatalf("expected status ok, got %s", resp.Status)
	}
}

func TestHealth_Degraded(t *testing.T) {
	cfg := testConfig()
	st := memory.New()
	agents := map[string]broker.Agent{
		"agent-1": &stubAgent{id: "agent-1", provider: "anthropic", healthy: true},
		"agent-2": &stubAgent{id: "agent-2", provider: "openai", healthy: false, healthErr: "connection refused"},
	}
	reg, _ := contract.NewRegistry(nil, "")
	logger := slog.Default()
	b := broker.New(cfg, st, agents, reg, logger, nil, nil)
	srv := NewServer(b, logger, nil, "")
	defer srv.Shutdown(context.Background())

	req := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	var resp healthResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Status != "degraded" {
		t.Fatalf("expected status degraded, got %s", resp.Status)
	}
	if resp.Agents["agent-1"] != "ok" {
		t.Fatalf("expected agent-1 ok, got %s", resp.Agents["agent-1"])
	}
	if !strings.HasPrefix(resp.Agents["agent-2"], "error:") {
		t.Fatalf("expected agent-2 error, got %s", resp.Agents["agent-2"])
	}
}

// --- X-Request-ID ---

func TestRequestID_Generated(t *testing.T) {
	srv, _ := newTestServer(t)
	defer srv.Shutdown(context.Background())

	req := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	id := w.Header().Get("X-Request-ID")
	if id == "" {
		t.Fatal("expected X-Request-ID header to be set")
	}
}

func TestRequestID_Echoed(t *testing.T) {
	srv, _ := newTestServer(t)
	defer srv.Shutdown(context.Background())

	req := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	req.Header.Set("X-Request-ID", "custom-id-123")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Header().Get("X-Request-ID") != "custom-id-123" {
		t.Fatalf("expected echoed request ID, got %s", w.Header().Get("X-Request-ID"))
	}
}

// --- JSON error format ---

func TestErrorResponse_Format(t *testing.T) {
	srv, _ := newTestServer(t)
	defer srv.Shutdown(context.Background())

	req := httptest.NewRequest(http.MethodGet, "/v1/tasks/nonexistent", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	var resp errorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("expected valid JSON error: %v", err)
	}
	if resp.Error == "" || resp.Code == "" {
		t.Fatalf("expected error and code fields, got %+v", resp)
	}
}

// --- Rate limiter ---

func TestRateLimiter_BlocksAfterBurst(t *testing.T) {
	srv, _ := newTestServer(t)
	defer srv.Shutdown(context.Background())

	// Exhaust the burst (100 requests).
	for i := 0; i < 100; i++ {
		req := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
		req.RemoteAddr = "10.0.0.1:12345"
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		if w.Code == http.StatusTooManyRequests {
			t.Fatalf("rate limited too early at request %d", i+1)
		}
	}

	// The 101st request should be rate limited.
	req := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	req.RemoteAddr = "10.0.0.1:12345"
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 on 101st request, got %d", w.Code)
	}

	// A different IP should still work.
	req = httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	req.RemoteAddr = "10.0.0.2:12345"
	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code == http.StatusTooManyRequests {
		t.Fatal("different IP should not be rate limited")
	}
}

// --- WebSocket ---

func TestWebSocket_ReceivesStateChangeEvent(t *testing.T) {
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

	// Give the hub a moment to register the client.
	time.Sleep(50 * time.Millisecond)

	// Submit a task — this triggers a PENDING state via Submit, and we can
	// also publish a synthetic event directly via the EventBus.
	b.EventBus().Publish(broker.TaskEvent{
		Event:      "state_change",
		TaskID:     "test-task-1",
		PipelineID: "test-pipeline",
		StageID:    "intake",
		From:       broker.TaskStatePending,
		To:         broker.TaskStateRouting,
		Timestamp:  time.Now(),
	})

	// Read the event from the WebSocket.
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, msg, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	var ev broker.TaskEvent
	if err := json.Unmarshal(msg, &ev); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if ev.Event != "state_change" {
		t.Fatalf("expected state_change event, got %s", ev.Event)
	}
	if ev.TaskID != "test-task-1" {
		t.Fatalf("expected task_id test-task-1, got %s", ev.TaskID)
	}
	if ev.From != broker.TaskStatePending || ev.To != broker.TaskStateRouting {
		t.Fatalf("unexpected state transition: %s → %s", ev.From, ev.To)
	}
}

func TestWebSocket_FilterByTaskID(t *testing.T) {
	srv, b := newTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	defer srv.Shutdown(context.Background())

	// Connect with task_id filter.
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/v1/stream?task_id=target-task"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	time.Sleep(50 * time.Millisecond)

	// Publish event for a different task — should be filtered.
	b.EventBus().Publish(broker.TaskEvent{
		Event:      "state_change",
		TaskID:     "other-task",
		PipelineID: "test-pipeline",
		StageID:    "intake",
		From:       broker.TaskStatePending,
		To:         broker.TaskStateRouting,
		Timestamp:  time.Now(),
	})

	// Publish event for the target task.
	b.EventBus().Publish(broker.TaskEvent{
		Event:      "state_change",
		TaskID:     "target-task",
		PipelineID: "test-pipeline",
		StageID:    "intake",
		From:       broker.TaskStateRouting,
		To:         broker.TaskStateExecuting,
		Timestamp:  time.Now(),
	})

	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, msg, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	var ev broker.TaskEvent
	json.Unmarshal(msg, &ev)
	if ev.TaskID != "target-task" {
		t.Fatalf("expected target-task, got %s", ev.TaskID)
	}
}

func TestWebSocket_FilterByPipelineID(t *testing.T) {
	srv, b := newTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	defer srv.Shutdown(context.Background())

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/v1/stream?pipeline_id=test-pipeline"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	time.Sleep(50 * time.Millisecond)

	// Publish event for a different pipeline.
	b.EventBus().Publish(broker.TaskEvent{
		Event:      "state_change",
		TaskID:     "t1",
		PipelineID: "other-pipeline",
		StageID:    "s1",
		From:       broker.TaskStatePending,
		To:         broker.TaskStateRouting,
		Timestamp:  time.Now(),
	})

	// Publish event for matching pipeline.
	b.EventBus().Publish(broker.TaskEvent{
		Event:      "state_change",
		TaskID:     "t2",
		PipelineID: "test-pipeline",
		StageID:    "intake",
		From:       broker.TaskStatePending,
		To:         broker.TaskStateDone,
		Timestamp:  time.Now(),
	})

	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, msg, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	var ev broker.TaskEvent
	json.Unmarshal(msg, &ev)
	if ev.PipelineID != "test-pipeline" {
		t.Fatalf("expected test-pipeline, got %s", ev.PipelineID)
	}
}

// --- Graceful shutdown ---

func TestGracefulShutdown_InFlightCompletes(t *testing.T) {
	srv, _ := newTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// Start a slow request.
	var wg sync.WaitGroup
	resultCh := make(chan int, 1)

	wg.Add(1)
	go func() {
		defer wg.Done()
		resp, err := http.Get(ts.URL + "/v1/health")
		if err != nil {
			resultCh <- 0
			return
		}
		defer resp.Body.Close()
		resultCh <- resp.StatusCode
	}()

	// Give the request time to start, then initiate shutdown.
	time.Sleep(10 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	wg.Wait()

	select {
	case code := <-resultCh:
		if code != http.StatusOK {
			t.Fatalf("expected 200 from in-flight request, got %d", code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for in-flight request")
	}
}

func TestGracefulShutdown_WebSocketCloses(t *testing.T) {
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

	// Initiate shutdown — should send close frame.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv.Shutdown(ctx)

	// Reading should now return an error or close message.
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, _, err = conn.ReadMessage()
	if err == nil {
		t.Fatal("expected error after shutdown, got nil")
	}
}

// --- Auth integration tests via full routes() ---

// newTestServerWithAuth creates a Server with auth enabled using the given keys.
func newTestServerWithAuth(t *testing.T, keys []auth.APIKey, m *metrics.Metrics) *Server {
	t.Helper()
	cfg := testConfig()
	st := memory.New()
	agents := map[string]broker.Agent{
		"agent-1": &stubAgent{id: "agent-1", provider: "anthropic", healthy: true},
		"agent-2": &stubAgent{id: "agent-2", provider: "openai", healthy: true},
	}
	reg, _ := contract.NewRegistry(nil, "")
	logger := slog.Default()
	b := broker.New(cfg, st, agents, reg, logger, nil, nil)
	srv := NewServer(b, logger, m, "", keys)
	return srv
}

func TestRoutes_HealthEndpointAuthEnforced(t *testing.T) {
	// Test 5: Verify the auth middleware is actually wired to the health
	// route in server.go, not just tested in middleware isolation.

	t.Setenv("HEALTH_READ_KEY", "health-read-key-value")
	keys, err := auth.LoadKeys([]config.AuthKeyConfig{
		{Name: "reader", KeyEnv: "HEALTH_READ_KEY", Scopes: []string{"read"}},
	})
	if err != nil {
		t.Fatalf("LoadKeys: %v", err)
	}

	srv := newTestServerWithAuth(t, keys, nil)
	defer srv.Shutdown(context.Background())

	handler := srv.Handler()

	// No Authorization header → 401.
	t.Run("no_auth", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/v1/health", nil)
		req.RemoteAddr = "127.0.0.1:12345"
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusUnauthorized {
			t.Errorf("expected 401 with no auth, got %d", rec.Code)
		}
	})

	// Read-scoped key → 200.
	t.Run("read_key", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/v1/health", nil)
		req.Header.Set("Authorization", "Bearer health-read-key-value")
		req.RemoteAddr = "127.0.0.1:12345"
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("expected 200 with read key, got %d", rec.Code)
		}
	})

	// Invalid key → 401.
	t.Run("invalid_key", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/v1/health", nil)
		req.Header.Set("Authorization", "Bearer totally-wrong-key")
		req.RemoteAddr = "127.0.0.1:12345"
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusUnauthorized {
			t.Errorf("expected 401 with invalid key, got %d", rec.Code)
		}
	})
}

func TestRoutes_MetricsAuthBypass(t *testing.T) {
	// Test 6: Verify /metrics bypasses auth while /v1/tasks enforces it.
	// Both assertions in the same test prove the bypass is selective
	// (metrics only) and not a global auth disable.

	t.Setenv("METRICS_READ_KEY", "metrics-read-key-value")
	keys, err := auth.LoadKeys([]config.AuthKeyConfig{
		{Name: "reader", KeyEnv: "METRICS_READ_KEY", Scopes: []string{"read"}},
	})
	if err != nil {
		t.Fatalf("LoadKeys: %v", err)
	}

	// Create server with metrics enabled so /metrics is registered.
	m := metrics.New()
	srv := newTestServerWithAuth(t, keys, m)
	defer srv.Shutdown(context.Background())

	handler := srv.Handler()

	// GET /metrics with no auth → 200 (auth bypassed for metrics).
	req := httptest.NewRequest("GET", "/metrics", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("GET /metrics without auth: expected 200 (auth bypassed), got %d", rec.Code)
	}

	// GET /v1/tasks with no auth → 401 (auth enforced for API routes).
	req = httptest.NewRequest("GET", "/v1/tasks", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("GET /v1/tasks without auth: expected 401, got %d", rec.Code)
	}
}

func TestAuthDisabled_AllRequestsPass_FullServer(t *testing.T) {
	// Test 10: Verify auth disabled works via the full routes() code path,
	// not just a bare handler. When auth.enabled = false (no authKeys passed),
	// all registered routes should return non-401 responses.

	srv, _ := newTestServer(t) // newTestServer creates without auth
	defer srv.Shutdown(context.Background())

	handler := srv.Handler()

	endpoints := []struct {
		method string
		path   string
	}{
		{"GET", "/v1/tasks"},
		{"GET", "/v1/health"},
		{"GET", "/v1/pipelines/"},
	}

	for _, ep := range endpoints {
		t.Run(ep.method+" "+ep.path, func(t *testing.T) {
			req := httptest.NewRequest(ep.method, ep.path, nil)
			req.RemoteAddr = "127.0.0.1:12345"
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code == http.StatusUnauthorized {
				t.Errorf("%s %s returned 401 with auth disabled — auth middleware should not be active", ep.method, ep.path)
			}
		})
	}
}
