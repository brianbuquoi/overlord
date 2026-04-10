// Package plugin defines the public API for Orcastrator plugin authors.
// Plugin authors implement AgentPlugin and Agent, compile to a .so file,
// and export a package-level variable:
//
//	var Plugin plugin.AgentPlugin
//
// Orcastrator loads the .so at startup and registers the provider.
package plugin

import (
	"context"
	"encoding/json"
	"time"
)

// AgentPlugin is the interface that every plugin must implement.
// The exported "Plugin" variable in the .so must satisfy this interface.
type AgentPlugin interface {
	// ProviderName returns the unique provider name for this plugin.
	// It must not conflict with built-in providers (anthropic, google,
	// openai, ollama, copilot) or other loaded plugins.
	ProviderName() string

	// NewAgent creates a new agent instance from the given configuration.
	NewAgent(cfg PluginAgentConfig) (Agent, error)
}

// PluginAgentConfig holds the configuration passed to a plugin when
// creating an agent. It mirrors the YAML agent config with resolved
// auth values.
type PluginAgentConfig struct {
	ID           string            // Agent ID from YAML config.
	Model        string            // Model name from YAML config.
	Auth         map[string]string // Env var name → resolved value.
	SystemPrompt string            // System prompt text.
	Temperature  float64           // Sampling temperature.
	MaxTokens    int               // Max tokens for response.
	Timeout      time.Duration     // Request timeout.
	Extra        map[string]any    // Arbitrary provider-specific config from YAML.
}

// Agent is the interface that plugin agent implementations must satisfy.
// It mirrors the internal agent.Agent interface so plugin authors do not
// need to import internal packages.
type Agent interface {
	ID() string
	Provider() string
	Execute(ctx context.Context, task *Task) (*TaskResult, error)
	HealthCheck(ctx context.Context) error
}

// Task represents a unit of work passed to an agent for execution.
// This is the plugin-facing view of the internal broker.Task type.
type Task struct {
	ID                  string
	PipelineID          string
	StageID             string
	InputSchemaName     string
	InputSchemaVersion  string
	OutputSchemaName    string
	OutputSchemaVersion string
	Payload             json.RawMessage
	Metadata            map[string]any
	Attempts            int
	MaxAttempts         int
}

// TaskResult holds the output from an agent execution.
type TaskResult struct {
	TaskID   string
	Payload  json.RawMessage
	Metadata map[string]any
	Error    string
}

// AgentError wraps an error with provider context and retryability info.
type AgentError struct {
	Err        error
	AgentID    string
	Provider   string
	Retryable  bool
	RetryAfter time.Duration
}

func (e *AgentError) Error() string {
	return "agent " + e.AgentID + " (" + e.Provider + "): " + e.Err.Error()
}

func (e *AgentError) Unwrap() error {
	return e.Err
}

// IsRetryable returns whether the error is retryable.
func (e *AgentError) IsRetryable() bool {
	return e.Retryable
}
