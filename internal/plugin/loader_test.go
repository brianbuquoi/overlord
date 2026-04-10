package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	goplugin "plugin"
	"testing"

	"github.com/orcastrator/orcastrator/internal/broker"
	pluginapi "github.com/orcastrator/orcastrator/pkg/plugin"
)

// --- mock helpers ---

// mockPlugin implements pluginapi.AgentPlugin for testing.
type mockPlugin struct {
	name string
}

func (p *mockPlugin) ProviderName() string { return p.name }
func (p *mockPlugin) NewAgent(cfg pluginapi.PluginAgentConfig) (pluginapi.Agent, error) {
	return nil, nil
}

// mockLookup simulates a loaded .so file.
type mockLookup struct {
	sym     goplugin.Symbol
	symName string
	err     error
}

func (m *mockLookup) Lookup(symName string) (goplugin.Symbol, error) {
	if m.err != nil {
		return nil, m.err
	}
	if m.symName != "" && symName != m.symName {
		return nil, fmt.Errorf("plugin: symbol %s not found", symName)
	}
	return m.sym, nil
}

// mockOpener returns a PluginOpener that returns the given lookup or error.
func mockOpener(lookup PluginLookup, err error) PluginOpener {
	return func(path string) (PluginLookup, error) {
		if err != nil {
			return nil, err
		}
		return lookup, nil
	}
}

// multiMockOpener routes by path to different lookups.
func multiMockOpener(lookups map[string]PluginLookup) PluginOpener {
	return func(path string) (PluginLookup, error) {
		l, ok := lookups[path]
		if !ok {
			return nil, fmt.Errorf("open %s: no such file", path)
		}
		return l, nil
	}
}

func testLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// --- tests ---

func TestLoadOne_FileNotFound(t *testing.T) {
	opener := mockOpener(nil, fmt.Errorf("open /bad/path.so: no such file"))
	_, err := loadOne("/bad/path.so", opener)
	if err == nil {
		t.Fatal("expected error for missing file")
	}
	if want := "/bad/path.so"; !containsStr(err.Error(), want) {
		t.Errorf("error should name file path %q, got: %s", want, err)
	}
	if !containsStr(err.Error(), "failed to open") {
		t.Errorf("error should mention open failure, got: %s", err)
	}
}

func TestLoadOne_MissingPluginSymbol(t *testing.T) {
	lookup := &mockLookup{
		err: fmt.Errorf("plugin: symbol Plugin not found"),
	}
	opener := mockOpener(lookup, nil)
	_, err := loadOne("/some/plugin.so", opener)
	if err == nil {
		t.Fatal("expected error for missing symbol")
	}
	if !containsStr(err.Error(), "/some/plugin.so") {
		t.Errorf("error should name file, got: %s", err)
	}
	if !containsStr(err.Error(), "Plugin") {
		t.Errorf("error should name missing symbol, got: %s", err)
	}
}

func TestLoadOne_WrongSymbolType(t *testing.T) {
	wrongType := "not a plugin"
	lookup := &mockLookup{sym: &wrongType}
	opener := mockOpener(lookup, nil)
	_, err := loadOne("/some/plugin.so", opener)
	if err == nil {
		t.Fatal("expected error for wrong type")
	}
	if !containsStr(err.Error(), "/some/plugin.so") {
		t.Errorf("error should name file, got: %s", err)
	}
	if !containsStr(err.Error(), "AgentPlugin") {
		t.Errorf("error should name expected type, got: %s", err)
	}
}

func TestLoadOne_ValidPlugin(t *testing.T) {
	var ap pluginapi.AgentPlugin = &mockPlugin{name: "testprovider"}
	lookup := &mockLookup{sym: &ap}
	opener := mockOpener(lookup, nil)
	result, err := loadOne("/some/plugin.so", opener)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.ProviderName() != "testprovider" {
		t.Errorf("expected provider name 'testprovider', got %q", result.ProviderName())
	}
}

func TestLoadPlugins_ReservedNames(t *testing.T) {
	reserved := []string{"anthropic", "google", "openai", "ollama", "copilot"}
	for _, name := range reserved {
		t.Run(name, func(t *testing.T) {
			var ap pluginapi.AgentPlugin = &mockPlugin{name: name}
			lookup := &mockLookup{sym: &ap}
			opener := mockOpener(lookup, nil)

			var buf bytes.Buffer
			logger := testLogger(&buf)

			cfg := configWithFiles([]string{"/test/plugin.so"})
			_, err := loadPluginsWithOpener(cfg, logger, opener)
			if err == nil {
				t.Fatalf("expected error for reserved name %q", name)
			}
			if !containsStr(err.Error(), name) {
				t.Errorf("error should name reserved provider %q, got: %s", name, err)
			}
			if !containsStr(err.Error(), "reserved") {
				t.Errorf("error should mention 'reserved', got: %s", err)
			}
		})
	}
}

func TestLoadPlugins_DuplicateProviderName(t *testing.T) {
	var ap1 pluginapi.AgentPlugin = &mockPlugin{name: "myprovider"}
	var ap2 pluginapi.AgentPlugin = &mockPlugin{name: "myprovider"}
	lookups := map[string]PluginLookup{
		"/plugins/a.so": &mockLookup{sym: &ap1},
		"/plugins/b.so": &mockLookup{sym: &ap2},
	}
	opener := multiMockOpener(lookups)

	var buf bytes.Buffer
	logger := testLogger(&buf)

	cfg := configWithFiles([]string{"/plugins/a.so", "/plugins/b.so"})
	_, err := loadPluginsWithOpener(cfg, logger, opener)
	if err == nil {
		t.Fatal("expected error for duplicate provider name")
	}
	if !containsStr(err.Error(), "myprovider") {
		t.Errorf("error should name conflicting provider, got: %s", err)
	}
	if !containsStr(err.Error(), "conflicts") {
		t.Errorf("error should mention conflict, got: %s", err)
	}
}

func TestLoadPlugins_WarnLogEmitted(t *testing.T) {
	var ap pluginapi.AgentPlugin = &mockPlugin{name: "testplugin"}
	lookup := &mockLookup{sym: &ap}
	opener := mockOpener(lookup, nil)

	var buf bytes.Buffer
	logger := testLogger(&buf)

	cfg := configWithFiles([]string{"/test/plugin.so"})
	plugins, err := loadPluginsWithOpener(cfg, logger, opener)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(plugins) != 1 {
		t.Fatalf("expected 1 plugin, got %d", len(plugins))
	}

	logOutput := buf.String()
	if !containsStr(logOutput, "WARN") {
		t.Errorf("expected WARN level log, got: %s", logOutput)
	}
	if !containsStr(logOutput, "/test/plugin.so") {
		t.Errorf("expected file path in log, got: %s", logOutput)
	}
	if !containsStr(logOutput, "trusted") {
		t.Errorf("expected 'trusted' warning in log, got: %s", logOutput)
	}
}

func TestLoadPlugins_EmptyConfig(t *testing.T) {
	var buf bytes.Buffer
	logger := testLogger(&buf)

	cfg := configWithFiles(nil)
	plugins, err := loadPluginsWithOpener(cfg, logger, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(plugins) != 0 {
		t.Errorf("expected 0 plugins, got %d", len(plugins))
	}
}

func TestLoadPlugins_PanicRecovery(t *testing.T) {
	// Simulate a plugin whose load triggers a panic.
	opener := func(path string) (PluginLookup, error) {
		panic("plugin loading crashed")
	}

	_, err := loadOne("/panic/plugin.so", opener)
	if err == nil {
		t.Fatal("expected error from panic recovery")
	}
	if !containsStr(err.Error(), "panic") {
		t.Errorf("error should mention panic, got: %s", err)
	}
}

// --- Fix #1: Metadata isolation ---

// metadataMutatingAgent is a plugin agent that mutates task.Metadata
// during Execute and adds keys to result.Metadata.
type metadataMutatingAgent struct{}

func (a *metadataMutatingAgent) ID() string                          { return "mutator" }
func (a *metadataMutatingAgent) Provider() string                    { return "test" }
func (a *metadataMutatingAgent) HealthCheck(_ context.Context) error { return nil }
func (a *metadataMutatingAgent) Execute(_ context.Context, task *pluginapi.Task) (*pluginapi.TaskResult, error) {
	// Mutate the inbound metadata — should not affect the broker's copy.
	task.Metadata["injected_by_plugin"] = "evil"
	delete(task.Metadata, "original_key")

	return &pluginapi.TaskResult{
		TaskID:  task.ID,
		Payload: task.Payload,
		Metadata: map[string]any{
			"result_key": "result_value",
		},
	}, nil
}

func TestPluginAgentAdapter_MetadataIsolation(t *testing.T) {
	adapter := NewPluginAgentAdapter(&metadataMutatingAgent{})

	brokerMeta := map[string]any{
		"original_key": "original_value",
	}
	task := &broker.Task{
		ID:       "task-iso",
		Payload:  json.RawMessage(`{}`),
		Metadata: brokerMeta,
	}

	result, err := adapter.Execute(context.Background(), task)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	// The broker's original metadata must be unchanged.
	if _, ok := brokerMeta["injected_by_plugin"]; ok {
		t.Error("plugin mutation leaked into broker task metadata: found 'injected_by_plugin'")
	}
	if v, ok := brokerMeta["original_key"]; !ok || v != "original_value" {
		t.Errorf("broker metadata was mutated: original_key = %v (exists: %v)", v, ok)
	}

	// The result metadata must be a copy — mutating it after the call
	// should not affect what the adapter returned.
	if result.Metadata == nil {
		t.Fatal("expected non-nil result metadata")
	}
	if result.Metadata["result_key"] != "result_value" {
		t.Errorf("expected result_key = result_value, got %v", result.Metadata["result_key"])
	}
}

// nilMetaAgent is a plugin agent that does not touch metadata.
type nilMetaAgent struct{}

func (a *nilMetaAgent) ID() string                          { return "nilmeta" }
func (a *nilMetaAgent) Provider() string                    { return "test" }
func (a *nilMetaAgent) HealthCheck(_ context.Context) error { return nil }
func (a *nilMetaAgent) Execute(_ context.Context, task *pluginapi.Task) (*pluginapi.TaskResult, error) {
	return &pluginapi.TaskResult{
		TaskID:  task.ID,
		Payload: task.Payload,
		// nil Metadata in result
	}, nil
}

func TestPluginAgentAdapter_NilMetadata(t *testing.T) {
	adapter := NewPluginAgentAdapter(&nilMetaAgent{})

	task := &broker.Task{
		ID:       "task-nil-meta",
		Payload:  json.RawMessage(`{}`),
		Metadata: nil, // nil metadata on broker task
	}

	// copyMap(nil) should return nil without panicking.
	result, err := adapter.Execute(context.Background(), task)
	if err != nil {
		t.Fatalf("Execute with nil metadata should not fail: %v", err)
	}
	if result.Metadata != nil {
		t.Errorf("expected nil result metadata, got %v", result.Metadata)
	}
}

// --- Fix #3: Invalid provider names ---

func TestLoadPlugins_EmptyProviderName(t *testing.T) {
	var ap pluginapi.AgentPlugin = &mockPlugin{name: ""}
	lookup := &mockLookup{sym: &ap}
	opener := mockOpener(lookup, nil)

	var buf bytes.Buffer
	logger := testLogger(&buf)

	cfg := configWithFiles([]string{"/test/empty.so"})
	_, err := loadPluginsWithOpener(cfg, logger, opener)
	if err == nil {
		t.Fatal("expected error for empty provider name")
	}
	if !containsStr(err.Error(), "invalid identifier") {
		t.Errorf("error should mention invalid identifier, got: %s", err)
	}
}

func TestLoadPlugins_WhitespaceProviderName(t *testing.T) {
	var ap pluginapi.AgentPlugin = &mockPlugin{name: "has space"}
	lookup := &mockLookup{sym: &ap}
	opener := mockOpener(lookup, nil)

	var buf bytes.Buffer
	logger := testLogger(&buf)

	cfg := configWithFiles([]string{"/test/space.so"})
	_, err := loadPluginsWithOpener(cfg, logger, opener)
	if err == nil {
		t.Fatal("expected error for provider name with spaces")
	}
	if !containsStr(err.Error(), "invalid identifier") {
		t.Errorf("error should mention invalid identifier, got: %s", err)
	}
}

func TestLoadPlugins_SpecialCharProviderName(t *testing.T) {
	badNames := []string{"../escape", "/slash", "semi;colon", "null\x00byte", " leading-space"}
	for _, name := range badNames {
		t.Run(name, func(t *testing.T) {
			var ap pluginapi.AgentPlugin = &mockPlugin{name: name}
			lookup := &mockLookup{sym: &ap}
			opener := mockOpener(lookup, nil)

			var buf bytes.Buffer
			logger := testLogger(&buf)

			cfg := configWithFiles([]string{"/test/bad.so"})
			_, err := loadPluginsWithOpener(cfg, logger, opener)
			if err == nil {
				t.Fatalf("expected error for invalid provider name %q", name)
			}
			if !containsStr(err.Error(), "invalid identifier") {
				t.Errorf("error should mention invalid identifier for %q, got: %s", name, err)
			}
		})
	}
}

// --- helpers ---

// loadPluginsWithOpener is a test helper that bypasses file existence
// checks (resolvePaths) and exercises the core loading + validation logic
// directly with mocked paths and opener.
func loadPluginsWithOpener(cfg configForTest, logger *slog.Logger, opener PluginOpener) (map[string]pluginapi.AgentPlugin, error) {
	return loadFromPaths(cfg.files, logger, opener)
}

type configForTest struct {
	files []string
}

func configWithFiles(files []string) configForTest {
	return configForTest{files: files}
}

func containsStr(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(substr) == 0 || findSubstr(s, substr))
}

func findSubstr(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
