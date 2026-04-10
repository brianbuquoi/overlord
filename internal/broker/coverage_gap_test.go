package broker_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/orcastrator/orcastrator/internal/agent"
	"github.com/orcastrator/orcastrator/internal/broker"
	"github.com/orcastrator/orcastrator/internal/config"
	"github.com/orcastrator/orcastrator/internal/contract"
	"github.com/orcastrator/orcastrator/internal/store/memory"
)

// --- Test 1: RequiredCount for majority with even agent counts ---

func TestRequiredCount_MajorityEvenAgents(t *testing.T) {
	// RequiredCount is a method on FanOutExecutor. We create a minimal executor
	// just to call it — no real agents or validator needed.
	executor := broker.NewFanOutExecutor(nil, nil, nil, nil)

	tests := []struct {
		total int
		want  int
	}{
		{total: 2, want: 2},  // 2/2 + 1 = 2
		{total: 4, want: 3},  // 4/2 + 1 = 3
		{total: 6, want: 4},  // 6/2 + 1 = 4
		{total: 1, want: 1},  // 1/2 + 1 = 1
		{total: 3, want: 2},  // 3/2 + 1 = 2
		{total: 5, want: 3},  // 5/2 + 1 = 3
		{total: 10, want: 6}, // 10/2 + 1 = 6
	}

	for _, tt := range tests {
		t.Run(fmt.Sprintf("total=%d", tt.total), func(t *testing.T) {
			got := executor.RequiredCount(config.RequirePolicyMajority, tt.total)
			if got != tt.want {
				t.Errorf("RequiredCount(Majority, %d) = %d, want %d", tt.total, got, tt.want)
			}
		})
	}

	// Also verify the other policies for completeness.
	t.Run("all_policy", func(t *testing.T) {
		got := executor.RequiredCount(config.RequirePolicyAll, 4)
		if got != 4 {
			t.Errorf("RequiredCount(All, 4) = %d, want 4", got)
		}
	})
	t.Run("any_policy", func(t *testing.T) {
		got := executor.RequiredCount(config.RequirePolicyAny, 4)
		if got != 1 {
			t.Errorf("RequiredCount(Any, 4) = %d, want 1", got)
		}
	})
}

// --- Test 2: MaxAttempts=0 means no retries ---

func TestMaxAttempts_Zero(t *testing.T) {
	dir := t.TempDir()

	objSchema := func(prop string, typ string) map[string]any {
		return map[string]any{
			"type": "object",
			"properties": map[string]any{
				prop: map[string]any{"type": typ},
			},
			"required":             []string{prop},
			"additionalProperties": false,
		}
	}

	s1in := writeSchema(t, dir, "s1_in.json", objSchema("request", "string"))
	s1out := writeSchema(t, dir, "s1_out.json", objSchema("category", "string"))

	var calls atomic.Int32

	cfg := &config.Config{
		Version: "1",
		SchemaRegistry: []config.SchemaEntry{
			{Name: "s1_in", Version: "v1", Path: s1in},
			{Name: "s1_out", Version: "v1", Path: s1out},
		},
		Pipelines: []config.Pipeline{
			{
				Name:        "zero-retry-pipeline",
				Concurrency: 1,
				Store:       "memory",
				Stages: []config.Stage{
					{
						ID:           "stage1",
						Agent:        "agent1",
						InputSchema:  config.StageSchemaRef{Name: "s1_in", Version: "v1"},
						OutputSchema: config.StageSchemaRef{Name: "s1_out", Version: "v1"},
						Timeout:      config.Duration{Duration: 5 * time.Second},
						Retry: config.RetryPolicy{
							MaxAttempts: 0,
							Backoff:     "fixed",
							BaseDelay:   config.Duration{Duration: 10 * time.Millisecond},
						},
						OnSuccess: config.StaticOnSuccess("done"),
						OnFailure: "dead-letter",
					},
				},
			},
		},
		Agents: []config.Agent{
			{ID: "agent1", Provider: "mock", SystemPrompt: "Stage 1 prompt"},
		},
	}

	reg, err := contract.NewRegistry(cfg.SchemaRegistry, "/")
	if err != nil {
		t.Fatal(err)
	}

	st := memory.New()
	agents := map[string]broker.Agent{
		"agent1": &mockAgent{
			id:       "agent1",
			provider: "mock",
			handler: func(_ context.Context, _ *broker.Task) (*broker.TaskResult, error) {
				calls.Add(1)
				return nil, &agent.AgentError{
					Err:       errors.New("always fails"),
					AgentID:   "agent1",
					Prov:      "mock",
					Retryable: true,
				}
			},
		},
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	b := broker.New(cfg, st, agents, reg, logger, nil, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go b.Run(ctx)

	task, err := b.Submit(ctx, "zero-retry-pipeline", json.RawMessage(`{"request":"no-retry"}`))
	if err != nil {
		t.Fatal(err)
	}

	final := waitForTaskState(t, st, task.ID, broker.TaskStateFailed, 5*time.Second)

	// With MaxAttempts=0: the task executes once (attempt 0). On failure,
	// attempts is incremented to 1, and 1 < 0 is false, so no retry.
	// The agent should be called exactly once.
	if c := calls.Load(); c != 1 {
		t.Errorf("expected exactly 1 agent call with MaxAttempts=0, got %d", c)
	}
	if final.State != broker.TaskStateFailed {
		t.Errorf("expected task state FAILED, got %s", final.State)
	}
}

// --- Test 3: Zero stages rejected by broker Submit ---

func TestBroker_ZeroStages(t *testing.T) {
	// The broker's Submit method checks len(p.Stages) == 0 and returns an error.
	// This test verifies that path directly.
	dir := t.TempDir()

	objSchema := func(prop string, typ string) map[string]any {
		return map[string]any{
			"type": "object",
			"properties": map[string]any{
				prop: map[string]any{"type": typ},
			},
			"required":             []string{prop},
			"additionalProperties": false,
		}
	}

	dummySchema := writeSchema(t, dir, "dummy.json", objSchema("x", "string"))

	cfg := &config.Config{
		Version: "1",
		SchemaRegistry: []config.SchemaEntry{
			{Name: "dummy", Version: "v1", Path: dummySchema},
		},
		Pipelines: []config.Pipeline{
			{
				Name:        "empty-pipeline",
				Concurrency: 1,
				Store:       "memory",
				Stages:      []config.Stage{}, // zero stages
			},
		},
		Agents: []config.Agent{},
	}

	reg, err := contract.NewRegistry(cfg.SchemaRegistry, "/")
	if err != nil {
		t.Fatal(err)
	}

	st := memory.New()
	agents := map[string]broker.Agent{}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	b := broker.New(cfg, st, agents, reg, logger, nil, nil)

	ctx := context.Background()

	_, err = b.Submit(ctx, "empty-pipeline", json.RawMessage(`{"x":"test"}`))
	if err == nil {
		t.Fatal("expected error when submitting to pipeline with zero stages")
	}
	if got := err.Error(); got == "" {
		t.Error("error message should not be empty")
	}

	// Also verify submitting to a non-existent pipeline returns an error.
	_, err = b.Submit(ctx, "nonexistent-pipeline", json.RawMessage(`{"x":"test"}`))
	if err == nil {
		t.Fatal("expected error when submitting to nonexistent pipeline")
	}
}

// --- Test 4: State transitions via event bus ---

func TestBroker_StateTransitions(t *testing.T) {
	cfg, st, agents, reg := buildTestEnv(t)

	// Happy path: all agents succeed.
	agents["agent1"].(*mockAgent).setHandler(func(_ context.Context, _ *broker.Task) (*broker.TaskResult, error) {
		return &broker.TaskResult{Payload: json.RawMessage(`{"category":"test"}`)}, nil
	})
	agents["agent2"].(*mockAgent).setHandler(func(_ context.Context, _ *broker.Task) (*broker.TaskResult, error) {
		return &broker.TaskResult{Payload: json.RawMessage(`{"result":"processed"}`)}, nil
	})
	agents["agent3"].(*mockAgent).setHandler(func(_ context.Context, _ *broker.Task) (*broker.TaskResult, error) {
		return &broker.TaskResult{Payload: json.RawMessage(`{"valid":true}`)}, nil
	})

	b := newBroker(cfg, st, agents, reg)

	// Subscribe to events before submitting.
	sub := b.EventBus().Subscribe(256)
	defer sub.Unsubscribe()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go b.Run(ctx)

	task, err := b.Submit(ctx, "test-pipeline", json.RawMessage(`{"request":"transitions"}`))
	if err != nil {
		t.Fatal(err)
	}

	// Wait for DONE state.
	waitForTaskState(t, st, task.ID, broker.TaskStateDone, 5*time.Second)

	// Collect all events for this task.
	// Drain the subscription channel with a short deadline.
	var events []broker.TaskEvent
	drainDeadline := time.After(500 * time.Millisecond)
drain:
	for {
		select {
		case evt, ok := <-sub.C:
			if !ok {
				break drain
			}
			if evt.TaskID == task.ID {
				events = append(events, evt)
			}
		case <-drainDeadline:
			break drain
		}
	}

	// We should see at least one state_change event ending in DONE.
	foundDone := false
	for _, evt := range events {
		if evt.To == broker.TaskStateDone {
			foundDone = true
		}
	}
	if !foundDone {
		t.Errorf("expected at least one event transitioning to DONE, got %d events: %+v", len(events), events)
	}

	// --- Failing task: agent returns non-retryable error ---
	t.Run("failing_task_transitions", func(t *testing.T) {
		// Reconfigure agent1 to fail with non-retryable error.
		agents["agent1"].(*mockAgent).setHandler(func(_ context.Context, _ *broker.Task) (*broker.TaskResult, error) {
			return nil, &agent.AgentError{
				Err:       errors.New("fatal error"),
				AgentID:   "agent1",
				Prov:      "mock",
				Retryable: false,
			}
		})

		sub2 := b.EventBus().Subscribe(256)
		defer sub2.Unsubscribe()

		failTask, err := b.Submit(ctx, "test-pipeline", json.RawMessage(`{"request":"will-fail"}`))
		if err != nil {
			t.Fatal(err)
		}

		waitForTaskState(t, st, failTask.ID, broker.TaskStateFailed, 5*time.Second)

		var failEvents []broker.TaskEvent
		drainDeadline := time.After(500 * time.Millisecond)
	drainFail:
		for {
			select {
			case evt, ok := <-sub2.C:
				if !ok {
					break drainFail
				}
				if evt.TaskID == failTask.ID {
					failEvents = append(failEvents, evt)
				}
			case <-drainDeadline:
				break drainFail
			}
		}

		foundFailed := false
		for _, evt := range failEvents {
			if evt.To == broker.TaskStateFailed {
				foundFailed = true
			}
		}
		if !foundFailed {
			t.Errorf("expected at least one event transitioning to FAILED, got %d events: %+v", len(failEvents), failEvents)
		}
	})
}

// --- Test 5: Fan-out all agents timeout simultaneously ---

func TestFanOut_AllTimeoutsSimultaneous(t *testing.T) {
	dir := t.TempDir()

	objSchema := func(prop string, typ string) map[string]any {
		return map[string]any{
			"type": "object",
			"properties": map[string]any{
				prop: map[string]any{"type": typ},
			},
			"required":             []string{prop},
			"additionalProperties": false,
		}
	}

	intakeIn := writeSchema(t, dir, "intake_in.json", objSchema("request", "string"))
	intakeOut := writeSchema(t, dir, "intake_out.json", objSchema("category", "string"))
	reviewOut := writeSchema(t, dir, "review_out.json", objSchema("score", "integer"))
	aggSchema := writeSchema(t, dir, "review_agg.json", map[string]any{
		"type": "object",
		"properties": map[string]any{
			"results":         map[string]any{"type": "array"},
			"succeeded_count": map[string]any{"type": "integer"},
			"failed_count":    map[string]any{"type": "integer"},
			"mode":            map[string]any{"type": "string"},
		},
		"required": []string{"results", "succeeded_count", "failed_count", "mode"},
	})

	cfg := &config.Config{
		Version: "1",
		SchemaRegistry: []config.SchemaEntry{
			{Name: "intake_in", Version: "v1", Path: intakeIn},
			{Name: "intake_out", Version: "v1", Path: intakeOut},
			{Name: "review_out", Version: "v1", Path: reviewOut},
			{Name: "review_agg", Version: "v1", Path: aggSchema},
		},
		Pipelines: []config.Pipeline{
			{
				Name:        "timeout-fanout-pipeline",
				Concurrency: 2,
				Store:       "memory",
				Stages: []config.Stage{
					{
						ID:           "intake",
						Agent:        "intake-agent",
						InputSchema:  config.StageSchemaRef{Name: "intake_in", Version: "v1"},
						OutputSchema: config.StageSchemaRef{Name: "intake_out", Version: "v1"},
						Timeout:      config.Duration{Duration: 5 * time.Second},
						Retry:        config.RetryPolicy{MaxAttempts: 1, Backoff: "fixed", BaseDelay: config.Duration{Duration: 10 * time.Millisecond}},
						OnSuccess:    config.StaticOnSuccess("review"),
						OnFailure:    "dead-letter",
					},
					{
						ID: "review",
						FanOut: &config.FanOutConfig{
							Agents: []config.FanOutAgent{
								{ID: "slow-a"},
								{ID: "slow-b"},
								{ID: "slow-c"},
							},
							Mode:    config.FanOutModeGather,
							Timeout: config.Duration{Duration: 50 * time.Millisecond}, // very short
							Require: config.RequirePolicyAll,
						},
						InputSchema:     config.StageSchemaRef{Name: "intake_out", Version: "v1"},
						OutputSchema:    config.StageSchemaRef{Name: "review_out", Version: "v1"},
						AggregateSchema: &config.StageSchemaRef{Name: "review_agg", Version: "v1"},
						Timeout:         config.Duration{Duration: 5 * time.Second},
						Retry:           config.RetryPolicy{MaxAttempts: 1, Backoff: "fixed", BaseDelay: config.Duration{Duration: 10 * time.Millisecond}},
						OnSuccess:       config.StaticOnSuccess("done"),
						OnFailure:       "dead-letter",
					},
				},
			},
		},
		Agents: []config.Agent{
			{ID: "intake-agent", Provider: "mock", SystemPrompt: "Intake prompt"},
			{ID: "slow-a", Provider: "mock", SystemPrompt: "Slow A prompt"},
			{ID: "slow-b", Provider: "mock", SystemPrompt: "Slow B prompt"},
			{ID: "slow-c", Provider: "mock", SystemPrompt: "Slow C prompt"},
		},
	}

	reg, err := contract.NewRegistry(cfg.SchemaRegistry, "/")
	if err != nil {
		t.Fatal(err)
	}

	st := memory.New()

	// Intake agent succeeds immediately.
	intakeAgent := &mockAgent{
		id:       "intake-agent",
		provider: "mock",
		handler: func(_ context.Context, _ *broker.Task) (*broker.TaskResult, error) {
			return &broker.TaskResult{Payload: json.RawMessage(`{"category":"test"}`)}, nil
		},
	}

	// All fan-out agents sleep longer than the 50ms timeout.
	slowHandler := func(ctx context.Context, _ *broker.Task) (*broker.TaskResult, error) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(5 * time.Second):
			return &broker.TaskResult{Payload: json.RawMessage(`{"score":1}`)}, nil
		}
	}

	agents := map[string]broker.Agent{
		"intake-agent": intakeAgent,
		"slow-a":       &mockAgent{id: "slow-a", provider: "mock", handler: slowHandler},
		"slow-b":       &mockAgent{id: "slow-b", provider: "mock", handler: slowHandler},
		"slow-c":       &mockAgent{id: "slow-c", provider: "mock", handler: slowHandler},
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	b := broker.New(cfg, st, agents, reg, logger, nil, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go b.Run(ctx)

	task, err := b.Submit(ctx, "timeout-fanout-pipeline", json.RawMessage(`{"request":"timeout-test"}`))
	if err != nil {
		t.Fatal(err)
	}

	// The fan-out should fail because all agents time out and require=all is not met.
	final := waitForTaskState(t, st, task.ID, broker.TaskStateFailed, 10*time.Second)
	if final.State != broker.TaskStateFailed {
		t.Errorf("expected task state FAILED after fan-out timeout, got %s", final.State)
	}
}

// --- Test 6: Worker pool exhaustion with concurrency=1 ---

func TestBroker_WorkerPoolExhaustion(t *testing.T) {
	dir := t.TempDir()

	objSchema := func(prop string, typ string) map[string]any {
		return map[string]any{
			"type": "object",
			"properties": map[string]any{
				prop: map[string]any{"type": typ},
			},
			"required":             []string{prop},
			"additionalProperties": false,
		}
	}

	s1in := writeSchema(t, dir, "s1_in.json", objSchema("request", "string"))
	s1out := writeSchema(t, dir, "s1_out.json", objSchema("result", "string"))

	cfg := &config.Config{
		Version: "1",
		SchemaRegistry: []config.SchemaEntry{
			{Name: "s1_in", Version: "v1", Path: s1in},
			{Name: "s1_out", Version: "v1", Path: s1out},
		},
		Pipelines: []config.Pipeline{
			{
				Name:        "queue-pipeline",
				Concurrency: 1, // only 1 worker
				Store:       "memory",
				Stages: []config.Stage{
					{
						ID:           "stage1",
						Agent:        "agent1",
						InputSchema:  config.StageSchemaRef{Name: "s1_in", Version: "v1"},
						OutputSchema: config.StageSchemaRef{Name: "s1_out", Version: "v1"},
						Timeout:      config.Duration{Duration: 5 * time.Second},
						Retry:        config.RetryPolicy{MaxAttempts: 1, Backoff: "fixed", BaseDelay: config.Duration{Duration: 10 * time.Millisecond}},
						OnSuccess:    config.StaticOnSuccess("done"),
						OnFailure:    "dead-letter",
					},
				},
			},
		},
		Agents: []config.Agent{
			{ID: "agent1", Provider: "mock", SystemPrompt: "Stage 1 prompt"},
		},
	}

	reg, err := contract.NewRegistry(cfg.SchemaRegistry, "/")
	if err != nil {
		t.Fatal(err)
	}

	st := memory.New()

	var completed atomic.Int32

	agents := map[string]broker.Agent{
		"agent1": &mockAgent{
			id:       "agent1",
			provider: "mock",
			handler: func(_ context.Context, _ *broker.Task) (*broker.TaskResult, error) {
				// Simulate a small amount of work so tasks don't complete instantly.
				time.Sleep(10 * time.Millisecond)
				completed.Add(1)
				return &broker.TaskResult{Payload: json.RawMessage(`{"result":"done"}`)}, nil
			},
		},
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	b := broker.New(cfg, st, agents, reg, logger, nil, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go b.Run(ctx)

	// Submit 5 tasks to a pipeline with concurrency=1.
	const taskCount = 5
	taskIDs := make([]string, taskCount)
	for i := 0; i < taskCount; i++ {
		task, err := b.Submit(ctx, "queue-pipeline", json.RawMessage(fmt.Sprintf(`{"request":"task-%d"}`, i)))
		if err != nil {
			t.Fatalf("failed to submit task %d: %v", i, err)
		}
		taskIDs[i] = task.ID
	}

	// Wait for all tasks to complete. With concurrency=1 they must be
	// processed sequentially, but all should eventually finish.
	for i, id := range taskIDs {
		waitForTaskState(t, st, id, broker.TaskStateDone, 15*time.Second)
		t.Logf("task %d (%s) completed", i, id)
	}

	if c := completed.Load(); c != taskCount {
		t.Errorf("expected %d completed tasks, got %d", taskCount, c)
	}
}
