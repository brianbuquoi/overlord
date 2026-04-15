package registry

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brianbuquoi/overlord/internal/broker"
	"github.com/brianbuquoi/overlord/internal/config"
)

func TestNewFromConfig_Anthropic(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	a, err := NewFromConfig(config.Agent{
		ID:       "a1",
		Provider: "anthropic",
		Model:    "claude-sonnet-4-5",
	}, slog.Default())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if a.ID() != "a1" {
		t.Errorf("ID: want a1, got %s", a.ID())
	}
	if a.Provider() != "anthropic" {
		t.Errorf("Provider: want anthropic, got %s", a.Provider())
	}
}

func TestNewFromConfig_OpenAI(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "test-key")
	a, err := NewFromConfig(config.Agent{
		ID:       "o1",
		Provider: "openai",
		Model:    "gpt-4o",
	}, slog.Default())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if a.Provider() != "openai" {
		t.Errorf("Provider: want openai, got %s", a.Provider())
	}
}

func TestNewFromConfig_Google(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "test-key")
	a, err := NewFromConfig(config.Agent{
		ID:       "g1",
		Provider: "google",
		Model:    "gemini-2.5-pro",
	}, slog.Default())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if a.Provider() != "google" {
		t.Errorf("Provider: want google, got %s", a.Provider())
	}
}

func TestNewFromConfig_Ollama(t *testing.T) {
	a, err := NewFromConfig(config.Agent{
		ID:       "ol1",
		Provider: "ollama",
		Model:    "llama3",
	}, slog.Default())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if a.Provider() != "ollama" {
		t.Errorf("Provider: want ollama, got %s", a.Provider())
	}
}

func TestNewFromConfig_Copilot(t *testing.T) {
	a, err := NewFromConfig(config.Agent{
		ID:       "cp1",
		Provider: "copilot",
		Model:    "copilot-chat",
	}, slog.Default())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if a.Provider() != "copilot" {
		t.Errorf("Provider: want copilot, got %s", a.Provider())
	}
}

// --- Item 9: Unknown provider with actionable error ---

func TestNewFromConfig_UnknownProvider(t *testing.T) {
	_, err := NewFromConfig(config.Agent{
		ID:       "x1",
		Provider: "gpt-wizard-9000",
		Model:    "wizard-xl",
	}, slog.Default())
	if err == nil {
		t.Fatal("expected error for unknown provider")
	}

	errMsg := err.Error()

	if !strings.Contains(errMsg, "unknown agent provider") {
		t.Errorf("error should mention unknown provider, got: %v", errMsg)
	}
	if !strings.Contains(errMsg, "gpt-wizard-9000") {
		t.Errorf("error should include the bad provider name, got: %v", errMsg)
	}

	// The error MUST list all valid providers so the user can fix their config.
	validProviders := []string{"anthropic", "openai", "google", "ollama", "copilot"}
	for _, p := range validProviders {
		if !strings.Contains(errMsg, p) {
			t.Errorf("error must list valid provider %q for user-actionability, got: %v", p, errMsg)
		}
	}
}

// --- Item 10: Build all adapters from example YAML configs ---

// projectRoot returns the module root directory.
func projectRoot(t *testing.T) string {
	t.Helper()
	// Try go env GOMOD to find go.mod, then derive the directory.
	out, err := exec.Command("go", "env", "GOMOD").Output()
	if err == nil {
		mod := strings.TrimSpace(string(out))
		if mod != "" && mod != os.DevNull {
			return filepath.Dir(mod)
		}
	}
	// Fallback: walk up from cwd looking for go.mod.
	dir, _ := os.Getwd()
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("cannot find project root (go.mod)")
		}
		dir = parent
	}
}

func TestNewFromConfig_AllExampleConfigs(t *testing.T) {
	root := projectRoot(t)
	examples, err := filepath.Glob(filepath.Join(root, "config", "examples", "*.yaml"))
	if err != nil {
		t.Fatalf("glob examples: %v", err)
	}
	if len(examples) == 0 {
		t.Fatal("no example YAML files found in config/examples/")
	}

	for _, path := range examples {
		t.Run(filepath.Base(path), func(t *testing.T) {
			cfg, err := config.Load(path)
			if err != nil {
				t.Fatalf("load config: %v", err)
			}

			for _, agentCfg := range cfg.Agents {
				t.Run(agentCfg.ID, func(t *testing.T) {
					// Plugin agents validate their manifest and binary
					// eagerly — this test covers LLM-adapter construction
					// semantics ("no credentials at construction"). Plugin
					// validation has its own coverage in internal/plugin.
					if agentCfg.Provider == "plugin" {
						t.Skip("plugin provider uses eager manifest/binary validation")
					}
					// Adapters must construct without API keys set.
					// Credential errors should only surface at HealthCheck/Execute.
					a, err := NewFromConfig(agentCfg, slog.Default())
					if err != nil {
						t.Fatalf("NewFromConfig failed for agent %q (provider: %s): %v — "+
							"adapters must not validate credentials at construction time",
							agentCfg.ID, agentCfg.Provider, err)
					}
					if a.ID() != agentCfg.ID {
						t.Errorf("ID mismatch: want %s, got %s", agentCfg.ID, a.ID())
					}
					if a.Provider() != agentCfg.Provider {
						t.Errorf("Provider mismatch: want %s, got %s", agentCfg.Provider, a.Provider())
					}
				})
			}
		})
	}
}

func TestStoppers_FiltersByInterface(t *testing.T) {
	nonStop := &fakeAgent{id: "plain"}
	stop := &fakeStoppable{id: "plug"}

	agents := map[string]broker.Agent{
		"plain": nonStop,
		"plug":  stop,
	}
	got := Stoppers(agents)
	if len(got) != 1 {
		t.Fatalf("expected 1 stopper, got %d", len(got))
	}
	if err := got[0].Stop(); err != nil {
		t.Errorf("stop: %v", err)
	}
	if !stop.stopped {
		t.Errorf("Stop was not invoked")
	}
}

func TestStoppers_EmptyMap(t *testing.T) {
	if got := Stoppers(nil); got != nil {
		t.Errorf("expected nil, got %v", got)
	}
	if got := Stoppers(map[string]broker.Agent{}); got != nil {
		t.Errorf("expected nil, got %v", got)
	}
}

type fakeAgent struct {
	id string
}

func (f *fakeAgent) ID() string       { return f.id }
func (f *fakeAgent) Provider() string { return "fake" }
func (f *fakeAgent) Execute(ctx context.Context, task *broker.Task) (*broker.TaskResult, error) {
	return nil, nil
}
func (f *fakeAgent) HealthCheck(ctx context.Context) error { return nil }

type fakeStoppable struct {
	id      string
	stopped bool
}

func (f *fakeStoppable) ID() string       { return f.id }
func (f *fakeStoppable) Provider() string { return "fake-stop" }
func (f *fakeStoppable) Execute(ctx context.Context, task *broker.Task) (*broker.TaskResult, error) {
	return nil, nil
}
func (f *fakeStoppable) HealthCheck(ctx context.Context) error { return nil }
func (f *fakeStoppable) Stop() error {
	f.stopped = true
	return nil
}
