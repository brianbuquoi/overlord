package migration_test

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/orcastrator/orcastrator/internal/broker"
	"github.com/orcastrator/orcastrator/internal/config"
	"github.com/orcastrator/orcastrator/internal/contract"
	"github.com/orcastrator/orcastrator/internal/migration"
	"github.com/orcastrator/orcastrator/internal/store/memory"
)

// runTestMigration simulates what the CLI does: list tasks, migrate payloads,
// validate against target schema, and optionally update the store.
func runTestMigration(
	t *testing.T,
	st broker.Store,
	migReg *migration.Registry,
	validator *contract.Validator,
	pipelineID, schemaName, fromVer, toVer string,
	dryRun bool,
) (migrated, failed int) {
	t.Helper()

	ctx := context.Background()
	pid := pipelineID

	result, err := st.ListTasks(ctx, broker.TaskFilter{PipelineID: &pid, Limit: 1000})
	if err != nil {
		t.Fatalf("list tasks: %v", err)
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
			continue
		}

		sv := contract.SchemaVersion(toVer)
		valErr := validator.ValidateInput(schemaName, sv, sv, newPayload)
		if valErr != nil {
			valErr2 := validator.ValidateOutput(schemaName, sv, sv, newPayload)
			if valErr2 != nil {
				failed++
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
				continue
			}
		}
		migrated++
	}
	return migrated, failed
}

func makeValidator(t *testing.T, schemaJSON string, name, version string) *contract.Validator {
	t.Helper()

	dir := t.TempDir()
	path := dir + "/schema.json"
	if err := writeFile(path, schemaJSON); err != nil {
		t.Fatalf("write schema file: %v", err)
	}

	entries := []config.SchemaEntry{{Name: name, Version: version, Path: path}}
	reg, err := contract.NewRegistry(entries, dir)
	if err != nil {
		t.Fatalf("contract registry: %v", err)
	}
	return contract.NewValidator(reg)
}

func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0644)
}

func TestDryRunNoStoreWrites(t *testing.T) {
	st := memory.New()
	ctx := context.Background()

	task := &broker.Task{
		ID:                 "task-1",
		PipelineID:         "pipe",
		StageID:            "stage1",
		InputSchemaName:    "test_schema",
		InputSchemaVersion: "v1",
		Payload:            json.RawMessage(`{"name":"alice"}`),
		State:              broker.TaskStateDone,
	}
	st.EnqueueTask(ctx, "stage1", task)

	reg := migration.NewRegistry()
	reg.MustRegister(addFieldMigration("test_schema", "v1", "v2", "age", 0))

	v2Schema := `{
		"$schema":"https://json-schema.org/draft/2020-12/schema",
		"type":"object",
		"properties":{"name":{"type":"string"},"age":{"type":"integer"}},
		"required":["name","age"]
	}`
	validator := makeValidator(t, v2Schema, "test_schema", "v2")

	migrated, failed := runTestMigration(t, st, reg, validator, "pipe", "test_schema", "v1", "v2", true)
	if migrated != 1 || failed != 0 {
		t.Fatalf("expected 1 migrated 0 failed, got %d migrated %d failed", migrated, failed)
	}

	// Verify the store was NOT updated (dry run).
	got, err := st.GetTask(ctx, "task-1")
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got.InputSchemaVersion != "v1" {
		t.Errorf("dry run modified schema version: got %s, want v1", got.InputSchemaVersion)
	}

	var payload map[string]any
	json.Unmarshal(got.Payload, &payload)
	if _, hasAge := payload["age"]; hasAge {
		t.Error("dry run wrote payload changes to store")
	}
}

func TestMigrationValidatesOutputAgainstTargetSchema(t *testing.T) {
	st := memory.New()
	ctx := context.Background()

	task := &broker.Task{
		ID:                 "task-1",
		PipelineID:         "pipe",
		StageID:            "stage1",
		InputSchemaName:    "test_schema",
		InputSchemaVersion: "v1",
		Payload:            json.RawMessage(`{"name":"alice"}`),
		State:              broker.TaskStateDone,
	}
	st.EnqueueTask(ctx, "stage1", task)

	// Migration that produces output missing the required "email" field.
	reg := migration.NewRegistry()
	reg.MustRegister(&migration.FuncMigration{
		Schema: "test_schema",
		From:   "v1",
		To:     "v2",
		Fn: func(ctx context.Context, payload json.RawMessage) (json.RawMessage, error) {
			// Intentionally does NOT add the required "email" field.
			return payload, nil
		},
	})

	v2Schema := `{
		"$schema":"https://json-schema.org/draft/2020-12/schema",
		"type":"object",
		"properties":{"name":{"type":"string"},"email":{"type":"string"}},
		"required":["name","email"]
	}`
	validator := makeValidator(t, v2Schema, "test_schema", "v2")

	migrated, failed := runTestMigration(t, st, reg, validator, "pipe", "test_schema", "v1", "v2", false)
	if migrated != 0 {
		t.Errorf("expected 0 migrated, got %d", migrated)
	}
	if failed != 1 {
		t.Errorf("expected 1 failed, got %d", failed)
	}

	// Verify the store was NOT updated.
	got, _ := st.GetTask(ctx, "task-1")
	if got.InputSchemaVersion != "v1" {
		t.Errorf("failed migration modified schema version: got %s", got.InputSchemaVersion)
	}
}

func TestSuccessfulMigrationUpdatesStore(t *testing.T) {
	st := memory.New()
	ctx := context.Background()

	task := &broker.Task{
		ID:                 "task-1",
		PipelineID:         "pipe",
		StageID:            "stage1",
		InputSchemaName:    "test_schema",
		InputSchemaVersion: "v1",
		Payload:            json.RawMessage(`{"name":"alice"}`),
		State:              broker.TaskStateDone,
	}
	st.EnqueueTask(ctx, "stage1", task)

	reg := migration.NewRegistry()
	reg.MustRegister(addFieldMigration("test_schema", "v1", "v2", "age", 0))

	v2Schema := `{
		"$schema":"https://json-schema.org/draft/2020-12/schema",
		"type":"object",
		"properties":{"name":{"type":"string"},"age":{"type":"integer"}},
		"required":["name","age"]
	}`
	validator := makeValidator(t, v2Schema, "test_schema", "v2")

	migrated, failed := runTestMigration(t, st, reg, validator, "pipe", "test_schema", "v1", "v2", false)
	if migrated != 1 || failed != 0 {
		t.Fatalf("expected 1 migrated 0 failed, got %d migrated %d failed", migrated, failed)
	}

	got, _ := st.GetTask(ctx, "task-1")
	if got.InputSchemaVersion != "v2" {
		t.Errorf("schema version not updated: got %s, want v2", got.InputSchemaVersion)
	}

	var payload map[string]any
	json.Unmarshal(got.Payload, &payload)
	if payload["name"] != "alice" {
		t.Errorf("original field lost")
	}
	if payload["age"] != float64(0) {
		t.Errorf("new field missing: age=%v", payload["age"])
	}
}
