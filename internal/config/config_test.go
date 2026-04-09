package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTestConfig(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

const validConfig = `
version: "1"

schema_registry:
  - name: input_a
    version: "v1"
    path: schemas/input_a_v1.json
  - name: output_a
    version: "v1"
    path: schemas/output_a_v1.json

pipelines:
  - name: test-pipeline
    concurrency: 3
    store: memory
    stages:
      - id: stage-1
        agent: agent-1
        input_schema:
          name: input_a
          version: "v1"
        output_schema:
          name: output_a
          version: "v1"
        timeout: 30s
        retry:
          max_attempts: 2
          backoff: exponential
          base_delay: 1s
        on_success: done
        on_failure: dead-letter

agents:
  - id: agent-1
    provider: anthropic
    model: claude-sonnet-4-20250514
    auth:
      api_key_env: ANTHROPIC_API_KEY
    system_prompt: "Test agent"
    temperature: 0.1
    max_tokens: 1024
    timeout: 30s

stores:
  memory:
    max_tasks: 1000
`

func TestLoad_ValidConfig(t *testing.T) {
	path := writeTestConfig(t, validConfig)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if cfg.Version != "1" {
		t.Errorf("expected version 1, got %q", cfg.Version)
	}
	if len(cfg.Pipelines) != 1 {
		t.Errorf("expected 1 pipeline, got %d", len(cfg.Pipelines))
	}
	if len(cfg.Agents) != 1 {
		t.Errorf("expected 1 agent, got %d", len(cfg.Agents))
	}
	if len(cfg.SchemaRegistry) != 2 {
		t.Errorf("expected 2 schema entries, got %d", len(cfg.SchemaRegistry))
	}
	if cfg.Pipelines[0].Stages[0].Timeout.Seconds() != 30 {
		t.Errorf("expected 30s timeout, got %v", cfg.Pipelines[0].Stages[0].Timeout)
	}
}

func TestLoad_UnknownAgent(t *testing.T) {
	cfg := strings.Replace(validConfig, "agent: agent-1", "agent: nonexistent", 1)
	path := writeTestConfig(t, cfg)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for unknown agent")
	}
	if !strings.Contains(err.Error(), "nonexistent") {
		t.Errorf("error should mention the unknown agent, got: %v", err)
	}
}

func TestLoad_SchemaNotInRegistry(t *testing.T) {
	cfg := strings.Replace(validConfig, "name: input_a\n          version: \"v1\"", "name: nonexistent_schema\n          version: \"v1\"", 1)
	path := writeTestConfig(t, cfg)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for schema not in registry")
	}
	if !strings.Contains(err.Error(), "nonexistent_schema") {
		t.Errorf("error should mention the unknown schema, got: %v", err)
	}
	if !strings.Contains(err.Error(), "not declared in schema_registry") {
		t.Errorf("error should say 'not declared in schema_registry', got: %v", err)
	}
}

func TestLoad_SchemaVersionMismatch(t *testing.T) {
	// input_a exists at v1, but we reference v2
	cfg := strings.Replace(validConfig,
		"input_schema:\n          name: input_a\n          version: \"v1\"",
		"input_schema:\n          name: input_a\n          version: \"v2\"", 1)
	path := writeTestConfig(t, cfg)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for schema version mismatch")
	}
	if !strings.Contains(err.Error(), "v2") {
		t.Errorf("error should mention requested version v2, got: %v", err)
	}
	if !strings.Contains(err.Error(), "not registered") {
		t.Errorf("error should say 'not registered', got: %v", err)
	}
}

func TestLoad_DuplicateStageIDs(t *testing.T) {
	cfg := `
version: "1"

schema_registry:
  - name: in
    version: "v1"
    path: schemas/in.json
  - name: out
    version: "v1"
    path: schemas/out.json

pipelines:
  - name: dup-test
    concurrency: 1
    store: memory
    stages:
      - id: same-id
        agent: a1
        input_schema:
          name: in
          version: "v1"
        output_schema:
          name: out
          version: "v1"
        timeout: 10s
        on_success: done
        on_failure: dead-letter
      - id: same-id
        agent: a1
        input_schema:
          name: in
          version: "v1"
        output_schema:
          name: out
          version: "v1"
        timeout: 10s
        on_success: done
        on_failure: dead-letter

agents:
  - id: a1
    provider: anthropic
    model: claude-sonnet-4-20250514
    auth:
      api_key_env: ANTHROPIC_API_KEY
    timeout: 10s
`
	path := writeTestConfig(t, cfg)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for duplicate stage IDs")
	}
	if !strings.Contains(err.Error(), "duplicate stage ID") {
		t.Errorf("error should mention 'duplicate stage ID', got: %v", err)
	}
}

func TestLoad_InvalidTimeout(t *testing.T) {
	cfg := strings.Replace(validConfig, "timeout: 30s", "timeout: not-a-duration", 1)
	path := writeTestConfig(t, cfg)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for invalid timeout format")
	}
}

func TestLoad_DuplicateAgentIDs(t *testing.T) {
	cfg := `
version: "1"

schema_registry:
  - name: in
    version: "v1"
    path: schemas/in.json
  - name: out
    version: "v1"
    path: schemas/out.json

pipelines:
  - name: p1
    concurrency: 1
    store: memory
    stages:
      - id: s1
        agent: agent-1
        input_schema:
          name: in
          version: "v1"
        output_schema:
          name: out
          version: "v1"
        timeout: 10s
        on_success: done
        on_failure: dead-letter

agents:
  - id: agent-1
    provider: anthropic
    model: claude-sonnet-4-20250514
    auth:
      api_key_env: ANTHROPIC_API_KEY
    timeout: 10s
  - id: agent-1
    provider: google
    model: gemini-2.0-flash
    auth:
      api_key_env: GEMINI_API_KEY
    timeout: 10s
`
	path := writeTestConfig(t, cfg)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for duplicate agent IDs")
	}
	if !strings.Contains(err.Error(), "duplicate agent ID") {
		t.Errorf("error should mention 'duplicate agent ID', got: %v", err)
	}
}

func TestLoad_DuplicatePipelineNames(t *testing.T) {
	cfg := `
version: "1"

schema_registry:
  - name: in
    version: "v1"
    path: schemas/in.json
  - name: out
    version: "v1"
    path: schemas/out.json

pipelines:
  - name: same-name
    concurrency: 1
    store: memory
    stages:
      - id: s1
        agent: a1
        input_schema:
          name: in
          version: "v1"
        output_schema:
          name: out
          version: "v1"
        timeout: 10s
        on_success: done
        on_failure: dead-letter
  - name: same-name
    concurrency: 1
    store: memory
    stages:
      - id: s2
        agent: a1
        input_schema:
          name: in
          version: "v1"
        output_schema:
          name: out
          version: "v1"
        timeout: 10s
        on_success: done
        on_failure: dead-letter

agents:
  - id: a1
    provider: anthropic
    model: claude-sonnet-4-20250514
    auth:
      api_key_env: ANTHROPIC_API_KEY
    timeout: 10s
`
	path := writeTestConfig(t, cfg)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for duplicate pipeline names")
	}
	if !strings.Contains(err.Error(), "duplicate pipeline name") {
		t.Errorf("error should mention 'duplicate pipeline name', got: %v", err)
	}
}

func TestLoad_InvalidOnSuccess(t *testing.T) {
	cfg := strings.Replace(validConfig, "on_success: done", "on_success: nonexistent-stage", 1)
	path := writeTestConfig(t, cfg)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for invalid on_success target")
	}
	if !strings.Contains(err.Error(), "nonexistent-stage") {
		t.Errorf("error should mention the unknown stage, got: %v", err)
	}
}

func TestLoad_DuplicateSchemaNameVersion(t *testing.T) {
	// Same name + same version = duplicate, must error.
	cfg := `
version: "1"

schema_registry:
  - name: dupe
    version: "v1"
    path: schemas/a.json
  - name: dupe
    version: "v1"
    path: schemas/b.json

pipelines: []
agents: []
`
	path := writeTestConfig(t, cfg)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for duplicate schema name+version")
	}
	if !strings.Contains(err.Error(), "duplicate schema registry entry") {
		t.Errorf("error should mention 'duplicate schema registry entry', got: %v", err)
	}
}

// ─── SEC2-NEW-001: Symlink and error sanitization tests ─────────────────────

func TestLoad_RejectsSymlinkToPasswd(t *testing.T) {
	dir := t.TempDir()
	symlink := filepath.Join(dir, "evil.yaml")
	if err := os.Symlink("/etc/passwd", symlink); err != nil {
		t.Skip("cannot create symlink")
	}

	_, err := Load(symlink)
	if err == nil {
		t.Fatal("expected error for symlink config")
	}

	errStr := err.Error()
	if !strings.Contains(errStr, "symlink") {
		t.Errorf("expected error to mention symlink, got: %v", err)
	}
	// Must NOT contain /etc/passwd content
	if strings.Contains(errStr, "root:x:") || strings.Contains(errStr, "/bin/bash") {
		t.Errorf("error leaks file content: %s", errStr)
	}
}

func TestLoad_RejectsSymlinkToNonexistent(t *testing.T) {
	dir := t.TempDir()
	symlink := filepath.Join(dir, "dangling.yaml")
	if err := os.Symlink("/nonexistent/path", symlink); err != nil {
		t.Skip("cannot create symlink")
	}

	_, err := Load(symlink)
	if err == nil {
		t.Fatal("expected error for dangling symlink")
	}
	// Lstat succeeds on a dangling symlink — it reports the symlink itself.
	// So we should get the "must not be a symlink" error.
	if !strings.Contains(err.Error(), "symlink") {
		t.Errorf("expected symlink rejection, got: %v", err)
	}
}

func TestLoad_MalformedYAML_SanitizedError(t *testing.T) {
	// YAML syntax errors don't include raw values — they're safe.
	// Unmarshal errors (type mismatch) DO include raw values and must be sanitized.
	content := `this is not valid yaml: [[[`
	path := writeTestConfig(t, content)

	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for malformed YAML")
	}

	errStr := err.Error()
	if !strings.Contains(errStr, "config.yaml") {
		t.Errorf("expected filename in error, got: %v", err)
	}
}

func TestLoad_UnmarshalError_SanitizedError(t *testing.T) {
	// When a non-YAML file (e.g. /etc/passwd content) is parsed, yaml.Unmarshal
	// produces "cannot unmarshal !!str 'root:x:...' into config.Config" which
	// leaks the file content. This must be sanitized.
	content := "root:x:0:0:root:/root:/bin/bash\nnobody:x:65534:65534:nobody:/nonexistent:/usr/sbin/nologin"
	path := writeTestConfig(t, content)

	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for non-config content")
	}

	errStr := err.Error()
	if !strings.Contains(errStr, "invalid YAML structure") {
		t.Errorf("expected 'invalid YAML structure', got: %v", err)
	}
	// Must not contain file content
	if strings.Contains(errStr, "root:x:") || strings.Contains(errStr, "/bin/bash") {
		t.Errorf("error leaks file content: %s", errStr)
	}
}

func TestLoad_ValidConfig_Regression(t *testing.T) {
	// Regression: valid config must still load correctly after symlink checks.
	path := writeTestConfig(t, validConfig)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("valid config should load, got: %v", err)
	}
	if cfg.Version != "1" {
		t.Errorf("expected version 1, got %q", cfg.Version)
	}
}

func TestLoad_BasicYAMLFile(t *testing.T) {
	// Test loading the actual basic.yaml example
	path := filepath.Join("..", "..", "config", "examples", "basic.yaml")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("basic.yaml should load cleanly, got: %v", err)
	}
	if len(cfg.Pipelines) != 1 {
		t.Errorf("expected 1 pipeline, got %d", len(cfg.Pipelines))
	}
	if len(cfg.Pipelines[0].Stages) != 3 {
		t.Errorf("expected 3 stages, got %d", len(cfg.Pipelines[0].Stages))
	}
	if len(cfg.Agents) != 3 {
		t.Errorf("expected 3 agents, got %d", len(cfg.Agents))
	}
	if len(cfg.SchemaRegistry) != 6 {
		t.Errorf("expected 6 schema entries, got %d", len(cfg.SchemaRegistry))
	}
}
