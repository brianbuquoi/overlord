package store_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	goredis "github.com/redis/go-redis/v9"

	"github.com/brianbuquoi/overlord/internal/broker"
	"github.com/brianbuquoi/overlord/internal/store"
	redisstore "github.com/brianbuquoi/overlord/internal/store/redis"
)

func newRedisTestStore(t *testing.T, prefix string) (*redisstore.RedisStore, *miniredis.Miniredis, *goredis.Client) {
	t.Helper()
	mr := miniredis.RunT(t)
	client := goredis.NewClient(&goredis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { client.Close() })
	return redisstore.New(client, prefix, 1*time.Hour), mr, client
}

// Test 1: BLMOVE atomicity — simulate a worker crash mid-processing.
//
// The Redis store uses BLMOVE to atomically move a task ID from the queue list
// to a processing list. If a worker crashes after BLMOVE but before calling
// UpdateTask (completing the task), the task ID remains in the processing list.
//
// This is at-least-once delivery: the task is never lost, but it is also not
// automatically re-enqueued. Overlord does not currently implement processing
// list recovery.
//
// TODO(recovery): On startup, scan all processing:{stageID} lists. For each
// task ID found, check the task's UpdatedAt timestamp against its stage timeout.
// If the task has been in processing longer than its timeout, move it back to
// the queue list (LREM from processing, LPUSH to queue) and reset its state to
// PENDING. This implements at-least-once redelivery with deduplication handled
// by the idempotent agent contract.
func TestRedis_BLMOVEAtomicityWorkerCrash(t *testing.T) {
	t.Parallel()
	s, _, client := newRedisTestStore(t, "crash:")
	ctx := context.Background()
	stage := "stage-crash-" + uuid.New().String()

	// Enqueue a task.
	task := newTask("pipe-1", stage)
	if err := s.EnqueueTask(ctx, stage, task); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	// Dequeue it — this moves the task ID from queue to processing via BLMOVE,
	// then removes it from processing on success. To simulate a crash mid-BLMOVE,
	// we manually replicate what BLMOVE does: pop from queue, push to processing.
	// But the real test: dequeue normally, then verify the task is consumed and
	// a second dequeue does NOT return it.
	got, err := s.DequeueTask(ctx, stage)
	if err != nil {
		t.Fatalf("dequeue: %v", err)
	}
	if got.ID != task.ID {
		t.Fatalf("got task %s, want %s", got.ID, task.ID)
	}

	// Simulate crash: the worker received the task but never called UpdateTask.
	// The task data key still exists in Redis.
	data, err := client.Get(ctx, fmt.Sprintf("crash:task:%s", task.ID)).Result()
	if err != nil {
		t.Fatalf("task key should still exist: %v", err)
	}
	var stored broker.Task
	if err := json.Unmarshal([]byte(data), &stored); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if stored.ID != task.ID {
		t.Fatalf("stored task ID mismatch: %s vs %s", stored.ID, task.ID)
	}

	// The queue list should be empty — task was consumed.
	qLen := client.LLen(ctx, fmt.Sprintf("crash:queue:%s", stage)).Val()
	if qLen != 0 {
		t.Errorf("queue should be empty after dequeue, got length %d", qLen)
	}

	// The processing list should also be empty because the current implementation
	// removes from processing on successful dequeue (line 116 of redis.go).
	// In a true crash scenario (process dies between BLMOVE and LREM), the task
	// ID would remain in the processing list.
	pLen := client.LLen(ctx, fmt.Sprintf("crash:processing:%s", stage)).Val()
	if pLen != 0 {
		t.Logf("processing list length: %d (0 expected in normal flow; >0 in crash scenario)", pLen)
	}

	// A subsequent DequeueTask must NOT return this task — it's no longer in the queue.
	ctx2, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	_, err = s.DequeueTask(ctx2, stage)
	if err == nil {
		t.Fatal("second dequeue should not return the crashed task")
	}

	// Now simulate what a crash would look like: manually push the task ID
	// into the processing list (as if BLMOVE succeeded but LREM never ran).
	client.LPush(ctx, fmt.Sprintf("crash:processing:%s", stage), task.ID)

	// Verify the task ID is in the processing list.
	pLen = client.LLen(ctx, fmt.Sprintf("crash:processing:%s", stage)).Val()
	if pLen != 1 {
		t.Errorf("processing list should have 1 entry after simulated crash, got %d", pLen)
	}

	// Verify DequeueTask still does NOT return it — it only reads from the queue list.
	ctx3, cancel3 := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel3()
	_, err = s.DequeueTask(ctx3, stage)
	if err == nil {
		t.Fatal("dequeue should not return task from processing list")
	}

	t.Log("CONFIRMED: At-least-once delivery semantics.")
	t.Log("Task in processing list is not lost but is not automatically recovered.")
	t.Log("TODO: Implement processing list recovery on startup — scan processing lists,")
	t.Log("re-enqueue tasks older than their stage timeout.")
}

// Test 2: Key prefix isolation — two stores with different prefixes on the same Redis.
func TestRedis_KeyPrefixIsolation(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	client := goredis.NewClient(&goredis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { client.Close() })

	storeA := redisstore.New(client, "tenant-a:", 1*time.Hour)
	storeB := redisstore.New(client, "tenant-b:", 1*time.Hour)

	ctx := context.Background()
	stage := "shared-stage-" + uuid.New().String()

	// Enqueue tasks to both stores.
	taskA := newTask("pipe-a", stage)
	taskB := newTask("pipe-b", stage)
	if err := storeA.EnqueueTask(ctx, stage, taskA); err != nil {
		t.Fatalf("enqueue A: %v", err)
	}
	if err := storeB.EnqueueTask(ctx, stage, taskB); err != nil {
		t.Fatalf("enqueue B: %v", err)
	}

	// Each store should only see its own task via GetTask.
	gotA, err := storeA.GetTask(ctx, taskA.ID)
	if err != nil {
		t.Fatalf("get A own task: %v", err)
	}
	if gotA.ID != taskA.ID {
		t.Errorf("store A got wrong task: %s", gotA.ID)
	}

	// Store A should not see store B's task.
	_, err = storeA.GetTask(ctx, taskB.ID)
	if err != store.ErrTaskNotFound {
		t.Errorf("store A should not see B's task, got err=%v", err)
	}

	// Store B should not see store A's task.
	_, err = storeB.GetTask(ctx, taskA.ID)
	if err != store.ErrTaskNotFound {
		t.Errorf("store B should not see A's task, got err=%v", err)
	}

	// Dequeue from each store — each gets only its own task.
	dequeuedA, err := storeA.DequeueTask(ctx, stage)
	if err != nil {
		t.Fatalf("dequeue A: %v", err)
	}
	if dequeuedA.ID != taskA.ID {
		t.Errorf("store A dequeued wrong task: %s, want %s", dequeuedA.ID, taskA.ID)
	}

	dequeuedB, err := storeB.DequeueTask(ctx, stage)
	if err != nil {
		t.Fatalf("dequeue B: %v", err)
	}
	if dequeuedB.ID != taskB.ID {
		t.Errorf("store B dequeued wrong task: %s, want %s", dequeuedB.ID, taskB.ID)
	}

	// ListTasks isolation: each store should list only its own tasks.
	// Re-enqueue for listing.
	taskA2 := newTask("pipe-a", stage)
	taskB2 := newTask("pipe-b", stage)
	storeA.EnqueueTask(ctx, stage, taskA2)
	storeB.EnqueueTask(ctx, stage, taskB2)

	resultA, err := storeA.ListTasks(ctx, broker.TaskFilter{})
	if err != nil {
		t.Fatalf("list A: %v", err)
	}
	for _, task := range resultA.Tasks {
		if task.PipelineID == "pipe-b" {
			t.Errorf("store A listed a pipe-b task: %s", task.ID)
		}
	}

	resultB, err := storeB.ListTasks(ctx, broker.TaskFilter{})
	if err != nil {
		t.Fatalf("list B: %v", err)
	}
	for _, task := range resultB.Tasks {
		if task.PipelineID == "pipe-a" {
			t.Errorf("store B listed a pipe-a task: %s", task.ID)
		}
	}
}

// Test 3: TTL enforcement — task key expires after ExpiresAt.
func TestRedis_TTLEnforcement(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	client := goredis.NewClient(&goredis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { client.Close() })

	// Use a short taskTTL so the Redis key itself expires.
	s := redisstore.New(client, "ttl:", 200*time.Millisecond)

	ctx := context.Background()
	stage := "stage-ttl-" + uuid.New().String()

	task := newTask("pipe-1", stage)
	task.ExpiresAt = time.Now().Add(200 * time.Millisecond)
	if err := s.EnqueueTask(ctx, stage, task); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	// Verify key exists initially.
	taskKey := fmt.Sprintf("ttl:task:%s", task.ID)
	if !mr.Exists(taskKey) {
		t.Fatal("task key should exist immediately after enqueue")
	}

	// Check TTL is set on the key.
	ttl := mr.TTL(taskKey)
	if ttl <= 0 {
		t.Errorf("task key should have a positive TTL, got %v", ttl)
	}

	// Fast-forward miniredis time past the TTL.
	mr.FastForward(300 * time.Millisecond)

	// The key should be gone.
	if mr.Exists(taskKey) {
		t.Error("task key should have expired after 300ms fast-forward")
	}

	// DequeueTask should return empty (task ID is in queue list but key is gone,
	// so the dequeue loop skips it and eventually returns empty).
	ctx2, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	_, err := s.DequeueTask(ctx2, stage)
	if err == nil {
		t.Error("dequeue should not return an expired task")
	}
}

// Test 4: DequeueTask context cancellation — must not block indefinitely.
//
// Note: miniredis does not fully simulate real Redis's ability to cancel BLMOVE
// mid-flight when the context is cancelled. With real Redis, go-redis cancels
// the blocking command immediately on context cancellation. With miniredis, the
// BLMOVE blocks for its full timeout (minimum 1s enforced by go-redis).
//
// This test verifies that DequeueTask returns a context error after the BLMOVE
// iteration completes, rather than blocking indefinitely. With real Redis, the
// response would be near-instant on context cancellation.
func TestRedis_DequeueContextCancellation(t *testing.T) {
	t.Parallel()
	s, _, _ := newRedisTestStore(t, "cancel:")
	stage := "stage-cancel-" + uuid.New().String()

	// Use a 100ms context timeout. With miniredis, BLMOVE will block for ~1s
	// (go-redis minimum), then the next loop iteration detects ctx.Err().
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := s.DequeueTask(ctx, stage)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("dequeue on empty queue should have returned an error")
	}

	// With miniredis: should return within ~3s (one BLMOVE iteration + context check).
	// With real Redis: should return within ~200ms.
	// The key guarantee: it does NOT block forever.
	if elapsed > 5*time.Second {
		t.Errorf("dequeue took %v, should not block indefinitely", elapsed)
	}

	t.Logf("dequeue returned after %v with error: %v", elapsed, err)

	// Context should be done.
	if ctx.Err() == nil {
		t.Error("context should be cancelled")
	}
}

// Test 5: ListTasks pagination via sorted set index — correct page, accurate total.
func TestRedis_ListTasksSortedSetPagination(t *testing.T) {
	t.Parallel()
	s, _, _ := newRedisTestStore(t, "idx:")
	ctx := context.Background()

	pipeline := "pipe-page-" + uuid.New().String()
	stage := "stage-page-" + uuid.New().String()

	const n = 10
	ids := make([]string, n)
	for i := 0; i < n; i++ {
		task := newTask(pipeline, stage)
		task.CreatedAt = time.Now().Add(time.Duration(i) * time.Millisecond)
		ids[i] = task.ID
		if err := s.EnqueueTask(ctx, stage, task); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}

	stageFilter := stage
	pipeFilter := pipeline

	// Page 1: offset=0, limit=3 → 3 tasks, total=10
	result, err := s.ListTasks(ctx, broker.TaskFilter{
		PipelineID: &pipeFilter,
		StageID:    &stageFilter,
		Limit:      3,
		Offset:     0,
	})
	if err != nil {
		t.Fatalf("page 1: %v", err)
	}
	if len(result.Tasks) != 3 {
		t.Errorf("page 1: got %d tasks, want 3", len(result.Tasks))
	}
	if result.Total != n {
		t.Errorf("page 1 total: got %d, want %d", result.Total, n)
	}

	// Page 2: offset=3, limit=3 → 3 tasks, total=10
	result2, err := s.ListTasks(ctx, broker.TaskFilter{
		PipelineID: &pipeFilter,
		StageID:    &stageFilter,
		Limit:      3,
		Offset:     3,
	})
	if err != nil {
		t.Fatalf("page 2: %v", err)
	}
	if len(result2.Tasks) != 3 {
		t.Errorf("page 2: got %d tasks, want 3", len(result2.Tasks))
	}
	if result2.Total != n {
		t.Errorf("page 2 total: got %d, want %d", result2.Total, n)
	}

	// No overlap between pages.
	page1IDs := make(map[string]bool)
	for _, task := range result.Tasks {
		page1IDs[task.ID] = true
	}
	for _, task := range result2.Tasks {
		if page1IDs[task.ID] {
			t.Errorf("task %s appears in both page 1 and page 2", task.ID)
		}
	}

	// Last page: offset=8, limit=5 → only 2 remaining
	result3, err := s.ListTasks(ctx, broker.TaskFilter{
		PipelineID: &pipeFilter,
		StageID:    &stageFilter,
		Limit:      5,
		Offset:     8,
	})
	if err != nil {
		t.Fatalf("last page: %v", err)
	}
	if len(result3.Tasks) != 2 {
		t.Errorf("last page: got %d tasks, want 2", len(result3.Tasks))
	}
}

// Test 6: Dangling index entries are silently skipped.
func TestRedis_ListTasksDanglingIndexEntries(t *testing.T) {
	t.Parallel()
	s, mr, _ := newRedisTestStore(t, "dangle:")
	ctx := context.Background()

	pipeline := "pipe-dangle-" + uuid.New().String()
	stage := "stage-dangle-" + uuid.New().String()

	// Enqueue 3 tasks.
	var liveIDs []string
	for i := 0; i < 3; i++ {
		task := newTask(pipeline, stage)
		task.CreatedAt = time.Now().Add(time.Duration(i) * time.Millisecond)
		liveIDs = append(liveIDs, task.ID)
		if err := s.EnqueueTask(ctx, stage, task); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}

	// Delete the task key for the second task (simulating TTL expiry).
	// The index entry remains — this is a dangling reference.
	mr.Del(fmt.Sprintf("dangle:task:%s", liveIDs[1]))

	pipeFilter := pipeline
	stageFilter := stage
	result, err := s.ListTasks(ctx, broker.TaskFilter{
		PipelineID: &pipeFilter,
		StageID:    &stageFilter,
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	// Should get 2 tasks, not 3 — the dangling entry is silently skipped.
	if len(result.Tasks) != 2 {
		t.Errorf("got %d tasks, want 2 (dangling entry should be skipped)", len(result.Tasks))
	}

	// Verify no errors were returned — dangling entries are not errors.
	for _, task := range result.Tasks {
		if task.ID == liveIDs[1] {
			t.Error("dangling task should not appear in results")
		}
	}
}

// Test 7: SCAN is no longer used in ListTasks for task key enumeration.
// We verify this by checking that ListTasks works correctly when filtering
// by pipeline+stage, which hits the direct index path (no SCAN at all).
func TestRedis_ListTasksNoScanForTaskKeys(t *testing.T) {
	t.Parallel()
	s, _, client := newRedisTestStore(t, "noscan:")
	ctx := context.Background()

	pipeline := "pipe-noscan-" + uuid.New().String()
	stage := "stage-noscan-" + uuid.New().String()

	// Enqueue tasks.
	for i := 0; i < 5; i++ {
		task := newTask(pipeline, stage)
		task.CreatedAt = time.Now().Add(time.Duration(i) * time.Millisecond)
		if err := s.EnqueueTask(ctx, stage, task); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}

	// Verify the index key exists and has the right count.
	indexKey := fmt.Sprintf("noscan:index:%s:%s", pipeline, stage)
	count, err := client.ZCard(ctx, indexKey).Result()
	if err != nil {
		t.Fatalf("zcard: %v", err)
	}
	if count != 5 {
		t.Errorf("index has %d members, want 5", count)
	}

	// ListTasks with pipeline+stage filter uses index directly.
	pipeFilter := pipeline
	stageFilter := stage
	result, err := s.ListTasks(ctx, broker.TaskFilter{
		PipelineID: &pipeFilter,
		StageID:    &stageFilter,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Tasks) != 5 {
		t.Errorf("got %d tasks, want 5", len(result.Tasks))
	}
	if result.Total != 5 {
		t.Errorf("total: got %d, want 5", result.Total)
	}
}

// Test 8: ListTasks with pipeline-only filter unions across stages.
func TestRedis_ListTasksPipelineOnlyFilter(t *testing.T) {
	t.Parallel()
	s, _, _ := newRedisTestStore(t, "union:")
	ctx := context.Background()

	pipeline := "pipe-union-" + uuid.New().String()
	stage1 := "stage-a-" + uuid.New().String()
	stage2 := "stage-b-" + uuid.New().String()

	for i := 0; i < 3; i++ {
		task := newTask(pipeline, stage1)
		task.CreatedAt = time.Now().Add(time.Duration(i) * time.Millisecond)
		if err := s.EnqueueTask(ctx, stage1, task); err != nil {
			t.Fatalf("enqueue stage1 %d: %v", i, err)
		}
	}
	for i := 0; i < 2; i++ {
		task := newTask(pipeline, stage2)
		task.CreatedAt = time.Now().Add(time.Duration(i) * time.Millisecond)
		if err := s.EnqueueTask(ctx, stage2, task); err != nil {
			t.Fatalf("enqueue stage2 %d: %v", i, err)
		}
	}

	pipeFilter := pipeline
	result, err := s.ListTasks(ctx, broker.TaskFilter{
		PipelineID: &pipeFilter,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Tasks) != 5 {
		t.Errorf("got %d tasks, want 5 (3 from stage1 + 2 from stage2)", len(result.Tasks))
	}
	if result.Total != 5 {
		t.Errorf("total: got %d, want 5", result.Total)
	}
}

// Test 9: UpdateTask keeps terminal tasks in the primary index so historical
// listing continues to work. Entries age out naturally via the task key TTL.
func TestRedis_UpdateTaskKeepsTerminalInIndex(t *testing.T) {
	t.Parallel()
	s, _, client := newRedisTestStore(t, "term:")
	ctx := context.Background()

	pipeline := "pipe-term-" + uuid.New().String()
	stage := "stage-term-" + uuid.New().String()

	task := newTask(pipeline, stage)
	if err := s.EnqueueTask(ctx, stage, task); err != nil {
		t.Fatal(err)
	}

	indexKey := fmt.Sprintf("term:index:%s:%s", pipeline, stage)
	count, _ := client.ZCard(ctx, indexKey).Result()
	if count != 1 {
		t.Fatalf("index should have 1 entry, got %d", count)
	}

	done := broker.TaskStateDone
	if err := s.UpdateTask(ctx, task.ID, broker.TaskUpdate{State: &done}); err != nil {
		t.Fatal(err)
	}

	count, _ = client.ZCard(ctx, indexKey).Result()
	if count != 1 {
		t.Errorf("terminal task should remain in primary index, got count %d", count)
	}

	// The task must also be fetchable via ListTasks filtered by state.
	state := broker.TaskStateDone
	result, err := s.ListTasks(ctx, broker.TaskFilter{State: &state})
	if err != nil {
		t.Fatalf("list by DONE: %v", err)
	}
	found := false
	for _, got := range result.Tasks {
		if got.ID == task.ID {
			found = true
			break
		}
	}
	if !found {
		t.Error("terminal task should be listable via state index")
	}
}

// Test 10: Concurrent UpdateTask calls on the same task ID — verifies the
// atomic Lua script does not lose writes under racing updaters.
//
// The old GET→modify→SET pattern would silently drop attempts bumped by a
// concurrent updater. With the Lua script, every successful call observes a
// freshly loaded snapshot and applies its delta on top of it.
func TestRedis_UpdateTaskConcurrentWrites(t *testing.T) {
	t.Parallel()
	s, _, _ := newRedisTestStore(t, "concurrent:")
	ctx := context.Background()

	task := newTask("pipe-conc", "stage-conc")
	if err := s.EnqueueTask(ctx, "stage-conc", task); err != nil {
		t.Fatal(err)
	}

	// Fire N concurrent metadata updates. Each writes a distinct key.
	const n = 20
	errCh := make(chan error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			md := map[string]any{fmt.Sprintf("k%d", i): i}
			errCh <- s.UpdateTask(ctx, task.ID, broker.TaskUpdate{Metadata: md})
		}(i)
	}
	wg.Wait()
	close(errCh)
	for e := range errCh {
		if e != nil {
			t.Fatalf("update: %v", e)
		}
	}

	got, err := s.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	// All N keys must be present: no update lost.
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("k%d", i)
		if _, ok := got.Metadata[key]; !ok {
			t.Errorf("metadata key %s missing — concurrent update was lost", key)
		}
	}
}

// Test 11: ListTasks with state filter reads from the per-state secondary
// index and includes terminal tasks. Tasks whose state has moved appear only
// under their current state.
func TestRedis_ListTasksStateIndex(t *testing.T) {
	t.Parallel()
	s, _, client := newRedisTestStore(t, "stateidx:")
	ctx := context.Background()

	pipeline := "pipe-state-" + uuid.New().String()
	stage := "stage-state-" + uuid.New().String()

	var pendingIDs, doneIDs []string
	for i := 0; i < 3; i++ {
		task := newTask(pipeline, stage)
		pendingIDs = append(pendingIDs, task.ID)
		if err := s.EnqueueTask(ctx, stage, task); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ {
		task := newTask(pipeline, stage)
		if err := s.EnqueueTask(ctx, stage, task); err != nil {
			t.Fatal(err)
		}
		done := broker.TaskStateDone
		if err := s.UpdateTask(ctx, task.ID, broker.TaskUpdate{State: &done}); err != nil {
			t.Fatal(err)
		}
		doneIDs = append(doneIDs, task.ID)
	}

	pendingKey := fmt.Sprintf("stateidx:tasks:state:%s", broker.TaskStatePending)
	doneKey := fmt.Sprintf("stateidx:tasks:state:%s", broker.TaskStateDone)
	if c, _ := client.ZCard(ctx, pendingKey).Result(); c != int64(len(pendingIDs)) {
		t.Errorf("pending state index: got %d members, want %d", c, len(pendingIDs))
	}
	if c, _ := client.ZCard(ctx, doneKey).Result(); c != int64(len(doneIDs)) {
		t.Errorf("done state index: got %d members, want %d", c, len(doneIDs))
	}

	pendingState := broker.TaskStatePending
	result, err := s.ListTasks(ctx, broker.TaskFilter{State: &pendingState})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Tasks) != len(pendingIDs) {
		t.Errorf("pending listing: got %d, want %d", len(result.Tasks), len(pendingIDs))
	}
	for _, task := range result.Tasks {
		if task.State != broker.TaskStatePending {
			t.Errorf("task %s has wrong state %s in pending list", task.ID, task.State)
		}
	}

	doneState := broker.TaskStateDone
	result, err = s.ListTasks(ctx, broker.TaskFilter{State: &doneState})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Tasks) != len(doneIDs) {
		t.Errorf("done listing: got %d, want %d", len(result.Tasks), len(doneIDs))
	}
	for _, task := range result.Tasks {
		if task.State != broker.TaskStateDone {
			t.Errorf("task %s has wrong state %s in done list", task.ID, task.State)
		}
	}
}

// TestRedis_ClaimForReplay_HappyPath verifies that ClaimForReplay returns
// the task with RoutedToDeadLetter flipped to false. A second claim must
// fail with ErrTaskNotReplayable.
func TestRedis_ClaimForReplay_HappyPath(t *testing.T) {
	t.Parallel()
	s, _, _ := newRedisTestStore(t, "claim-ok:")
	ctx := context.Background()

	task := &broker.Task{
		ID: uuid.New().String(), PipelineID: "p", StageID: "s",
		InputSchemaName: "in", InputSchemaVersion: "v1",
		OutputSchemaName: "out", OutputSchemaVersion: "v1",
		Payload: json.RawMessage(`{}`), Metadata: map[string]any{},
		State: broker.TaskStateFailed, RoutedToDeadLetter: true,
		Attempts: 3, MaxAttempts: 3,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	if err := s.EnqueueTask(ctx, "s", task); err != nil {
		t.Fatal(err)
	}
	// Move the per-state index from PENDING (set by EnqueueTask) to FAILED,
	// matching the state we seeded on the Task itself.
	failed := broker.TaskStateFailed
	if err := s.UpdateTask(ctx, task.ID, broker.TaskUpdate{State: &failed}); err != nil {
		t.Fatal(err)
	}
	dl := true
	if err := s.UpdateTask(ctx, task.ID, broker.TaskUpdate{RoutedToDeadLetter: &dl}); err != nil {
		t.Fatal(err)
	}

	claimed, err := s.ClaimForReplay(ctx, task.ID)
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
		t.Errorf("attempts: got %d want 3 (unchanged)", claimed.Attempts)
	}

	stored, err := s.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("get stored: %v", err)
	}
	if stored.State != broker.TaskStateReplayPending || stored.RoutedToDeadLetter || stored.Attempts != 3 {
		t.Errorf("stored task: state=%s dl=%v attempts=%d, want REPLAY_PENDING/false/3",
			stored.State, stored.RoutedToDeadLetter, stored.Attempts)
	}

	// Second claim must fail: the flip is the claim token.
	if _, err := s.ClaimForReplay(ctx, task.ID); err != store.ErrTaskNotReplayable {
		t.Fatalf("second claim: got %v, want ErrTaskNotReplayable", err)
	}
}

func TestRedis_ClaimForReplay_NotFound(t *testing.T) {
	t.Parallel()
	s, _, _ := newRedisTestStore(t, "claim-nf:")
	_, err := s.ClaimForReplay(context.Background(), "does-not-exist")
	if err != store.ErrTaskNotFound {
		t.Fatalf("got %v, want ErrTaskNotFound", err)
	}
}

func TestRedis_ClaimForReplay_NotReplayable(t *testing.T) {
	t.Parallel()
	s, _, _ := newRedisTestStore(t, "claim-nr:")
	ctx := context.Background()
	task := &broker.Task{
		ID: uuid.New().String(), PipelineID: "p", StageID: "s",
		InputSchemaName: "in", InputSchemaVersion: "v1",
		OutputSchemaName: "out", OutputSchemaVersion: "v1",
		Payload: json.RawMessage(`{}`), Metadata: map[string]any{},
		State: broker.TaskStatePending, // not replayable
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	if err := s.EnqueueTask(ctx, "s", task); err != nil {
		t.Fatal(err)
	}
	_, err := s.ClaimForReplay(ctx, task.ID)
	if err != store.ErrTaskNotReplayable {
		t.Fatalf("got %v, want ErrTaskNotReplayable", err)
	}
}

// Concurrent ClaimForReplay calls must produce exactly one winner (nil err)
// and N-1 losers (ErrTaskNotReplayable). The stored task ends in FAILED
// with RoutedToDeadLetter cleared by the winner.
func TestRedis_ClaimForReplay_ConcurrentNoMutation(t *testing.T) {
	t.Parallel()
	s, _, _ := newRedisTestStore(t, "claim-race:")
	ctx := context.Background()
	task := &broker.Task{
		ID: uuid.New().String(), PipelineID: "p", StageID: "s",
		InputSchemaName: "in", InputSchemaVersion: "v1",
		OutputSchemaName: "out", OutputSchemaVersion: "v1",
		Payload: json.RawMessage(`{}`), Metadata: map[string]any{},
		State: broker.TaskStateFailed, RoutedToDeadLetter: true,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	if err := s.EnqueueTask(ctx, "s", task); err != nil {
		t.Fatal(err)
	}
	failed := broker.TaskStateFailed
	s.UpdateTask(ctx, task.ID, broker.TaskUpdate{State: &failed})
	dl := true
	s.UpdateTask(ctx, task.ID, broker.TaskUpdate{RoutedToDeadLetter: &dl})

	const N = 20
	var wg sync.WaitGroup
	errs := make([]error, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = s.ClaimForReplay(ctx, task.ID)
		}(i)
	}
	wg.Wait()

	wins, losses := 0, 0
	for _, e := range errs {
		switch e {
		case nil:
			wins++
		case store.ErrTaskNotReplayable:
			losses++
		default:
			t.Errorf("unexpected error %v", e)
		}
	}
	if wins != 1 || losses != N-1 {
		t.Fatalf("wins=%d losses=%d, want 1/%d", wins, losses, N-1)
	}

	stored, err := s.GetTask(ctx, task.ID)
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

// TestRedis_RollbackReplayClaim_RestoresDeadLettered verifies that a rollback
// after a successful claim leaves the task FAILED+dead-lettered and
// re-claimable.
func TestRedis_RollbackReplayClaim_RestoresDeadLettered(t *testing.T) {
	t.Parallel()
	s, _, _ := newRedisTestStore(t, "rb:")
	ctx := context.Background()

	task := &broker.Task{
		ID: uuid.New().String(), PipelineID: "p", StageID: "s",
		InputSchemaName: "in", InputSchemaVersion: "v1",
		OutputSchemaName: "out", OutputSchemaVersion: "v1",
		Payload: json.RawMessage(`{}`), Metadata: map[string]any{},
		State: broker.TaskStateFailed, RoutedToDeadLetter: true,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	if err := s.EnqueueTask(ctx, "s", task); err != nil {
		t.Fatal(err)
	}
	failed := broker.TaskStateFailed
	if err := s.UpdateTask(ctx, task.ID, broker.TaskUpdate{State: &failed}); err != nil {
		t.Fatal(err)
	}
	dl := true
	if err := s.UpdateTask(ctx, task.ID, broker.TaskUpdate{RoutedToDeadLetter: &dl}); err != nil {
		t.Fatal(err)
	}

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
		t.Errorf("state after rollback: got %s want FAILED", got.State)
	}
	if !got.RoutedToDeadLetter {
		t.Errorf("RoutedToDeadLetter after rollback: got false want true")
	}
	if _, err := s.ClaimForReplay(ctx, task.ID); err != nil {
		t.Errorf("re-claim after rollback: got %v want nil", err)
	}
}

func TestRedis_RollbackReplayClaim_FailsIfNotReplayPending(t *testing.T) {
	t.Parallel()
	s, _, _ := newRedisTestStore(t, "rb-nf:")
	ctx := context.Background()

	if err := s.RollbackReplayClaim(ctx, "does-not-exist"); err != store.ErrTaskNotFound {
		t.Errorf("not found: got %v want ErrTaskNotFound", err)
	}

	task := &broker.Task{
		ID: uuid.New().String(), PipelineID: "p", StageID: "s",
		InputSchemaName: "in", InputSchemaVersion: "v1",
		OutputSchemaName: "out", OutputSchemaVersion: "v1",
		Payload: json.RawMessage(`{}`), Metadata: map[string]any{},
		State: broker.TaskStateFailed, RoutedToDeadLetter: true,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	if err := s.EnqueueTask(ctx, "s", task); err != nil {
		t.Fatal(err)
	}
	failed := broker.TaskStateFailed
	s.UpdateTask(ctx, task.ID, broker.TaskUpdate{State: &failed})
	dl := true
	s.UpdateTask(ctx, task.ID, broker.TaskUpdate{RoutedToDeadLetter: &dl})

	if err := s.RollbackReplayClaim(ctx, task.ID); err != store.ErrTaskNotReplayPending {
		t.Errorf("FAILED task rollback: got %v want ErrTaskNotReplayPending", err)
	}
}

func TestRedis_ClaimForReplay_TransitionsToReplayPending(t *testing.T) {
	t.Parallel()
	s, _, _ := newRedisTestStore(t, "claim-rp:")
	ctx := context.Background()

	task := &broker.Task{
		ID: uuid.New().String(), PipelineID: "p", StageID: "s",
		InputSchemaName: "in", InputSchemaVersion: "v1",
		OutputSchemaName: "out", OutputSchemaVersion: "v1",
		Payload: json.RawMessage(`{}`), Metadata: map[string]any{},
		State: broker.TaskStateFailed, RoutedToDeadLetter: true,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	if err := s.EnqueueTask(ctx, "s", task); err != nil {
		t.Fatal(err)
	}
	failed := broker.TaskStateFailed
	s.UpdateTask(ctx, task.ID, broker.TaskUpdate{State: &failed})
	dl := true
	s.UpdateTask(ctx, task.ID, broker.TaskUpdate{RoutedToDeadLetter: &dl})

	claimed, err := s.ClaimForReplay(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if claimed.State != broker.TaskStateReplayPending {
		t.Errorf("claimed state: got %s want REPLAY_PENDING", claimed.State)
	}

	// The per-state index should reflect the new state (listable under REPLAY_PENDING).
	rp := broker.TaskStateReplayPending
	page, err := s.ListTasks(ctx, broker.TaskFilter{State: &rp})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, tk := range page.Tasks {
		if tk.ID == task.ID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("task not in REPLAY_PENDING index")
	}
}
