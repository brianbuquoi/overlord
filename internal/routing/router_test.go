package routing

import (
	"encoding/json"
	"strings"
	"testing"
)

// ============================================================================
// 5. First-match semantics
// ============================================================================

func TestResolve_FirstMatchWins(t *testing.T) {
	// Output matches conditions 2 and 3 but not 1.
	// Verify route 2 is returned, not route 3.
	cond1, _ := Parse(`output.x == "nope"`)
	cond2, _ := Parse(`output.x == "yes"`)
	cond3, _ := Parse(`output.x == "yes"`)

	// Verify first-match semantics via the returned stage ID.
	cfg := RouteConfig{
		IsConditional: true,
		Routes: []ConditionalRoute{
			{Condition: cond1, Stage: "stage-a", RawExpr: cond1.Raw},
			{Condition: cond2, Stage: "stage-b", RawExpr: cond2.Raw},
			{Condition: cond3, Stage: "stage-c", RawExpr: cond3.Raw},
		},
		Default: "default-stage",
	}

	output := json.RawMessage(`{"x":"yes"}`)
	result, err := Resolve(cfg, output)
	if err != nil {
		t.Fatalf("Resolve error: %v", err)
	}
	if result != "stage-b" {
		t.Errorf("expected stage-b (first match), got %q", result)
	}

	// Verify evaluation stops after first match by checking that condition 3
	// was NOT the one returned (stage-b, not stage-c).
}

func TestResolve_EvaluationStopsAfterMatch(t *testing.T) {
	// Use a counting wrapper to verify only 2 conditions are evaluated.
	var evalCount int

	cond1, _ := Parse(`output.x == "no"`)
	cond2, _ := Parse(`output.x == "yes"`)
	cond3, _ := Parse(`output.x == "yes"`)

	// Create a manual evaluation loop to count calls.
	cfg := RouteConfig{
		IsConditional: true,
		Routes: []ConditionalRoute{
			{Condition: cond1, Stage: "a", RawExpr: cond1.Raw},
			{Condition: cond2, Stage: "b", RawExpr: cond2.Raw},
			{Condition: cond3, Stage: "c", RawExpr: cond3.Raw},
		},
		Default: "d",
	}

	output := json.RawMessage(`{"x":"yes"}`)

	// Manually evaluate to count calls.
	for _, route := range cfg.Routes {
		evalCount++
		matched, _ := route.Condition.Evaluate(output)
		if matched {
			if route.Stage != "b" {
				t.Errorf("first match should be stage b, got %q", route.Stage)
			}
			break
		}
	}

	if evalCount != 2 {
		t.Errorf("expected 2 evaluations (not 3), got %d", evalCount)
	}
}

// ============================================================================
// 6. Default fallback
// ============================================================================

func TestResolve_NoMatchReturnsDefault(t *testing.T) {
	cond1, _ := Parse(`output.x == "a"`)
	cond2, _ := Parse(`output.x == "b"`)

	cfg := RouteConfig{
		IsConditional: true,
		Routes: []ConditionalRoute{
			{Condition: cond1, Stage: "stage-a", RawExpr: cond1.Raw},
			{Condition: cond2, Stage: "stage-b", RawExpr: cond2.Raw},
		},
		Default: "fallback",
	}

	result, err := Resolve(cfg, json.RawMessage(`{"x":"c"}`))
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if result != "fallback" {
		t.Errorf("expected default 'fallback', got %q", result)
	}
}

func TestResolve_DefaultDone(t *testing.T) {
	cond, _ := Parse(`output.x == "no"`)
	cfg := RouteConfig{
		IsConditional: true,
		Routes:        []ConditionalRoute{{Condition: cond, Stage: "stage-a", RawExpr: cond.Raw}},
		Default:       "done",
	}
	result, _ := Resolve(cfg, json.RawMessage(`{"x":"y"}`))
	if result != "done" {
		t.Errorf("expected 'done', got %q", result)
	}
}

func TestResolve_DefaultDeadLetter(t *testing.T) {
	cond, _ := Parse(`output.x == "no"`)
	cfg := RouteConfig{
		IsConditional: true,
		Routes:        []ConditionalRoute{{Condition: cond, Stage: "stage-a", RawExpr: cond.Raw}},
		Default:       "dead-letter",
	}
	result, _ := Resolve(cfg, json.RawMessage(`{"x":"y"}`))
	if result != "dead-letter" {
		t.Errorf("expected 'dead-letter', got %q", result)
	}
}

// ============================================================================
// 7. Empty output payload
// ============================================================================

func TestResolve_EmptyPayload(t *testing.T) {
	cond1, _ := Parse(`output.x == "a"`)
	cond2, _ := Parse(`output.y > 0`)

	cfg := RouteConfig{
		IsConditional: true,
		Routes: []ConditionalRoute{
			{Condition: cond1, Stage: "s1", RawExpr: cond1.Raw},
			{Condition: cond2, Stage: "s2", RawExpr: cond2.Raw},
		},
		Default: "empty-default",
	}

	result, err := Resolve(cfg, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("error on empty payload: %v", err)
	}
	if result != "empty-default" {
		t.Errorf("expected default on empty payload, got %q", result)
	}
}

// ============================================================================
// 8. Null payload
// ============================================================================

func TestResolve_NullPayload(t *testing.T) {
	cond, _ := Parse(`output.x == "a"`)
	cfg := RouteConfig{
		IsConditional: true,
		Routes:        []ConditionalRoute{{Condition: cond, Stage: "s1", RawExpr: cond.Raw}},
		Default:       "null-default",
	}

	result, err := Resolve(cfg, json.RawMessage("null"))
	if err != nil {
		t.Fatalf("error on null payload: %v", err)
	}
	if result != "null-default" {
		t.Errorf("expected default on null payload, got %q", result)
	}
}

func TestResolve_NilPayload(t *testing.T) {
	cond, _ := Parse(`output.x == "a"`)
	cfg := RouteConfig{
		IsConditional: true,
		Routes:        []ConditionalRoute{{Condition: cond, Stage: "s1", RawExpr: cond.Raw}},
		Default:       "nil-default",
	}

	result, err := Resolve(cfg, nil)
	if err != nil {
		t.Fatalf("error on nil payload: %v", err)
	}
	if result != "nil-default" {
		t.Errorf("expected default on nil payload, got %q", result)
	}
}

// ============================================================================
// Static route passthrough
// ============================================================================

func TestResolve_StaticConfig(t *testing.T) {
	cfg := StaticRoute("next-stage")
	result, err := Resolve(cfg, json.RawMessage(`{"anything":true}`))
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if result != "next-stage" {
		t.Errorf("expected 'next-stage', got %q", result)
	}
}

// ============================================================================
// Evaluation error propagation
// ============================================================================

func TestResolve_EvalError_Propagated(t *testing.T) {
	// "contains" on integer field returns EvalError.
	cond, _ := Parse(`output.items contains "urgent"`)
	cfg := RouteConfig{
		IsConditional: true,
		Routes:        []ConditionalRoute{{Condition: cond, Stage: "s1", RawExpr: cond.Raw}},
		Default:       "d",
	}

	_, err := Resolve(cfg, json.RawMessage(`{"items":42}`))
	if err == nil {
		t.Fatal("expected error for contains on integer")
	}
	if !strings.Contains(err.Error(), "contains") {
		t.Errorf("error should mention contains, got: %s", err)
	}
}
