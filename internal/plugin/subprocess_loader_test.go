package plugin

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brianbuquoi/overlord/internal/config"
)

func writeLoaderManifest(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "manifest.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	return p
}

// enabledPolicy returns a plugin policy with the opt-in gate flipped on so
// these tests exercise the post-gate validation path (manifest parsing,
// binary existence, allowlist) rather than re-asserting the gate itself.
// The gate has dedicated coverage in TestLoadAndCreate_Gate*.
func enabledPolicy() config.PluginConfig {
	return config.PluginConfig{Enabled: true}
}

func TestLoadAndCreate_MissingBinary(t *testing.T) {
	mp := writeLoaderManifest(t, "name: x\nbinary: ./does_not_exist\n")
	_, err := LoadAndCreate(config.Agent{
		ID:           "missing-bin",
		Provider:     "plugin",
		ManifestPath: mp,
	}, enabledPolicy(), slog.Default())
	if err == nil {
		t.Fatal("expected eager validation error, got nil")
	}
	var pve *PluginValidationError
	if !errors.As(err, &pve) {
		t.Fatalf("expected *PluginValidationError, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected 'not found' in error, got: %v", err)
	}
}

func TestLoadAndCreate_NonExecutableBinary(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "binary")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	mp := filepath.Join(dir, "manifest.yaml")
	if err := os.WriteFile(mp, []byte("name: x\nbinary: ./binary\n"), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	_, err := LoadAndCreate(config.Agent{
		ID:           "non-exec",
		Provider:     "plugin",
		ManifestPath: mp,
	}, enabledPolicy(), slog.Default())
	if err == nil {
		t.Fatal("expected eager validation error")
	}
	var pve *PluginValidationError
	if !errors.As(err, &pve) {
		t.Fatalf("expected *PluginValidationError, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "not executable") {
		t.Errorf("expected 'not executable' in error, got: %v", err)
	}
}

func TestLoadAndCreate_EmptyManifestReturnsValidationError(t *testing.T) {
	_, err := LoadAndCreate(config.Agent{
		ID:       "nopath",
		Provider: "plugin",
	}, enabledPolicy(), slog.Default())
	if err == nil {
		t.Fatal("expected error for empty manifest path")
	}
	var pve *PluginValidationError
	if !errors.As(err, &pve) {
		t.Fatalf("expected *PluginValidationError, got %T: %v", err, err)
	}
}

// TestLoadAndCreate_GateDisabledRefuses verifies the fail-closed opt-in
// gate: without plugins.enabled: true in the policy, LoadAndCreate must
// refuse before touching the manifest or binary. This is the second half
// of the defense — config.Validate is the first half — so that bypassing
// Validate does not silently downgrade to the pre-gate behavior.
func TestLoadAndCreate_GateDisabledRefuses(t *testing.T) {
	// Use a real manifest+binary so the only thing that should fail is
	// the gate itself.
	dir := t.TempDir()
	bin := filepath.Join(dir, "binary")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho x\n"), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}
	mp := filepath.Join(dir, "manifest.yaml")
	if err := os.WriteFile(mp, []byte("name: x\nbinary: ./binary\n"), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	_, err := LoadAndCreate(config.Agent{
		ID:           "gated",
		Provider:     "plugin",
		ManifestPath: mp,
	}, config.PluginConfig{Enabled: false}, slog.Default())
	if err == nil {
		t.Fatal("expected gate refusal, got nil error")
	}
	var pve *PluginValidationError
	if !errors.As(err, &pve) {
		t.Fatalf("expected *PluginValidationError, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "plugins are disabled") {
		t.Errorf("expected 'plugins are disabled' in error, got: %v", err)
	}
}

// TestLoadAndCreate_AllowlistRejectsBinaryNotInList verifies that when
// plugins.allow is non-empty, a resolved binary path that is not in the
// allowlist is rejected even though plugins.enabled is true.
func TestLoadAndCreate_AllowlistRejectsBinaryNotInList(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "binary")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho x\n"), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}
	mp := filepath.Join(dir, "manifest.yaml")
	if err := os.WriteFile(mp, []byte("name: x\nbinary: ./binary\n"), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	_, err := LoadAndCreate(config.Agent{
		ID:           "denied",
		Provider:     "plugin",
		ManifestPath: mp,
	}, config.PluginConfig{
		Enabled: true,
		Allow:   []string{"/some/other/path"},
	}, slog.Default())
	if err == nil {
		t.Fatal("expected allowlist refusal, got nil error")
	}
	var pve *PluginValidationError
	if !errors.As(err, &pve) {
		t.Fatalf("expected *PluginValidationError, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "plugins.allow") {
		t.Errorf("expected 'plugins.allow' in error, got: %v", err)
	}
}

// TestLoadAndCreate_AllowlistPermitsListedBinary verifies that when the
// resolved binary path matches an entry in plugins.allow, construction
// succeeds.
func TestLoadAndCreate_AllowlistPermitsListedBinary(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "binary")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho x\n"), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}
	mp := filepath.Join(dir, "manifest.yaml")
	if err := os.WriteFile(mp, []byte("name: x\nbinary: ./binary\n"), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	_, err := LoadAndCreate(config.Agent{
		ID:           "allowed",
		Provider:     "plugin",
		ManifestPath: mp,
	}, config.PluginConfig{
		Enabled: true,
		Allow:   []string{filepath.Join(dir, "binary")},
	}, slog.Default())
	if err != nil {
		t.Fatalf("expected allowlisted binary to be accepted, got: %v", err)
	}
}
