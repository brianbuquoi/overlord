package chain

import (
	"strings"
	"testing"
)

func newTestChain() *Chain {
	return &Chain{
		ID: "unit",
		Input: &Input{Type: "text"},
		Vars: map[string]string{"topic": "go"},
		Steps: []Step{
			{
				ID:      "draft",
				Model:   "mock/m1",
				Prompt:  "draft {{vars.topic}} {{input}}",
				Fixture: "fixtures/draft.json",
			},
			{
				ID:      "review",
				Model:   "mock/m2",
				Prompt:  "review {{steps.draft.output}}",
				Fixture: "fixtures/review.json",
			},
		},
	}
}

func TestValidate_OK(t *testing.T) {
	if err := Validate(newTestChain()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidate_EmptyID(t *testing.T) {
	c := newTestChain()
	c.ID = ""
	if err := Validate(c); err == nil {
		t.Fatal("want error for empty id")
	}
}

func TestValidate_UnknownProvider(t *testing.T) {
	c := newTestChain()
	c.Steps[0].Model = "whispernet/fast"
	c.Steps[0].Fixture = ""
	err := Validate(c)
	if err == nil || !strings.Contains(err.Error(), "unknown provider") {
		t.Fatalf("want unknown provider error, got: %v", err)
	}
}

func TestValidate_UnknownVarReference(t *testing.T) {
	c := newTestChain()
	c.Steps[0].Prompt = "use {{vars.missing}}"
	if err := Validate(c); err == nil || !strings.Contains(err.Error(), "unknown var") {
		t.Fatalf("want unknown var error, got: %v", err)
	}
}

func TestValidate_ForwardStepReference(t *testing.T) {
	c := newTestChain()
	// draft references review, which runs AFTER it.
	c.Steps[0].Prompt = "bad {{steps.review.output}}"
	err := Validate(c)
	if err == nil || !strings.Contains(err.Error(), "runs at or after") {
		t.Fatalf("want forward-reference error, got: %v", err)
	}
}

func TestValidate_DuplicateStepID(t *testing.T) {
	c := newTestChain()
	c.Steps = append(c.Steps, Step{
		ID:      "draft",
		Model:   "mock/m3",
		Prompt:  "dup",
		Fixture: "fixtures/draft.json",
	})
	err := Validate(c)
	if err == nil || !strings.Contains(err.Error(), "duplicate step") {
		t.Fatalf("want duplicate step error, got: %v", err)
	}
}

func TestValidate_ReservedStepID(t *testing.T) {
	c := newTestChain()
	c.Steps[0].ID = "done"
	err := Validate(c)
	if err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("want reserved error, got: %v", err)
	}
}

func TestValidate_MockRequiresFixture(t *testing.T) {
	c := newTestChain()
	c.Steps[0].Fixture = ""
	err := Validate(c)
	if err == nil || !strings.Contains(err.Error(), "fixture") {
		t.Fatalf("want fixture error, got: %v", err)
	}
}

func TestValidate_FixtureRejectedForRealProvider(t *testing.T) {
	c := newTestChain()
	c.Steps[0].Model = "anthropic/claude-sonnet-4-5"
	// Fixture still set from the helper — that's the error we want.
	err := Validate(c)
	if err == nil || !strings.Contains(err.Error(), "fixture is only valid for mock") {
		t.Fatalf("want mock-only-fixture error, got: %v", err)
	}
}

func TestValidate_OutputFromUnknownStep(t *testing.T) {
	c := newTestChain()
	c.Output = &Output{From: "steps.nope.output"}
	err := Validate(c)
	if err == nil || !strings.Contains(err.Error(), "unknown step") {
		t.Fatalf("want unknown-step error, got: %v", err)
	}
}

// output.from v1 must reference the last step. Pointing at an
// intermediate step is a documented validation error — authors who
// need it graduate via `overlord chain export`.
func TestValidate_OutputFromIntermediateStepRejected(t *testing.T) {
	c := newTestChain()
	c.Output = &Output{From: "steps.draft.output"}
	err := Validate(c)
	if err == nil || !strings.Contains(err.Error(), "must reference the last step") {
		t.Fatalf("want last-step error, got: %v", err)
	}
}

func TestValidate_OutputFromLastStepAccepted(t *testing.T) {
	c := newTestChain()
	c.Output = &Output{From: "steps.review.output"}
	if err := Validate(c); err != nil {
		t.Fatalf("last-step ref rejected: %v", err)
	}
}

// A bare provider is only allowed for "mock". Every real provider
// must be written as <provider>/<model>.
func TestValidate_BareProviderRejectedForRealProvider(t *testing.T) {
	c := newTestChain()
	c.Steps[0].Model = "anthropic"
	c.Steps[0].Fixture = ""
	err := Validate(c)
	if err == nil || !strings.Contains(err.Error(), "bare provider names are only allowed for \"mock\"") {
		t.Fatalf("want bare-provider rejection, got: %v", err)
	}
}

// Bare "mock" remains allowed because the mock provider never
// consults the model field.
func TestValidate_BareMockStillAccepted(t *testing.T) {
	c := newTestChain()
	c.Steps[0].Model = "mock"
	if err := Validate(c); err != nil {
		t.Fatalf("bare mock rejected: %v", err)
	}
}

// A model string with an explicit provider but empty model half
// (e.g. "anthropic/") is rejected as an incomplete reference.
func TestValidate_EmptyModelAfterSlashRejected(t *testing.T) {
	c := newTestChain()
	c.Steps[0].Model = "anthropic/"
	c.Steps[0].Fixture = ""
	err := Validate(c)
	if err == nil || !strings.Contains(err.Error(), "empty model") {
		t.Fatalf("want empty-model error, got: %v", err)
	}
}

// Inline output.schema is only valid when output.type: json.
func TestValidate_OutputSchemaRequiresJSONType(t *testing.T) {
	c := newTestChain()
	c.Output = &Output{
		Type:   "text",
		Schema: map[string]any{"type": "object"},
	}
	err := Validate(c)
	if err == nil || !strings.Contains(err.Error(), "only valid when chain.output.type is \"json\"") {
		t.Fatalf("want schema-requires-json error, got: %v", err)
	}
}

// Inline output.schema must be a JSONSchema the compiler can compile.
func TestValidate_OutputSchemaRejectsInvalidJSONSchema(t *testing.T) {
	c := newTestChain()
	c.Output = &Output{
		Type: "json",
		// "type" must be a string in JSONSchema; feeding an int here
		// makes the JSONSchema compiler reject the document.
		Schema: map[string]any{"type": 42},
	}
	err := Validate(c)
	if err == nil || !strings.Contains(err.Error(), "invalid JSONSchema") {
		t.Fatalf("want invalid-schema error, got: %v", err)
	}
}

func TestValidate_OutputSchemaValidJSONSchemaAccepted(t *testing.T) {
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
	if err := Validate(c); err != nil {
		t.Fatalf("valid inline schema rejected: %v", err)
	}
}

func TestValidate_EmptySteps(t *testing.T) {
	c := &Chain{ID: "x", Steps: nil}
	if err := Validate(c); err == nil {
		t.Fatal("want non-nil error for empty steps")
	}
}
