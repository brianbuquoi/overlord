package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/brianbuquoi/overlord/internal/agent"
	"github.com/brianbuquoi/overlord/internal/broker"
	"github.com/brianbuquoi/overlord/internal/config"
)

// echoBinaryPath is the compiled path of the echo plugin, set by TestMain.
var echoBinaryPath string

func TestMain(m *testing.M) {
	tmp, err := os.MkdirTemp("", "overlord-plugin-test-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "create tmp: %v\n", err)
		os.Exit(1)
	}
	defer os.RemoveAll(tmp)

	echoBinaryPath = filepath.Join(tmp, "echo_plugin")
	build := exec.Command("go", "build", "-o", echoBinaryPath, "./testdata/echo_plugin")
	build.Stdout = os.Stdout
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "build echo_plugin: %v\n", err)
		os.Exit(1)
	}

	code := m.Run()
	_ = os.RemoveAll(tmp)
	os.Exit(code)
}

// buildAgent writes a manifest pointing at the compiled echo plugin with the
// given env and manifest overrides, then constructs the agent.
func buildAgent(t *testing.T, extra map[string]string, overrides func(m *Manifest)) *Agent {
	t.Helper()
	m := &Manifest{
		Name:            "echo",
		Binary:          echoBinaryPath,
		RPCTimeout:      Duration{Duration: 5 * time.Second},
		ShutdownTimeout: Duration{Duration: 1 * time.Second},
		MaxRestarts:     3,
	}
	// Allow-list env vars the plugin reads for test configuration.
	for k := range extra {
		m.Env = append(m.Env, k)
	}
	if overrides != nil {
		overrides(m)
	}
	for k, v := range extra {
		t.Setenv(k, v)
	}
	// bypass LoadManifest (absolute Binary is fine here).
	if err := m.Validate(); err != nil {
		t.Fatalf("manifest validate: %v", err)
	}
	a, err := New("test-plugin", "sys prompt", m, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	t.Cleanup(func() { _ = a.Stop() })
	return a
}

func newTask(payload string) *broker.Task {
	return &broker.Task{
		ID:         "task-1",
		PipelineID: "pipe-1",
		StageID:    "stage-1",
		Payload:    json.RawMessage(payload),
	}
}

func TestAgent_ExecuteEchoesPayload(t *testing.T) {
	a := buildAgent(t, nil, nil)
	ctx := context.Background()
	res, err := a.Execute(ctx, newTask(`{"hello":"world"}`))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if string(res.Payload) != `{"hello":"world"}` {
		t.Errorf("payload: got %s", string(res.Payload))
	}
	if got := res.Metadata["echo_task_id"]; got != "task-1" {
		t.Errorf("metadata echo_task_id: got %v", got)
	}
}

func TestAgent_HealthCheckOK(t *testing.T) {
	a := buildAgent(t, nil, nil)
	if err := a.HealthCheck(context.Background()); err != nil {
		t.Fatalf("health: %v", err)
	}
}

func TestAgent_HealthCheckUnhealthy(t *testing.T) {
	a := buildAgent(t, map[string]string{"ECHO_PLUGIN_UNHEALTHY": "1"}, nil)
	err := a.HealthCheck(context.Background())
	if err == nil {
		t.Fatal("expected unhealthy error")
	}
	if !strings.Contains(err.Error(), "unhealthy") {
		t.Errorf("err: %v", err)
	}
}

func TestAgent_InvalidParamsIsNonRetryable(t *testing.T) {
	a := buildAgent(t, map[string]string{"ECHO_PLUGIN_RETURN_INVALID": "1"}, nil)
	_, err := a.Execute(context.Background(), newTask(`{}`))
	if err == nil {
		t.Fatal("expected error")
	}
	var ae *agent.AgentError
	if !errors.As(err, &ae) {
		t.Fatalf("expected AgentError, got %T", err)
	}
	if ae.Retryable {
		t.Errorf("invalid_params must be non-retryable")
	}
}

func TestAgent_InternalRetryableByDefault(t *testing.T) {
	a := buildAgent(t, map[string]string{"ECHO_PLUGIN_RETURN_INTERNAL": "1"}, nil)
	_, err := a.Execute(context.Background(), newTask(`{}`))
	if err == nil {
		t.Fatal("expected error")
	}
	var ae *agent.AgentError
	if !errors.As(err, &ae) {
		t.Fatalf("expected AgentError, got %T", err)
	}
	if !ae.Retryable {
		t.Errorf("internal error should be retryable by default")
	}
}

func TestAgent_InternalNonRetryableWhenManifestSaysSo(t *testing.T) {
	a := buildAgent(t, map[string]string{"ECHO_PLUGIN_RETURN_INTERNAL": "1"}, func(m *Manifest) {
		m.OnFailure = OnFailureNonRetryable
	})
	_, err := a.Execute(context.Background(), newTask(`{}`))
	var ae *agent.AgentError
	if !errors.As(err, &ae) {
		t.Fatalf("expected AgentError, got %T", err)
	}
	if ae.Retryable {
		t.Errorf("internal error should be non-retryable per manifest")
	}
}

func TestAgent_RestartOnCrash(t *testing.T) {
	// Plugin exits after the FIRST execute. A second execute should
	// trigger restart and succeed on the fresh subprocess.
	a := buildAgent(t, map[string]string{"ECHO_PLUGIN_ECHO_EXIT_AFTER": "1"}, nil)
	if _, err := a.Execute(context.Background(), newTask(`{"n":1}`)); err != nil {
		t.Fatalf("first execute: %v", err)
	}
	// Give the subprocess a moment to actually exit.
	time.Sleep(100 * time.Millisecond)
	// Second call — restart path.
	res, err := a.Execute(context.Background(), newTask(`{"n":2}`))
	if err != nil {
		t.Fatalf("second execute (restart): %v", err)
	}
	if string(res.Payload) != `{"n":2}` {
		t.Errorf("payload after restart: %s", string(res.Payload))
	}
}

func TestAgent_MaxRestartsExhausted(t *testing.T) {
	a := buildAgent(t, map[string]string{"ECHO_PLUGIN_CRASH_ON_EXECUTE": "1"}, func(m *Manifest) {
		m.MaxRestarts = 1
	})
	// First execute crashes the plugin mid-call.
	_, err := a.Execute(context.Background(), newTask(`{}`))
	if err == nil {
		t.Fatal("expected error from crash")
	}
	// Next few calls also crash → eventually exhaust restart budget.
	var lastErr error
	for i := 0; i < 5; i++ {
		_, lastErr = a.Execute(context.Background(), newTask(`{}`))
		if lastErr == nil {
			continue
		}
		var ae *agent.AgentError
		if errors.As(lastErr, &ae) && !ae.Retryable && strings.Contains(ae.Error(), "restart budget") {
			return
		}
	}
	t.Fatalf("never saw non-retryable restart-budget error; last: %v", lastErr)
}

func TestAgent_RPCTimeout(t *testing.T) {
	a := buildAgent(t, map[string]string{"ECHO_PLUGIN_SLOW_MS": "500"}, func(m *Manifest) {
		m.RPCTimeout = Duration{Duration: 50 * time.Millisecond}
	})
	_, err := a.Execute(context.Background(), newTask(`{}`))
	if err == nil {
		t.Fatal("expected timeout error")
	}
	var ae *agent.AgentError
	if !errors.As(err, &ae) {
		t.Fatalf("expected AgentError: %v", err)
	}
	if !ae.Retryable {
		t.Errorf("timeout should be retryable")
	}
}

func TestAgent_EnvIsolation(t *testing.T) {
	// SECRET_NOT_ALLOWED is NOT in the manifest env list so the plugin
	// won't see it. Plugin echoes its payload so we cannot directly
	// observe the env, but we can at least verify the plugin starts and
	// responds normally without leaking — this test documents intent.
	t.Setenv("SECRET_NOT_ALLOWED", "leaked")
	a := buildAgent(t, nil, nil)
	if _, err := a.Execute(context.Background(), newTask(`{}`)); err != nil {
		t.Fatalf("execute: %v", err)
	}
}

func TestAgent_Stop(t *testing.T) {
	a := buildAgent(t, nil, nil)
	if _, err := a.Execute(context.Background(), newTask(`{}`)); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if err := a.Stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	// Stop is idempotent.
	if err := a.Stop(); err != nil {
		t.Fatalf("stop 2nd: %v", err)
	}
}

// TestPluginAgent_Stop_SendsSIGTERM: a plugin that exits cleanly on stdin EOF
// / SIGTERM (the default echo plugin behavior) is stopped by Stop() within the
// configured shutdown_timeout. We record the PID before Stop and verify the
// process is gone afterwards.
func TestPluginAgent_Stop_SendsSIGTERM(t *testing.T) {
	a := buildAgent(t, nil, func(m *Manifest) {
		m.ShutdownTimeout = Duration{Duration: 2 * time.Second}
	})
	if _, err := a.Execute(context.Background(), newTask(`{}`)); err != nil {
		t.Fatalf("execute: %v", err)
	}
	pid := a.cmd.Process.Pid

	start := time.Now()
	if err := a.Stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("Stop took too long (%v); expected clean exit on signal", d)
	}
	// PID should no longer belong to a live echo plugin process.
	if err := syscall.Kill(pid, 0); err == nil {
		t.Errorf("process %d still alive after Stop", pid)
	}
}

// TestPluginAgent_Stop_KillsAfterTimeout: a plugin that ignores SIGINT/SIGTERM
// and blocks forever is killed via SIGKILL after shutdown_timeout elapses.
func TestPluginAgent_Stop_KillsAfterTimeout(t *testing.T) {
	a := buildAgent(t,
		map[string]string{"ECHO_PLUGIN_IGNORE_SHUTDOWN": "1"},
		func(m *Manifest) {
			m.ShutdownTimeout = Duration{Duration: 200 * time.Millisecond}
		},
	)
	if _, err := a.Execute(context.Background(), newTask(`{}`)); err != nil {
		t.Fatalf("execute: %v", err)
	}
	pid := a.cmd.Process.Pid

	start := time.Now()
	if err := a.Stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	// Stop returned only after the SIGKILL path executed, so the process
	// must be dead. Elapsed time should be >= shutdown_timeout.
	if d := time.Since(start); d < 200*time.Millisecond {
		t.Errorf("Stop returned too quickly (%v); expected >= shutdown_timeout", d)
	}
	if err := syscall.Kill(pid, 0); err == nil {
		t.Errorf("process %d still alive after SIGKILL path", pid)
	}
}

func TestAgent_IDAndProvider(t *testing.T) {
	a := buildAgent(t, nil, nil)
	if a.ID() != "test-plugin" {
		t.Errorf("id: %q", a.ID())
	}
	if a.Provider() != "plugin" {
		t.Errorf("provider: %q", a.Provider())
	}
}

func TestLoadAndCreate_EagerMissingManifest(t *testing.T) {
	// Manifest validation is now eager — missing manifest fails at
	// construction time rather than on first HealthCheck/Execute.
	_, err := LoadAndCreate(config.Agent{
		ID:           "missing",
		Provider:     "plugin",
		ManifestPath: "/nonexistent/manifest.yaml",
	}, slog.Default())
	if err == nil {
		t.Fatal("expected eager validation error")
	}
}
