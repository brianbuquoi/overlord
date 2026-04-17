package plugin

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/brianbuquoi/overlord/internal/agent"
	"github.com/brianbuquoi/overlord/internal/broker"
	"github.com/brianbuquoi/overlord/internal/config"
)

// compile-time interface satisfaction
var _ agent.Agent = (*lazyAgent)(nil)
var _ agent.Stopper = (*lazyAgent)(nil)

// PluginValidationError wraps a configuration-time plugin error (bad manifest,
// missing or non-executable binary) so callers can distinguish it from a
// runtime transport/task-execution error. CLIs that care about exit codes
// (e.g. `overlord exec`) use errors.As with this type to map to the
// config-error exit code rather than the task-failure exit code.
type PluginValidationError struct {
	AgentID string
	Err     error
}

func (e *PluginValidationError) Error() string {
	return fmt.Sprintf("plugin agent %q: %v", e.AgentID, e.Err)
}

func (e *PluginValidationError) Unwrap() error { return e.Err }

// LoadAndCreate builds a subprocess-backed agent.Agent from a plugin config.
// The manifest is loaded and validated eagerly; the binary path is resolved
// and checked for existence and executability. The subprocess itself is
// still started lazily on the first Execute/HealthCheck call.
//
// A validation failure is returned as *PluginValidationError so callers can
// distinguish a config-time error from a runtime task failure.
//
// The policy argument carries the operator's plugins.enabled opt-in and
// optional allowlist. LoadAndCreate refuses to build a plugin agent when
// policy.Enabled is false (fail-closed double-check over config-time
// validation), and refuses when the resolved binary path is not in the
// allowlist (when the allowlist is non-empty).
//
// This is the entry point used by the agent registry when Provider == "plugin".
// It is distinct from the older .so loader (CreatePluginAgent) which loads
// in-process shared-library plugins.
func LoadAndCreate(cfg config.Agent, policy config.PluginConfig, logger *slog.Logger) (agent.Agent, error) {
	if !policy.Enabled {
		return nil, &PluginValidationError{
			AgentID: cfg.ID,
			Err:     fmt.Errorf("plugins are disabled; set plugins.enabled: true in the config to load this agent"),
		}
	}
	if cfg.ManifestPath == "" {
		return nil, &PluginValidationError{
			AgentID: cfg.ID,
			Err:     fmt.Errorf("manifest path is required (set `manifest:` in the agent config)"),
		}
	}
	if logger == nil {
		logger = slog.Default()
	}

	manifest, err := LoadManifest(cfg.ManifestPath)
	if err != nil {
		return nil, &PluginValidationError{AgentID: cfg.ID, Err: err}
	}
	resolved := manifest.ResolveBinary()
	if err := manifest.ValidateBinary(resolved); err != nil {
		return nil, &PluginValidationError{AgentID: cfg.ID, Err: err}
	}
	if !policy.PathAllowed(resolved) {
		return nil, &PluginValidationError{
			AgentID: cfg.ID,
			Err: fmt.Errorf(
				"plugin binary %q is not in plugins.allow; add it to the allowlist to permit this plugin",
				resolved,
			),
		}
	}

	return &lazyAgent{
		id:           cfg.ID,
		systemPrompt: cfg.SystemPrompt,
		manifest:     manifest,
		logger:       logger,
	}, nil
}

// lazyAgent wraps a subprocess Agent and defers subprocess startup until
// first use. The manifest has already been loaded and the binary validated
// at construction time.
type lazyAgent struct {
	id           string
	systemPrompt string
	manifest     *Manifest
	logger       *slog.Logger

	mu    sync.Mutex
	inner *Agent
	err   error
}

func (l *lazyAgent) ID() string       { return l.id }
func (l *lazyAgent) Provider() string { return "plugin" }

func (l *lazyAgent) resolve() (*Agent, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.inner != nil {
		return l.inner, nil
	}
	if l.err != nil {
		return nil, l.err
	}
	a, err := New(l.id, l.systemPrompt, l.manifest, l.logger)
	if err != nil {
		l.err = err
		return nil, err
	}
	l.inner = a
	return a, nil
}

func (l *lazyAgent) Execute(ctx context.Context, task *broker.Task) (*broker.TaskResult, error) {
	a, err := l.resolve()
	if err != nil {
		return nil, err
	}
	return a.Execute(ctx, task)
}

func (l *lazyAgent) HealthCheck(ctx context.Context) error {
	a, err := l.resolve()
	if err != nil {
		return err
	}
	return a.HealthCheck(ctx)
}

// Stop terminates the wrapped subprocess if one was started. Safe to call
// even when the subprocess has not been started (no-op in that case).
func (l *lazyAgent) Stop() error {
	l.mu.Lock()
	inner := l.inner
	l.mu.Unlock()
	if inner == nil {
		return nil
	}
	return inner.Stop()
}
