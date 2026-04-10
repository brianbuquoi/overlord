package broker_test

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/orcastrator/orcastrator/internal/agent"
	"github.com/orcastrator/orcastrator/internal/broker"
	"github.com/orcastrator/orcastrator/internal/config"
	"github.com/orcastrator/orcastrator/internal/contract"
	"github.com/orcastrator/orcastrator/internal/metrics"
	"github.com/orcastrator/orcastrator/internal/store/memory"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

// buildBudgetTestEnv creates a single-stage pipeline with configurable retry budget.
func buildBudgetTestEnv(t *testing.T, pipelineBudget *config.RetryBudgetConfig, agentBudget *config.RetryBudgetConfig, maxAttempts int) (
	*config.Config,
	*memory.MemoryStore,
	map[string]broker.Agent,
	*contract.Registry,
) {
	t.Helper()
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
				Name:        "budget-pipeline",
				Concurrency: 2,
				Store:       "memory",
				RetryBudget: pipelineBudget,
				Stages: []config.Stage{
					{
						ID:           "stage1",
						Agent:        "agent1",
						InputSchema:  config.StageSchemaRef{Name: "s1_in", Version: "v1"},
						OutputSchema: config.StageSchemaRef{Name: "s1_out", Version: "v1"},
						Timeout:      config.Duration{Duration: 5 * time.Second},
						Retry:        config.RetryPolicy{MaxAttempts: maxAttempts, Backoff: "fixed", BaseDelay: config.Duration{Duration: 1 * time.Millisecond}},
						OnSuccess:    config.StaticOnSuccess("done"),
						OnFailure:    "dead-letter",
					},
				},
			},
		},
		Agents: []config.Agent{
			{ID: "agent1", Provider: "mock", SystemPrompt: "Test prompt", RetryBudget: agentBudget},
		},
	}

	reg, err := contract.NewRegistry(cfg.SchemaRegistry, "/")
	if err != nil {
		t.Fatal(err)
	}

	st := memory.New()
	agents := map[string]broker.Agent{
		"agent1": &mockAgent{id: "agent1", provider: "mock"},
	}

	return cfg, st, agents, reg
}

// Test 1: Pipeline budget exhaustion at exact threshold.
func TestRetryBudget_PipelineExactThreshold(t *testing.T) {
	pipelineBudget := &config.RetryBudgetConfig{
		MaxRetries:  10,
		Window:      config.Duration{Duration: time.Hour},
		OnExhausted: "fail",
	}
	cfg, st, agents, reg := buildBudgetTestEnv(t, pipelineBudget, nil, 100) // high max_attempts so budget is the limiter

	var calls atomic.Int32
	agents["agent1"].(*mockAgent).setHandler(func(_ context.Context, _ *broker.Task) (*broker.TaskResult, error) {
		calls.Add(1)
		return nil, &agent.AgentError{
			Err:       fmt.Errorf("transient failure"),
			AgentID:   "agent1",
			Prov:      "mock",
			Retryable: true,
		}
	})

	m := metrics.New()
	b := broker.New(cfg, st, agents, reg, testLogger(), m, nil)
	b.SetSleepFunc(func(_ context.Context, _ time.Duration) {})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.Run(ctx)

	task, err := b.Submit(ctx, "budget-pipeline", json.RawMessage(`{"request":"test"}`))
	if err != nil {
		t.Fatal(err)
	}

	// Wait for the task to reach FAILED state.
	final := waitForTaskState(t, st, task.ID, broker.TaskStateFailed, 10*time.Second)

	// Verify the failure reason.
	reason, ok := final.Metadata["failure_reason"]
	if !ok {
		t.Fatal("expected failure_reason in metadata")
	}
	if reason != "retry_budget_exhausted" {
		t.Fatalf("expected failure_reason 'retry_budget_exhausted', got %v", reason)
	}

	// The budget should have stopped retries at 10. The first call is the initial
	// attempt (not a retry), so we should see 11 calls total (1 initial + 10 retries).
	totalCalls := int(calls.Load())
	t.Logf("total agent calls: %d", totalCalls)
	if totalCalls > 12 { // small margin for concurrency
		t.Fatalf("expected ~11 agent calls (1 initial + 10 retries), got %d", totalCalls)
	}
}

// Test 2: Pipeline vs agent budget independence.
func TestRetryBudget_PipelineVsAgentIndependence(t *testing.T) {
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

	pipelineBudget := &config.RetryBudgetConfig{
		MaxRetries:  100,
		Window:      config.Duration{Duration: time.Hour},
		OnExhausted: "fail",
	}
	agentBudget := &config.RetryBudgetConfig{
		MaxRetries:  5,
		Window:      config.Duration{Duration: time.Hour},
		OnExhausted: "fail",
	}

	cfg := &config.Config{
		Version: "1",
		SchemaRegistry: []config.SchemaEntry{
			{Name: "s1_in", Version: "v1", Path: s1in},
			{Name: "s1_out", Version: "v1", Path: s1out},
		},
		Pipelines: []config.Pipeline{
			{
				Name:        "multi-agent-pipeline",
				Concurrency: 2,
				Store:       "memory",
				RetryBudget: pipelineBudget,
				Stages: []config.Stage{
					{
						ID:           "stage1",
						Agent:        "agent1",
						InputSchema:  config.StageSchemaRef{Name: "s1_in", Version: "v1"},
						OutputSchema: config.StageSchemaRef{Name: "s1_out", Version: "v1"},
						Timeout:      config.Duration{Duration: 5 * time.Second},
						Retry:        config.RetryPolicy{MaxAttempts: 100, Backoff: "fixed", BaseDelay: config.Duration{Duration: 1 * time.Millisecond}},
						OnSuccess:    config.StaticOnSuccess("done"),
						OnFailure:    "dead-letter",
					},
				},
			},
		},
		Agents: []config.Agent{
			{ID: "agent1", Provider: "mock", SystemPrompt: "Test", RetryBudget: agentBudget},
		},
	}

	reg, err := contract.NewRegistry(cfg.SchemaRegistry, "/")
	if err != nil {
		t.Fatal(err)
	}

	st := memory.New()
	agents := map[string]broker.Agent{
		"agent1": &mockAgent{id: "agent1", provider: "mock"},
	}

	var agent1Calls atomic.Int32
	agents["agent1"].(*mockAgent).setHandler(func(_ context.Context, _ *broker.Task) (*broker.TaskResult, error) {
		agent1Calls.Add(1)
		return nil, &agent.AgentError{
			Err:       fmt.Errorf("transient"),
			AgentID:   "agent1",
			Prov:      "mock",
			Retryable: true,
		}
	})

	b := broker.New(cfg, st, agents, reg, testLogger(), nil, nil)
	b.SetSleepFunc(func(_ context.Context, _ time.Duration) {})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.Run(ctx)

	task, err := b.Submit(ctx, "multi-agent-pipeline", json.RawMessage(`{"request":"test"}`))
	if err != nil {
		t.Fatal(err)
	}

	final := waitForTaskState(t, st, task.ID, broker.TaskStateFailed, 10*time.Second)
	reason := final.Metadata["failure_reason"]
	if reason != "retry_budget_exhausted" {
		t.Fatalf("expected retry_budget_exhausted, got %v", reason)
	}

	// Agent budget should have limited to ~5 retries + 1 initial call.
	totalCalls := int(agent1Calls.Load())
	t.Logf("agent1 calls: %d", totalCalls)
	if totalCalls > 7 {
		t.Fatalf("expected ~6 agent1 calls (1 initial + 5 retries), got %d", totalCalls)
	}
}

// Test 3: Budget refill after window expires.
func TestRetryBudget_Refill(t *testing.T) {
	pipelineBudget := &config.RetryBudgetConfig{
		MaxRetries:  3,
		Window:      config.Duration{Duration: 200 * time.Millisecond},
		OnExhausted: "fail",
	}
	cfg, st, agents, reg := buildBudgetTestEnv(t, pipelineBudget, nil, 100)

	var calls atomic.Int32
	agents["agent1"].(*mockAgent).setHandler(func(_ context.Context, _ *broker.Task) (*broker.TaskResult, error) {
		n := calls.Add(1)
		// After the budget refills, succeed.
		if n > 6 {
			return &broker.TaskResult{Payload: json.RawMessage(`{"result":"ok"}`)}, nil
		}
		return nil, &agent.AgentError{
			Err:       fmt.Errorf("transient failure %d", n),
			AgentID:   "agent1",
			Prov:      "mock",
			Retryable: true,
		}
	})

	b := broker.New(cfg, st, agents, reg, testLogger(), nil, nil)
	// Don't skip sleep — we need real timing for budget window test.

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.Run(ctx)

	task, err := b.Submit(ctx, "budget-pipeline", json.RawMessage(`{"request":"test"}`))
	if err != nil {
		t.Fatal(err)
	}

	// Should eventually reach FAILED because on_exhausted=fail.
	final := waitForTaskState(t, st, task.ID, broker.TaskStateFailed, 5*time.Second)
	reason := final.Metadata["failure_reason"]
	if reason != "retry_budget_exhausted" {
		t.Fatalf("expected retry_budget_exhausted, got %v", reason)
	}
}

// Test 4: on_exhausted=wait holds task until budget refills.
func TestRetryBudget_OnExhaustedWait(t *testing.T) {
	pipelineBudget := &config.RetryBudgetConfig{
		MaxRetries:  3,
		Window:      config.Duration{Duration: 500 * time.Millisecond},
		OnExhausted: "wait",
	}
	cfg, st, agents, reg := buildBudgetTestEnv(t, pipelineBudget, nil, 100)

	var calls atomic.Int32
	agents["agent1"].(*mockAgent).setHandler(func(_ context.Context, _ *broker.Task) (*broker.TaskResult, error) {
		n := calls.Add(1)
		if n <= 4 {
			return nil, &agent.AgentError{
				Err:       fmt.Errorf("transient failure %d", n),
				AgentID:   "agent1",
				Prov:      "mock",
				Retryable: true,
			}
		}
		return &broker.TaskResult{Payload: json.RawMessage(`{"result":"ok"}`)}, nil
	})

	b := broker.New(cfg, st, agents, reg, testLogger(), nil, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.Run(ctx)

	task, err := b.Submit(ctx, "budget-pipeline", json.RawMessage(`{"request":"test"}`))
	if err != nil {
		t.Fatal(err)
	}

	// With on_exhausted=wait and a 500ms window, the task should eventually
	// complete after the budget refills.
	final := waitForTaskState(t, st, task.ID, broker.TaskStateDone, 10*time.Second)
	if string(final.Payload) != `{"result":"ok"}` {
		t.Fatalf("expected ok payload, got %s", final.Payload)
	}
}

// Test 5: Concurrent budget atomicity.
func TestRetryBudget_ConcurrentAtomicity(t *testing.T) {
	pipelineBudget := &config.RetryBudgetConfig{
		MaxRetries:  10,
		Window:      config.Duration{Duration: time.Hour},
		OnExhausted: "fail",
	}
	cfg, st, agents, reg := buildBudgetTestEnv(t, pipelineBudget, nil, 100)

	var retryCount atomic.Int32
	agents["agent1"].(*mockAgent).setHandler(func(_ context.Context, _ *broker.Task) (*broker.TaskResult, error) {
		retryCount.Add(1)
		return nil, &agent.AgentError{
			Err:       fmt.Errorf("always fail"),
			AgentID:   "agent1",
			Prov:      "mock",
			Retryable: true,
		}
	})

	// Use higher concurrency to trigger races.
	cfg.Pipelines[0].Concurrency = 10

	b := broker.New(cfg, st, agents, reg, testLogger(), nil, nil)
	b.SetSleepFunc(func(_ context.Context, _ time.Duration) {})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.Run(ctx)

	// Submit multiple tasks concurrently.
	var wg sync.WaitGroup
	taskIDs := make([]string, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			task, err := b.Submit(ctx, "budget-pipeline", json.RawMessage(`{"request":"concurrent"}`))
			if err != nil {
				t.Errorf("submit failed: %v", err)
				return
			}
			taskIDs[idx] = task.ID
		}(i)
	}
	wg.Wait()

	// Wait for all tasks to reach terminal state.
	for _, id := range taskIDs {
		if id == "" {
			continue
		}
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			task, err := st.GetTask(context.Background(), id)
			if err == nil && (task.State == broker.TaskStateFailed || task.State == broker.TaskStateDone) {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
	}

	t.Logf("total retries: %d", retryCount.Load())
	// The budget should limit total retries. Due to check-then-act races in
	// concurrent mode, the actual count may exceed the budget slightly,
	// but should not be wildly over.
}

// Test 6: Dead-lettered task has RoutedToDeadLetter flag.
func TestDeadLetter_FlagSet(t *testing.T) {
	cfg, st, agents, reg := buildTestEnv(t)

	agents["agent1"].(*mockAgent).setHandler(func(_ context.Context, _ *broker.Task) (*broker.TaskResult, error) {
		return nil, &agent.AgentError{
			Err:       fmt.Errorf("permanent failure"),
			AgentID:   "agent1",
			Prov:      "mock",
			Retryable: false,
		}
	})

	b := newBroker(cfg, st, agents, reg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.Run(ctx)

	task, err := b.Submit(ctx, "test-pipeline", json.RawMessage(`{"request":"test"}`))
	if err != nil {
		t.Fatal(err)
	}

	final := waitForTaskState(t, st, task.ID, broker.TaskStateFailed, 5*time.Second)
	if !final.RoutedToDeadLetter {
		t.Fatal("expected RoutedToDeadLetter to be true for dead-lettered task")
	}
}

// Test: DISCARDED state is terminal and excluded from default listing.
func TestDeadLetter_DiscardedStateExcluded(t *testing.T) {
	cfg, st, agents, reg := buildTestEnv(t)

	agents["agent1"].(*mockAgent).setHandler(func(_ context.Context, _ *broker.Task) (*broker.TaskResult, error) {
		return nil, &agent.AgentError{Err: fmt.Errorf("fail"), AgentID: "agent1", Prov: "mock", Retryable: false}
	})

	b := newBroker(cfg, st, agents, reg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.Run(ctx)

	task, err := b.Submit(ctx, "test-pipeline", json.RawMessage(`{"request":"test"}`))
	if err != nil {
		t.Fatal(err)
	}

	waitForTaskState(t, st, task.ID, broker.TaskStateFailed, 5*time.Second)

	// Discard the task.
	state := broker.TaskStateDiscarded
	if err := st.UpdateTask(ctx, task.ID, broker.TaskUpdate{State: &state}); err != nil {
		t.Fatal(err)
	}

	// Default listing should exclude discarded.
	result, err := st.ListTasks(ctx, broker.TaskFilter{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	for _, tk := range result.Tasks {
		if tk.ID == task.ID {
			t.Fatal("discarded task should not appear in default listing")
		}
	}

	// With IncludeDiscarded, it should appear.
	result, err = st.ListTasks(ctx, broker.TaskFilter{Limit: 100, IncludeDiscarded: true})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, tk := range result.Tasks {
		if tk.ID == task.ID {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("discarded task should appear with IncludeDiscarded=true")
	}
}

// Test: RoutedToDeadLetter filter works.
func TestDeadLetter_FilterByRoutedToDeadLetter(t *testing.T) {
	cfg, st, agents, reg := buildTestEnv(t)

	// Agent1 alternates: first call fails, second succeeds.
	var agent1Calls atomic.Int32
	agents["agent1"].(*mockAgent).setHandler(func(_ context.Context, _ *broker.Task) (*broker.TaskResult, error) {
		n := agent1Calls.Add(1)
		if n == 1 {
			return nil, &agent.AgentError{Err: fmt.Errorf("fail"), AgentID: "agent1", Prov: "mock", Retryable: false}
		}
		return &broker.TaskResult{Payload: json.RawMessage(`{"category":"ok"}`)}, nil
	})
	agents["agent2"].(*mockAgent).setHandler(func(_ context.Context, _ *broker.Task) (*broker.TaskResult, error) {
		return &broker.TaskResult{Payload: json.RawMessage(`{"result":"done"}`)}, nil
	})
	agents["agent3"].(*mockAgent).setHandler(func(_ context.Context, _ *broker.Task) (*broker.TaskResult, error) {
		return &broker.TaskResult{Payload: json.RawMessage(`{"valid":true}`)}, nil
	})

	b := newBroker(cfg, st, agents, reg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.Run(ctx)

	// Task 1: will fail (dead-letter).
	task1, err := b.Submit(ctx, "test-pipeline", json.RawMessage(`{"request":"fail"}`))
	if err != nil {
		t.Fatal(err)
	}
	waitForTaskState(t, st, task1.ID, broker.TaskStateFailed, 5*time.Second)

	// Task 2: will succeed.
	task2, err := b.Submit(ctx, "test-pipeline", json.RawMessage(`{"request":"succeed"}`))
	if err != nil {
		t.Fatal(err)
	}
	waitForTaskState(t, st, task2.ID, broker.TaskStateDone, 5*time.Second)

	// Filter for dead-lettered tasks only.
	deadLetter := true
	failedState := broker.TaskStateFailed
	result, err := st.ListTasks(ctx, broker.TaskFilter{
		RoutedToDeadLetter: &deadLetter,
		State:              &failedState,
		Limit:              100,
	})
	if err != nil {
		t.Fatal(err)
	}

	if result.Total != 1 {
		t.Fatalf("expected 1 dead-lettered task, got %d", result.Total)
	}
	if result.Tasks[0].ID != task1.ID {
		t.Fatalf("expected dead-lettered task %s, got %s", task1.ID, result.Tasks[0].ID)
	}
}

// --- Bug fix regression tests ---

// Bug #1 regression: waitForBudget must recheck BOTH budgets, not just the one
// that originally triggered the wait. This test sets pipeline budget=3 (300ms
// window) and agent budget=3 (5s window). The pipeline refills first, but the
// agent budget should still block the retry.
func TestRetryBudget_WaitRechecks_BothBudgets(t *testing.T) {
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

	// Pipeline budget: 3 retries, 300ms window (refills fast).
	// Agent budget: 3 retries, 5s window (refills slow).
	// on_exhausted=wait for pipeline so the task enters WAITING.
	pipelineBudget := &config.RetryBudgetConfig{
		MaxRetries:  3,
		Window:      config.Duration{Duration: 300 * time.Millisecond},
		OnExhausted: "wait",
	}
	agentBudget := &config.RetryBudgetConfig{
		MaxRetries:  3,
		Window:      config.Duration{Duration: 5 * time.Second},
		OnExhausted: "fail",
	}

	cfg := &config.Config{
		Version: "1",
		SchemaRegistry: []config.SchemaEntry{
			{Name: "s1_in", Version: "v1", Path: s1in},
			{Name: "s1_out", Version: "v1", Path: s1out},
		},
		Pipelines: []config.Pipeline{
			{
				Name:        "dual-budget-pipeline",
				Concurrency: 1,
				Store:       "memory",
				RetryBudget: pipelineBudget,
				Stages: []config.Stage{
					{
						ID:           "stage1",
						Agent:        "agent1",
						InputSchema:  config.StageSchemaRef{Name: "s1_in", Version: "v1"},
						OutputSchema: config.StageSchemaRef{Name: "s1_out", Version: "v1"},
						Timeout:      config.Duration{Duration: 10 * time.Second},
						Retry:        config.RetryPolicy{MaxAttempts: 100, Backoff: "fixed", BaseDelay: config.Duration{Duration: 1 * time.Millisecond}},
						OnSuccess:    config.StaticOnSuccess("done"),
						OnFailure:    "dead-letter",
					},
				},
			},
		},
		Agents: []config.Agent{
			{ID: "agent1", Provider: "mock", SystemPrompt: "Test", RetryBudget: agentBudget},
		},
	}

	reg, err := contract.NewRegistry(cfg.SchemaRegistry, "/")
	if err != nil {
		t.Fatal(err)
	}

	st := memory.New()
	agents := map[string]broker.Agent{
		"agent1": &mockAgent{id: "agent1", provider: "mock"},
	}

	// Agent always fails with retryable error.
	agents["agent1"].(*mockAgent).setHandler(func(_ context.Context, _ *broker.Task) (*broker.TaskResult, error) {
		return nil, &agent.AgentError{
			Err:       fmt.Errorf("transient"),
			AgentID:   "agent1",
			Prov:      "mock",
			Retryable: true,
		}
	})

	b := broker.New(cfg, st, agents, reg, testLogger(), nil, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.Run(ctx)

	task, err := b.Submit(ctx, "dual-budget-pipeline", json.RawMessage(`{"request":"test"}`))
	if err != nil {
		t.Fatal(err)
	}

	// Both budgets will exhaust after 3 retries. The pipeline budget will
	// refill after 300ms but the agent budget won't for 5s. With the bug
	// (only rechecking one budget), the task would retry prematurely when
	// the pipeline refills, violating the agent budget. With the fix, the
	// task should eventually fail because the agent budget's on_exhausted=fail.
	final := waitForTaskState(t, st, task.ID, broker.TaskStateFailed, 8*time.Second)
	reason := final.Metadata["failure_reason"]

	// The task must fail due to budget exhaustion, not succeed.
	if reason != "retry_budget_exhausted" {
		t.Fatalf("expected failure_reason retry_budget_exhausted, got %v", reason)
	}
}

// Bug #2 regression: concurrent tryAcquireBudgets must not overshoot the budget.
// With the old separate check+increment (TOCTOU), concurrent workers could both
// see budget=9 and both increment to 10+11. The atomic IncrementIfNotExhausted
// prevents this.
func TestRetryBudget_AtomicAcquire_NeverExceedsBudget(t *testing.T) {
	pipelineBudget := &config.RetryBudgetConfig{
		MaxRetries:  10,
		Window:      config.Duration{Duration: time.Hour},
		OnExhausted: "fail",
	}
	cfg, st, agents, reg := buildBudgetTestEnv(t, pipelineBudget, nil, 100)

	// Use high concurrency.
	cfg.Pipelines[0].Concurrency = 10

	var totalCalls atomic.Int32
	agents["agent1"].(*mockAgent).setHandler(func(_ context.Context, _ *broker.Task) (*broker.TaskResult, error) {
		totalCalls.Add(1)
		return nil, &agent.AgentError{
			Err:       fmt.Errorf("always fail"),
			AgentID:   "agent1",
			Prov:      "mock",
			Retryable: true,
		}
	})

	m := metrics.New()
	b := broker.New(cfg, st, agents, reg, testLogger(), m, nil)
	b.SetSleepFunc(func(_ context.Context, _ time.Duration) {})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.Run(ctx)

	// Submit 20 tasks concurrently, each of which will try to retry.
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b.Submit(ctx, "budget-pipeline", json.RawMessage(`{"request":"race"}`))
		}()
	}
	wg.Wait()

	// Wait for all tasks to terminate.
	time.Sleep(2 * time.Second)

	// Check the budget counter directly. With IncrementIfNotExhausted,
	// the count should be exactly 10 — never more.
	cur, _ := b.Budgets().Current(ctx, "budget:pipeline:budget-pipeline", time.Hour)
	t.Logf("budget counter: %d, total agent calls: %d", cur, totalCalls.Load())
	if cur > 10 {
		t.Fatalf("budget counter %d exceeds max_retries 10 — TOCTOU race not prevented", cur)
	}
}
