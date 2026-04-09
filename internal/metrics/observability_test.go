package metrics_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/orcastrator/orcastrator/internal/agent"
	"github.com/orcastrator/orcastrator/internal/api"
	"github.com/orcastrator/orcastrator/internal/broker"
	"github.com/orcastrator/orcastrator/internal/config"
	"github.com/orcastrator/orcastrator/internal/contract"
	"github.com/orcastrator/orcastrator/internal/metrics"
	"github.com/orcastrator/orcastrator/internal/store/memory"
	"github.com/orcastrator/orcastrator/internal/tracing"
)

// Verify mockAgent implements broker.Agent at compile time.
var _ broker.Agent = (*mockAgent)(nil)

type mockAgent struct {
	id       string
	provider string
	handler  func(ctx context.Context, task *broker.Task) (*broker.TaskResult, error)
}

func (m *mockAgent) ID() string                          { return m.id }
func (m *mockAgent) Provider() string                    { return m.provider }
func (m *mockAgent) HealthCheck(_ context.Context) error { return nil }
func (m *mockAgent) Execute(ctx context.Context, task *broker.Task) (*broker.TaskResult, error) {
	return m.handler(ctx, task)
}

func writeSchema(t *testing.T, dir, name string, schema map[string]any) string {
	t.Helper()
	data, _ := json.Marshal(schema)
	path := filepath.Join(dir, name)
	os.WriteFile(path, data, 0644)
	return path
}

func objSchema(prop, typ string) map[string]any {
	return map[string]any{
		"type":                 "object",
		"properties":           map[string]any{prop: map[string]any{"type": typ}},
		"required":             []string{prop},
		"additionalProperties": false,
	}
}

func buildTestEnv(t *testing.T) (*config.Config, *memory.MemoryStore, map[string]broker.Agent, *contract.Registry) {
	t.Helper()
	dir := t.TempDir()

	inPath := writeSchema(t, dir, "in.json", objSchema("request", "string"))
	outPath := writeSchema(t, dir, "out.json", objSchema("result", "string"))

	cfg := &config.Config{
		Version: "1",
		SchemaRegistry: []config.SchemaEntry{
			{Name: "in", Version: "v1", Path: inPath},
			{Name: "out", Version: "v1", Path: outPath},
		},
		Pipelines: []config.Pipeline{
			{
				Name:        "test-pipeline",
				Concurrency: 1,
				Store:       "memory",
				Stages: []config.Stage{
					{
						ID:           "stage1",
						Agent:        "agent1",
						InputSchema:  config.StageSchemaRef{Name: "in", Version: "v1"},
						OutputSchema: config.StageSchemaRef{Name: "out", Version: "v1"},
						Timeout:      config.Duration{Duration: 5 * time.Second},
						Retry:        config.RetryPolicy{MaxAttempts: 3, Backoff: "fixed", BaseDelay: config.Duration{Duration: 10 * time.Millisecond}},
						OnSuccess:    "done",
						OnFailure:    "dead-letter",
					},
				},
			},
		},
		Agents: []config.Agent{
			{ID: "agent1", Provider: "mock", Model: "test-model"},
		},
	}

	reg, err := contract.NewRegistry(cfg.SchemaRegistry, "/")
	if err != nil {
		t.Fatal(err)
	}
	st := memory.New()
	agents := map[string]broker.Agent{}
	return cfg, st, agents, reg
}

func waitForState(t *testing.T, st *memory.MemoryStore, taskID string, want broker.TaskState, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		task, err := st.GetTask(context.Background(), taskID)
		if err == nil && task.State == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	task, _ := st.GetTask(context.Background(), taskID)
	t.Fatalf("task did not reach state %s within %v; current: %s", want, timeout, task.State)
}

func TestMetricsEndpointReturns200(t *testing.T) {
	cfg, st, agents, reg := buildTestEnv(t)
	agents["agent1"] = &mockAgent{id: "agent1", provider: "mock", handler: func(_ context.Context, _ *broker.Task) (*broker.TaskResult, error) {
		return &broker.TaskResult{Payload: json.RawMessage(`{"result":"ok"}`)}, nil
	}}

	m := metrics.New()
	// Initialize a counter so the prometheus text output includes our metrics.
	m.TasksTotal.WithLabelValues("test-pipeline", "stage1", "DONE").Add(0)

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	b := broker.New(cfg, st, agents, reg, logger, m, nil)
	srv := api.NewServer(b, logger, m, "/metrics")

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/metrics returned %d, want 200", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	bodyStr := string(body)
	// Should contain valid Prometheus text format with our metric families.
	if !strings.Contains(bodyStr, "orcastrator_tasks_total") {
		t.Fatal("/metrics response does not contain orcastrator_tasks_total")
	}
}

func TestTaskCompletionIncrementsCounter(t *testing.T) {
	cfg, st, agents, reg := buildTestEnv(t)
	agents["agent1"] = &mockAgent{
		id: "agent1", provider: "mock",
		handler: func(_ context.Context, task *broker.Task) (*broker.TaskResult, error) {
			return &broker.TaskResult{
				TaskID:  task.ID,
				Payload: json.RawMessage(`{"result":"done"}`),
				Metadata: map[string]any{
					"input_tokens":  10,
					"output_tokens": 5,
				},
			}, nil
		},
	}

	m := metrics.New()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	b := broker.New(cfg, st, agents, reg, logger, m, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.Run(ctx)

	task, err := b.Submit(ctx, "test-pipeline", json.RawMessage(`{"request":"hello"}`))
	if err != nil {
		t.Fatal(err)
	}

	waitForState(t, st, task.ID, broker.TaskStateDone, 5*time.Second)

	count := gatherCounter(t, m, "orcastrator_tasks_total")
	if count < 1 {
		t.Fatalf("orcastrator_tasks_total should be >= 1, got %v", count)
	}
}

func TestRetryIncrementsRetryCounter(t *testing.T) {
	cfg, st, agents, reg := buildTestEnv(t)

	calls := 0
	agents["agent1"] = &mockAgent{
		id: "agent1", provider: "mock",
		handler: func(_ context.Context, task *broker.Task) (*broker.TaskResult, error) {
			calls++
			if calls <= 1 {
				return nil, &agent.AgentError{
					Err:       errRetry,
					AgentID:   "agent1",
					Prov:      "mock",
					Retryable: true,
				}
			}
			return &broker.TaskResult{
				TaskID:  task.ID,
				Payload: json.RawMessage(`{"result":"ok"}`),
			}, nil
		},
	}

	m := metrics.New()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	b := broker.New(cfg, st, agents, reg, logger, m, nil)
	b.SetSleepFunc(func(_ context.Context, _ time.Duration) {})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.Run(ctx)

	task, err := b.Submit(ctx, "test-pipeline", json.RawMessage(`{"request":"test"}`))
	if err != nil {
		t.Fatal(err)
	}

	waitForState(t, st, task.ID, broker.TaskStateDone, 5*time.Second)

	retries := gatherCounter(t, m, "orcastrator_task_retries_total")
	if retries < 1 {
		t.Fatalf("orcastrator_task_retries_total should be >= 1, got %v", retries)
	}
}

var errRetry = errorString("retryable error")

type errorString string

func (e errorString) Error() string { return string(e) }

func TestTraceIDInTaskMetadata(t *testing.T) {
	cfg, st, agents, reg := buildTestEnv(t)
	agents["agent1"] = &mockAgent{
		id: "agent1", provider: "mock",
		handler: func(_ context.Context, task *broker.Task) (*broker.TaskResult, error) {
			return &broker.TaskResult{
				TaskID:  task.ID,
				Payload: json.RawMessage(`{"result":"ok"}`),
			}, nil
		},
	}

	tr, err := tracing.New(context.Background(), tracing.Config{
		Enabled:  true,
		Exporter: "stdout",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Shutdown(context.Background())

	m := metrics.New()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	b := broker.New(cfg, st, agents, reg, logger, m, tr)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.Run(ctx)

	task, err := b.Submit(ctx, "test-pipeline", json.RawMessage(`{"request":"hello"}`))
	if err != nil {
		t.Fatal(err)
	}

	waitForState(t, st, task.ID, broker.TaskStateDone, 5*time.Second)

	final, err := st.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}

	traceID, ok := final.Metadata["trace_id"]
	if !ok {
		t.Fatal("trace_id not found in task metadata")
	}
	if traceID == "" || traceID == nil {
		t.Fatal("trace_id is empty")
	}

	spanID, ok := final.Metadata["span_id"]
	if !ok {
		t.Fatal("span_id not found in task metadata")
	}
	if spanID == "" || spanID == nil {
		t.Fatal("span_id is empty")
	}
}

func TestRegistryIsolation(t *testing.T) {
	m1 := metrics.New()
	m2 := metrics.New()

	m1.TasksTotal.WithLabelValues("p", "s", "DONE").Add(10)
	m2.TasksTotal.WithLabelValues("p", "s", "DONE").Add(3)

	v1 := gatherCounter(t, m1, "orcastrator_tasks_total")
	v2 := gatherCounter(t, m2, "orcastrator_tasks_total")

	if v1 != 10 {
		t.Fatalf("m1 expected 10, got %v", v1)
	}
	if v2 != 3 {
		t.Fatalf("m2 expected 3, got %v", v2)
	}
}
