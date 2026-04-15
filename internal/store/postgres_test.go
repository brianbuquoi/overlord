//go:build integration

package store_test

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/brianbuquoi/overlord/internal/broker"
	"github.com/brianbuquoi/overlord/internal/store"
	pgstore "github.com/brianbuquoi/overlord/internal/store/postgres"
)

func setupPostgresTest(t *testing.T) (*pgxpool.Pool, string) {
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

	// Use a unique table per test to avoid interference.
	table := "overlord_tasks_test_" + uuid.New().String()[:8]
	// Replace hyphens with underscores for valid SQL identifier.
	safeTable := ""
	for _, c := range table {
		if c == '-' {
			safeTable += "_"
		} else {
			safeTable += string(c)
		}
	}

	_, err = pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS `+safeTable+` (
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

	_, err = pool.Exec(ctx, `CREATE INDEX IF NOT EXISTS idx_`+safeTable+`_dequeue
		ON `+safeTable+` (stage_id, state, created_at)`)
	if err != nil {
		t.Fatalf("create index: %v", err)
	}

	t.Cleanup(func() {
		pool.Exec(context.Background(), "DROP TABLE IF EXISTS "+safeTable)
	})

	return pool, safeTable
}

// Test 5: SKIP LOCKED under contention — 5 goroutines dequeuing the same stage.
// Each task must be returned to exactly one goroutine (no duplicates).
func TestPostgres_SkipLockedContention(t *testing.T) {
	pool, table := setupPostgresTest(t)
	ctx := context.Background()
	s, err := pgstore.New(pool, table)
	if err != nil {
		t.Fatal(err)
	}
	stage := "stage-contention"

	const numTasks = 20
	const numWorkers = 5

	// Enqueue tasks.
	taskIDs := make(map[string]bool)
	for i := 0; i < numTasks; i++ {
		task := newTask("pipe-1", stage)
		task.CreatedAt = time.Now().Add(time.Duration(i) * time.Millisecond)
		taskIDs[task.ID] = true
		if err := s.EnqueueTask(ctx, stage, task); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}

	// Start 5 workers all dequeuing simultaneously.
	var mu sync.Mutex
	dequeued := make(map[string]int) // taskID → count of times dequeued
	var wg sync.WaitGroup
	wg.Add(numWorkers)

	for w := 0; w < numWorkers; w++ {
		go func() {
			defer wg.Done()
			for {
				workerCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
				task, err := s.DequeueTask(workerCtx, stage)
				cancel()
				if err != nil {
					return // queue empty or timeout
				}
				mu.Lock()
				dequeued[task.ID]++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	// Verify: each task dequeued exactly once.
	if len(dequeued) != numTasks {
		t.Errorf("dequeued %d unique tasks, want %d", len(dequeued), numTasks)
	}
	for id, count := range dequeued {
		if count != 1 {
			t.Errorf("task %s dequeued %d times (want exactly 1)", id, count)
		}
	}

	// Verify all original tasks were dequeued.
	for id := range taskIDs {
		if _, ok := dequeued[id]; !ok {
			t.Errorf("task %s was never dequeued", id)
		}
	}
}

// Test 6: Migration idempotency — running the migration SQL twice must not error.
func TestPostgres_MigrationIdempotency(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	migrationSQL := `CREATE TABLE IF NOT EXISTS overlord_tasks (
		id                    TEXT PRIMARY KEY,
		pipeline_id           TEXT NOT NULL,
		stage_id              TEXT NOT NULL,
		input_schema_name     TEXT NOT NULL DEFAULT '',
		input_schema_version  TEXT NOT NULL DEFAULT '',
		output_schema_name    TEXT NOT NULL DEFAULT '',
		output_schema_version TEXT NOT NULL DEFAULT '',
		payload               JSONB,
		metadata              JSONB,
		state                 TEXT NOT NULL DEFAULT 'PENDING',
		attempts              INTEGER NOT NULL DEFAULT 0,
		max_attempts          INTEGER NOT NULL DEFAULT 1,
		created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
		updated_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
		expires_at            TIMESTAMPTZ
	);

	CREATE INDEX IF NOT EXISTS idx_tasks_stage_state_created
		ON overlord_tasks (stage_id, state, created_at);`

	// Run migration twice — second run must not error.
	for i := 0; i < 2; i++ {
		_, err := pool.Exec(ctx, migrationSQL)
		if err != nil {
			t.Fatalf("migration run %d failed: %v", i+1, err)
		}
	}
	t.Log("Migration is idempotent: both IF NOT EXISTS guards work correctly")
}

// Test 7: Index usage — verify the dequeue query uses the (stage_id, state, created_at) index.
func TestPostgres_IndexUsage(t *testing.T) {
	pool, table := setupPostgresTest(t)
	ctx := context.Background()
	s, err := pgstore.New(pool, table)
	if err != nil {
		t.Fatal(err)
	}
	stage := "stage-explain"

	// Insert some data so the planner has something to work with.
	for i := 0; i < 50; i++ {
		task := newTask("pipe-1", stage)
		if err := s.EnqueueTask(ctx, stage, task); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}

	// ANALYZE the table so the query planner has up-to-date statistics.
	_, err := pool.Exec(ctx, "ANALYZE "+table)
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}

	// Run EXPLAIN on the dequeue subquery (the SELECT ... FOR UPDATE SKIP LOCKED part).
	explainQuery := `EXPLAIN (FORMAT TEXT) SELECT id FROM ` + table + `
		WHERE stage_id = $1
		AND state = $2
		AND (expires_at IS NULL OR expires_at = '0001-01-01T00:00:00Z' OR expires_at > $3)
		ORDER BY created_at ASC
		LIMIT 1
		FOR UPDATE SKIP LOCKED`

	rows, err := pool.Query(ctx, explainQuery, stage, string(broker.TaskStatePending), time.Now())
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer rows.Close()

	var plan []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan explain: %v", err)
		}
		plan = append(plan, line)
	}

	// Log the full plan for debugging.
	t.Log("EXPLAIN output:")
	for _, line := range plan {
		t.Log("  ", line)
	}

	// Check that it uses an index scan, not a sequential scan.
	usesIndex := false
	usesSeqScan := false
	for _, line := range plan {
		if contains(line, "Index Scan") || contains(line, "Index Only Scan") || contains(line, "Bitmap Index Scan") {
			usesIndex = true
		}
		if contains(line, "Seq Scan") {
			usesSeqScan = true
		}
	}

	if usesSeqScan && !usesIndex {
		t.Error("dequeue query uses sequential scan — the (stage_id, state, created_at) index is not being used. Check the index definition or query predicates.")
	}
	if usesIndex {
		t.Log("CONFIRMED: dequeue query uses index scan")
	}
}

// Test: Two goroutines call UpdateTask on the same task with conflicting state
// changes. Exactly one wins; the final state is consistent (not a mix).
func TestPostgres_UpdateTaskAtomicity(t *testing.T) {
	pool, table := setupPostgresTest(t)
	ctx := context.Background()
	s, err := pgstore.New(pool, table)
	if err != nil {
		t.Fatal(err)
	}
	stage := "stage-atomic-update"

	task := newTask("pipe-1", stage)
	if err := s.EnqueueTask(ctx, stage, task); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	stateA := broker.TaskStateExecuting
	attemptsA := 10
	updateA := broker.TaskUpdate{
		State:    &stateA,
		Attempts: &attemptsA,
		Metadata: map[string]any{"writer": "A"},
	}

	stateB := broker.TaskStateFailed
	attemptsB := 99
	updateB := broker.TaskUpdate{
		State:    &stateB,
		Attempts: &attemptsB,
		Metadata: map[string]any{"writer": "B"},
	}

	var wg sync.WaitGroup
	wg.Add(2)
	var errA, errB error
	go func() {
		defer wg.Done()
		errA = s.UpdateTask(ctx, task.ID, updateA)
	}()
	go func() {
		defer wg.Done()
		errB = s.UpdateTask(ctx, task.ID, updateB)
	}()
	wg.Wait()

	if errA != nil {
		t.Fatalf("updateA: %v", errA)
	}
	if errB != nil {
		t.Fatalf("updateB: %v", errB)
	}

	got, err := s.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	// The final state must be entirely from one writer — not a mix.
	writer, ok := got.Metadata["writer"].(string)
	if !ok {
		t.Fatal("metadata missing writer key")
	}
	switch writer {
	case "A":
		if got.State != broker.TaskStateExecuting {
			t.Errorf("writer=A but state=%s (want EXECUTING)", got.State)
		}
		if got.Attempts != 10 {
			t.Errorf("writer=A but attempts=%d (want 10)", got.Attempts)
		}
	case "B":
		if got.State != broker.TaskStateFailed {
			t.Errorf("writer=B but state=%s (want FAILED)", got.State)
		}
		if got.Attempts != 99 {
			t.Errorf("writer=B but attempts=%d (want 99)", got.Attempts)
		}
	default:
		t.Fatalf("unexpected writer: %s", writer)
	}
	t.Logf("winner: writer %s (state=%s, attempts=%d)", writer, got.State, got.Attempts)
}

// Test: UpdateTask on unknown ID returns ErrTaskNotFound (within transaction path).
func TestPostgres_UpdateTaskNotFound(t *testing.T) {
	pool, table := setupPostgresTest(t)
	ctx := context.Background()
	s, err := pgstore.New(pool, table)
	if err != nil {
		t.Fatal(err)
	}

	state := broker.TaskStateExecuting
	err := s.UpdateTask(ctx, "nonexistent-"+uuid.New().String(), broker.TaskUpdate{
		State: &state,
	})
	if err != store.ErrTaskNotFound {
		t.Errorf("got %v, want ErrTaskNotFound", err)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchString(s, substr)
}

func searchString(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
