package store_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/orcastrator/orcastrator/internal/broker"
	"github.com/orcastrator/orcastrator/internal/store"
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
			if string(got.Payload) != `{"key":"updated"}` {
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
