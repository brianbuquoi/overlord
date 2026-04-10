package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// writeMinimalSchemaFile writes a minimal valid JSON schema to path.
func writeMinimalSchemaFile(t *testing.T, path string) {
	t.Helper()
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("creating schema dir: %v", err)
	}
	if err := os.WriteFile(path, []byte(`{"type": "object"}`), 0644); err != nil {
		t.Fatalf("writing schema file: %v", err)
	}
}

// writeCfgFile writes YAML content to a config file path.
func writeCfgFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("writing config file: %v", err)
	}
}

// minimalValidYAML returns the smallest valid YAML config. Schema paths are
// relative, so callers must create corresponding JSON files in the temp dir.
func minimalValidYAML() string {
	return `version: "1"

schema_registry:
  - name: in
    version: "v1"
    path: schemas/in_v1.json
  - name: out
    version: "v1"
    path: schemas/out_v1.json

pipelines:
  - name: p1
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

agents:
  - id: a1
    provider: anthropic
    model: claude-sonnet-4-20250514
    auth:
      api_key_env: ANTHROPIC_API_KEY
    system_prompt: "test"
    temperature: 0.0
    max_tokens: 100
    timeout: 10s

stores:
  memory:
    max_tasks: 100
`
}

// setupMinimalCfg creates a temp dir with valid config + schema files and
// returns the config file path.
func setupMinimalCfg(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	writeCfgFile(t, cfgPath, minimalValidYAML())
	writeMinimalSchemaFile(t, filepath.Join(dir, "schemas", "in_v1.json"))
	writeMinimalSchemaFile(t, filepath.Join(dir, "schemas", "out_v1.json"))
	return cfgPath
}

// ---------------------------------------------------------------------------
// 1.1 TestWatch_NoDebounce
// ---------------------------------------------------------------------------

// TestWatch_NoDebounce documents that Watch does NOT debounce filesystem
// events. Each write to the config file triggers a separate reload callback.
// This is a known behavioural characteristic, not a bug -- but callers should
// be aware that rapid config writes produce rapid reload calls.
func TestWatch_NoDebounce(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	writeCfgFile(t, cfgPath, minimalValidYAML())
	writeMinimalSchemaFile(t, filepath.Join(dir, "schemas", "in_v1.json"))
	writeMinimalSchemaFile(t, filepath.Join(dir, "schemas", "out_v1.json"))

	var callCount atomic.Int64

	err := Watch(cfgPath, func(cfg *Config) {
		callCount.Add(1)
	})
	if err != nil {
		t.Fatalf("Watch returned error: %v", err)
	}

	// Give the watcher goroutine time to start.
	time.Sleep(100 * time.Millisecond)

	// Write the config file 3 times with 50ms intervals.
	for i := 0; i < 3; i++ {
		writeCfgFile(t, cfgPath, minimalValidYAML())
		time.Sleep(50 * time.Millisecond)
	}

	// Allow time for all events to be processed.
	deadline := time.After(3 * time.Second)
	for {
		if callCount.Load() >= 3 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("expected onChange to be called at least 3 times (no debounce), got %d calls", callCount.Load())
		default:
			time.Sleep(20 * time.Millisecond)
		}
	}

	// Watch has no debounce: each write fired a separate callback.
	got := callCount.Load()
	if got < 3 {
		t.Fatalf("expected at least 3 onChange calls (no debounce), got %d", got)
	}
	t.Logf("onChange called %d times for 3 writes -- confirms Watch does NOT debounce", got)
}

// ---------------------------------------------------------------------------
// 1.2 TestLoad_MinimalConfig
// ---------------------------------------------------------------------------

// TestLoad_MinimalConfig verifies that a config with only required fields
// loads successfully and that optional fields have expected zero-value defaults.
func TestLoad_MinimalConfig(t *testing.T) {
	cfgPath := setupMinimalCfg(t)

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	// At least one pipeline with at least one stage.
	if len(cfg.Pipelines) < 1 {
		t.Fatal("expected at least one pipeline")
	}
	p := cfg.Pipelines[0]
	if len(p.Stages) < 1 {
		t.Fatal("expected at least one stage in pipeline")
	}

	// Agents referenced by stages exist.
	agentIDs := make(map[string]bool)
	for _, a := range cfg.Agents {
		agentIDs[a.ID] = true
	}
	for _, s := range p.Stages {
		if s.Agent != "" && !agentIDs[s.Agent] {
			t.Errorf("stage %q references unknown agent %q", s.ID, s.Agent)
		}
	}

	// Schema registry entries exist for referenced schemas.
	schemaNames := make(map[string]bool)
	for _, se := range cfg.SchemaRegistry {
		schemaNames[schemaKey(se.Name, se.Version)] = true
	}
	for _, s := range p.Stages {
		inKey := schemaKey(s.InputSchema.Name, s.InputSchema.Version)
		if !schemaNames[inKey] {
			t.Errorf("stage %q input_schema %s not in registry", s.ID, inKey)
		}
		outKey := schemaKey(s.OutputSchema.Name, s.OutputSchema.Version)
		if !schemaNames[outKey] {
			t.Errorf("stage %q output_schema %s not in registry", s.ID, outKey)
		}
	}

	// Default values: Concurrency defaults to 0 (broker treats <1 as 1).
	if p.Concurrency != 0 {
		t.Errorf("expected default Concurrency 0, got %d", p.Concurrency)
	}

	s := p.Stages[0]

	// Retry.MaxAttempts defaults to 0 (no retries).
	if s.Retry.MaxAttempts != 0 {
		t.Errorf("expected default Retry.MaxAttempts 0, got %d", s.Retry.MaxAttempts)
	}

	// OnSuccess defaults to empty string (static route "").
	if s.OnSuccess.Static != "" {
		t.Errorf("expected default OnSuccess.Static empty string, got %q", s.OnSuccess.Static)
	}

	// OnFailure defaults to empty string.
	if s.OnFailure != "" {
		t.Errorf("expected default OnFailure empty string, got %q", s.OnFailure)
	}
}

// ---------------------------------------------------------------------------
// 1.3 TestValidate_LargeSchemaRegistry
// ---------------------------------------------------------------------------

// TestValidate_LargeSchemaRegistry verifies that Load handles a config with
// 50+ schema_registry entries without error and that all schemas resolve
// correctly when referenced by stages.
func TestValidate_LargeSchemaRegistry(t *testing.T) {
	dir := t.TempDir()
	schemasDir := filepath.Join(dir, "schemas")
	if err := os.MkdirAll(schemasDir, 0755); err != nil {
		t.Fatal(err)
	}

	const numSchemas = 55

	// Build schema_registry YAML entries and create JSON files.
	var registryLines string
	for i := 0; i < numSchemas; i++ {
		name := fmt.Sprintf("schema_%03d", i)
		fname := fmt.Sprintf("%s_v1.json", name)
		registryLines += fmt.Sprintf("  - name: %s\n    version: \"v1\"\n    path: schemas/%s\n", name, fname)
		writeMinimalSchemaFile(t, filepath.Join(schemasDir, fname))
	}

	// Use first two schemas for the stage.
	yamlContent := fmt.Sprintf(`version: "1"

schema_registry:
%s
pipelines:
  - name: big-registry
    store: memory
    stages:
      - id: s1
        agent: a1
        input_schema:
          name: schema_000
          version: "v1"
        output_schema:
          name: schema_001
          version: "v1"

agents:
  - id: a1
    provider: anthropic
    model: claude-sonnet-4-20250514
    auth:
      api_key_env: ANTHROPIC_API_KEY
    system_prompt: "test"
    temperature: 0.0
    max_tokens: 100
    timeout: 10s

stores:
  memory:
    max_tasks: 100
`, registryLines)

	cfgPath := filepath.Join(dir, "config.yaml")
	writeCfgFile(t, cfgPath, yamlContent)

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load failed with %d schema entries: %v", numSchemas, err)
	}

	if len(cfg.SchemaRegistry) != numSchemas {
		t.Errorf("expected %d schema_registry entries, got %d", numSchemas, len(cfg.SchemaRegistry))
	}

	// Verify all schemas are individually accessible in the index.
	index, err := collectSchemaRegistry(cfg.SchemaRegistry)
	if err != nil {
		t.Fatalf("collectSchemaRegistry failed: %v", err)
	}
	for i := 0; i < numSchemas; i++ {
		name := fmt.Sprintf("schema_%03d", i)
		key := schemaKey(name, "v1")
		if _, ok := index[key]; !ok {
			t.Errorf("schema %s not found in index", key)
		}
	}
}

// ---------------------------------------------------------------------------
// 1.4 TestValidate_CircularStageReferences
// ---------------------------------------------------------------------------

// TestValidate_CircularStageReferences verifies that validation now detects
// closed routing cycles with no exit path. A pipeline where stage A routes to
// B and stage B routes back to A (with no path to done/dead-letter) is rejected.
// Previously this was a known gap — now resolved with cycle detection.
func TestValidate_CircularStageReferences(t *testing.T) {
	dir := t.TempDir()
	writeMinimalSchemaFile(t, filepath.Join(dir, "schemas", "in_v1.json"))
	writeMinimalSchemaFile(t, filepath.Join(dir, "schemas", "out_v1.json"))

	yamlContent := `version: "1"

schema_registry:
  - name: in
    version: "v1"
    path: schemas/in_v1.json
  - name: out
    version: "v1"
    path: schemas/out_v1.json

pipelines:
  - name: circular
    store: memory
    stages:
      - id: stage-a
        agent: a1
        input_schema:
          name: in
          version: "v1"
        output_schema:
          name: out
          version: "v1"
        on_success: stage-b
        on_failure: stage-b
      - id: stage-b
        agent: a1
        input_schema:
          name: in
          version: "v1"
        output_schema:
          name: out
          version: "v1"
        on_success: stage-a
        on_failure: stage-a

agents:
  - id: a1
    provider: anthropic
    model: claude-sonnet-4-20250514
    auth:
      api_key_env: ANTHROPIC_API_KEY
    system_prompt: "test"
    temperature: 0.0
    max_tokens: 100
    timeout: 10s

stores:
  memory:
    max_tasks: 100
`

	cfgPath := filepath.Join(dir, "config.yaml")
	writeCfgFile(t, cfgPath, yamlContent)

	// Closed cycle (no exit to done/dead-letter) — must be rejected.
	_, err := Load(cfgPath)
	if err == nil {
		t.Fatal("expected closed cycle to be rejected by validation")
	}
	if !strings.Contains(err.Error(), "routing cycle") {
		t.Errorf("expected 'routing cycle' in error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 1.5 TestValidate_SameOnSuccessOnFailure
// ---------------------------------------------------------------------------

// TestValidate_SameOnSuccessOnFailure verifies that a stage may point both
// on_success and on_failure to the same target stage. This is valid config.
func TestValidate_SameOnSuccessOnFailure(t *testing.T) {
	dir := t.TempDir()
	writeMinimalSchemaFile(t, filepath.Join(dir, "schemas", "in_v1.json"))
	writeMinimalSchemaFile(t, filepath.Join(dir, "schemas", "out_v1.json"))

	yamlContent := `version: "1"

schema_registry:
  - name: in
    version: "v1"
    path: schemas/in_v1.json
  - name: out
    version: "v1"
    path: schemas/out_v1.json

pipelines:
  - name: same-target
    store: memory
    stages:
      - id: first
        agent: a1
        input_schema:
          name: in
          version: "v1"
        output_schema:
          name: out
          version: "v1"
        on_success: second
        on_failure: second
      - id: second
        agent: a1
        input_schema:
          name: in
          version: "v1"
        output_schema:
          name: out
          version: "v1"
        on_success: done
        on_failure: dead-letter

agents:
  - id: a1
    provider: anthropic
    model: claude-sonnet-4-20250514
    auth:
      api_key_env: ANTHROPIC_API_KEY
    system_prompt: "test"
    temperature: 0.0
    max_tokens: 100
    timeout: 10s

stores:
  memory:
    max_tasks: 100
`

	cfgPath := filepath.Join(dir, "config.yaml")
	writeCfgFile(t, cfgPath, yamlContent)

	_, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("expected on_success and on_failure pointing to same stage to be valid, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 1.6 TestValidate_FanOutAggregateSchemaInRegistry
// ---------------------------------------------------------------------------

// TestValidate_FanOutAggregateSchemaInRegistry verifies that:
// (a) a fan-out stage with aggregate_schema referencing a valid registry entry passes, and
// (b) a fan-out stage with aggregate_schema referencing a name NOT in the registry fails.
func TestValidate_FanOutAggregateSchemaInRegistry(t *testing.T) {
	t.Run("valid_aggregate_schema_in_registry", func(t *testing.T) {
		dir := t.TempDir()
		writeMinimalSchemaFile(t, filepath.Join(dir, "schemas", "in_v1.json"))
		writeMinimalSchemaFile(t, filepath.Join(dir, "schemas", "out_v1.json"))
		writeMinimalSchemaFile(t, filepath.Join(dir, "schemas", "agg_v1.json"))

		yamlContent := `version: "1"

schema_registry:
  - name: in
    version: "v1"
    path: schemas/in_v1.json
  - name: out
    version: "v1"
    path: schemas/out_v1.json
  - name: agg
    version: "v1"
    path: schemas/agg_v1.json

pipelines:
  - name: fanout-valid
    store: memory
    stages:
      - id: fan
        fan_out:
          agents:
            - id: a1
            - id: a2
          mode: gather
          timeout: 30s
          require: all
        input_schema:
          name: in
          version: "v1"
        output_schema:
          name: out
          version: "v1"
        aggregate_schema:
          name: agg
          version: "v1"

agents:
  - id: a1
    provider: anthropic
    model: claude-sonnet-4-20250514
    auth:
      api_key_env: ANTHROPIC_API_KEY
    system_prompt: "agent 1"
    temperature: 0.0
    max_tokens: 100
    timeout: 10s
  - id: a2
    provider: anthropic
    model: claude-sonnet-4-20250514
    auth:
      api_key_env: ANTHROPIC_API_KEY
    system_prompt: "agent 2"
    temperature: 0.0
    max_tokens: 100
    timeout: 10s

stores:
  memory:
    max_tasks: 100
`

		cfgPath := filepath.Join(dir, "config.yaml")
		writeCfgFile(t, cfgPath, yamlContent)

		_, err := Load(cfgPath)
		if err != nil {
			t.Fatalf("expected valid fan-out with aggregate_schema in registry to pass, got: %v", err)
		}
	})

	t.Run("invalid_aggregate_schema_not_in_registry", func(t *testing.T) {
		dir := t.TempDir()
		writeMinimalSchemaFile(t, filepath.Join(dir, "schemas", "in_v1.json"))
		writeMinimalSchemaFile(t, filepath.Join(dir, "schemas", "out_v1.json"))
		// Note: no agg schema file or registry entry is created.

		yamlContent := `version: "1"

schema_registry:
  - name: in
    version: "v1"
    path: schemas/in_v1.json
  - name: out
    version: "v1"
    path: schemas/out_v1.json

pipelines:
  - name: fanout-invalid
    store: memory
    stages:
      - id: fan
        fan_out:
          agents:
            - id: a1
            - id: a2
          mode: gather
          timeout: 30s
          require: all
        input_schema:
          name: in
          version: "v1"
        output_schema:
          name: out
          version: "v1"
        aggregate_schema:
          name: missing_agg
          version: "v1"

agents:
  - id: a1
    provider: anthropic
    model: claude-sonnet-4-20250514
    auth:
      api_key_env: ANTHROPIC_API_KEY
    system_prompt: "agent 1"
    temperature: 0.0
    max_tokens: 100
    timeout: 10s
  - id: a2
    provider: anthropic
    model: claude-sonnet-4-20250514
    auth:
      api_key_env: ANTHROPIC_API_KEY
    system_prompt: "agent 2"
    temperature: 0.0
    max_tokens: 100
    timeout: 10s

stores:
  memory:
    max_tasks: 100
`

		cfgPath := filepath.Join(dir, "config.yaml")
		writeCfgFile(t, cfgPath, yamlContent)

		_, err := Load(cfgPath)
		if err == nil {
			t.Fatal("expected error for aggregate_schema referencing name not in registry, got nil")
		}

		// Verify the error mentions the missing schema.
		errMsg := err.Error()
		if !(strContains(errMsg, "missing_agg") && strContains(errMsg, "schema_registry")) {
			t.Errorf("expected error about missing_agg not in schema_registry, got: %v", err)
		}
	})
}

// strContains checks if s contains substr.
func strContains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
