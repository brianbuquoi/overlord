package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// =============================================================================
// Section 7.1 — All CLI commands with --help
// Every subcommand produces help output that exits 0 and contains a description.
// =============================================================================

func TestCLI_AllSubcommands_HelpExits0(t *testing.T) {
	root := rootCmd()

	// Collect all subcommands recursively.
	var allCmds []*cobra.Command
	var collect func(cmd *cobra.Command)
	collect = func(cmd *cobra.Command) {
		for _, sub := range cmd.Commands() {
			allCmds = append(allCmds, sub)
			collect(sub)
		}
	}
	collect(root)

	if len(allCmds) == 0 {
		t.Fatal("expected at least one subcommand")
	}

	for _, cmd := range allCmds {
		t.Run(cmd.CommandPath(), func(t *testing.T) {
			r := rootCmd()
			var stdout bytes.Buffer
			r.SetOut(&stdout)
			r.SetErr(&stdout)

			args := strings.Fields(cmd.CommandPath())
			// Remove the root command name.
			if len(args) > 0 {
				args = args[1:]
			}
			args = append(args, "--help")
			r.SetArgs(args)

			err := r.Execute()
			if err != nil {
				t.Fatalf("--help should exit 0, got error: %v", err)
			}

			output := stdout.String()
			if output == "" {
				t.Fatal("--help produced no output")
			}

			// Every command should have at least a short description.
			if !strings.Contains(output, "Usage:") {
				t.Error("help output should contain 'Usage:' section")
			}
		})
	}
}

// =============================================================================
// Section 7.4 — File-accepting flags (@file syntax)
// =============================================================================

func TestCLI_Submit_FilePayload_Directory(t *testing.T) {
	configPath := writeTestYAML(t)
	dir := t.TempDir()

	root := rootCmd()
	root.SetArgs([]string{"submit", "--config", configPath, "--pipeline", "test-pipeline",
		"--payload", "@" + dir})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected error when payload file is a directory")
	}
}

func TestCLI_Submit_FilePayload_Empty(t *testing.T) {
	configPath := writeTestYAML(t)
	dir := t.TempDir()
	emptyFile := filepath.Join(dir, "empty.json")
	os.WriteFile(emptyFile, []byte(""), 0o644)

	root := rootCmd()
	root.SetArgs([]string{"submit", "--config", configPath, "--pipeline", "test-pipeline",
		"--payload", "@" + emptyFile})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected error when payload file is empty")
	}
}

func TestCLI_Submit_FilePayload_ValidJSON(t *testing.T) {
	configPath := writeTestYAML(t)
	dir := t.TempDir()
	payloadFile := filepath.Join(dir, "valid.json")
	os.WriteFile(payloadFile, []byte(`{"request":"from file"}`), 0o644)

	root := rootCmd()
	root.SetArgs([]string{"submit", "--config", configPath, "--pipeline", "test-pipeline",
		"--payload", "@" + payloadFile})
	err := root.Execute()
	if err != nil {
		t.Fatalf("valid file payload should succeed: %v", err)
	}
}

// =============================================================================
// Section 7.5 — Environment variable precedence
// =============================================================================

func TestCLI_EnvVarPrecedence_Port(t *testing.T) {
	// envOrDefault returns env var value if set, otherwise the default.
	t.Run("env_set", func(t *testing.T) {
		t.Setenv("ORCASTRATOR_PORT", "9090")
		got := envOrDefault("ORCASTRATOR_PORT", "8080")
		if got != "9090" {
			t.Fatalf("expected 9090, got %s", got)
		}
	})

	t.Run("env_not_set", func(t *testing.T) {
		// Unset the variable (Setenv with empty string doesn't unset).
		os.Unsetenv("ORCASTRATOR_PORT")
		got := envOrDefault("ORCASTRATOR_PORT", "8080")
		if got != "8080" {
			t.Fatalf("expected default 8080, got %s", got)
		}
	})

	t.Run("empty_env_returns_default", func(t *testing.T) {
		t.Setenv("ORCASTRATOR_PORT", "")
		got := envOrDefault("ORCASTRATOR_PORT", "8080")
		if got != "8080" {
			t.Fatalf("expected default 8080 for empty env var, got %s", got)
		}
	})
}

func TestCLI_EnvVarPrecedence_APIKey(t *testing.T) {
	t.Run("flag_wins_over_env", func(t *testing.T) {
		t.Setenv("ORCASTRATOR_API_KEY", "env-key-123")

		root := rootCmd()
		root.SetArgs([]string{"--api-key", "flag-key-456", "health", "--help"})

		// Find the flag value after parsing.
		root.Execute()
		flagVal, _ := root.Flags().GetString("api-key")
		if flagVal != "flag-key-456" {
			t.Fatalf("flag should win over env, got %s", flagVal)
		}
	})

	t.Run("env_used_when_no_flag", func(t *testing.T) {
		t.Setenv("ORCASTRATOR_API_KEY", "env-key-789")

		// resolveAPIKey reads flag first, then env var.
		root := rootCmd()
		root.SetArgs([]string{"health", "--help"})
		root.Execute()
		key := resolveAPIKey(root)
		if key != "env-key-789" {
			t.Fatalf("expected env key, got %s", key)
		}
	})
}

// =============================================================================
// Section 7.6 — Validate with each example config
// =============================================================================

func TestCLI_Validate_ExampleConfigs(t *testing.T) {
	examples, err := filepath.Glob("../../config/examples/*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(examples) == 0 {
		// Try from project root.
		examples, _ = filepath.Glob("config/examples/*.yaml")
	}
	if len(examples) == 0 {
		t.Skip("no example configs found — run from project root or cmd/orcastrator")
	}

	for _, example := range examples {
		t.Run(filepath.Base(example), func(t *testing.T) {
			root := rootCmd()
			var stdout bytes.Buffer
			root.SetOut(&stdout)
			root.SetArgs([]string{"validate", "--config", example})
			err := root.Execute()
			if err != nil {
				t.Fatalf("example config %s failed validation: %v", example, err)
			}
		})
	}
}

// =============================================================================
// Section 7.1 supplement — Specific commands have descriptions
// =============================================================================

func TestCLI_CommandDescriptions(t *testing.T) {
	root := rootCmd()
	cmds := make(map[string]*cobra.Command)
	for _, cmd := range root.Commands() {
		cmds[cmd.Name()] = cmd
	}

	required := []string{"run", "submit", "status", "validate", "health", "cancel", "pipelines", "dead-letter", "migrate", "completion"}
	for _, name := range required {
		t.Run(name, func(t *testing.T) {
			cmd, ok := cmds[name]
			if !ok {
				t.Fatalf("missing subcommand: %s", name)
			}
			if cmd.Short == "" {
				t.Errorf("subcommand %s has no Short description", name)
			}
		})
	}
}

// =============================================================================
// Section 7 supplement — Migrate subcommands exist
// =============================================================================

func TestCLI_MigrateSubcommands(t *testing.T) {
	root := rootCmd()
	migrateCmd, _, _ := root.Find([]string{"migrate"})
	if migrateCmd == nil {
		t.Fatal("migrate command not found")
	}

	subs := make(map[string]bool)
	for _, cmd := range migrateCmd.Commands() {
		subs[cmd.Name()] = true
	}

	for _, name := range []string{"list", "run", "validate"} {
		if !subs[name] {
			t.Errorf("migrate subcommand %q not found", name)
		}
	}
}

// =============================================================================
// Section 7 supplement — Dead-letter subcommands exist
// =============================================================================

func TestCLI_DeadLetterSubcommands(t *testing.T) {
	root := rootCmd()
	dlCmd, _, _ := root.Find([]string{"dead-letter"})
	if dlCmd == nil {
		t.Fatal("dead-letter command not found")
	}

	subs := make(map[string]bool)
	for _, cmd := range dlCmd.Commands() {
		subs[cmd.Name()] = true
	}

	for _, name := range []string{"list", "replay", "replay-all", "discard", "discard-all"} {
		if !subs[name] {
			t.Errorf("dead-letter subcommand %q not found", name)
		}
	}
}

// =============================================================================
// Section 7 supplement — configBasePath edge cases
// =============================================================================

func TestConfigBasePath(t *testing.T) {
	tests := []struct {
		input string
		want  string // just check it's non-empty and absolute
	}{
		{"config.yaml", ""}, // relative path
		{"/tmp/config.yaml", "/tmp"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := configBasePath(tt.input)
			if got == "" {
				t.Fatal("configBasePath should return non-empty path")
			}
			if tt.want != "" && got != tt.want {
				t.Fatalf("expected %s, got %s", tt.want, got)
			}
		})
	}
}

// =============================================================================
// Section 7 supplement — fmtConfigError wrapping
// =============================================================================

func TestFmtConfigError(t *testing.T) {
	err := fmtConfigError("/path/to/config.yaml", os.ErrNotExist)
	if err == nil {
		t.Fatal("expected non-nil error")
	}
	errStr := err.Error()
	if !strings.Contains(errStr, "Hint:") {
		t.Errorf("expected Hint in error message, got: %s", errStr)
	}
}
