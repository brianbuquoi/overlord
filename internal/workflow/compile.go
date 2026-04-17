package workflow

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/brianbuquoi/overlord/internal/chain"
	"github.com/brianbuquoi/overlord/internal/config"
)

// Compiled is the lowered form of a workflow. It wraps the chain-layer
// compile result so callers keep access to the synthesized schemas
// and resolved templates without peeking into the chain package
// directly. Runtime carries whatever `runtime:` block the workflow
// declared, applied onto Chain.Config so `overlord serve` observes the
// author's bind/store/auth choices.
type Compiled struct {
	*chain.Compiled

	// Workflow is the source workflow the compiler started from.
	Workflow *Workflow

	// Runtime is the resolved runtime block with defaults applied
	// (never nil — a workflow without a runtime block still gets
	// sensible serve defaults).
	Runtime *Runtime
}

// prevRE matches {{prev}} placeholders (with any surrounding
// whitespace) used as an ergonomic shorthand for
// {{steps.<previous>.output}}. The compiler rewrites prev into a
// concrete step reference before handing the workflow off to the
// chain lowering so the chain layer never has to learn about prev.
var prevRE = regexp.MustCompile(`\{\{\s*prev\s*\}\}`)

// Compile lowers a workflow into a runtime-ready record. basePath is
// the directory relative fixture paths resolve against; pass empty
// when the workflow has no mock-provider steps.
func Compile(file *File, basePath string) (*Compiled, error) {
	if file == nil || file.Workflow == nil {
		return nil, fmt.Errorf("compile: empty workflow")
	}
	if err := Validate(file.Workflow); err != nil {
		return nil, err
	}

	ch, err := toChain(file.Workflow)
	if err != nil {
		return nil, err
	}

	compiledChain, err := chain.CompileWithBase(ch, basePath)
	if err != nil {
		return nil, err
	}

	rt := resolveRuntime(file.Runtime)
	if err := applyRuntime(compiledChain.Config, rt); err != nil {
		return nil, err
	}

	return &Compiled{
		Compiled: compiledChain,
		Workflow: file.Workflow,
		Runtime:  rt,
	}, nil
}

// toChain translates a workflow into the chain-layer shape the chain
// compiler already understands. Step IDs are auto-generated when blank;
// {{prev}} shorthand is rewritten to {{steps.<prior>.output}}.
func toChain(w *Workflow) (*chain.Chain, error) {
	steps := make([]chain.Step, 0, len(w.Steps))
	prevID := ""
	used := map[string]bool{}
	for i, src := range w.Steps {
		id := src.ID
		if id == "" {
			id = autoStepID(i, used)
		}
		used[id] = true

		prompt, err := rewritePrev(src.Prompt, prevID, id)
		if err != nil {
			return nil, err
		}

		steps = append(steps, chain.Step{
			ID:          id,
			Model:       src.Model,
			Prompt:      prompt,
			Temperature: src.Temperature,
			MaxTokens:   src.MaxTokens,
			Timeout:     src.Timeout,
			Fixture:     src.Fixture,
		})
		prevID = id
	}

	cin := (*chain.Input)(nil)
	if w.Input != nil {
		cin = &chain.Input{Type: w.Input.Type}
	}

	cout := (*chain.Output)(nil)
	if w.Output != nil {
		cout = &chain.Output{Type: w.Output.Type, Schema: w.Output.Schema}
	}

	return &chain.Chain{
		ID:     w.ID,
		Input:  cin,
		Vars:   w.Vars,
		Steps:  steps,
		Output: cout,
	}, nil
}

// rewritePrev substitutes `{{prev}}` with `{{steps.<prior>.output}}`
// in a step's prompt. The first step has no prior, so any `{{prev}}`
// reference there is a hard error (clearer than letting the chain
// validator complain about an unknown step).
func rewritePrev(prompt, priorID, currentID string) (string, error) {
	if !prevRE.MatchString(prompt) {
		return prompt, nil
	}
	if priorID == "" {
		return "", fmt.Errorf("step %q: {{prev}} is not available on the first step", currentID)
	}
	replacement := "{{steps." + priorID + ".output}}"
	return prevRE.ReplaceAllString(prompt, replacement), nil
}

// autoStepID picks the next available step_N identifier. Collisions
// with author-declared IDs are rare (authors either name all steps or
// none), but we guard anyway so compile never produces duplicate
// stage IDs.
func autoStepID(idx int, used map[string]bool) string {
	base := fmt.Sprintf("step_%d", idx+1)
	if !used[base] {
		return base
	}
	for n := 1; ; n++ {
		candidate := fmt.Sprintf("%s_%d", base, n)
		if !used[candidate] {
			return candidate
		}
	}
}

// resolveRuntime returns a non-nil Runtime with author choices and
// sensible defaults merged. Callers should treat the returned value as
// the authoritative runtime view.
func resolveRuntime(rt *Runtime) *Runtime {
	out := Runtime{}
	if rt != nil {
		out = *rt
	}
	if out.Bind == "" {
		out.Bind = "127.0.0.1:8080"
	}
	if out.Store == nil {
		out.Store = &RuntimeStore{Type: "memory"}
	}
	if out.Store.Type == "" {
		out.Store.Type = "memory"
	}
	return &out
}

// applyRuntime overlays the workflow's runtime block onto the
// compiled strict config. The chain compiler already produced a
// config with memory-store defaults; applyRuntime upgrades that to
// whatever the author declared so `overlord serve` honors the
// choice.
func applyRuntime(cfg *config.Config, rt *Runtime) error {
	if cfg == nil || rt == nil {
		return nil
	}

	switch strings.ToLower(rt.Store.Type) {
	case "memory":
		if cfg.Stores.Memory == nil {
			cfg.Stores.Memory = &config.MemoryStoreConfig{MaxTasks: 1024}
		}
		for i := range cfg.Pipelines {
			cfg.Pipelines[i].Store = "memory"
		}
	case "postgres":
		if rt.Store.DSNEnv == "" {
			return fmt.Errorf("runtime.store.type=postgres requires dsn_env")
		}
		cfg.Stores.Postgres = &config.PostgresStoreConfig{
			DSNEnv: rt.Store.DSNEnv,
			Table:  rt.Store.Table,
		}
		for i := range cfg.Pipelines {
			cfg.Pipelines[i].Store = "postgres"
		}
	case "redis":
		if rt.Store.URLEnv == "" {
			return fmt.Errorf("runtime.store.type=redis requires url_env")
		}
		cfg.Stores.Redis = &config.RedisStoreConfig{
			URLEnv:    rt.Store.URLEnv,
			KeyPrefix: rt.Store.KeyPrefix,
		}
		for i := range cfg.Pipelines {
			cfg.Pipelines[i].Store = "redis"
		}
	default:
		return fmt.Errorf("runtime.store.type %q must be one of memory|redis|postgres", rt.Store.Type)
	}

	if rt.Auth != nil && rt.Auth.Enabled {
		cfg.Auth.Enabled = true
		keys := make([]config.AuthKeyConfig, 0, len(rt.Auth.Keys))
		for _, k := range rt.Auth.Keys {
			keys = append(keys, config.AuthKeyConfig{
				Name:   k.Name,
				KeyEnv: k.KeyEnv,
				Scopes: k.Scopes,
			})
		}
		cfg.Auth.Keys = keys
	}

	if rt.Dashboard != nil {
		cfg.Dashboard.Enabled = rt.Dashboard
	}

	return nil
}

// IsMemoryStore reports whether the compiled workflow runs against an
// in-memory store. `overlord run` uses this to skip postgres/redis
// setup on the one-shot local path — the chain layer's memory store
// is always safe for the single-task case.
func (c *Compiled) IsMemoryStore() bool {
	if c == nil || c.Runtime == nil || c.Runtime.Store == nil {
		return true
	}
	return strings.EqualFold(c.Runtime.Store.Type, "memory") || c.Runtime.Store.Type == ""
}
