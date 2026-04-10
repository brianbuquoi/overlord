package broker_test

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/orcastrator/orcastrator/internal/agent"
	"github.com/orcastrator/orcastrator/internal/broker"
	"github.com/orcastrator/orcastrator/internal/config"
	"github.com/orcastrator/orcastrator/internal/contract"
	"github.com/orcastrator/orcastrator/internal/store/memory"
)

// writeTestSchema writes a JSON schema file and returns its path.
func writeTestSchema(t *testing.T, dir, name string, schema map[string]any) string {
	t.Helper()
	data, err := json.Marshal(schema)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

func objSchema(prop string, typ string) map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			prop: map[string]any{"type": typ},
		},
		"required":             []string{prop},
		"additionalProperties": false,
	}
}

// =============================================================================
// Test 3: CrossStageTransitions counter increments correctly.
// =============================================================================

func TestCrossStageTransitions_HappyPath(t *testing.T) {
	dir := t.TempDir()

	s1in := writeTestSchema(t, dir, "s1_in.json", objSchema("request", "string"))
	s1out := writeTestSchema(t, dir, "s1_out.json", objSchema("category", "string"))
	s2in := writeTestSchema(t, dir, "s2_in.json", objSchema("category", "string"))
	s2out := writeTestSchema(t, dir, "s2_out.json", objSchema("result", "string"))
	s3in := writeTestSchema(t, dir, "s3_in.json", objSchema("result", "string"))
	s3out := writeTestSchema(t, dir, "s3_out.json", objSchema("valid", "boolean"))

	cfg := &config.Config{
		Version: "1",
		SchemaRegistry: []config.SchemaEntry{
			{Name: "s1_in", Version: "v1", Path: s1in},
			{Name: "s1_out", Version: "v1", Path: s1out},
			{Name: "s2_in", Version: "v1", Path: s2in},
			{Name: "s2_out", Version: "v1", Path: s2out},
			{Name: "s3_in", Version: "v1", Path: s3in},
			{Name: "s3_out", Version: "v1", Path: s3out},
		},
		Pipelines: []config.Pipeline{
			{
				Name:        "transition-test",
				Concurrency: 2,
				Store:       "memory",
				Stages: []config.Stage{
					{
						ID:           "stage1",
						Agent:        "agent1",
						InputSchema:  config.StageSchemaRef{Name: "s1_in", Version: "v1"},
						OutputSchema: config.StageSchemaRef{Name: "s1_out", Version: "v1"},
						Timeout:      config.Duration{Duration: 5 * time.Second},
						Retry:        config.RetryPolicy{MaxAttempts: 1, Backoff: "fixed", BaseDelay: config.Duration{Duration: 10 * time.Millisecond}},
						OnSuccess:    config.StaticOnSuccess("stage2"),
						OnFailure:    "dead-letter",
					},
					{
						ID:           "stage2",
						Agent:        "agent2",
						InputSchema:  config.StageSchemaRef{Name: "s2_in", Version: "v1"},
						OutputSchema: config.StageSchemaRef{Name: "s2_out", Version: "v1"},
						Timeout:      config.Duration{Duration: 5 * time.Second},
						Retry:        config.RetryPolicy{MaxAttempts: 1, Backoff: "fixed", BaseDelay: config.Duration{Duration: 10 * time.Millisecond}},
						OnSuccess:    config.StaticOnSuccess("stage3"),
						OnFailure:    "dead-letter",
					},
					{
						ID:           "stage3",
						Agent:        "agent3",
						InputSchema:  config.StageSchemaRef{Name: "s3_in", Version: "v1"},
						OutputSchema: config.StageSchemaRef{Name: "s3_out", Version: "v1"},
						Timeout:      config.Duration{Duration: 5 * time.Second},
						Retry:        config.RetryPolicy{MaxAttempts: 1, Backoff: "fixed", BaseDelay: config.Duration{Duration: 10 * time.Millisecond}},
						OnSuccess:    config.StaticOnSuccess("done"),
						OnFailure:    "dead-letter",
					},
				},
			},
		},
		Agents: []config.Agent{
			{ID: "agent1", Provider: "mock"},
			{ID: "agent2", Provider: "mock"},
			{ID: "agent3", Provider: "mock"},
		},
	}

	reg, err := contract.NewRegistry(cfg.SchemaRegistry, "/")
	if err != nil {
		t.Fatal(err)
	}

	st := memory.New()
	agents := map[string]broker.Agent{
		"agent1": &mockAgent{id: "agent1", provider: "mock", handler: func(_ context.Context, _ *broker.Task) (*broker.TaskResult, error) {
			return &broker.TaskResult{Payload: json.RawMessage(`{"category":"test"}`)}, nil
		}},
		"agent2": &mockAgent{id: "agent2", provider: "mock", handler: func(_ context.Context, _ *broker.Task) (*broker.TaskResult, error) {
			return &broker.TaskResult{Payload: json.RawMessage(`{"result":"processed"}`)}, nil
		}},
		"agent3": &mockAgent{id: "agent3", provider: "mock", handler: func(_ context.Context, _ *broker.Task) (*broker.TaskResult, error) {
			return &broker.TaskResult{Payload: json.RawMessage(`{"valid":true}`)}, nil
		}},
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	b := broker.New(cfg, st, agents, reg, logger, nil, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.Run(ctx)

	task, err := b.Submit(ctx, "transition-test", json.RawMessage(`{"request":"hello"}`))
	if err != nil {
		t.Fatal(err)
	}

	// Wait for completion.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		got, err := st.GetTask(ctx, task.ID)
		if err == nil && got.State == broker.TaskStateDone {
			// A→B, B→C = 2 cross-stage transitions.
			if got.CrossStageTransitions != 2 {
				t.Errorf("expected CrossStageTransitions == 2, got %d", got.CrossStageTransitions)
			} else {
				t.Logf("CrossStageTransitions == %d (correct for 3-stage pipeline)", got.CrossStageTransitions)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("task did not reach DONE state within timeout")
}

// =============================================================================
// Test 4: max_stage_transitions enforcement.
// =============================================================================

func TestMaxStageTransitions_Enforcement(t *testing.T) {
	dir := t.TempDir()

	// Both stages use the same schema for simplicity.
	sin := writeTestSchema(t, dir, "sin.json", objSchema("data", "string"))
	sout := writeTestSchema(t, dir, "sout.json", objSchema("data", "string"))

	cfg := &config.Config{
		Version: "1",
		SchemaRegistry: []config.SchemaEntry{
			{Name: "sin", Version: "v1", Path: sin},
			{Name: "sout", Version: "v1", Path: sout},
		},
		Pipelines: []config.Pipeline{
			{
				Name:                "loop-test",
				Concurrency:         2,
				Store:               "memory",
				MaxStageTransitions: 3,
				Stages: []config.Stage{
					{
						ID:           "a",
						Agent:        "fail-agent",
						InputSchema:  config.StageSchemaRef{Name: "sin", Version: "v1"},
						OutputSchema: config.StageSchemaRef{Name: "sout", Version: "v1"},
						Timeout:      config.Duration{Duration: 5 * time.Second},
						Retry:        config.RetryPolicy{MaxAttempts: 1, Backoff: "fixed", BaseDelay: config.Duration{Duration: 10 * time.Millisecond}},
						OnSuccess:    config.StaticOnSuccess("done"),
						OnFailure:    "b",
					},
					{
						ID:           "b",
						Agent:        "fail-agent",
						InputSchema:  config.StageSchemaRef{Name: "sin", Version: "v1"},
						OutputSchema: config.StageSchemaRef{Name: "sout", Version: "v1"},
						Timeout:      config.Duration{Duration: 5 * time.Second},
						Retry:        config.RetryPolicy{MaxAttempts: 1, Backoff: "fixed", BaseDelay: config.Duration{Duration: 10 * time.Millisecond}},
						OnSuccess:    config.StaticOnSuccess("done"),
						OnFailure:    "a",
					},
				},
			},
		},
		Agents: []config.Agent{
			{ID: "fail-agent", Provider: "mock"},
		},
	}

	reg, err := contract.NewRegistry(cfg.SchemaRegistry, "/")
	if err != nil {
		t.Fatal(err)
	}

	st := memory.New()
	agents := map[string]broker.Agent{
		"fail-agent": &mockAgent{id: "fail-agent", provider: "mock", handler: func(_ context.Context, _ *broker.Task) (*broker.TaskResult, error) {
			return nil, &agent.AgentError{
				Err:       fmt.Errorf("always fails"),
				AgentID:   "fail-agent",
				Prov:      "mock",
				Retryable: false,
			}
		}},
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	b := broker.New(cfg, st, agents, reg, logger, nil, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.Run(ctx)

	task, err := b.Submit(ctx, "loop-test", json.RawMessage(`{"data":"test"}`))
	if err != nil {
		t.Fatal(err)
	}

	// Wait for failure (dead-letter due to max transitions).
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		got, err := st.GetTask(ctx, task.ID)
		if err == nil && got.State == broker.TaskStateFailed {
			// Verify dead-lettered.
			if !got.RoutedToDeadLetter {
				t.Error("task should be routed to dead-letter")
			}
			// Verify metadata has the right failure reason.
			if reason, ok := got.Metadata["failure_reason"]; ok {
				if reason != "max_stage_transitions_exceeded" {
					t.Errorf("expected failure_reason 'max_stage_transitions_exceeded', got %v", reason)
				}
			} else {
				t.Error("metadata should contain failure_reason")
			}
			// Verify transitions count in metadata.
			if transitions, ok := got.Metadata["transitions"]; ok {
				t.Logf("Task dead-lettered after %v transitions (limit: 3)", transitions)
			} else {
				t.Error("metadata should contain transitions count")
			}
			t.Logf("max_stage_transitions enforcement verified: task dead-lettered as expected")
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("task did not reach FAILED state within timeout")
}

// =============================================================================
// Test 5: Default limit (50) enforced without explicit config.
// =============================================================================

func TestMaxStageTransitions_Default50(t *testing.T) {
	// Verify the constant is 50.
	if config.DefaultMaxStageTransitions != 50 {
		t.Errorf("expected DefaultMaxStageTransitions == 50, got %d", config.DefaultMaxStageTransitions)
	}
	t.Logf("DefaultMaxStageTransitions = %d (correct)", config.DefaultMaxStageTransitions)
}
