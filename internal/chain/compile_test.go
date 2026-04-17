package chain

import (
	"strings"
	"testing"
)

func TestCompile_BasicShape(t *testing.T) {
	c := newTestChain()
	compiled, err := Compile(c)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	cfg := compiled.Config
	if got, want := len(cfg.Pipelines), 1; got != want {
		t.Fatalf("pipelines: got %d, want %d", got, want)
	}
	p := cfg.Pipelines[0]
	if p.Name != "unit" {
		t.Errorf("pipeline name: got %q", p.Name)
	}
	if got, want := len(p.Stages), 2; got != want {
		t.Fatalf("stages: got %d, want %d", got, want)
	}
	if p.Stages[0].ID != "draft" || p.Stages[1].ID != "review" {
		t.Errorf("stages order: %q/%q", p.Stages[0].ID, p.Stages[1].ID)
	}
	if p.Stages[0].OnSuccess.Static != "review" {
		t.Errorf("draft on_success: got %q", p.Stages[0].OnSuccess.Static)
	}
	if p.Stages[1].OnSuccess.Static != "done" {
		t.Errorf("review on_success: got %q", p.Stages[1].OnSuccess.Static)
	}

	if got, want := len(cfg.Agents), 2; got != want {
		t.Fatalf("agents: got %d, want %d", got, want)
	}
}

func TestCompile_VarsResolvedAtCompileTime(t *testing.T) {
	c := newTestChain()
	compiled, err := Compile(c)
	if err != nil {
		t.Fatal(err)
	}
	if got := compiled.Templates["draft"]; !strings.Contains(got, "go") {
		t.Errorf("vars.topic not substituted into draft template: %q", got)
	}
	if got := compiled.Templates["draft"]; !strings.Contains(got, "{{input}}") {
		t.Errorf("expected {{input}} preserved, got %q", got)
	}
}

func TestCompile_SynthesizedSchemas(t *testing.T) {
	c := newTestChain()
	compiled, err := Compile(c)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := compiled.Registry.Lookup(textSchemaName, textSchemaVersion); err != nil {
		t.Errorf("text schema not registered: %v", err)
	}
}

func TestCompile_JSONOutputUsesJSONSchemaOnFinalStage(t *testing.T) {
	c := newTestChain()
	c.Output = &Output{Type: "json"}
	compiled, err := Compile(c)
	if err != nil {
		t.Fatal(err)
	}
	final := compiled.Config.Pipelines[0].Stages[len(compiled.Config.Pipelines[0].Stages)-1]
	if final.OutputSchema.Name != jsonSchemaName {
		t.Errorf("final stage output schema: got %q, want %q", final.OutputSchema.Name, jsonSchemaName)
	}
	if _, err := compiled.Registry.Lookup(jsonSchemaName, jsonSchemaVersion); err != nil {
		t.Errorf("json schema not registered: %v", err)
	}
}

func TestCompile_InvalidModelStringForNonMockProvider(t *testing.T) {
	c := newTestChain()
	c.Steps[0].Model = "bogus"
	c.Steps[0].Fixture = ""
	if _, err := Compile(c); err == nil {
		t.Fatal("expected error for unknown provider")
	}
}

// Inline output.schema must replace the default open-object schema so
// the compiled registry enforces the user's contract on the final
// stage.
func TestCompile_InlineOutputSchemaOverridesDefault(t *testing.T) {
	c := newTestChain()
	c.Output = &Output{
		Type: "json",
		Schema: map[string]any{
			"type":     "object",
			"required": []any{"summary"},
			"properties": map[string]any{
				"summary": map[string]any{"type": "string"},
			},
		},
	}
	compiled, err := Compile(c)
	if err != nil {
		t.Fatal(err)
	}
	key := schemaKey(jsonSchemaName, jsonSchemaVersion)
	raw, ok := compiled.Schemas[key]
	if !ok {
		t.Fatalf("synthesized schemas missing %q", key)
	}
	if !strings.Contains(string(raw), "\"summary\"") {
		t.Fatalf("compiled schema bytes did not preserve user fields: %s", raw)
	}
	cs, err := compiled.Registry.Lookup(jsonSchemaName, jsonSchemaVersion)
	if err != nil {
		t.Fatalf("lookup json schema: %v", err)
	}
	// The user schema declared `summary` required; a payload missing
	// it must fail validation, a payload with it must pass.
	if err := cs.Schema.Validate(map[string]any{}); err == nil {
		t.Fatalf("expected validation error for empty object under required-summary schema")
	}
	if err := cs.Schema.Validate(map[string]any{"summary": "ok"}); err != nil {
		t.Fatalf("valid payload rejected: %v", err)
	}
}

// See TestExport_RoundTrip_InlineOutputSchema in run_test.go for the
// end-to-end export round-trip of an inline output schema through a
// scaffolded chain (so real fixture files exist on disk).
