//go:build integration

package postgres

// Security Audit Verification — Section 4: SQL Injection
// Test 16: Verify parameterized queries prevent SQL injection.
// Requires live Postgres (run via `make test-integration`).

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/brianbuquoi/overlord/internal/broker"
)

func TestSecurity_SQLInjection(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set")
	}

	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	st, err := New(pool, "overlord_tasks")
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	injectionPayloads := []struct {
		name  string
		value string
	}{
		{"classic_drop", "'; DROP TABLE overlord_tasks; --"},
		{"union_select", "' UNION SELECT * FROM pg_tables; --"},
		{"comment_bypass", "admin'--"},
		{"boolean_blind", "' OR '1'='1"},
	}

	for _, tc := range injectionPayloads {
		t.Run("GetTask_"+tc.name, func(t *testing.T) {
			task, err := st.GetTask(ctx, tc.value)
			if err != nil {
				// Expected: task not found or safe error
				t.Logf("GetTask(%q) safely returned error: %v", tc.value, err)
			} else if task != nil {
				t.Errorf("GetTask(%q) returned unexpected task: %+v", tc.value, task)
			}
		})

		t.Run("ListTasks_pipeline_"+tc.name, func(t *testing.T) {
			pipelineID := tc.value
			result, err := st.ListTasks(ctx, broker.TaskFilter{
				PipelineID: &pipelineID,
				Limit:      10,
			})
			if err != nil {
				t.Logf("ListTasks(pipeline=%q) safely returned error: %v", tc.value, err)
			} else if len(result.Tasks) > 0 {
				t.Logf("ListTasks returned %d tasks (should be 0 for injection payload)", len(result.Tasks))
			}
		})

		t.Run("ListTasks_state_"+tc.name, func(t *testing.T) {
			state := broker.TaskState(tc.value)
			result, err := st.ListTasks(ctx, broker.TaskFilter{
				State: &state,
				Limit: 10,
			})
			if err != nil {
				t.Logf("ListTasks(state=%q) safely returned error: %v", tc.value, err)
			} else {
				t.Logf("ListTasks returned %d tasks (expected 0)", len(result.Tasks))
			}
		})
	}

	// Verify the table still exists after all injection attempts
	t.Run("table_still_exists", func(t *testing.T) {
		result, err := st.ListTasks(ctx, broker.TaskFilter{Limit: 1})
		if err != nil {
			t.Fatalf("table appears to be dropped or corrupted after injection tests: %v", err)
		}
		t.Logf("Table intact, %d total tasks", result.Total)
	})
}
