package memory_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/brianbuquoi/overlord/internal/broker"
	"github.com/brianbuquoi/overlord/internal/store"
	"github.com/brianbuquoi/overlord/internal/store/memory"
)

// TestRequeueTask_MovesTaskToNewStage is the core RequeueTask
// contract: after the call, the same task ID dequeues on the
// destination stage (not the source stage) with the update
// applied. This mirrors what the broker relies on when routing
// from one stage to another.
func TestRequeueTask_MovesTaskToNewStage(t *testing.T) {
	ctx := context.Background()
	st := memory.New()

	task := &broker.Task{
		ID:         "t1",
		PipelineID: "p1",
		StageID:    "stage_a",
		State:      broker.TaskStatePending,
		Payload:    json.RawMessage(`{"v":1}`),
		CreatedAt:  time.Now(),
	}
	if err := st.EnqueueTask(ctx, "stage_a", task); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	// Dequeue once from stage_a so the task is in flight (simulating
	// what the broker does between DequeueTask and RequeueTask).
	first, err := st.DequeueTask(ctx, "stage_a")
	if err != nil {
		t.Fatalf("dequeue stage_a: %v", err)
	}
	if first.ID != "t1" {
		t.Fatalf("got task %q, want t1", first.ID)
	}

	// Requeue onto stage_b with updated state + stageID + payload.
	pending := broker.TaskStatePending
	nextStage := "stage_b"
	newPayload := json.RawMessage(`{"v":2}`)
	if err := st.RequeueTask(ctx, "t1", nextStage, broker.TaskUpdate{
		State:   &pending,
		StageID: &nextStage,
		Payload: &newPayload,
	}); err != nil {
		t.Fatalf("requeue: %v", err)
	}

	// stage_a's queue must be empty now — no residual of the old
	// queueing after we dequeued it above.
	if _, err := st.DequeueTask(ctx, "stage_a"); !errors.Is(err, store.ErrQueueEmpty) {
		t.Errorf("stage_a should be empty after requeue; got err=%v", err)
	}

	// stage_b must produce exactly that task with the new payload
	// and stage assignment.
	got, err := st.DequeueTask(ctx, "stage_b")
	if err != nil {
		t.Fatalf("dequeue stage_b: %v", err)
	}
	if got.ID != "t1" {
		t.Errorf("stage_b task id: got %q, want t1", got.ID)
	}
	if got.StageID != "stage_b" {
		t.Errorf("task.StageID: got %q, want stage_b", got.StageID)
	}
	if string(got.Payload) != `{"v":2}` {
		t.Errorf("task.Payload: got %q, want updated payload", string(got.Payload))
	}
}

// TestRequeueTask_ReturnsErrTaskNotFoundForMissingTask documents
// the error contract that broker.recordStoreError relies on.
func TestRequeueTask_ReturnsErrTaskNotFoundForMissingTask(t *testing.T) {
	ctx := context.Background()
	st := memory.New()
	err := st.RequeueTask(ctx, "nonexistent", "any", broker.TaskUpdate{})
	if !errors.Is(err, store.ErrTaskNotFound) {
		t.Fatalf("got err=%v, want ErrTaskNotFound", err)
	}
}

// TestRequeueTask_UpdateAndQueueAreAtomic confirms the
// "fail-closed" invariant on the backend: if a concurrent reader
// dequeues before RequeueTask, it sees the old task; if after, the
// new state. There is no in-between. Memory store is single-mutex
// so this is a smoke test, but the existence of the assertion keeps
// the contract explicit should a future implementation split the
// lock.
func TestRequeueTask_UpdateAndQueueAreAtomic(t *testing.T) {
	ctx := context.Background()
	st := memory.New()
	task := &broker.Task{
		ID:         "t1",
		PipelineID: "p1",
		StageID:    "source",
		State:      broker.TaskStatePending,
		CreatedAt:  time.Now(),
	}
	if err := st.EnqueueTask(ctx, "source", task); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DequeueTask(ctx, "source"); err != nil {
		t.Fatal(err)
	}

	newStage := "target"
	pending := broker.TaskStatePending
	if err := st.RequeueTask(ctx, "t1", newStage, broker.TaskUpdate{
		State:   &pending,
		StageID: &newStage,
	}); err != nil {
		t.Fatal(err)
	}

	got, err := st.GetTask(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.StageID != "target" || got.State != broker.TaskStatePending {
		t.Errorf("post-requeue state: stageID=%q state=%s; want target/PENDING", got.StageID, got.State)
	}
	// And the target queue must have the task available for the
	// next dequeue caller.
	pulled, err := st.DequeueTask(ctx, "target")
	if err != nil {
		t.Fatalf("target queue should be dequeueable after requeue; got err=%v", err)
	}
	if pulled.ID != "t1" {
		t.Errorf("target queue dequeue: got %q, want t1", pulled.ID)
	}
}
