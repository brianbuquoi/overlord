//go:build live

package code_review_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/orcastrator/orcastrator/internal/agent/registry"
	"github.com/orcastrator/orcastrator/internal/broker"
	"github.com/orcastrator/orcastrator/internal/config"
	"github.com/orcastrator/orcastrator/internal/contract"
	"github.com/orcastrator/orcastrator/internal/store/memory"
)

func TestLive_CodeReviewPipeline(t *testing.T) {
	if os.Getenv("ANTHROPIC_API_KEY") == "" {
		t.Skip("ANTHROPIC_API_KEY not set — skipping live test")
	}

	// Resolve paths relative to repo root.
	repoRoot := filepath.Join("..", "..")
	configPath := filepath.Join(repoRoot, "config", "examples", "code_review.yaml")
	payloadPath := filepath.Join(repoRoot, "examples", "code_review", "sample_input.json")

	// Load config.
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	// Build contract registry — schemas are resolved relative to the config file.
	basePath := filepath.Dir(configPath)
	reg, err := contract.NewRegistry(cfg.SchemaRegistry, basePath)
	if err != nil {
		t.Fatalf("contract registry: %v", err)
	}

	// Build real agents from config.
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	agents := make(map[string]broker.Agent)
	for _, ac := range cfg.Agents {
		a, err := registry.NewFromConfig(ac, logger)
		if err != nil {
			t.Fatalf("create agent %q: %v", ac.ID, err)
		}
		agents[ac.ID] = a.(broker.Agent)
	}

	// Build broker with memory store.
	st := memory.New()
	b := broker.New(cfg, st, agents, reg, logger, nil, nil)

	// Load sample payload.
	payloadBytes, err := os.ReadFile(payloadPath)
	if err != nil {
		t.Fatalf("read payload: %v", err)
	}

	// Run the pipeline.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	go b.Run(ctx)

	task, err := b.Submit(ctx, "code-review", json.RawMessage(payloadBytes))
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	t.Logf("submitted task %s", task.ID)

	// Poll until terminal state.
	var finalTask *broker.Task
	deadline := time.After(3 * time.Minute)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-deadline:
			t.Fatalf("timeout waiting for task to complete; last state: %+v", finalTask)
		case <-ticker.C:
			finalTask, err = b.GetTask(ctx, task.ID)
			if err != nil {
				continue
			}
			if finalTask.State == broker.TaskStateDone {
				goto done
			}
			if finalTask.State == broker.TaskStateFailed {
				t.Fatalf("task failed: %+v", finalTask)
			}
		}
	}
done:
	cancel()

	t.Logf("final payload: %s", string(finalTask.Payload))

	// Parse the summarize stage output.
	var summary struct {
		ExecutiveSummary   string   `json:"executive_summary"`
		CriticalCount      int      `json:"critical_count"`
		HighCount          int      `json:"high_count"`
		ActionRequired     bool     `json:"action_required"`
		TopRecommendations []string `json:"top_recommendations"`
	}
	if err := json.Unmarshal(finalTask.Payload, &summary); err != nil {
		t.Fatalf("unmarshal summary output: %v\npayload: %s", err, string(finalTask.Payload))
	}

	t.Logf("executive_summary: %s", summary.ExecutiveSummary)
	t.Logf("critical_count=%d high_count=%d action_required=%v",
		summary.CriticalCount, summary.HighCount, summary.ActionRequired)
	t.Logf("top_recommendations: %v", summary.TopRecommendations)

	// The planted SQL injection and hardcoded credential should produce at least
	// one critical or high finding.
	if summary.CriticalCount+summary.HighCount == 0 {
		t.Errorf("expected at least one critical or high finding for planted security issues, got critical=%d high=%d",
			summary.CriticalCount, summary.HighCount)
	}

	if !summary.ActionRequired {
		t.Errorf("expected action_required=true given security findings")
	}

	// Now check the review stage output by looking at the task chain.
	// The final task's payload is from summarize. We need to verify that
	// the review stage produced "request_changes". We can check this
	// via the executive summary or top_recommendations mentioning the issues.
	// But for a stronger assertion, let's check the intermediate task.
	//
	// Since the broker chains tasks, the summarize input IS the review output.
	// We verify indirectly: if critical+high > 0 and action_required is true,
	// the review stage must have produced request_changes (the summarize prompt
	// only sets action_required=true when critical/high findings exist, which
	// only happen when overall_assessment is request_changes).

	// Additional assertion: the summary should mention SQL injection or credentials.
	summaryLower := strings.ToLower(summary.ExecutiveSummary)
	recsLower := strings.ToLower(strings.Join(summary.TopRecommendations, " "))
	combined := summaryLower + " " + recsLower

	hasSQLMention := strings.Contains(combined, "sql") || strings.Contains(combined, "injection") || strings.Contains(combined, "query")
	hasCredMention := strings.Contains(combined, "credential") || strings.Contains(combined, "hardcod") || strings.Contains(combined, "secret") || strings.Contains(combined, "token")

	if !hasSQLMention && !hasCredMention {
		t.Errorf("expected summary to mention SQL injection or hardcoded credentials, got:\n  summary: %s\n  recommendations: %v",
			summary.ExecutiveSummary, summary.TopRecommendations)
	}
}

// TestLive_CodeReviewPipeline_CleanDiff submits a clean diff (no security
// issues) and verifies the pipeline handles the positive case.
func TestLive_CodeReviewPipeline_CleanDiff(t *testing.T) {
	if os.Getenv("ANTHROPIC_API_KEY") == "" {
		t.Skip("ANTHROPIC_API_KEY not set — skipping live test")
	}

	repoRoot := filepath.Join("..", "..")
	configPath := filepath.Join(repoRoot, "config", "examples", "code_review.yaml")
	payloadPath := filepath.Join(repoRoot, "examples", "code_review", "clean_input.json")

	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	basePath := filepath.Dir(configPath)
	reg, err := contract.NewRegistry(cfg.SchemaRegistry, basePath)
	if err != nil {
		t.Fatalf("contract registry: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	agents := make(map[string]broker.Agent)
	for _, ac := range cfg.Agents {
		a, err := registry.NewFromConfig(ac, logger)
		if err != nil {
			t.Fatalf("create agent %q: %v", ac.ID, err)
		}
		agents[ac.ID] = a.(broker.Agent)
	}

	st := memory.New()
	b := broker.New(cfg, st, agents, reg, logger, nil, nil)

	payloadBytes, err := os.ReadFile(payloadPath)
	if err != nil {
		t.Fatalf("read payload: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	go b.Run(ctx)

	task, err := b.Submit(ctx, "code-review", json.RawMessage(payloadBytes))
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	t.Logf("submitted task %s", task.ID)

	var finalTask *broker.Task
	deadline := time.After(3 * time.Minute)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-deadline:
			t.Fatalf("timeout waiting for task to complete; last state: %+v", finalTask)
		case <-ticker.C:
			finalTask, err = b.GetTask(ctx, task.ID)
			if err != nil {
				continue
			}
			if finalTask.State == broker.TaskStateDone {
				goto done
			}
			if finalTask.State == broker.TaskStateFailed {
				t.Fatalf("task failed: %+v", finalTask)
			}
		}
	}
done:
	cancel()

	t.Logf("final payload: %s", string(finalTask.Payload))

	var summary struct {
		ExecutiveSummary   string   `json:"executive_summary"`
		CriticalCount      int      `json:"critical_count"`
		HighCount          int      `json:"high_count"`
		ActionRequired     bool     `json:"action_required"`
		TopRecommendations []string `json:"top_recommendations"`
	}
	if err := json.Unmarshal(finalTask.Payload, &summary); err != nil {
		t.Fatalf("unmarshal summary output: %v\npayload: %s", err, string(finalTask.Payload))
	}

	t.Logf("executive_summary: %s", summary.ExecutiveSummary)
	t.Logf("critical_count=%d high_count=%d action_required=%v",
		summary.CriticalCount, summary.HighCount, summary.ActionRequired)

	// Clean diff should have no critical or high findings.
	if summary.CriticalCount+summary.HighCount > 0 {
		t.Errorf("expected no critical/high findings for clean diff, got critical=%d high=%d",
			summary.CriticalCount, summary.HighCount)
	}

	// overall assessment should be approve or comment, not request_changes.
	// We check action_required is false as a proxy.
	if summary.ActionRequired {
		t.Errorf("expected action_required=false for clean diff")
	}

	if summary.ExecutiveSummary == "" {
		t.Error("executive_summary should not be empty even for clean diffs")
	}
}
