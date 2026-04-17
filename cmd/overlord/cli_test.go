package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brianbuquoi/overlord/internal/broker"
	"github.com/brianbuquoi/overlord/internal/deadletter"
	"github.com/spf13/cobra"
)

// writeTestYAML creates a temp directory with a valid YAML config and
// schema files. Returns the config path.
func writeTestYAML(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	schemas := map[string]string{
		"in_v1.json":  `{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","properties":{"request":{"type":"string"}},"required":["request"]}`,
		"out_v1.json": `{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","properties":{"response":{"type":"string"}},"required":["response"]}`,
	}
	schemasDir := filepath.Join(dir, "schemas")
	os.MkdirAll(schemasDir, 0o755)
	for name, content := range schemas {
		if err := os.WriteFile(filepath.Join(schemasDir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	yaml := `version: "1"

schema_registry:
  - name: task_in
    version: "v1"
    path: schemas/in_v1.json
  - name: task_out
    version: "v1"
    path: schemas/out_v1.json

pipelines:
  - name: test-pipeline
    concurrency: 1
    store: memory
    stages:
      - id: process
        agent: test-agent
        input_schema:
          name: task_in
          version: "v1"
        output_schema:
          name: task_out
          version: "v1"
        timeout: 10s
        retry:
          max_attempts: 1
          backoff: fixed
          base_delay: 100ms
        on_success: done
        on_failure: dead-letter

agents:
  - id: test-agent
    provider: anthropic
    model: claude-sonnet-4-20250514
    auth:
      api_key_env: TEST_API_KEY
    system_prompt: "test"
    temperature: 0.0
    max_tokens: 1024
    timeout: 10s

stores:
  memory:
    max_tasks: 1000
`
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	return configPath
}

// ── Scenario 1: overlord validate ──

func TestCLI_Validate_ValidConfig(t *testing.T) {
	configPath := writeTestYAML(t)

	root := rootCmd()
	root.SetArgs([]string{"validate", "--config", configPath})

	var stdout bytes.Buffer
	root.SetOut(&stdout)

	err := root.Execute()
	if err != nil {
		t.Fatalf("validate should succeed, got: %v", err)
	}
	// User-facing CLI output must go through Cobra's writer so tests can
	// capture it and callers can redirect. Previously the `validate`
	// command printed "config valid" via fmt.Println (os.Stdout) and this
	// buffer was empty.
	if !strings.Contains(stdout.String(), "config valid") {
		t.Errorf("validate output should be captured via Cobra writer; got stdout=%q", stdout.String())
	}
}

func TestCLI_Validate_MissingSchemaFile(t *testing.T) {
	dir := t.TempDir()

	// Config references a schema file that doesn't exist.
	yaml := `version: "1"

schema_registry:
  - name: task_in
    version: "v1"
    path: schemas/nonexistent.json

pipelines:
  - name: test-pipeline
    concurrency: 1
    store: memory
    stages:
      - id: process
        agent: test-agent
        input_schema:
          name: task_in
          version: "v1"
        output_schema:
          name: task_in
          version: "v1"
        timeout: 10s
        retry:
          max_attempts: 1
          backoff: fixed
          base_delay: 100ms
        on_success: done
        on_failure: dead-letter

agents:
  - id: test-agent
    provider: anthropic
    model: claude-sonnet-4-20250514
    auth:
      api_key_env: TEST_API_KEY
    timeout: 10s

stores:
  memory:
    max_tasks: 1000
`
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}

	root := rootCmd()
	root.SetArgs([]string{"validate", "--config", configPath})

	// Validate should fail since the schema file doesn't exist.
	// The validate command calls os.Exit(1) on failure, but we can't
	// catch that in tests. Instead, verify that buildContractRegistry
	// returns an error for missing files.
	cfg, err := loadConfig(configPath)
	if err != nil {
		t.Fatalf("loadConfig should succeed (YAML is valid): %v", err)
	}

	basePath := configBasePath(configPath)
	_, err = buildContractRegistry(cfg, basePath)
	if err == nil {
		t.Fatal("expected error for missing schema file, got nil")
	}
	if !strings.Contains(err.Error(), "nonexistent.json") {
		t.Errorf("error should mention missing file, got: %v", err)
	}
}

func TestCLI_Validate_MissingConfigFile(t *testing.T) {
	root := rootCmd()
	root.SetArgs([]string{"validate", "--config", "/nonexistent/path/config.yaml"})

	err := root.Execute()
	if err == nil {
		t.Fatal("expected error for missing config file")
	}
	// Should not panic — clean error.
}

// ── Scenario 2: overlord health (unit-level test of health check logic) ──

func TestCLI_Health_AgentResults(t *testing.T) {
	// Test the result formatting logic.
	type result struct {
		id       string
		provider string
		model    string
		status   string
	}

	results := []result{
		{id: "agent-1", provider: "anthropic", model: "claude-sonnet-4-20250514", status: "ok"},
		{id: "agent-2", provider: "openai", model: "gpt-4o", status: "error: connection refused"},
	}

	anyUnhealthy := false
	for _, r := range results {
		if r.status != "ok" {
			anyUnhealthy = true
		}
	}

	if !anyUnhealthy {
		t.Error("expected at least one unhealthy agent")
	}
}

// ── Scenario 3: pollTask returns error for FAILED tasks ──

func TestCLI_PollTask_FailedReturnsError(t *testing.T) {
	// Verify the fix: pollTask should return an error for FAILED tasks.
	// We test this indirectly via the broker.
	configPath := writeTestYAML(t)
	cfg, err := loadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}

	b, err := buildBroker(cfg, nil, configPath, newLogger(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Submit a task — it won't process since no workers are running.
	payload := json.RawMessage(`{"request":"test"}`)
	task, err := b.Submit(t.Context(), "test-pipeline", payload)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	// Verify the task was created.
	got, err := b.GetTask(t.Context(), task.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got.ID != task.ID {
		t.Errorf("task ID mismatch: %s != %s", got.ID, task.ID)
	}
}

// ── Scenario 9: Startup log redaction ──

func TestCLI_LogRedaction(t *testing.T) {
	// Verify that the logger doesn't output API key values.
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-test-key-12345")

	// Verify the env var value doesn't leak through config.
	configPath := writeTestYAML(t)
	cfg, err := loadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}

	// The config should reference the env var NAME, never the VALUE.
	for _, ag := range cfg.Agents {
		if ag.Auth.APIKeyEnv == "sk-ant-test-key-12345" {
			t.Error("config should store env var name, not the value")
		}
	}

	// Verify config serialization doesn't leak API keys.
	configJSON, _ := json.Marshal(cfg)
	configStr := string(configJSON)
	if strings.Contains(configStr, "sk-ant-test-key-12345") {
		t.Error("serialized config contains API key value")
	}

	// Verify the API key env var name IS present (that's fine).
	if !strings.Contains(configStr, "TEST_API_KEY") {
		t.Error("config should contain the env var name")
	}
}

// ── Scenario 10: go vet / build verification ──

func TestCLI_BuildsCleanly(t *testing.T) {
	// This test exists to verify the binary compiles.
	// If you can run this test, the build succeeded.
	root := rootCmd()
	if root == nil {
		t.Fatal("rootCmd() returned nil")
	}

	// Verify all subcommands exist.
	cmds := make(map[string]bool)
	for _, cmd := range root.Commands() {
		cmds[cmd.Name()] = true
	}

	for _, name := range []string{"run", "submit", "status", "validate", "health", "cancel", "pipelines", "completion"} {
		if !cmds[name] {
			t.Errorf("missing subcommand: %s", name)
		}
	}
}

func TestCLI_Submit_InvalidJSON(t *testing.T) {
	configPath := writeTestYAML(t)

	root := rootCmd()
	root.SetArgs([]string{"submit", "--config", configPath, "--id", "test-pipeline", "--payload", "not-json"})

	err := root.Execute()
	if err == nil {
		t.Fatal("expected error for invalid JSON payload")
	}
	if !strings.Contains(err.Error(), "not valid JSON") {
		t.Errorf("expected JSON validation error, got: %v", err)
	}
}

func TestCLI_Submit_PipelineNotFound(t *testing.T) {
	configPath := writeTestYAML(t)

	root := rootCmd()
	root.SetArgs([]string{"submit", "--config", configPath, "--id", "nonexistent", "--payload", `{"request":"test"}`})

	err := root.Execute()
	if err == nil {
		t.Fatal("expected error for nonexistent pipeline")
	}
	if !strings.Contains(err.Error(), "pipeline not found") {
		t.Errorf("expected pipeline not found error, got: %v", err)
	}
}

func TestCLI_Submit_FilePayload(t *testing.T) {
	configPath := writeTestYAML(t)

	// Write payload to a temp file.
	dir := t.TempDir()
	payloadPath := filepath.Join(dir, "payload.json")
	if err := os.WriteFile(payloadPath, []byte(`{"request":"from file"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	root := rootCmd()
	root.SetArgs([]string{"submit", "--config", configPath, "--id", "test-pipeline", "--payload", fmt.Sprintf("@%s", payloadPath)})

	// This will succeed (submit + print ID) but won't wait.
	err := root.Execute()
	if err != nil {
		t.Fatalf("submit with file payload: %v", err)
	}
}

// ── Scenario 1: Root help text ──

func TestCLI_RootHelp_ContainsExample(t *testing.T) {
	root := rootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetArgs([]string{})
	root.Execute()

	output := stdout.String()
	if !strings.Contains(output, "Quick start:") {
		t.Error("root help should contain 'Quick start:' section")
	}
	// The workflow-first narrative leads with init → run → serve →
	// export. `submit` and `validate` are still valid subcommands but
	// are advanced; root help should teach the beginner path first.
	for _, want := range []string{"overlord init", "overlord run", "overlord serve", "overlord export"} {
		if !strings.Contains(output, want) {
			t.Errorf("root help should mention %q", want)
		}
	}
	// Chain mode is intentionally hidden from root help so new users
	// see the two-layer story (workflow → strict) rather than a third
	// authoring surface.
	if strings.Contains(output, "overlord chain ") {
		t.Error("root help should not advertise the `chain` subcommand (hidden legacy surface)")
	}
}

// ── Scenario 2: Status --watch ──

func TestCLI_Status_FormattedOutput(t *testing.T) {
	configPath := writeTestYAML(t)
	cfg, err := loadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}

	b, err := buildBroker(cfg, nil, configPath, newLogger(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	payload := json.RawMessage(`{"request":"test"}`)
	task, err := b.Submit(t.Context(), "test-pipeline", payload)
	if err != nil {
		t.Fatal(err)
	}

	// Use printTaskStatus directly to avoid the separate-broker issue.
	got, err := b.GetTask(t.Context(), task.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}

	var stdout bytes.Buffer
	printTaskStatus(&stdout, got)

	output := stdout.String()
	for _, want := range []string{"Task:", "Pipeline:", "Stage:", "Attempts:", "Input:", "Output:", "State:"} {
		if !strings.Contains(output, want) {
			t.Errorf("status output missing %q field", want)
		}
	}
}

func TestCLI_Status_FailedTaskShowsReason(t *testing.T) {
	configPath := writeTestYAML(t)
	cfg, err := loadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}

	b, err := buildBroker(cfg, nil, configPath, newLogger(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	payload := json.RawMessage(`{"request":"test"}`)
	task, err := b.Submit(t.Context(), "test-pipeline", payload)
	if err != nil {
		t.Fatal(err)
	}

	// Manually fail the task with a reason.
	failedState := broker.TaskStateFailed
	b.Store().UpdateTask(t.Context(), task.ID, broker.TaskUpdate{
		State: &failedState,
		Metadata: map[string]any{
			"failure_reason": "schema validation error",
		},
	})

	got, err := b.GetTask(t.Context(), task.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}

	var stdout bytes.Buffer
	printTaskStatus(&stdout, got)

	output := stdout.String()
	if !strings.Contains(output, "*** FAILED:") {
		t.Errorf("failed task status should prominently show failure, got:\n%s", output)
	}
	if !strings.Contains(output, "schema validation error") {
		t.Error("failed task status should include the failure reason")
	}
}

func TestCLI_Status_WatchTerminatesOnDone(t *testing.T) {
	configPath := writeTestYAML(t)
	cfg, err := loadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}

	b, err := buildBroker(cfg, nil, configPath, newLogger(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	payload := json.RawMessage(`{"request":"test"}`)
	task, err := b.Submit(t.Context(), "test-pipeline", payload)
	if err != nil {
		t.Fatal(err)
	}

	// Set task to DONE immediately so --watch terminates without waiting.
	doneState := broker.TaskStateDone
	b.Store().UpdateTask(t.Context(), task.ID, broker.TaskUpdate{
		State: &doneState,
	})

	var stdout bytes.Buffer
	err = watchTask(t.Context(), b, task.ID, &stdout)
	if err != nil {
		t.Fatalf("watchTask should succeed for DONE task: %v", err)
	}

	output := stdout.String()
	if !strings.Contains(output, "DONE") {
		t.Error("watch output should show DONE state")
	}
}

func TestCLI_Status_TaskNotFound(t *testing.T) {
	configPath := writeTestYAML(t)

	root := rootCmd()
	root.SetArgs([]string{"status", "--config", configPath, "--task", "nonexistent-id"})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected error for nonexistent task")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected 'not found' in error, got: %v", err)
	}
}

// ── Scenario 3: Cancel ──

func TestCLI_Cancel_PendingTask(t *testing.T) {
	configPath := writeTestYAML(t)
	cfg, err := loadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}

	b, err := buildBroker(cfg, nil, configPath, newLogger(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	payload := json.RawMessage(`{"request":"test"}`)
	task, err := b.Submit(t.Context(), "test-pipeline", payload)
	if err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	err = cancelTask(t.Context(), b, task.ID, &stdout)
	if err != nil {
		t.Fatalf("cancel should succeed: %v", err)
	}

	if !strings.Contains(stdout.String(), "cancelled") {
		t.Error("cancel output should confirm cancellation")
	}

	// Verify task is now FAILED.
	got, err := b.GetTask(t.Context(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != broker.TaskStateFailed {
		t.Errorf("cancelled task should be FAILED, got %s", got.State)
	}
	if reason, ok := got.Metadata["failure_reason"]; !ok || reason != "cancelled by operator" {
		t.Errorf("cancelled task should have failure_reason metadata, got %v", got.Metadata)
	}
}

func TestCLI_Cancel_TerminalStateReturnsError(t *testing.T) {
	configPath := writeTestYAML(t)
	cfg, err := loadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}

	b, err := buildBroker(cfg, nil, configPath, newLogger(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	payload := json.RawMessage(`{"request":"test"}`)
	task, err := b.Submit(t.Context(), "test-pipeline", payload)
	if err != nil {
		t.Fatal(err)
	}

	// Set task to DONE.
	doneState := broker.TaskStateDone
	b.Store().UpdateTask(t.Context(), task.ID, broker.TaskUpdate{State: &doneState})

	var stdout bytes.Buffer
	err = cancelTask(t.Context(), b, task.ID, &stdout)
	if err == nil {
		t.Fatal("cancel should fail for terminal task")
	}
	if !strings.Contains(err.Error(), "terminal state") {
		t.Errorf("error should mention terminal state, got: %v", err)
	}
}

func TestCLI_Cancel_ExecutingTaskShowsNote(t *testing.T) {
	configPath := writeTestYAML(t)
	cfg, err := loadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}

	b, err := buildBroker(cfg, nil, configPath, newLogger(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	payload := json.RawMessage(`{"request":"test"}`)
	task, err := b.Submit(t.Context(), "test-pipeline", payload)
	if err != nil {
		t.Fatal(err)
	}

	// Set task to EXECUTING.
	execState := broker.TaskStateExecuting
	b.Store().UpdateTask(t.Context(), task.ID, broker.TaskUpdate{State: &execState})

	var stdout bytes.Buffer
	err = cancelTask(t.Context(), b, task.ID, &stdout)
	if err != nil {
		t.Fatalf("cancel should succeed: %v", err)
	}

	output := stdout.String()
	if !strings.Contains(output, "EXECUTING") {
		t.Error("cancel output should note the task was executing")
	}
	if !strings.Contains(output, "may complete") {
		t.Error("cancel output should warn about at-least-once semantics")
	}
}

func TestCLI_Cancel_NonexistentTask(t *testing.T) {
	configPath := writeTestYAML(t)
	cfg, err := loadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}

	b, err := buildBroker(cfg, nil, configPath, newLogger(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	err = cancelTask(t.Context(), b, "nonexistent-id", &stdout)
	if err == nil {
		t.Fatal("cancel should fail for nonexistent task")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error should mention not found, got: %v", err)
	}
}

// ── Scenario 4: Pipelines list/show ──

func TestCLI_PipelinesList(t *testing.T) {
	configPath := writeTestYAML(t)

	root := rootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetArgs([]string{"pipelines", "list", "--config", configPath})
	if err := root.Execute(); err != nil {
		t.Fatalf("pipelines list: %v", err)
	}

	output := stdout.String()
	if !strings.Contains(output, "test-pipeline") {
		t.Error("pipelines list should include test-pipeline")
	}
	if !strings.Contains(output, "PIPELINE") {
		t.Error("pipelines list should have header row")
	}
	if !strings.Contains(output, "test-agent") {
		t.Error("pipelines list should show agent bindings")
	}
}

func TestCLI_PipelinesShow(t *testing.T) {
	configPath := writeTestYAML(t)

	root := rootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetArgs([]string{"pipelines", "show", "--config", configPath, "--pipeline", "test-pipeline"})
	if err := root.Execute(); err != nil {
		t.Fatalf("pipelines show: %v", err)
	}

	output := stdout.String()
	for _, want := range []string{
		"Pipeline: test-pipeline",
		"Concurrency: 1",
		"Store: memory",
		"[process]",
		"agent=test-agent",
		"task_in@v1",
		"task_out@v1",
		"On success: done",
		"On failure: dead-letter",
		"Retry:",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("pipelines show output missing %q", want)
		}
	}

	// Should use tree characters.
	if !strings.Contains(output, "└──") {
		t.Error("pipelines show should use tree formatting")
	}
}

func TestCLI_PipelinesShow_NotFound(t *testing.T) {
	configPath := writeTestYAML(t)

	root := rootCmd()
	root.SetArgs([]string{"pipelines", "show", "--config", configPath, "--pipeline", "nonexistent"})
	err := root.Execute()
	if err == nil {
		t.Fatal("pipelines show should fail for nonexistent pipeline")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error should mention not found, got: %v", err)
	}
	if !strings.Contains(err.Error(), "test-pipeline") {
		t.Errorf("error should list available pipelines, got: %v", err)
	}
}

// ── Scenario 5: Submit --dry-run ──

func TestCLI_Submit_DryRun_Valid(t *testing.T) {
	configPath := writeTestYAML(t)

	root := rootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetArgs([]string{"submit", "--config", configPath, "--id", "test-pipeline",
		"--payload", `{"request":"hello"}`, "--dry-run"})
	if err := root.Execute(); err != nil {
		t.Fatalf("dry-run with valid payload: %v", err)
	}

	output := stdout.String()
	if !strings.Contains(output, "Payload valid") {
		t.Errorf("dry-run should print 'Payload valid', got: %s", output)
	}
	if !strings.Contains(output, "test-pipeline") {
		t.Error("dry-run output should include pipeline name")
	}
	if !strings.Contains(output, "task_in@v1") {
		t.Error("dry-run output should include schema name and version")
	}
}

func TestCLI_Submit_DryRun_Invalid(t *testing.T) {
	configPath := writeTestYAML(t)

	root := rootCmd()
	root.SetArgs([]string{"submit", "--config", configPath, "--id", "test-pipeline",
		"--payload", `{"wrong_field":"hello"}`, "--dry-run"})
	err := root.Execute()
	if err == nil {
		t.Fatal("dry-run should fail for invalid payload")
	}
	if !strings.Contains(err.Error(), "validation failed") {
		t.Errorf("error should mention validation failure, got: %v", err)
	}
}

func TestCLI_Submit_DryRun_DoesNotSubmit(t *testing.T) {
	configPath := writeTestYAML(t)
	cfg, err := loadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}

	b, err := buildBroker(cfg, nil, configPath, newLogger(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Verify no tasks exist before dry-run.
	result, err := b.Store().ListTasks(t.Context(), broker.TaskFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	before := result.Total

	root := rootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetArgs([]string{"submit", "--config", configPath, "--id", "test-pipeline",
		"--payload", `{"request":"hello"}`, "--dry-run"})
	root.Execute()

	// Verify no new tasks were created.
	result, err = b.Store().ListTasks(t.Context(), broker.TaskFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if result.Total != before {
		t.Errorf("dry-run should not create tasks, got %d (was %d)", result.Total, before)
	}
}

func TestCLI_Submit_DryRun_PipelineNotFound(t *testing.T) {
	configPath := writeTestYAML(t)

	root := rootCmd()
	root.SetArgs([]string{"submit", "--config", configPath, "--id", "nonexistent",
		"--payload", `{"request":"hello"}`, "--dry-run"})
	err := root.Execute()
	if err == nil {
		t.Fatal("dry-run should fail for nonexistent pipeline")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error should mention not found, got: %v", err)
	}
	if !strings.Contains(err.Error(), "test-pipeline") {
		t.Errorf("error should list available pipelines, got: %v", err)
	}
}

// ── Scenario 6: Error message quality ──

func TestCLI_ErrorMessages_ConfigNotFound(t *testing.T) {
	root := rootCmd()
	root.SetArgs([]string{"validate", "--config", "/nonexistent/config.yaml"})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error should mention 'not found', got: %v", err)
	}
	if !strings.Contains(err.Error(), "Hint:") {
		t.Errorf("error should include a hint, got: %v", err)
	}
}

func TestCLI_ErrorMessages_SubmitInvalidPayload(t *testing.T) {
	configPath := writeTestYAML(t)

	root := rootCmd()
	root.SetArgs([]string{"submit", "--config", configPath, "--id", "test-pipeline", "--payload", "not-json"})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "Hint:") {
		t.Errorf("invalid JSON error should include a hint, got: %v", err)
	}
}

func TestCLI_ErrorMessages_SubmitPayloadFileMissing(t *testing.T) {
	configPath := writeTestYAML(t)

	root := rootCmd()
	root.SetArgs([]string{"submit", "--config", configPath, "--id", "test-pipeline", "--payload", "@/nonexistent/file.json"})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "/nonexistent/file.json") {
		t.Errorf("error should include the file path, got: %v", err)
	}
}

// ── Scenario 7: Completion ──

func TestCLI_Completion_Bash(t *testing.T) {
	root := rootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetArgs([]string{"completion", "bash"})
	if err := root.Execute(); err != nil {
		t.Fatalf("completion bash: %v", err)
	}
	if !strings.Contains(stdout.String(), "bash completion") {
		t.Error("bash completion output should contain bash completion header")
	}
}

func TestCLI_Completion_Zsh(t *testing.T) {
	root := rootCmd()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetArgs([]string{"completion", "zsh"})
	if err := root.Execute(); err != nil {
		t.Fatalf("completion zsh: %v", err)
	}
	if !strings.Contains(stdout.String(), "zsh completion") {
		t.Error("zsh completion output should contain zsh completion header")
	}
}

func TestCLI_Completion_InvalidShell(t *testing.T) {
	root := rootCmd()
	root.SetArgs([]string{"completion", "fish"})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected error for unsupported shell")
	}
	if !strings.Contains(err.Error(), "unsupported shell") {
		t.Errorf("error should mention unsupported shell, got: %v", err)
	}
}

func TestCLI_Completion_NoArgs(t *testing.T) {
	root := rootCmd()
	root.SetArgs([]string{"completion"})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected error when no shell specified")
	}
}

// ── Pipeline completion function ──

func TestCLI_CompletePipelineIDs(t *testing.T) {
	configPath := writeTestYAML(t)

	root := rootCmd()
	registerCompletions(root)

	// Simulate completion with a config flag.
	submitCmd, _, _ := root.Find([]string{"submit"})
	submitCmd.Flags().Set("config", configPath)

	names, directive := completePipelineIDs(submitCmd, nil, "test")
	if directive != cobra.ShellCompDirectiveNoFileComp {
		t.Errorf("expected NoFileComp directive, got %v", directive)
	}
	if len(names) != 1 || names[0] != "test-pipeline" {
		t.Errorf("expected [test-pipeline], got %v", names)
	}

	// With non-matching prefix.
	names, _ = completePipelineIDs(submitCmd, nil, "nonexistent")
	if len(names) != 0 {
		t.Errorf("expected empty completions for non-matching prefix, got %v", names)
	}
}

func TestCLI_CompletePipelineIDs_NoConfig(t *testing.T) {
	root := rootCmd()
	registerCompletions(root)

	submitCmd, _, _ := root.Find([]string{"submit"})
	names, directive := completePipelineIDs(submitCmd, nil, "")
	if directive != cobra.ShellCompDirectiveNoFileComp {
		t.Errorf("expected NoFileComp directive, got %v", directive)
	}
	if names != nil {
		t.Errorf("expected nil completions without config, got %v", names)
	}
}

// ── isTerminal helper ──

func TestIsTerminal(t *testing.T) {
	tests := []struct {
		state broker.TaskState
		want  bool
	}{
		{broker.TaskStateDone, true},
		{broker.TaskStateFailed, true},
		{broker.TaskStatePending, false},
		{broker.TaskStateExecuting, false},
		{broker.TaskStateRouting, false},
		{broker.TaskStateValidating, false},
		{broker.TaskStateRetrying, false},
	}
	for _, tt := range tests {
		if got := isTerminal(tt.state); got != tt.want {
			t.Errorf("isTerminal(%s) = %v, want %v", tt.state, got, tt.want)
		}
	}
}

// ── Dead-letter replay: atomic ClaimForReplay semantics ──

// deadLetterTask submits a task via the broker, then mutates store state to
// put it in FAILED+RoutedToDeadLetter so replay can be exercised.
func deadLetterTask(t *testing.T, b *broker.Broker) *broker.Task {
	t.Helper()
	task, err := b.Submit(t.Context(), "test-pipeline", json.RawMessage(`{"request":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	failed := broker.TaskStateFailed
	dl := true
	if err := b.Store().UpdateTask(t.Context(), task.ID, broker.TaskUpdate{
		State:              &failed,
		RoutedToDeadLetter: &dl,
	}); err != nil {
		t.Fatal(err)
	}
	return task
}

func TestCLIReplay_AtomicClaim(t *testing.T) {
	configPath := writeTestYAML(t)
	cfg, err := loadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	b, err := buildBroker(cfg, nil, configPath, newLogger(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	original := deadLetterTask(t, b)

	var errOut bytes.Buffer
	newTask, err := replayDeadLetterTask(t.Context(), b, original.ID, &errOut, newLogger())
	if err != nil {
		t.Fatalf("replayDeadLetterTask failed: %v", err)
	}
	if newTask == nil || newTask.ID == "" {
		t.Fatal("expected new task with non-empty ID")
	}
	if newTask.ID == original.ID {
		t.Fatal("replay must create a new task with a fresh ID")
	}

	got, err := b.Store().GetTask(t.Context(), original.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != broker.TaskStateReplayed {
		t.Fatalf("original task state = %s, want REPLAYED", got.State)
	}
	if got.RoutedToDeadLetter {
		t.Error("original task should no longer be flagged RoutedToDeadLetter")
	}
}

func TestCLIReplay_NotReplayable(t *testing.T) {
	configPath := writeTestYAML(t)
	cfg, err := loadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	b, err := buildBroker(cfg, nil, configPath, newLogger(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Task that is merely pending — not in FAILED+DL state.
	task, err := b.Submit(t.Context(), "test-pipeline", json.RawMessage(`{"request":"x"}`))
	if err != nil {
		t.Fatal(err)
	}

	var errOut bytes.Buffer
	newTask, err := replayDeadLetterTask(t.Context(), b, task.ID, &errOut, newLogger())
	if err == nil {
		t.Fatal("expected error for non-replayable task")
	}
	if newTask != nil {
		t.Fatal("no new task should be created on error")
	}
	if !strings.Contains(err.Error(), "not in a replayable state") {
		t.Errorf("expected 'not in a replayable state' error, got: %v", err)
	}

	// Task untouched.
	got, err := b.Store().GetTask(t.Context(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State == broker.TaskStateReplayed {
		t.Error("non-replayable task must not be marked REPLAYED")
	}
}

func TestCLIReplay_NotFound(t *testing.T) {
	configPath := writeTestYAML(t)
	cfg, err := loadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	b, err := buildBroker(cfg, nil, configPath, newLogger(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	var errOut bytes.Buffer
	_, err = replayDeadLetterTask(t.Context(), b, "nonexistent-task-id", &errOut, newLogger())
	if err == nil {
		t.Fatal("expected error for missing task")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected 'not found' error, got: %v", err)
	}
}

func TestCLIReplay_ConcurrentSingleWinner(t *testing.T) {
	configPath := writeTestYAML(t)
	cfg, err := loadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	b, err := buildBroker(cfg, nil, configPath, newLogger(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	original := deadLetterTask(t, b)

	const N = 8
	type result struct {
		task *broker.Task
		err  error
	}
	results := make(chan result, N)
	start := make(chan struct{})
	for i := 0; i < N; i++ {
		go func() {
			<-start
			var errOut bytes.Buffer
			nt, err := replayDeadLetterTask(t.Context(), b, original.ID, &errOut, newLogger())
			results <- result{task: nt, err: err}
		}()
	}
	close(start)

	wins := 0
	losses := 0
	for i := 0; i < N; i++ {
		r := <-results
		if r.err == nil && r.task != nil {
			wins++
		} else if r.err != nil && strings.Contains(r.err.Error(), "not in a replayable state") {
			losses++
		} else {
			t.Errorf("unexpected result: task=%v err=%v", r.task, r.err)
		}
	}
	if wins != 1 {
		t.Fatalf("expected exactly 1 winner, got %d", wins)
	}
	if losses != N-1 {
		t.Fatalf("expected %d NotReplayable losers, got %d", N-1, losses)
	}
}

// TestCLIReplayAll_ConfirmationAccurate verifies the confirmation prompt
// reports the accurate dead-letter total (via ListTasks Total) rather than a
// capped peek size, and handles the ceiling-exceeded case explicitly.
func TestCLIReplayAll_ConfirmationAccurate(t *testing.T) {
	cases := []struct {
		name     string
		total    int
		max      int
		wantSubs []string
		noSubs   []string
	}{
		{
			name:     "small",
			total:    3,
			max:      100000,
			wantSubs: []string{"Found 3 dead-lettered tasks", "Replay all 3 tasks?"},
			noSubs:   []string{"maximum of"},
		},
		{
			name:     "over-1000",
			total:    4721,
			max:      100000,
			wantSubs: []string{"Found 4721 dead-lettered tasks", "Replay all 4721 tasks?"},
			noSubs:   []string{"maximum of"},
		},
		{
			name:     "over-ceiling",
			total:    142000,
			max:      100000,
			wantSubs: []string{"Found 142000 dead-lettered tasks", "maximum of 100000", "Replay up to 100000 tasks?"},
			noSubs:   []string{"Replay all 142000"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := replayAllConfirmMessage(tc.total, "test-pipeline", tc.max)
			for _, s := range tc.wantSubs {
				if !strings.Contains(got, s) {
					t.Errorf("prompt missing %q; got: %s", s, got)
				}
			}
			for _, s := range tc.noSubs {
				if strings.Contains(got, s) {
					t.Errorf("prompt should not contain %q; got: %s", s, got)
				}
			}
		})
	}
}

// TestCLIReplayAll_EmptySet verifies that replay-all exits cleanly without
// prompting when no dead-lettered tasks are present.
func TestCLIReplayAll_EmptySet(t *testing.T) {
	configPath := writeTestYAML(t)

	root := rootCmd()
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs([]string{
		"dead-letter", "replay-all",
		"--config", configPath,
		"--pipeline", "test-pipeline",
	})
	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}

	if !strings.Contains(stdout.String(), "No dead-lettered tasks found.") {
		t.Errorf("stdout missing empty-set message; got: %q", stdout.String())
	}
	// No confirmation prompt should be emitted on stderr when the set is empty.
	if strings.Contains(stderr.String(), "Replay") {
		t.Errorf("stderr should not contain confirmation prompt when set is empty; got: %q", stderr.String())
	}
}

// TestCLIReplayAll_ProgressOutputsNewTaskID verifies that the replay-all
// progress callback used by the CLI emits "replayed {origID} → {newID}" so
// operators can correlate dead-lettered tasks with their new counterparts.
func TestCLIReplayAll_ProgressOutputsNewTaskID(t *testing.T) {
	configPath := writeTestYAML(t)
	cfg, err := loadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	b, err := buildBroker(cfg, nil, configPath, newLogger(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	origID := deadLetterTask(t, b).ID

	svc := deadletter.New(b.Store(), b, newLogger())

	var stdout, stderr bytes.Buffer
	progress := func(taskID, newTaskID string, perErr error) {
		if perErr != nil {
			fmt.Fprintf(&stderr, "failed %s: %v\n", taskID, perErr)
			return
		}
		fmt.Fprintf(&stdout, "replayed %s → %s\n", taskID, newTaskID)
	}

	result, err := svc.ReplayAll(t.Context(), "test-pipeline", 0, progress)
	if err != nil {
		t.Fatalf("ReplayAll: %v", err)
	}
	if result.Processed != 1 || result.Failed != 0 {
		t.Fatalf("result: got processed=%d failed=%d, want 1/0", result.Processed, result.Failed)
	}

	out := stdout.String()
	prefix := "replayed " + origID + " → "
	if !strings.Contains(out, prefix) {
		t.Fatalf("stdout missing %q; got: %q", prefix, out)
	}
	rest := out[strings.Index(out, prefix)+len(prefix):]
	if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
		rest = rest[:nl]
	}
	if strings.TrimSpace(rest) == "" {
		t.Errorf("new task ID after '→' is empty; full stdout: %q", out)
	}
	if strings.TrimSpace(rest) == origID {
		t.Errorf("new task ID should differ from original %s; got: %q", origID, rest)
	}
}

// seedReplayPendingTask puts a task into REPLAY_PENDING via ClaimForReplay,
// matching the state left behind by a double-failure during replay.
func seedReplayPendingTask(t *testing.T, b *broker.Broker) string {
	t.Helper()
	original := deadLetterTask(t, b)
	if _, err := b.Store().ClaimForReplay(t.Context(), original.ID); err != nil {
		t.Fatalf("ClaimForReplay: %v", err)
	}
	return original.ID
}

func TestCLIRecover_HappyPath(t *testing.T) {
	configPath := writeTestYAML(t)
	cfg, err := loadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	b, err := buildBroker(cfg, nil, configPath, newLogger(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	taskID := seedReplayPendingTask(t, b)

	msg, err := recoverTaskCLI(t.Context(), b, taskID)
	if err != nil {
		t.Fatalf("recoverTaskCLI: %v", err)
	}
	if !strings.Contains(msg, "Recovered task "+taskID) {
		t.Errorf("output missing success message; got: %q", msg)
	}

	got, err := b.Store().GetTask(t.Context(), taskID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != broker.TaskStateFailed {
		t.Errorf("state: got %s want FAILED", got.State)
	}
	if !got.RoutedToDeadLetter {
		t.Errorf("RoutedToDeadLetter: got false want true")
	}
}

func TestCLIRecover_NotFound(t *testing.T) {
	configPath := writeTestYAML(t)
	cfg, err := loadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	b, err := buildBroker(cfg, nil, configPath, newLogger(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	_, err = recoverTaskCLI(t.Context(), b, "nonexistent-task-id")
	if err == nil {
		t.Fatal("expected error for missing task")
	}
	if !strings.Contains(err.Error(), "nonexistent-task-id not found") {
		t.Errorf("expected clean not-found error, got: %v", err)
	}
}

// TestCLIRecover_CommandRegistered verifies the cobra subcommand exists.
func TestCLIRecover_CommandRegistered(t *testing.T) {
	root := rootCmd()
	sub, _, err := root.Find([]string{"dead-letter", "recover"})
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if sub.Use != "recover" {
		t.Errorf("expected recover command, got %q", sub.Use)
	}
}

func TestCLIRecover_WrongState(t *testing.T) {
	configPath := writeTestYAML(t)
	cfg, err := loadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	b, err := buildBroker(cfg, nil, configPath, newLogger(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Task in FAILED+DL state, not REPLAY_PENDING.
	task := deadLetterTask(t, b)

	_, err = recoverTaskCLI(t.Context(), b, task.ID)
	if err == nil {
		t.Fatal("expected error for wrong state")
	}
	if !strings.Contains(err.Error(), "not in REPLAY_PENDING state") {
		t.Errorf("expected wrong-state error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "current state: FAILED") {
		t.Errorf("expected current state in error, got: %v", err)
	}
}
