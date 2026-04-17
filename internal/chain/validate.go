package chain

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/brianbuquoi/overlord/internal/contract"
)

// validID matches the same character class pipelines and agents use —
// chain-mode identifiers are compiled into pipeline/agent/stage IDs so
// they must satisfy the same constraints.
var validID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

// knownProviders enumerates the provider strings accepted on the
// left-hand side of a chain step's `model:` field. Kept in parity with
// the registry package's built-in provider switch — adding a provider
// here without wiring the registry is a silent bug.
var knownProviders = map[string]struct{}{
	"anthropic":        {},
	"openai":           {},
	"openai-responses": {},
	"google":           {},
	"ollama":           {},
	"mock":             {},
	"copilot":          {},
}

// reservedStepIDs are step IDs that would collide with reserved
// routing targets in the compiled pipeline.
var reservedStepIDs = map[string]struct{}{
	"done":        {},
	"dead-letter": {},
}

// Validate checks structural rules for a chain. It runs in two phases:
// identifier / shape checks first, then template-reference checks that
// require the full step list to be known.
func Validate(c *Chain) error {
	if c == nil {
		return fmt.Errorf("chain is nil")
	}
	if c.ID == "" {
		return fmt.Errorf("chain.id must not be empty")
	}
	if !validID.MatchString(c.ID) {
		return fmt.Errorf("chain.id %q contains invalid characters (must match %s)", c.ID, validID.String())
	}
	if len(c.Steps) == 0 {
		return fmt.Errorf("chain.steps must be non-empty")
	}

	inputType := c.InputType()
	switch inputType {
	case "text", "json":
	default:
		return fmt.Errorf("chain.input.type %q must be \"text\" or \"json\"", inputType)
	}

	outputType := c.OutputType()
	switch outputType {
	case "text", "json":
	default:
		return fmt.Errorf("chain.output.type %q must be \"text\" or \"json\"", outputType)
	}

	if c.Output != nil && c.Output.From != "" {
		if !strings.HasPrefix(c.Output.From, "steps.") || !strings.HasSuffix(c.Output.From, ".output") {
			return fmt.Errorf("chain.output.from %q must look like \"steps.<id>.output\"", c.Output.From)
		}
	}

	if c.Output != nil && len(c.Output.Schema) > 0 {
		if outputType != "json" {
			return fmt.Errorf("chain.output.schema is only valid when chain.output.type is \"json\" (got %q)", outputType)
		}
		if err := validateInlineOutputSchema(c.Output.Schema); err != nil {
			return fmt.Errorf("chain.output.schema: %w", err)
		}
	}

	for name := range c.Vars {
		if !validID.MatchString(name) {
			return fmt.Errorf("chain.vars key %q contains invalid characters", name)
		}
	}

	seen := make(map[string]struct{}, len(c.Steps))
	stepsBeforeMap := make(map[string]int, len(c.Steps))
	for i, st := range c.Steps {
		if st.ID == "" {
			return fmt.Errorf("step[%d].id must not be empty", i)
		}
		if !validID.MatchString(st.ID) {
			return fmt.Errorf("step[%d].id %q contains invalid characters", i, st.ID)
		}
		if _, reserved := reservedStepIDs[st.ID]; reserved {
			return fmt.Errorf("step[%d].id %q is reserved", i, st.ID)
		}
		if _, dup := seen[st.ID]; dup {
			return fmt.Errorf("step[%d]: duplicate step id %q", i, st.ID)
		}
		seen[st.ID] = struct{}{}
		stepsBeforeMap[st.ID] = i

		if st.Model == "" {
			return fmt.Errorf("step %q: model must not be empty", st.ID)
		}
		provider, _, bare, err := splitModel(st.Model)
		if err != nil {
			return fmt.Errorf("step %q: %w", st.ID, err)
		}
		if _, ok := knownProviders[provider]; !ok {
			return fmt.Errorf("step %q: unknown provider %q (valid: %s)", st.ID, provider, providerList())
		}
		// Only the mock provider accepts a bare provider string
		// (`mock`) because its model field is unused. Every other
		// built-in adapter needs a concrete model, and a bare
		// "anthropic" / "openai" string is almost always a typo.
		if bare && provider != "mock" {
			return fmt.Errorf("step %q: model %q must be in the form %q/<model> (e.g. %q/claude-sonnet-4-5); bare provider names are only allowed for \"mock\"", st.ID, st.Model, provider, provider)
		}

		if strings.TrimSpace(st.Prompt) == "" {
			return fmt.Errorf("step %q: prompt must not be empty", st.ID)
		}
		if provider == "mock" && st.Fixture == "" {
			return fmt.Errorf("step %q: mock provider requires fixture path", st.ID)
		}
		if provider != "mock" && st.Fixture != "" {
			return fmt.Errorf("step %q: fixture is only valid for mock provider (got %q)", st.ID, provider)
		}
		if st.Timeout != "" {
			// Duration parsing happens at compile-time; here we only
			// reject the empty string form to surface authoring errors
			// earlier. The compile pass re-validates.
			if strings.TrimSpace(st.Timeout) == "" {
				return fmt.Errorf("step %q: timeout must not be blank", st.ID)
			}
		}
	}

	if out := c.OutputFrom(); out != "" {
		if _, ok := seen[out]; !ok {
			return fmt.Errorf("chain.output.from references unknown step %q", out)
		}
		// v1 constraint: chain.output.from must reference the last
		// step. Supporting an intermediate step would require the
		// runtime to either surface a non-terminal task's payload or
		// truncate the pipeline at an earlier stage — both break the
		// "chains compile into a normal pipeline" model. Authors who
		// genuinely need intermediate-step output should graduate to
		// pipeline mode via `overlord chain export`, where any stage
		// can route to `done` explicitly.
		lastID := c.Steps[len(c.Steps)-1].ID
		if out != lastID {
			return fmt.Errorf("chain.output.from references step %q but must reference the last step %q (v1 limitation: intermediate-step output is not supported — graduate via `overlord chain export`)", out, lastID)
		}
	}

	// Template-reference checks: {{steps.<id>.output}} must point at a
	// step that runs earlier than the referencing step; {{vars.<name>}}
	// must be declared; {{input}} is always legal.
	for i, st := range c.Steps {
		refs := Placeholders(st.Prompt)
		for _, ref := range refs {
			switch {
			case ref == "input":
				// always legal
			case strings.HasPrefix(ref, "vars."):
				key := strings.TrimPrefix(ref, "vars.")
				if _, ok := c.Vars[key]; !ok {
					return fmt.Errorf("step %q: prompt references unknown var %q", st.ID, key)
				}
			case strings.HasPrefix(ref, "steps."):
				inner := strings.TrimPrefix(ref, "steps.")
				parts := strings.SplitN(inner, ".", 2)
				if len(parts) != 2 || parts[1] != "output" {
					return fmt.Errorf("step %q: invalid reference %q (expected steps.<id>.output)", st.ID, ref)
				}
				targetID := parts[0]
				targetIdx, ok := stepsBeforeMap[targetID]
				if !ok {
					return fmt.Errorf("step %q: prompt references unknown step %q", st.ID, targetID)
				}
				if targetIdx >= i {
					return fmt.Errorf("step %q: references step %q which runs at or after the current step", st.ID, targetID)
				}
			default:
				return fmt.Errorf("step %q: unsupported placeholder %q (allowed: input, vars.*, steps.<id>.output)", st.ID, ref)
			}
		}
	}

	return nil
}

// splitModel parses a "<provider>/<model>" string into its two
// components. The bare return flag distinguishes "no slash present"
// (bare provider form, e.g. "mock") from "slash present with empty
// halves". The validator consumes bare to enforce that only the mock
// provider may omit the model.
func splitModel(s string) (provider, model string, bare bool, err error) {
	idx := strings.Index(s, "/")
	if idx < 0 {
		return s, "", true, nil
	}
	provider = s[:idx]
	model = s[idx+1:]
	if provider == "" {
		return "", "", false, fmt.Errorf("model %q has empty provider", s)
	}
	if model == "" {
		return "", "", false, fmt.Errorf("model %q has empty model after %q/", s, provider)
	}
	return provider, model, false, nil
}

// validateInlineOutputSchema ensures a user-authored output.schema
// round-trips to valid JSON and compiles as a JSONSchema. The chain
// compiler will compile the same schema bytes into the runtime
// registry, so failing fast here surfaces authoring errors at load
// time — any caller that goes through chain.Load (chain run, chain
// inspect, chain export) sees the error before the broker starts.
func validateInlineOutputSchema(schema map[string]any) error {
	if len(schema) == 0 {
		return nil
	}
	data, err := json.Marshal(schema)
	if err != nil {
		return fmt.Errorf("cannot serialize to JSON: %w", err)
	}
	if _, err := contract.NewRegistryFromRaw([]contract.RawSchemaEntry{{
		Name:    jsonSchemaName,
		Version: jsonSchemaVersion,
		Data:    data,
	}}); err != nil {
		return fmt.Errorf("invalid JSONSchema: %w", err)
	}
	return nil
}

// providerList returns a stable human-readable list for error messages.
func providerList() string {
	names := make([]string, 0, len(knownProviders))
	for k := range knownProviders {
		names = append(names, k)
	}
	// Sort so error messages are deterministic.
	for i := 0; i < len(names); i++ {
		for j := i + 1; j < len(names); j++ {
			if names[i] > names[j] {
				names[i], names[j] = names[j], names[i]
			}
		}
	}
	return strings.Join(names, ", ")
}
