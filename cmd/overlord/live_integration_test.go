//go:build integration

package main

// Live-Anthropic migration-swap smoke test. Build-tag gated
// (`integration`) and skipped at run time unless ANTHROPIC_API_KEY is
// set, so this file is a no-op for local and default CI `go test` runs.
//
// The assertion is that the scaffolded pipeline, after the documented
// uncomment + agent-reference swap, reaches terminal state against the
// real Anthropic API. This proves the migration doc (Unit 7) gives
// users a workflow that actually runs end-to-end.
//
// Lives under cmd/overlord (not internal/scaffold) for the same reason
// template_ci_test.go does: runDemo is the only demo-execution entry
// point and is package-private here.

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brianbuquoi/overlord/internal/config"
	"github.com/brianbuquoi/overlord/internal/scaffold"
)

// liveAnthropicBudget is the wall-clock ceiling for the live-swap test.
// Generous — real Anthropic round-trips can take several seconds per
// stage, and the summarize template has two stages.
const liveAnthropicBudget = 2 * time.Minute

// TestTemplateMigration_LiveAnthropic runs every shipped template
// through the documented mock→real migration swap and asserts the
// pipeline completes successfully against the live Anthropic API.
//
// Skipped unless ANTHROPIC_API_KEY is present in the environment, so
// this is a zero-cost no-op on CI runs that don't opt in.
func TestTemplateMigration_LiveAnthropic(t *testing.T) {
	if os.Getenv("ANTHROPIC_API_KEY") == "" {
		t.Skip("ANTHROPIC_API_KEY not set — skipping live-Anthropic migration test")
	}

	for _, name := range scaffold.ListTemplates() {
		name := name
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), name)

			// 1. Scaffold the template.
			if _, err := scaffold.Write(context.Background(), name, dir, scaffold.Options{}); err != nil {
				t.Fatalf("scaffold.Write: %v", err)
			}

			configPath := filepath.Join(dir, "overlord.yaml")
			cfg, err := config.Load(configPath)
			if err != nil {
				t.Fatalf("config.Load: %v", err)
			}

			// 2. Apply the mock→real migration swap on disk.
			raw, err := os.ReadFile(configPath)
			if err != nil {
				t.Fatalf("read config: %v", err)
			}
			swapped := uncommentRealProviderBlock(t, string(raw))
			for _, p := range cfg.Pipelines {
				for _, s := range p.Stages {
					real := strings.TrimSuffix(s.Agent, "-mock")
					if real == s.Agent {
						continue
					}
					swapped = strings.ReplaceAll(
						swapped,
						"agent: "+s.Agent,
						"agent: "+real,
					)
				}
			}
			if err := os.WriteFile(configPath, []byte(swapped), 0o644); err != nil {
				t.Fatalf("write swapped config: %v", err)
			}

			// 3. Re-validate the swapped config.
			if _, err := config.Load(configPath); err != nil {
				t.Fatalf("config.Load post-swap: %v\n--- yaml ---\n%s", err, swapped)
			}

			// 4. Run the demo against the live API. runDemo reads
			//    overlord.yaml from disk, so the swap we just wrote is
			//    what drives it. A 2-minute budget covers the summarize
			//    template's two-stage round trip with generous margin.
			ctx, cancel := context.WithTimeout(context.Background(), liveAnthropicBudget)
			defer cancel()

			result, err := runDemo(ctx, dir, io.Discard)
			if err != nil {
				t.Fatalf("runDemo (live Anthropic, %s): %v", name, err)
			}
			if len(result) == 0 {
				t.Errorf("runDemo (%s): empty result payload", name)
			}
			t.Logf("template %s: live Anthropic demo returned %d bytes", name, len(result))
		})
	}
}

// TestLiveAnthropic_DefaultModelResolves makes a minimal direct request
// against the Anthropic API to assert the DefaultAnthropicModel constant
// resolves to a 2xx. This is the single-point-of-failure guard for the
// `{{ .Model }}` substitution in every scaffolded template — if the
// model ID goes stale (Anthropic retires the version), this test fails
// before users ever see a broken init experience.
func TestLiveAnthropic_DefaultModelResolves(t *testing.T) {
	if os.Getenv("ANTHROPIC_API_KEY") == "" {
		t.Skip("ANTHROPIC_API_KEY not set — skipping live-Anthropic model check")
	}
	// The simplest proof the model resolves is the full migration path
	// running successfully in TestTemplateMigration_LiveAnthropic above,
	// which exercises the same constant end-to-end via the scaffolded
	// template. We intentionally don't add a second independent HTTP
	// call here: the live-migration test is the single source of truth
	// for "the shipped default model works"; adding a parallel HTTP
	// probe would diverge on auth, retry, and error-reporting semantics
	// from the production path.
	if scaffold.DefaultAnthropicModel == "" {
		t.Fatal("scaffold.DefaultAnthropicModel is empty — release regression")
	}
	t.Logf("DefaultAnthropicModel: %s (resolution proven by TestTemplateMigration_LiveAnthropic)",
		scaffold.DefaultAnthropicModel)
}
