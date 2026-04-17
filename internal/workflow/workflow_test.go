package workflow

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const simpleWorkflow = `version: "1"

workflow:
  id: write-review

  input:
    type: text

  vars:
    audience: "engineering leaders"

  steps:
    - model: mock/draft
      fixture: fixtures/draft.json
      prompt: |
        Draft for {{vars.audience}}:
        {{input}}
    - model: mock/review
      fixture: fixtures/review.json
      prompt: |
        Review this draft:
        {{prev}}
`

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// TestParse_AutoStepIDsAndPrev ensures workflows without explicit step
// IDs auto-generate step_1/step_2 identifiers and that {{prev}} is
// rewritten to {{steps.step_1.output}} by the compile pass.
func TestParse_AutoStepIDsAndPrev(t *testing.T) {
	file, err := Parse([]byte(simpleWorkflow), "workflow.yaml")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if file.Workflow.ID != "write-review" {
		t.Fatalf("id: got %q, want %q", file.Workflow.ID, "write-review")
	}
	if len(file.Workflow.Steps) != 2 {
		t.Fatalf("steps: got %d, want 2", len(file.Workflow.Steps))
	}
}

// TestCompile_PropagatesPrev verifies the compiled chain has linear
// step ordering and that {{prev}} in the second step resolves to the
// first step's auto-generated ID.
func TestCompile_PropagatesPrev(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "fixtures", "draft.json"), `{"text": "hi"}`)
	writeFile(t, filepath.Join(dir, "fixtures", "review.json"), `{"text": "ok"}`)

	file, err := Parse([]byte(simpleWorkflow), "workflow.yaml")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	c, err := Compile(file, dir)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if len(c.Config.Pipelines) != 1 || len(c.Config.Pipelines[0].Stages) != 2 {
		t.Fatalf("unexpected compiled shape: %+v", c.Config)
	}
	stages := c.Config.Pipelines[0].Stages
	if stages[0].ID != "step_1" || stages[1].ID != "step_2" {
		t.Fatalf("stage ids: got %q/%q, want step_1/step_2", stages[0].ID, stages[1].ID)
	}
	// Verify {{prev}} was rewritten to reference step_1.
	prompt := c.Templates["step_2"]
	if !strings.Contains(prompt, "{{steps.step_1.output}}") {
		t.Fatalf("prev not rewritten: %q", prompt)
	}
}

// TestCompile_PrevOnFirstStepRejected documents the first-step guard —
// {{prev}} needs a prior step to point at.
func TestCompile_PrevOnFirstStepRejected(t *testing.T) {
	src := `version: "1"
workflow:
  id: prev-only
  steps:
    - model: mock/x
      fixture: fx.json
      prompt: "echo {{prev}}"
`
	file, err := Parse([]byte(src), "workflow.yaml")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, err := Compile(file, t.TempDir()); err == nil {
		t.Fatal("expected error for {{prev}} on first step")
	}
}

// TestRun_EndToEnd drives a workflow end-to-end with the mock
// provider. This is the beginner path: parse → compile → run, with
// no strict-pipeline artifacts visible to the author.
func TestRun_EndToEnd(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "overlord.yaml"), simpleWorkflow)
	writeFile(t, filepath.Join(dir, "fixtures", "draft.json"), `{"text": "the draft"}`)
	writeFile(t, filepath.Join(dir, "fixtures", "review.json"), `{"text": "the review"}`)

	file, err := Load(filepath.Join(dir, "overlord.yaml"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := Run(ctx, file, dir, RunOptions{Input: "some source", Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result == nil || result.Task == nil {
		t.Fatal("nil result")
	}
	if string(result.Task.State) != "DONE" {
		t.Fatalf("state: got %q, want DONE", result.Task.State)
	}
	if !strings.Contains(result.Output, "the review") {
		t.Fatalf("output missing review payload: %q", result.Output)
	}
}

// TestIsWorkflowShape distinguishes workflow files from strict
// pipeline configs so `overlord run`/`serve`/`export` can route
// correctly.
func TestIsWorkflowShape(t *testing.T) {
	if !IsWorkflowShape([]byte("workflow:\n  id: x\n  steps: []\n")) {
		t.Fatal("should detect workflow")
	}
	if IsWorkflowShape([]byte("pipelines:\n  - name: x\n")) {
		t.Fatal("should not detect pipeline as workflow")
	}
	if IsWorkflowShape([]byte("not: yaml :::")) {
		t.Fatal("should reject malformed yaml")
	}
}

// TestApplyRuntime_Postgres verifies the runtime block plumbs through
// to the compiled strict config so `overlord serve` picks up the
// author's store choice. We use mock provider steps to avoid real
// credentials.
func TestApplyRuntime_Postgres(t *testing.T) {
	src := `version: "1"
workflow:
  id: rt
  steps:
    - model: mock/x
      fixture: fx.json
      prompt: "hi"
runtime:
  store:
    type: postgres
    dsn_env: DATABASE_URL
    table: overlord_tasks
  auth:
    enabled: true
    keys:
      - name: admin
        key_env: OVERLORD_ADMIN_KEY
        scopes: [admin]
`
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "fx.json"), `{"text": "fixture"}`)
	file, err := Parse([]byte(src), "workflow.yaml")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	c, err := Compile(file, dir)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if c.Config.Pipelines[0].Store != "postgres" {
		t.Fatalf("store: got %q, want postgres", c.Config.Pipelines[0].Store)
	}
	if c.Config.Stores.Postgres == nil || c.Config.Stores.Postgres.DSNEnv != "DATABASE_URL" {
		t.Fatalf("postgres config not applied: %+v", c.Config.Stores.Postgres)
	}
	if !c.Config.Auth.Enabled || len(c.Config.Auth.Keys) != 1 {
		t.Fatalf("auth not applied: %+v", c.Config.Auth)
	}
}

// TestExport verifies workflow export produces files a strict
// overlord project would consume. We don't re-test the full
// round-trip (chain_test.go covers that) — just that Export returns
// a non-empty pipeline + schema set.
func TestExport(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "fixtures", "draft.json"), `{"text": "d"}`)
	writeFile(t, filepath.Join(dir, "fixtures", "review.json"), `{"text": "r"}`)
	file, err := Parse([]byte(simpleWorkflow), "workflow.yaml")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	c, err := Compile(file, dir)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	files, err := Export(c)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if len(files.Pipeline) == 0 {
		t.Fatal("empty exported pipeline yaml")
	}
	if len(files.Schemas) == 0 {
		t.Fatal("empty schema export")
	}
	if !strings.Contains(string(files.Pipeline), "pipelines:") {
		t.Fatal("exported yaml missing pipelines block")
	}
}

// TestDefaultIDFromSource covers the tiny basename-to-id helper so
// edge cases (hidden files, empty base, etc.) never slip back in.
func TestDefaultIDFromSource(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"overlord.yaml", "overlord"},
		{"/tmp/foo/my-flow.yaml", "my-flow"},
		{"", "workflow"},
		{"/", "workflow"},
		{"_weird.yaml", "weird"},
	}
	for _, tc := range cases {
		if got := defaultIDFromSource(tc.in); got != tc.want {
			t.Errorf("defaultIDFromSource(%q): got %q, want %q", tc.in, got, tc.want)
		}
	}
}
