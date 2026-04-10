package contract

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/orcastrator/orcastrator/internal/config"
)

// =============================================================================
// Section 2.1 — Schema with $ref (internal definition)
// =============================================================================

func TestSchema_Ref(t *testing.T) {
	dir := t.TempDir()
	schema := `{
		"$schema": "https://json-schema.org/draft/2020-12/schema",
		"type": "object",
		"properties": {
			"item": {"$ref": "#/$defs/Item"}
		},
		"$defs": {
			"Item": {
				"type": "object",
				"properties": {"name": {"type": "string"}},
				"required": ["name"]
			}
		}
	}`
	writeSchema(t, dir, "ref_test_v1.json", schema)

	entries := []config.SchemaEntry{
		{Name: "ref_test", Version: "v1", Path: "ref_test_v1.json"},
	}
	reg, err := NewRegistry(entries, dir)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	v := NewValidator(reg)

	// Valid: item has name field.
	err = v.ValidateInput("ref_test", "v1", "v1", json.RawMessage(`{"item":{"name":"hello"}}`))
	if err != nil {
		t.Fatalf("valid payload failed: %v", err)
	}

	// Invalid: item missing required name.
	err = v.ValidateInput("ref_test", "v1", "v1", json.RawMessage(`{"item":{}}`))
	if err == nil {
		t.Fatal("expected validation error for missing name")
	}
	ce, ok := err.(*ContractError)
	if !ok {
		t.Fatalf("expected ContractError, got %T", err)
	}
	if len(ce.Failures) == 0 {
		t.Fatal("expected at least one failure")
	}
}

// =============================================================================
// Section 2.2 — Schema combinators: allOf, anyOf, oneOf
// =============================================================================

func TestSchema_Combinators(t *testing.T) {
	dir := t.TempDir()

	// allOf: must be an object AND have a name string.
	writeSchema(t, dir, "allof_v1.json", `{
		"$schema": "https://json-schema.org/draft/2020-12/schema",
		"allOf": [
			{"type": "object"},
			{"properties": {"name": {"type": "string"}}, "required": ["name"]}
		]
	}`)

	// anyOf: value can be string or integer.
	writeSchema(t, dir, "anyof_v1.json", `{
		"$schema": "https://json-schema.org/draft/2020-12/schema",
		"type": "object",
		"properties": {
			"value": {"anyOf": [{"type": "string"}, {"type": "integer"}]}
		},
		"required": ["value"]
	}`)

	// oneOf: exactly one subschema matches.
	writeSchema(t, dir, "oneof_v1.json", `{
		"$schema": "https://json-schema.org/draft/2020-12/schema",
		"type": "object",
		"properties": {
			"data": {"oneOf": [
				{"type": "object", "properties": {"kind": {"const": "a"}}, "required": ["kind"]},
				{"type": "object", "properties": {"kind": {"const": "b"}}, "required": ["kind"]}
			]}
		},
		"required": ["data"]
	}`)

	entries := []config.SchemaEntry{
		{Name: "allof", Version: "v1", Path: "allof_v1.json"},
		{Name: "anyof", Version: "v1", Path: "anyof_v1.json"},
		{Name: "oneof", Version: "v1", Path: "oneof_v1.json"},
	}
	reg, err := NewRegistry(entries, dir)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	v := NewValidator(reg)

	t.Run("allOf valid", func(t *testing.T) {
		err := v.ValidateInput("allof", "v1", "v1", json.RawMessage(`{"name":"test"}`))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("allOf invalid", func(t *testing.T) {
		err := v.ValidateInput("allof", "v1", "v1", json.RawMessage(`{}`))
		if err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("anyOf string valid", func(t *testing.T) {
		err := v.ValidateInput("anyof", "v1", "v1", json.RawMessage(`{"value":"hello"}`))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("anyOf integer valid", func(t *testing.T) {
		err := v.ValidateInput("anyof", "v1", "v1", json.RawMessage(`{"value":42}`))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("anyOf boolean invalid", func(t *testing.T) {
		err := v.ValidateInput("anyof", "v1", "v1", json.RawMessage(`{"value":true}`))
		if err == nil {
			t.Fatal("expected error for boolean value")
		}
	})

	t.Run("oneOf matches exactly one", func(t *testing.T) {
		err := v.ValidateInput("oneof", "v1", "v1", json.RawMessage(`{"data":{"kind":"a"}}`))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("oneOf no match", func(t *testing.T) {
		err := v.ValidateInput("oneof", "v1", "v1", json.RawMessage(`{"data":{"kind":"c"}}`))
		if err == nil {
			t.Fatal("expected error for non-matching oneOf")
		}
	})
}

// =============================================================================
// Section 2.3 — Deeply nested validation error paths
// =============================================================================

func TestValidation_DeeplyNestedError(t *testing.T) {
	dir := t.TempDir()
	schema := `{
		"$schema": "https://json-schema.org/draft/2020-12/schema",
		"type": "object",
		"properties": {
			"results": {
				"type": "array",
				"items": {
					"type": "object",
					"properties": {
						"findings": {
							"type": "array",
							"items": {
								"type": "object",
								"properties": {
									"severity": {"type": "string"}
								},
								"required": ["severity"]
							}
						}
					},
					"required": ["findings"]
				}
			}
		},
		"required": ["results"]
	}`
	writeSchema(t, dir, "nested_v1.json", schema)

	entries := []config.SchemaEntry{
		{Name: "nested", Version: "v1", Path: "nested_v1.json"},
	}
	reg, err := NewRegistry(entries, dir)
	if err != nil {
		t.Fatal(err)
	}
	v := NewValidator(reg)

	// Severity should be string, but we pass an integer at depth.
	payload := `{"results":[{"findings":[{"severity":5}]}]}`
	err = v.ValidateInput("nested", "v1", "v1", json.RawMessage(payload))
	if err == nil {
		t.Fatal("expected validation error for integer severity")
	}

	ce, ok := err.(*ContractError)
	if !ok {
		t.Fatalf("expected ContractError, got %T", err)
	}

	// Verify the error includes path information.
	found := false
	for _, f := range ce.Failures {
		if len(f) > 0 {
			found = true
		}
	}
	if !found {
		t.Fatal("expected at least one failure message with path information")
	}
}

// =============================================================================
// Section 2.4 — Concurrent schema validation
// =============================================================================

func TestConcurrent_SchemaValidation(t *testing.T) {
	dir := t.TempDir()
	writeSchema(t, dir, "concurrent_v1.json", `{
		"$schema": "https://json-schema.org/draft/2020-12/schema",
		"type": "object",
		"properties": {"name": {"type": "string"}},
		"required": ["name"]
	}`)

	entries := []config.SchemaEntry{
		{Name: "concurrent", Version: "v1", Path: "concurrent_v1.json"},
	}
	reg, err := NewRegistry(entries, dir)
	if err != nil {
		t.Fatal(err)
	}
	v := NewValidator(reg)

	const goroutines = 100
	var wg sync.WaitGroup
	errs := make([]error, goroutines)

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			errs[idx] = v.ValidateInput("concurrent", "v1", "v1",
				json.RawMessage(`{"name":"test"}`))
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d got error: %v", i, err)
		}
	}
}

// =============================================================================
// Section 2.5 — Empty string vs missing field
// =============================================================================

func TestValidation_EmptyStringVsMissing(t *testing.T) {
	dir := t.TempDir()
	writeSchema(t, dir, "required_v1.json", `{
		"$schema": "https://json-schema.org/draft/2020-12/schema",
		"type": "object",
		"properties": {"name": {"type": "string"}},
		"required": ["name"]
	}`)

	entries := []config.SchemaEntry{
		{Name: "required", Version: "v1", Path: "required_v1.json"},
	}
	reg, err := NewRegistry(entries, dir)
	if err != nil {
		t.Fatal(err)
	}
	v := NewValidator(reg)

	t.Run("empty string passes", func(t *testing.T) {
		// Empty string is a valid string — should pass.
		err := v.ValidateInput("required", "v1", "v1", json.RawMessage(`{"name":""}`))
		if err != nil {
			t.Fatalf("empty string should be valid: %v", err)
		}
	})

	t.Run("missing field fails", func(t *testing.T) {
		// Missing required field — should fail.
		err := v.ValidateInput("required", "v1", "v1", json.RawMessage(`{}`))
		if err == nil {
			t.Fatal("missing required field should fail validation")
		}
	})
}

// =============================================================================
// Section 2.6 — Version compatibility with pre-release versions
// =============================================================================

func TestIsCompatible_PreRelease(t *testing.T) {
	// Pre-release versions use the same major version extraction logic.
	// "v1.0.0-beta" → MajorVersion = "v1" (split on first ".").
	// No special semver handling — pre-release is just a minor/patch suffix.

	tests := []struct {
		task     SchemaVersion
		stage    SchemaVersion
		wantComp bool
		desc     string
	}{
		{"v1.0.0-beta", "v1.0.0", true, "pre-release same major"},
		{"v1.0.0-beta", "v1", true, "pre-release vs bare major"},
		{"v2.0.0-rc1", "v1.0.0", false, "different major with pre-release"},
		{"v1.0.0-alpha", "v1.0.0-beta", true, "both pre-release same major"},
		{"v1", "v1.2.3-rc1", true, "bare vs pre-release"},
	}

	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			got, err := IsCompatible(tt.task, tt.stage)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.wantComp {
				t.Fatalf("IsCompatible(%q, %q) = %v, want %v",
					tt.task, tt.stage, got, tt.wantComp)
			}
		})
	}

	// Document: Pre-release versions are NOT treated specially.
	// MajorVersion("v1.0.0-beta") returns "v1", same as "v1.0.0".
	if mv := MajorVersion("v1.0.0-beta"); mv != "v1" {
		t.Fatalf("expected MajorVersion(v1.0.0-beta) = v1, got %s", mv)
	}
}

// =============================================================================
// Helpers
// =============================================================================

func writeSchema(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
