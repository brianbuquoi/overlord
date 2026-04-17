package chain

import (
	"regexp"
	"strings"
)

// placeholderRE matches the lightweight `{{ <expr> }}` form used in
// chain prompts. The expression is restricted to dotted identifiers so
// the regex doubles as a sanity check: anything fancier (pipes,
// functions, conditionals) is not a chain-mode placeholder.
var placeholderRE = regexp.MustCompile(`\{\{\s*([a-zA-Z0-9_.]+)\s*\}\}`)

// Scope is the set of values a chain placeholder can resolve to.
// Fields left zero resolve to empty strings and are reported back to
// the caller as "missing" so validation can distinguish "known but
// empty" from "not declared".
type Scope struct {
	// Vars holds compile-time variable substitutions.
	Vars map[string]string
	// Input is the text of the chain's initial payload ({{input}}).
	Input string
	// Outputs maps step IDs to their captured text output.
	Outputs map[string]string
}

// Render substitutes placeholders in s using scope. Placeholders whose
// references cannot be resolved are left as empty strings and their
// expressions are returned in the missing slice — callers decide
// whether to treat this as an error (strict mode) or as a soft miss
// (chain adapter at runtime, which tolerates missing {{input}} on the
// first step before the submitter populates it).
func Render(s string, scope Scope) (rendered string, missing []string) {
	rendered = placeholderRE.ReplaceAllStringFunc(s, func(match string) string {
		m := placeholderRE.FindStringSubmatch(match)
		if len(m) < 2 {
			return match
		}
		expr := m[1]
		val, ok := resolveScope(expr, scope)
		if !ok {
			missing = append(missing, expr)
			return ""
		}
		return val
	})
	return rendered, missing
}

// resolveScope returns the value bound to expr and a boolean ok. An
// expression that is structurally valid but unbound (e.g. vars.foo
// with foo not in Vars) returns ("", false) so the caller can report
// it; a structurally invalid expression also returns ("", false) for
// the same reason.
func resolveScope(expr string, scope Scope) (string, bool) {
	switch {
	case expr == "input":
		if scope.Input == "" {
			return "", false
		}
		return scope.Input, true
	case strings.HasPrefix(expr, "vars."):
		key := strings.TrimPrefix(expr, "vars.")
		v, ok := scope.Vars[key]
		return v, ok
	case strings.HasPrefix(expr, "steps."):
		rest := strings.TrimPrefix(expr, "steps.")
		parts := strings.SplitN(rest, ".", 2)
		if len(parts) != 2 || parts[1] != "output" {
			return "", false
		}
		v, ok := scope.Outputs[parts[0]]
		return v, ok
	}
	return "", false
}

// Placeholders returns every `{{ <expr> }}` reference found in s, in
// document order. Duplicates are preserved. Used by the validator to
// check references statically.
func Placeholders(s string) []string {
	matches := placeholderRE.FindAllStringSubmatch(s, -1)
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		out = append(out, m[1])
	}
	return out
}

// ResolveVarsOnly returns s with only {{vars.*}} placeholders
// substituted and every other placeholder left intact. Used by the
// compiler so that the synthesized agent system prompt still carries
// runtime references ({{input}}, {{steps.<id>.output}}) that the
// chain-step adapter substitutes per-task.
func ResolveVarsOnly(s string, vars map[string]string) string {
	return placeholderRE.ReplaceAllStringFunc(s, func(match string) string {
		m := placeholderRE.FindStringSubmatch(match)
		if len(m) < 2 {
			return match
		}
		expr := m[1]
		if !strings.HasPrefix(expr, "vars.") {
			return match
		}
		key := strings.TrimPrefix(expr, "vars.")
		if v, ok := vars[key]; ok {
			return v
		}
		return match
	})
}
