package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/orcastrator/orcastrator/internal/broker"
	"github.com/orcastrator/orcastrator/internal/store"
)

// ---------- 2. Worker contention test ----------

func TestWorkerContention(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := New()

	const totalTasks = 50
	const intakeEnqueuers = 5
	const intakeToProcess = 3
	const processToValidate = 2
	const validateToDone = 1
	tasksPerEnqueuer := totalTasks / intakeEnqueuers

	// Track all enqueued task IDs.
	allIDs := &sync.Map{}

	var wg sync.WaitGroup

	// Phase 1: 5 goroutines enqueue tasks to "intake" simultaneously.
	wg.Add(intakeEnqueuers)
	for w := 0; w < intakeEnqueuers; w++ {
		go func() {
			defer wg.Done()
			for i := 0; i < tasksPerEnqueuer; i++ {
				task := newTask("pipe-contention", "intake")
				allIDs.Store(task.ID, true)
				if err := s.EnqueueTask(ctx, "intake", task); err != nil {
					t.Errorf("enqueue intake: %v", err)
				}
			}
		}()
	}
	wg.Wait()

	// Phase 2: 3 goroutines dequeue from "intake" and re-enqueue to "process".
	wg.Add(intakeToProcess)
	for w := 0; w < intakeToProcess; w++ {
		go func() {
			defer wg.Done()
			for {
				task, err := s.DequeueTask(ctx, "intake")
				if err == store.ErrQueueEmpty {
					return
				}
				if err != nil {
					t.Errorf("dequeue intake: %v", err)
					return
				}
				task.StageID = "process"
				if err := s.EnqueueTask(ctx, "process", task); err != nil {
					t.Errorf("enqueue process: %v", err)
				}
			}
		}()
	}
	wg.Wait()

	// Phase 3: 2 goroutines dequeue from "process" and re-enqueue to "validate".
	wg.Add(processToValidate)
	for w := 0; w < processToValidate; w++ {
		go func() {
			defer wg.Done()
			for {
				task, err := s.DequeueTask(ctx, "process")
				if err == store.ErrQueueEmpty {
					return
				}
				if err != nil {
					t.Errorf("dequeue process: %v", err)
					return
				}
				task.StageID = "validate"
				if err := s.EnqueueTask(ctx, "validate", task); err != nil {
					t.Errorf("enqueue validate: %v", err)
				}
			}
		}()
	}
	wg.Wait()

	// Phase 4: 1 goroutine dequeues from "validate" and marks DONE.
	doneIDs := &sync.Map{}
	wg.Add(validateToDone)
	go func() {
		defer wg.Done()
		for {
			task, err := s.DequeueTask(ctx, "validate")
			if err == store.ErrQueueEmpty {
				return
			}
			if err != nil {
				t.Errorf("dequeue validate: %v", err)
				return
			}
			state := broker.TaskStateDone
			if err := s.UpdateTask(ctx, task.ID, broker.TaskUpdate{State: &state}); err != nil {
				t.Errorf("update done: %v", err)
			}
			if _, loaded := doneIDs.LoadOrStore(task.ID, true); loaded {
				t.Errorf("task %s marked DONE more than once (duplicate)", task.ID)
			}
		}
	}()
	wg.Wait()

	// Assert: every enqueued task ID appears exactly once in DONE state.
	doneCount := 0
	doneIDs.Range(func(_, _ any) bool {
		doneCount++
		return true
	})
	if doneCount != totalTasks {
		t.Errorf("DONE tasks: got %d, want %d", doneCount, totalTasks)
	}

	// Cross-check: every originally-enqueued ID is in doneIDs.
	allIDs.Range(func(key, _ any) bool {
		if _, ok := doneIDs.Load(key); !ok {
			t.Errorf("task %s was enqueued but never reached DONE (lost)", key)
		}
		return true
	})

	// Verify all tasks are actually in DONE state in the store.
	doneIDs.Range(func(key, _ any) bool {
		task, err := s.GetTask(ctx, key.(string))
		if err != nil {
			t.Errorf("GetTask %s: %v", key, err)
			return true
		}
		if task.State != broker.TaskStateDone {
			t.Errorf("task %s state: got %s, want DONE", key, task.State)
		}
		return true
	})
}

// ---------- 3. FIFO guarantee ----------

func TestFIFOGuaranteeUnderConcurrentEnqueue(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := New()

	// FIFO guarantee: dequeue order matches enqueue order (queue insertion order).
	// With concurrent enqueuers, the queue order is determined by lock acquisition
	// inside the store. We serialize enqueue + order recording under a single mutex
	// so the recorded order exactly matches the store's internal queue order.

	const enqueuers = 5
	const tasksPerEnqueuer = 20
	total := enqueuers * tasksPerEnqueuer

	// Serialize enqueue + order recording so recorded order matches queue order.
	var enqueueMu sync.Mutex
	enqueueOrder := make([]string, 0, total)

	var wg sync.WaitGroup
	wg.Add(enqueuers)

	for w := 0; w < enqueuers; w++ {
		go func() {
			defer wg.Done()
			for i := 0; i < tasksPerEnqueuer; i++ {
				task := newTask("pipe-fifo", "stage-fifo")
				// Hold our mutex across both enqueue and order recording.
				// This guarantees the recorded order matches the store's FIFO order.
				enqueueMu.Lock()
				if err := s.EnqueueTask(ctx, "stage-fifo", task); err != nil {
					enqueueMu.Unlock()
					t.Errorf("enqueue: %v", err)
					return
				}
				enqueueOrder = append(enqueueOrder, task.ID)
				enqueueMu.Unlock()
			}
		}()
	}
	wg.Wait()

	// Dequeue all tasks.
	var dequeued []*broker.Task
	for {
		task, err := s.DequeueTask(ctx, "stage-fifo")
		if err == store.ErrQueueEmpty {
			break
		}
		if err != nil {
			t.Fatalf("dequeue: %v", err)
		}
		dequeued = append(dequeued, task)
	}

	if len(dequeued) != total {
		t.Fatalf("dequeued %d tasks, want %d", len(dequeued), total)
	}

	// Verify dequeue order matches the recorded enqueue order exactly.
	for i, task := range dequeued {
		if task.ID != enqueueOrder[i] {
			t.Errorf("FIFO violation at index %d: dequeued %s, expected %s", i, task.ID, enqueueOrder[i])
		}
	}

	// Verify no duplicates.
	seen := make(map[string]bool)
	for _, task := range dequeued {
		if seen[task.ID] {
			t.Errorf("task %s dequeued twice", task.ID)
		}
		seen[task.ID] = true
	}
	if len(seen) != total {
		t.Errorf("unique tasks: got %d, want %d", len(seen), total)
	}
}

// ---------- 4. TTL expiry with sleep ----------

func TestTTLExpiryWithSleep(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := New()

	task := newTask("pipe-ttl", "stage-ttl")
	task.ExpiresAt = time.Now().Add(1 * time.Millisecond)

	if err := s.EnqueueTask(ctx, "stage-ttl", task); err != nil {
		t.Fatal(err)
	}

	time.Sleep(5 * time.Millisecond)

	// First dequeue: must return ErrQueueEmpty, not the expired task.
	_, err := s.DequeueTask(ctx, "stage-ttl")
	if err != store.ErrQueueEmpty {
		t.Errorf("first dequeue: got %v, want ErrQueueEmpty", err)
	}

	// Second dequeue: verify expired task is actually removed, not just skipped.
	_, err = s.DequeueTask(ctx, "stage-ttl")
	if err != store.ErrQueueEmpty {
		t.Errorf("second dequeue: got %v, want ErrQueueEmpty (task should be removed from memory)", err)
	}

	// GetTask must also not find it.
	_, err = s.GetTask(ctx, task.ID)
	if err != store.ErrTaskNotFound {
		t.Errorf("GetTask after expiry: got %v, want ErrTaskNotFound", err)
	}

	// Verify the task is actually gone from the internal map by listing all tasks.
	result, err := s.ListTasks(ctx, broker.TaskFilter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range result.Tasks {
		if r.ID == task.ID {
			t.Errorf("expired task %s still visible in ListTasks", task.ID)
		}
	}
}

// ---------- 5. TaskFilter comprehensive ----------

func TestListTasksFilterByStageID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := New()

	t1 := newTask("pipe-1", "stage-a")
	t2 := newTask("pipe-1", "stage-b")
	t3 := newTask("pipe-1", "stage-a")
	_ = s.EnqueueTask(ctx, "stage-a", t1)
	_ = s.EnqueueTask(ctx, "stage-b", t2)
	_ = s.EnqueueTask(ctx, "stage-a", t3)

	stageA := "stage-a"
	result, err := s.ListTasks(ctx, broker.TaskFilter{StageID: &stageA})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Tasks) != 2 {
		t.Errorf("stage filter: got %d, want 2", len(result.Tasks))
	}
	for _, r := range result.Tasks {
		if r.StageID != "stage-a" {
			t.Errorf("unexpected stage %s in results", r.StageID)
		}
	}
}

func TestListTasksCombinedFilters(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := New()

	t1 := newTask("pipe-1", "stage-1")
	t1.State = broker.TaskStatePending

	t2 := newTask("pipe-1", "stage-1")
	t2.State = broker.TaskStateDone

	t3 := newTask("pipe-2", "stage-1")
	t3.State = broker.TaskStatePending

	_ = s.EnqueueTask(ctx, "stage-1", t1)
	_ = s.EnqueueTask(ctx, "stage-1", t2)
	_ = s.EnqueueTask(ctx, "stage-1", t3)

	// PipelineID + State
	pid := "pipe-1"
	state := broker.TaskStatePending
	result, err := s.ListTasks(ctx, broker.TaskFilter{PipelineID: &pid, State: &state})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Tasks) != 1 {
		t.Errorf("combined filter: got %d, want 1", len(result.Tasks))
	}
	if len(result.Tasks) > 0 && result.Tasks[0].ID != t1.ID {
		t.Errorf("combined filter: got task %s, want %s", result.Tasks[0].ID, t1.ID)
	}
}

func TestListTasksLimitOffset(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := New()

	// Enqueue 10 tasks with known IDs.
	ids := make([]string, 10)
	for i := 0; i < 10; i++ {
		task := newTask("pipe-page", "stage-page")
		task.ID = fmt.Sprintf("task-%02d", i)
		ids[i] = task.ID
		_ = s.EnqueueTask(ctx, "stage-page", task)
	}

	// Get all tasks to establish the full ordering.
	allResult, _ := s.ListTasks(ctx, broker.TaskFilter{})
	if len(allResult.Tasks) != 10 {
		t.Fatalf("got %d tasks, want 10", len(allResult.Tasks))
	}

	// Get page 1 (first 3).
	page1Result, _ := s.ListTasks(ctx, broker.TaskFilter{Limit: 3})
	if len(page1Result.Tasks) != 3 {
		t.Fatalf("page1: got %d, want 3", len(page1Result.Tasks))
	}

	// Get page 2 (offset 3, limit 3).
	page2Result, _ := s.ListTasks(ctx, broker.TaskFilter{Limit: 3, Offset: 3})
	if len(page2Result.Tasks) != 3 {
		t.Fatalf("page2: got %d, want 3", len(page2Result.Tasks))
	}

	// Pages must not overlap.
	page1IDs := make(map[string]bool)
	for _, task := range page1Result.Tasks {
		page1IDs[task.ID] = true
	}
	for _, task := range page2Result.Tasks {
		if page1IDs[task.ID] {
			t.Errorf("task %s appears in both page1 and page2", task.ID)
		}
	}

	// All pages together should cover all tasks.
	remainingResult, _ := s.ListTasks(ctx, broker.TaskFilter{Offset: 6})
	totalPaged := len(page1Result.Tasks) + len(page2Result.Tasks) + len(remainingResult.Tasks)
	if totalPaged != 10 {
		t.Errorf("paged total: got %d, want 10", totalPaged)
	}
}

// ---------- 6. Metadata isolation ----------

func TestMetadataIsolation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := New()

	task := newTask("pipe-iso", "stage-iso")
	task.Metadata = map[string]any{"key": "original"}

	_ = s.EnqueueTask(ctx, "stage-iso", task)

	// Mutate the caller's metadata map after enqueue.
	task.Metadata["key"] = "mutated"
	task.Metadata["injected"] = "bad"

	// The stored copy must be unaffected.
	got, err := s.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Metadata["key"] != "original" {
		t.Errorf("metadata key: got %v, want 'original' (shallow copy bug)", got.Metadata["key"])
	}
	if _, exists := got.Metadata["injected"]; exists {
		t.Errorf("metadata has injected key (shallow copy bug)")
	}

	// Also verify dequeue returns an isolated copy.
	dequeued, err := s.DequeueTask(ctx, "stage-iso")
	if err != nil {
		t.Fatal(err)
	}
	dequeued.Metadata["dequeue_mutation"] = true

	// Re-fetch: the store copy must not have the dequeue mutation.
	got2, err := s.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := got2.Metadata["dequeue_mutation"]; exists {
		t.Errorf("dequeued task mutation leaked back to store")
	}
}

func TestPayloadIsolation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := New()

	task := newTask("pipe-iso", "stage-iso")
	task.Payload = json.RawMessage(`{"original":true}`)

	_ = s.EnqueueTask(ctx, "stage-iso", task)

	// Mutate the caller's payload slice in place.
	copy(task.Payload, []byte(`{"HACKED!":tru`))

	got, err := s.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Payload) != `{"original":true}` {
		t.Errorf("payload: got %s, want original (shallow copy bug)", got.Payload)
	}
}

// ---------- 7. UpdateTask atomicity ----------

func TestUpdateTaskConcurrentUpdates(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := New()

	task := newTask("pipe-atomic", "stage-atomic")
	task.Attempts = 0
	_ = s.EnqueueTask(ctx, "stage-atomic", task)

	// Two goroutines each try to increment attempts 100 times.
	// Since the memory store uses a full mutex (not optimistic locking),
	// each UpdateTask call is atomic. However, read-modify-write across
	// separate GetTask + UpdateTask calls is NOT atomic.
	//
	// Known limitation: the memory store does not support optimistic locking
	// (e.g., version-based CAS). A caller doing GetTask → modify → UpdateTask
	// can lose updates if another goroutine updates in between. This is
	// acceptable for the in-memory dev/test store. Production stores (Redis,
	// Postgres) should use transactions or optimistic locking.

	const increments = 100
	var wg sync.WaitGroup
	wg.Add(2)

	increment := func() {
		defer wg.Done()
		for i := 0; i < increments; i++ {
			// This is a race-prone pattern (read-modify-write without CAS),
			// but individual UpdateTask calls are mutex-protected.
			got, err := s.GetTask(ctx, task.ID)
			if err != nil {
				t.Errorf("get: %v", err)
				return
			}
			newAttempts := got.Attempts + 1
			if err := s.UpdateTask(ctx, task.ID, broker.TaskUpdate{Attempts: &newAttempts}); err != nil {
				t.Errorf("update: %v", err)
				return
			}
		}
	}

	go increment()
	go increment()
	wg.Wait()

	got, _ := s.GetTask(ctx, task.ID)
	// With 2 goroutines × 100 increments, perfect serialization = 200.
	// Lost updates reduce this. We verify the store didn't crash and
	// the value is within the expected range.
	if got.Attempts < increments || got.Attempts > 2*increments {
		t.Errorf("attempts: got %d, want between %d and %d", got.Attempts, increments, 2*increments)
	}
	if got.Attempts < 2*increments {
		t.Logf("NOTE: %d lost updates detected (got %d, expected %d). "+
			"This is a known limitation: the memory store does not support optimistic locking. "+
			"Read-modify-write across separate GetTask/UpdateTask calls is not atomic.",
			2*increments-got.Attempts, got.Attempts, 2*increments)
	}
}

// Test that individual UpdateTask operations are serialized (no data corruption).
func TestUpdateTaskFieldsAreSerialized(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := New()

	task := newTask("pipe-serial", "stage-serial")
	_ = s.EnqueueTask(ctx, "stage-serial", task)

	const workers = 10
	var wg sync.WaitGroup
	wg.Add(workers)

	// Each worker sets a different metadata key.
	for w := 0; w < workers; w++ {
		go func(id int) {
			defer wg.Done()
			key := fmt.Sprintf("worker_%d", id)
			err := s.UpdateTask(ctx, task.ID, broker.TaskUpdate{
				Metadata: map[string]any{key: id},
			})
			if err != nil {
				t.Errorf("update from worker %d: %v", id, err)
			}
		}(w)
	}
	wg.Wait()

	got, _ := s.GetTask(ctx, task.ID)
	for w := 0; w < workers; w++ {
		key := fmt.Sprintf("worker_%d", w)
		if got.Metadata[key] != w {
			t.Errorf("metadata %s: got %v, want %d", key, got.Metadata[key], w)
		}
	}
}

// ---------- Additional edge cases ----------

func TestDequeueDoesNotReturnSameTaskTwice(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := New()

	task := newTask("pipe-1", "stage-1")
	_ = s.EnqueueTask(ctx, "stage-1", task)

	got1, err := s.DequeueTask(ctx, "stage-1")
	if err != nil {
		t.Fatal(err)
	}
	if got1.ID != task.ID {
		t.Errorf("first dequeue: got %s, want %s", got1.ID, task.ID)
	}

	_, err = s.DequeueTask(ctx, "stage-1")
	if err != store.ErrQueueEmpty {
		t.Errorf("second dequeue: got %v, want ErrQueueEmpty", err)
	}
}

func TestListTasksFilterByPipelineID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := New()

	t1 := newTask("pipe-alpha", "stage-1")
	t2 := newTask("pipe-beta", "stage-1")
	t3 := newTask("pipe-alpha", "stage-2")
	_ = s.EnqueueTask(ctx, "stage-1", t1)
	_ = s.EnqueueTask(ctx, "stage-1", t2)
	_ = s.EnqueueTask(ctx, "stage-2", t3)

	pid := "pipe-alpha"
	result, err := s.ListTasks(ctx, broker.TaskFilter{PipelineID: &pid})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Tasks) != 2 {
		t.Errorf("pipeline filter: got %d, want 2", len(result.Tasks))
	}
	for _, r := range result.Tasks {
		if r.PipelineID != "pipe-alpha" {
			t.Errorf("unexpected pipeline %s", r.PipelineID)
		}
	}
}

func TestListTasksFilterByState(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := New()

	tasks := []*broker.Task{
		newTask("pipe-1", "stage-1"),
		newTask("pipe-1", "stage-1"),
		newTask("pipe-1", "stage-1"),
	}
	tasks[0].State = broker.TaskStatePending
	tasks[1].State = broker.TaskStateExecuting
	tasks[2].State = broker.TaskStatePending

	for _, task := range tasks {
		_ = s.EnqueueTask(ctx, "stage-1", task)
	}

	state := broker.TaskStateExecuting
	result, err := s.ListTasks(ctx, broker.TaskFilter{State: &state})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Tasks) != 1 {
		t.Errorf("state filter: got %d, want 1", len(result.Tasks))
	}
}

// ---------- Concurrent multi-stage pipeline (stress) ----------

func TestConcurrentMultiStagePipeline(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := New()

	const totalTasks = 200
	allIDs := make([]string, totalTasks)

	// Enqueue all tasks.
	for i := 0; i < totalTasks; i++ {
		task := newTask("pipe-stress", "intake")
		allIDs[i] = task.ID
		if err := s.EnqueueTask(ctx, "intake", task); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
	}

	// Move through pipeline: intake → process → done
	moveStage := func(from, to string, workers int, markDone bool) {
		var wg sync.WaitGroup
		wg.Add(workers)
		for w := 0; w < workers; w++ {
			go func() {
				defer wg.Done()
				for {
					task, err := s.DequeueTask(ctx, from)
					if err == store.ErrQueueEmpty {
						return
					}
					if err != nil {
						t.Errorf("dequeue %s: %v", from, err)
						return
					}
					if markDone {
						state := broker.TaskStateDone
						_ = s.UpdateTask(ctx, task.ID, broker.TaskUpdate{State: &state})
					} else {
						_ = s.EnqueueTask(ctx, to, task)
					}
				}
			}()
		}
		wg.Wait()
	}

	moveStage("intake", "process", 5, false)
	moveStage("process", "", 3, true)

	// Verify all tasks are DONE.
	doneState := broker.TaskStateDone
	result, _ := s.ListTasks(ctx, broker.TaskFilter{State: &doneState})

	resultIDs := make(map[string]bool)
	for _, r := range result.Tasks {
		resultIDs[r.ID] = true
	}

	sort.Strings(allIDs)
	for _, id := range allIDs {
		if !resultIDs[id] {
			t.Errorf("task %s not in DONE state", id)
		}
	}
	if len(result.Tasks) != totalTasks {
		t.Errorf("DONE count: got %d, want %d", len(result.Tasks), totalTasks)
	}
}

// ---------- Enqueue with unique ID collision ----------

func TestEnqueueDuplicateIDOverwrites(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := New()

	id := uuid.New().String()
	t1 := newTask("pipe-1", "stage-1")
	t1.ID = id
	t1.Metadata = map[string]any{"version": 1}

	t2 := newTask("pipe-1", "stage-1")
	t2.ID = id
	t2.Metadata = map[string]any{"version": 2}

	_ = s.EnqueueTask(ctx, "stage-1", t1)
	_ = s.EnqueueTask(ctx, "stage-1", t2)

	got, _ := s.GetTask(ctx, id)
	if got.Metadata["version"] != 2 {
		t.Errorf("duplicate enqueue should overwrite: got version %v, want 2", got.Metadata["version"])
	}
}
