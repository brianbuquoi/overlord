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

func TestValidate_EmptySteps(t *testing.T) {
	c := &Chain{ID: "x", Steps: nil}
	if err := Validate(c); err == nil {
		t.Fatal("want non-nil error for empty steps")
	}
}
