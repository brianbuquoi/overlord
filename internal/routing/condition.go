// Package routing implements conditional routing for pipeline stages.
// Conditions use a restricted expression language: JSONPath-style field access
// with comparison operators, evaluated against a stage's output payload.
package routing

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"unicode"
)

// Condition is a parsed condition expression that can be evaluated against
// a JSON payload.
type Condition struct {
	FieldPath []pathSegment // parsed from "output.foo.bar[0]"
	Operator  Operator
	Value     condValue
	Raw       string // original expression for error messages
}

// Operator is a comparison operator in a condition expression.
type Operator int

const (
	OpEq       Operator = iota // ==
	OpNeq                      // !=
	OpGt                       // >
	OpGte                      // >=
	OpLt                       // <
	OpLte                      // <=
	OpContains                 // contains
)

func (op Operator) String() string {
	switch op {
	case OpEq:
		return "=="
	case OpNeq:
		return "!="
	case OpGt:
		return ">"
	case OpGte:
		return ">="
	case OpLt:
		return "<"
	case OpLte:
		return "<="
	case OpContains:
		return "contains"
	default:
		return "unknown"
	}
}

// pathSegment is either a key lookup or an array index.
type pathSegment struct {
	Key     string // non-empty for object key
	Index   int    // used when IsIndex is true
	IsIndex bool
}

// condValue is the right-hand side of a condition.
type condValue struct {
	Kind valueKind
	Str  string
	Num  float64
	Bool bool
}

type valueKind int

const (
	valString valueKind = iota
	valNumber
	valBool
	valNull
)

// MaxFieldPathDepth is the maximum nesting depth for field paths.
const MaxFieldPathDepth = 32

// MaxFieldPathLength is the maximum character length for a field path.
const MaxFieldPathLength = 512

// EvalError is returned when a condition cannot be evaluated due to a type
// mismatch at runtime (e.g., "contains" on an integer field).
type EvalError struct {
	Condition string
	Field     string
	Message   string
}

func (e *EvalError) Error() string {
	return fmt.Sprintf("condition %q: field %q: %s", e.Condition, e.Field, e.Message)
}

// Parse parses a condition expression string into a Condition.
// The expression must follow the grammar:
//
//	condition = field_access operator value
//	field_access = "output." json_path
func Parse(expr string) (*Condition, error) {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return nil, fmt.Errorf("empty condition expression")
	}

	// Parse field path (starts with "output.")
	if !strings.HasPrefix(expr, "output.") {
		return nil, &ParseError{
			Expr: expr,
			Pos:  0,
			Msg:  `condition must start with "output."`,
		}
	}

	// Find operator boundary by scanning past the field path
	fieldEnd, op, opEnd, err := findOperator(expr, 7) // 7 = len("output.")
	if err != nil {
		return nil, err
	}

	fieldStr := expr[7:fieldEnd] // strip "output." prefix
	if fieldStr == "" {
		return nil, &ParseError{Expr: expr, Pos: 7, Msg: "empty field path after 'output.'"}
	}

	path, err := parseFieldPath(expr, fieldStr)
	if err != nil {
		return nil, err
	}

	// Parse value (everything after operator)
	valueStr := strings.TrimSpace(expr[opEnd:])
	if valueStr == "" {
		return nil, &ParseError{Expr: expr, Pos: opEnd, Msg: "missing value after operator"}
	}

	val, err := parseValue(expr, opEnd, valueStr)
	if err != nil {
		return nil, err
	}

	return &Condition{
		FieldPath: path,
		Operator:  op,
		Value:     val,
		Raw:       expr,
	}, nil
}

// ParseError describes a syntax error in a condition expression.
type ParseError struct {
	Expr string
	Pos  int
	Msg  string
}

func (e *ParseError) Error() string {
	return fmt.Sprintf("parse error at position %d in %q: %s", e.Pos, e.Expr, e.Msg)
}

// findOperator scans from startPos to find the operator in the expression.
// Returns (fieldEnd, operator, operatorEnd, error).
func findOperator(expr string, startPos int) (int, Operator, int, error) {
	i := startPos
	n := len(expr)

	// Scan past field path characters: alphanumeric, dot, underscore, brackets, digits
	for i < n {
		ch := expr[i]
		if ch == ' ' || ch == '\t' || ch == '=' || ch == '!' || ch == '>' || ch == '<' {
			break
		}
		if ch == 'c' && i+8 <= n && expr[i:i+8] == "contains" {
			// Check if "contains" is preceded by whitespace (it's an operator, not field name)
			if i > startPos && (expr[i-1] == ' ' || expr[i-1] == '\t') {
				break
			}
		}
		i++
	}

	fieldEnd := i

	// Skip whitespace before operator
	for i < n && (expr[i] == ' ' || expr[i] == '\t') {
		i++
	}

	if i >= n {
		return 0, 0, 0, &ParseError{Expr: expr, Pos: i, Msg: "missing operator"}
	}

	// Parse operator
	switch {
	case i+2 <= n && expr[i:i+2] == "==":
		return fieldEnd, OpEq, i + 2, nil
	case i+2 <= n && expr[i:i+2] == "!=":
		return fieldEnd, OpNeq, i + 2, nil
	case i+2 <= n && expr[i:i+2] == ">=":
		return fieldEnd, OpGte, i + 2, nil
	case i+2 <= n && expr[i:i+2] == "<=":
		return fieldEnd, OpLte, i + 2, nil
	case expr[i] == '>':
		return fieldEnd, OpGt, i + 1, nil
	case expr[i] == '<':
		return fieldEnd, OpLt, i + 1, nil
	case i+8 <= n && expr[i:i+8] == "contains":
		// Verify "contains" is followed by whitespace or end
		if i+8 < n && !unicode.IsSpace(rune(expr[i+8])) {
			return 0, 0, 0, &ParseError{Expr: expr, Pos: i, Msg: fmt.Sprintf("unexpected character after 'contains': %c", expr[i+8])}
		}
		return fieldEnd, OpContains, i + 8, nil
	default:
		return 0, 0, 0, &ParseError{Expr: expr, Pos: i, Msg: fmt.Sprintf("unknown operator at %q", expr[i:])}
	}
}

// parseFieldPath parses a dotted field path with optional array indices.
func parseFieldPath(expr, fieldStr string) ([]pathSegment, error) {
	if len(fieldStr) > MaxFieldPathLength {
		return nil, &ParseError{Expr: expr, Pos: 7, Msg: fmt.Sprintf("field path exceeds maximum length of %d characters", MaxFieldPathLength)}
	}

	var segments []pathSegment
	i := 0
	n := len(fieldStr)

	for i < n {
		if len(segments) >= MaxFieldPathDepth {
			return nil, &ParseError{Expr: expr, Pos: 7 + i, Msg: fmt.Sprintf("field path exceeds maximum depth of %d", MaxFieldPathDepth)}
		}

		// Parse key segment
		start := i
		for i < n && fieldStr[i] != '.' && fieldStr[i] != '[' {
			i++
		}

		key := fieldStr[start:i]
		if key == "" {
			return nil, &ParseError{Expr: expr, Pos: 7 + start, Msg: "empty key in field path"}
		}

		segments = append(segments, pathSegment{Key: key})

		// Check for array index
		for i < n && fieldStr[i] == '[' {
			if len(segments) >= MaxFieldPathDepth {
				return nil, &ParseError{Expr: expr, Pos: 7 + i, Msg: fmt.Sprintf("field path exceeds maximum depth of %d", MaxFieldPathDepth)}
			}

			i++ // skip '['
			idxStart := i
			for i < n && fieldStr[i] != ']' {
				i++
			}
			if i >= n {
				return nil, &ParseError{Expr: expr, Pos: 7 + idxStart, Msg: "unclosed bracket in field path"}
			}

			idxStr := fieldStr[idxStart:i]
			idx, err := strconv.Atoi(idxStr)
			if err != nil {
				return nil, &ParseError{Expr: expr, Pos: 7 + idxStart, Msg: fmt.Sprintf("invalid array index %q", idxStr)}
			}
			if idx < 0 {
				return nil, &ParseError{Expr: expr, Pos: 7 + idxStart, Msg: fmt.Sprintf("negative array index %d not allowed", idx)}
			}

			segments = append(segments, pathSegment{Index: idx, IsIndex: true})
			i++ // skip ']'
		}

		// Skip dot separator
		if i < n && fieldStr[i] == '.' {
			i++
			if i >= n {
				return nil, &ParseError{Expr: expr, Pos: 7 + i, Msg: "trailing dot in field path"}
			}
		}
	}

	return segments, nil
}

// parseValue parses the right-hand value of a condition expression.
func parseValue(expr string, pos int, s string) (condValue, error) {
	switch {
	case s == "null":
		return condValue{Kind: valNull}, nil
	case s == "true":
		return condValue{Kind: valBool, Bool: true}, nil
	case s == "false":
		return condValue{Kind: valBool, Bool: false}, nil
	case len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"':
		return condValue{Kind: valString, Str: s[1 : len(s)-1]}, nil
	default:
		// Try number
		num, err := strconv.ParseFloat(s, 64)
		if err == nil {
			return condValue{Kind: valNumber, Num: num}, nil
		}
		return condValue{}, &ParseError{Expr: expr, Pos: pos, Msg: fmt.Sprintf("invalid value %q (expected string literal, number, bool, or null)", s)}
	}
}

// Evaluate evaluates the condition against a JSON output payload.
// Returns true if the condition matches, false if it doesn't.
// Returns an EvalError only for type errors with "contains" on non-string/non-array fields.
// Missing fields evaluate to false (not error), except for == null which returns true.
func (c *Condition) Evaluate(output json.RawMessage) (bool, error) {
	if len(output) == 0 || string(output) == "null" {
		// Null or empty payload: only "== null" can match
		if c.Operator == OpEq && c.Value.Kind == valNull {
			return true, nil
		}
		if c.Operator == OpNeq && c.Value.Kind != valNull {
			return true, nil
		}
		return false, nil
	}

	// Parse the output JSON
	var data interface{}
	if err := json.Unmarshal(output, &data); err != nil {
		return false, nil
	}

	// Navigate to the field
	val, found := navigatePath(data, c.FieldPath)
	if !found {
		// Missing field: only matches "== null"
		if c.Operator == OpEq && c.Value.Kind == valNull {
			return true, nil
		}
		if c.Operator == OpNeq && c.Value.Kind != valNull {
			return true, nil
		}
		return false, nil
	}

	// Handle null field value
	if val == nil {
		return c.compareNull()
	}

	return c.compareValue(val)
}

// navigatePath traverses the parsed field path through the JSON data.
func navigatePath(data interface{}, path []pathSegment) (interface{}, bool) {
	current := data
	for _, seg := range path {
		if current == nil {
			return nil, false
		}
		if seg.IsIndex {
			arr, ok := current.([]interface{})
			if !ok {
				return nil, false
			}
			if seg.Index >= len(arr) {
				return nil, false
			}
			current = arr[seg.Index]
		} else {
			obj, ok := current.(map[string]interface{})
			if !ok {
				return nil, false
			}
			v, exists := obj[seg.Key]
			if !exists {
				return nil, false
			}
			current = v
		}
	}
	return current, true
}

// compareNull handles comparisons when the field value is JSON null.
func (c *Condition) compareNull() (bool, error) {
	switch c.Operator {
	case OpEq:
		return c.Value.Kind == valNull, nil
	case OpNeq:
		return c.Value.Kind != valNull, nil
	default:
		// Null compared with >, <, >=, <=, contains → false
		return false, nil
	}
}

// compareValue handles comparisons with a non-null field value.
func (c *Condition) compareValue(fieldVal interface{}) (bool, error) {
	fieldPath := c.fieldPathString()

	switch c.Operator {
	case OpContains:
		return c.evalContains(fieldVal, fieldPath)
	case OpEq:
		return c.evalEq(fieldVal), nil
	case OpNeq:
		return !c.evalEq(fieldVal), nil
	case OpGt, OpGte, OpLt, OpLte:
		return c.evalOrdered(fieldVal), nil
	default:
		return false, nil
	}
}

// evalContains evaluates the "contains" operator.
func (c *Condition) evalContains(fieldVal interface{}, fieldPath string) (bool, error) {
	switch v := fieldVal.(type) {
	case string:
		if c.Value.Kind != valString {
			return false, nil
		}
		return strings.Contains(v, c.Value.Str), nil
	case []interface{}:
		for _, elem := range v {
			if c.matchesValue(elem) {
				return true, nil
			}
		}
		return false, nil
	default:
		return false, &EvalError{
			Condition: c.Raw,
			Field:     fieldPath,
			Message:   fmt.Sprintf("\"contains\" requires a string or array field, got %T", fieldVal),
		}
	}
}

// evalEq checks equality between the field value and the condition value.
func (c *Condition) evalEq(fieldVal interface{}) bool {
	if c.Value.Kind == valNull {
		return fieldVal == nil
	}
	return c.matchesValue(fieldVal)
}

// matchesValue checks if a JSON value matches the condition value.
func (c *Condition) matchesValue(v interface{}) bool {
	switch c.Value.Kind {
	case valString:
		s, ok := v.(string)
		return ok && s == c.Value.Str
	case valNumber:
		f, ok := toFloat64(v)
		return ok && f == c.Value.Num
	case valBool:
		b, ok := v.(bool)
		return ok && b == c.Value.Bool
	case valNull:
		return v == nil
	default:
		return false
	}
}

// evalOrdered evaluates ordered comparison operators (>, >=, <, <=).
// Returns false on type mismatch (no error, no panic).
func (c *Condition) evalOrdered(fieldVal interface{}) bool {
	if c.Value.Kind != valNumber {
		return false
	}
	f, ok := toFloat64(fieldVal)
	if !ok {
		return false
	}
	switch c.Operator {
	case OpGt:
		return f > c.Value.Num
	case OpGte:
		return f >= c.Value.Num
	case OpLt:
		return f < c.Value.Num
	case OpLte:
		return f <= c.Value.Num
	default:
		return false
	}
}

// toFloat64 converts a JSON number value to float64.
func toFloat64(v interface{}) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	default:
		return 0, false
	}
}

// fieldPathString reconstructs the field path string for error messages.
func (c *Condition) fieldPathString() string {
	var b strings.Builder
	b.WriteString("output")
	for _, seg := range c.FieldPath {
		if seg.IsIndex {
			b.WriteString(fmt.Sprintf("[%d]", seg.Index))
		} else {
			b.WriteByte('.')
			b.WriteString(seg.Key)
		}
	}
	return b.String()
}
