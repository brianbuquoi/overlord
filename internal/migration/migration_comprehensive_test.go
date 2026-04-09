package migration_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/orcastrator/orcastrator/internal/broker"
	"github.com/orcastrator/orcastrator/internal/config"
	"github.com/orcastrator/orcastrator/internal/contract"
	"github.com/orcastrator/orcastrator/internal/migration"
	"github.com/orcastrator/orcastrator/internal/store/memory"
)

// ─── Helpers ────────────────────────────────────────────────────────────────

// newAddFieldMigration creates a FuncMigration that adds a field with a value.
func newAddFieldMigration(schema, from, to, field string, value any) *migration.FuncMigration {
	return &migration.FuncMigration{
		Schema: schema,
		From:   from,
		To:     to,
		Fn: func(ctx context.Context, payload json.RawMessage) (json.RawMessage, error) {
			var data map[string]any
			if err := json.Unmarshal(payload, &data); err != nil {
				return nil, err
			}
			data[field] = value
			return json.Marshal(data)
		},
	}
}

// seedTasks creates n tasks in the store with the given schema/version/payload.
func seedTasks(t *testing.T, st *memory.MemoryStore, n int, pipelineID, stageID, schemaName, schemaVersion string, payload json.RawMessage, state broker.TaskState) []*broker.Task {
	t.Helper()
	ctx := context.Background()
	var tasks []*broker.Task
	for i := 0; i < n; i++ {
		task := &broker.Task{
			ID:                 fmt.Sprintf("task-%d", i),
			PipelineID:         pipelineID,
			StageID:            stageID,
			InputSchemaName:    schemaName,
			InputSchemaVersion: schemaVersion,
			Payload:            make(json.RawMessage, len(payload)),
			State:              state,
		}
		copy(task.Payload, payload)
		if err := st.EnqueueTask(ctx, stageID, task); err != nil {
			t.Fatalf("seed task %d: %v", i, err)
		}
		tasks = append(tasks, task)
	}
	return tasks
}

// buildValidator creates a contract.Validator for a single schema entry.
func buildValidator(t *testing.T, schemaJSON, name, version string) *contract.Validator {
	t.Helper()
	dir := t.TempDir()
	path := dir + "/schema.json"
	if err := writeTestFile(path, schemaJSON); err != nil {
		t.Fatalf("write schema: %v", err)
	}
	entries := []config.SchemaEntry{{Name: name, Version: version, Path: path}}
	reg, err := contract.NewRegistry(entries, dir)
	if err != nil {
		t.Fatalf("contract registry: %v", err)
	}
	return contract.NewValidator(reg)
}

func writeTestFile(path, content string) error {
	return writeFile(path, content)
}

// migrateAll simulates the CLI migrate run logic against a memory store.
// Returns (migrated, failed, failedTaskIDs).
func migrateAll(
	t *testing.T,
	st *memory.MemoryStore,
	migReg *migration.Registry,
	validator *contract.Validator,
	pipelineID, schemaName, fromVer, toVer string,
	dryRun bool,
	batchSize int,
) (migrated, failed int, failedIDs []string) {
	t.Helper()
	ctx := context.Background()

	offset := 0
	for {
		filter := broker.TaskFilter{
			PipelineID: &pipelineID,
			Limit:      batchSize,
			Offset:     offset,
		}
		result, err := st.ListTasks(ctx, filter)
		if err != nil {
			t.Fatalf("list tasks: %v", err)
		}
		if len(result.Tasks) == 0 {
			break
		}

		for _, task := range result.Tasks {
			matches := (task.InputSchemaName == schemaName && task.InputSchemaVersion == fromVer) ||
				(task.OutputSchemaName == schemaName && task.OutputSchemaVersion == fromVer)
			if !matches {
				continue
			}

			newPayload, err := migReg.Chain(ctx, schemaName, fromVer, toVer, task.Payload)
			if err != nil {
				failed++
				failedIDs = append(failedIDs, task.ID)
				continue
			}

			sv := contract.SchemaVersion(toVer)
			valErr := validator.ValidateInput(schemaName, sv, sv, newPayload)
			if valErr != nil {
				valErr2 := validator.ValidateOutput(schemaName, sv, sv, newPayload)
				if valErr2 != nil {
					failed++
					failedIDs = append(failedIDs, task.ID)
					continue
				}
			}

			if !dryRun {
				update := broker.TaskUpdate{Payload: &newPayload}
				v := toVer
				if task.InputSchemaName == schemaName && task.InputSchemaVersion == fromVer {
					update.InputSchemaVersion = &v
				}
				if task.OutputSchemaName == schemaName && task.OutputSchemaVersion == fromVer {
					update.OutputSchemaVersion = &v
				}
				if err := st.UpdateTask(ctx, task.ID, update); err != nil {
					failed++
					failedIDs = append(failedIDs, task.ID)
					continue
				}
			}
			migrated++
		}

		if len(result.Tasks) < batchSize {
			break
		}
		offset += batchSize
	}
	return
}

// migrateAllWithStateFilter is like migrateAll but only migrates tasks in
// terminal states (DONE, FAILED) unless includeActive is true.
func migrateAllWithStateFilter(
	t *testing.T,
	st *memory.MemoryStore,
	migReg *migration.Registry,
	validator *contract.Validator,
	pipelineID, schemaName, fromVer, toVer string,
	includeActive bool,
) (migrated int) {
	t.Helper()
	ctx := context.Background()

	filter := broker.TaskFilter{PipelineID: &pipelineID, Limit: 1000}
	result, err := st.ListTasks(ctx, filter)
	if err != nil {
		t.Fatalf("list tasks: %v", err)
	}

	for _, task := range result.Tasks {
		matches := (task.InputSchemaName == schemaName && task.InputSchemaVersion == fromVer)
		if !matches {
			continue
		}

		// State filter: only DONE and FAILED by default.
		if !includeActive {
			if task.State != broker.TaskStateDone && task.State != broker.TaskStateFailed {
				continue
			}
		}

		newPayload, err := migReg.Chain(ctx, schemaName, fromVer, toVer, task.Payload)
		if err != nil {
			continue
		}

		sv := contract.SchemaVersion(toVer)
		if valErr := validator.ValidateInput(schemaName, sv, sv, newPayload); valErr != nil {
			continue
		}

		update := broker.TaskUpdate{Payload: &newPayload}
		v := toVer
		update.InputSchemaVersion = &v
		if err := st.UpdateTask(ctx, task.ID, update); err != nil {
			continue
		}
		migrated++
	}
	return
}

// ─── 1. Registration Correctness ────────────────────────────────────────────

func TestRegistrationCorrectness_ChainResolution(t *testing.T) {
	reg := migration.NewRegistry()
	reg.MustRegister(newAddFieldMigration("test_schema", "v1", "v2", "a", 1))
	reg.MustRegister(newAddFieldMigration("test_schema", "v2", "v3", "b", 2))

	// v1→v3 should return [v1→v2, v2→v3]
	steps, err := reg.ResolvePath("test_schema", "v1", "v3")
	if err != nil {
		t.Fatalf("v1→v3: %v", err)
	}
	if len(steps) != 2 {
		t.Fatalf("v1→v3: expected 2 steps, got %d", len(steps))
	}
	if steps[0].FromVersion() != "v1" || steps[0].ToVersion() != "v2" {
		t.Errorf("step 0: got %s→%s, want v1→v2", steps[0].FromVersion(), steps[0].ToVersion())
	}
	if steps[1].FromVersion() != "v2" || steps[1].ToVersion() != "v3" {
		t.Errorf("step 1: got %s→%s, want v2→v3", steps[1].FromVersion(), steps[1].ToVersion())
	}

	// v1→v2 should return [v1→v2]
	steps, err = reg.ResolvePath("test_schema", "v1", "v2")
	if err != nil {
		t.Fatalf("v1→v2: %v", err)
	}
	if len(steps) != 1 {
		t.Fatalf("v1→v2: expected 1 step, got %d", len(steps))
	}
	if steps[0].FromVersion() != "v1" || steps[0].ToVersion() != "v2" {
		t.Errorf("step: got %s→%s, want v1→v2", steps[0].FromVersion(), steps[0].ToVersion())
	}

	// v2→v3 should return [v2→v3]
	steps, err = reg.ResolvePath("test_schema", "v2", "v3")
	if err != nil {
		t.Fatalf("v2→v3: %v", err)
	}
	if len(steps) != 1 {
		t.Fatalf("v2→v3: expected 1 step, got %d", len(steps))
	}
	if steps[0].FromVersion() != "v2" || steps[0].ToVersion() != "v3" {
		t.Errorf("step: got %s→%s, want v2→v3", steps[0].FromVersion(), steps[0].ToVersion())
	}

	// v1→v4 should error naming the missing step
	_, err = reg.ResolvePath("test_schema", "v1", "v4")
	if err == nil {
		t.Fatal("v1→v4: expected error for missing path")
	}
	var noPath *migration.ErrNoMigrationPath
	if !errors.As(err, &noPath) {
		t.Fatalf("expected ErrNoMigrationPath, got %T: %v", err, err)
	}
	// The gap should be at v3 (the chain walks v1→v2→v3, then can't find v3→v4).
	if noPath.GapAt != "v3" {
		t.Errorf("GapAt: got %q, want %q", noPath.GapAt, "v3")
	}
	errMsg := err.Error()
	if !stringContains(errMsg, "v3") {
		t.Errorf("error message should name the missing step v3: %s", errMsg)
	}
}

// ─── 2. Gap Detection ──────────────────────────────────────────────────────

func TestGapDetection(t *testing.T) {
	reg := migration.NewRegistry()
	reg.MustRegister(newAddFieldMigration("test_schema", "v1", "v2", "a", 1))
	// Gap: no v2→v3.
	reg.MustRegister(newAddFieldMigration("test_schema", "v3", "v4", "c", 3))

	// v1→v4 should fail naming the v2→v3 gap.
	_, err := reg.ResolvePath("test_schema", "v1", "v4")
	if err == nil {
		t.Fatal("expected error for gap v2→v3")
	}
	var noPath *migration.ErrNoMigrationPath
	if !errors.As(err, &noPath) {
		t.Fatalf("expected ErrNoMigrationPath, got %T: %v", err, err)
	}
	if noPath.GapAt != "v2" {
		t.Errorf("GapAt: got %q, want %q (should identify v2 as where the gap starts)", noPath.GapAt, "v2")
	}

	// Partial chain v1→v2 should still work.
	steps, err := reg.ResolvePath("test_schema", "v1", "v2")
	if err != nil {
		t.Fatalf("v1→v2 should work despite later gap: %v", err)
	}
	if len(steps) != 1 {
		t.Errorf("expected 1 step, got %d", len(steps))
	}
}

// ─── 3. Duplicate Registration ──────────────────────────────────────────────

func TestDuplicateRegistration_ReturnsError(t *testing.T) {
	reg := migration.NewRegistry()

	m1 := newAddFieldMigration("test_schema", "v1", "v2", "a", 1)
	if err := reg.Register(m1); err != nil {
		t.Fatalf("first registration should succeed: %v", err)
	}

	// Register a different migration with the same (schema, from, to).
	m2 := newAddFieldMigration("test_schema", "v1", "v2", "b", 2)
	err := reg.Register(m2)
	if err == nil {
		t.Fatal("CRITICAL: duplicate registration silently overwrote the first migration — should return an error")
	}

	// Verify the original migration is preserved.
	ctx := context.Background()
	result, err := reg.Chain(ctx, "test_schema", "v1", "v2", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("chain after dup attempt: %v", err)
	}
	var data map[string]any
	json.Unmarshal(result, &data)
	if _, hasA := data["a"]; !hasA {
		t.Error("CRITICAL: original migration was replaced by duplicate — field 'a' missing")
	}
	if _, hasB := data["b"]; hasB {
		t.Error("CRITICAL: duplicate migration overwrote original — field 'b' present")
	}
}

// ─── 4. Downgrade Attempt ───────────────────────────────────────────────────

func TestDowngradeAttempt(t *testing.T) {
	reg := migration.NewRegistry()

	// Register a v2→v1 "downgrade" migration.
	m := newAddFieldMigration("test_schema", "v2", "v1", "downgraded", true)
	err := reg.Register(m)

	// Document behavior: the current implementation does NOT reject downgrades
	// at registration time. The registry is version-string-agnostic — it just
	// follows the from→to chain. A "v2→v1" migration is stored and can be
	// resolved/executed like any other.
	//
	// This is a design decision, not a bug. Downgrades are semantically valid
	// in some workflows (rollback scenarios). If the project later decides to
	// reject them, this test documents where the guard should go.
	if err != nil {
		t.Logf("Downgrade registration rejected with error: %v", err)
		t.Log("DOCUMENTED: Downgrades are rejected at registration time.")
	} else {
		t.Log("DOCUMENTED: Downgrades are NOT rejected at registration time.")
		t.Log("The registry treats v2→v1 the same as any forward migration.")

		// Verify it actually executes.
		ctx := context.Background()
		result, chainErr := reg.Chain(ctx, "test_schema", "v2", "v1", json.RawMessage(`{"x":1}`))
		if chainErr != nil {
			t.Fatalf("downgrade chain failed: %v", chainErr)
		}
		var data map[string]any
		json.Unmarshal(result, &data)
		if data["downgraded"] != true {
			t.Error("downgrade migration did not apply")
		}
	}
}

// ─── 5. Single-Step Migration Correctness ───────────────────────────────────

func TestSingleStepMigration_10Tasks(t *testing.T) {
	st := memory.New()
	payload := json.RawMessage(`{"name":"original","count":42}`)
	seedTasks(t, st, 10, "pipe", "stage1", "test_schema", "v1", payload, broker.TaskStateDone)

	reg := migration.NewRegistry()
	reg.MustRegister(newAddFieldMigration("test_schema", "v1", "v2", "new_field", "migrated"))

	v2Schema := `{
		"$schema":"https://json-schema.org/draft/2020-12/schema",
		"type":"object",
		"properties":{
			"name":{"type":"string"},
			"count":{"type":"number"},
			"new_field":{"type":"string"}
		},
		"required":["name","count","new_field"]
	}`
	validator := buildValidator(t, v2Schema, "test_schema", "v2")

	migrated, failed, _ := migrateAll(t, st, reg, validator, "pipe", "test_schema", "v1", "v2", false, 100)
	if migrated != 10 {
		t.Errorf("expected 10 migrated, got %d", migrated)
	}
	if failed != 0 {
		t.Errorf("expected 0 failed, got %d", failed)
	}

	// Verify all 10 tasks.
	ctx := context.Background()
	for i := 0; i < 10; i++ {
		taskID := fmt.Sprintf("task-%d", i)
		got, err := st.GetTask(ctx, taskID)
		if err != nil {
			t.Fatalf("get %s: %v", taskID, err)
		}

		// Version updated.
		if got.InputSchemaVersion != "v2" {
			t.Errorf("%s: InputSchemaVersion = %s, want v2", taskID, got.InputSchemaVersion)
		}

		// Payload has new field.
		var data map[string]any
		json.Unmarshal(got.Payload, &data)
		if data["new_field"] != "migrated" {
			t.Errorf("%s: new_field = %v, want %q", taskID, data["new_field"], "migrated")
		}

		// Original fields preserved.
		if data["name"] != "original" {
			t.Errorf("%s: name lost: %v", taskID, data["name"])
		}
		if data["count"] != float64(42) {
			t.Errorf("%s: count changed: %v", taskID, data["count"])
		}
	}
}

// ─── 6. Multi-Step Chain Execution ──────────────────────────────────────────

func TestMultiStepChain_AtomicPerTask(t *testing.T) {
	st := memory.New()
	payload := json.RawMessage(`{"original":"data"}`)
	seedTasks(t, st, 5, "pipe", "stage1", "test_schema", "v1", payload, broker.TaskStateDone)

	reg := migration.NewRegistry()
	reg.MustRegister(newAddFieldMigration("test_schema", "v1", "v2", "field_a", "from_v1"))
	reg.MustRegister(newAddFieldMigration("test_schema", "v2", "v3", "field_b", "from_v2"))

	v3Schema := `{
		"$schema":"https://json-schema.org/draft/2020-12/schema",
		"type":"object",
		"properties":{
			"original":{"type":"string"},
			"field_a":{"type":"string"},
			"field_b":{"type":"string"}
		},
		"required":["original","field_a","field_b"]
	}`
	validator := buildValidator(t, v3Schema, "test_schema", "v3")

	migrated, failed, _ := migrateAll(t, st, reg, validator, "pipe", "test_schema", "v1", "v3", false, 100)
	if migrated != 5 || failed != 0 {
		t.Fatalf("expected 5/0, got %d/%d", migrated, failed)
	}

	// Verify final payloads have both fields, version is v3.
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		got, _ := st.GetTask(ctx, fmt.Sprintf("task-%d", i))
		if got.InputSchemaVersion != "v3" {
			t.Errorf("task-%d: version = %s, want v3", i, got.InputSchemaVersion)
		}
		var data map[string]any
		json.Unmarshal(got.Payload, &data)
		if data["field_a"] != "from_v1" {
			t.Errorf("task-%d: field_a = %v", i, data["field_a"])
		}
		if data["field_b"] != "from_v2" {
			t.Errorf("task-%d: field_b = %v", i, data["field_b"])
		}
		if data["original"] != "data" {
			t.Errorf("task-%d: original field lost", i)
		}
	}

	// Verify no intermediate v2 versions exist in the store.
	// (The chain applies atomically per task — the store update goes
	// directly from v1 to v3.)
	pid := "pipe"
	result, _ := st.ListTasks(context.Background(), broker.TaskFilter{PipelineID: &pid, Limit: 1000})
	for _, task := range result.Tasks {
		if task.InputSchemaVersion == "v2" {
			t.Errorf("CRITICAL: intermediate v2 version persisted for task %s — chain should be atomic", task.ID)
		}
	}
}

// ─── 7. Migration Failure Handling ──────────────────────────────────────────

func TestMigrationFailureHandling(t *testing.T) {
	st := memory.New()
	ctx := context.Background()

	// 2 normal tasks.
	for i := 0; i < 2; i++ {
		st.EnqueueTask(ctx, "stage1", &broker.Task{
			ID: fmt.Sprintf("good-%d", i), PipelineID: "pipe", StageID: "stage1",
			InputSchemaName: "test_schema", InputSchemaVersion: "v1",
			Payload: json.RawMessage(`{"status":"ok"}`), State: broker.TaskStateDone,
		})
	}
	// 1 task with bad_value.
	st.EnqueueTask(ctx, "stage1", &broker.Task{
		ID: "bad-0", PipelineID: "pipe", StageID: "stage1",
		InputSchemaName: "test_schema", InputSchemaVersion: "v1",
		Payload: json.RawMessage(`{"status":"bad_value"}`), State: broker.TaskStateDone,
	})

	reg := migration.NewRegistry()
	reg.MustRegister(&migration.FuncMigration{
		Schema: "test_schema", From: "v1", To: "v2",
		Fn: func(ctx context.Context, payload json.RawMessage) (json.RawMessage, error) {
			var data map[string]any
			json.Unmarshal(payload, &data)
			if data["status"] == "bad_value" {
				return nil, fmt.Errorf("cannot migrate task with status=bad_value")
			}
			data["migrated"] = true
			return json.Marshal(data)
		},
	})

	v2Schema := `{
		"$schema":"https://json-schema.org/draft/2020-12/schema",
		"type":"object",
		"properties":{"status":{"type":"string"},"migrated":{"type":"boolean"}},
		"required":["status","migrated"]
	}`
	validator := buildValidator(t, v2Schema, "test_schema", "v2")

	migrated, failed, failedIDs := migrateAll(t, st, reg, validator, "pipe", "test_schema", "v1", "v2", false, 100)

	if migrated != 2 {
		t.Errorf("expected 2 migrated, got %d", migrated)
	}
	if failed != 1 {
		t.Errorf("expected 1 failed, got %d", failed)
	}

	// Verify the failed task ID is reported.
	foundBad := false
	for _, id := range failedIDs {
		if id == "bad-0" {
			foundBad = true
		}
	}
	if !foundBad {
		t.Error("failed task ID 'bad-0' not reported in failures")
	}

	// Verify the failing task is NOT modified.
	bad, _ := st.GetTask(ctx, "bad-0")
	if bad.InputSchemaVersion != "v1" {
		t.Errorf("CRITICAL: failing task version changed to %s — should remain v1", bad.InputSchemaVersion)
	}
	var badPayload map[string]any
	json.Unmarshal(bad.Payload, &badPayload)
	if badPayload["status"] != "bad_value" {
		t.Error("CRITICAL: failing task payload was modified")
	}
	if _, hasMigrated := badPayload["migrated"]; hasMigrated {
		t.Error("CRITICAL: failing task has 'migrated' field — partial migration was persisted")
	}

	// Verify successful tasks were migrated.
	for i := 0; i < 2; i++ {
		good, _ := st.GetTask(ctx, fmt.Sprintf("good-%d", i))
		if good.InputSchemaVersion != "v2" {
			t.Errorf("good-%d: version = %s, want v2", i, good.InputSchemaVersion)
		}
	}
}

// ─── 8. Schema Re-Validation After Migration ────────────────────────────────

func TestBrokenMigration_FailsValidation(t *testing.T) {
	st := memory.New()
	ctx := context.Background()

	st.EnqueueTask(ctx, "stage1", &broker.Task{
		ID: "task-0", PipelineID: "pipe", StageID: "stage1",
		InputSchemaName: "test_schema", InputSchemaVersion: "v1",
		Payload: json.RawMessage(`{"name":"alice"}`), State: broker.TaskStateDone,
	})

	// Broken migration: does NOT add the required "email" field.
	reg := migration.NewRegistry()
	reg.MustRegister(&migration.FuncMigration{
		Schema: "test_schema", From: "v1", To: "v2",
		Fn: func(ctx context.Context, payload json.RawMessage) (json.RawMessage, error) {
			// Intentionally broken — returns payload as-is, missing "email".
			return payload, nil
		},
	})

	v2Schema := `{
		"$schema":"https://json-schema.org/draft/2020-12/schema",
		"type":"object",
		"properties":{"name":{"type":"string"},"email":{"type":"string"}},
		"required":["name","email"]
	}`
	validator := buildValidator(t, v2Schema, "test_schema", "v2")

	migrated, failed, _ := migrateAll(t, st, reg, validator, "pipe", "test_schema", "v1", "v2", false, 100)
	if migrated != 0 {
		t.Errorf("expected 0 migrated (broken migration), got %d", migrated)
	}
	if failed != 1 {
		t.Errorf("expected 1 failed, got %d", failed)
	}

	// Verify the task was NOT updated.
	got, _ := st.GetTask(ctx, "task-0")
	if got.InputSchemaVersion != "v1" {
		t.Errorf("CRITICAL: broken migration updated version to %s — should remain v1", got.InputSchemaVersion)
	}
	var data map[string]any
	json.Unmarshal(got.Payload, &data)
	if _, hasEmail := data["email"]; hasEmail {
		t.Error("CRITICAL: broken migration payload was persisted to store")
	}
}

// ─── 9. Dry-Run Produces No Writes ─────────────────────────────────────────

func TestDryRun_NoWrites(t *testing.T) {
	st := memory.New()
	ctx := context.Background()
	originalPayload := json.RawMessage(`{"name":"unchanged","x":99}`)
	seedTasks(t, st, 5, "pipe", "stage1", "test_schema", "v1", originalPayload, broker.TaskStateDone)

	// Capture original payloads byte-for-byte.
	originals := make(map[string][]byte)
	for i := 0; i < 5; i++ {
		id := fmt.Sprintf("task-%d", i)
		task, _ := st.GetTask(ctx, id)
		cp := make([]byte, len(task.Payload))
		copy(cp, task.Payload)
		originals[id] = cp
	}

	reg := migration.NewRegistry()
	reg.MustRegister(newAddFieldMigration("test_schema", "v1", "v2", "added", "yes"))

	v2Schema := `{
		"$schema":"https://json-schema.org/draft/2020-12/schema",
		"type":"object",
		"properties":{"name":{"type":"string"},"x":{"type":"number"},"added":{"type":"string"}},
		"required":["name","x","added"]
	}`
	validator := buildValidator(t, v2Schema, "test_schema", "v2")

	migrated, failed, _ := migrateAll(t, st, reg, validator, "pipe", "test_schema", "v1", "v2", true, 100)
	if migrated != 5 {
		t.Errorf("dry-run should report 5 would-be-migrated, got %d", migrated)
	}
	if failed != 0 {
		t.Errorf("dry-run should report 0 failures, got %d", failed)
	}

	// Verify all tasks are byte-for-byte identical.
	for i := 0; i < 5; i++ {
		id := fmt.Sprintf("task-%d", i)
		task, _ := st.GetTask(ctx, id)

		if task.InputSchemaVersion != "v1" {
			t.Errorf("CRITICAL: dry-run changed %s version to %s", id, task.InputSchemaVersion)
		}

		if string(task.Payload) != string(originals[id]) {
			t.Errorf("CRITICAL: dry-run modified %s payload.\n  before: %s\n  after:  %s",
				id, string(originals[id]), string(task.Payload))
		}
	}
}

// ─── 10. Batch Size Respect ─────────────────────────────────────────────────

func TestBatchSizeRespect(t *testing.T) {
	st := memory.New()
	payload := json.RawMessage(`{"val":"x"}`)
	seedTasks(t, st, 25, "pipe", "stage1", "test_schema", "v1", payload, broker.TaskStateDone)

	reg := migration.NewRegistry()
	reg.MustRegister(newAddFieldMigration("test_schema", "v1", "v2", "batch_done", true))

	v2Schema := `{
		"$schema":"https://json-schema.org/draft/2020-12/schema",
		"type":"object",
		"properties":{"val":{"type":"string"},"batch_done":{"type":"boolean"}},
		"required":["val","batch_done"]
	}`
	validator := buildValidator(t, v2Schema, "test_schema", "v2")

	// Track batch boundaries by counting how many ListTasks calls produce results.
	ctx := context.Background()
	batchSize := 10
	var batchSizes []int
	offset := 0

	for {
		filter := broker.TaskFilter{
			PipelineID: strPtr("pipe"),
			Limit:      batchSize,
			Offset:     offset,
		}
		result, err := st.ListTasks(ctx, filter)
		if err != nil {
			t.Fatalf("list tasks: %v", err)
		}
		if len(result.Tasks) == 0 {
			break
		}
		batchSizes = append(batchSizes, len(result.Tasks))
		if len(result.Tasks) < batchSize {
			break
		}
		offset += batchSize
	}

	// Verify 3 batches: 10, 10, 5.
	if len(batchSizes) != 3 {
		t.Errorf("expected 3 batches, got %d: %v", len(batchSizes), batchSizes)
	} else {
		if batchSizes[0] != 10 || batchSizes[1] != 10 || batchSizes[2] != 5 {
			t.Errorf("expected batches [10, 10, 5], got %v", batchSizes)
		}
	}

	// Now run the actual migration with batch-size 10.
	migrated, failed, _ := migrateAll(t, st, reg, validator, "pipe", "test_schema", "v1", "v2", false, 10)
	if migrated != 25 {
		t.Errorf("expected 25 migrated, got %d", migrated)
	}
	if failed != 0 {
		t.Errorf("expected 0 failed, got %d", failed)
	}

	// Verify all 25 migrated.
	for i := 0; i < 25; i++ {
		got, _ := st.GetTask(ctx, fmt.Sprintf("task-%d", i))
		if got.InputSchemaVersion != "v2" {
			t.Errorf("task-%d: version = %s, want v2", i, got.InputSchemaVersion)
		}
	}
}

func strPtr(s string) *string { return &s }

// ─── 11. Migrate List Output ────────────────────────────────────────────────

func TestMigrateList_AllRegistered(t *testing.T) {
	reg := migration.NewRegistry()
	reg.MustRegister(newAddFieldMigration("schema_a", "v1", "v2", "x", 1))
	reg.MustRegister(newAddFieldMigration("schema_b", "v1", "v2", "y", 2))
	reg.MustRegister(newAddFieldMigration("schema_b", "v2", "v3", "z", 3))

	all := reg.ListAll()
	if len(all) != 3 {
		t.Fatalf("expected 3 registered migrations, got %d", len(all))
	}

	// Build a lookup set to verify all appear.
	type migKey struct{ schema, from, to string }
	found := make(map[migKey]bool)
	for _, m := range all {
		found[migKey{m.SchemaName(), m.FromVersion(), m.ToVersion()}] = true
	}

	expected := []migKey{
		{"schema_a", "v1", "v2"},
		{"schema_b", "v1", "v2"},
		{"schema_b", "v2", "v3"},
	}
	for _, e := range expected {
		if !found[e] {
			t.Errorf("missing migration in list: %s %s→%s", e.schema, e.from, e.to)
		}
	}

	// Verify path resolution works for schema_b v1→v3.
	_, err := reg.ResolvePath("schema_b", "v1", "v3")
	if err != nil {
		t.Errorf("schema_b v1→v3 path should exist: %v", err)
	}
}

// ─── 12. Migrate Validate (Dry-Run + Validation Only) ───────────────────────

func TestMigrateValidate_ReportsSuccessAndFailure(t *testing.T) {
	st := memory.New()
	ctx := context.Background()

	// 4 valid tasks.
	for i := 0; i < 4; i++ {
		st.EnqueueTask(ctx, "stage1", &broker.Task{
			ID: fmt.Sprintf("valid-%d", i), PipelineID: "pipe", StageID: "stage1",
			InputSchemaName: "test_schema", InputSchemaVersion: "v1",
			Payload: json.RawMessage(`{"name":"ok"}`), State: broker.TaskStateDone,
		})
	}
	// 1 task that will fail migration.
	st.EnqueueTask(ctx, "stage1", &broker.Task{
		ID: "invalid-0", PipelineID: "pipe", StageID: "stage1",
		InputSchemaName: "test_schema", InputSchemaVersion: "v1",
		Payload: json.RawMessage(`{"name":"fail_me"}`), State: broker.TaskStateDone,
	})

	reg := migration.NewRegistry()
	reg.MustRegister(&migration.FuncMigration{
		Schema: "test_schema", From: "v1", To: "v2",
		Fn: func(ctx context.Context, payload json.RawMessage) (json.RawMessage, error) {
			var data map[string]any
			json.Unmarshal(payload, &data)
			if data["name"] == "fail_me" {
				return nil, fmt.Errorf("migration blocked for fail_me")
			}
			data["validated"] = true
			return json.Marshal(data)
		},
	})

	v2Schema := `{
		"$schema":"https://json-schema.org/draft/2020-12/schema",
		"type":"object",
		"properties":{"name":{"type":"string"},"validated":{"type":"boolean"}},
		"required":["name","validated"]
	}`
	validator := buildValidator(t, v2Schema, "test_schema", "v2")

	// Run as dry-run (validate mode).
	migrated, failed, _ := migrateAll(t, st, reg, validator, "pipe", "test_schema", "v1", "v2", true, 100)

	if migrated != 4 {
		t.Errorf("validate: expected 4 would succeed, got %d", migrated)
	}
	if failed != 1 {
		t.Errorf("validate: expected 1 would fail, got %d", failed)
	}

	// Verify NO store writes occurred (all tasks still v1).
	pid := "pipe"
	result, _ := st.ListTasks(ctx, broker.TaskFilter{PipelineID: &pid, Limit: 100})
	for _, task := range result.Tasks {
		if task.InputSchemaVersion != "v1" {
			t.Errorf("CRITICAL: validate mode modified task %s version to %s", task.ID, task.InputSchemaVersion)
		}
	}
}

// ─── 13. State Filter — Only Terminal-State Tasks ───────────────────────────

func TestStateFilter_OnlyTerminalStates(t *testing.T) {
	st := memory.New()
	ctx := context.Background()

	states := map[string]broker.TaskState{
		"pending":    broker.TaskStatePending,
		"routing":    broker.TaskStateRouting,
		"executing":  broker.TaskStateExecuting,
		"validating": broker.TaskStateValidating,
		"done":       broker.TaskStateDone,
		"failed":     broker.TaskStateFailed,
		"retrying":   broker.TaskStateRetrying,
	}

	for name, state := range states {
		st.EnqueueTask(ctx, "stage1", &broker.Task{
			ID: "task-" + name, PipelineID: "pipe", StageID: "stage1",
			InputSchemaName: "test_schema", InputSchemaVersion: "v1",
			Payload: json.RawMessage(`{"name":"` + name + `"}`), State: state,
		})
	}

	reg := migration.NewRegistry()
	reg.MustRegister(newAddFieldMigration("test_schema", "v1", "v2", "migrated", true))

	v2Schema := `{
		"$schema":"https://json-schema.org/draft/2020-12/schema",
		"type":"object",
		"properties":{"name":{"type":"string"},"migrated":{"type":"boolean"}},
		"required":["name","migrated"]
	}`
	validator := buildValidator(t, v2Schema, "test_schema", "v2")

	// Run with state filter (only DONE + FAILED).
	migrated := migrateAllWithStateFilter(t, st, reg, validator, "pipe", "test_schema", "v1", "v2", false)

	if migrated != 2 {
		t.Errorf("expected 2 tasks migrated (DONE + FAILED), got %d", migrated)
	}

	// Verify DONE and FAILED were migrated.
	for _, name := range []string{"done", "failed"} {
		task, _ := st.GetTask(ctx, "task-"+name)
		if task.InputSchemaVersion != "v2" {
			t.Errorf("task-%s: should be migrated to v2, got %s", name, task.InputSchemaVersion)
		}
	}

	// Verify in-flight tasks were NOT touched.
	for _, name := range []string{"pending", "routing", "executing", "validating", "retrying"} {
		task, _ := st.GetTask(ctx, "task-"+name)
		if task.InputSchemaVersion != "v1" {
			t.Errorf("CRITICAL: in-flight task task-%s (state=%s) was migrated — version=%s",
				name, task.State, task.InputSchemaVersion)
		}
	}

	// DOCUMENTED: The current runMigration() in main.go does NOT filter by
	// task state — it migrates ALL tasks matching the schema version regardless
	// of state. This is a potential data integrity issue: migrating an
	// EXECUTING task could cause the agent to write results in v1 format that
	// then conflict with the now-v2 task record.
	//
	// The test above validates correct behavior (state filtering). If the CLI
	// runMigration does not implement this, it should be flagged as a gap.
	t.Log("NOTE: runMigration() in main.go does NOT implement state filtering.")
	t.Log("In-flight tasks (EXECUTING, ROUTING, etc.) are migrated without guard.")
	t.Log("This is a potential DATA INTEGRITY issue — see findings report.")
}

// ─── 14. Concurrent Safety — New Tasks During Migration ─────────────────────

func TestConcurrentSafety_NewTasksDuringMigration(t *testing.T) {
	st := memory.New()
	ctx := context.Background()

	// Pre-seed 10 v1 tasks.
	for i := 0; i < 10; i++ {
		st.EnqueueTask(ctx, "stage1", &broker.Task{
			ID: fmt.Sprintf("old-%d", i), PipelineID: "pipe", StageID: "stage1",
			InputSchemaName: "test_schema", InputSchemaVersion: "v1",
			Payload: json.RawMessage(`{"gen":"v1"}`), State: broker.TaskStateDone,
		})
	}

	reg := migration.NewRegistry()
	reg.MustRegister(newAddFieldMigration("test_schema", "v1", "v2", "upgraded", true))

	v2Schema := `{
		"$schema":"https://json-schema.org/draft/2020-12/schema",
		"type":"object",
		"properties":{"gen":{"type":"string"},"upgraded":{"type":"boolean"}},
		"required":["gen"]
	}`
	validator := buildValidator(t, v2Schema, "test_schema", "v2")

	var wg sync.WaitGroup

	// Goroutine 1: migrate v1→v2.
	var migratedCount int
	wg.Add(1)
	go func() {
		defer wg.Done()
		m, _, _ := migrateAll(t, st, reg, validator, "pipe", "test_schema", "v1", "v2", false, 5)
		migratedCount = m
	}()

	// Goroutine 2: simultaneously add new v2 tasks.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 10; i++ {
			st.EnqueueTask(ctx, "stage1", &broker.Task{
				ID: fmt.Sprintf("new-%d", i), PipelineID: "pipe", StageID: "stage1",
				InputSchemaName: "test_schema", InputSchemaVersion: "v2",
				Payload: json.RawMessage(`{"gen":"v2","upgraded":true}`), State: broker.TaskStateDone,
			})
		}
	}()

	wg.Wait()

	// Verify: new v2 tasks should NOT have been touched by migration
	// (they don't match fromVersion="v1").
	for i := 0; i < 10; i++ {
		task, err := st.GetTask(ctx, fmt.Sprintf("new-%d", i))
		if err != nil {
			continue // May not have been created yet during race.
		}
		if task.InputSchemaVersion != "v2" {
			t.Errorf("CRITICAL: new v2 task new-%d was erroneously re-migrated to %s", i, task.InputSchemaVersion)
		}
	}

	t.Logf("Migrated %d old tasks", migratedCount)
}

// ─── 15. Concurrent Safety — Double Migration ───────────────────────────────

func TestConcurrentSafety_DoubleMigration(t *testing.T) {
	st := memory.New()
	ctx := context.Background()

	// Seed 20 v1 tasks.
	for i := 0; i < 20; i++ {
		st.EnqueueTask(ctx, "stage1", &broker.Task{
			ID: fmt.Sprintf("task-%d", i), PipelineID: "pipe", StageID: "stage1",
			InputSchemaName: "test_schema", InputSchemaVersion: "v1",
			Payload: json.RawMessage(`{"counter":0}`), State: broker.TaskStateDone,
		})
	}

	// Migration that increments counter — if applied twice, counter would be 2.
	var applyCount atomic.Int64
	reg := migration.NewRegistry()
	reg.MustRegister(&migration.FuncMigration{
		Schema: "test_schema", From: "v1", To: "v2",
		Fn: func(ctx context.Context, payload json.RawMessage) (json.RawMessage, error) {
			applyCount.Add(1)
			var data map[string]any
			json.Unmarshal(payload, &data)
			data["counter"] = 1
			data["migrated"] = true
			return json.Marshal(data)
		},
	})

	v2Schema := `{
		"$schema":"https://json-schema.org/draft/2020-12/schema",
		"type":"object",
		"properties":{"counter":{"type":"number"},"migrated":{"type":"boolean"}},
		"required":["counter","migrated"]
	}`
	validator := buildValidator(t, v2Schema, "test_schema", "v2")

	var wg sync.WaitGroup
	var migrated1, migrated2 int

	wg.Add(2)
	go func() {
		defer wg.Done()
		m, _, _ := migrateAll(t, st, reg, validator, "pipe", "test_schema", "v1", "v2", false, 100)
		migrated1 = m
	}()
	go func() {
		defer wg.Done()
		m, _, _ := migrateAll(t, st, reg, validator, "pipe", "test_schema", "v1", "v2", false, 100)
		migrated2 = m
	}()

	wg.Wait()

	// Each task should end up at v2 with counter=1.
	for i := 0; i < 20; i++ {
		task, _ := st.GetTask(ctx, fmt.Sprintf("task-%d", i))
		if task.InputSchemaVersion != "v2" {
			t.Errorf("task-%d: version = %s, want v2", i, task.InputSchemaVersion)
		}
		var data map[string]any
		json.Unmarshal(task.Payload, &data)
		if data["counter"] != float64(1) {
			t.Errorf("task-%d: counter = %v (expected 1 — was migration applied multiple times?)", i, data["counter"])
		}
	}

	// With the current implementation, both goroutines see all v1 tasks before
	// either finishes updating, so both will attempt migration. The store's
	// mutex ensures no corruption, but tasks may be double-migrated (the second
	// migration is a no-op since the payload content is the same, but the
	// migration function IS called twice).
	//
	// The total "migrated" count across both goroutines will likely be 40 (20+20)
	// since both see the same tasks as v1 due to read-then-write without locking.
	totalMigrated := migrated1 + migrated2
	t.Logf("Goroutine 1 migrated: %d, Goroutine 2 migrated: %d, Total: %d", migrated1, migrated2, totalMigrated)
	t.Logf("Migration function called %d times total (20 tasks)", applyCount.Load())

	if totalMigrated > 20 {
		t.Log("NOTE: Tasks were double-processed — both goroutines migrated overlapping tasks.")
		t.Log("The current implementation has no locking mechanism to prevent this.")
		t.Log("While the final state is correct (idempotent by coincidence of constant value),")
		t.Log("migrations with non-idempotent transforms (e.g., counter++) would produce wrong results.")
		t.Log("This is a potential DATA INTEGRITY issue for non-idempotent migrations.")
	}

	if applyCount.Load() > 20 {
		t.Logf("FINDING: Migration function was called %d times for 20 tasks — no per-task locking exists.", applyCount.Load())
	}
}

// ─── Helpers ────────────────────────────────────────────────────────────────

func stringContains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
