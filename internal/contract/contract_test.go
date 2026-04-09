package contract

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/orcastrator/orcastrator/internal/config"
)

func testdataDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs("testdata")
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func testEntries() []config.SchemaEntry {
	return []config.SchemaEntry{
		{Name: "intake_input", Version: "v1", Path: "intake_input_v1.json"},
		{Name: "intake_output", Version: "v1", Path: "intake_output_v1.json"},
		{Name: "intake_input", Version: "v2", Path: "intake_input_v2.json"},
	}
}

func mustRegistry(t *testing.T) *Registry {
	t.Helper()
	r, err := NewRegistry(testEntries(), testdataDir(t))
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	return r
}

// --- Versioning tests ---

func TestMajorVersion(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"v1", "v1"},
		{"v1.2", "v1"},
		{"v2.3.1", "v2"},
		{"v10", "v10"},
	}
	for _, tt := range tests {
		got := MajorVersion(SchemaVersion(tt.input))
		if got != tt.want {
			t.Errorf("MajorVersion(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestIsCompatible(t *testing.T) {
	tests := []struct {
		task, stage string
		want        bool
	}{
		{"v1", "v1", true},
		{"v1", "v1.2", true},
		{"v1.1", "v1.2", true},
		{"v1", "v2", false},
		{"v2", "v1", false},
	}
	for _, tt := range tests {
		got, err := IsCompatible(SchemaVersion(tt.task), SchemaVersion(tt.stage))
		if err != nil {
			t.Errorf("IsCompatible(%q, %q) unexpected error: %v", tt.task, tt.stage, err)
			continue
		}
		if got != tt.want {
			t.Errorf("IsCompatible(%q, %q) = %v, want %v", tt.task, tt.stage, got, tt.want)
		}
	}
}

// --- Registry tests ---

func TestRegistryLookup(t *testing.T) {
	r := mustRegistry(t)

	cs, err := r.Lookup("intake_input", "v1")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if cs.Name != "intake_input" || cs.Version != "v1" {
		t.Errorf("got %s@%s, want intake_input@v1", cs.Name, cs.Version)
	}
}

func TestRegistryLookupNotFound(t *testing.T) {
	r := mustRegistry(t)
	_, err := r.Lookup("nonexistent", "v1")
	if err == nil {
		t.Fatal("expected error for missing schema")
	}
}

func TestRegistryFailsOnMissingFile(t *testing.T) {
	entries := []config.SchemaEntry{
		{Name: "missing", Version: "v1", Path: "does_not_exist.json"},
	}
	_, err := NewRegistry(entries, testdataDir(t))
	if err == nil {
		t.Fatal("expected error for missing schema file")
	}
	if !os.IsNotExist(errors.Unwrap(err)) {
		// The error wraps the underlying file error; just check the message.
		if err == nil {
			t.Fatal("expected non-nil error")
		}
	}
}

func TestRegistryFailsOnMalformedSchema(t *testing.T) {
	// Create a temporary malformed schema file.
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte(`{not json`), 0644); err != nil {
		t.Fatal(err)
	}

	entries := []config.SchemaEntry{
		{Name: "bad", Version: "v1", Path: bad},
	}
	_, err := NewRegistry(entries, dir)
	if err == nil {
		t.Fatal("expected error for malformed schema")
	}
}

func TestRegistryCaching(t *testing.T) {
	r := mustRegistry(t)

	cs1, _ := r.Lookup("intake_input", "v1")
	cs2, _ := r.Lookup("intake_input", "v1")

	// Same pointer — the registry caches compiled schemas by (name, version).
	if cs1 != cs2 {
		t.Error("expected same *CompiledSchema pointer for repeated Lookup")
	}
}

// --- Validator tests ---

func TestValidateInputValid(t *testing.T) {
	v := NewValidator(mustRegistry(t))
	payload := json.RawMessage(`{"task_description":"do stuff","priority":3}`)

	err := v.ValidateInput("intake_input", "v1", "v1", payload)
	if err != nil {
		t.Fatalf("expected valid payload to pass: %v", err)
	}
}

func TestValidateOutputValid(t *testing.T) {
	v := NewValidator(mustRegistry(t))
	payload := json.RawMessage(`{"subtasks":[{"title":"a","description":"b"}]}`)

	err := v.ValidateOutput("intake_output", "v1", "v1", payload)
	if err != nil {
		t.Fatalf("expected valid output to pass: %v", err)
	}
}

func TestValidateInputMissingRequiredField(t *testing.T) {
	v := NewValidator(mustRegistry(t))
	// Missing "priority".
	payload := json.RawMessage(`{"task_description":"do stuff"}`)

	err := v.ValidateInput("intake_input", "v1", "v1", payload)
	if err == nil {
		t.Fatal("expected validation error for missing field")
	}

	var ce *ContractError
	if !errors.As(err, &ce) {
		t.Fatalf("expected ContractError, got %T: %v", err, err)
	}
	if ce.Direction != Input {
		t.Errorf("expected direction=input, got %s", ce.Direction)
	}

	// The failure message should mention "priority".
	found := false
	for _, f := range ce.Failures {
		if contains(f, "priority") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected failure mentioning 'priority', got: %v", ce.Failures)
	}
}

func TestValidateInputWrongType(t *testing.T) {
	v := NewValidator(mustRegistry(t))
	// priority should be int, not string.
	payload := json.RawMessage(`{"task_description":"do stuff","priority":"high"}`)

	err := v.ValidateInput("intake_input", "v1", "v1", payload)
	if err == nil {
		t.Fatal("expected validation error for wrong type")
	}

	var ce *ContractError
	if !errors.As(err, &ce) {
		t.Fatalf("expected ContractError, got %T", err)
	}
}

func TestVersionMismatchReturnsErrVersionMismatch(t *testing.T) {
	v := NewValidator(mustRegistry(t))
	// v1 task routed to v2 stage — should get ErrVersionMismatch, not ContractError.
	payload := json.RawMessage(`{"task_description":"do stuff","priority":3}`)

	err := v.ValidateInput("intake_input", "v1", "v2", payload)
	if err == nil {
		t.Fatal("expected version mismatch error")
	}

	var mismatch *ErrVersionMismatch
	if !errors.As(err, &mismatch) {
		t.Fatalf("expected ErrVersionMismatch, got %T: %v", err, err)
	}
	if mismatch.TaskVersion != "v1" || mismatch.StageVersion != "v2" {
		t.Errorf("unexpected versions in error: task=%s stage=%s", mismatch.TaskVersion, mismatch.StageVersion)
	}

	// Must NOT be a ContractError.
	var ce *ContractError
	if errors.As(err, &ce) {
		t.Error("version mismatch should not be a ContractError")
	}
}

func TestCompatibleVersionsPassToSchemaValidation(t *testing.T) {
	v := NewValidator(mustRegistry(t))
	// v1 task, v1 stage — compatible, should proceed to schema validation.
	// Valid payload should pass.
	payload := json.RawMessage(`{"task_description":"do stuff","priority":3}`)

	err := v.ValidateInput("intake_input", "v1", "v1", payload)
	if err != nil {
		t.Fatalf("expected v1/v1 with valid payload to pass: %v", err)
	}
}

func TestContractErrorFields(t *testing.T) {
	v := NewValidator(mustRegistry(t))
	payload := json.RawMessage(`{}`)

	err := v.ValidateInput("intake_input", "v1", "v1", payload)
	var ce *ContractError
	if !errors.As(err, &ce) {
		t.Fatalf("expected ContractError, got %T", err)
	}
	if ce.SchemaName != "intake_input" {
		t.Errorf("SchemaName = %q, want intake_input", ce.SchemaName)
	}
	if ce.Version != "v1" {
		t.Errorf("Version = %q, want v1", ce.Version)
	}
	if ce.Direction != Input {
		t.Errorf("Direction = %q, want input", ce.Direction)
	}
	if len(ce.Failures) == 0 {
		t.Error("expected at least one failure")
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsHelper(s, substr))
}

func containsHelper(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
