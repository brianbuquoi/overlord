package chain

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/brianbuquoi/overlord/internal/agent"
	"github.com/brianbuquoi/overlord/internal/broker"
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

	// Substitute placeholders inside the (possibly envelope-wrapped)
	// system prompt. Missing {{input}} or {{steps.X.output}} references
	// render as empty strings; the validator already rejected chains
	// that reference unknown or out-of-order steps, so a runtime miss
	// here would have to come from external tampering. We tolerate it
	// silently in the wrapper — the broker's contract validation and
	// sanitizer envelope still provide safety nets downstream.
	rendered, _ := Render(task.Prompt, Scope{
		Vars:    nil, // vars are already resolved at compile time
		Input:   cm.Input,
		Outputs: cm.Outputs,
	})

	inner := *task
	inner.Prompt = rendered

	result, err := a.base.Execute(ctx, &inner)
	if err != nil {
		return nil, err
	}

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
