package memory

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

func TestEnqueueDequeueFIFO(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := New()

	tasks := make([]*broker.Task, 5)
	for i := range tasks {
		tasks[i] = newTask("pipe-1", "stage-1")
		if err := s.EnqueueTask(ctx, "stage-1", tasks[i]); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}

	for i := range tasks {
		got, err := s.DequeueTask(ctx, "stage-1")
		if err != nil {
			t.Fatalf("dequeue %d: %v", i, err)
		}
		if got.ID != tasks[i].ID {
			t.Errorf("dequeue %d: got ID %s, want %s", i, got.ID, tasks[i].ID)
		}
	}
}

func TestDequeueEmptyQueue(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := New()

	_, err := s.DequeueTask(ctx, "nonexistent")
	if err != store.ErrQueueEmpty {
		t.Errorf("got %v, want ErrQueueEmpty", err)
	}
}

func TestGetTaskNotFound(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := New()

	_, err := s.GetTask(ctx, "nonexistent-id")
	if err != store.ErrTaskNotFound {
		t.Errorf("got %v, want ErrTaskNotFound", err)
	}
}

func TestUpdateTaskFields(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := New()

	task := newTask("pipe-1", "stage-1")
	if err := s.EnqueueTask(ctx, "stage-1", task); err != nil {
		t.Fatal(err)
	}

	newState := broker.TaskStateExecuting
	newStage := "stage-2"
	newPayload := json.RawMessage(`{"updated":true}`)
	newAttempts := 2

	err := s.UpdateTask(ctx, task.ID, broker.TaskUpdate{
		State:    &newState,
		StageID:  &newStage,
		Payload:  &newPayload,
		Metadata: map[string]any{"warning": "sanitizer flagged"},
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
	if got.StageID != "stage-2" {
		t.Errorf("stage_id: got %s, want stage-2", got.StageID)
	}
	if string(got.Payload) != `{"updated":true}` {
		t.Errorf("payload: got %s", got.Payload)
	}
	if got.Attempts != 2 {
		t.Errorf("attempts: got %d, want 2", got.Attempts)
	}
	if got.Metadata["warning"] != "sanitizer flagged" {
		t.Errorf("metadata missing warning key")
	}
	// Original metadata should still be present.
	if got.Metadata["source"] != "test" {
		t.Errorf("metadata lost original source key")
	}
	if !got.UpdatedAt.After(task.UpdatedAt) {
		t.Errorf("updated_at should have advanced")
	}
}

func TestUpdateTaskNotFound(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := New()

	err := s.UpdateTask(ctx, "nonexistent", broker.TaskUpdate{})
	if err != store.ErrTaskNotFound {
		t.Errorf("got %v, want ErrTaskNotFound", err)
	}
}

func TestTTLExpiry(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := New()

	task := newTask("pipe-1", "stage-1")
	task.ExpiresAt = time.Now().Add(-1 * time.Second) // already expired

	if err := s.EnqueueTask(ctx, "stage-1", task); err != nil {
		t.Fatal(err)
	}

	// Dequeue should skip expired tasks and return ErrQueueEmpty.
	_, err := s.DequeueTask(ctx, "stage-1")
	if err != store.ErrQueueEmpty {
		t.Errorf("dequeue expired: got %v, want ErrQueueEmpty", err)
	}

	// GetTask should also not find expired tasks.
	_, err = s.GetTask(ctx, task.ID)
	if err != store.ErrTaskNotFound {
		t.Errorf("get expired: got %v, want ErrTaskNotFound", err)
	}
}

func TestTTLExpiryInListTasks(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := New()

	live := newTask("pipe-1", "stage-1")
	expired := newTask("pipe-1", "stage-1")
	expired.ExpiresAt = time.Now().Add(-1 * time.Second)

	_ = s.EnqueueTask(ctx, "stage-1", live)
	_ = s.EnqueueTask(ctx, "stage-1", expired)

	pid := "pipe-1"
	result, err := s.ListTasks(ctx, broker.TaskFilter{PipelineID: &pid})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Tasks) != 1 {
		t.Errorf("list: got %d tasks, want 1 (expired should be excluded)", len(result.Tasks))
	}
}

func TestListTasksFilters(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := New()

	t1 := newTask("pipe-1", "stage-1")
	t1.State = broker.TaskStatePending

	t2 := newTask("pipe-1", "stage-2")
	t2.State = broker.TaskStateExecuting

	t3 := newTask("pipe-2", "stage-1")
	t3.State = broker.TaskStatePending

	_ = s.EnqueueTask(ctx, "stage-1", t1)
	_ = s.EnqueueTask(ctx, "stage-2", t2)
	_ = s.EnqueueTask(ctx, "stage-1", t3)

	// Filter by pipeline.
	pid := "pipe-1"
	result, _ := s.ListTasks(ctx, broker.TaskFilter{PipelineID: &pid})
	if len(result.Tasks) != 2 {
		t.Errorf("pipeline filter: got %d, want 2", len(result.Tasks))
	}

	// Filter by state.
	state := broker.TaskStatePending
	result, _ = s.ListTasks(ctx, broker.TaskFilter{State: &state})
	if len(result.Tasks) != 2 {
		t.Errorf("state filter: got %d, want 2", len(result.Tasks))
	}

	// Filter with limit.
	result, _ = s.ListTasks(ctx, broker.TaskFilter{Limit: 1})
	if len(result.Tasks) != 1 {
		t.Errorf("limit filter: got %d, want 1", len(result.Tasks))
	}
}

func TestConcurrentAccess(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := New()

	const workers = 10
	const tasksPerWorker = 50

	var wg sync.WaitGroup
	wg.Add(workers)

	// Enqueue concurrently.
	for w := 0; w < workers; w++ {
		go func() {
			defer wg.Done()
			for i := 0; i < tasksPerWorker; i++ {
				task := newTask("pipe-1", "stage-1")
				if err := s.EnqueueTask(ctx, "stage-1", task); err != nil {
					t.Errorf("concurrent enqueue: %v", err)
				}
			}
		}()
	}
	wg.Wait()

	// Dequeue concurrently — every task should come out exactly once.
	dequeued := make(chan string, workers*tasksPerWorker)
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func() {
			defer wg.Done()
			for {
				task, err := s.DequeueTask(ctx, "stage-1")
				if err == store.ErrQueueEmpty {
					return
				}
				if err != nil {
					t.Errorf("concurrent dequeue: %v", err)
					return
				}
				dequeued <- task.ID
			}
		}()
	}
	wg.Wait()
	close(dequeued)

	seen := make(map[string]bool)
	for id := range dequeued {
		if seen[id] {
			t.Errorf("task %s dequeued more than once", id)
		}
		seen[id] = true
	}

	total := len(seen)
	if total != workers*tasksPerWorker {
		t.Errorf("dequeued %d tasks, want %d", total, workers*tasksPerWorker)
	}
}

func TestEnqueueCopiesTask(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := New()

	task := newTask("pipe-1", "stage-1")
	_ = s.EnqueueTask(ctx, "stage-1", task)

	// Mutate the original — store should be unaffected.
	task.State = broker.TaskStateFailed

	got, _ := s.GetTask(ctx, task.ID)
	if got.State != broker.TaskStatePending {
		t.Errorf("store was mutated via external reference: got %s", got.State)
	}
}

func TestJSONSerializableTask(t *testing.T) {
	t.Parallel()
	task := newTask("pipe-1", "stage-1")

	data, err := json.Marshal(task)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded broker.Task
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if decoded.ID != task.ID {
		t.Errorf("round-trip ID mismatch")
	}
	if decoded.State != task.State {
		t.Errorf("round-trip state mismatch")
	}
	if string(decoded.Payload) != string(task.Payload) {
		t.Errorf("round-trip payload mismatch")
	}
}

func TestClaimForReplay_HappyPath(t *testing.T) {
	m := New()
	ctx := context.Background()
	task := newTask("p1", "s1")
	task.State = broker.TaskStateFailed
	task.RoutedToDeadLetter = true
	task.Attempts = 3
	if err := m.EnqueueTask(ctx, "s1", task); err != nil {
		t.Fatal(err)
	}
	claimed, err := m.ClaimForReplay(ctx, task.ID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if claimed.State != broker.TaskStateReplayPending {
		t.Errorf("state: got %s want REPLAY_PENDING", claimed.State)
	}
	if claimed.RoutedToDeadLetter {
		t.Errorf("routed_to_dead_letter should be cleared by the claim")
	}
	if claimed.Attempts != 3 {
		t.Errorf("attempts: got %d want 3", claimed.Attempts)
	}

	stored, err := m.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("get stored: %v", err)
	}
	if stored.State != broker.TaskStateReplayPending || stored.RoutedToDeadLetter || stored.Attempts != 3 {
		t.Errorf("stored: state=%s dl=%v attempts=%d, want REPLAY_PENDING/false/3",
			stored.State, stored.RoutedToDeadLetter, stored.Attempts)
	}

	// Second claim must fail: the flip is the claim token.
	if _, err := m.ClaimForReplay(ctx, task.ID); err != store.ErrTaskNotReplayable {
		t.Fatalf("second claim: got %v, want ErrTaskNotReplayable", err)
	}
}

func TestClaimForReplay_NotFound(t *testing.T) {
	m := New()
	_, err := m.ClaimForReplay(context.Background(), "does-not-exist")
	if err != store.ErrTaskNotFound {
		t.Fatalf("got %v, want ErrTaskNotFound", err)
	}
}

func TestClaimForReplay_NotReplayable(t *testing.T) {
	m := New()
	ctx := context.Background()
	task := newTask("p1", "s1")
	// Task in PENDING state — not replayable.
	if err := m.EnqueueTask(ctx, "s1", task); err != nil {
		t.Fatal(err)
	}
	_, err := m.ClaimForReplay(ctx, task.ID)
	if err != store.ErrTaskNotReplayable {
		t.Fatalf("got %v, want ErrTaskNotReplayable", err)
	}

	// FAILED but not dead-lettered → also not replayable.
	failed := broker.TaskStateFailed
	m.UpdateTask(ctx, task.ID, broker.TaskUpdate{State: &failed})
	_, err = m.ClaimForReplay(ctx, task.ID)
	if err != store.ErrTaskNotReplayable {
		t.Fatalf("got %v, want ErrTaskNotReplayable for non-dead-letter FAILED", err)
	}
}

// Concurrent ClaimForReplay calls must produce exactly one winner; the
// losers see ErrTaskNotReplayable once the winner flips the dead-letter
// flag.
func TestClaimForReplay_ConcurrentNoMutation(t *testing.T) {
	m := New()
	ctx := context.Background()
	task := newTask("p1", "s1")
	task.State = broker.TaskStateFailed
	task.RoutedToDeadLetter = true
	if err := m.EnqueueTask(ctx, "s1", task); err != nil {
		t.Fatal(err)
	}

	const N = 50
	var wg sync.WaitGroup
	errs := make([]error, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = m.ClaimForReplay(ctx, task.ID)
		}(i)
	}
	wg.Wait()

	wins, losses := 0, 0
	for _, err := range errs {
		switch err {
		case nil:
			wins++
		case store.ErrTaskNotReplayable:
			losses++
		default:
			t.Errorf("unexpected error: %v", err)
		}
	}
	if wins != 1 || losses != N-1 {
		t.Fatalf("wins=%d losses=%d, want 1/%d", wins, losses, N-1)
	}

	stored, err := m.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("get stored: %v", err)
	}
	if stored.State != broker.TaskStateReplayPending {
		t.Errorf("stored state: got %s want REPLAY_PENDING", stored.State)
	}
	if stored.RoutedToDeadLetter {
		t.Errorf("stored RoutedToDeadLetter should be cleared after a successful claim")
	}
}

func TestRollbackReplayClaim_RestoresDeadLettered(t *testing.T) {
	m := New()
	ctx := context.Background()
	task := newTask("p1", "s1")
	task.State = broker.TaskStateFailed
	task.RoutedToDeadLetter = true
	if err := m.EnqueueTask(ctx, "s1", task); err != nil {
		t.Fatal(err)
	}
	claimed, err := m.ClaimForReplay(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if claimed.State != broker.TaskStateReplayPending {
		t.Fatalf("claimed state: got %s want REPLAY_PENDING", claimed.State)
	}
	if err := m.RollbackReplayClaim(ctx, task.ID); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	got, err := m.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != broker.TaskStateFailed {
		t.Errorf("state after rollback: got %s want FAILED", got.State)
	}
	if !got.RoutedToDeadLetter {
		t.Errorf("RoutedToDeadLetter after rollback: got false want true")
	}
	// Task should be replayable again.
	if _, err := m.ClaimForReplay(ctx, task.ID); err != nil {
		t.Errorf("re-claim after rollback: got %v want nil", err)
	}
}

func TestRollbackReplayClaim_FailsIfNotReplayPending(t *testing.T) {
	m := New()
	ctx := context.Background()

	// Not found.
	if err := m.RollbackReplayClaim(ctx, "does-not-exist"); err != store.ErrTaskNotFound {
		t.Errorf("not found: got %v want ErrTaskNotFound", err)
	}

	// Task in FAILED state (not REPLAY_PENDING).
	task := newTask("p1", "s1")
	task.State = broker.TaskStateFailed
	task.RoutedToDeadLetter = true
	if err := m.EnqueueTask(ctx, "s1", task); err != nil {
		t.Fatal(err)
	}
	if err := m.RollbackReplayClaim(ctx, task.ID); err != store.ErrTaskNotReplayPending {
		t.Errorf("FAILED task: got %v want ErrTaskNotReplayPending", err)
	}
}

func TestClaimForReplay_TransitionsToReplayPending(t *testing.T) {
	m := New()
	ctx := context.Background()
	task := newTask("p1", "s1")
	task.State = broker.TaskStateFailed
	task.RoutedToDeadLetter = true
	if err := m.EnqueueTask(ctx, "s1", task); err != nil {
		t.Fatal(err)
	}
	claimed, err := m.ClaimForReplay(ctx, task.ID)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if claimed.State != broker.TaskStateReplayPending {
		t.Errorf("claimed state: got %s want REPLAY_PENDING", claimed.State)
	}
	stored, _ := m.GetTask(ctx, task.ID)
	if stored.State != broker.TaskStateReplayPending {
		t.Errorf("stored state: got %s want REPLAY_PENDING", stored.State)
	}
}
