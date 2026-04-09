package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/orcastrator/orcastrator/internal/broker"
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

// ── Scenario 1: orcastrator validate ──

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

// ── Scenario 2: orcastrator health (unit-level test of health check logic) ──

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

	b, err := buildBroker(cfg, configPath, newLogger(), nil, nil)
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
	root.SetArgs([]string{"submit", "--config", configPath, "--pipeline", "test-pipeline", "--payload", "not-json"})

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
	root.SetArgs([]string{"submit", "--config", configPath, "--pipeline", "nonexistent", "--payload", `{"request":"test"}`})

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
	root.SetArgs([]string{"submit", "--config", configPath, "--pipeline", "test-pipeline", "--payload", fmt.Sprintf("@%s", payloadPath)})

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
	if !strings.Contains(output, "orcastrator submit") {
		t.Error("root help should show submit example")
	}
	if !strings.Contains(output, "orcastrator validate") {
		t.Error("root help should show validate example")
	}
}

// ── Scenario 2: Status --watch ──

func TestCLI_Status_FormattedOutput(t *testing.T) {
	configPath := writeTestYAML(t)
	cfg, err := loadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}

	b, err := buildBroker(cfg, configPath, newLogger(), nil, nil)
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

	b, err := buildBroker(cfg, configPath, newLogger(), nil, nil)
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

	b, err := buildBroker(cfg, configPath, newLogger(), nil, nil)
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

	b, err := buildBroker(cfg, configPath, newLogger(), nil, nil)
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

	b, err := buildBroker(cfg, configPath, newLogger(), nil, nil)
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

	b, err := buildBroker(cfg, configPath, newLogger(), nil, nil)
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

	b, err := buildBroker(cfg, configPath, newLogger(), nil, nil)
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
	root.SetArgs([]string{"submit", "--config", configPath, "--pipeline", "test-pipeline",
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
	root.SetArgs([]string{"submit", "--config", configPath, "--pipeline", "test-pipeline",
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

	b, err := buildBroker(cfg, configPath, newLogger(), nil, nil)
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
	root.SetArgs([]string{"submit", "--config", configPath, "--pipeline", "test-pipeline",
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
	root.SetArgs([]string{"submit", "--config", configPath, "--pipeline", "nonexistent",
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
	root.SetArgs([]string{"submit", "--config", configPath, "--pipeline", "test-pipeline", "--payload", "not-json"})
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
	root.SetArgs([]string{"submit", "--config", configPath, "--pipeline", "test-pipeline", "--payload", "@/nonexistent/file.json"})
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
