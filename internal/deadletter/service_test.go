package deadletter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/brianbuquoi/overlord/internal/broker"
	"github.com/brianbuquoi/overlord/internal/config"
	"github.com/brianbuquoi/overlord/internal/contract"
	"github.com/brianbuquoi/overlord/internal/store/memory"
	mock "github.com/brianbuquoi/overlord/internal/testutil/storemock"
)

// --- Test helpers ---

type stubAgent struct {
	id       string
	provider string
}

func (a *stubAgent) ID() string       { return a.id }
func (a *stubAgent) Provider() string { return a.provider }
func (a *stubAgent) Execute(ctx context.Context, task *broker.Task) (*broker.TaskResult, error) {
	return &broker.TaskResult{TaskID: task.ID, Payload: json.RawMessage(`{"result":"ok"}`)}, nil
}
func (a *stubAgent) HealthCheck(ctx context.Context) error { return nil }

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
		},
	}
}

// buildServiceWithStore returns a Service wired to the given store plus the
// underlying broker (needed by tests that Submit directly).
func buildServiceWithStore(t *testing.T, st broker.Store, logger *slog.Logger) (*Service, *broker.Broker) {
	t.Helper()
	cfg := testConfig()
	agents := map[string]broker.Agent{
		"agent-1": &stubAgent{id: "agent-1", provider: "anthropic"},
	}
	reg, _ := contract.NewRegistry(nil, "")
	b := broker.New(cfg, st, agents, reg, logger, nil, nil)
	return New(st, b, logger), b
}

// seed creates n dead-lettered tasks in the mock's memory backend.
func seed(t *testing.T, b *broker.Broker, m *memory.MemoryStore, n int) []string {
	t.Helper()
	ctx := context.Background()
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		task, err := b.Submit(ctx, "test-pipeline", json.RawMessage(fmt.Sprintf(`{"input":"fail-%d"}`, i)))
		if err != nil {
			t.Fatal(err)
		}
		state := broker.TaskStateFailed
		dl := true
		if err := m.UpdateTask(ctx, task.ID, broker.TaskUpdate{State: &state, RoutedToDeadLetter: &dl}); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, task.ID)
	}
	return ids
}

// --- ReplayAll tests ---

func TestReplayAll_HappyPath(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil))
	mstore := mock.New()
	svc, b := buildServiceWithStore(t, mstore, logger)

	ids := seed(t, b, mstore.Memory(), 3)

	type call struct {
		taskID, newTaskID string
		err               error
	}
	var calls []call
	progress := func(taskID, newTaskID string, err error) {
		calls = append(calls, call{taskID, newTaskID, err})
	}

	result, err := svc.ReplayAll(context.Background(), "test-pipeline", 0, progress)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Processed != 3 {
		t.Errorf("processed: got %d want 3", result.Processed)
	}
	if result.Failed != 0 {
		t.Errorf("failed: got %d want 0", result.Failed)
	}
	if result.Truncated {
		t.Errorf("truncated: got true want false")
	}
	if len(calls) != 3 {
		t.Fatalf("progress calls: got %d want 3", len(calls))
	}
	for _, c := range calls {
		if c.err != nil {
			t.Errorf("unexpected progress err for %s: %v", c.taskID, c.err)
		}
		if c.newTaskID == "" {
			t.Errorf("expected non-empty newTaskID for successful replay of %s", c.taskID)
		}
		if c.newTaskID == c.taskID {
			t.Errorf("newTaskID should differ from original taskID %s", c.taskID)
		}
	}
	for _, id := range ids {
		tk, err := mstore.Memory().GetTask(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if tk.State != broker.TaskStateReplayed {
			t.Errorf("task %s: got state=%s want REPLAYED", id, tk.State)
		}
	}
}

func TestReplayAll_PartialFailure(t *testing.T) {
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	mstore := mock.New()
	svc, b := buildServiceWithStore(t, mstore, logger)

	ids := seed(t, b, mstore.Memory(), 5)
	mem := mstore.Memory()
	mstore.OnEnqueueTask = func(ctx context.Context, stageID string, task *broker.Task) error {
		payload := string(task.Payload)
		if strings.Contains(payload, `"fail-0"`) || strings.Contains(payload, `"fail-1"`) {
			return mock.ErrInjected
		}
		return mem.EnqueueTask(ctx, stageID, task)
	}

	successes := 0
	failures := 0
	progress := func(taskID, newTaskID string, err error) {
		if err != nil {
			failures++
			if newTaskID != "" {
				t.Errorf("failed callback for %s: newTaskID should be empty, got %q", taskID, newTaskID)
			}
			return
		}
		successes++
		if newTaskID == "" {
			t.Errorf("success callback for %s: newTaskID should be non-empty", taskID)
		}
	}

	result, err := svc.ReplayAll(context.Background(), "test-pipeline", 0, progress)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Processed != 3 {
		t.Errorf("processed: got %d want 3", result.Processed)
	}
	if result.Failed != 2 {
		t.Errorf("failed: got %d want 2", result.Failed)
	}
	if successes != 3 || failures != 2 {
		t.Errorf("progress split: got successes=%d failures=%d, want 3/2", successes, failures)
	}

	for i, id := range ids {
		tk, err := mem.GetTask(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if i < 2 {
			if tk.State != broker.TaskStateFailed || !tk.RoutedToDeadLetter {
				t.Errorf("task %d: expected rolled back to FAILED+DL=true, got state=%s dl=%v",
					i, tk.State, tk.RoutedToDeadLetter)
			}
		} else if tk.State != broker.TaskStateReplayed {
			t.Errorf("task %d: expected REPLAYED, got state=%s", i, tk.State)
		}
	}
}

func TestReplayAll_RollbackDoesNotInflateCount(t *testing.T) {
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	mstore := mock.New()
	svc, b := buildServiceWithStore(t, mstore, logger)

	ids := seed(t, b, mstore.Memory(), 3)
	failingID := ids[0]

	mem := mstore.Memory()
	mstore.OnEnqueueTask = func(ctx context.Context, stageID string, task *broker.Task) error {
		if strings.Contains(string(task.Payload), `"fail-0"`) {
			return mock.ErrInjected
		}
		return mem.EnqueueTask(ctx, stageID, task)
	}

	result, err := svc.ReplayAll(context.Background(), "test-pipeline", 0, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Processed != 2 {
		t.Errorf("processed: got %d want 2", result.Processed)
	}
	if result.Failed != 1 {
		t.Errorf("failed: got %d want 1 (distinct failing task)", result.Failed)
	}

	// The failing task should appear exactly once in Warn logs — the
	// failedIDs guard prevents retries after RollbackReplayClaim restores
	// it to the dead-letter filter.
	occurrences := strings.Count(logBuf.String(), `"task_id":"`+failingID+`"`)
	if occurrences != 1 {
		t.Errorf("failing task %s logged %d times, want 1 (logs: %s)", failingID, occurrences, logBuf.String())
	}
}

func TestReplayAll_Truncation(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil))
	mstore := mock.New()
	svc, b := buildServiceWithStore(t, mstore, logger)

	seed(t, b, mstore.Memory(), 5)

	result, err := svc.ReplayAll(context.Background(), "test-pipeline", 2, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Truncated {
		t.Errorf("truncated: got false want true")
	}
	if result.Processed > 2 {
		t.Errorf("processed: got %d, want <= 2", result.Processed)
	}
}

func TestReplayAll_DoubleFailure(t *testing.T) {
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	mstore := mock.New()
	svc, b := buildServiceWithStore(t, mstore, logger)

	ids := seed(t, b, mstore.Memory(), 1)
	mem := mstore.Memory()
	mstore.OnEnqueueTask = func(ctx context.Context, stageID string, task *broker.Task) error {
		if strings.Contains(string(task.Payload), `"fail-0"`) {
			return mock.ErrInjected
		}
		return mem.EnqueueTask(ctx, stageID, task)
	}
	mstore.OnRollbackReplayClaim = func(ctx context.Context, taskID string) error {
		return mock.ErrInjected
	}

	_, err := svc.ReplayAll(context.Background(), "test-pipeline", 0, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	logs := logBuf.String()
	if !strings.Contains(logs, `"msg":"replay-all rollback failed: task may be stranded in REPLAY_PENDING"`) {
		t.Errorf("expected rollback-failed Error log, got: %s", logs)
	}
	if !strings.Contains(logs, `"submit_error"`) || !strings.Contains(logs, `"rollback_error"`) {
		t.Errorf("expected submit_error and rollback_error in log, got: %s", logs)
	}
	if !strings.Contains(logs, `"task_id":"`+ids[0]+`"`) {
		t.Errorf("expected task_id %q in log, got: %s", ids[0], logs)
	}

	// Task should be stuck in REPLAY_PENDING since rollback failed.
	tk, err := mem.GetTask(context.Background(), ids[0])
	if err != nil {
		t.Fatal(err)
	}
	if tk.State != broker.TaskStateReplayPending {
		t.Errorf("state after double-failure: got %s want REPLAY_PENDING", tk.State)
	}
}

// --- DiscardAll tests ---

func TestDiscardAll_HappyPath(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil))
	mstore := mock.New()
	svc, b := buildServiceWithStore(t, mstore, logger)

	ids := seed(t, b, mstore.Memory(), 3)

	result, err := svc.DiscardAll(context.Background(), "test-pipeline", 0, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Processed != 3 {
		t.Errorf("processed: got %d want 3", result.Processed)
	}
	if result.Failed != 0 {
		t.Errorf("failed: got %d want 0", result.Failed)
	}

	for _, id := range ids {
		tk, err := mstore.Memory().GetTask(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if tk.State != broker.TaskStateDiscarded {
			t.Errorf("task %s: got state=%s want DISCARDED", id, tk.State)
		}
	}
}

func TestDiscardAll_PartialFailure(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil))
	mstore := mock.New()
	svc, b := buildServiceWithStore(t, mstore, logger)

	ids := seed(t, b, mstore.Memory(), 5)
	failSet := map[string]struct{}{ids[0]: {}, ids[1]: {}}
	mem := mstore.Memory()
	mstore.OnUpdateTask = func(ctx context.Context, taskID string, update broker.TaskUpdate) error {
		if update.State != nil && *update.State == broker.TaskStateDiscarded {
			if _, bad := failSet[taskID]; bad {
				return mock.ErrInjected
			}
		}
		return mem.UpdateTask(ctx, taskID, update)
	}

	result, err := svc.DiscardAll(context.Background(), "test-pipeline", 0, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Processed != 3 {
		t.Errorf("processed: got %d want 3", result.Processed)
	}
	if result.Failed != 2 {
		t.Errorf("failed: got %d want 2", result.Failed)
	}
}

func TestDiscardAll_Truncation(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil))
	mstore := mock.New()
	svc, b := buildServiceWithStore(t, mstore, logger)

	seed(t, b, mstore.Memory(), 5)

	result, err := svc.DiscardAll(context.Background(), "test-pipeline", 2, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Truncated {
		t.Errorf("truncated: got false want true")
	}
	if result.Processed > 2 {
		t.Errorf("processed: got %d, want <= 2", result.Processed)
	}
}

// --- Count tests ---

func TestCount_ReturnsAccurateTotal(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil))
	mstore := mock.New()
	svc, b := buildServiceWithStore(t, mstore, logger)

	seed(t, b, mstore.Memory(), 7)

	n, err := svc.Count(context.Background(), "test-pipeline")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 7 {
		t.Errorf("count: got %d want 7", n)
	}
}
