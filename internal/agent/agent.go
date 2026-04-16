// Package agent defines the Agent interface that every LLM provider adapter
// implements, along with shared types used across adapters.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/brianbuquoi/overlord/internal/broker"
)

// ParseJSONObjectOutput normalizes an LLM adapter's extracted text output
// into a JSON object payload. Surrounding markdown code fences are stripped.
// If the cleaned text decodes as a JSON object it is returned unchanged so
// downstream schema validation sees the model's structured response
// verbatim. Any other input — JSON arrays, scalars, plain text, empty
// strings — is wrapped as {"text": "<raw>"} using the original unmodified
// text, giving downstream validation a consistent object shape regardless
// of which adapter produced the output.
//
// Returning an error is effectively impossible: the wrap step marshals a
// map with a fixed key and the raw string, which json.Marshal cannot fail
// on. The signature preserves the error return so callers can continue to
// treat it as a fallible step without further churn.
func ParseJSONObjectOutput(text string) (json.RawMessage, error) {
	cleaned := strings.TrimSpace(text)
	if strings.HasPrefix(cleaned, "```") {
		if idx := strings.Index(cleaned, "\n"); idx != -1 {
			cleaned = cleaned[idx+1:]
		}
		if idx := strings.LastIndex(cleaned, "```"); idx != -1 {
			cleaned = cleaned[:idx]
		}
		cleaned = strings.TrimSpace(cleaned)
	}
	var obj map[string]interface{}
	if err := json.Unmarshal([]byte(cleaned), &obj); err == nil {
		return json.RawMessage(cleaned), nil
	}
	wrapped, err := json.Marshal(map[string]any{"text": text})
	if err != nil {
		return nil, fmt.Errorf("wrap non-JSON output: %w", err)
	}
	return wrapped, nil
}

// Truncate returns the first n characters of s, appending "...(truncated)" if
// the input exceeds n. It is intended for debug logging of prompt/response
// content where a preview is sufficient. The helper must never be applied to
// credentials — only to non-secret fields such as system prompts, user message
// bodies, and raw model responses.
func Truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "...(truncated)"
}

// Agent is the interface every LLM provider adapter implements.
type Agent interface {
	ID() string
	Provider() string
	Execute(ctx context.Context, task *broker.Task) (*broker.TaskResult, error)
	HealthCheck(ctx context.Context) error
}

// Stopper is implemented by agents that manage external resources (e.g.
// subprocess plugin agents) and require explicit cleanup on shutdown.
// Not all agents implement Stopper — built-in LLM adapters do not, since
// they hold only HTTP clients that clean up via GC.
type Stopper interface {
	Stop() error
}

// Drainer is implemented by agents that support graceful draining before
// shutdown. Call Drain() to stop new work from being dispatched, wait for
// in-flight work to complete (poll InFlightCount), then call Stop() to
// terminate the subprocess.
type Drainer interface {
	Drain()
	IsDraining() bool
	InFlightCount() int32
}

// AgentError wraps an underlying error with provider context and retryability.
type AgentError struct {
	Err        error         `json:"-"`
	AgentID    string        `json:"agent_id"`
	Prov       string        `json:"provider"`
	Retryable  bool          `json:"retryable"`
	RetryAfter time.Duration `json:"retry_after,omitempty"` // hint from provider (e.g. 429 Retry-After)
}

func (e *AgentError) Error() string {
	return fmt.Sprintf("agent %s (%s): %v", e.AgentID, e.Prov, e.Err)
}

func (e *AgentError) Unwrap() error {
	return e.Err
}

// IsRetryable returns whether the error is retryable.
func (e *AgentError) IsRetryable() bool {
	return e.Retryable
}
