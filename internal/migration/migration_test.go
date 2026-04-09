package migration_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/orcastrator/orcastrator/internal/migration"
)

// helper to make a FuncMigration that adds a field.
func addFieldMigration(schema, from, to, field string, value any) *migration.FuncMigration {
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

func TestSingleStepMigration(t *testing.T) {
	reg := migration.NewRegistry()
	reg.MustRegister(addFieldMigration("test_schema", "v1", "v2", "new_field", "default"))

	ctx := context.Background()
	input := json.RawMessage(`{"existing":"value"}`)

	result, err := reg.Chain(ctx, "test_schema", "v1", "v2", input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var data map[string]any
	if err := json.Unmarshal(result, &data); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}

	if data["existing"] != "value" {
		t.Errorf("existing field lost: got %v", data["existing"])
	}
	if data["new_field"] != "default" {
		t.Errorf("new_field: got %v, want %q", data["new_field"], "default")
	}
}

func TestMultiStepChain(t *testing.T) {
	reg := migration.NewRegistry()
	reg.MustRegister(addFieldMigration("test_schema", "v1", "v2", "field_a", "a"))
	reg.MustRegister(addFieldMigration("test_schema", "v2", "v3", "field_b", "b"))

	ctx := context.Background()
	input := json.RawMessage(`{"original":true}`)

	result, err := reg.Chain(ctx, "test_schema", "v1", "v3", input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var data map[string]any
	if err := json.Unmarshal(result, &data); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}

	if data["original"] != true {
		t.Error("original field lost")
	}
	if data["field_a"] != "a" {
		t.Errorf("field_a: got %v, want %q", data["field_a"], "a")
	}
	if data["field_b"] != "b" {
		t.Errorf("field_b: got %v, want %q", data["field_b"], "b")
	}
}

func TestMissingMigrationPath(t *testing.T) {
	reg := migration.NewRegistry()
	reg.MustRegister(addFieldMigration("test_schema", "v1", "v2", "x", "y"))
	// No v2→v3 migration registered.

	ctx := context.Background()
	input := json.RawMessage(`{}`)

	_, err := reg.Chain(ctx, "test_schema", "v1", "v3", input)
	if err == nil {
		t.Fatal("expected error for missing migration path")
	}

	var noPath *migration.ErrNoMigrationPath
	if !errors.As(err, &noPath) {
		t.Fatalf("expected ErrNoMigrationPath, got %T: %v", err, err)
	}
	if noPath.GapAt != "v2" {
		t.Errorf("GapAt: got %q, want %q", noPath.GapAt, "v2")
	}
	if noPath.SchemaName != "test_schema" {
		t.Errorf("SchemaName: got %q, want %q", noPath.SchemaName, "test_schema")
	}
}

func TestFailedMigrationReturnsError(t *testing.T) {
	reg := migration.NewRegistry()
	reg.MustRegister(&migration.FuncMigration{
		Schema: "test_schema",
		From:   "v1",
		To:     "v2",
		Fn: func(ctx context.Context, payload json.RawMessage) (json.RawMessage, error) {
			return nil, errors.New("transform failed: bad data")
		},
	})

	ctx := context.Background()
	input := json.RawMessage(`{"x":1}`)

	_, err := reg.Chain(ctx, "test_schema", "v1", "v2", input)
	if err == nil {
		t.Fatal("expected error from failed migration")
	}
	if !errors.Is(err, err) { // sanity
		t.Fatal("error should be itself")
	}
	// Ensure the error message contains useful context.
	errMsg := err.Error()
	if !contains(errMsg, "v1") || !contains(errMsg, "v2") || !contains(errMsg, "bad data") {
		t.Errorf("error message missing context: %s", errMsg)
	}
}

func TestSameVersionNoOp(t *testing.T) {
	reg := migration.NewRegistry()

	steps, err := reg.ResolvePath("test_schema", "v1", "v1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(steps) != 0 {
		t.Errorf("expected 0 steps for same version, got %d", len(steps))
	}
}

func TestDuplicateRegistration(t *testing.T) {
	reg := migration.NewRegistry()
	m := addFieldMigration("test_schema", "v1", "v2", "x", "y")

	if err := reg.Register(m); err != nil {
		t.Fatalf("first register failed: %v", err)
	}

	err := reg.Register(m)
	if err == nil {
		t.Fatal("expected error for duplicate registration")
	}
}

func TestResolvePathReturnsOrderedSteps(t *testing.T) {
	reg := migration.NewRegistry()
	reg.MustRegister(addFieldMigration("s", "v1", "v2", "a", 1))
	reg.MustRegister(addFieldMigration("s", "v2", "v3", "b", 2))
	reg.MustRegister(addFieldMigration("s", "v3", "v4", "c", 3))

	steps, err := reg.ResolvePath("s", "v1", "v4")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(steps) != 3 {
		t.Fatalf("expected 3 steps, got %d", len(steps))
	}

	expected := []struct{ from, to string }{
		{"v1", "v2"}, {"v2", "v3"}, {"v3", "v4"},
	}
	for i, e := range expected {
		if steps[i].FromVersion() != e.from || steps[i].ToVersion() != e.to {
			t.Errorf("step %d: got %s→%s, want %s→%s",
				i, steps[i].FromVersion(), steps[i].ToVersion(), e.from, e.to)
		}
	}
}

func TestListAll(t *testing.T) {
	reg := migration.NewRegistry()
	reg.MustRegister(addFieldMigration("a", "v1", "v2", "x", 1))
	reg.MustRegister(addFieldMigration("b", "v1", "v2", "y", 2))

	all := reg.ListAll()
	if len(all) != 2 {
		t.Fatalf("expected 2 migrations, got %d", len(all))
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
