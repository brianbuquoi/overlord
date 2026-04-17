package chain

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brianbuquoi/overlord/internal/config"
)

// configLoad is a thin test-only alias over config.Load. Kept as a
// helper so the export round-trip test reads clearly.
func configLoad(path string) (*config.Config, error) { return config.Load(path) }

// TestRun_EndToEndWithMock scaffolds the write-review template into a
// temp dir, loads the chain, and runs it. This exercises the whole
// chain → compile → broker path: loader, validator, compile layer,
// wrapper adapter, memory store, and broker stage routing.
func TestRun_EndToEndWithMock(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "write-review")
	if _, err := Scaffold("write-review", target, ScaffoldOptions{}); err != nil {
		t.Fatalf("scaffold: %v", err)
	}

	ch, err := Load(filepath.Join(target, "chain.yaml"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	result, err := Run(ctx, ch, target, RunOptions{
		Input:   "some initial source material",
		Timeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.Task == nil {
		t.Fatal("nil task")
	}
	if got := string(result.Task.State); got != "DONE" {
		t.Fatalf("state: got %q, want DONE", got)
	}
	// The scaffolded review fixture contains "positioning chain mode";
	// we assert the final output carries it through so template refs
	// and metadata flow both worked.
	if !strings.Contains(result.Output, "positioning chain mode") {
		t.Fatalf("final output missing expected phrase: %q", result.Output)
	}

	// Chain metadata should be persisted on the task, with both steps
	// captured — that's how forward references would resolve if a
	// third step existed.
	raw, ok := result.Task.Metadata[ChainMetaKey]
	if !ok {
		t.Fatalf("expected task metadata to contain %q", ChainMetaKey)
	}
	b, _ := json.Marshal(raw)
	var cm chainMeta
	if err := json.Unmarshal(b, &cm); err != nil {
		t.Fatalf("decode chain meta: %v", err)
	}
	if cm.Input != "some initial source material" {
		t.Errorf("chain meta input: got %q", cm.Input)
	}
	if _, ok := cm.Outputs["draft"]; !ok {
		t.Errorf("chain meta outputs missing draft: %v", cm.Outputs)
	}
	if _, ok := cm.Outputs["review"]; !ok {
		t.Errorf("chain meta outputs missing review: %v", cm.Outputs)
	}
}

// TestBuildInitialPayload_TextWrapsAsJSON ensures text-mode chains
// wire their raw input through as {"text": "..."} so the synthesized
// chain_text schema accepts it.
func TestBuildInitialPayload_TextWrapsAsJSON(t *testing.T) {
	ch := &Chain{ID: "x", Input: &Input{Type: "text"}, Steps: []Step{{ID: "s", Model: "mock/m", Fixture: "f.json", Prompt: "p"}}}
	payload, err := BuildInitialPayload(ch, "hello world")
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]string
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded["text"] != "hello world" {
		t.Errorf("payload: got %q", decoded["text"])
	}
}

func TestBuildInitialPayload_JSONRequiresObject(t *testing.T) {
	ch := &Chain{ID: "x", Input: &Input{Type: "json"}, Steps: []Step{{ID: "s", Model: "mock/m", Fixture: "f.json", Prompt: "p"}}}
	if _, err := BuildInitialPayload(ch, "[1,2,3]"); err == nil {
		t.Fatal("expected error for non-object json")
	}
	if _, err := BuildInitialPayload(ch, `{"a": 1}`); err != nil {
		t.Fatalf("valid object rejected: %v", err)
	}
}

// TestExport_RoundTrip confirms that the exported pipeline YAML is
// loadable by the config package and covers the same schemas the
// compiled chain used.
func TestExport_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "write-review")
	if _, err := Scaffold("write-review", target, ScaffoldOptions{}); err != nil {
		t.Fatalf("scaffold: %v", err)
	}
	ch, err := Load(filepath.Join(target, "chain.yaml"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	compiled, err := CompileWithBase(ch, target)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	files, err := Export(compiled)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	outDir := filepath.Join(dir, "export")
	if err := files.WriteTo(outDir); err != nil {
		t.Fatalf("write export: %v", err)
	}
	// Sanity: overlord.yaml and schemas are present.
	if _, err := os.Stat(filepath.Join(outDir, "overlord.yaml")); err != nil {
		t.Fatalf("overlord.yaml: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outDir, "schemas", "chain_text_v1.json")); err != nil {
		t.Fatalf("chain_text_v1 schema: %v", err)
	}
	// Fixtures must round-trip.
	if _, err := os.Stat(filepath.Join(outDir, "fixtures", "draft.json")); err != nil {
		t.Fatalf("fixtures/draft.json: %v", err)
	}
}

// TestExport_LoadsViaConfigPackage asserts that the exported YAML is
// accepted by the config loader (which applies full pipeline-mode
// validation) — catching any drift between what the compiler emits
// and what the strict runtime accepts.
func TestExport_LoadsViaConfigPackage(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "write-review")
	if _, err := Scaffold("write-review", target, ScaffoldOptions{}); err != nil {
		t.Fatalf("scaffold: %v", err)
	}
	ch, err := Load(filepath.Join(target, "chain.yaml"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	compiled, err := CompileWithBase(ch, target)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	files, err := Export(compiled)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	outDir := filepath.Join(dir, "export")
	if err := files.WriteTo(outDir); err != nil {
		t.Fatalf("write export: %v", err)
	}
	cfg, err := configLoad(filepath.Join(outDir, "overlord.yaml"))
	if err != nil {
		t.Fatalf("config.Load on export: %v", err)
	}
	if len(cfg.Pipelines) != 1 || cfg.Pipelines[0].Name != "write-review" {
		t.Fatalf("unexpected exported config: %+v", cfg)
	}
}
