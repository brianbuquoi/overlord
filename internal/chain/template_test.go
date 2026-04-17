package chain

import (
	"reflect"
	"testing"
)

func TestRender_Basic(t *testing.T) {
	got, missing := Render("hello {{input}} and {{vars.name}}", Scope{
		Vars:  map[string]string{"name": "world"},
		Input: "there",
	})
	if got != "hello there and world" {
		t.Fatalf("got %q", got)
	}
	if len(missing) != 0 {
		t.Fatalf("unexpected missing: %v", missing)
	}
}

func TestRender_MissingReturnsEmpty(t *testing.T) {
	got, missing := Render("{{vars.x}} {{input}}", Scope{})
	if got != " " {
		t.Fatalf("expected single space, got %q", got)
	}
	wantMissing := []string{"vars.x", "input"}
	if !reflect.DeepEqual(missing, wantMissing) {
		t.Fatalf("missing: %v, want %v", missing, wantMissing)
	}
}

func TestRender_StepsRef(t *testing.T) {
	got, missing := Render("got: {{steps.draft.output}}", Scope{
		Outputs: map[string]string{"draft": "DRAFT_OUTPUT"},
	})
	if got != "got: DRAFT_OUTPUT" {
		t.Fatalf("got %q", got)
	}
	if len(missing) != 0 {
		t.Fatalf("unexpected missing: %v", missing)
	}
}

func TestResolveVarsOnly_LeavesRuntimeRefsIntact(t *testing.T) {
	got := ResolveVarsOnly(
		"{{vars.topic}} - {{input}} - {{steps.x.output}} - {{vars.missing}}",
		map[string]string{"topic": "Go"},
	)
	want := "Go - {{input}} - {{steps.x.output}} - {{ vars.missing }}"
	// ResolveVarsOnly preserves the original match exactly when the
	// var is unbound, so the unbound form should be "{{vars.missing}}"
	// (no inner whitespace added). Adjust expectation accordingly.
	want = "Go - {{input}} - {{steps.x.output}} - {{vars.missing}}"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestPlaceholders(t *testing.T) {
	refs := Placeholders("a {{input}} b {{vars.x}} c {{steps.s.output}} {{vars.x}}")
	want := []string{"input", "vars.x", "steps.s.output", "vars.x"}
	if !reflect.DeepEqual(refs, want) {
		t.Fatalf("got %v, want %v", refs, want)
	}
}
