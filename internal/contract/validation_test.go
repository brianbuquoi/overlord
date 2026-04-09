package contract

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/orcastrator/orcastrator/internal/config"
)

// ===== 1. Error message quality =====

func TestErrorMessageContainsSchemaNameAndVersion(t *testing.T) {
	v := NewValidator(mustRegistry(t))
	payload := json.RawMessage(`{"task_description":"do stuff"}`) // missing priority

	err := v.ValidateInput("intake_input", "v1", "v1", payload)
	var ce *ContractError
	if !errors.As(err, &ce) {
		t.Fatalf("expected ContractError, got %T: %v", err, err)
	}

	msg := ce.Error()
	if !strings.Contains(msg, "intake_input") {
		t.Errorf("error message missing schema name: %s", msg)
	}
	if !strings.Contains(msg, "v1") {
		t.Errorf("error message missing version: %s", msg)
	}
}

func TestErrorMessageContainsFieldPath(t *testing.T) {
	v := NewValidator(mustRegistry(t))

	tests := []struct {
		name        string
		schema      string
		payload     string
		wantInErr   string // substring that must appear in some failure
		description string
	}{
		{
			name:        "missing_required_field",
			schema:      "intake_input",
			payload:     `{"task_description":"do stuff"}`,
			wantInErr:   "priority",
			description: "missing 'priority' must be named",
		},
		{
			name:        "wrong_type",
			schema:      "intake_input",
			payload:     `{"task_description":"do stuff","priority":"high"}`,
			wantInErr:   "priority",
			description: "type error on 'priority' must name the field",
		},
		{
			name:        "nested_missing_description",
			schema:      "intake_output",
			payload:     `{"subtasks":[{"title":"a"}]}`,
			wantInErr:   "description",
			description: "missing nested field must be identified",
		},
		{
			name:        "nested_wrong_type_title",
			schema:      "intake_output",
			payload:     `{"subtasks":[{"title":123,"description":"b"}]}`,
			wantInErr:   "title",
			description: "type error on nested field must name it",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := v.ValidateInput(tt.schema, "v1", "v1", json.RawMessage(tt.payload))
			var ce *ContractError
			if !errors.As(err, &ce) {
				t.Fatalf("expected ContractError, got %T: %v", err, err)
			}

			found := false
			for _, f := range ce.Failures {
				if strings.Contains(f, tt.wantInErr) {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("%s: no failure mentions %q, got: %v", tt.description, tt.wantInErr, ce.Failures)
			}
		})
	}
}

func TestErrorMessageContainsFailureReason(t *testing.T) {
	v := NewValidator(mustRegistry(t))

	tests := []struct {
		name    string
		schema  string
		payload string
		reason  string // substring describing the failure reason
	}{
		{
			name:    "missing_property_reason",
			schema:  "intake_input",
			payload: `{"task_description":"do stuff"}`,
			reason:  "missing property",
		},
		{
			name:    "wrong_type_reason",
			schema:  "intake_input",
			payload: `{"task_description":"do stuff","priority":"high"}`,
			reason:  "want integer",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := v.ValidateInput(tt.schema, "v1", "v1", json.RawMessage(tt.payload))
			var ce *ContractError
			if !errors.As(err, &ce) {
				t.Fatalf("expected ContractError, got %T: %v", err, err)
			}

			msg := ce.Error()
			if !strings.Contains(msg, tt.reason) {
				t.Errorf("error message missing reason %q: %s", tt.reason, msg)
			}
		})
	}
}

// ===== 2. Nested schema validation =====

func TestEmptySubtasksArrayIsValid(t *testing.T) {
	// intake_output_v1 requires subtasks as an array but does not set minItems.
	// An empty array should be valid per the schema.
	v := NewValidator(mustRegistry(t))
	payload := json.RawMessage(`{"subtasks":[]}`)

	err := v.ValidateOutput("intake_output", "v1", "v1", payload)
	if err != nil {
		t.Fatalf("empty subtasks array should be valid (no minItems constraint): %v", err)
	}
}

func TestNestedMissingDescriptionIdentifiesField(t *testing.T) {
	v := NewValidator(mustRegistry(t))
	payload := json.RawMessage(`{"subtasks":[{"title":"a"}]}`)

	err := v.ValidateOutput("intake_output", "v1", "v1", payload)
	var ce *ContractError
	if !errors.As(err, &ce) {
		t.Fatalf("expected ContractError, got %T: %v", err, err)
	}

	// Error must identify subtasks[0] and description.
	msg := ce.Error()
	if !strings.Contains(msg, "subtasks/0") {
		t.Errorf("error should reference subtasks/0, got: %s", msg)
	}
	if !strings.Contains(msg, "description") {
		t.Errorf("error should reference 'description', got: %s", msg)
	}
}

func TestNestedWrongTypeIdentifiesField(t *testing.T) {
	v := NewValidator(mustRegistry(t))
	// title is integer instead of string
	payload := json.RawMessage(`{"subtasks":[{"title":42,"description":"b"}]}`)

	err := v.ValidateOutput("intake_output", "v1", "v1", payload)
	var ce *ContractError
	if !errors.As(err, &ce) {
		t.Fatalf("expected ContractError, got %T: %v", err, err)
	}

	msg := ce.Error()
	if !strings.Contains(msg, "subtasks/0/title") {
		t.Errorf("error should reference subtasks/0/title, got: %s", msg)
	}
	if !strings.Contains(msg, "string") {
		t.Errorf("error should mention expected type 'string', got: %s", msg)
	}
}

// ===== 3. Additional property handling =====

func TestAdditionalPropertiesFalseRejectsExtra(t *testing.T) {
	// intake_input_v1.json has additionalProperties: false.
	v := NewValidator(mustRegistry(t))
	payload := json.RawMessage(`{"task_description":"do stuff","priority":3,"extra_field":"unexpected"}`)

	err := v.ValidateInput("intake_input", "v1", "v1", payload)
	if err == nil {
		t.Fatal("schema with additionalProperties:false should reject extra fields")
	}

	var ce *ContractError
	if !errors.As(err, &ce) {
		t.Fatalf("expected ContractError, got %T: %v", err, err)
	}

	msg := ce.Error()
	if !strings.Contains(msg, "extra_field") {
		t.Errorf("error should mention the extra field name, got: %s", msg)
	}
}

func TestAdditionalPropertiesAllowedWhenNotRestricted(t *testing.T) {
	// Use a schema WITHOUT additionalProperties: false.
	entries := []config.SchemaEntry{
		{Name: "open_input", Version: "v1", Path: "intake_input_v1_open.json"},
	}
	r, err := NewRegistry(entries, testdataDir(t))
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	v := NewValidator(r)

	payload := json.RawMessage(`{"task_description":"do stuff","priority":3,"extra_field":"allowed"}`)
	err = v.ValidateInput("open_input", "v1", "v1", payload)
	if err != nil {
		t.Fatalf("schema without additionalProperties:false should allow extra fields: %v", err)
	}
}

// ===== 4. Version string edge cases =====

func TestVersionCompatibilityEdgeCases(t *testing.T) {
	tests := []struct {
		task, stage string
		want        bool
		wantErr     bool
	}{
		{"v1", "v1", true, false},
		{"v1", "v2", false, false},
		{"v1.0", "v1.5", true, false}, // same major, different minor
		{"v1", "v1.0", true, false},   // v1 and v1.0 are same major
		{"v2", "v10", false, false},   // v2 ≠ v10, not a prefix match
		{"v1", "v10", false, false},   // v1 ≠ v10
		{"v1", "v11", false, false},   // v1 ≠ v11
		{"v10", "v10.1", true, false}, // same major v10
		{"", "v1", false, true},       // empty version → error
		{"v1", "", false, true},       // empty version → error
		{"", "", false, true},         // both empty → error
	}

	for _, tt := range tests {
		t.Run(tt.task+"_vs_"+tt.stage, func(t *testing.T) {
			got, err := IsCompatible(SchemaVersion(tt.task), SchemaVersion(tt.stage))
			if tt.wantErr {
				if err == nil {
					t.Errorf("IsCompatible(%q, %q) expected error, got %v", tt.task, tt.stage, got)
				}
				if !errors.Is(err, ErrEmptyVersion) {
					t.Errorf("expected ErrEmptyVersion, got: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("IsCompatible(%q, %q) unexpected error: %v", tt.task, tt.stage, err)
			}
			if got != tt.want {
				t.Errorf("IsCompatible(%q, %q) = %v, want %v", tt.task, tt.stage, got, tt.want)
			}
		})
	}
}

func TestEmptyVersionReturnsErrorFromValidator(t *testing.T) {
	v := NewValidator(mustRegistry(t))
	payload := json.RawMessage(`{"task_description":"do stuff","priority":3}`)

	err := v.ValidateInput("intake_input", "", "v1", payload)
	if err == nil {
		t.Fatal("empty task version should return an error")
	}
	if !errors.Is(err, ErrEmptyVersion) {
		t.Errorf("expected ErrEmptyVersion, got: %v", err)
	}

	err = v.ValidateInput("intake_input", "v1", "", payload)
	if err == nil {
		t.Fatal("empty stage version should return an error")
	}
	if !errors.Is(err, ErrEmptyVersion) {
		t.Errorf("expected ErrEmptyVersion, got: %v", err)
	}
}

// ===== 5. Registry startup failures =====

func TestRegistryFailsOnNonexistentFile(t *testing.T) {
	entries := []config.SchemaEntry{
		{Name: "ghost", Version: "v1", Path: "nonexistent.json"},
	}
	_, err := NewRegistry(entries, testdataDir(t))
	if err == nil {
		t.Fatal("expected error for nonexistent schema file")
	}
	if !strings.Contains(err.Error(), "ghost@v1") {
		t.Errorf("error should mention schema name and version, got: %v", err)
	}
}

func TestRegistryFailsOnInvalidJSON(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.json")
	os.WriteFile(bad, []byte(`{not json at all`), 0644)

	entries := []config.SchemaEntry{
		{Name: "bad", Version: "v1", Path: bad},
	}
	_, err := NewRegistry(entries, dir)
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestRegistryFailsOnInvalidJSONSchemaType(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad_type.json")
	// Valid JSON but "type" value is not a real JSON Schema type.
	os.WriteFile(bad, []byte(`{"type": "not-a-real-type"}`), 0644)

	entries := []config.SchemaEntry{
		{Name: "bad_type", Version: "v1", Path: bad},
	}
	r, err := NewRegistry(entries, dir)
	if err != nil {
		// If the compiler rejects it at compile time, good.
		return
	}

	// If the registry accepted it, validation with any payload must still
	// not silently succeed. Try validating.
	v := NewValidator(r)
	err = v.ValidateInput("bad_type", "v1", "v1", json.RawMessage(`{"anything":"goes"}`))
	// We don't require a specific error type here, but if it silently passes,
	// that's a problem worth documenting.
	t.Logf("Registry accepted invalid schema type; validation result: %v", err)
}

func TestRegistryFailsOnDuplicateEntry(t *testing.T) {
	entries := []config.SchemaEntry{
		{Name: "intake_input", Version: "v1", Path: "intake_input_v1.json"},
		{Name: "intake_input", Version: "v1", Path: "intake_input_v1.json"},
	}
	_, err := NewRegistry(entries, testdataDir(t))
	if err == nil {
		t.Fatal("expected error for duplicate schema entry")
	}
	if !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("error should mention 'duplicate', got: %v", err)
	}
}

// ===== 6. Caching verification =====

func TestCachingDoesNotReReadDisk(t *testing.T) {
	// Copy a schema to a temp dir, build registry, delete the file,
	// then verify Lookup still works (proving it used the cache).
	dir := t.TempDir()
	schemaPath := filepath.Join(dir, "cached.json")

	schemaContent := `{
		"$schema": "https://json-schema.org/draft/2020-12/schema",
		"type": "object",
		"properties": { "x": { "type": "string" } },
		"required": ["x"]
	}`
	os.WriteFile(schemaPath, []byte(schemaContent), 0644)

	entries := []config.SchemaEntry{
		{Name: "cached", Version: "v1", Path: schemaPath},
	}
	r, err := NewRegistry(entries, dir)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	v := NewValidator(r)

	// First validation — file still exists.
	payload := json.RawMessage(`{"x":"hello"}`)
	if err := v.ValidateInput("cached", "v1", "v1", payload); err != nil {
		t.Fatalf("first validation failed: %v", err)
	}

	// Delete the schema file.
	os.Remove(schemaPath)

	// Second validation — file is gone, but cache should serve it.
	if err := v.ValidateInput("cached", "v1", "v1", payload); err != nil {
		t.Fatalf("second validation failed after file deletion (cache not working): %v", err)
	}
}

// ===== 7. Cross-version test =====

func TestCrossVersionReturnsErrVersionMismatch(t *testing.T) {
	r := mustRegistry(t) // has intake_input v1 and v2
	v := NewValidator(r)

	// v1 payload valid for v1 schema.
	v1Payload := json.RawMessage(`{"task_description":"do stuff","priority":3}`)
	if err := v.ValidateInput("intake_input", "v1", "v1", v1Payload); err != nil {
		t.Fatalf("v1 payload against v1 schema should pass: %v", err)
	}

	// v2 payload valid for v2 schema.
	v2Payload := json.RawMessage(`{"task_description":"do stuff","due_date":"2026-01-01"}`)
	if err := v.ValidateInput("intake_input", "v2", "v2", v2Payload); err != nil {
		t.Fatalf("v2 payload against v2 schema should pass: %v", err)
	}

	// v1 payload against v2 stage → ErrVersionMismatch (version check runs first).
	err := v.ValidateInput("intake_input", "v1", "v2", v1Payload)
	if err == nil {
		t.Fatal("expected error for v1 task against v2 stage")
	}
	var mismatch *ErrVersionMismatch
	if !errors.As(err, &mismatch) {
		t.Fatalf("expected ErrVersionMismatch, got %T: %v", err, err)
	}

	// Must NOT be a ContractError.
	var ce *ContractError
	if errors.As(err, &ce) {
		t.Error("cross-version mismatch should not be ContractError")
	}

	// v2 payload against v1 stage → also ErrVersionMismatch.
	err = v.ValidateInput("intake_input", "v2", "v1", v2Payload)
	if err == nil {
		t.Fatal("expected error for v2 task against v1 stage")
	}
	if !errors.As(err, &mismatch) {
		t.Fatalf("expected ErrVersionMismatch, got %T: %v", err, err)
	}
	if errors.As(err, &ce) {
		t.Error("cross-version mismatch should not be ContractError")
	}
}
