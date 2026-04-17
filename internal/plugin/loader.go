// Package plugin loads Go shared library (.so) plugins at startup and
// registers them as agent providers. Each plugin exports a single symbol:
//
//	var Plugin plugin.AgentPlugin
//
// Plugins are loaded with the full trust of the Overlord process.
package plugin

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	goplugin "plugin"

	"github.com/brianbuquoi/overlord/internal/broker"
	"github.com/brianbuquoi/overlord/internal/config"
	pluginapi "github.com/brianbuquoi/overlord/pkg/plugin"
)

// validID matches safe identifiers — same regex as config.validateID.
var validID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

// reservedProviders are built-in provider names that plugins cannot override.
var reservedProviders = map[string]bool{
	"anthropic": true,
	"google":    true,
	"openai":    true,
	"ollama":    true,
	"copilot":   true,
}

// PluginOpener abstracts plugin.Open for testing. The default implementation
// uses Go's plugin package. Tests can replace this with a mock.
type PluginOpener func(path string) (PluginLookup, error)

// PluginLookup abstracts symbol lookup from a loaded plugin.
type PluginLookup interface {
	Lookup(symName string) (goplugin.Symbol, error)
}

// DefaultOpener uses Go's plugin.Open to load .so files.
func DefaultOpener(path string) (PluginLookup, error) {
	return goplugin.Open(path)
}

// LoadPlugins discovers and loads plugin .so files from the given config.
// Returns a map of provider name → AgentPlugin. Errors are returned for:
//   - plugins.enabled is false but .so inputs are present (fail-closed)
//   - a resolved .so path is not in plugins.allow (when non-empty)
//   - .so file not found or not loadable
//   - "Plugin" symbol not found in the .so
//   - Symbol does not implement AgentPlugin
//   - Two plugins register the same provider name
//   - Plugin attempts to use a reserved provider name
func LoadPlugins(cfg config.PluginConfig, logger *slog.Logger) (map[string]pluginapi.AgentPlugin, error) {
	return loadPlugins(cfg, logger, DefaultOpener)
}

func loadPlugins(cfg config.PluginConfig, logger *slog.Logger, opener PluginOpener) (map[string]pluginapi.AgentPlugin, error) {
	paths, err := resolvePaths(cfg)
	if err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		return map[string]pluginapi.AgentPlugin{}, nil
	}
	// Fail-closed: even if config.Validate was bypassed, LoadPlugins
	// refuses to dlopen anything unless the operator explicitly opted in.
	if !cfg.Enabled {
		return nil, fmt.Errorf(
			"plugins are disabled but %d .so input(s) resolved; set plugins.enabled: true in the config to load plugins",
			len(paths),
		)
	}
	// Enforce the optional allowlist against resolved absolute paths so
	// symlinks and relative paths cannot sneak past an operator's list.
	for i, p := range paths {
		abs, err := filepath.Abs(p)
		if err != nil {
			return nil, fmt.Errorf("plugin %q: resolve absolute path: %w", p, err)
		}
		paths[i] = abs
		if !cfg.PathAllowed(abs) {
			return nil, fmt.Errorf(
				"plugin %q is not in plugins.allow; add it to the allowlist to permit this plugin",
				abs,
			)
		}
	}
	return loadFromPaths(paths, logger, opener)
}

// loadFromPaths loads plugins from pre-resolved file paths.
func loadFromPaths(paths []string, logger *slog.Logger, opener PluginOpener) (map[string]pluginapi.AgentPlugin, error) {
	plugins := make(map[string]pluginapi.AgentPlugin)

	for _, path := range paths {
		ap, err := loadOne(path, opener)
		if err != nil {
			return nil, err
		}

		name := ap.ProviderName()
		if name == "" || !validID.MatchString(name) {
			return nil, fmt.Errorf("plugin %q: ProviderName() returned invalid identifier %q (must match %s)", path, name, validID.String())
		}
		if reservedProviders[name] {
			return nil, fmt.Errorf("plugin %q: provider name %q is reserved (built-in providers: anthropic, google, openai, ollama, copilot)", path, name)
		}
		if existing, ok := plugins[name]; ok {
			_ = existing // both register same name
			return nil, fmt.Errorf("plugin %q: provider name %q conflicts with an already-loaded plugin", path, name)
		}

		logger.Warn("Loading plugin — ensure this file is trusted", "path", path, "provider", name)
		plugins[name] = ap
	}

	return plugins, nil
}

// loadOne loads a single .so file and extracts the AgentPlugin.
func loadOne(path string, opener PluginOpener) (ap pluginapi.AgentPlugin, err error) {
	// Recover from panics during plugin loading.
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("plugin %q: panic during load: %v", path, r)
		}
	}()

	p, err := opener(path)
	if err != nil {
		return nil, fmt.Errorf("plugin %q: failed to open: %w", path, err)
	}

	sym, err := p.Lookup("Plugin")
	if err != nil {
		return nil, fmt.Errorf("plugin %q: missing \"Plugin\" symbol — every plugin must export: var Plugin plugin.AgentPlugin", path)
	}

	ptr, ok := sym.(*pluginapi.AgentPlugin)
	if !ok {
		return nil, fmt.Errorf("plugin %q: \"Plugin\" symbol has type %T, expected *plugin.AgentPlugin", path, sym)
	}

	return *ptr, nil
}

// resolvePaths collects .so file paths from the plugin config.
func resolvePaths(cfg config.PluginConfig) ([]string, error) {
	var paths []string

	// Explicit file list takes priority.
	for _, f := range cfg.Files {
		if _, err := os.Stat(f); err != nil {
			return nil, fmt.Errorf("plugin file not found: %s", f)
		}
		paths = append(paths, f)
	}

	// Scan directory for .so files.
	if cfg.Dir != "" {
		entries, err := os.ReadDir(cfg.Dir)
		if err != nil {
			return nil, fmt.Errorf("plugin directory %q: %w", cfg.Dir, err)
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			if strings.HasSuffix(e.Name(), ".so") {
				paths = append(paths, filepath.Join(cfg.Dir, e.Name()))
			}
		}
	}

	return paths, nil
}

// PluginAgentAdapter wraps a plugin's Agent to satisfy the broker.Agent interface.
// It converts between pkg/plugin types and internal broker types.
type PluginAgentAdapter struct {
	inner pluginapi.Agent
}

// NewPluginAgentAdapter wraps a plugin agent so it can be used by the broker.
func NewPluginAgentAdapter(a pluginapi.Agent) broker.Agent {
	return &PluginAgentAdapter{inner: a}
}

func (a *PluginAgentAdapter) ID() string       { return a.inner.ID() }
func (a *PluginAgentAdapter) Provider() string { return a.inner.Provider() }

func (a *PluginAgentAdapter) Execute(ctx context.Context, task *broker.Task) (*broker.TaskResult, error) {
	// Convert broker.Task → plugin.Task with a defensive copy of
	// Metadata so plugin mutations don't corrupt broker state.
	pt := &pluginapi.Task{
		ID:                  task.ID,
		PipelineID:          task.PipelineID,
		StageID:             task.StageID,
		InputSchemaName:     task.InputSchemaName,
		InputSchemaVersion:  task.InputSchemaVersion,
		OutputSchemaName:    task.OutputSchemaName,
		OutputSchemaVersion: task.OutputSchemaVersion,
		Payload:             task.Payload,
		Metadata:            copyMap(task.Metadata),
		Attempts:            task.Attempts,
		MaxAttempts:         task.MaxAttempts,
	}

	result, err := a.inner.Execute(ctx, pt)
	if err != nil {
		return nil, err
	}

	// Convert plugin.TaskResult → broker.TaskResult with a defensive
	// copy of Metadata so plugin-held references can't mutate broker state.
	return &broker.TaskResult{
		TaskID:   result.TaskID,
		Payload:  result.Payload,
		Metadata: copyMap(result.Metadata),
		Error:    result.Error,
	}, nil
}

// copyMap returns a shallow copy of m. Returns nil for nil input.
func copyMap(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func (a *PluginAgentAdapter) HealthCheck(ctx context.Context) error {
	return a.inner.HealthCheck(ctx)
}

// CreatePluginAgent creates a broker.Agent from a plugin and agent config.
// It resolves auth env vars and maps config fields to PluginAgentConfig.
func CreatePluginAgent(ap pluginapi.AgentPlugin, cfg config.Agent) (agent broker.Agent, err error) {
	// Recover from panics in plugin's NewAgent.
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("plugin %q panicked in NewAgent: %v", cfg.Provider, r)
		}
	}()

	auth := make(map[string]string)
	if cfg.Auth.APIKeyEnv != "" {
		auth[cfg.Auth.APIKeyEnv] = os.Getenv(cfg.Auth.APIKeyEnv)
	}

	pcfg := pluginapi.PluginAgentConfig{
		ID:           cfg.ID,
		Model:        cfg.Model,
		Auth:         auth,
		SystemPrompt: cfg.SystemPrompt,
		Temperature:  cfg.Temperature,
		MaxTokens:    cfg.MaxTokens,
		Timeout:      cfg.Timeout.Duration,
		Extra:        cfg.Extra,
	}

	inner, err := ap.NewAgent(pcfg)
	if err != nil {
		return nil, fmt.Errorf("plugin %q: NewAgent failed: %w", cfg.Provider, err)
	}

	return NewPluginAgentAdapter(inner), nil
}
