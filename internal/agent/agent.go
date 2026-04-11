// Package agent defines the Agent interface that every LLM provider adapter
// implements, along with shared types used across adapters.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/brianbuquoi/orcastrator/internal/broker"
)

// ParseJSONObjectOutput validates that an LLM adapter's extracted text output
// is a JSON object, stripping a single pair of surrounding markdown code fences
// if present. On success it returns the raw JSON bytes ready to be stored in
// TaskResult.Payload. On failure it returns a descriptive error — callers wrap
// it as a non-retryable AgentError so the broker routes the task to failure.
//
// The broker validates task payloads against JSONSchema output schemas, which
// require an object at the root. Rejecting non-object output here gives a
// clear provider-level error instead of a generic contract violation.
func ParseJSONObjectOutput(text string) (json.RawMessage, error) {
	cleaned := strings.TrimSpace(text)
	if cleaned == "" {
		return nil, fmt.Errorf("model output is empty")
	}
	if strings.HasPrefix(cleaned, "```") {
		if idx := strings.Index(cleaned, "\n"); idx != -1 {
			cleaned = cleaned[idx+1:]
		}
		if idx := strings.LastIndex(cleaned, "```"); idx != -1 {
			cleaned = cleaned[:idx]
		}
		cleaned = strings.TrimSpace(cleaned)
	}
	if cleaned == "" {
		return nil, fmt.Errorf("model output is empty after stripping code fences")
	}
	var obj map[string]interface{}
	if err := json.Unmarshal([]byte(cleaned), &obj); err != nil {
		raw := text
		if len(raw) > 200 {
			raw = raw[:200]
		}
		return nil, fmt.Errorf("model output is not a valid JSON object: %v (raw: %s)", err, raw)
	}
	return json.RawMessage(cleaned), nil
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
