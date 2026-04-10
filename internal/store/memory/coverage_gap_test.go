package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/orcastrator/orcastrator/internal/broker"
)

// TestMemoryStore_PayloadSizes verifies that payloads of various sizes
// round-trip correctly through EnqueueTask and GetTask.
func TestMemoryStore_PayloadSizes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := New()

	sizes := map[string]int{
		"1B":    1,
		"1KB":   1024,
		"100KB": 100 * 1024,
		"1MB":   1024 * 1024,
	}

	for label, size := range sizes {
		t.Run(label, func(t *testing.T) {
			// Build a valid JSON payload of approximately the target size.
			// Use a JSON string filled with 'x' characters.
			filler := strings.Repeat("x", size)
			payload := json.RawMessage(fmt.Sprintf(`{"data":"%s"}`, filler))

			task := newTask("pipe-payload", "stage-payload")
			task.Payload = payload

			if err := s.EnqueueTask(ctx, "stage-payload", task); err != nil {
				t.Fatalf("EnqueueTask failed for %s payload: %v", label, err)
			}

			got, err := s.GetTask(ctx, task.ID)
			if err != nil {
				t.Fatalf("GetTask failed for %s payload: %v", label, err)
			}

			if string(got.Payload) != string(payload) {
				t.Errorf("payload mismatch for %s: got %d bytes, want %d bytes",
					label, len(got.Payload), len(payload))
			}
		})
	}
}

// TestMemoryStore_ListTasksOrdering verifies that ListTasks returns tasks in
// deterministic insertion order, and that this order is consistent across
// multiple calls. The memory store maintains insertion order via the
// taskOrder slice.
func TestMemoryStore_ListTasksOrdering(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := New()

	const count = 10
	ids := make([]string, count)
	for i := 0; i < count; i++ {
		task := newTask("pipe-order", "stage-order")
		task.ID = fmt.Sprintf("task-order-%02d", i) // deterministic IDs
		ids[i] = task.ID
		if err := s.EnqueueTask(ctx, "stage-order", task); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}

	pid := "pipe-order"
	filter := broker.TaskFilter{PipelineID: &pid}

	// Call ListTasks multiple times and verify consistent ordering.
	for call := 0; call < 3; call++ {
		result, err := s.ListTasks(ctx, filter)
		if err != nil {
			t.Fatalf("ListTasks call %d: %v", call, err)
		}
		if len(result.Tasks) != count {
			t.Fatalf("ListTasks call %d: got %d tasks, want %d", call, len(result.Tasks), count)
		}
		for i, task := range result.Tasks {
			if task.ID != ids[i] {
				t.Errorf("ListTasks call %d, index %d: got ID %s, want %s",
					call, i, task.ID, ids[i])
			}
		}
	}
}

// TestMemoryStore_ContextCancellation documents that the memory store does NOT
// respect context cancellation. All operations use _ for the context parameter,
// so they complete successfully even with a cancelled context. This is by
// design for the in-memory backend; the broker layer handles context
// cancellation at a higher level.
func TestMemoryStore_ContextCancellation(t *testing.T) {
	t.Parallel()
	s := New()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	task := newTask("pipe-ctx", "stage-ctx")

	// EnqueueTask should succeed with cancelled context.
	if err := s.EnqueueTask(ctx, "stage-ctx", task); err != nil {
		t.Fatalf("EnqueueTask with cancelled ctx: %v", err)
	}

	// GetTask should succeed with cancelled context.
	got, err := s.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("GetTask with cancelled ctx: %v", err)
	}
	if got.ID != task.ID {
		t.Errorf("GetTask returned wrong task: got %s, want %s", got.ID, task.ID)
	}

	// DequeueTask should succeed with cancelled context.
	dequeued, err := s.DequeueTask(ctx, "stage-ctx")
	if err != nil {
		t.Fatalf("DequeueTask with cancelled ctx: %v", err)
	}
	if dequeued.ID != task.ID {
		t.Errorf("DequeueTask returned wrong task: got %s, want %s", dequeued.ID, task.ID)
	}

	// UpdateTask should succeed with cancelled context.
	// Re-enqueue to have something to update.
	task2 := newTask("pipe-ctx", "stage-ctx")
	_ = s.EnqueueTask(ctx, "stage-ctx", task2)
	newState := broker.TaskStateDone
	if err := s.UpdateTask(ctx, task2.ID, broker.TaskUpdate{State: &newState}); err != nil {
		t.Fatalf("UpdateTask with cancelled ctx: %v", err)
	}

	// ListTasks should succeed with cancelled context.
	pid := "pipe-ctx"
	result, err := s.ListTasks(ctx, broker.TaskFilter{PipelineID: &pid})
	if err != nil {
		t.Fatalf("ListTasks with cancelled ctx: %v", err)
	}
	if len(result.Tasks) < 1 {
		t.Errorf("ListTasks returned no tasks, expected at least 1")
	}
}

// TestMemoryStore_DequeueStateFiltering documents that the memory store does
// NOT filter tasks by state on dequeue. It returns whatever task is next in
// the queue, regardless of state. State-based filtering is the broker's
// responsibility, not the store's.
func TestMemoryStore_DequeueStateFiltering(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := New()

	stageID := "stage-states"

	// Enqueue tasks with different states.
	pending := newTask("pipe-states", stageID)
	pending.State = broker.TaskStatePending

	done := newTask("pipe-states", stageID)
	done.State = broker.TaskStateDone

	failed := newTask("pipe-states", stageID)
	failed.State = broker.TaskStateFailed

	for _, task := range []*broker.Task{pending, done, failed} {
		if err := s.EnqueueTask(ctx, stageID, task); err != nil {
			t.Fatalf("enqueue %s task: %v", task.State, err)
		}
	}

	// DequeueTask should return all three, in FIFO order, regardless of state.
	expectedIDs := []string{pending.ID, done.ID, failed.ID}
	expectedStates := []broker.TaskState{broker.TaskStatePending, broker.TaskStateDone, broker.TaskStateFailed}

	for i, wantID := range expectedIDs {
		got, err := s.DequeueTask(ctx, stageID)
		if err != nil {
			t.Fatalf("dequeue %d: %v", i, err)
		}
		if got.ID != wantID {
			t.Errorf("dequeue %d: got ID %s, want %s", i, got.ID, wantID)
		}
		if got.State != expectedStates[i] {
			t.Errorf("dequeue %d: got state %s, want %s", i, got.State, expectedStates[i])
		}
	}
}

// TestMemoryStore_ConcurrentOperations launches 100 goroutines that each
// enqueue a unique task to the same stage simultaneously. Verifies that all
// 100 tasks are stored without data loss, testing thread safety of the
// RWMutex-based synchronization.
func TestMemoryStore_ConcurrentOperations(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := New()

	const goroutines = 100
	stageID := "stage-concurrent"
	pipelineID := "pipe-concurrent"

	taskIDs := make([]string, goroutines)
	for i := range taskIDs {
		taskIDs[i] = uuid.New().String()
	}

	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := 0; i < goroutines; i++ {
		go func(idx int) {
			defer wg.Done()
			task := &broker.Task{
				ID:                  taskIDs[idx],
				PipelineID:          pipelineID,
				StageID:             stageID,
				InputSchemaName:     "test_input",
				InputSchemaVersion:  "v1",
				OutputSchemaName:    "test_output",
				OutputSchemaVersion: "v1",
				Payload:             json.RawMessage(fmt.Sprintf(`{"index":%d}`, idx)),
				Metadata:            map[string]any{"goroutine": idx},
				State:               broker.TaskStatePending,
				MaxAttempts:         3,
			}
			if err := s.EnqueueTask(ctx, stageID, task); err != nil {
				t.Errorf("goroutine %d enqueue failed: %v", idx, err)
			}
		}(i)
	}
	wg.Wait()

	// Verify all 100 tasks are stored via ListTasks.
	pid := pipelineID
	result, err := s.ListTasks(ctx, broker.TaskFilter{PipelineID: &pid})
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if result.Total != goroutines {
		t.Errorf("ListTasks total: got %d, want %d", result.Total, goroutines)
	}
	if len(result.Tasks) != goroutines {
		t.Errorf("ListTasks returned %d tasks, want %d", len(result.Tasks), goroutines)
	}

	// Verify each task ID is present.
	stored := make(map[string]bool, goroutines)
	for _, task := range result.Tasks {
		stored[task.ID] = true
	}
	for _, id := range taskIDs {
		if !stored[id] {
			t.Errorf("task %s not found in store", id)
		}
	}
}

// TestMemoryStore_DeepCopyIsolation verifies that modifying a task returned
// by GetTask does not affect the stored copy. The memory store returns deep
// copies to prevent callers from corrupting internal state.
func TestMemoryStore_DeepCopyIsolation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := New()

	task := newTask("pipe-copy", "stage-copy")
	task.Metadata = map[string]any{"original": "value"}
	task.Payload = json.RawMessage(`{"original":true}`)

	if err := s.EnqueueTask(ctx, "stage-copy", task); err != nil {
		t.Fatal(err)
	}

	// Get the task and mutate the returned copy.
	got, err := s.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}

	// Mutate metadata on the returned copy.
	got.Metadata["original"] = "corrupted"
	got.Metadata["injected"] = "bad"

	// Mutate payload on the returned copy.
	got.Payload = json.RawMessage(`{"corrupted":true}`)

	// Mutate scalar fields on the returned copy.
	got.State = broker.TaskStateFailed
	got.Attempts = 999

	// Get the task again from the store.
	stored, err := s.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}

	// Verify the stored copy is unchanged.
	if stored.State != broker.TaskStatePending {
		t.Errorf("state was corrupted: got %s, want PENDING", stored.State)
	}
	if stored.Attempts != 0 {
		t.Errorf("attempts was corrupted: got %d, want 0", stored.Attempts)
	}
	if stored.Metadata["original"] != "value" {
		t.Errorf("metadata[original] was corrupted: got %v, want 'value'", stored.Metadata["original"])
	}
	if _, exists := stored.Metadata["injected"]; exists {
		t.Errorf("metadata[injected] should not exist in stored copy")
	}
	if string(stored.Payload) != `{"original":true}` {
		t.Errorf("payload was corrupted: got %s, want {\"original\":true}", stored.Payload)
	}
}

// TestMemoryStore_UpdateTaskMetadataMerge verifies that UpdateTask merges
// metadata non-destructively. Existing keys that are not in the update are
// preserved, existing keys in the update are overwritten, and new keys from
// the update are added.
func TestMemoryStore_UpdateTaskMetadataMerge(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := New()

	task := newTask("pipe-merge", "stage-merge")
	task.Metadata = map[string]any{"a": 1, "b": 2}

	if err := s.EnqueueTask(ctx, "stage-merge", task); err != nil {
		t.Fatal(err)
	}

	// Update with overlapping and new keys.
	err := s.UpdateTask(ctx, task.ID, broker.TaskUpdate{
		Metadata: map[string]any{"b": 3, "c": 4},
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := s.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}

	// Verify merged result: {a:1, b:3, c:4}.
	expected := map[string]any{"a": 1, "b": 3, "c": 4}
	for key, want := range expected {
		gotVal, exists := got.Metadata[key]
		if !exists {
			t.Errorf("metadata[%s] missing after merge", key)
			continue
		}
		// Compare as strings since map[string]any values may differ in numeric type.
		if fmt.Sprintf("%v", gotVal) != fmt.Sprintf("%v", want) {
			t.Errorf("metadata[%s]: got %v, want %v", key, gotVal, want)
		}
	}

	// Ensure no extra keys exist.
	if len(got.Metadata) != len(expected) {
		t.Errorf("metadata has %d keys, want %d; contents: %v",
			len(got.Metadata), len(expected), got.Metadata)
	}
}
