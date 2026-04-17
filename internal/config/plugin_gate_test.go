package config

import (
	"strings"
	"testing"
)

// TestValidatePluginGate_RefusesPluginAgentWhenDisabled verifies the
// fail-closed opt-in: an agent with provider: plugin must be rejected
// unless plugins.enabled is explicitly true. Without this check, a
// malicious or careless YAML author can coerce Overlord into exec'ing
// an arbitrary binary — see audit finding SEC4-NEW-001.
func TestValidatePluginGate_RefusesPluginAgentWhenDisabled(t *testing.T) {
	err := validatePluginGate(
		PluginConfig{}, // Enabled defaults to false
		[]Agent{{ID: "ext", Provider: "plugin"}},
	)
	if err == nil {
		t.Fatal("expected refusal when plugins.enabled is false; got nil")
	}
	if !strings.Contains(err.Error(), "plugins are disabled") {
		t.Errorf("error must say plugins are disabled; got: %v", err)
	}
	if !strings.Contains(err.Error(), "ext") {
		t.Errorf("error must name the offending agent id; got: %v", err)
	}
	if !strings.Contains(err.Error(), "plugins.enabled: true") {
		t.Errorf("error must tell the operator how to opt in; got: %v", err)
	}
}

func TestValidatePluginGate_RefusesSOInputsWhenDisabled(t *testing.T) {
	cases := []struct {
		name string
		pcfg PluginConfig
	}{
		{name: "dir_set", pcfg: PluginConfig{Dir: "/opt/plugins"}},
		{name: "files_set", pcfg: PluginConfig{Files: []string{"/opt/plugins/echo.so"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validatePluginGate(tc.pcfg, nil)
			if err == nil {
				t.Fatal("expected refusal when .so inputs set but plugins disabled")
			}
			if !strings.Contains(err.Error(), "plugins.enabled: true") {
				t.Errorf("error must tell operator how to opt in; got: %v", err)
			}
		})
	}
}

func TestValidatePluginGate_AcceptsWhenEnabled(t *testing.T) {
	err := validatePluginGate(
		PluginConfig{Enabled: true},
		[]Agent{{ID: "ext", Provider: "plugin"}},
	)
	if err != nil {
		t.Fatalf("expected acceptance when plugins.enabled: true; got %v", err)
	}
}

func TestValidatePluginGate_AcceptsNoPlugins(t *testing.T) {
	err := validatePluginGate(
		PluginConfig{},
		[]Agent{{ID: "ext", Provider: "anthropic"}},
	)
	if err != nil {
		t.Fatalf("expected acceptance when no plugins referenced; got %v", err)
	}
}

// TestValidatePluginGate_ListsAllOffendingAgents verifies the error
// names every plugin-provider agent id, not just the first one, so an
// operator fixing the config in one pass sees everything to address.
func TestValidatePluginGate_ListsAllOffendingAgents(t *testing.T) {
	err := validatePluginGate(
		PluginConfig{},
		[]Agent{
			{ID: "plug-b", Provider: "plugin"},
			{ID: "plug-a", Provider: "plugin"},
			{ID: "anth", Provider: "anthropic"},
		},
	)
	if err == nil {
		t.Fatal("expected refusal")
	}
	if !strings.Contains(err.Error(), "plug-a") || !strings.Contains(err.Error(), "plug-b") {
		t.Errorf("error must list every plugin agent id; got: %v", err)
	}
	// Sorted: plug-a appears before plug-b.
	if idxA, idxB := strings.Index(err.Error(), "plug-a"), strings.Index(err.Error(), "plug-b"); idxA > idxB {
		t.Errorf("agent ids must be sorted for deterministic operator output; got: %v", err)
	}
}

// TestPluginConfig_PathAllowed codifies the allowlist semantics that
// PathAllowed encodes for both the .so loader and the subprocess
// loader. Kept here next to the gate validator so the two halves of
// the policy stay legible as a unit.
func TestPluginConfig_PathAllowed(t *testing.T) {
	cases := []struct {
		name   string
		cfg    PluginConfig
		path   string
		expect bool
	}{
		{"empty_allowlist_permits", PluginConfig{}, "/any/path", true},
		{"match_permits", PluginConfig{Allow: []string{"/opt/p/bin"}}, "/opt/p/bin", true},
		{"non_match_refuses", PluginConfig{Allow: []string{"/opt/p/bin"}}, "/bin/sh", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.PathAllowed(tc.path); got != tc.expect {
				t.Errorf("PathAllowed(%q) = %v, want %v", tc.path, got, tc.expect)
			}
		})
	}
}
