package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestChainCmd_ScaffoldInspectRun drives the chain command group
// end-to-end via cobra's in-process runner: scaffold the embedded
// template, inspect the compiled pipeline, and run it. No network,
// no external LLMs — the scaffolded project uses the mock provider.
func TestChainCmd_ScaffoldInspectRun(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "wr")

	// 1. chain init
	cmd := rootCmd()
	var stderr bytes.Buffer
	cmd.SetOut(os.Stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"chain", "init", "write-review", target})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("chain init: %v\n%s", err, stderr.String())
	}
	if _, err := os.Stat(filepath.Join(target, "chain.yaml")); err != nil {
		t.Fatalf("scaffold missing chain.yaml: %v", err)
	}

	// 2. chain inspect
	cmd = rootCmd()
	var inspectOut bytes.Buffer
	cmd.SetOut(&inspectOut)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"chain", "inspect", "--chain", filepath.Join(target, "chain.yaml")})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("chain inspect: %v", err)
	}
	if !strings.Contains(inspectOut.String(), "Compiles to pipeline \"write-review\"") {
		t.Fatalf("inspect output missing pipeline header:\n%s", inspectOut.String())
	}

	// 3. chain run
	cmd = rootCmd()
	var runOut bytes.Buffer
	cmd.SetOut(&runOut)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{
		"chain", "run",
		"--chain", filepath.Join(target, "chain.yaml"),
		"--input", "seed",
		"--quiet",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("chain run: %v", err)
	}
	if !strings.Contains(runOut.String(), "positioning chain mode") {
		t.Fatalf("run output missing review phrase:\n%s", runOut.String())
	}

	// 4. chain export
	outDir := filepath.Join(dir, "export")
	cmd = rootCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{
		"chain", "export",
		"--chain", filepath.Join(target, "chain.yaml"),
		"--out", outDir,
	})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("chain export: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outDir, "overlord.yaml")); err != nil {
		t.Fatalf("export missing overlord.yaml: %v", err)
	}
	// The exported dir should itself validate via the pipeline-mode
	// validate command. This verifies chain mode never emits a config
	// the strict runtime would reject.
	cmd = rootCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"validate", "--config", filepath.Join(outDir, "overlord.yaml")})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("overlord validate on export: %v", err)
	}
}

// TestChainCmd_RunRejectsPipelineConfigMistake catches the common
// mistake of pointing `chain run` at a full pipeline config.
func TestChainCmd_RunRejectsPipelineConfigMistake(t *testing.T) {
	// Use an existing pipeline example, which is definitively not a
	// chain file.
	path := filepath.Join("..", "..", "config", "examples", "basic.yaml")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("example config not present: %v", err)
	}
	cmd := rootCmd()
	var stderr bytes.Buffer
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"chain", "run", "--chain", path, "--input", "x"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when pointing chain run at a pipeline config")
	}
	if !strings.Contains(err.Error(), "full pipeline config") {
		t.Fatalf("expected pipeline-config hint in error: %v", err)
	}
}
