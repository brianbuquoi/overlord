// Package registry provides a factory for creating agent adapters from config.
package registry

import (
	"fmt"
	"log/slog"

	"github.com/brianbuquoi/overlord/internal/agent"
	"github.com/brianbuquoi/overlord/internal/agent/anthropic"
	"github.com/brianbuquoi/overlord/internal/agent/copilot"
	"github.com/brianbuquoi/overlord/internal/agent/google"
	mockagent "github.com/brianbuquoi/overlord/internal/agent/mock"
	"github.com/brianbuquoi/overlord/internal/agent/ollama"
	"github.com/brianbuquoi/overlord/internal/agent/openai"
	openairesp "github.com/brianbuquoi/overlord/internal/agent/openai_responses"
	"github.com/brianbuquoi/overlord/internal/broker"
	"github.com/brianbuquoi/overlord/internal/config"
	"github.com/brianbuquoi/overlord/internal/contract"
	"github.com/brianbuquoi/overlord/internal/metrics"
	internalplugin "github.com/brianbuquoi/overlord/internal/plugin"
	pluginapi "github.com/brianbuquoi/overlord/pkg/plugin"
)

// NewFromConfig creates the appropriate Agent adapter based on the provider
// field in the agent configuration. Built-in providers are checked first,
// then loaded plugins.
//
// This is a convenience wrapper that constructs providers that need no
// contract registry or stage bindings (every built-in LLM adapter). The
// first-class mock provider is rejected here because it requires a
// compiled registry + the filtered stages to validate fixtures — callers
// that may encounter provider: "mock" must use NewFromConfigWithPlugins.
func NewFromConfig(cfg config.Agent, logger *slog.Logger, m ...*metrics.Metrics) (agent.Agent, error) {
	return NewFromConfigWithPlugins(cfg, nil, logger, nil, "", nil, m...)
}

// NewFromConfigWithPlugins creates an Agent adapter, checking built-in
// providers first and falling back to loaded plugins.
//
// The registry, basePath, and stages parameters are consumed by the mock
// provider for constructor-time fixture validation; every other built-in
// adapter ignores them. stages must already be filtered to the bindings
// that reference cfg.ID (that filtering is the caller's responsibility).
// basePath is the directory that relative fixture paths resolve against
// and should be the same value passed to contract.NewRegistry.
func NewFromConfigWithPlugins(
	cfg config.Agent,
	plugins map[string]pluginapi.AgentPlugin,
	logger *slog.Logger,
	registry *contract.Registry,
	basePath string,
	stages []config.Stage,
	m ...*metrics.Metrics,
) (agent.Agent, error) {
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

	case "openai-responses":
		return openairesp.New(openairesp.Config{
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

	case "mock":
		return mockagent.New(mockagent.Config{
			ID:       cfg.ID,
			Fixtures: cfg.Fixtures,
			BasePath: basePath,
		}, logger, registry, stages, m...)

	case "plugin":
		return internalplugin.LoadAndCreate(cfg, logger)
	}

	// Check plugins.
	if plugins != nil {
		if ap, ok := plugins[cfg.Provider]; ok {
			brokerAgent, err := internalplugin.CreatePluginAgent(ap, cfg)
			if err != nil {
				return nil, err
			}
			// PluginAgentAdapter implements broker.Agent; cast to agent.Agent
			// via the identical interface.
			return brokerAgent.(agent.Agent), nil
		}
	}

	return nil, fmt.Errorf(
		"unknown agent provider %q for agent %q — valid providers: anthropic, openai, openai-responses, google, ollama, copilot, mock, plugin",
		cfg.Provider, cfg.ID,
	)
}

// StagesForAgent returns the subset of stages whose agent binding
// references agentID — either as a single-agent stage or as a fan-out
// participant. Callers pass the result to NewFromConfigWithPlugins so the
// mock adapter constructor sees only the bindings relevant to its own ID.
func StagesForAgent(pipelines []config.Pipeline, agentID string) []config.Stage {
	var out []config.Stage
	for _, p := range pipelines {
		for _, s := range p.Stages {
			if s.Agent == agentID {
				out = append(out, s)
				continue
			}
			if s.FanOut != nil {
				for _, fa := range s.FanOut.Agents {
					if fa.ID == agentID {
						out = append(out, s)
						break
					}
				}
			}
		}
	}
	return out
}

// Stoppers returns all agents from the given registry map that implement
// agent.Stopper. Call Stop() on each during shutdown, after the broker has
// drained. Built-in LLM adapters do not implement Stopper; plugin
// subprocess agents do.
func Stoppers(agents map[string]broker.Agent) []agent.Stopper {
	if len(agents) == 0 {
		return nil
	}
	stoppers := make([]agent.Stopper, 0, len(agents))
	for _, a := range agents {
		if s, ok := a.(agent.Stopper); ok {
			stoppers = append(stoppers, s)
		}
	}
	return stoppers
}

// Drainers returns all agents from the given registry map that implement
// agent.Drainer. Used during hot-reload to drain in-flight RPCs before
// stopping old plugin subprocesses.
func Drainers(agents map[string]broker.Agent) []agent.Drainer {
	if len(agents) == 0 {
		return nil
	}
	drainers := make([]agent.Drainer, 0, len(agents))
	for _, a := range agents {
		if d, ok := a.(agent.Drainer); ok {
			drainers = append(drainers, d)
		}
	}
	return drainers
}
