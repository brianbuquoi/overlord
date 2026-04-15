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

// LoadAndCreate builds a subprocess-backed agent.Agent from a plugin config.
// Manifest loading and subprocess startup are BOTH deferred to the first
// Execute/HealthCheck call — the registry contract is that adapters must not
// touch the filesystem or validate credentials at construction time.
//
// This is the entry point used by the agent registry when Provider == "plugin".
// It is distinct from the older .so loader (CreatePluginAgent) which loads
// in-process shared-library plugins.
func LoadAndCreate(cfg config.Agent, logger *slog.Logger) (agent.Agent, error) {
	if cfg.ManifestPath == "" {
		return nil, fmt.Errorf("plugin agent %q: manifest path is required (set `manifest:` in the agent config)", cfg.ID)
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &lazyAgent{
		id:           cfg.ID,
		systemPrompt: cfg.SystemPrompt,
		manifestPath: cfg.ManifestPath,
		logger:       logger,
	}, nil
}

// lazyAgent wraps a subprocess Agent and defers manifest loading until first
// use. It satisfies agent.Agent.
type lazyAgent struct {
	id           string
	systemPrompt string
	manifestPath string
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
	manifest, err := LoadManifest(l.manifestPath)
	if err != nil {
		l.err = fmt.Errorf("plugin agent %q: %w", l.id, err)
		return nil, l.err
	}
	a, err := New(l.id, l.systemPrompt, manifest, l.logger)
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
