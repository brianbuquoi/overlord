package chain

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sampleYAML = `version: "1"
chain:
  id: sample
  input:
    type: text
  vars:
    greeting: hello
  steps:
    - id: draft
      model: mock/m1
      fixture: fixtures/draft.json
      prompt: |
        {{vars.greeting}} {{input}}
    - id: review
      model: mock/m2
      fixture: fixtures/review.json
      prompt: |
        review {{steps.draft.output}}
  output:
    from: steps.review.output
    type: text
`

func TestParse_OK(t *testing.T) {
	ch, err := Parse([]byte(sampleYAML), "<test>")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if ch.ID != "sample" {
		t.Errorf("id: got %q", ch.ID)
	}
	if len(ch.Steps) != 2 {
		t.Errorf("steps: got %d", len(ch.Steps))
	}
}

func TestParse_RejectsMissingVersion(t *testing.T) {
	_, err := Parse([]byte(`chain:
  id: x
  steps:
    - id: s
      model: mock/m
      fixture: f.json
      prompt: p
`), "<test>")
	if err == nil || !strings.Contains(err.Error(), "missing version") {
		t.Fatalf("want missing-version, got: %v", err)
	}
}

func TestParse_RejectsPipelineConfigSalted(t *testing.T) {
	_, err := Parse([]byte(`version: "1"
schema_registry:
  - name: a
    version: v1
    path: x.json
pipelines:
  - name: p
`), "<test>")
	if err == nil || !strings.Contains(err.Error(), "full pipeline config") {
		t.Fatalf("want pipeline-config hint, got: %v", err)
	}
}

func TestLoad_File(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "chain.yaml")
	if err := os.WriteFile(p, []byte(sampleYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	ch, err := Load(p)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if ch.ID != "sample" {
		t.Errorf("id: got %q", ch.ID)
	}
}

func TestLoad_RejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real.yaml")
	if err := os.WriteFile(real, []byte(sampleYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.yaml")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}
	_, err := Load(link)
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("want symlink rejection, got: %v", err)
	}
}
