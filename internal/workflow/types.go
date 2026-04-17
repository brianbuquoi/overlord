// Package workflow is Overlord's simple authoring surface. A workflow
// is a linear sequence of LLM calls authored in one YAML file with a
// top-level `workflow:` block. The workflow compiler lowers the
// workflow into a chain (internal/chain) which is then compiled into
// the strict runtime config.Config the broker already accepts.
//
// Workflows are the default product surface. They intentionally hide
// the schema registry, stage graph, agent binding, and store
// selection the strict format exposes — beginners author one file,
// run it locally with `overlord run`, or serve it with `overlord
// serve`. Graduation to the strict format is an explicit
// `overlord export --advanced` step; the exported project is a
// normal pipeline-mode project that the same runtime accepts.
package workflow

// File is the top-level shape of a workflow YAML document.
type File struct {
	Version  string    `yaml:"version"`
	Workflow *Workflow `yaml:"workflow"`
	Runtime  *Runtime  `yaml:"runtime,omitempty"`
}

// Workflow is a single authored workflow.
type Workflow struct {
	// ID names the workflow. Optional; defaults to the base name of the
	// workflow file (or "workflow" when loaded from bytes). Compiled into
	// the pipeline name.
	ID string `yaml:"id,omitempty"`

	// Input describes how the workflow accepts its initial input.
	// Optional; defaults to text input.
	Input *Input `yaml:"input,omitempty"`

	// Vars is a flat map of string-valued variables referenceable from
	// any step's prompt via {{vars.<name>}}. Resolved at compile time.
	Vars map[string]string `yaml:"vars,omitempty"`

	// Steps is the ordered list of LLM calls the workflow performs.
	// Must be non-empty. Linear execution only — advanced control flow
	// (fan-out, conditional routing, retry budgets) is exposed via
	// `overlord export --advanced`.
	Steps []Step `yaml:"steps"`

	// Output describes how the final step's payload is surfaced to the
	// caller. Optional; defaults to text output from the last step.
	Output *Output `yaml:"output,omitempty"`
}

// Input declares how the workflow receives its initial payload.
// Accepts two YAML shapes: a bare scalar (`input: text`) for the
// common case, or a mapping (`input: { type: text }`) for future
// expansion. Both shapes are equivalent today.
type Input struct {
	// Type is either "text" (default) or "json". Matches chain.Input.Type.
	Type string `yaml:"type"`
}

// UnmarshalYAML accepts Input as either a bare string or a mapping.
func (i *Input) UnmarshalYAML(unmarshal func(any) error) error {
	var s string
	if err := unmarshal(&s); err == nil && s != "" {
		i.Type = s
		return nil
	}
	var m struct {
		Type string `yaml:"type"`
	}
	if err := unmarshal(&m); err != nil {
		return err
	}
	i.Type = m.Type
	return nil
}

// Step is a single LLM call in a workflow.
type Step struct {
	// ID names the step. Optional — the compiler auto-generates
	// `step_N` identifiers when blank. When set, must match the shared
	// identifier character class and be unique within the workflow.
	ID string `yaml:"id,omitempty"`

	// Model is a "<provider>/<model>" string, e.g.
	// "anthropic/claude-sonnet-4-5", "openai/gpt-4o". The mock provider
	// may be referenced as bare `mock` — its model field is unused.
	Model string `yaml:"model"`

	// Prompt is the step's system prompt. Supported placeholders:
	//   {{input}}             — the workflow's initial input
	//   {{prev}}              — shorthand for the previous step's output
	//   {{vars.<name>}}       — a variable from workflow.vars
	//   {{steps.<id>.output}} — the output of an earlier step (only when
	//                           the referenced step has an explicit id)
	Prompt string `yaml:"prompt"`

	// Temperature is an optional model knob.
	Temperature float64 `yaml:"temperature,omitempty"`

	// MaxTokens is an optional cap on generated tokens.
	MaxTokens int `yaml:"max_tokens,omitempty"`

	// Timeout overrides the per-step LLM call timeout. Accepts any
	// duration string Go's time.ParseDuration accepts.
	Timeout string `yaml:"timeout,omitempty"`

	// Fixture is the relative path to a JSON fixture file. Only
	// consumed by the mock provider; required for mock steps, rejected
	// for any other provider.
	Fixture string `yaml:"fixture,omitempty"`
}

// Output declares the workflow's final-output shape. Accepts a bare
// scalar (`output: text`) or a mapping form (`output: { type: json,
// schema: {...} }`).
type Output struct {
	Type   string         `yaml:"type,omitempty"`
	Schema map[string]any `yaml:"schema,omitempty"`
}

// UnmarshalYAML accepts Output as either a bare string or a mapping.
func (o *Output) UnmarshalYAML(unmarshal func(any) error) error {
	var s string
	if err := unmarshal(&s); err == nil && s != "" {
		o.Type = s
		return nil
	}
	var m struct {
		Type   string         `yaml:"type"`
		Schema map[string]any `yaml:"schema"`
	}
	if err := unmarshal(&m); err != nil {
		return err
	}
	o.Type = m.Type
	o.Schema = m.Schema
	return nil
}

// Runtime is the optional block that configures how `overlord serve`
// hosts the workflow. It is ignored by `overlord run` (single-shot
// local execution never binds a port).
type Runtime struct {
	// Bind is the HTTP bind address for `overlord serve`. Accepts
	// "host", "host:port", or ":port". Defaults to 127.0.0.1:8080.
	Bind string `yaml:"bind,omitempty"`

	// Dashboard enables the embedded web dashboard at /dashboard.
	// nil means "use the default" (enabled).
	Dashboard *bool `yaml:"dashboard,omitempty"`

	// Store configures the backing store when the workflow graduates
	// from one-shot local runs to a long-lived serve. Defaults to
	// memory for loopback binds.
	Store *RuntimeStore `yaml:"store,omitempty"`

	// Auth controls API authentication. Optional.
	Auth *RuntimeAuth `yaml:"auth,omitempty"`
}

// RuntimeStore picks a backing store for `overlord serve`.
type RuntimeStore struct {
	// Type is one of "memory" (default), "redis", or "postgres".
	Type string `yaml:"type,omitempty"`

	// DSNEnv is the environment variable name holding the Postgres DSN
	// when Type is "postgres".
	DSNEnv string `yaml:"dsn_env,omitempty"`

	// Table is the Postgres table name when Type is "postgres".
	Table string `yaml:"table,omitempty"`

	// URLEnv is the environment variable name holding the Redis URL
	// when Type is "redis".
	URLEnv string `yaml:"url_env,omitempty"`

	// KeyPrefix is the Redis key prefix when Type is "redis".
	KeyPrefix string `yaml:"key_prefix,omitempty"`
}

// RuntimeAuth declares API-key authentication for `overlord serve`.
type RuntimeAuth struct {
	Enabled bool              `yaml:"enabled,omitempty"`
	Keys    []RuntimeAuthKey  `yaml:"keys,omitempty"`
}

// RuntimeAuthKey mirrors config.AuthKeyConfig for the simple surface.
type RuntimeAuthKey struct {
	Name   string   `yaml:"name"`
	KeyEnv string   `yaml:"key_env"`
	Scopes []string `yaml:"scopes"`
}

// InputType returns the normalized input type, defaulting to "text".
func (w *Workflow) InputType() string {
	if w == nil || w.Input == nil || w.Input.Type == "" {
		return "text"
	}
	return w.Input.Type
}

// OutputType returns the normalized output type, defaulting to "text".
func (w *Workflow) OutputType() string {
	if w == nil || w.Output == nil || w.Output.Type == "" {
		return "text"
	}
	return w.Output.Type
}
