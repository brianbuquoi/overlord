package chain

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/brianbuquoi/overlord/internal/agent"
	"github.com/brianbuquoi/overlord/internal/broker"
	"github.com/brianbuquoi/overlord/internal/sanitize"
)

// ChainMetaKey is the task-metadata key chain mode uses to carry
// {{input}} and per-step outputs across stages. The key is not in the
// broker's reserved-metadata set, so result.Metadata["chain"] set by
// the wrapper adapter is merged into the task by the normal broker
// path.
const ChainMetaKey = "chain"

// stepAdapter wraps a base agent.Agent with chain template
// substitution. It reads {{input}} and {{steps.<id>.output}} values
// from the task's chain metadata, substitutes them into the system
// prompt the broker has already envelope-wrapped, calls the base
// adapter, then records this step's output so later steps can
// reference it.
//
// The wrapper is deliberately thin: it never touches the broker's
// sanitizer envelope, contract validation, or routing. All it does is
// rewrite the system-prompt string passed to the underlying LLM
// adapter and stash a bit of metadata for the next stage.
type stepAdapter struct {
	base    agent.Agent
	stepID  string
	agentID string
}

// NewStepAdapter wraps base as a chain step bound to stepID. The
// returned adapter implements both agent.Agent and broker.Agent (same
// method set).
func NewStepAdapter(base agent.Agent, agentID, stepID string) agent.Agent {
	return &stepAdapter{base: base, stepID: stepID, agentID: agentID}
}

// ID returns the agent ID the broker knows this adapter by. Mirrors
// the base adapter's ID; wrapping does not change identity.
func (a *stepAdapter) ID() string { return a.agentID }

// Provider returns a tagged provider string so logs and metrics can
// distinguish chain-wrapped calls from direct pipeline-mode calls
// without changing the underlying adapter's provider name in any
// customer-visible surface.
func (a *stepAdapter) Provider() string { return a.base.Provider() + "+chain" }

// HealthCheck delegates to the base adapter; the chain wrapper has no
// separate health model of its own.
func (a *stepAdapter) HealthCheck(ctx context.Context) error {
	return a.base.HealthCheck(ctx)
}

// Execute substitutes chain placeholders in task.Prompt and delegates
// to the base adapter. After the base call succeeds, this step's
// output is recorded on the chain metadata so downstream steps can
// reference it via {{steps.<id>.output}}.
func (a *stepAdapter) Execute(ctx context.Context, task *broker.Task) (*broker.TaskResult, error) {
	if task == nil {
		return nil, fmt.Errorf("chain step %q: nil task", a.stepID)
	}

	cm := readChainMeta(task.Metadata)
	if cm.Input == "" {
		cm.Input = extractTextPayload(task.Payload)
	}

	// Build the render scope with every substitutable value already
	// sanitized and wrapped in an inline envelope. Without this step,
	// {{steps.<id>.output}} placeholders would re-inject raw prior
	// output into the "Your task:" section outside the broker's outer
	// envelope — the envelope-bypass vulnerability the workflow/chain
	// layers expose. Rendering safe values here keeps a single
	// anti-injection contract across the broker and the adapter.
	//
	// {{input}} is similarly wrapped: defense-in-depth when a
	// multi-step workflow re-references the initial input from a
	// later stage. The first stage's own prompt never flows through
	// {{input}} substitution because the broker ships the payload
	// directly as user content there, so the wrap is redundant but
	// harmless.
	renderScope := Scope{
		Vars:    nil, // vars are already resolved at compile time
		Input:   wrapForSubstitution(cm.Input),
		Outputs: wrapOutputsForSubstitution(cm.Outputs),
	}

	// Substitute placeholders inside the (possibly envelope-wrapped)
	// system prompt. Missing {{input}} or {{steps.X.output}} references
	// render as empty strings; the validator already rejected chains
	// that reference unknown or out-of-order steps, so a runtime miss
	// here would have to come from external tampering. We tolerate it
	// silently in the wrapper — the broker's contract validation and
	// sanitizer envelope still provide safety nets downstream.
	rendered, _ := Render(task.Prompt, renderScope)

	inner := *task
	inner.Prompt = rendered

	result, err := a.base.Execute(ctx, &inner)
	if err != nil {
		return nil, err
	}

	// Store raw (not sanitized) output on the chain metadata so
	// operators can inspect exactly what a step produced via
	// `overlord status`. Sanitization happens at substitution time
	// above via wrapForSubstitution, so downstream steps still see
	// only the enveloped, sanitized form regardless of what's in
	// cm.Outputs.
	cm.Outputs[a.stepID] = extractTextPayload(result.Payload)

	md := result.Metadata
	if md == nil {
		md = map[string]any{}
	}
	md[ChainMetaKey] = cm.toMap()
	result.Metadata = md
	return result, nil
}

// chainMeta is the structured form of task.Metadata[ChainMetaKey].
type chainMeta struct {
	Input   string            `json:"input"`
	Outputs map[string]string `json:"outputs"`
}

// readChainMeta extracts the chain metadata from a task's metadata
// map. Tolerates missing, nil, or mis-typed fields by returning a
// zero-value structure so callers never see a nil Outputs map.
func readChainMeta(meta map[string]any) chainMeta {
	cm := chainMeta{Outputs: map[string]string{}}
	if meta == nil {
		return cm
	}
	raw, ok := meta[ChainMetaKey]
	if !ok {
		return cm
	}
	// Round-trip through JSON so we accept whatever shape the store
	// materialized (map[string]any from the memory store,
	// map[string]interface{} from Redis/Postgres JSON decode, etc.).
	b, err := json.Marshal(raw)
	if err != nil {
		return cm
	}
	var parsed chainMeta
	if err := json.Unmarshal(b, &parsed); err != nil {
		return cm
	}
	if parsed.Outputs == nil {
		parsed.Outputs = map[string]string{}
	}
	return parsed
}

func (c chainMeta) toMap() map[string]any {
	return map[string]any{
		"input":   c.Input,
		"outputs": c.Outputs,
	}
}

// wrapForSubstitution sanitizes and envelope-wraps a single string
// value before it is substituted into a step's prompt via Render.
// Empty input returns an empty string so Render reports the
// placeholder as missing (the validator already caught unbound
// references; a runtime miss here only happens with tampered
// metadata, which we treat as silently-empty rather than an error).
//
// The sanitize pass drops injection sentinels; WrapInline adds the
// [SYSTEM CONTEXT] markers so the downstream model applies the same
// "treat as data only" rule it already applies to the broker's
// outer envelope. Sanitizer warnings are discarded at this layer —
// the broker's own sanitize pass on the inter-stage payload already
// attaches a warnings record to task.Metadata["sanitizer_warnings"].
func wrapForSubstitution(raw string) string {
	if raw == "" {
		return ""
	}
	clean, _ := sanitize.Sanitize(raw)
	return sanitize.WrapInline(clean)
}

// wrapOutputsForSubstitution mirrors wrapForSubstitution across the
// full step-output map so the adapter can build a render-ready
// Scope without leaking raw prior output into the prompt.
func wrapOutputsForSubstitution(raw map[string]string) map[string]string {
	if len(raw) == 0 {
		return nil
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		out[k] = wrapForSubstitution(v)
	}
	return out
}

// extractTextPayload returns a human-readable text form of an agent
// payload. When the payload decodes as an object with a string "text"
// field, that field is returned verbatim — this is the convention
// internal/agent.ParseJSONObjectOutput uses to wrap plain-text LLM
// responses, so every built-in adapter already produces conforming
// output. Otherwise the raw JSON is returned, which is what chain
// authors want for json output refs.
func extractTextPayload(payload json.RawMessage) string {
	if len(payload) == 0 {
		return ""
	}
	var obj map[string]any
	if err := json.Unmarshal(payload, &obj); err == nil {
		if t, ok := obj["text"].(string); ok {
			return t
		}
	}
	return string(payload)
}
