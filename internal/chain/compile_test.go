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

// TestCompile_ReservedSchemaNamesRoundtripCleanly is the regression test
// called out in CLAUDE.md: the three synthesized names (chain_text@v1,
// chain_json@v1, chain_input_json@v1) are reserved for the chain
// compiler, and compiling must produce each reserved name exactly once
// per occurrence, with no accidental shadowing or collision between
// compile runs.
//
// Collision safety today is structural — step/chain IDs live in a
// different namespace than schema names, and workflow/chain authors
// cannot declare schemas directly. This test locks that structural
// contract in: a future change that introduced a second writer of the
// reserved names, or that leaked schemas across compiles via a shared
// registry, would fail this test.
func TestCompile_ReservedSchemaNamesRoundtripCleanly(t *testing.T) {
	// (1) A text-in / text-out chain must synthesize chain_text@v1 only.
	textOnly, err := Compile(newTestChain())
	if err != nil {
		t.Fatalf("text chain compile: %v", err)
	}
	assertSchemaPresent(t, textOnly, textSchemaName, textSchemaVersion)
	assertSchemaAbsent(t, textOnly, jsonSchemaName, jsonSchemaVersion)
	assertSchemaAbsent(t, textOnly, inputJSONSchemaName, inputJSONSchemaVersion)

	// (2) A json-out chain must synthesize chain_text@v1 AND chain_json@v1.
	jsonOut := newTestChain()
	jsonOut.Output = &Output{Type: "json"}
	jsonOutCompiled, err := Compile(jsonOut)
	if err != nil {
		t.Fatalf("json-out chain compile: %v", err)
	}
	assertSchemaPresent(t, jsonOutCompiled, textSchemaName, textSchemaVersion)
	assertSchemaPresent(t, jsonOutCompiled, jsonSchemaName, jsonSchemaVersion)
	assertSchemaAbsent(t, jsonOutCompiled, inputJSONSchemaName, inputJSONSchemaVersion)

	// (3) A json-in chain must synthesize chain_input_json@v1 on the
	//     first stage's input plus chain_text@v1 for inter-step wire.
	jsonIn := newTestChain()
	jsonIn.Input = &Input{Type: "json"}
	jsonInCompiled, err := Compile(jsonIn)
	if err != nil {
		t.Fatalf("json-in chain compile: %v", err)
	}
	assertSchemaPresent(t, jsonInCompiled, textSchemaName, textSchemaVersion)
	assertSchemaPresent(t, jsonInCompiled, inputJSONSchemaName, inputJSONSchemaVersion)

	// (4) Two compiles must not share schemas — the compiler returns a
	//     fresh registry each time so an accidental global sink cannot
	//     introduce cross-compile collisions. Mutating one compile's
	//     schema bytes must not leak into another compile.
	a, err := Compile(newTestChain())
	if err != nil {
		t.Fatalf("compile a: %v", err)
	}
	b, err := Compile(newTestChain())
	if err != nil {
		t.Fatalf("compile b: %v", err)
	}
	keyA := schemaKey(textSchemaName, textSchemaVersion)
	if &a.Schemas == &b.Schemas {
		t.Fatal("two compiles returned the same Schemas map pointer — cross-compile state sharing")
	}
	if a.Registry == b.Registry {
		t.Fatal("two compiles returned the same registry pointer — cross-compile state sharing")
	}
	a.Schemas[keyA] = []byte(`{"mutated":true}`)
	if string(b.Schemas[keyA]) == `{"mutated":true}` {
		t.Fatal("mutating one compile's schema bytes leaked into another compile")
	}

	// (5) The compiled config.Config.SchemaRegistry must reference ONLY
	//     the reserved names. If a future change lets users declare
	//     arbitrary schemas directly on a chain, this assertion must be
	//     updated deliberately — silently widening the registry would
	//     break the "reserved names are compiler-private" invariant.
	for _, entry := range jsonInCompiled.Config.SchemaRegistry {
		switch entry.Name {
		case textSchemaName, jsonSchemaName, inputJSONSchemaName:
			// allowed
		default:
			t.Errorf("unexpected schema name in compiled SchemaRegistry: %q (only the three reserved chain_* names are expected)", entry.Name)
		}
	}
}

func assertSchemaPresent(t *testing.T, c *Compiled, name, version string) {
	t.Helper()
	if _, err := c.Registry.Lookup(name, version); err != nil {
		t.Errorf("registry lookup for %s@%s: %v", name, version, err)
	}
	if _, ok := c.Schemas[schemaKey(name, version)]; !ok {
		t.Errorf("Schemas map missing %s@%s", name, version)
	}
}

func assertSchemaAbsent(t *testing.T, c *Compiled, name, version string) {
	t.Helper()
	if _, err := c.Registry.Lookup(name, version); err == nil {
		t.Errorf("registry unexpectedly contains %s@%s", name, version)
	}
}
