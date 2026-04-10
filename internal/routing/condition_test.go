package routing

import (
	"encoding/json"
	"strings"
	"testing"
)

// ============================================================================
// 1. Operator coverage
// ============================================================================

func TestParse_AllOperators(t *testing.T) {
	tests := []struct {
		expr string
		op   Operator
	}{
		{`output.x == "a"`, OpEq},
		{`output.x != "a"`, OpNeq},
		{`output.x > 5`, OpGt},
		{`output.x >= 5`, OpGte},
		{`output.x < 5`, OpLt},
		{`output.x <= 5`, OpLte},
		{`output.x contains "a"`, OpContains},
	}
	for _, tt := range tests {
		c, err := Parse(tt.expr)
		if err != nil {
			t.Errorf("Parse(%q) error: %v", tt.expr, err)
			continue
		}
		if c.Operator != tt.op {
			t.Errorf("Parse(%q) operator = %v, want %v", tt.expr, c.Operator, tt.op)
		}
	}
}

func TestEvaluate_EqOperator(t *testing.T) {
	tests := []struct {
		name   string
		expr   string
		output string
		want   bool
	}{
		{"string match", `output.x == "hello"`, `{"x":"hello"}`, true},
		{"string mismatch", `output.x == "hello"`, `{"x":"world"}`, false},
		{"number match", `output.x == 42`, `{"x":42}`, true},
		{"number mismatch", `output.x == 42`, `{"x":43}`, false},
		{"bool true match", `output.x == true`, `{"x":true}`, true},
		{"bool false match", `output.x == false`, `{"x":false}`, true},
		{"bool mismatch", `output.x == true`, `{"x":false}`, false},
		{"null match", `output.x == null`, `{"x":null}`, true},
		{"null mismatch", `output.x == null`, `{"x":"something"}`, false},
		{"missing field is null", `output.x == null`, `{"y":1}`, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := Parse(tt.expr)
			if err != nil {
				t.Fatalf("Parse error: %v", err)
			}
			got, err := c.Evaluate(json.RawMessage(tt.output))
			if err != nil {
				t.Fatalf("Evaluate error: %v", err)
			}
			if got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestEvaluate_NeqOperator(t *testing.T) {
	tests := []struct {
		name   string
		expr   string
		output string
		want   bool
	}{
		{"string neq match", `output.x != "a"`, `{"x":"b"}`, true},
		{"string neq same", `output.x != "a"`, `{"x":"a"}`, false},
		{"number neq", `output.x != 5`, `{"x":6}`, true},
		{"bool neq", `output.x != true`, `{"x":false}`, true},
		{"null neq non-null", `output.x != null`, `{"x":"a"}`, true},
		{"null neq null", `output.x != null`, `{"x":null}`, false},
		{"missing field neq string", `output.x != "a"`, `{"y":1}`, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := Parse(tt.expr)
			got, _ := c.Evaluate(json.RawMessage(tt.output))
			if got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestEvaluate_OrderedOperators(t *testing.T) {
	tests := []struct {
		name   string
		expr   string
		output string
		want   bool
	}{
		// Greater than
		{"gt true", `output.x > 5`, `{"x":6}`, true},
		{"gt false", `output.x > 5`, `{"x":5}`, false},
		{"gt equal", `output.x > 5`, `{"x":5}`, false},
		// Greater equal
		{"gte true gt", `output.x >= 5`, `{"x":6}`, true},
		{"gte true eq", `output.x >= 5`, `{"x":5}`, true},
		{"gte false", `output.x >= 5`, `{"x":4}`, false},
		// Less than
		{"lt true", `output.x < 5`, `{"x":4}`, true},
		{"lt false", `output.x < 5`, `{"x":5}`, false},
		// Less equal
		{"lte true lt", `output.x <= 5`, `{"x":4}`, true},
		{"lte true eq", `output.x <= 5`, `{"x":5}`, true},
		{"lte false", `output.x <= 5`, `{"x":6}`, false},
		// Floats
		{"gt float", `output.x > 3.14`, `{"x":3.15}`, true},
		{"lt float", `output.x < 3.14`, `{"x":3.13}`, true},
		// Negative numbers
		{"gt negative", `output.x > -5`, `{"x":-4}`, true},
		{"lt negative", `output.x < -5`, `{"x":-6}`, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := Parse(tt.expr)
			got, _ := c.Evaluate(json.RawMessage(tt.output))
			if got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestEvaluate_ContainsString(t *testing.T) {
	c, _ := Parse(`output.msg contains "error"`)
	got, _ := c.Evaluate(json.RawMessage(`{"msg":"fatal error occurred"}`))
	if !got {
		t.Error("expected substring match to return true")
	}
	got, _ = c.Evaluate(json.RawMessage(`{"msg":"all clear"}`))
	if got {
		t.Error("expected non-match to return false")
	}
}

func TestEvaluate_ContainsArray(t *testing.T) {
	c, _ := Parse(`output.tags contains "urgent"`)
	got, _ := c.Evaluate(json.RawMessage(`{"tags":["low","urgent","new"]}`))
	if !got {
		t.Error("expected array membership to return true")
	}
	got, _ = c.Evaluate(json.RawMessage(`{"tags":["low","new"]}`))
	if got {
		t.Error("expected non-member to return false")
	}
}

func TestEvaluate_ContainsOnObject_ReturnsError(t *testing.T) {
	c, _ := Parse(`output.data contains "x"`)
	_, err := c.Evaluate(json.RawMessage(`{"data":{"key":"value"}}`))
	if err == nil {
		t.Fatal("expected EvalError for contains on object")
	}
	var evalErr *EvalError
	if !isEvalError(err, &evalErr) {
		t.Fatalf("expected *EvalError, got %T", err)
	}
	if !strings.Contains(evalErr.Message, "contains") {
		t.Errorf("error message should mention 'contains', got: %s", evalErr.Message)
	}
}

func TestEvaluate_ContainsOnInteger_ReturnsError(t *testing.T) {
	c, _ := Parse(`output.items contains "urgent"`)
	_, err := c.Evaluate(json.RawMessage(`{"items":42}`))
	if err == nil {
		t.Fatal("expected EvalError for contains on integer")
	}
}

func isEvalError(err error, target **EvalError) bool {
	if e, ok := err.(*EvalError); ok {
		*target = e
		return true
	}
	return false
}

// ============================================================================
// 2. Field path edge cases
// ============================================================================

func TestEvaluate_NestedFieldAccess(t *testing.T) {
	// output.a.b.c (three levels)
	c, _ := Parse(`output.a.b.c == "deep"`)
	got, _ := c.Evaluate(json.RawMessage(`{"a":{"b":{"c":"deep"}}}`))
	if !got {
		t.Error("expected 3-level nested field to match")
	}
}

func TestEvaluate_ArrayIndex(t *testing.T) {
	c, _ := Parse(`output.items[0] == "first"`)
	got, _ := c.Evaluate(json.RawMessage(`{"items":["first","second"]}`))
	if !got {
		t.Error("expected items[0] to match 'first'")
	}
}

func TestEvaluate_ArrayIndexOutOfBounds(t *testing.T) {
	c, _ := Parse(`output.items[2] == "x"`)
	got, _ := c.Evaluate(json.RawMessage(`{"items":["a","b"]}`))
	if got {
		t.Error("expected out-of-bounds index to return false, not panic")
	}
}

func TestParse_NegativeArrayIndex_Error(t *testing.T) {
	_, err := Parse(`output.items[-1] == "x"`)
	if err == nil {
		t.Fatal("expected error for negative array index")
	}
	if !strings.Contains(err.Error(), "negative") {
		t.Errorf("error should mention 'negative', got: %s", err)
	}
}

func TestParse_OutputOnly_Error(t *testing.T) {
	_, err := Parse(`output == "x"`)
	if err == nil {
		t.Fatal("expected error for root-only access")
	}
}

func TestParse_TrailingDot_Error(t *testing.T) {
	_, err := Parse(`output.field. == "x"`)
	if err == nil {
		t.Fatal("expected error for trailing dot")
	}
	if !strings.Contains(err.Error(), "trailing dot") {
		t.Errorf("error should mention 'trailing dot', got: %s", err)
	}
}

func TestParse_DoubleDot_Error(t *testing.T) {
	_, err := Parse(`output..field == "x"`)
	if err == nil {
		t.Fatal("expected error for double dot")
	}
	if !strings.Contains(err.Error(), "empty key") {
		t.Errorf("error should mention 'empty key', got: %s", err)
	}
}

// ============================================================================
// 3. Type coercion traps — verify NO silent coercion
// ============================================================================

func TestEvaluate_TypeMismatch_NoCoercion(t *testing.T) {
	tests := []struct {
		name   string
		expr   string
		output string
		want   bool
	}{
		// Integer 5 compared to string "5" → false
		{"int vs string", `output.count == "5"`, `{"count":5}`, false},
		// Boolean true compared to number 1 → false
		{"bool vs number", `output.flag == 1`, `{"flag":true}`, false},
		// Empty string "" compared to null → false
		{"empty string vs null", `output.name == null`, `{"name":""}`, false},
		// Missing key → true for == null
		{"missing key vs null", `output.name == null`, `{"other":1}`, true},
		// Explicit JSON null → true for == null
		{"explicit null vs null", `output.name == null`, `{"name":null}`, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := Parse(tt.expr)
			got, _ := c.Evaluate(json.RawMessage(tt.output))
			if got != tt.want {
				t.Errorf("got %v, want %v (type coercion must not happen)", got, tt.want)
			}
		})
	}
}

// ============================================================================
// 4. Injection attempts
// ============================================================================

func TestParse_ShellMetachars_Safe(t *testing.T) {
	// These should parse as invalid values, not execute anything
	exprs := []string{
		"output.x == `whoami`",
		"output.x == $(rm -rf /)",
		"output.x == ; DROP TABLE",
	}
	for _, expr := range exprs {
		_, err := Parse(expr)
		if err == nil {
			t.Errorf("expected parse error for %q", expr)
		}
	}
}

func TestParse_NonOutputPath_Error(t *testing.T) {
	_, err := Parse(`metadata.trace_id == "x"`)
	if err == nil {
		t.Fatal("expected error for non-output path")
	}
	if !strings.Contains(err.Error(), `must start with "output."`) {
		t.Errorf("error should say must start with output., got: %s", err)
	}
}

func TestParse_VeryLongFieldPath_Error(t *testing.T) {
	longPath := "output." + strings.Repeat("a.", 300) + "b == 1"
	_, err := Parse(longPath)
	if err == nil {
		t.Fatal("expected error for very long field path")
	}
	if !strings.Contains(err.Error(), "maximum") {
		t.Errorf("error should mention maximum, got: %s", err)
	}
}

func TestParse_DeeplyNestedPath_Error(t *testing.T) {
	parts := make([]string, 50)
	for i := range parts {
		parts[i] = "a"
	}
	expr := "output." + strings.Join(parts, ".") + " == 1"
	_, err := Parse(expr)
	if err == nil {
		t.Fatal("expected error for deeply nested path (50 levels)")
	}
	if !strings.Contains(err.Error(), "maximum depth") {
		t.Errorf("error should mention depth, got: %s", err)
	}
}

func TestEvaluate_MissingField_False(t *testing.T) {
	c, _ := Parse(`output.nonexistent == "x"`)
	got, _ := c.Evaluate(json.RawMessage(`{"other":"y"}`))
	if got {
		t.Error("missing field should return false for non-null comparison")
	}
}

func TestEvaluate_NullPayload_AllConditionsFalse(t *testing.T) {
	tests := []struct {
		expr string
		want bool
	}{
		{`output.x == "a"`, false},
		{`output.x > 5`, false},
		{`output.x == null`, true},
	}
	for _, tt := range tests {
		c, _ := Parse(tt.expr)
		got, _ := c.Evaluate(json.RawMessage("null"))
		if got != tt.want {
			t.Errorf("expr=%q on null payload: got %v, want %v", tt.expr, got, tt.want)
		}
	}
}

func TestEvaluate_EmptyPayload_AllConditionsFalse(t *testing.T) {
	c, _ := Parse(`output.x == "a"`)
	got, _ := c.Evaluate(json.RawMessage("{}"))
	if got {
		t.Error("expected false for empty payload with non-null comparison")
	}
}

// All ordered operators on null field return false except == null.
func TestEvaluate_NullField_OrderedOperators(t *testing.T) {
	ops := []string{">", ">=", "<", "<="}
	for _, op := range ops {
		c, _ := Parse("output.x " + op + " 5")
		got, _ := c.Evaluate(json.RawMessage(`{"x":null}`))
		if got {
			t.Errorf("null field with %s should return false", op)
		}
	}
}

// Nested field with array: output.findings[0].severity == "critical"
func TestEvaluate_NestedArrayFieldAccess(t *testing.T) {
	c, _ := Parse(`output.findings[0].severity == "critical"`)
	payload := `{"findings":[{"severity":"critical","file":"a.go"}]}`
	got, _ := c.Evaluate(json.RawMessage(payload))
	if !got {
		t.Error("expected nested array field access to match")
	}
}

func TestParse_EmptyExpression_Error(t *testing.T) {
	_, err := Parse("")
	if err == nil {
		t.Fatal("expected error for empty expression")
	}
}

func TestParse_MissingValue_Error(t *testing.T) {
	_, err := Parse(`output.x ==`)
	if err == nil {
		t.Fatal("expected error for missing value")
	}
}
