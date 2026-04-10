//go:build plugin_test

package plugin

import (
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"github.com/orcastrator/orcastrator/internal/broker"
	pluginapi "github.com/orcastrator/orcastrator/pkg/plugin"
)

var (
	echoPluginOnce sync.Once
	echoPluginPath string
	echoPluginErr  error
)

func getEchoPlugin(t *testing.T) string {
	t.Helper()
	echoPluginOnce.Do(func() {
		_, thisFile, _, _ := runtime.Caller(0)
		projectRoot := filepath.Join(filepath.Dir(thisFile), "..", "..")
		srcDir := filepath.Join(projectRoot, "examples", "plugins", "echo")
		outDir := t.TempDir()
		echoPluginPath = filepath.Join(outDir, "echo.so")

		cmd := exec.Command("go", "build", "-race", "-buildmode=plugin", "-o", echoPluginPath, srcDir)
		cmd.Dir = projectRoot
		out, err := cmd.CombinedOutput()
		if err != nil {
			echoPluginErr = err
			t.Logf("build output: %s", out)
		}
	})
	if echoPluginErr != nil {
		t.Fatalf("failed to build echo plugin: %v", echoPluginErr)
	}
	return echoPluginPath
}

func TestEchoPlugin_LoadAndExecute(t *testing.T) {
	soPath := getEchoPlugin(t)

	p, err := DefaultOpener(soPath)
	if err != nil {
		t.Fatalf("failed to open plugin: %v", err)
	}

	sym, err := p.Lookup("Plugin")
	if err != nil {
		t.Fatalf("failed to find Plugin symbol: %v", err)
	}

	ptr, ok := sym.(*pluginapi.AgentPlugin)
	if !ok {
		t.Fatalf("Plugin symbol has type %T, expected *pluginapi.AgentPlugin", sym)
	}

	ap := *ptr

	// Verify ProviderName.
	if name := ap.ProviderName(); name != "echo" {
		t.Errorf("expected ProviderName 'echo', got %q", name)
	}

	// Create agent.
	agent, err := ap.NewAgent(pluginapi.PluginAgentConfig{
		ID:    "test-echo",
		Model: "echo-v1",
	})
	if err != nil {
		t.Fatalf("NewAgent failed: %v", err)
	}

	if agent.ID() != "test-echo" {
		t.Errorf("expected ID 'test-echo', got %q", agent.ID())
	}
	if agent.Provider() != "echo" {
		t.Errorf("expected Provider 'echo', got %q", agent.Provider())
	}

	// Execute — should echo input payload.
	input := json.RawMessage(`{"input":"hello"}`)
	result, err := agent.Execute(context.Background(), &pluginapi.Task{
		ID:      "task-1",
		Payload: input,
	})
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if string(result.Payload) != string(input) {
		t.Errorf("expected payload %s, got %s", input, result.Payload)
	}

	// HealthCheck — should return nil.
	if err := agent.HealthCheck(context.Background()); err != nil {
		t.Errorf("HealthCheck failed: %v", err)
	}

	// Test the adapter wrapping (broker types).
	t.Run("Adapter", func(t *testing.T) {
		adapter := NewPluginAgentAdapter(agent)
		if adapter.ID() != "test-echo" {
			t.Errorf("expected ID 'test-echo', got %q", adapter.ID())
		}

		adapterInput := json.RawMessage(`{"key":"value"}`)
		brokerResult, err := adapter.Execute(context.Background(), &broker.Task{
			ID:      "task-2",
			Payload: adapterInput,
		})
		if err != nil {
			t.Fatalf("Execute via adapter failed: %v", err)
		}
		if string(brokerResult.Payload) != string(adapterInput) {
			t.Errorf("expected payload %s, got %s", adapterInput, brokerResult.Payload)
		}

		if err := adapter.HealthCheck(context.Background()); err != nil {
			t.Errorf("HealthCheck via adapter failed: %v", err)
		}
	})
}
