// Package registry provides a factory for creating agent adapters from config.
package registry

import (
	"fmt"
	"log/slog"

	"github.com/orcastrator/orcastrator/internal/agent"
	"github.com/orcastrator/orcastrator/internal/agent/anthropic"
	"github.com/orcastrator/orcastrator/internal/agent/copilot"
	"github.com/orcastrator/orcastrator/internal/agent/google"
	"github.com/orcastrator/orcastrator/internal/agent/ollama"
	"github.com/orcastrator/orcastrator/internal/agent/openai"
	"github.com/orcastrator/orcastrator/internal/config"
	"github.com/orcastrator/orcastrator/internal/metrics"
)

// NewFromConfig creates the appropriate Agent adapter based on the provider
// field in the agent configuration.
func NewFromConfig(cfg config.Agent, logger *slog.Logger, m ...*metrics.Metrics) (agent.Agent, error) {
	switch cfg.Provider {
	case "anthropic":
		return anthropic.New(anthropic.Config{
			ID:           cfg.ID,
			Model:        cfg.Model,
			APIKeyEnv:    cfg.Auth.APIKeyEnv,
			SystemPrompt: cfg.SystemPrompt,
			Temperature:  cfg.Temperature,
			MaxTokens:    cfg.MaxTokens,
			Timeout:      cfg.Timeout.Duration,
		}, logger, m...)

	case "openai":
		return openai.New(openai.Config{
			ID:           cfg.ID,
			Model:        cfg.Model,
			APIKeyEnv:    cfg.Auth.APIKeyEnv,
			SystemPrompt: cfg.SystemPrompt,
			Temperature:  cfg.Temperature,
			MaxTokens:    cfg.MaxTokens,
			Timeout:      cfg.Timeout.Duration,
		}, logger, m...)

	case "google":
		return google.New(google.Config{
			ID:           cfg.ID,
			Model:        cfg.Model,
			APIKeyEnv:    cfg.Auth.APIKeyEnv,
			SystemPrompt: cfg.SystemPrompt,
			Temperature:  cfg.Temperature,
			MaxTokens:    cfg.MaxTokens,
			Timeout:      cfg.Timeout.Duration,
		}, logger, m...)

	case "ollama":
		return ollama.New(ollama.Config{
			ID:           cfg.ID,
			Model:        cfg.Model,
			SystemPrompt: cfg.SystemPrompt,
			Temperature:  cfg.Temperature,
			MaxTokens:    cfg.MaxTokens,
			Timeout:      cfg.Timeout.Duration,
		}, logger, m...)

	case "copilot":
		return copilot.New(copilot.Config{
			ID:    cfg.ID,
			Model: cfg.Model,
		}), nil

	default:
		return nil, fmt.Errorf(
			"unknown agent provider %q for agent %q — valid providers: anthropic, openai, google, ollama, copilot",
			cfg.Provider, cfg.ID,
		)
	}
}
