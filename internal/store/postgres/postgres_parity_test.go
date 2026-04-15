//go:build integration

package postgres

import (
	"context"
	"encoding/json"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/brianbuquoi/overlord/internal/broker"
	"github.com/brianbuquoi/overlord/internal/store"
)

// setupParityTest spins up a dedicated table per test so the Postgres-parity
// assertions run in isolation. The table schema mirrors migrations/001 plus
// migrations/003 (the columns introduced for store-contract parity).
func setupParityTest(t *testing.T) (*PostgresStore, *pgxpool.Pool, string) {
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

	table := "overlord_parity_" + sanitizeID(uuid.New().String()[:8])
	_, err = pool.Exec(ctx, `CREATE TABLE `+table+` (
		id                      TEXT PRIMARY KEY,
		pipeline_id             TEXT NOT NULL,
		stage_id                TEXT NOT NULL,
		input_schema_name       TEXT NOT NULL DEFAULT '',
		input_schema_version    TEXT NOT NULL DEFAULT '',
		output_schema_name      TEXT NOT NULL DEFAULT '',
		output_schema_version   TEXT NOT NULL DEFAULT '',
		payload                 JSONB,
		metadata                JSONB,
		state                   TEXT NOT NULL DEFAULT 'PENDING',
		attempts                INTEGER NOT NULL DEFAULT 0,
		max_attempts            INTEGER NOT NULL DEFAULT 1,
		created_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
		updated_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
		expires_at              TIMESTAMPTZ,
		routed_to_dead_letter   BOOLEAN NOT NULL DEFAULT FALSE,
		cross_stage_transitions INTEGER NOT NULL DEFAULT 0
	)`)
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
	t.Cleanup(func() {
		pool.Exec(context.Background(), "DROP TABLE IF EXISTS "+table)
	})

	s, err := New(pool, table)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	return s, pool, table
}

func sanitizeID(raw string) string {
	out := make([]byte, 0, len(raw))
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if c == '-' {
			out = append(out, '_')
		} else {
			out = append(out, c)
		}
	}
	return string(out)
}

func parityTask(id, pipeline, stage string, state broker.TaskState) *broker.Task {
	now := time.Now()
	return &broker.Task{
		ID:                  id,
		PipelineID:          pipeline,
		StageID:             stage,
		InputSchemaName:     "in",
		InputSchemaVersion:  "v1",
		OutputSchemaName:    "out",
		OutputSchemaVersion: "v1",
		Payload:             json.RawMessage(`{"k":"v"}`),
		Metadata:            map[string]any{},
		State:               state,
		Attempts:            0,
		MaxAttempts:         3,
		CreatedAt:           now,
		UpdatedAt:           now,
	}
}

// TestPostgres_ClaimForReplay_RequiresDeadLettered asserts the contract:
// a FAILED task must ALSO have RoutedToDeadLetter = true to be replayable.
// Postgres previously ignored the dead-letter flag and accepted any FAILED
// task, which diverged from Redis and Memory.
func TestPostgres_ClaimForReplay_RequiresDeadLettered(t *testing.T) {
	s, _, _ := setupParityTest(t)
	ctx := context.Background()

	// FAILED but NOT dead-lettered: must be rejected.
	plain := parityTask(uuid.New().String(), "p1", "s1", broker.TaskStateFailed)
	if err := s.EnqueueTask(ctx, "s1", plain); err != nil {
		t.Fatalf("enqueue plain: %v", err)
	}
	if _, err := s.ClaimForReplay(ctx, plain.ID); err != store.ErrTaskNotReplayable {
		t.Fatalf("plain FAILED: got %v, want ErrTaskNotReplayable", err)
	}

	// FAILED + dead-lettered: must succeed and leave the task untouched.
	dl := parityTask(uuid.New().String(), "p1", "s1", broker.TaskStateFailed)
	dl.RoutedToDeadLetter = true
	dl.Attempts = 5
	if err := s.EnqueueTask(ctx, "s1", dl); err != nil {
		t.Fatalf("enqueue dl: %v", err)
	}
	claimed, err := s.ClaimForReplay(ctx, dl.ID)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if claimed.State != broker.TaskStateFailed {
		t.Errorf("returned state: got %s want FAILED (unchanged)", claimed.State)
	}
	if claimed.RoutedToDeadLetter {
		t.Error("returned RoutedToDeadLetter should be cleared by the claim")
	}
	if claimed.Attempts != 5 {
		t.Errorf("returned Attempts: got %d want 5", claimed.Attempts)
	}

	// Reread the stored row: RoutedToDeadLetter is now false.
	stored, err := s.GetTask(ctx, dl.ID)
	if err != nil {
		t.Fatalf("get stored: %v", err)
	}
	if stored.State != broker.TaskStateFailed || stored.RoutedToDeadLetter || stored.Attempts != 5 {
		t.Errorf("stored: state=%s dl=%v attempts=%d, want FAILED/false/5",
			stored.State, stored.RoutedToDeadLetter, stored.Attempts)
	}

	// A second claim on the same task must fail: the flip is the claim token.
	if _, err := s.ClaimForReplay(ctx, dl.ID); err != store.ErrTaskNotReplayable {
		t.Fatalf("second claim: got %v, want ErrTaskNotReplayable", err)
	}
}

// TestPostgres_ClaimForReplay_Concurrent ensures that N concurrent claims on
// the same task produce exactly one winner. The losing callers receive
// ErrTaskNotReplayable once the winner's UPDATE commits.
func TestPostgres_ClaimForReplay_Concurrent(t *testing.T) {
	s, _, _ := setupParityTest(t)
	ctx := context.Background()

	task := parityTask(uuid.New().String(), "p1", "s1", broker.TaskStateFailed)
	task.RoutedToDeadLetter = true
	if err := s.EnqueueTask(ctx, "s1", task); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	const N = 20
	errs := make([]error, N)
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = s.ClaimForReplay(ctx, task.ID)
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

	stored, err := s.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("get stored: %v", err)
	}
	if stored.State != broker.TaskStateFailed {
		t.Errorf("stored state: got %s want FAILED", stored.State)
	}
	if stored.RoutedToDeadLetter {
		t.Error("stored RoutedToDeadLetter should be cleared by the winner")
	}
}

// TestPostgres_UpdateTask_RoutedToDeadLetter asserts that UpdateTask persists
// the RoutedToDeadLetter flag. Previously, this field was silently dropped
// by the UPDATE statement.
func TestPostgres_UpdateTask_RoutedToDeadLetter(t *testing.T) {
	s, _, _ := setupParityTest(t)
	ctx := context.Background()
	task := parityTask(uuid.New().String(), "p1", "s1", broker.TaskStateFailed)
	if err := s.EnqueueTask(ctx, "s1", task); err != nil {
		t.Fatal(err)
	}

	dl := true
	if err := s.UpdateTask(ctx, task.ID, broker.TaskUpdate{RoutedToDeadLetter: &dl}); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, err := s.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !got.RoutedToDeadLetter {
		t.Fatal("RoutedToDeadLetter did not persist")
	}

	// Clearing the flag must also persist.
	off := false
	if err := s.UpdateTask(ctx, task.ID, broker.TaskUpdate{RoutedToDeadLetter: &off}); err != nil {
		t.Fatalf("clear: %v", err)
	}
	got, err = s.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("get after clear: %v", err)
	}
	if got.RoutedToDeadLetter {
		t.Fatal("RoutedToDeadLetter still set after clear")
	}
}

// TestPostgres_UpdateTask_CrossStageTransitions asserts counter persistence.
func TestPostgres_UpdateTask_CrossStageTransitions(t *testing.T) {
	s, _, _ := setupParityTest(t)
	ctx := context.Background()
	task := parityTask(uuid.New().String(), "p1", "s1", broker.TaskStatePending)
	if err := s.EnqueueTask(ctx, "s1", task); err != nil {
		t.Fatal(err)
	}

	cnt := 4
	if err := s.UpdateTask(ctx, task.ID, broker.TaskUpdate{CrossStageTransitions: &cnt}); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, err := s.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.CrossStageTransitions != 4 {
		t.Fatalf("CrossStageTransitions: got %d want 4", got.CrossStageTransitions)
	}
}

// TestPostgres_ListTasks_RoutedToDeadLetterFilter asserts that the filter
// restricts results to tasks whose RoutedToDeadLetter flag matches.
func TestPostgres_ListTasks_RoutedToDeadLetterFilter(t *testing.T) {
	s, _, _ := setupParityTest(t)
	ctx := context.Background()

	// 3 dead-lettered + 2 regular failed tasks.
	var dlIDs []string
	for i := 0; i < 3; i++ {
		tk := parityTask(uuid.New().String(), "p1", "s1", broker.TaskStateFailed)
		tk.RoutedToDeadLetter = true
		if err := s.EnqueueTask(ctx, "s1", tk); err != nil {
			t.Fatal(err)
		}
		dlIDs = append(dlIDs, tk.ID)
	}
	for i := 0; i < 2; i++ {
		tk := parityTask(uuid.New().String(), "p1", "s1", broker.TaskStateFailed)
		if err := s.EnqueueTask(ctx, "s1", tk); err != nil {
			t.Fatal(err)
		}
	}

	on := true
	page, err := s.ListTasks(ctx, broker.TaskFilter{RoutedToDeadLetter: &on})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if page.Total != 3 {
		t.Fatalf("total: got %d want 3", page.Total)
	}
	got := make(map[string]bool, len(page.Tasks))
	for _, tk := range page.Tasks {
		got[tk.ID] = true
		if !tk.RoutedToDeadLetter {
			t.Errorf("task %s: RoutedToDeadLetter=false, should be true", tk.ID)
		}
	}
	for _, id := range dlIDs {
		if !got[id] {
			t.Errorf("expected dead-lettered task %s in results", id)
		}
	}

	off := false
	page, err = s.ListTasks(ctx, broker.TaskFilter{RoutedToDeadLetter: &off})
	if err != nil {
		t.Fatalf("list off: %v", err)
	}
	if page.Total != 2 {
		t.Fatalf("total off: got %d want 2", page.Total)
	}
}

// TestPostgres_ListTasks_IncludeDiscarded asserts that DISCARDED tasks are
// hidden by default and surfaced when IncludeDiscarded is set.
func TestPostgres_ListTasks_IncludeDiscarded(t *testing.T) {
	s, _, _ := setupParityTest(t)
	ctx := context.Background()

	live := parityTask(uuid.New().String(), "p1", "s1", broker.TaskStatePending)
	if err := s.EnqueueTask(ctx, "s1", live); err != nil {
		t.Fatal(err)
	}
	discarded := parityTask(uuid.New().String(), "p1", "s1", broker.TaskStateDiscarded)
	if err := s.EnqueueTask(ctx, "s1", discarded); err != nil {
		t.Fatal(err)
	}

	// Default: discarded hidden.
	page, err := s.ListTasks(ctx, broker.TaskFilter{})
	if err != nil {
		t.Fatalf("list default: %v", err)
	}
	if page.Total != 1 {
		t.Fatalf("default total: got %d want 1", page.Total)
	}
	if page.Tasks[0].ID != live.ID {
		t.Errorf("default result: got %s want %s", page.Tasks[0].ID, live.ID)
	}

	// IncludeDiscarded: both returned.
	page, err = s.ListTasks(ctx, broker.TaskFilter{IncludeDiscarded: true})
	if err != nil {
		t.Fatalf("list include: %v", err)
	}
	if page.Total != 2 {
		t.Fatalf("include total: got %d want 2", page.Total)
	}
}
