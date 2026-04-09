// Package copilot implements a stub agent.Agent for GitHub Copilot.
//
// The GitHub Copilot agentic API is not yet publicly available. This adapter
// exists so that "copilot" is a recognized provider in the YAML config and
// the agent registry, ready to be implemented when the API launches.
package copilot

import (
	"context"
	"errors"
	"fmt"

	"github.com/orcastrator/orcastrator/internal/agent"
	"github.com/orcastrator/orcastrator/internal/broker"
)

const providerName = "copilot"

// ErrCopilotNotAvailable is returned by Execute and HealthCheck until
// GitHub ships a public agentic API for Copilot.
var ErrCopilotNotAvailable = errors.New(
	"github copilot agentic API is not yet publicly available",
)

// TODO: Expected future implementation when GitHub ships the Copilot agentic API:
//
// Authentication:
//   - OAuth device flow (similar to `gh auth login`). The user would run a
//     device authorization flow and store the resulting token. The adapter
//     would read the token from an env var (e.g. GITHUB_COPILOT_TOKEN) or
//     from the GitHub CLI credential store (~/.config/gh/hosts.yml).
//
// Endpoint:
//   - Likely https://api.github.com/copilot/chat/completions or a similar
//     path under the GitHub API. The request shape will probably mirror
//     OpenAI's chat completions format since Copilot Chat already uses that
//     convention internally.
//
// Model naming:
//   - GitHub abstracts model names behind aliases like "copilot-chat" or
//     version-pinned identifiers. The adapter should accept whatever string
//     the user provides and pass it through, similar to how the Ollama
//     adapter works.
//
// Implementation:
//   - Once the API is available, this adapter should follow the same patterns
//     as the OpenAI adapter (chat completions shape), with GitHub-specific
//     auth headers (Authorization: token <PAT> or Bearer <oauth-token>).

// Config holds the adapter configuration derived from the YAML agent block.
type Config struct {
	ID    string
	Model string
}

// Adapter implements agent.Agent as a stub for GitHub Copilot.
type Adapter struct {
	cfg Config
}

// New creates a new Copilot stub adapter.
func New(cfg Config) *Adapter {
	return &Adapter{cfg: cfg}
}

func (a *Adapter) ID() string       { return a.cfg.ID }
func (a *Adapter) Provider() string { return providerName }

// Execute always returns ErrCopilotNotAvailable.
func (a *Adapter) Execute(_ context.Context, _ *broker.Task) (*broker.TaskResult, error) {
	return nil, &agent.AgentError{
		Err:       ErrCopilotNotAvailable,
		AgentID:   a.cfg.ID,
		Prov:      providerName,
		Retryable: false,
	}
}

// HealthCheck always returns ErrCopilotNotAvailable.
func (a *Adapter) HealthCheck(_ context.Context) error {
	return fmt.Errorf("copilot health check: %w", ErrCopilotNotAvailable)
}
