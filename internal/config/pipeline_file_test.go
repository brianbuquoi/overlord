package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writePipelineTestInfra(t *testing.T) *Config {
	t.Helper()
	return &Config{
		Version: "1",
		Agents: []Agent{
			{ID: "reviewer", Provider: "anthropic", Model: "x", Auth: AuthConfig{APIKeyEnv: "X"}},
		},
		SchemaRegistry: []SchemaEntry{
			{Name: "shared_in", Version: "v1", Path: "/tmp/ignored_a.json"},
		},
	}
}

func writePipelineYAML(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "pipeline.yaml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPipelineFile_LoadAndMergeInto_Success(t *testing.T) {
	dir := t.TempDir()
	schemaPath := filepath.Join(dir, "in.json")
	if err := os.WriteFile(schemaPath, []byte(`{"type":"object"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	body := `version: "1"
schema_registry:
  - name: pipe_in
    version: "v1"
    path: ` + schemaPath + `
  - name: pipe_out
    version: "v1"
    path: ` + schemaPath + `

pipelines:
  - name: extra-pipe
    concurrency: 1
    store: memory
    stages:
      - id: s1
        agent: reviewer
        input_schema: { name: pipe_in, version: "v1" }
        output_schema: { name: pipe_out, version: "v1" }
        timeout: 1s
        retry: { max_attempts: 1, backoff: fixed, base_delay: 1s }
        on_success: done
        on_failure: dead-letter
`
	p := writePipelineYAML(t, body)

	pf, pfPath, err := LoadPipelineFile(p)
	if err != nil {
		t.Fatalf("LoadPipelineFile: %v", err)
	}
	cfg := writePipelineTestInfra(t)
	// Give infra a valid schema path for re-validation.
	cfg.SchemaRegistry[0].Path = schemaPath

	if err := pf.MergeInto(cfg, pfPath); err != nil {
		t.Fatalf("MergeInto: %v", err)
	}
	if len(cfg.Pipelines) != 1 || cfg.Pipelines[0].Name != "extra-pipe" {
		t.Fatalf("expected merged pipeline extra-pipe, got %+v", cfg.Pipelines)
	}
	if len(cfg.SchemaRegistry) != 3 {
		t.Fatalf("expected 3 schemas after merge, got %d", len(cfg.SchemaRegistry))
	}
}

func TestPipelineFile_MergeInto_SchemaConflict(t *testing.T) {
	cfg := writePipelineTestInfra(t)
	pf := &PipelineFile{
		Version: "1",
		SchemaRegistry: []SchemaEntry{
			{Name: "shared_in", Version: "v1", Path: "/other.json"},
		},
		Pipelines: []Pipeline{},
	}
	err := pf.MergeInto(cfg, "/tmp/fake-pipeline.yaml")
	if err == nil {
		t.Fatal("expected schema conflict error, got nil")
	}
	if !strings.Contains(err.Error(), "conflicts") {
		t.Errorf("expected conflict error, got: %v", err)
	}
}

func TestPipelineFile_MergeInto_UnknownAgent(t *testing.T) {
	cfg := writePipelineTestInfra(t)
	pf := &PipelineFile{
		Version: "1",
		Pipelines: []Pipeline{{
			Name:        "bad",
			Concurrency: 1,
			Store:       "memory",
			Stages: []Stage{{
				ID:           "s1",
				Agent:        "does-not-exist",
				InputSchema:  StageSchemaRef{Name: "shared_in", Version: "v1"},
				OutputSchema: StageSchemaRef{Name: "shared_in", Version: "v1"},
				OnSuccess:    StaticOnSuccess("done"),
				OnFailure:    "dead-letter",
				Retry:        RetryPolicy{MaxAttempts: 1, Backoff: "fixed"},
			}},
		}},
	}
	err := pf.MergeInto(cfg, "/tmp/fake-pipeline.yaml")
	if err == nil {
		t.Fatal("expected unknown-agent error, got nil")
	}
	if !strings.Contains(err.Error(), "does-not-exist") {
		t.Errorf("expected error to mention agent id, got: %v", err)
	}
}

func TestPipelineFile_MergeInto_VersionMismatch(t *testing.T) {
	cfg := writePipelineTestInfra(t)
	pf := &PipelineFile{Version: "2", Pipelines: []Pipeline{{Name: "p"}}}
	if err := pf.MergeInto(cfg, "/tmp/fake-pipeline.yaml"); err == nil || !strings.Contains(err.Error(), "version") {
		t.Fatalf("expected version-mismatch error, got %v", err)
	}
}

// TestPipelineFile_RelativeSchemaPathRebased verifies that a relative
// schema path inside a pipeline file is rebased against the pipeline
// file's own directory (not the infra config's directory) and stored as
// an absolute path so the schema registry resolves it correctly.
func TestPipelineFile_RelativeSchemaPathRebased(t *testing.T) {
	root := t.TempDir()

	pipelineDir := filepath.Join(root, "pipelines")
	if err := os.MkdirAll(pipelineDir, 0o755); err != nil {
		t.Fatal(err)
	}
	schemaDir := filepath.Join(pipelineDir, "schemas")
	if err := os.MkdirAll(schemaDir, 0o755); err != nil {
		t.Fatal(err)
	}
	schemaFile := filepath.Join(schemaDir, "in.json")
	if err := os.WriteFile(schemaFile, []byte(`{"type":"object"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	body := `version: "1"
schema_registry:
  - name: pipe_in
    version: "v1"
    path: ./schemas/in.json
  - name: pipe_out
    version: "v1"
    path: ./schemas/in.json
pipelines:
  - name: rebased-pipe
    concurrency: 1
    store: memory
    stages:
      - id: s1
        agent: reviewer
        input_schema: { name: pipe_in, version: "v1" }
        output_schema: { name: pipe_out, version: "v1" }
        timeout: 1s
        retry: { max_attempts: 1, backoff: fixed, base_delay: 1s }
        on_success: done
        on_failure: dead-letter
`
	pipelinePath := filepath.Join(pipelineDir, "pipeline.yaml")
	if err := os.WriteFile(pipelinePath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	pf, pfPath, err := LoadPipelineFile(pipelinePath)
	if err != nil {
		t.Fatalf("LoadPipelineFile: %v", err)
	}
	if !filepath.IsAbs(pfPath) {
		t.Errorf("expected absolute pipeline path, got %q", pfPath)
	}

	cfg := writePipelineTestInfra(t)
	cfg.SchemaRegistry[0].Path = schemaFile

	if err := pf.MergeInto(cfg, pfPath); err != nil {
		t.Fatalf("MergeInto: %v", err)
	}

	// The two schemas added from the pipeline file should be absolute
	// and point at the file under pipelineDir/schemas/, not under the
	// process CWD.
	wantAbs, _ := filepath.Abs(schemaFile)
	for _, e := range cfg.SchemaRegistry {
		if e.Name == "pipe_in" || e.Name == "pipe_out" {
			if !filepath.IsAbs(e.Path) {
				t.Errorf("schema %s path %q is not absolute after rebasing", e.Name, e.Path)
			}
			if e.Path != wantAbs {
				t.Errorf("schema %s path = %q, want %q", e.Name, e.Path, wantAbs)
			}
		}
	}
}

// TestPipelineFile_AbsoluteSchemaPathPreserved verifies that an absolute
// schema path declared in a pipeline file is not rewritten by MergeInto.
func TestPipelineFile_AbsoluteSchemaPathPreserved(t *testing.T) {
	dir := t.TempDir()
	schemaFile := filepath.Join(dir, "abs.json")
	if err := os.WriteFile(schemaFile, []byte(`{"type":"object"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	pf := &PipelineFile{
		Version: "1",
		SchemaRegistry: []SchemaEntry{
			{Name: "abs_in", Version: "v1", Path: schemaFile},
		},
		Pipelines: []Pipeline{{
			Name:        "abs-pipe",
			Concurrency: 1,
			Store:       "memory",
			Stages: []Stage{{
				ID:           "s1",
				Agent:        "reviewer",
				InputSchema:  StageSchemaRef{Name: "abs_in", Version: "v1"},
				OutputSchema: StageSchemaRef{Name: "abs_in", Version: "v1"},
				OnSuccess:    StaticOnSuccess("done"),
				OnFailure:    "dead-letter",
				Retry:        RetryPolicy{MaxAttempts: 1, Backoff: "fixed", BaseDelay: Duration{time.Second}},
				Timeout:      Duration{time.Second},
			}},
		}},
	}

	cfg := writePipelineTestInfra(t)
	cfg.SchemaRegistry[0].Path = schemaFile

	pipelinePath := filepath.Join(t.TempDir(), "elsewhere", "pipeline.yaml")
	if err := pf.MergeInto(cfg, pipelinePath); err != nil {
		t.Fatalf("MergeInto: %v", err)
	}

	for _, e := range cfg.SchemaRegistry {
		if e.Name == "abs_in" && e.Path != schemaFile {
			t.Errorf("absolute schema path mutated: got %q want %q", e.Path, schemaFile)
		}
	}
}

// TestPipelineFile_RelativePathMismatch verifies that a relative schema
// path that fails to resolve (file does not exist after rebasing)
// produces a clear error referencing the pipeline file rather than a
// cryptic file-not-found at schema compile time.
func TestPipelineFile_RelativePathMismatch(t *testing.T) {
	dir := t.TempDir()
	pipelinePath := filepath.Join(dir, "pipeline.yaml")
	body := `version: "1"
schema_registry:
  - name: missing
    version: "v1"
    path: ./does-not-exist.json
pipelines:
  - name: bad-pipe
    concurrency: 1
    store: memory
    stages:
      - id: s1
        agent: reviewer
        input_schema: { name: missing, version: "v1" }
        output_schema: { name: missing, version: "v1" }
        timeout: 1s
        retry: { max_attempts: 1, backoff: fixed, base_delay: 1s }
        on_success: done
        on_failure: dead-letter
`
	if err := os.WriteFile(pipelinePath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	pf, pfPath, err := LoadPipelineFile(pipelinePath)
	if err != nil {
		t.Fatalf("LoadPipelineFile: %v", err)
	}

	cfg := writePipelineTestInfra(t)
	// Make infra schema path satisfiable so the only failure cause is
	// the missing pipeline-file schema.
	infraSchema := filepath.Join(dir, "infra.json")
	if err := os.WriteFile(infraSchema, []byte(`{"type":"object"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg.SchemaRegistry[0].Path = infraSchema

	err = pf.MergeInto(cfg, pfPath)
	if err == nil {
		t.Fatal("expected error for missing schema file, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "does-not-exist.json") {
		t.Errorf("error should mention the missing schema path, got: %v", err)
	}
	if !strings.Contains(msg, pfPath) {
		t.Errorf("error should reference the pipeline file %q, got: %v", pfPath, err)
	}
}
