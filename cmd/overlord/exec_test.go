package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeExecTestYAML writes an ollama-backed infra+pipeline config that
// points at the provided httptest server. Returns the config path.
func writeExecTestYAML(t *testing.T, ollamaURL string) string {
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
        timeout: 5s
        retry:
          max_attempts: 1
          backoff: fixed
          base_delay: 50ms
        on_success: done
        on_failure: dead-letter

agents:
  - id: test-agent
    provider: ollama
    model: llama3
    system_prompt: "test"
    temperature: 0.0
    max_tokens: 1024
    timeout: 5s

stores:
  memory:
    max_tasks: 1000
`
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OLLAMA_ENDPOINT", ollamaURL)
	return configPath
}

// ollamaEchoServer returns a mock ollama /api/chat endpoint that responds
// with the given content (JSON) as the assistant's reply.
func ollamaServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

func ollamaOKResponse(content string) string {
	return fmt.Sprintf(`{"message":{"role":"assistant","content":%q},"prompt_eval_count":1,"eval_count":1}`, content)
}

func runExecCmd(t *testing.T, args ...string) (exitCode int, stdout, stderr string) {
	t.Helper()
	root := rootCmd()
	root.SetArgs(append([]string{"exec"}, args...))
	var outBuf, errBuf bytes.Buffer
	root.SetOut(&outBuf)
	root.SetErr(&errBuf)

	err := root.Execute()
	if err == nil {
		return 0, outBuf.String(), errBuf.String()
	}
	var ee *execExitError
	if errors.As(err, &ee) {
		// Mirror main()'s behavior: print the error message to stderr so
		// tests can assert against the same output a user would see.
		if ee.msg != "" {
			fmt.Fprintln(&errBuf, "Error:", ee.msg)
		}
		return ee.code, outBuf.String(), errBuf.String()
	}
	fmt.Fprintln(&errBuf, "Error:", err)
	return 1, outBuf.String(), errBuf.String()
}

func TestExec_HappyPath(t *testing.T) {
	srv := ollamaServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(ollamaOKResponse(`{"response":"hello"}`)))
	})
	cfg := writeExecTestYAML(t, srv.URL)

	code, stdout, stderr := runExecCmd(t,
		"--config", cfg,
		"--id", "test-pipeline",
		"--payload", `{"request":"hi"}`,
		"--timeout", "10s",
	)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, `"response"`) {
		t.Errorf("expected payload on stdout, got: %s", stdout)
	}
	if !strings.Contains(stderr, "Completed") {
		t.Errorf("expected progress output on stderr, got: %s", stderr)
	}
}

func TestExec_TaskFails(t *testing.T) {
	srv := ollamaServer(t, func(w http.ResponseWriter, r *http.Request) {
		// 400 is non-retryable → task fails after max_attempts
		http.Error(w, "bad request", http.StatusBadRequest)
	})
	cfg := writeExecTestYAML(t, srv.URL)

	code, _, stderr := runExecCmd(t,
		"--config", cfg,
		"--id", "test-pipeline",
		"--payload", `{"request":"hi"}`,
		"--timeout", "10s",
	)
	if code != 1 {
		t.Fatalf("expected exit 1, got %d\nstderr: %s", code, stderr)
	}
	if !strings.Contains(stderr, "dead-letter") && !strings.Contains(stderr, "FAILED") {
		t.Errorf("expected dead-letter or FAILED marker on stderr, got: %s", stderr)
	}
}

func TestExec_Timeout(t *testing.T) {
	srv := ollamaServer(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(3 * time.Second)
		w.Write([]byte(ollamaOKResponse(`{"response":"late"}`)))
	})
	cfg := writeExecTestYAML(t, srv.URL)

	code, _, stderr := runExecCmd(t,
		"--config", cfg,
		"--id", "test-pipeline",
		"--payload", `{"request":"hi"}`,
		"--timeout", "500ms",
	)
	if code != 2 {
		t.Fatalf("expected exit 2 (timeout), got %d\nstderr: %s", code, stderr)
	}
}

func TestExec_OutputJSON(t *testing.T) {
	srv := ollamaServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(ollamaOKResponse(`{"response":"ok"}`)))
	})
	cfg := writeExecTestYAML(t, srv.URL)

	code, stdout, _ := runExecCmd(t,
		"--config", cfg,
		"--id", "test-pipeline",
		"--payload", `{"request":"hi"}`,
		"--output", "json",
		"--quiet",
		"--timeout", "10s",
	)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
	// stdout must be parseable JSON only (no "Result (stage…)" header).
	if strings.Contains(stdout, "Result") {
		t.Errorf("--output json --quiet should not print Result header, got: %s", stdout)
	}
	var v map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &v); err != nil {
		t.Fatalf("stdout is not valid JSON: %v\n%s", err, stdout)
	}
	if v["response"] != "ok" {
		t.Errorf("unexpected payload: %v", v)
	}
}

func TestExec_PayloadFromSymlink(t *testing.T) {
	srv := ollamaServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(ollamaOKResponse(`{"response":"ok"}`)))
	})
	cfg := writeExecTestYAML(t, srv.URL)

	dir := t.TempDir()
	target := filepath.Join(dir, "real.json")
	if err := os.WriteFile(target, []byte(`{"request":"hi"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	code, _, stderr := runExecCmd(t,
		"--config", cfg,
		"--id", "test-pipeline",
		"--payload", "@"+link,
		"--timeout", "5s",
	)
	if code != execExitConfig {
		t.Fatalf("expected exit %d, got %d\nstderr: %s", execExitConfig, code, stderr)
	}
	if !strings.Contains(stderr, "symlink") {
		t.Errorf("expected stderr to mention 'symlink', got: %s", stderr)
	}
}

func TestExec_PayloadFileNotFound(t *testing.T) {
	srv := ollamaServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(ollamaOKResponse(`{"response":"ok"}`)))
	})
	cfg := writeExecTestYAML(t, srv.URL)

	missing := filepath.Join(t.TempDir(), "missing.json")
	code, _, stderr := runExecCmd(t,
		"--config", cfg,
		"--id", "test-pipeline",
		"--payload", "@"+missing,
		"--timeout", "5s",
	)
	if code != execExitConfig {
		t.Fatalf("expected exit %d, got %d\nstderr: %s", execExitConfig, code, stderr)
	}
	if !strings.Contains(stderr, "payload file not found") {
		t.Errorf("expected human-readable not-found message, got: %s", stderr)
	}
	// Don't expose wrapped Go errno like "no such file or directory: open ...".
	if strings.Contains(stderr, "open ") && strings.Contains(stderr, ": no such") {
		t.Errorf("error message leaks raw Go error: %s", stderr)
	}
}

func TestExec_PayloadFromFile(t *testing.T) {
	srv := ollamaServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(ollamaOKResponse(`{"response":"ok"}`)))
	})
	cfg := writeExecTestYAML(t, srv.URL)

	dir := t.TempDir()
	payloadFile := filepath.Join(dir, "payload.json")
	if err := os.WriteFile(payloadFile, []byte(`{"request":"from-file"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	code, _, stderr := runExecCmd(t,
		"--config", cfg,
		"--id", "test-pipeline",
		"--payload", "@"+payloadFile,
		"--timeout", "10s",
	)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d\nstderr: %s", code, stderr)
	}
}

// TestExec_PipelineFileFlag: infra config + separate pipeline file.
func TestExec_PipelineFileFlag(t *testing.T) {
	srv := ollamaServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(ollamaOKResponse(`{"response":"ok"}`)))
	})

	dir := t.TempDir()
	schemasDir := filepath.Join(dir, "schemas")
	os.MkdirAll(schemasDir, 0o755)
	in := `{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","properties":{"request":{"type":"string"}},"required":["request"]}`
	out := `{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","properties":{"response":{"type":"string"}},"required":["response"]}`
	os.WriteFile(filepath.Join(schemasDir, "in.json"), []byte(in), 0o644)
	os.WriteFile(filepath.Join(schemasDir, "out.json"), []byte(out), 0o644)

	infra := `version: "1"
agents:
  - id: test-agent
    provider: ollama
    model: llama3
    system_prompt: "t"
    timeout: 5s
stores:
  memory:
    max_tasks: 100
`
	infraPath := filepath.Join(dir, "infra.yaml")
	os.WriteFile(infraPath, []byte(infra), 0o644)

	pipe := `version: "1"
schema_registry:
  - name: task_in
    version: "v1"
    path: schemas/in.json
  - name: task_out
    version: "v1"
    path: schemas/out.json
pipelines:
  - name: split-pipe
    concurrency: 1
    store: memory
    stages:
      - id: process
        agent: test-agent
        input_schema: { name: task_in, version: "v1" }
        output_schema: { name: task_out, version: "v1" }
        timeout: 5s
        retry: { max_attempts: 1, backoff: fixed, base_delay: 50ms }
        on_success: done
        on_failure: dead-letter
`
	pipePath := filepath.Join(dir, "pipe.yaml")
	os.WriteFile(pipePath, []byte(pipe), 0o644)

	t.Setenv("OLLAMA_ENDPOINT", srv.URL)

	code, stdout, stderr := runExecCmd(t,
		"--config", infraPath,
		"--pipeline", pipePath,
		"--id", "split-pipe",
		"--payload", `{"request":"hi"}`,
		"--timeout", "10s",
	)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d\nstderr: %s\nstdout: %s", code, stderr, stdout)
	}
	if !strings.Contains(stdout, `"response"`) {
		t.Errorf("expected response payload on stdout, got: %s", stdout)
	}
}

// TestExec_PluginBinaryNotFound: a plugin agent whose manifest points at a
// nonexistent binary must fail at agent-registry construction (exit 3 =
// config error), not at task execution (exit 1 = task failure).
func TestExec_PluginBinaryNotFound(t *testing.T) {
	dir := t.TempDir()
	schemasDir := filepath.Join(dir, "schemas")
	if err := os.MkdirAll(schemasDir, 0o755); err != nil {
		t.Fatal(err)
	}
	inSchema := `{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","properties":{"request":{"type":"string"}},"required":["request"]}`
	outSchema := `{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","properties":{"response":{"type":"string"}},"required":["response"]}`
	os.WriteFile(filepath.Join(schemasDir, "in_v1.json"), []byte(inSchema), 0o644)
	os.WriteFile(filepath.Join(schemasDir, "out_v1.json"), []byte(outSchema), 0o644)

	// Write a manifest that points at a binary that does not exist.
	manifestPath := filepath.Join(dir, "plugin-manifest.yaml")
	os.WriteFile(manifestPath, []byte("name: fake\nbinary: ./does_not_exist\n"), 0o600)

	// plugins.enabled: true is required to reach the binary-existence
	// check at all; without the opt-in, validate() would refuse the
	// config itself. The test covers the "binary not found" path, not
	// the gate — see TestExec_PluginGateRefusesWhenDisabled below.
	yaml := `version: "1"

plugins:
  enabled: true

schema_registry:
  - name: task_in
    version: "v1"
    path: schemas/in_v1.json
  - name: task_out
    version: "v1"
    path: schemas/out_v1.json

pipelines:
  - name: plugin-pipe
    concurrency: 1
    store: memory
    stages:
      - id: only
        agent: bad-plugin
        input_schema: { name: task_in, version: "v1" }
        output_schema: { name: task_out, version: "v1" }
        timeout: 5s
        retry: { max_attempts: 1, backoff: fixed, base_delay: 50ms }
        on_success: done
        on_failure: dead-letter

agents:
  - id: bad-plugin
    provider: plugin
    manifest: ` + manifestPath + `

stores:
  memory:
    max_tasks: 1000
`
	configPath := filepath.Join(dir, "config.yaml")
	os.WriteFile(configPath, []byte(yaml), 0o644)

	code, _, stderr := runExecCmd(t,
		"--config", configPath,
		"--id", "plugin-pipe",
		"--payload", `{"request":"hi"}`,
		"--timeout", "5s",
	)
	if code != execExitConfig {
		t.Fatalf("expected exit %d (config error), got %d\nstderr: %s", execExitConfig, code, stderr)
	}
	if !strings.Contains(stderr, "not found") {
		t.Errorf("expected 'not found' in error, got: %s", stderr)
	}
}

// TestExec_PluginGateRefusesWhenDisabled verifies the fail-closed opt-in:
// a config that references provider: plugin without plugins.enabled: true
// must be refused at config load (exit 3 = config error), not silently
// accepted and exec'd. This is the CLI-level half of the defense that
// complements the validator-level test in internal/config.
func TestExec_PluginGateRefusesWhenDisabled(t *testing.T) {
	dir := t.TempDir()
	schemasDir := filepath.Join(dir, "schemas")
	if err := os.MkdirAll(schemasDir, 0o755); err != nil {
		t.Fatal(err)
	}
	inSchema := `{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","properties":{"request":{"type":"string"}},"required":["request"]}`
	outSchema := `{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","properties":{"response":{"type":"string"}},"required":["response"]}`
	os.WriteFile(filepath.Join(schemasDir, "in_v1.json"), []byte(inSchema), 0o644)
	os.WriteFile(filepath.Join(schemasDir, "out_v1.json"), []byte(outSchema), 0o644)

	manifestPath := filepath.Join(dir, "plugin-manifest.yaml")
	os.WriteFile(manifestPath, []byte("name: fake\nbinary: ./some_binary\n"), 0o600)

	// No plugins block at all — the operator has not opted in, so this
	// config must be rejected even though every other field is valid.
	yaml := `version: "1"

schema_registry:
  - name: task_in
    version: "v1"
    path: schemas/in_v1.json
  - name: task_out
    version: "v1"
    path: schemas/out_v1.json

pipelines:
  - name: gated
    concurrency: 1
    store: memory
    stages:
      - id: only
        agent: plug
        input_schema: { name: task_in, version: "v1" }
        output_schema: { name: task_out, version: "v1" }
        timeout: 5s
        retry: { max_attempts: 1, backoff: fixed, base_delay: 50ms }
        on_success: done
        on_failure: dead-letter

agents:
  - id: plug
    provider: plugin
    manifest: ` + manifestPath + `

stores:
  memory:
    max_tasks: 1000
`
	configPath := filepath.Join(dir, "config.yaml")
	os.WriteFile(configPath, []byte(yaml), 0o644)

	code, _, stderr := runExecCmd(t,
		"--config", configPath,
		"--id", "gated",
		"--payload", `{"request":"hi"}`,
		"--timeout", "5s",
	)
	if code != execExitConfig {
		t.Fatalf("expected exit %d (config error) for disabled-plugin config, got %d\nstderr: %s", execExitConfig, code, stderr)
	}
	if !strings.Contains(stderr, "plugins are disabled") {
		t.Errorf("expected 'plugins are disabled' in error, got: %s", stderr)
	}
	if !strings.Contains(stderr, "plugins.enabled: true") {
		t.Errorf("expected error to tell operator how to opt in, got: %s", stderr)
	}
}
