package chain

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// mkChainFile writes a chain YAML into dir along with the given
// fixture files (keyed by relative path). Returns the chain config
// path and the directory. Used by the export tests below to
// exercise the end-to-end Load → Compile → Export flow without
// re-implementing scaffold plumbing.
func mkChainFile(t *testing.T, dir, chainYAML string, fixtures map[string]string) string {
	t.Helper()
	for rel, body := range fixtures {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatalf("write fixture: %v", err)
		}
	}
	chainPath := filepath.Join(dir, "chain.yaml")
	if err := os.WriteFile(chainPath, []byte(chainYAML), 0o644); err != nil {
		t.Fatalf("write chain: %v", err)
	}
	return chainPath
}

// TestExport_LowersAdjacentPlaceholders verifies the happy path:
// a two-step chain with `{{input}}` on step 1 and
// `{{steps.<step1>.output}}` on step 2 exports successfully, and
// the resulting pipeline YAML contains no `{{...}}` placeholders.
// A strict-mode adapter would therefore send a stable prompt to
// the LLM — the workflow runtime's substitution behavior is
// preserved via the broker's envelope wrapper (which already
// delivers the prior stage's output to the downstream model).
func TestExport_LowersAdjacentPlaceholders(t *testing.T) {
	dir := t.TempDir()
	chainYAML := `version: "1"
chain:
  id: lowerable
  input:
    type: text
  steps:
    - id: draft
      model: mock/d
      fixture: fixtures/d.json
      prompt: |
        Write a draft for:
        {{input}}
    - id: review
      model: mock/r
      fixture: fixtures/r.json
      prompt: |
        Review this draft:
        {{steps.draft.output}}
`
	chainPath := mkChainFile(t, dir, chainYAML, map[string]string{
		"fixtures/d.json": `{"text":"drafted"}`,
		"fixtures/r.json": `{"text":"reviewed"}`,
	})

	ch, err := Load(chainPath)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	compiled, err := CompileWithBase(ch, filepath.Dir(chainPath))
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	files, err := Export(compiled)
	if err != nil {
		t.Fatalf("export: %v", err)
	}

	// The exported pipeline YAML must not contain any `{{...}}`
	// placeholder — real-provider adapters would ship those
	// verbatim to the LLM.
	if m := regexp.MustCompile(`\{\{[^}]+\}\}`).FindStringSubmatch(string(files.Pipeline)); len(m) > 0 {
		t.Fatalf("exported pipeline YAML still carries a placeholder %q:\n%s", m[0], files.Pipeline)
	}
	// Sanity: some remnant of the original prompts must survive
	// so graduating authors can see where their text went.
	if !strings.Contains(string(files.Pipeline), "Write a draft for:") {
		t.Errorf("export dropped the stage-1 prompt text: %s", files.Pipeline)
	}
	if !strings.Contains(string(files.Pipeline), "Review this draft:") {
		t.Errorf("export dropped the stage-2 prompt text: %s", files.Pipeline)
	}
	// And the narration for the adjacent-step placeholder must
	// survive so the prompt flows naturally.
	if !strings.Contains(string(files.Pipeline), "prior stage output") {
		t.Errorf("export missing narration for adjacent-step placeholder: %s", files.Pipeline)
	}

	// Critical: the in-memory live config must still carry the
	// `{{steps.draft.output}}` placeholder — the chain step
	// adapter's runtime substitution depends on it. Lowering is
	// export-only, not a global mutation.
	liveReview := ""
	for _, a := range compiled.Config.Agents {
		if a.ID == "review" {
			liveReview = a.SystemPrompt
		}
	}
	if !strings.Contains(liveReview, "{{steps.draft.output}}") {
		t.Errorf("lowering leaked into the live config: step 'review' lost its runtime placeholder:\n%s", liveReview)
	}
}

// TestExport_RejectsNonAdjacentStepReference covers the escape-
// hatch: a 3-step chain where step 3 references step 1 (skipping
// the intermediate step) cannot be lowered, because the strict
// runtime's envelope only carries the immediately preceding
// stage's output. Export must stop with an ExportLoweringError
// naming the offending step and placeholder.
func TestExport_RejectsNonAdjacentStepReference(t *testing.T) {
	dir := t.TempDir()
	chainYAML := `version: "1"
chain:
  id: non-adjacent
  input:
    type: text
  steps:
    - id: a
      model: mock/a
      fixture: fixtures/a.json
      prompt: "start with {{input}}"
    - id: b
      model: mock/b
      fixture: fixtures/b.json
      prompt: "continue with {{steps.a.output}}"
    - id: c
      model: mock/c
      fixture: fixtures/c.json
      prompt: "finish; also reference {{steps.a.output}}"
`
	chainPath := mkChainFile(t, dir, chainYAML, map[string]string{
		"fixtures/a.json": `{"text":"a"}`,
		"fixtures/b.json": `{"text":"b"}`,
		"fixtures/c.json": `{"text":"c"}`,
	})

	ch, err := Load(chainPath)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	compiled, err := CompileWithBase(ch, filepath.Dir(chainPath))
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	_, err = Export(compiled)
	if err == nil {
		t.Fatal("expected Export to reject a non-adjacent step reference")
	}
	var lerr *ExportLoweringError
	if !errors.As(err, &lerr) {
		t.Fatalf("expected *ExportLoweringError; got %T: %v", err, err)
	}
	if lerr.StepID != "c" {
		t.Errorf("error names wrong step: got %q, want %q", lerr.StepID, "c")
	}
	if !strings.Contains(lerr.Placeholder, "steps.a.output") {
		t.Errorf("error should name the offending placeholder; got %q", lerr.Placeholder)
	}
	if !strings.Contains(lerr.Reason, "not the immediately preceding step") {
		t.Errorf("error reason should mention non-adjacent step; got %q", lerr.Reason)
	}
}

// TestExport_RejectsInputOnNonFirstStep covers the second escape-
// hatch case: `{{input}}` is only safe to lower on step 1 (where
// the strict runtime ships the payload as user content). A
// non-first reference needs per-task metadata the strict runtime
// does not carry.
func TestExport_RejectsInputOnNonFirstStep(t *testing.T) {
	dir := t.TempDir()
	chainYAML := `version: "1"
chain:
  id: input-late
  input:
    type: text
  steps:
    - id: first
      model: mock/f
      fixture: fixtures/f.json
      prompt: "draft for {{input}}"
    - id: second
      model: mock/s
      fixture: fixtures/s.json
      prompt: "review draft; original request was {{input}}"
`
	chainPath := mkChainFile(t, dir, chainYAML, map[string]string{
		"fixtures/f.json": `{"text":"f"}`,
		"fixtures/s.json": `{"text":"s"}`,
	})
	ch, err := Load(chainPath)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	compiled, err := CompileWithBase(ch, filepath.Dir(chainPath))
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	_, err = Export(compiled)
	var lerr *ExportLoweringError
	if !errors.As(err, &lerr) {
		t.Fatalf("expected *ExportLoweringError; got %T: %v", err, err)
	}
	if lerr.StepID != "second" {
		t.Errorf("error names wrong step: got %q, want %q", lerr.StepID, "second")
	}
	if !strings.Contains(lerr.Placeholder, "input") {
		t.Errorf("error should name the offending placeholder; got %q", lerr.Placeholder)
	}
	if !strings.Contains(lerr.Reason, "first step") {
		t.Errorf("error reason should mention first-step restriction; got %q", lerr.Reason)
	}
}

// TestExport_DoesNotMutateLiveConfig is a property-style check
// that running Export never changes compiled.Config.Agents —
// lowering is pure w.r.t. the caller's in-memory state. Important
// because the same *Compiled record is held by an active
// in-process broker while `chain export --stdout` runs.
// TestExport_NamedStepsHaveNoAutoIDComment confirms that when the
// source chain/workflow gives every step an explicit id, the exported
// YAML header does not carry the auto-generated-ID rename hint — it
// would be noise for authors who already picked good names.
func TestExport_NamedStepsHaveNoAutoIDComment(t *testing.T) {
	dir := t.TempDir()
	chainYAML := `version: "1"
chain:
  id: named
  input:
    type: text
  steps:
    - id: draft
      model: mock/d
      fixture: fixtures/d.json
      prompt: "draft {{input}}"
    - id: review
      model: mock/r
      fixture: fixtures/r.json
      prompt: "review {{steps.draft.output}}"
`
	chainPath := mkChainFile(t, dir, chainYAML, map[string]string{
		"fixtures/d.json": `{"text":"drafted"}`,
		"fixtures/r.json": `{"text":"reviewed"}`,
	})
	ch, err := Load(chainPath)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	compiled, err := CompileWithBase(ch, filepath.Dir(chainPath))
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	files, err := Export(compiled)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if strings.Contains(string(files.Pipeline), "auto-generated by the workflow compiler") {
		t.Fatalf("named steps must not trigger the auto-ID rename hint; got:\n%s", files.Pipeline)
	}
}

// TestExport_AutoGeneratedStepIDsEmitRenameHint covers the SEC-audit
// graduation-cliff finding: when the compiler had to auto-generate
// `step_N` identifiers (source workflow omitted the `id:` field),
// the exported YAML's header must call that out so the first
// advanced edit is a rename rather than fan-out over `step_2`.
func TestExport_AutoGeneratedStepIDsEmitRenameHint(t *testing.T) {
	// Authored as a workflow (not a chain) so the workflow compiler's
	// auto-ID path actually runs — the chain schema requires explicit
	// step IDs.
	// We construct the compiled result by running the workflow → chain
	// lowering in-process to avoid pulling in the workflow package.
	ch := &Chain{
		ID:    "unnamed",
		Input: &Input{Type: "text"},
		Steps: []Step{
			{ID: "step_1", Model: "mock/draft", Prompt: "write {{input}}", Fixture: "fixtures/d.json"},
			{ID: "step_2", Model: "mock/review", Prompt: "review {{steps.step_1.output}}", Fixture: "fixtures/r.json"},
		},
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "fixtures/d.json"), []byte(`{"text":"d"}`), 0o644); err != nil {
		if err := os.MkdirAll(filepath.Join(dir, "fixtures"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "fixtures/d.json"), []byte(`{"text":"d"}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "fixtures/r.json"), []byte(`{"text":"r"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	compiled, err := CompileWithBase(ch, dir)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	files, err := Export(compiled)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	body := string(files.Pipeline)
	if !strings.Contains(body, "auto-generated by the workflow compiler") {
		t.Fatalf("exported header missing auto-ID rename hint; got:\n%s", body)
	}
	if !strings.Contains(body, "`step_1`") || !strings.Contains(body, "`step_2`") {
		t.Errorf("exported header should enumerate each auto-generated ID; got:\n%s", body)
	}
}

func TestExport_DoesNotMutateLiveConfig(t *testing.T) {
	dir := t.TempDir()
	chainYAML := `version: "1"
chain:
  id: purity
  input:
    type: text
  steps:
    - id: only
      model: mock/x
      fixture: fixtures/x.json
      prompt: "process {{input}}"
`
	chainPath := mkChainFile(t, dir, chainYAML, map[string]string{
		"fixtures/x.json": `{"text":"ok"}`,
	})
	ch, err := Load(chainPath)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	compiled, err := CompileWithBase(ch, filepath.Dir(chainPath))
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	beforePrompts := make(map[string]string, len(compiled.Config.Agents))
	for _, a := range compiled.Config.Agents {
		beforePrompts[a.ID] = a.SystemPrompt
	}

	if _, err := Export(compiled); err != nil {
		t.Fatalf("export: %v", err)
	}

	for _, a := range compiled.Config.Agents {
		if beforePrompts[a.ID] != a.SystemPrompt {
			t.Errorf("Export mutated live config: agent %q prompt changed from %q to %q",
				a.ID, beforePrompts[a.ID], a.SystemPrompt)
		}
	}
}
