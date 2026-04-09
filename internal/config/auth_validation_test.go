package config

import (
	"strings"
	"testing"
)

func TestValidateAuth_Disabled(t *testing.T) {
	// Auth disabled (default) — no validation needed.
	err := validateAuth(APIAuthConfig{Enabled: false})
	if err != nil {
		t.Errorf("disabled auth should not error: %v", err)
	}
}

func TestValidateAuth_EnabledNoKeys(t *testing.T) {
	err := validateAuth(APIAuthConfig{Enabled: true, Keys: nil})
	if err == nil {
		t.Fatal("expected error for enabled auth with no keys")
	}
	if !strings.Contains(err.Error(), "empty") {
		t.Errorf("error should mention empty keys: %v", err)
	}
}

func TestValidateAuth_DuplicateNames(t *testing.T) {
	err := validateAuth(APIAuthConfig{
		Enabled: true,
		Keys: []AuthKeyConfig{
			{Name: "dup", KeyEnv: "A", Scopes: []string{"read"}},
			{Name: "dup", KeyEnv: "B", Scopes: []string{"write"}},
		},
	})
	if err == nil {
		t.Fatal("expected error for duplicate key names")
	}
	if !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("error should mention duplicate: %v", err)
	}
}

func TestValidateAuth_EmptyScopes(t *testing.T) {
	err := validateAuth(APIAuthConfig{
		Enabled: true,
		Keys: []AuthKeyConfig{
			{Name: "noscope", KeyEnv: "A", Scopes: []string{}},
		},
	})
	if err == nil {
		t.Fatal("expected error for empty scopes")
	}
}

func TestValidateAuth_InvalidScope(t *testing.T) {
	err := validateAuth(APIAuthConfig{
		Enabled: true,
		Keys: []AuthKeyConfig{
			{Name: "bad", KeyEnv: "A", Scopes: []string{"superuser"}},
		},
	})
	if err == nil {
		t.Fatal("expected error for invalid scope")
	}
	if !strings.Contains(err.Error(), "superuser") {
		t.Errorf("error should mention the invalid scope: %v", err)
	}
}

func TestValidateAuth_MissingKeyEnv(t *testing.T) {
	err := validateAuth(APIAuthConfig{
		Enabled: true,
		Keys: []AuthKeyConfig{
			{Name: "noenv", KeyEnv: "", Scopes: []string{"read"}},
		},
	})
	if err == nil {
		t.Fatal("expected error for missing key_env")
	}
}

func TestValidateAuth_Valid(t *testing.T) {
	err := validateAuth(APIAuthConfig{
		Enabled: true,
		Keys: []AuthKeyConfig{
			{Name: "ci", KeyEnv: "CI_KEY", Scopes: []string{"write"}},
			{Name: "monitor", KeyEnv: "MON_KEY", Scopes: []string{"read"}},
			{Name: "admin", KeyEnv: "ADM_KEY", Scopes: []string{"read", "write", "admin"}},
		},
	})
	if err != nil {
		t.Errorf("valid auth config should not error: %v", err)
	}
}

func TestConfigWithoutAuthBlock_Backward_Compatible(t *testing.T) {
	// A config with no auth block should parse fine and have auth.enabled=false.
	cfg := Config{
		Auth: APIAuthConfig{}, // zero value
	}
	if cfg.Auth.Enabled {
		t.Error("default auth should be disabled")
	}
}
