// Package agent defines the Agent interface that every LLM provider adapter
// implements, along with shared types used across adapters.
package agent

import (
	"context"
	"fmt"
	"time"

	"github.com/orcastrator/orcastrator/internal/broker"
)

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
