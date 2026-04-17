package store_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/brianbuquoi/overlord/internal/broker"
	"github.com/brianbuquoi/overlord/internal/store"
)

// jsonEqual compares two JSON payloads semantically. Postgres JSONB
// reorders keys and normalizes whitespace on round-trip, so byte-for-
// byte equality against the originally-submitted bytes produces
// false negatives. All store conformance tests that compare payload
// identity after a round trip must use this helper instead of
// string() equality — the audit flagged raw JSON comparison as a
// source of flaky integration failures that looked like store bugs
// but were really JSON normalization.
func jsonEqual(a, b []byte) bool {
	var av, bv any
	if err := json.Unmarshal(a, &av); err != nil {
		return false
	}
	if err := json.Unmarshal(b, &bv); err != nil {
		return false
	}
	// reflect.DeepEqual handles nested maps and slices uniformly
	// after unmarshal into any.
	return deepEqualJSON(av, bv)
}

// deepEqualJSON walks parallel structures unmarshalled from JSON and
// returns true when they are semantically equal. It does not use
// reflect.DeepEqual directly because json.Number handling and
// interface nil-ness would need special casing that a small hand
// walk makes easier to read. Scalars are compared with ==, maps are
// compared key-by-key, and slices are compared in order.
func deepEqualJSON(a, b any) bool {
	switch av := a.(type) {
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for k, v := range av {
			w, ok := bv[k]
			if !ok || !deepEqualJSON(v, w) {
				return false
			}
		}
		return true
	case []any:
		bv, ok := b.([]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for i := range av {
			if !deepEqualJSON(av[i], bv[i]) {
				return false
			}
		}
		return true
	default:
		return a == b
	}
}

func newTask(pipelineID, stageID string) *broker.Task {
	return &broker.Task{
		ID:                  uuid.New().String(),
		PipelineID:          pipelineID,
		StageID:             stageID,
		InputSchemaName:     "test_input",
		InputSchemaVersion:  "v1",
		OutputSchemaName:    "test_output",
		OutputSchemaVersion: "v1",
		Payload:             json.RawMessage(`{"key":"value"}`),
		Metadata:            map[string]any{"source": "test"},
		State:               broker.TaskStatePending,
		Attempts:            0,
		MaxAttempts:         3,
		CreatedAt:           time.Now(),
		UpdatedAt:           time.Now(),
	}
}

// RunConformanceTests runs the standard store conformance suite.
// Any store.Store implementation must pass this suite.
func RunConformanceTests(t *testing.T, factory func() store.Store) {
	t.Helper()

	t.Run("EnqueueThenDequeueFIFO", func(t *testing.T) {
		t.Parallel()
		s := factory()
		ctx := context.Background()
		stage := "stage-fifo-" + uuid.New().String()

		tasks := make([]*broker.Task, 5)
		for i := range tasks {
			tasks[i] = newTask("pipe-1", stage)
			// Stagger CreatedAt to guarantee ordering.
			tasks[i].CreatedAt = time.Now().Add(time.Duration(i) * time.Millisecond)
			if err := s.EnqueueTask(ctx, stage, tasks[i]); err != nil {
				t.Fatalf("enqueue %d: %v", i, err)
			}
		}

		for i := range tasks {
			got, err := s.DequeueTask(ctx, stage)
			if err != nil {
				t.Fatalf("dequeue %d: %v", i, err)
			}
			if got.ID != tasks[i].ID {
				t.Errorf("dequeue %d: got ID %s, want %s", i, got.ID, tasks[i].ID)
			}
		}
	})

	t.Run("ConcurrentEnqueueSequentialDequeue", func(t *testing.T) {
		t.Parallel()
		s := factory()
		ctx := context.Background()
		stage := "stage-concurrent-" + uuid.New().String()

		const workers = 10
		const tasksPerWorker = 5

		var wg sync.WaitGroup
		wg.Add(workers)
		for w := 0; w < workers; w++ {
			go func() {
				defer wg.Done()
				for i := 0; i < tasksPerWorker; i++ {
					task := newTask("pipe-1", stage)
					if err := s.EnqueueTask(ctx, stage, task); err != nil {
						t.Errorf("concurrent enqueue: %v", err)
					}
				}
			}()
		}
		wg.Wait()

		seen := make(map[string]bool)
		for i := 0; i < workers*tasksPerWorker; i++ {
			got, err := s.DequeueTask(ctx, stage)
			if err != nil {
				t.Fatalf("dequeue %d: %v", i, err)
			}
			if seen[got.ID] {
				t.Errorf("task %s dequeued more than once", got.ID)
			}
			seen[got.ID] = true
		}

		if len(seen) != workers*tasksPerWorker {
			t.Errorf("dequeued %d tasks, want %d", len(seen), workers*tasksPerWorker)
		}
	})

	t.Run("UpdateTaskReflectedInGetTask", func(t *testing.T) {
		t.Parallel()
		s := factory()
		ctx := context.Background()
		stage := "stage-update-" + uuid.New().String()

		task := newTask("pipe-1", stage)
		if err := s.EnqueueTask(ctx, stage, task); err != nil {
			t.Fatal(err)
		}

		newState := broker.TaskStateExecuting
		newAttempts := 2
		err := s.UpdateTask(ctx, task.ID, broker.TaskUpdate{
			State:    &newState,
			Metadata: map[string]any{"extra": "data"},
			Attempts: &newAttempts,
		})
		if err != nil {
			t.Fatal(err)
		}

		got, err := s.GetTask(ctx, task.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.State != broker.TaskStateExecuting {
			t.Errorf("state: got %s, want EXECUTING", got.State)
		}
		if got.Attempts != 2 {
			t.Errorf("attempts: got %d, want 2", got.Attempts)
		}
		if got.Metadata["extra"] != "data" {
			t.Error("metadata missing extra key")
		}
		// Original metadata should be preserved (merge, not replace).
		if got.Metadata["source"] != "test" {
			t.Error("metadata lost original source key")
		}
	})

	t.Run("GetTaskUnknownIDReturnsErrTaskNotFound", func(t *testing.T) {
		t.Parallel()
		s := factory()
		ctx := context.Background()

		_, err := s.GetTask(ctx, "nonexistent-"+uuid.New().String())
		if err != store.ErrTaskNotFound {
			t.Errorf("got %v, want ErrTaskNotFound", err)
		}
	})

	t.Run("DequeueEmptyQueueReturnsErrQueueEmpty", func(t *testing.T) {
		t.Parallel()
		s := factory()
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()

		_, err := s.DequeueTask(ctx, "empty-stage-"+uuid.New().String())
		if err != store.ErrQueueEmpty && err != context.DeadlineExceeded {
			t.Errorf("got %v, want ErrQueueEmpty or context.DeadlineExceeded", err)
		}
	})

	t.Run("UpdateTaskUnknownIDReturnsErrTaskNotFound", func(t *testing.T) {
		t.Parallel()
		s := factory()
		ctx := context.Background()

		newState := broker.TaskStateExecuting
		err := s.UpdateTask(ctx, "nonexistent-"+uuid.New().String(), broker.TaskUpdate{
			State: &newState,
		})
		if err != store.ErrTaskNotFound {
			t.Errorf("got %v, want ErrTaskNotFound", err)
		}
	})

	t.Run("EnqueueDuplicateTaskID", func(t *testing.T) {
		t.Parallel()
		s := factory()
		ctx := context.Background()
		stage := "stage-dup-" + uuid.New().String()

		task := newTask("pipe-1", stage)
		if err := s.EnqueueTask(ctx, stage, task); err != nil {
			t.Fatal(err)
		}

		// Enqueue the same task ID again with different payload.
		dup := newTask("pipe-1", stage)
		dup.ID = task.ID
		dup.Payload = json.RawMessage(`{"key":"updated"}`)

		err := s.EnqueueTask(ctx, stage, dup)
		// Document observed behavior: Memory and Redis overwrite silently,
		// Postgres returns an error (PRIMARY KEY violation).
		// Both behaviors are acceptable — the important thing is that the
		// caller should never enqueue the same task ID twice. This test
		// documents the divergence.
		if err != nil {
			t.Logf("duplicate enqueue returned error (Postgres behavior): %v", err)
		} else {
			t.Log("duplicate enqueue succeeded silently (Memory/Redis behavior: overwrites)")
			// Verify the overwritten task has the new payload.
			got, err := s.GetTask(ctx, task.ID)
			if err != nil {
				t.Fatalf("get after overwrite: %v", err)
			}
			if !jsonEqual(got.Payload, []byte(`{"key":"updated"}`)) {
				t.Errorf("payload not updated: %s", string(got.Payload))
			}
		}
	})

	t.Run("ListTasksLimitZeroReturnsAll", func(t *testing.T) {
		t.Parallel()
		s := factory()
		ctx := context.Background()
		stage := "stage-limit0-" + uuid.New().String()

		const n = 5
		for i := 0; i < n; i++ {
			task := newTask("pipe-1", stage)
			if err := s.EnqueueTask(ctx, stage, task); err != nil {
				t.Fatalf("enqueue %d: %v", i, err)
			}
		}

		stageFilter := stage
		result, err := s.ListTasks(ctx, broker.TaskFilter{
			StageID: &stageFilter,
			Limit:   0, // zero means no limit — return all
		})
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(result.Tasks) != n {
			t.Errorf("got %d results, want %d (limit=0 should return all)", len(result.Tasks), n)
		}
	})

	t.Run("ListTasksWithPagination", func(t *testing.T) {
		t.Parallel()
		s := factory()
		ctx := context.Background()
		stage := "stage-page-" + uuid.New().String()

		const n = 10
		for i := 0; i < n; i++ {
			task := newTask("pipe-1", stage)
			task.CreatedAt = time.Now().Add(time.Duration(i) * time.Millisecond)
			if err := s.EnqueueTask(ctx, stage, task); err != nil {
				t.Fatalf("enqueue %d: %v", i, err)
			}
		}

		stageFilter := stage
		result, err := s.ListTasks(ctx, broker.TaskFilter{
			StageID: &stageFilter,
			Limit:   3,
			Offset:  0,
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Tasks) != 3 {
			t.Errorf("page 1: got %d, want 3", len(result.Tasks))
		}
	})

	// seedDeadLettered creates a task directly in FAILED+RoutedToDeadLetter=true
	// state so that ClaimForReplay can claim it.
	seedDeadLettered := func(t *testing.T, s store.Store, stage string) *broker.Task {
		t.Helper()
		ctx := context.Background()
		task := newTask("pipe-replay", stage)
		if err := s.EnqueueTask(ctx, stage, task); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
		failed := broker.TaskStateFailed
		dl := true
		if err := s.UpdateTask(ctx, task.ID, broker.TaskUpdate{
			State:              &failed,
			RoutedToDeadLetter: &dl,
		}); err != nil {
			t.Fatalf("set failed+dl: %v", err)
		}
		return task
	}

	t.Run("ClaimForReplay_HappyPath", func(t *testing.T) {
		t.Parallel()
		s := factory()
		task := seedDeadLettered(t, s, "stage-claim-happy-"+uuid.New().String())

		claimed, err := s.ClaimForReplay(context.Background(), task.ID)
		if err != nil {
			t.Fatalf("ClaimForReplay: %v", err)
		}
		if claimed.State != broker.TaskStateReplayPending {
			t.Errorf("state: got %s, want REPLAY_PENDING", claimed.State)
		}
		if claimed.RoutedToDeadLetter {
			t.Error("RoutedToDeadLetter should be false after claim")
		}
	})

	t.Run("ClaimForReplay_NotFound", func(t *testing.T) {
		t.Parallel()
		s := factory()
		_, err := s.ClaimForReplay(context.Background(), "nonexistent-"+uuid.New().String())
		if err != store.ErrTaskNotFound {
			t.Errorf("got %v, want ErrTaskNotFound", err)
		}
	})

	t.Run("ClaimForReplay_NotDeadLettered", func(t *testing.T) {
		t.Parallel()
		s := factory()
		ctx := context.Background()
		stage := "stage-claim-notdl-" + uuid.New().String()
		task := newTask("pipe-replay", stage)
		if err := s.EnqueueTask(ctx, stage, task); err != nil {
			t.Fatal(err)
		}
		failed := broker.TaskStateFailed
		// RoutedToDeadLetter stays false.
		if err := s.UpdateTask(ctx, task.ID, broker.TaskUpdate{State: &failed}); err != nil {
			t.Fatal(err)
		}
		_, err := s.ClaimForReplay(ctx, task.ID)
		if err != store.ErrTaskNotReplayable {
			t.Errorf("got %v, want ErrTaskNotReplayable", err)
		}
	})

	t.Run("ClaimForReplay_WrongState", func(t *testing.T) {
		t.Parallel()
		states := []broker.TaskState{
			broker.TaskStatePending,
			broker.TaskStateRouting,
			broker.TaskStateExecuting,
			broker.TaskStateValidating,
			broker.TaskStateDone,
			broker.TaskStateReplayed,
		}
		for _, st := range states {
			st := st
			t.Run(string(st), func(t *testing.T) {
				t.Parallel()
				s := factory()
				ctx := context.Background()
				stage := "stage-claim-wrong-" + uuid.New().String()
				task := newTask("pipe-replay", stage)
				if err := s.EnqueueTask(ctx, stage, task); err != nil {
					t.Fatal(err)
				}
				// Set non-replayable state; for the FAILED comparison we also
				// leave RoutedToDeadLetter=false so each case is non-replayable.
				state := st
				if err := s.UpdateTask(ctx, task.ID, broker.TaskUpdate{State: &state}); err != nil {
					t.Fatal(err)
				}
				_, err := s.ClaimForReplay(ctx, task.ID)
				if err != store.ErrTaskNotReplayable {
					t.Errorf("state=%s: got %v, want ErrTaskNotReplayable", st, err)
				}
			})
		}
	})

	t.Run("ClaimForReplay_AlreadyClaimed", func(t *testing.T) {
		t.Parallel()
		s := factory()
		task := seedDeadLettered(t, s, "stage-claim-twice-"+uuid.New().String())

		if _, err := s.ClaimForReplay(context.Background(), task.ID); err != nil {
			t.Fatalf("first claim: %v", err)
		}
		_, err := s.ClaimForReplay(context.Background(), task.ID)
		if err != store.ErrTaskNotReplayable {
			t.Errorf("second claim: got %v, want ErrTaskNotReplayable", err)
		}
	})

	t.Run("ClaimForReplay_Concurrent", func(t *testing.T) {
		t.Parallel()
		s := factory()
		task := seedDeadLettered(t, s, "stage-claim-concurrent-"+uuid.New().String())

		const N = 20
		type result struct {
			claimed *broker.Task
			err     error
		}
		results := make(chan result, N)
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(N)
		for i := 0; i < N; i++ {
			go func() {
				defer wg.Done()
				<-start
				c, err := s.ClaimForReplay(context.Background(), task.ID)
				results <- result{claimed: c, err: err}
			}()
		}
		close(start)
		wg.Wait()
		close(results)

		wins := 0
		notReplayable := 0
		for r := range results {
			switch {
			case r.err == nil && r.claimed != nil:
				wins++
			case r.err == store.ErrTaskNotReplayable:
				notReplayable++
			default:
				t.Errorf("unexpected result: claimed=%v err=%v", r.claimed, r.err)
			}
		}
		if wins != 1 {
			t.Errorf("winners: got %d, want 1", wins)
		}
		if notReplayable != N-1 {
			t.Errorf("ErrTaskNotReplayable losers: got %d, want %d", notReplayable, N-1)
		}
	})

	t.Run("RollbackReplayClaim_HappyPath", func(t *testing.T) {
		t.Parallel()
		s := factory()
		task := seedDeadLettered(t, s, "stage-rollback-happy-"+uuid.New().String())
		ctx := context.Background()

		if _, err := s.ClaimForReplay(ctx, task.ID); err != nil {
			t.Fatalf("claim: %v", err)
		}
		if err := s.RollbackReplayClaim(ctx, task.ID); err != nil {
			t.Fatalf("rollback: %v", err)
		}
		got, err := s.GetTask(ctx, task.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.State != broker.TaskStateFailed {
			t.Errorf("state: got %s, want FAILED", got.State)
		}
		if !got.RoutedToDeadLetter {
			t.Error("RoutedToDeadLetter should be true after rollback")
		}
	})

	t.Run("RollbackReplayClaim_NotFound", func(t *testing.T) {
		t.Parallel()
		s := factory()
		err := s.RollbackReplayClaim(context.Background(), "nonexistent-"+uuid.New().String())
		if err != store.ErrTaskNotFound {
			t.Errorf("got %v, want ErrTaskNotFound", err)
		}
	})

	t.Run("RollbackReplayClaim_WrongState", func(t *testing.T) {
		t.Parallel()
		s := factory()
		// Seed in FAILED+DL but do not claim — so it's not REPLAY_PENDING.
		task := seedDeadLettered(t, s, "stage-rollback-wrong-"+uuid.New().String())

		err := s.RollbackReplayClaim(context.Background(), task.ID)
		if err != store.ErrTaskNotReplayPending {
			t.Errorf("got %v, want ErrTaskNotReplayPending", err)
		}
	})

	t.Run("RollbackReplayClaim_AfterRollback", func(t *testing.T) {
		t.Parallel()
		s := factory()
		task := seedDeadLettered(t, s, "stage-rollback-then-claim-"+uuid.New().String())
		ctx := context.Background()

		if _, err := s.ClaimForReplay(ctx, task.ID); err != nil {
			t.Fatal(err)
		}
		if err := s.RollbackReplayClaim(ctx, task.ID); err != nil {
			t.Fatal(err)
		}
		claimed, err := s.ClaimForReplay(ctx, task.ID)
		if err != nil {
			t.Fatalf("claim-after-rollback: %v", err)
		}
		if claimed.State != broker.TaskStateReplayPending {
			t.Errorf("state: got %s, want REPLAY_PENDING", claimed.State)
		}
	})

	t.Run("ReplayPendingToReplayed", func(t *testing.T) {
		t.Parallel()
		s := factory()
		ctx := context.Background()
		task := seedDeadLettered(t, s, "stage-replayed-"+uuid.New().String())

		if _, err := s.ClaimForReplay(ctx, task.ID); err != nil {
			t.Fatalf("claim: %v", err)
		}
		replayed := broker.TaskStateReplayed
		if err := s.UpdateTask(ctx, task.ID, broker.TaskUpdate{State: &replayed}); err != nil {
			t.Fatalf("set REPLAYED: %v", err)
		}

		got, err := s.GetTask(ctx, task.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.State != broker.TaskStateReplayed {
			t.Errorf("state: got %s, want REPLAYED", got.State)
		}

		_, err = s.ClaimForReplay(ctx, task.ID)
		if err != store.ErrTaskNotReplayable {
			t.Errorf("claim on REPLAYED: got %v, want ErrTaskNotReplayable", err)
		}

		dl := true
		result, err := s.ListTasks(ctx, broker.TaskFilter{RoutedToDeadLetter: &dl})
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		for _, tk := range result.Tasks {
			if tk.ID == task.ID {
				t.Errorf("REPLAYED task %s should not appear in dead-letter listing", task.ID)
			}
		}
	})

	t.Run("RollbackReplayClaim_Idempotency", func(t *testing.T) {
		t.Parallel()
		s := factory()
		ctx := context.Background()
		task := seedDeadLettered(t, s, "stage-rollback-idem-"+uuid.New().String())

		if _, err := s.ClaimForReplay(ctx, task.ID); err != nil {
			t.Fatalf("claim: %v", err)
		}
		if err := s.RollbackReplayClaim(ctx, task.ID); err != nil {
			t.Fatalf("first rollback: %v", err)
		}
		// Second rollback: task is FAILED+DL, not REPLAY_PENDING.
		err := s.RollbackReplayClaim(ctx, task.ID)
		if err != store.ErrTaskNotReplayPending {
			t.Errorf("second rollback: got %v, want ErrTaskNotReplayPending", err)
		}
		got, err := s.GetTask(ctx, task.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.State != broker.TaskStateFailed {
			t.Errorf("state: got %s, want FAILED", got.State)
		}
		if !got.RoutedToDeadLetter {
			t.Error("RoutedToDeadLetter: got false, want true")
		}
	})

	// SEC4-008d: DiscardDeadLetter is the atomic CAS that replaces the
	// old read-check-write discard shape. Conformance coverage runs
	// against every backend — memory, Redis, Postgres — so the race the
	// audit flagged cannot come back through any implementation.

	t.Run("DiscardDeadLetter_HappyPath", func(t *testing.T) {
		t.Parallel()
		s := factory()
		ctx := context.Background()
		task := seedDeadLettered(t, s, "stage-discard-happy-"+uuid.New().String())

		if err := s.DiscardDeadLetter(ctx, task.ID); err != nil {
			t.Fatalf("discard: %v", err)
		}
		got, err := s.GetTask(ctx, task.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.State != broker.TaskStateDiscarded {
			t.Errorf("state: got %s, want DISCARDED", got.State)
		}
	})

	t.Run("DiscardDeadLetter_NotFound", func(t *testing.T) {
		t.Parallel()
		s := factory()
		err := s.DiscardDeadLetter(context.Background(), "nonexistent-"+uuid.New().String())
		if err != store.ErrTaskNotFound {
			t.Errorf("got %v, want ErrTaskNotFound", err)
		}
	})

	t.Run("DiscardDeadLetter_AlreadyDiscarded", func(t *testing.T) {
		t.Parallel()
		s := factory()
		ctx := context.Background()
		task := seedDeadLettered(t, s, "stage-discard-idem-"+uuid.New().String())
		if err := s.DiscardDeadLetter(ctx, task.ID); err != nil {
			t.Fatalf("first discard: %v", err)
		}
		err := s.DiscardDeadLetter(ctx, task.ID)
		if err != store.ErrTaskAlreadyDiscarded {
			t.Errorf("second discard: got %v, want ErrTaskAlreadyDiscarded", err)
		}
	})

	t.Run("DiscardDeadLetter_LosesToReplayClaim", func(t *testing.T) {
		t.Parallel()
		s := factory()
		ctx := context.Background()
		task := seedDeadLettered(t, s, "stage-discard-vs-replay-"+uuid.New().String())

		// Replay claims first — the winner.
		if _, err := s.ClaimForReplay(ctx, task.ID); err != nil {
			t.Fatalf("claim: %v", err)
		}
		// Discard arrives after the claim: the atomic CAS rejects it
		// so the REPLAY_PENDING state is preserved.
		err := s.DiscardDeadLetter(ctx, task.ID)
		if err != store.ErrTaskNotDiscardable {
			t.Errorf("discard after replay claim: got %v, want ErrTaskNotDiscardable", err)
		}
		got, err := s.GetTask(ctx, task.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.State != broker.TaskStateReplayPending {
			t.Errorf("state: got %s, want REPLAY_PENDING (replay must still own the task)", got.State)
		}
	})

	t.Run("DiscardDeadLetter_NotDiscardableWhenNotDeadLettered", func(t *testing.T) {
		t.Parallel()
		s := factory()
		ctx := context.Background()
		// Seed FAILED but NOT dead-lettered by doing a direct UpdateTask
		// after a normal enqueue.
		task := newTask("p", "stage-discard-notDL-"+uuid.New().String())
		if err := s.EnqueueTask(ctx, task.StageID, task); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
		failed := broker.TaskStateFailed
		if err := s.UpdateTask(ctx, task.ID, broker.TaskUpdate{State: &failed}); err != nil {
			t.Fatalf("set FAILED: %v", err)
		}
		if err := s.DiscardDeadLetter(ctx, task.ID); err != store.ErrTaskNotDiscardable {
			t.Errorf("discard on FAILED (not DL): got %v, want ErrTaskNotDiscardable", err)
		}
	})

	t.Run("UpdateTaskAllFields", func(t *testing.T) {
		t.Parallel()
		s := factory()
		ctx := context.Background()
		stage := "stage-update-all-" + uuid.New().String()

		task := newTask("pipe-update-all", stage)
		if err := s.EnqueueTask(ctx, stage, task); err != nil {
			t.Fatal(err)
		}

		newStage := "stage-update-all-moved-" + uuid.New().String()
		newPayload := json.RawMessage(`{"updated":"payload"}`)
		newState := broker.TaskStateExecuting
		newAttempts := 7
		newMaxAttempts := 42
		newInputName := "new_input"
		newInputVersion := "v9"
		newOutputName := "new_output"
		newOutputVersion := "v9"
		newDL := true
		newCross := 3

		if err := s.UpdateTask(ctx, task.ID, broker.TaskUpdate{
			State:                 &newState,
			StageID:               &newStage,
			Payload:               &newPayload,
			Metadata:              map[string]any{"extra": "data"},
			Attempts:              &newAttempts,
			MaxAttempts:           &newMaxAttempts,
			InputSchemaName:       &newInputName,
			InputSchemaVersion:    &newInputVersion,
			OutputSchemaName:      &newOutputName,
			OutputSchemaVersion:   &newOutputVersion,
			RoutedToDeadLetter:    &newDL,
			CrossStageTransitions: &newCross,
		}); err != nil {
			t.Fatalf("UpdateTask: %v", err)
		}

		got, err := s.GetTask(ctx, task.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.State != newState {
			t.Errorf("State: got %s, want %s", got.State, newState)
		}
		if got.StageID != newStage {
			t.Errorf("StageID: got %s, want %s", got.StageID, newStage)
		}
		if !jsonEqual(got.Payload, newPayload) {
			t.Errorf("Payload: got %s, want %s (semantic JSON comparison)", got.Payload, newPayload)
		}
		if got.Attempts != newAttempts {
			t.Errorf("Attempts: got %d, want %d", got.Attempts, newAttempts)
		}
		if got.MaxAttempts != newMaxAttempts {
			t.Errorf("MaxAttempts: got %d, want %d", got.MaxAttempts, newMaxAttempts)
		}
		if got.InputSchemaName != newInputName {
			t.Errorf("InputSchemaName: got %s, want %s", got.InputSchemaName, newInputName)
		}
		if got.InputSchemaVersion != newInputVersion {
			t.Errorf("InputSchemaVersion: got %s, want %s", got.InputSchemaVersion, newInputVersion)
		}
		if got.OutputSchemaName != newOutputName {
			t.Errorf("OutputSchemaName: got %s, want %s", got.OutputSchemaName, newOutputName)
		}
		if got.OutputSchemaVersion != newOutputVersion {
			t.Errorf("OutputSchemaVersion: got %s, want %s", got.OutputSchemaVersion, newOutputVersion)
		}
		if !got.RoutedToDeadLetter {
			t.Error("RoutedToDeadLetter: got false, want true")
		}
		if got.CrossStageTransitions != newCross {
			t.Errorf("CrossStageTransitions: got %d, want %d", got.CrossStageTransitions, newCross)
		}
		if got.Metadata["extra"] != "data" {
			t.Error("Metadata missing 'extra' key")
		}
		if got.Metadata["source"] != "test" {
			t.Error("Metadata: original 'source' key lost (merge expected)")
		}
	})

	t.Run("EnqueueDequeueRoundTripPreservesFields", func(t *testing.T) {
		t.Parallel()
		s := factory()
		ctx := context.Background()
		stage := "stage-rt-" + uuid.New().String()

		orig := newTask("pipe-rt", stage)
		orig.MaxAttempts = 9
		orig.Attempts = 1
		orig.CrossStageTransitions = 2
		orig.Payload = json.RawMessage(`{"roundtrip":true,"n":42}`)
		orig.Metadata = map[string]any{"source": "test", "trace": "abc-123"}

		if err := s.EnqueueTask(ctx, stage, orig); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
		got, err := s.DequeueTask(ctx, stage)
		if err != nil {
			t.Fatalf("dequeue: %v", err)
		}
		if got.ID != orig.ID {
			t.Errorf("ID: got %s, want %s", got.ID, orig.ID)
		}
		if got.PipelineID != orig.PipelineID {
			t.Errorf("PipelineID: got %s, want %s", got.PipelineID, orig.PipelineID)
		}
		if got.StageID != orig.StageID {
			t.Errorf("StageID: got %s, want %s", got.StageID, orig.StageID)
		}
		if got.InputSchemaName != orig.InputSchemaName {
			t.Errorf("InputSchemaName: got %s, want %s", got.InputSchemaName, orig.InputSchemaName)
		}
		if got.InputSchemaVersion != orig.InputSchemaVersion {
			t.Errorf("InputSchemaVersion: got %s, want %s", got.InputSchemaVersion, orig.InputSchemaVersion)
		}
		if got.OutputSchemaName != orig.OutputSchemaName {
			t.Errorf("OutputSchemaName: got %s, want %s", got.OutputSchemaName, orig.OutputSchemaName)
		}
		if got.OutputSchemaVersion != orig.OutputSchemaVersion {
			t.Errorf("OutputSchemaVersion: got %s, want %s", got.OutputSchemaVersion, orig.OutputSchemaVersion)
		}
		if !jsonEqual(got.Payload, orig.Payload) {
			t.Errorf("Payload: got %s, want %s (semantic JSON comparison)", got.Payload, orig.Payload)
		}
		if got.MaxAttempts != orig.MaxAttempts {
			t.Errorf("MaxAttempts: got %d, want %d", got.MaxAttempts, orig.MaxAttempts)
		}
		if got.Attempts != orig.Attempts {
			t.Errorf("Attempts: got %d, want %d", got.Attempts, orig.Attempts)
		}
		if got.CrossStageTransitions != orig.CrossStageTransitions {
			t.Errorf("CrossStageTransitions: got %d, want %d", got.CrossStageTransitions, orig.CrossStageTransitions)
		}
		if got.Metadata["source"] != "test" || got.Metadata["trace"] != "abc-123" {
			t.Errorf("Metadata not preserved: %v", got.Metadata)
		}
	})

	t.Run("ListTasksFilterByPipelineID", func(t *testing.T) {
		t.Parallel()
		s := factory()
		ctx := context.Background()
		pipeA := "pipe-a-" + uuid.New().String()
		pipeB := "pipe-b-" + uuid.New().String()
		stage := "stage-filter-pipe-" + uuid.New().String()

		for i := 0; i < 3; i++ {
			if err := s.EnqueueTask(ctx, stage, newTask(pipeA, stage)); err != nil {
				t.Fatal(err)
			}
		}
		for i := 0; i < 2; i++ {
			if err := s.EnqueueTask(ctx, stage, newTask(pipeB, stage)); err != nil {
				t.Fatal(err)
			}
		}

		result, err := s.ListTasks(ctx, broker.TaskFilter{PipelineID: &pipeA})
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Tasks) != 3 {
			t.Errorf("pipeA: got %d, want 3", len(result.Tasks))
		}
		for _, tk := range result.Tasks {
			if tk.PipelineID != pipeA {
				t.Errorf("wrong pipeline: %s", tk.PipelineID)
			}
		}

		result, err = s.ListTasks(ctx, broker.TaskFilter{PipelineID: &pipeB})
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Tasks) != 2 {
			t.Errorf("pipeB: got %d, want 2", len(result.Tasks))
		}
	})

	t.Run("ListTasksFilterByState", func(t *testing.T) {
		t.Parallel()
		s := factory()
		ctx := context.Background()
		stage := "stage-filter-state-" + uuid.New().String()

		var ids []string
		for i := 0; i < 4; i++ {
			tk := newTask("pipe-state", stage)
			if err := s.EnqueueTask(ctx, stage, tk); err != nil {
				t.Fatal(err)
			}
			ids = append(ids, tk.ID)
		}
		executing := broker.TaskStateExecuting
		for _, id := range ids[:2] {
			if err := s.UpdateTask(ctx, id, broker.TaskUpdate{State: &executing}); err != nil {
				t.Fatal(err)
			}
		}

		stageFilter := stage
		result, err := s.ListTasks(ctx, broker.TaskFilter{
			StageID: &stageFilter,
			State:   &executing,
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Tasks) != 2 {
			t.Errorf("executing: got %d, want 2", len(result.Tasks))
		}
		for _, tk := range result.Tasks {
			if tk.State != broker.TaskStateExecuting {
				t.Errorf("wrong state: %s", tk.State)
			}
		}

		pending := broker.TaskStatePending
		result, err = s.ListTasks(ctx, broker.TaskFilter{
			StageID: &stageFilter,
			State:   &pending,
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Tasks) != 2 {
			t.Errorf("pending: got %d, want 2", len(result.Tasks))
		}
	})

	t.Run("ListTasksExcludesDiscardedByDefault", func(t *testing.T) {
		t.Parallel()
		s := factory()
		ctx := context.Background()
		stage := "stage-filter-discard-" + uuid.New().String()

		var ids []string
		for i := 0; i < 3; i++ {
			tk := newTask("pipe-disc", stage)
			if err := s.EnqueueTask(ctx, stage, tk); err != nil {
				t.Fatal(err)
			}
			ids = append(ids, tk.ID)
		}
		discarded := broker.TaskStateDiscarded
		if err := s.UpdateTask(ctx, ids[0], broker.TaskUpdate{State: &discarded}); err != nil {
			t.Fatal(err)
		}

		stageFilter := stage

		result, err := s.ListTasks(ctx, broker.TaskFilter{StageID: &stageFilter})
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Tasks) != 2 {
			t.Errorf("default: got %d, want 2 (DISCARDED excluded)", len(result.Tasks))
		}
		for _, tk := range result.Tasks {
			if tk.State == broker.TaskStateDiscarded {
				t.Errorf("task %s is DISCARDED but was returned", tk.ID)
			}
		}

		result, err = s.ListTasks(ctx, broker.TaskFilter{
			StageID:          &stageFilter,
			IncludeDiscarded: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Tasks) != 3 {
			t.Errorf("include_discarded: got %d, want 3", len(result.Tasks))
		}
	})

	t.Run("ListTasksFilterByRoutedToDeadLetter", func(t *testing.T) {
		t.Parallel()
		s := factory()
		ctx := context.Background()
		stage := "stage-filter-dl-" + uuid.New().String()

		var ids []string
		for i := 0; i < 4; i++ {
			tk := newTask("pipe-dl", stage)
			if err := s.EnqueueTask(ctx, stage, tk); err != nil {
				t.Fatal(err)
			}
			ids = append(ids, tk.ID)
		}
		failed := broker.TaskStateFailed
		dl := true
		for _, id := range ids[:2] {
			if err := s.UpdateTask(ctx, id, broker.TaskUpdate{
				State:              &failed,
				RoutedToDeadLetter: &dl,
			}); err != nil {
				t.Fatal(err)
			}
		}

		stageFilter := stage
		truthy := true
		result, err := s.ListTasks(ctx, broker.TaskFilter{
			StageID:            &stageFilter,
			RoutedToDeadLetter: &truthy,
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Tasks) != 2 {
			t.Errorf("dl=true: got %d, want 2", len(result.Tasks))
		}
		for _, tk := range result.Tasks {
			if !tk.RoutedToDeadLetter {
				t.Errorf("task %s: RoutedToDeadLetter should be true", tk.ID)
			}
		}

		falsy := false
		result, err = s.ListTasks(ctx, broker.TaskFilter{
			StageID:            &stageFilter,
			RoutedToDeadLetter: &falsy,
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Tasks) != 2 {
			t.Errorf("dl=false: got %d, want 2", len(result.Tasks))
		}
		for _, tk := range result.Tasks {
			if tk.RoutedToDeadLetter {
				t.Errorf("task %s: RoutedToDeadLetter should be false", tk.ID)
			}
		}
	})

	t.Run("ListTasksCombinedFilters", func(t *testing.T) {
		t.Parallel()
		s := factory()
		ctx := context.Background()
		pipeA := "pipe-combo-a-" + uuid.New().String()
		pipeB := "pipe-combo-b-" + uuid.New().String()
		stage := "stage-combo-" + uuid.New().String()

		var aIDs, bIDs []string
		for i := 0; i < 3; i++ {
			tk := newTask(pipeA, stage)
			if err := s.EnqueueTask(ctx, stage, tk); err != nil {
				t.Fatal(err)
			}
			aIDs = append(aIDs, tk.ID)
		}
		for i := 0; i < 3; i++ {
			tk := newTask(pipeB, stage)
			if err := s.EnqueueTask(ctx, stage, tk); err != nil {
				t.Fatal(err)
			}
			bIDs = append(bIDs, tk.ID)
		}
		executing := broker.TaskStateExecuting
		for _, id := range aIDs[:2] {
			if err := s.UpdateTask(ctx, id, broker.TaskUpdate{State: &executing}); err != nil {
				t.Fatal(err)
			}
		}
		for _, id := range bIDs[:1] {
			if err := s.UpdateTask(ctx, id, broker.TaskUpdate{State: &executing}); err != nil {
				t.Fatal(err)
			}
		}

		stageFilter := stage
		result, err := s.ListTasks(ctx, broker.TaskFilter{
			PipelineID: &pipeA,
			StageID:    &stageFilter,
			State:      &executing,
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Tasks) != 2 {
			t.Errorf("combined: got %d, want 2", len(result.Tasks))
		}
		for _, tk := range result.Tasks {
			if tk.PipelineID != pipeA || tk.State != broker.TaskStateExecuting {
				t.Errorf("combined filter leak: %+v", tk)
			}
		}
	})

	t.Run("ListTasksOffsetAndTotal", func(t *testing.T) {
		t.Parallel()
		s := factory()
		ctx := context.Background()
		stage := "stage-offset-" + uuid.New().String()

		const n = 10
		for i := 0; i < n; i++ {
			tk := newTask("pipe-offset", stage)
			tk.CreatedAt = time.Now().Add(time.Duration(i) * time.Millisecond)
			if err := s.EnqueueTask(ctx, stage, tk); err != nil {
				t.Fatal(err)
			}
		}

		stageFilter := stage

		page1, err := s.ListTasks(ctx, broker.TaskFilter{
			StageID: &stageFilter,
			Limit:   3,
			Offset:  0,
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(page1.Tasks) != 3 {
			t.Errorf("page1 len: got %d, want 3", len(page1.Tasks))
		}
		if page1.Total != n {
			t.Errorf("page1 Total: got %d, want %d", page1.Total, n)
		}

		page2, err := s.ListTasks(ctx, broker.TaskFilter{
			StageID: &stageFilter,
			Limit:   3,
			Offset:  3,
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(page2.Tasks) != 3 {
			t.Errorf("page2 len: got %d, want 3", len(page2.Tasks))
		}
		if page2.Total != n {
			t.Errorf("page2 Total: got %d, want %d", page2.Total, n)
		}

		seen := make(map[string]bool)
		for _, tk := range page1.Tasks {
			seen[tk.ID] = true
		}
		for _, tk := range page2.Tasks {
			if seen[tk.ID] {
				t.Errorf("task %s appears in both page1 and page2", tk.ID)
			}
		}

		pageOOB, err := s.ListTasks(ctx, broker.TaskFilter{
			StageID: &stageFilter,
			Limit:   3,
			Offset:  100,
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(pageOOB.Tasks) != 0 {
			t.Errorf("oob page len: got %d, want 0", len(pageOOB.Tasks))
		}
		if pageOOB.Total != n {
			t.Errorf("oob Total: got %d, want %d", pageOOB.Total, n)
		}
	})

	t.Run("RollbackReplayClaim_Concurrent", func(t *testing.T) {
		t.Parallel()
		s := factory()
		task := seedDeadLettered(t, s, "stage-rollback-concurrent-"+uuid.New().String())
		ctx := context.Background()

		if _, err := s.ClaimForReplay(ctx, task.ID); err != nil {
			t.Fatalf("claim: %v", err)
		}

		const N = 20
		type result struct{ err error }
		results := make(chan result, N)
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(N)
		for i := 0; i < N; i++ {
			go func() {
				defer wg.Done()
				<-start
				results <- result{err: s.RollbackReplayClaim(ctx, task.ID)}
			}()
		}
		close(start)
		wg.Wait()
		close(results)

		wins := 0
		notPending := 0
		for r := range results {
			switch r.err {
			case nil:
				wins++
			case store.ErrTaskNotReplayPending:
				notPending++
			default:
				t.Errorf("unexpected error: %v", r.err)
			}
		}
		if wins != 1 {
			t.Errorf("winners: got %d, want 1", wins)
		}
		if notPending != N-1 {
			t.Errorf("ErrTaskNotReplayPending losers: got %d, want %d", notPending, N-1)
		}

		got, err := s.GetTask(ctx, task.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.State != broker.TaskStateFailed {
			t.Errorf("final state: got %s, want FAILED", got.State)
		}
		if !got.RoutedToDeadLetter {
			t.Error("RoutedToDeadLetter: got false, want true")
		}
	})

	t.Run("ExpiredTasksNotReturned", func(t *testing.T) {
		t.Parallel()
		s := factory()
		ctx := context.Background()
		stage := "stage-expired-" + uuid.New().String()

		expired := newTask("pipe-1", stage)
		expired.ExpiresAt = time.Now().Add(-1 * time.Second)

		live := newTask("pipe-1", stage)
		// Ensure live is enqueued after expired for FIFO ordering.
		live.CreatedAt = expired.CreatedAt.Add(time.Millisecond)

		if err := s.EnqueueTask(ctx, stage, expired); err != nil {
			t.Fatal(err)
		}
		if err := s.EnqueueTask(ctx, stage, live); err != nil {
			t.Fatal(err)
		}

		got, err := s.DequeueTask(ctx, stage)
		if err != nil {
			t.Fatalf("dequeue: %v", err)
		}
		if got.ID != live.ID {
			t.Errorf("got ID %s, want %s (expired task should be skipped)", got.ID, live.ID)
		}

		// GetTask on expired task should return ErrTaskNotFound.
		_, err = s.GetTask(ctx, expired.ID)
		if err != store.ErrTaskNotFound {
			t.Errorf("get expired: got %v, want ErrTaskNotFound", err)
		}
	})
}
