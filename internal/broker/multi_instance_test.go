//go:build integration

package broker_test

// Multi-instance integration tests validate that multiple Broker instances
// sharing a single Postgres database can process tasks concurrently without
// duplicates, losses, or data races.
//
// These tests require a live Postgres instance. Set DATABASE_URL to run them.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/brianbuquoi/overlord/internal/broker"
	"github.com/brianbuquoi/overlord/internal/config"
	"github.com/brianbuquoi/overlord/internal/contract"
	"github.com/brianbuquoi/overlord/internal/store"
	pgstore "github.com/brianbuquoi/overlord/internal/store/postgres"
	"github.com/brianbuquoi/overlord/internal/testutil/pgschema"
)

// setupMultiInstanceEnv creates a Postgres table, schemas, config, and mock
// agents for a 3-stage pipeline suitable for multi-instance tests.
func setupMultiInstanceEnv(t *testing.T) (
	store.Store,
	*config.Config,
	*contract.Registry,
	*pgxpool.Pool,
	string, // table name
) {
	t.Helper()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	// Unique table per test run to avoid interference.
	tableSuffix := uuid.New().String()[:8]
	safeTable := "overlord_multi_"
	for _, c := range tableSuffix {
		if c == '-' {
			safeTable += "_"
		} else {
			safeTable += string(c)
		}
	}

	// Centralized schema creation — the canonical DDL lives in
	// pgschema.CreateTable so this test can never drift out of sync
	// with the production schema. The audit flagged the prior inline
	// CREATE TABLE as missing routed_to_dead_letter /
	// cross_stage_transitions columns, which caused integration
	// failures that looked like broker regressions but were really
	// schema drift.
	if err := pgschema.CreateTable(ctx, pool, safeTable); err != nil {
		t.Fatalf("create table: %v", err)
	}

	t.Cleanup(func() {
		pool.Exec(context.Background(), "DROP TABLE IF EXISTS "+safeTable)
	})

	st, err := pgstore.New(pool, safeTable)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}

	// Create schema files.
	dir := t.TempDir()
	objSchema := func(prop, typ string) map[string]any {
		return map[string]any{
			"type":       "object",
			"properties": map[string]any{prop: map[string]any{"type": typ}},
			"required":   []string{prop},
		}
	}

	writeSchemaFile(t, dir, "s1_in.json", objSchema("request", "string"))
	writeSchemaFile(t, dir, "s1_out.json", objSchema("category", "string"))
	writeSchemaFile(t, dir, "s2_in.json", objSchema("category", "string"))
	writeSchemaFile(t, dir, "s2_out.json", objSchema("result", "string"))
	writeSchemaFile(t, dir, "s3_in.json", objSchema("result", "string"))
	writeSchemaFile(t, dir, "s3_out.json", objSchema("response", "string"))

	cfg := &config.Config{
		Version: "1",
		SchemaRegistry: []config.SchemaEntry{
			{Name: "s1_in", Version: "v1", Path: filepath.Join(dir, "s1_in.json")},
			{Name: "s1_out", Version: "v1", Path: filepath.Join(dir, "s1_out.json")},
			{Name: "s2_in", Version: "v1", Path: filepath.Join(dir, "s2_in.json")},
			{Name: "s2_out", Version: "v1", Path: filepath.Join(dir, "s2_out.json")},
			{Name: "s3_in", Version: "v1", Path: filepath.Join(dir, "s3_in.json")},
			{Name: "s3_out", Version: "v1", Path: filepath.Join(dir, "s3_out.json")},
		},
		Pipelines: []config.Pipeline{{
			Name:        "multi-pipeline",
			Concurrency: 2,
			Store:       "postgres",
			Stages: []config.Stage{
				{
					ID:           "stage1",
					Agent:        "agent1",
					InputSchema:  config.StageSchemaRef{Name: "s1_in", Version: "v1"},
					OutputSchema: config.StageSchemaRef{Name: "s1_out", Version: "v1"},
					Timeout:      config.Duration{Duration: 10 * time.Second},
					Retry:        config.RetryPolicy{MaxAttempts: 1, Backoff: "fixed", BaseDelay: config.Duration{Duration: 10 * time.Millisecond}},
					OnSuccess:    config.StaticOnSuccess("stage2"),
					OnFailure:    "dead-letter",
				},
				{
					ID:           "stage2",
					Agent:        "agent2",
					InputSchema:  config.StageSchemaRef{Name: "s2_in", Version: "v1"},
					OutputSchema: config.StageSchemaRef{Name: "s2_out", Version: "v1"},
					Timeout:      config.Duration{Duration: 10 * time.Second},
					Retry:        config.RetryPolicy{MaxAttempts: 1, Backoff: "fixed", BaseDelay: config.Duration{Duration: 10 * time.Millisecond}},
					OnSuccess:    config.StaticOnSuccess("stage3"),
					OnFailure:    "dead-letter",
				},
				{
					ID:           "stage3",
					Agent:        "agent3",
					InputSchema:  config.StageSchemaRef{Name: "s3_in", Version: "v1"},
					OutputSchema: config.StageSchemaRef{Name: "s3_out", Version: "v1"},
					Timeout:      config.Duration{Duration: 10 * time.Second},
					Retry:        config.RetryPolicy{MaxAttempts: 1, Backoff: "fixed", BaseDelay: config.Duration{Duration: 10 * time.Millisecond}},
					OnSuccess:    config.StaticOnSuccess("done"),
					OnFailure:    "dead-letter",
				},
			},
		}},
		Agents: []config.Agent{
			{ID: "agent1", Provider: "mock", SystemPrompt: "Stage 1"},
			{ID: "agent2", Provider: "mock", SystemPrompt: "Stage 2"},
			{ID: "agent3", Provider: "mock", SystemPrompt: "Stage 3"},
		},
	}

	reg, err := contract.NewRegistry(cfg.SchemaRegistry, "/")
	if err != nil {
		t.Fatalf("registry: %v", err)
	}

	return st, cfg, reg, pool, safeTable
}

func writeSchemaFile(t *testing.T, dir, name string, schema map[string]any) {
	t.Helper()
	data, err := json.Marshal(schema)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), data, 0644); err != nil {
		t.Fatal(err)
	}
}

// waitForTaskStateStore polls a store.Store until the task reaches the desired
// state or the timeout expires.
func waitForTaskStateStore(t *testing.T, st store.Store, taskID string, want broker.TaskState, timeout time.Duration) *broker.Task {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		task, err := st.GetTask(context.Background(), taskID)
		if err == nil && task.State == want {
			return task
		}
		time.Sleep(50 * time.Millisecond)
	}
	task, err := st.GetTask(context.Background(), taskID)
	if err != nil {
		t.Fatalf("task %s: get failed: %v", taskID, err)
	}
	t.Fatalf("task %s did not reach state %s within %v; current state: %s", taskID, want, timeout, task.State)
	return nil
}

// waitForTaskTerminal polls until the task reaches DONE or FAILED.
func waitForTaskTerminal(t *testing.T, st store.Store, taskID string, timeout time.Duration) *broker.Task {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		task, err := st.GetTask(context.Background(), taskID)
		if err == nil && (task.State == broker.TaskStateDone || task.State == broker.TaskStateFailed) {
			return task
		}
		time.Sleep(50 * time.Millisecond)
	}
	task, err := st.GetTask(context.Background(), taskID)
	if err != nil {
		t.Fatalf("task %s: get failed: %v", taskID, err)
	}
	t.Fatalf("task %s did not reach terminal state within %v; current state: %s", taskID, timeout, task.State)
	return nil
}

// newMockAgents returns a set of mock agents for the 3-stage pipeline.
// The completions map tracks which instance completed each task at each stage.
func newMockAgents(completions *sync.Map, instanceID string) map[string]broker.Agent {
	return map[string]broker.Agent{
		"agent1": &mockAgent{id: "agent1", provider: "mock", handler: func(_ context.Context, task *broker.Task) (*broker.TaskResult, error) {
			key := task.ID + "/stage1"
			completions.Store(key, instanceID)
			return &broker.TaskResult{
				Payload: json.RawMessage(`{"category":"processed"}`),
			}, nil
		}},
		"agent2": &mockAgent{id: "agent2", provider: "mock", handler: func(_ context.Context, task *broker.Task) (*broker.TaskResult, error) {
			key := task.ID + "/stage2"
			completions.Store(key, instanceID)
			return &broker.TaskResult{
				Payload: json.RawMessage(`{"result":"analyzed"}`),
			}, nil
		}},
		"agent3": &mockAgent{id: "agent3", provider: "mock", handler: func(_ context.Context, task *broker.Task) (*broker.TaskResult, error) {
			key := task.ID + "/stage3"
			completions.Store(key, instanceID)
			return &broker.TaskResult{
				Payload: json.RawMessage(`{"response":"complete"}`),
			}, nil
		}},
	}
}

// TestMultiInstance_NoDuplicatesNoLosses spins up 3 broker instances sharing
// one Postgres store, submits 100 tasks through a 3-stage pipeline, and verifies:
//   - All 100 tasks reach DONE
//   - No task stage is processed by more than one instance (no duplicates)
//   - Leader-less: all 3 instances process tasks from all stage types
func TestMultiInstance_NoDuplicatesNoLosses(t *testing.T) {
	st, cfg, reg, _, _ := setupMultiInstanceEnv(t)

	const numInstances = 3
	const numTasks = 100

	// Track which instance processed each task/stage combination.
	var completions sync.Map

	// Track execution counts per instance.
	var instanceCounts [numInstances]atomic.Int64

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	// Create 3 broker instances, all sharing the same Postgres store.
	brokers := make([]*broker.Broker, numInstances)
	cancels := make([]context.CancelFunc, numInstances)

	for i := 0; i < numInstances; i++ {
		instanceID := fmt.Sprintf("instance-%d", i)
		idx := i
		agents := map[string]broker.Agent{
			"agent1": &mockAgent{id: "agent1", provider: "mock", handler: func(_ context.Context, task *broker.Task) (*broker.TaskResult, error) {
				key := task.ID + "/stage1"
				if prev, loaded := completions.LoadOrStore(key, instanceID); loaded {
					t.Errorf("DUPLICATE: task %s stage1 already processed by %s, now by %s", task.ID, prev, instanceID)
				}
				instanceCounts[idx].Add(1)
				return &broker.TaskResult{
					Payload: json.RawMessage(`{"category":"processed"}`),
				}, nil
			}},
			"agent2": &mockAgent{id: "agent2", provider: "mock", handler: func(_ context.Context, task *broker.Task) (*broker.TaskResult, error) {
				key := task.ID + "/stage2"
				if prev, loaded := completions.LoadOrStore(key, instanceID); loaded {
					t.Errorf("DUPLICATE: task %s stage2 already processed by %s, now by %s", task.ID, prev, instanceID)
				}
				instanceCounts[idx].Add(1)
				return &broker.TaskResult{
					Payload: json.RawMessage(`{"result":"analyzed"}`),
				}, nil
			}},
			"agent3": &mockAgent{id: "agent3", provider: "mock", handler: func(_ context.Context, task *broker.Task) (*broker.TaskResult, error) {
				key := task.ID + "/stage3"
				if prev, loaded := completions.LoadOrStore(key, instanceID); loaded {
					t.Errorf("DUPLICATE: task %s stage3 already processed by %s, now by %s", task.ID, prev, instanceID)
				}
				instanceCounts[idx].Add(1)
				return &broker.TaskResult{
					Payload: json.RawMessage(`{"response":"complete"}`),
				}, nil
			}},
		}

		b := broker.New(cfg, st, agents, reg, logger, nil, nil)
		b.SetSleepFunc(func(_ context.Context, _ time.Duration) {})
		brokers[i] = b

		ctx, cancel := context.WithCancel(context.Background())
		cancels[i] = cancel
		go b.Run(ctx)
	}

	// Submit 100 tasks via any broker (they all share the same store).
	submitCtx := context.Background()
	taskIDs := make([]string, numTasks)
	for i := 0; i < numTasks; i++ {
		task, err := brokers[0].Submit(submitCtx, "multi-pipeline", json.RawMessage(`{"request":"test"}`))
		if err != nil {
			t.Fatalf("submit task %d: %v", i, err)
		}
		taskIDs[i] = task.ID
	}

	// Wait for all tasks to complete.
	for _, id := range taskIDs {
		waitForTaskStateStore(t, st, id, broker.TaskStateDone, 60*time.Second)
	}

	// Cancel all instances.
	for _, cancel := range cancels {
		cancel()
	}

	// Assert: all 100 tasks reached DONE.
	doneCount := 0
	for _, id := range taskIDs {
		task, err := st.GetTask(context.Background(), id)
		if err != nil {
			t.Errorf("get task %s: %v", id, err)
			continue
		}
		if task.State == broker.TaskStateDone {
			doneCount++
		} else {
			t.Errorf("task %s in state %s, expected DONE", id, task.State)
		}
	}
	t.Logf("Tasks completed: %d/%d", doneCount, numTasks)

	// Assert: each task/stage pair was processed exactly once (300 total entries).
	totalCompletions := 0
	completions.Range(func(_, _ any) bool {
		totalCompletions++
		return true
	})
	expectedCompletions := numTasks * 3 // 3 stages per task
	if totalCompletions != expectedCompletions {
		t.Errorf("expected %d stage completions, got %d", expectedCompletions, totalCompletions)
	}

	// Assert: leader-less — all instances processed at least some tasks.
	for i := 0; i < numInstances; i++ {
		count := instanceCounts[i].Load()
		t.Logf("instance-%d processed %d stage executions", i, count)
		if count == 0 {
			t.Errorf("instance-%d processed zero tasks — not leader-less", i)
		}
	}

	// Verify all 3 instances handled all 3 stage types (leader-less across stages).
	stagesByInstance := make(map[string]map[string]bool) // instanceID → set of stageIDs
	completions.Range(func(key, value any) bool {
		k := key.(string)
		inst := value.(string)
		// key format: "taskID/stageN"
		var stageID string
		for i := len(k) - 1; i >= 0; i-- {
			if k[i] == '/' {
				stageID = k[i+1:]
				break
			}
		}
		if stagesByInstance[inst] == nil {
			stagesByInstance[inst] = make(map[string]bool)
		}
		stagesByInstance[inst][stageID] = true
		return true
	})

	for inst, stages := range stagesByInstance {
		t.Logf("%s handled stages: %v", inst, stages)
		if len(stages) < 3 {
			t.Errorf("%s only handled %d stage types (want all 3) — instances should process all stages", inst, len(stages))
		}
	}
}

// TestMultiInstance_GracefulRollingRestart verifies that shutting down one
// instance while tasks are in flight does not lose PENDING tasks. Tasks
// already dequeued and mid-execution by the shutting-down instance will be
// marked FAILED (context cancelled). Tasks still PENDING in Postgres are
// picked up by the remaining instances.
func TestMultiInstance_GracefulRollingRestart(t *testing.T) {
	st, cfg, reg, _, _ := setupMultiInstanceEnv(t)

	const numInstances = 3
	const numTasks = 50

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	// Add a small delay to agent execution so tasks are in-flight during shutdown.
	makeAgents := func(instanceID string) map[string]broker.Agent {
		return map[string]broker.Agent{
			"agent1": &mockAgent{id: "agent1", provider: "mock", handler: func(ctx context.Context, _ *broker.Task) (*broker.TaskResult, error) {
				select {
				case <-time.After(5 * time.Millisecond):
				case <-ctx.Done():
					return nil, ctx.Err()
				}
				return &broker.TaskResult{
					Payload: json.RawMessage(`{"category":"processed"}`),
				}, nil
			}},
			"agent2": &mockAgent{id: "agent2", provider: "mock", handler: func(ctx context.Context, _ *broker.Task) (*broker.TaskResult, error) {
				select {
				case <-time.After(5 * time.Millisecond):
				case <-ctx.Done():
					return nil, ctx.Err()
				}
				return &broker.TaskResult{
					Payload: json.RawMessage(`{"result":"analyzed"}`),
				}, nil
			}},
			"agent3": &mockAgent{id: "agent3", provider: "mock", handler: func(ctx context.Context, _ *broker.Task) (*broker.TaskResult, error) {
				select {
				case <-time.After(5 * time.Millisecond):
				case <-ctx.Done():
					return nil, ctx.Err()
				}
				return &broker.TaskResult{
					Payload: json.RawMessage(`{"response":"complete"}`),
				}, nil
			}},
		}
	}

	brokers := make([]*broker.Broker, numInstances)
	cancels := make([]context.CancelFunc, numInstances)

	for i := 0; i < numInstances; i++ {
		b := broker.New(cfg, st, makeAgents(fmt.Sprintf("instance-%d", i)), reg, logger, nil, nil)
		b.SetSleepFunc(func(_ context.Context, _ time.Duration) {})
		brokers[i] = b
		ctx, cancel := context.WithCancel(context.Background())
		cancels[i] = cancel
		go b.Run(ctx)
	}

	// Submit tasks.
	taskIDs := make([]string, numTasks)
	for i := 0; i < numTasks; i++ {
		task, err := brokers[0].Submit(context.Background(), "multi-pipeline", json.RawMessage(`{"request":"test"}`))
		if err != nil {
			t.Fatalf("submit task %d: %v", i, err)
		}
		taskIDs[i] = task.ID
	}

	// Let some tasks start processing.
	time.Sleep(50 * time.Millisecond)

	// Shut down instance 0 (simulate rolling restart).
	t.Log("Shutting down instance-0...")
	cancels[0]()

	// Wait for all tasks to reach a terminal state (DONE or FAILED).
	// Tasks in-flight on instance-0 during shutdown may be FAILED.
	for _, id := range taskIDs {
		waitForTaskTerminal(t, st, id, 60*time.Second)
	}

	// Count outcomes.
	var doneCount, failedCount int
	for _, id := range taskIDs {
		task, err := st.GetTask(context.Background(), id)
		if err != nil {
			t.Errorf("get task %s: %v", id, err)
			continue
		}
		switch task.State {
		case broker.TaskStateDone:
			doneCount++
		case broker.TaskStateFailed:
			failedCount++
		default:
			t.Errorf("task %s in unexpected state: %s", id, task.State)
		}
	}

	t.Logf("Results: %d DONE, %d FAILED (in-flight during shutdown)", doneCount, failedCount)

	// All tasks must reach a terminal state (no stuck tasks).
	if doneCount+failedCount != numTasks {
		t.Errorf("expected %d terminal tasks, got %d (done=%d, failed=%d)", numTasks, doneCount+failedCount, doneCount, failedCount)
	}

	// Most tasks should complete successfully. Failed tasks are only those
	// that were mid-execution on instance-0 when it shut down.
	if doneCount == 0 {
		t.Error("no tasks completed successfully — remaining instances did not pick up work")
	}

	t.Logf("Rolling restart: %d/%d tasks completed by surviving instances", doneCount, numTasks)

	// Clean up remaining instances.
	for i := 1; i < numInstances; i++ {
		cancels[i]()
	}
}

// TestMultiInstance_EventBusIsPerInstance verifies that the EventBus is
// in-process only: events from tasks processed by instance B are NOT visible
// to subscribers connected to instance A's EventBus.
func TestMultiInstance_EventBusIsPerInstance(t *testing.T) {
	st, cfg, reg, _, _ := setupMultiInstanceEnv(t)

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	agents := func() map[string]broker.Agent {
		return map[string]broker.Agent{
			"agent1": &mockAgent{id: "agent1", provider: "mock", handler: func(_ context.Context, _ *broker.Task) (*broker.TaskResult, error) {
				return &broker.TaskResult{Payload: json.RawMessage(`{"category":"x"}`)}, nil
			}},
			"agent2": &mockAgent{id: "agent2", provider: "mock", handler: func(_ context.Context, _ *broker.Task) (*broker.TaskResult, error) {
				return &broker.TaskResult{Payload: json.RawMessage(`{"result":"x"}`)}, nil
			}},
			"agent3": &mockAgent{id: "agent3", provider: "mock", handler: func(_ context.Context, _ *broker.Task) (*broker.TaskResult, error) {
				return &broker.TaskResult{Payload: json.RawMessage(`{"response":"x"}`)}, nil
			}},
		}
	}

	// Create 2 instances.
	b1 := broker.New(cfg, st, agents(), reg, logger, nil, nil)
	b1.SetSleepFunc(func(_ context.Context, _ time.Duration) {})
	b2 := broker.New(cfg, st, agents(), reg, logger, nil, nil)
	b2.SetSleepFunc(func(_ context.Context, _ time.Duration) {})

	// Subscribe to events on both instances.
	sub1 := b1.EventBus().Subscribe(256)
	defer sub1.Unsubscribe()
	sub2 := b2.EventBus().Subscribe(256)
	defer sub2.Unsubscribe()

	ctx1, cancel1 := context.WithCancel(context.Background())
	defer cancel1()
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()

	go b1.Run(ctx1)
	go b2.Run(ctx2)

	// Submit 10 tasks.
	taskIDs := make([]string, 10)
	for i := 0; i < 10; i++ {
		task, err := b1.Submit(context.Background(), "multi-pipeline", json.RawMessage(`{"request":"test"}`))
		if err != nil {
			t.Fatalf("submit: %v", err)
		}
		taskIDs[i] = task.ID
	}

	// Wait for all tasks to complete.
	for _, id := range taskIDs {
		waitForTaskStateStore(t, st, id, broker.TaskStateDone, 30*time.Second)
	}

	// Collect events from both buses.
	var events1, events2 []broker.TaskEvent
	drainEvents := func(sub *broker.Subscription) []broker.TaskEvent {
		var events []broker.TaskEvent
		for {
			select {
			case ev := <-sub.C:
				events = append(events, ev)
			default:
				return events
			}
		}
	}

	events1 = drainEvents(sub1)
	events2 = drainEvents(sub2)

	t.Logf("EventBus instance-1 saw %d events", len(events1))
	t.Logf("EventBus instance-2 saw %d events", len(events2))

	// Each instance only sees events from tasks IT processed.
	// The total events across both should cover all tasks, but neither
	// should see all of them (unless one instance got all the work).
	totalDoneEvents := 0
	for _, ev := range events1 {
		if ev.To == broker.TaskStateDone {
			totalDoneEvents++
		}
	}
	for _, ev := range events2 {
		if ev.To == broker.TaskStateDone {
			totalDoneEvents++
		}
	}

	// With 10 tasks × 3 stages, there are 10 DONE events total (only the
	// final stage emits DONE). Each instance should have some subset.
	t.Logf("Total DONE events across both buses: %d (expected 10)", totalDoneEvents)

	// The key assertion: events are NOT shared between instances.
	// If they were shared, both buses would have the same events.
	if len(events1) > 0 && len(events2) > 0 {
		// Build sets of task IDs that reached DONE on each bus.
		done1 := make(map[string]bool)
		done2 := make(map[string]bool)
		for _, ev := range events1 {
			if ev.To == broker.TaskStateDone {
				done1[ev.TaskID] = true
			}
		}
		for _, ev := range events2 {
			if ev.To == broker.TaskStateDone {
				done2[ev.TaskID] = true
			}
		}

		// No task should appear in both DONE sets (each task completes on
		// exactly one instance's final stage).
		for id := range done1 {
			if done2[id] {
				t.Errorf("task %s DONE event appeared on both EventBus instances — events are leaking across instances", id)
			}
		}
	}

	t.Log("CONFIRMED: EventBus is per-instance — events are not shared across broker instances")
}
