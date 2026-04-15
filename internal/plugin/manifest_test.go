package plugin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeManifest(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "manifest.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	return p
}

func TestLoadManifest_Valid(t *testing.T) {
	p := writeManifest(t, `
name: good
binary: ./bin/thing
rpc_timeout: 10s
shutdown_timeout: 2s
max_restarts: 3
on_failure: non_retryable
env:
  - PATH
`)
	m, err := LoadManifest(p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m.Name != "good" {
		t.Errorf("name: got %q", m.Name)
	}
	if m.RPCTimeout.Duration != 10*time.Second {
		t.Errorf("rpc_timeout: got %v", m.RPCTimeout.Duration)
	}
	if m.ShutdownTimeoutOrDefault() != 2*time.Second {
		t.Errorf("shutdown_timeout: got %v", m.ShutdownTimeoutOrDefault())
	}
	if m.OnFailureOrDefault() != OnFailureNonRetryable {
		t.Errorf("on_failure: got %q", m.OnFailureOrDefault())
	}
	if m.ManifestDir() == "" {
		t.Errorf("manifestDir should be set")
	}
	if got := m.ResolveBinary(); !filepath.IsAbs(got) {
		t.Errorf("resolve binary should be absolute when manifestDir is set: got %q", got)
	}
}

func TestLoadManifest_MissingName(t *testing.T) {
	p := writeManifest(t, "binary: ./x\n")
	_, err := LoadManifest(p)
	if err == nil {
		t.Fatal("expected error for missing name")
	}
	if !strings.Contains(err.Error(), "name is required") {
		t.Errorf("error: %v", err)
	}
}

func TestLoadManifest_NameWithPathSeparator(t *testing.T) {
	p := writeManifest(t, "name: evil/slash\nbinary: ./x\n")
	_, err := LoadManifest(p)
	if err == nil || !strings.Contains(err.Error(), "path separators") {
		t.Fatalf("expected path-separator error, got: %v", err)
	}
}

func TestLoadManifest_MissingBinary(t *testing.T) {
	p := writeManifest(t, "name: x\n")
	_, err := LoadManifest(p)
	if err == nil || !strings.Contains(err.Error(), "binary is required") {
		t.Fatalf("expected binary-required error, got: %v", err)
	}
}

func TestLoadManifest_BadOnFailure(t *testing.T) {
	p := writeManifest(t, "name: x\nbinary: ./y\non_failure: maybe\n")
	_, err := LoadManifest(p)
	if err == nil || !strings.Contains(err.Error(), "on_failure") {
		t.Fatalf("expected on_failure error, got: %v", err)
	}
}

func TestLoadManifest_NegativeMaxRestarts(t *testing.T) {
	p := writeManifest(t, "name: x\nbinary: ./y\nmax_restarts: -1\n")
	_, err := LoadManifest(p)
	if err == nil || !strings.Contains(err.Error(), "max_restarts") {
		t.Fatalf("expected max_restarts error, got: %v", err)
	}
}

func TestLoadManifest_BadDuration(t *testing.T) {
	p := writeManifest(t, "name: x\nbinary: ./y\nrpc_timeout: banana\n")
	_, err := LoadManifest(p)
	if err == nil || !strings.Contains(err.Error(), "invalid duration") {
		t.Fatalf("expected duration error, got: %v", err)
	}
}

func TestManifest_ResolveBinary_Absolute(t *testing.T) {
	m := &Manifest{Name: "x", Binary: "/usr/bin/true"}
	if got := m.ResolveBinary(); got != "/usr/bin/true" {
		t.Errorf("got %q", got)
	}
}

func TestManifest_OnFailureDefault(t *testing.T) {
	m := &Manifest{}
	if got := m.OnFailureOrDefault(); got != OnFailureRetryable {
		t.Errorf("default on_failure: got %q", got)
	}
}
