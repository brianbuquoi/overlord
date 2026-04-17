// Package chain is Overlord's lightweight authoring layer for prompt
// workflows. A chain is a linear sequence of model calls authored in a
// minimal YAML shape; the compiler in this package lowers a chain into
// the strict pipeline configuration the broker runtime already accepts,
// so simple authoring and production-grade orchestration share a single
// engine.
//
// Chain mode intentionally keeps the YAML narrow: a chain has an id,
// optional input declaration, optional variables, an ordered list of
// steps (each a prompt + a model reference), and an optional output
// declaration. Everything else — per-step retry, store type, contracts,
// schema registry — is synthesized at compile time with conservative
// defaults. The compiler is the single place where simple authoring
// becomes strict runtime behavior; the broker sees a normal
// config.Config and does not know chain mode exists.
//
// Graduating to full pipeline mode is the `overlord chain export`
// command plus hand-editing: the exported YAML is the compiler's output,
// verbatim, and is a valid overlord run / overlord exec input.
package chain

// File is the top-level shape of a chain YAML document.
//
// A file carries exactly one chain today; the struct is split out so
// that the loader can validate the version marker independently of the
// chain body, and so a future multi-chain file format (not in v1 scope)
// could be added without rewriting the chain body shape.
type File struct {
	Version string `yaml:"version"`
	Chain   *Chain `yaml:"chain"`
}

// Chain is a single authored chain.
type Chain struct {
	// ID names the chain. Compiled into the pipeline name. Must match
	// the same identifier regex used by pipelines (see internal/config).
	ID string `yaml:"id"`

	// Input describes how the chain accepts its initial input. Optional;
	// defaults to text input.
	Input *Input `yaml:"input,omitempty"`

	// Vars is a flat map of string-valued variables referenceable from
	// any step's prompt via {{vars.<name>}}. Resolved at compile time.
	Vars map[string]string `yaml:"vars,omitempty"`

	// Steps is the ordered list of LLM calls that make up the chain.
	// Must be non-empty. v1 supports linear chains only.
	Steps []Step `yaml:"steps"`

	// Output describes how the final step's payload is surfaced to the
	// caller. Optional; defaults to text output from the last step.
	Output *Output `yaml:"output,omitempty"`
}

// Input declares how the chain receives its initial payload.
type Input struct {
	// Type is either "text" (default) or "json":
	//
	//   - text: the CLI accepts a raw string via --input/--input-file
	//     and wraps it as {"text": "..."} on the wire so every
	//     built-in adapter sees a uniform object. {{input}} in step
	//     prompts renders as that raw string.
	//   - json: the CLI accepts a JSON object verbatim. The JSON is
	//     forwarded unchanged to the first stage. {{input}} in step
	//     prompts renders as the full original JSON object string
	//     (never a single field of it) so authors can feed the whole
	//     object into a prompt, or pluck individual fields with their
	//     own string handling on the model side.
	Type string `yaml:"type"`
}

// Step is a single LLM call in a chain.
type Step struct {
	// ID names the step. Used for {{steps.<id>.output}} references
	// and as the stage ID in the compiled pipeline. Must be unique
	// within the chain.
	ID string `yaml:"id"`

	// Model is a "<provider>/<model>" string, e.g.
	// "anthropic/claude-sonnet-4-5", "openai/gpt-4o". The provider
	// portion must be a known built-in provider (see
	// internal/agent/registry). A bare provider (no slash) is accepted
	// **only** for the `mock` provider, where the model string is
	// unused anyway.
	Model string `yaml:"model"`

	// Prompt is the step's system prompt. Template references:
	//   {{input}}             — the chain's initial input text
	//   {{vars.<name>}}       — a variable from chain.vars (compile-time)
	//   {{steps.<id>.output}} — the text output of an earlier step
	Prompt string `yaml:"prompt"`

	// Temperature is an optional model knob passed through to the
	// underlying LLM adapter. 0 means "use the adapter default".
	Temperature float64 `yaml:"temperature,omitempty"`

	// MaxTokens is an optional cap on generated tokens. 0 means "use
	// the adapter default".
	MaxTokens int `yaml:"max_tokens,omitempty"`

	// Timeout overrides the per-step LLM call timeout. Accepts any
	// duration string Go's time.ParseDuration accepts. Empty means
	// "use the compile-time default" (see compile.go).
	Timeout string `yaml:"timeout,omitempty"`

	// Fixture is the relative path to a JSON fixture file, consumed
	// only when the step's provider is "mock". Ignored for any other
	// provider. Scaffolded chain templates set this so demos run
	// without credentials.
	Fixture string `yaml:"fixture,omitempty"`
}

// Output declares the chain's final-output shape.
type Output struct {
	// From is the reference expression the chain runner treats as the
	// final payload. v1 accepts only "steps.<id>.output" and the
	// referenced step must be the chain's **last** step — intermediate
	// step selection is not supported in chain mode. Authors who need
	// it should graduate via `overlord chain export`. Empty means "use
	// the last step's output".
	From string `yaml:"from,omitempty"`

	// Type is either "text" (default) or "json". Controls how the
	// final payload is surfaced by `overlord chain run` and which
	// synthesized schema the last stage validates against.
	Type string `yaml:"type,omitempty"`

	// Schema is an optional inline JSONSchema the last stage's output
	// payload is validated against. Only valid when Type is "json";
	// setting it with any other type is a validation error. The shape
	// is a map that YAML parses directly and the compiler serializes
	// to JSON before handing to the schema registry — so any valid
	// JSONSchema fragment works (type, required, properties, etc.).
	//
	// v1 keeps this deliberately lightweight: there is no $ref support,
	// no version bumping, and no registry ceremony. The schema is
	// synthesized under the reserved name chain_json@v1. Authors who
	// need full schema-registry semantics should graduate via
	// `overlord chain export` and hand-edit the exported pipeline.
	Schema map[string]any `yaml:"schema,omitempty"`
}

// InputType returns the normalized input type, defaulting to "text".
func (c *Chain) InputType() string {
	if c == nil || c.Input == nil || c.Input.Type == "" {
		return "text"
	}
	return c.Input.Type
}

// OutputType returns the normalized output type, defaulting to "text".
func (c *Chain) OutputType() string {
	if c == nil || c.Output == nil || c.Output.Type == "" {
		return "text"
	}
	return c.Output.Type
}

// OutputFrom returns the step ID that the final payload is sourced
// from, defaulting to the last step's ID. Empty string if the chain
// has no steps.
func (c *Chain) OutputFrom() string {
	if c == nil || len(c.Steps) == 0 {
		return ""
	}
	if c.Output == nil || c.Output.From == "" {
		return c.Steps[len(c.Steps)-1].ID
	}
	return stepFromRef(c.Output.From)
}

// stepFromRef parses "steps.<id>.output" and returns the id portion.
// Any other form is returned as-is so the validator can surface it as
// an explicit error with the original text intact.
func stepFromRef(ref string) string {
	const prefix = "steps."
	const suffix = ".output"
	if len(ref) <= len(prefix)+len(suffix) {
		return ref
	}
	if ref[:len(prefix)] != prefix {
		return ref
	}
	if ref[len(ref)-len(suffix):] != suffix {
		return ref
	}
	return ref[len(prefix) : len(ref)-len(suffix)]
}
