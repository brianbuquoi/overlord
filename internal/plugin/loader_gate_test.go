package plugin

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brianbuquoi/overlord/internal/config"
	pluginapi "github.com/brianbuquoi/overlord/pkg/plugin"
)

// writeFakeSO writes an empty file with a .so name that resolvePaths can
// stat. The content is never executed because the tests use a mock
// PluginOpener, so plugin.Open is never called on it.
func writeFakeSO(t *testing.T, name string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte{}, 0o644); err != nil {
		t.Fatalf("write fake .so: %v", err)
	}
	return p
}

// TestLoadPlugins_GateDisabledRefusesSOInputs verifies the .so loader
// refuses to dlopen anything when plugins.enabled is false, even if the
// file exists on disk. This mirrors the subprocess-loader gate; the two
// together implement the fail-closed opt-in contract.
func TestLoadPlugins_GateDisabledRefusesSOInputs(t *testing.T) {
	sopath := writeFakeSO(t, "fake.so")

	var apmock pluginapi.AgentPlugin = &mockPlugin{name: "x"}
	opener := mockOpener(&mockLookup{sym: &apmock}, nil)

	cfg := config.PluginConfig{
		Enabled: false,
		Files:   []string{sopath},
	}
	_, err := loadPlugins(cfg, slog.Default(), opener)
	if err == nil {
		t.Fatal("expected gate refusal, got nil")
	}
	if !strings.Contains(err.Error(), "plugins are disabled") {
		t.Errorf("error must say plugins are disabled; got: %v", err)
	}
}

// TestLoadPlugins_AllowlistRejectsSOPathNotInList verifies that even with
// plugins.enabled: true, a .so path that is not in plugins.allow is
// rejected before the opener is invoked.
func TestLoadPlugins_AllowlistRejectsSOPathNotInList(t *testing.T) {
	sopath := writeFakeSO(t, "untrusted.so")

	opener := func(path string) (PluginLookup, error) {
		t.Fatalf("opener must not be called when allowlist rejects the path (got path=%q)", path)
		return nil, nil
	}

	cfg := config.PluginConfig{
		Enabled: true,
		Allow:   []string{"/some/other/allowed.so"},
		Files:   []string{sopath},
	}
	_, err := loadPlugins(cfg, slog.Default(), opener)
	if err == nil {
		t.Fatal("expected allowlist refusal, got nil")
	}
	if !strings.Contains(err.Error(), "plugins.allow") {
		t.Errorf("error must mention plugins.allow; got: %v", err)
	}
}

// TestLoadPlugins_AllowlistPermitsListedSOPath verifies that a .so path
// matching an entry in plugins.allow is permitted past the allowlist
// check (the test stops short of a real plugin.Open and instead uses a
// mock lookup that returns a valid AgentPlugin).
func TestLoadPlugins_AllowlistPermitsListedSOPath(t *testing.T) {
	sopath := writeFakeSO(t, "trusted.so")

	var ap pluginapi.AgentPlugin = &mockPlugin{name: "trusted"}
	opener := mockOpener(&mockLookup{sym: &ap}, nil)

	cfg := config.PluginConfig{
		Enabled: true,
		Allow:   []string{sopath},
		Files:   []string{sopath},
	}
	plugins, err := loadPlugins(cfg, slog.Default(), opener)
	if err != nil {
		t.Fatalf("expected allowlisted .so to load; got %v", err)
	}
	if _, ok := plugins["trusted"]; !ok {
		t.Errorf("expected provider 'trusted' in plugins map; got keys: %v", keysOf(plugins))
	}
}

// TestLoadPlugins_GateDisabledButNoInputsIsNoop verifies that a
// fully-disabled plugin config with no inputs is not an error — it just
// produces an empty plugins map. This matters because the default
// deployment shape is "no plugins at all", and it must not trip the
// gate.
func TestLoadPlugins_GateDisabledButNoInputsIsNoop(t *testing.T) {
	plugins, err := loadPlugins(config.PluginConfig{}, slog.Default(), nil)
	if err != nil {
		t.Fatalf("expected no error for empty disabled config; got %v", err)
	}
	if len(plugins) != 0 {
		t.Errorf("expected empty plugins map; got %d entries", len(plugins))
	}
}

func keysOf(m map[string]pluginapi.AgentPlugin) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
