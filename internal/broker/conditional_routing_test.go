package broker_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/orcastrator/orcastrator/internal/broker"
	"github.com/orcastrator/orcastrator/internal/config"
	"github.com/orcastrator/orcastrator/internal/contract"
	"github.com/orcastrator/orcastrator/internal/routing"
	"github.com/orcastrator/orcastrator/internal/store/memory"
)

// ============================================================================
// 9. Code review pipeline with conditional routing
// ============================================================================

func buildConditionalPipeline(t *testing.T) (
	*config.Config,
	*memory.MemoryStore,
	map[string]broker.Agent,
	*contract.Registry,
) {
	t.Helper()
	dir := t.TempDir()

	// All stages accept a flexible JSON object.
	anySchema := func(name string) string {
		return writeSchema(t, dir, name, map[string]any{
			"type":                 "object",
			"additionalProperties": true,
		})
	}

	s1in := anySchema("s1_in.json")
	s1out := anySchema("s1_out.json")
	judgeIn := anySchema("judge_in.json")
	judgeOut := anySchema("judge_out.json")
	escIn := anySchema("esc_in.json")
	escOut := anySchema("esc_out.json")
	implIn := anySchema("impl_in.json")
	implOut := anySchema("impl_out.json")

	// Parse condition expressions for the judge stage.
	approveCond, _ := routing.Parse(`output.assessment == "approve"`)
	criticalCond, _ := routing.Parse(`output.critical_count > 0`)
	requestCond, _ := routing.Parse(`output.assessment == "request_changes"`)

	cfg := &config.Config{
		Version: "1",
		SchemaRegistry: []config.SchemaEntry{
			{Name: "s1_in", Version: "v1", Path: s1in},
			{Name: "s1_out", Version: "v1", Path: s1out},
			{Name: "judge_in", Version: "v1", Path: judgeIn},
			{Name: "judge_out", Version: "v1", Path: judgeOut},
			{Name: "esc_in", Version: "v1", Path: escIn},
			{Name: "esc_out", Version: "v1", Path: escOut},
			{Name: "impl_in", Version: "v1", Path: implIn},
			{Name: "impl_out", Version: "v1", Path: implOut},
		},
		Pipelines: []config.Pipeline{{
			Name:        "review",
			Concurrency: 2,
			Store:       "memory",
			Stages: []config.Stage{
				{
					ID:           "review",
					Agent:        "reviewer",
					InputSchema:  config.StageSchemaRef{Name: "s1_in", Version: "v1"},
					OutputSchema: config.StageSchemaRef{Name: "s1_out", Version: "v1"},
					Timeout:      config.Duration{Duration: 5 * time.Second},
					Retry:        config.RetryPolicy{MaxAttempts: 1, Backoff: "fixed", BaseDelay: config.Duration{Duration: 10 * time.Millisecond}},
					OnSuccess:    config.StaticOnSuccess("judge"),
					OnFailure:    "dead-letter",
				},
				{
					ID:           "judge",
					Agent:        "judge-agent",
					InputSchema:  config.StageSchemaRef{Name: "judge_in", Version: "v1"},
					OutputSchema: config.StageSchemaRef{Name: "judge_out", Version: "v1"},
					Timeout:      config.Duration{Duration: 5 * time.Second},
					Retry:        config.RetryPolicy{MaxAttempts: 1, Backoff: "fixed", BaseDelay: config.Duration{Duration: 10 * time.Millisecond}},
					OnSuccess: config.OnSuccessConfig{
						RouteConfig: routing.RouteConfig{
							IsConditional: true,
							Routes: []routing.ConditionalRoute{
								{Condition: approveCond, Stage: "done", RawExpr: approveCond.Raw},
								{Condition: criticalCond, Stage: "escalate", RawExpr: criticalCond.Raw},
								{Condition: requestCond, Stage: "implement", RawExpr: requestCond.Raw},
							},
							Default: "done",
						},
					},
					OnFailure: "dead-letter",
				},
				{
					ID:           "escalate",
					Agent:        "escalate-agent",
					InputSchema:  config.StageSchemaRef{Name: "esc_in", Version: "v1"},
					OutputSchema: config.StageSchemaRef{Name: "esc_out", Version: "v1"},
					Timeout:      config.Duration{Duration: 5 * time.Second},
					Retry:        config.RetryPolicy{MaxAttempts: 1, Backoff: "fixed", BaseDelay: config.Duration{Duration: 10 * time.Millisecond}},
					OnSuccess:    config.StaticOnSuccess("done"),
					OnFailure:    "dead-letter",
				},
				{
					ID:           "implement",
					Agent:        "implement-agent",
					InputSchema:  config.StageSchemaRef{Name: "impl_in", Version: "v1"},
					OutputSchema: config.StageSchemaRef{Name: "impl_out", Version: "v1"},
					Timeout:      config.Duration{Duration: 5 * time.Second},
					Retry:        config.RetryPolicy{MaxAttempts: 1, Backoff: "fixed", BaseDelay: config.Duration{Duration: 10 * time.Millisecond}},
					OnSuccess:    config.StaticOnSuccess("done"),
					OnFailure:    "dead-letter",
				},
			},
		}},
		Agents: []config.Agent{
			{ID: "reviewer", Provider: "mock", SystemPrompt: "review"},
			{ID: "judge-agent", Provider: "mock", SystemPrompt: "judge"},
			{ID: "escalate-agent", Provider: "mock", SystemPrompt: "escalate"},
			{ID: "implement-agent", Provider: "mock", SystemPrompt: "implement"},
		},
	}

	reg, err := contract.NewRegistry(cfg.SchemaRegistry, "/")
	if err != nil {
		t.Fatal(err)
	}

	st := memory.New()
	agents := map[string]broker.Agent{
		"reviewer":        &mockAgent{id: "reviewer", provider: "mock"},
		"judge-agent":     &mockAgent{id: "judge-agent", provider: "mock"},
		"escalate-agent":  &mockAgent{id: "escalate-agent", provider: "mock"},
		"implement-agent": &mockAgent{id: "implement-agent", provider: "mock"},
	}

	return cfg, st, agents, reg
}

func TestConditionalRouting_ApproveRoutesToDone(t *testing.T) {
	cfg, st, agents, reg := buildConditionalPipeline(t)

	agents["reviewer"].(*mockAgent).setHandler(func(_ context.Context, _ *broker.Task) (*broker.TaskResult, error) {
		return &broker.TaskResult{Payload: json.RawMessage(`{"status":"reviewed"}`)}, nil
	})
	agents["judge-agent"].(*mockAgent).setHandler(func(_ context.Context, _ *broker.Task) (*broker.TaskResult, error) {
		return &broker.TaskResult{Payload: json.RawMessage(`{"assessment":"approve","critical_count":0,"summary":"ok"}`)}, nil
	})

	b := newConditionalBroker(cfg, st, agents, reg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.Run(ctx)

	task, err := b.Submit(ctx, "review", json.RawMessage(`{"code":"clean"}`))
	if err != nil {
		t.Fatal(err)
	}

	final := waitForState(t, st, task.ID, broker.TaskStateDone, 5*time.Second)
	if final.State != broker.TaskStateDone {
		t.Fatalf("expected DONE, got %s", final.State)
	}
	// Should NOT have gone through escalate or implement.
	history := final.Metadata["stage_history"]
	if h, ok := history.([]interface{}); ok {
		for _, s := range h {
			if s == "escalate" || s == "implement" {
				t.Errorf("task should not have visited %s stage", s)
			}
		}
	}
}

func TestConditionalRouting_CriticalCountRoutesToEscalate(t *testing.T) {
	cfg, st, agents, reg := buildConditionalPipeline(t)

	agents["reviewer"].(*mockAgent).setHandler(func(_ context.Context, _ *broker.Task) (*broker.TaskResult, error) {
		return &broker.TaskResult{Payload: json.RawMessage(`{"status":"reviewed"}`)}, nil
	})
	agents["judge-agent"].(*mockAgent).setHandler(func(_ context.Context, _ *broker.Task) (*broker.TaskResult, error) {
		return &broker.TaskResult{Payload: json.RawMessage(`{"assessment":"request_changes","critical_count":2,"summary":"critical"}`)}, nil
	})
	agents["escalate-agent"].(*mockAgent).setHandler(func(_ context.Context, _ *broker.Task) (*broker.TaskResult, error) {
		return &broker.TaskResult{Payload: json.RawMessage(`{"action":"escalated"}`)}, nil
	})

	b := newConditionalBroker(cfg, st, agents, reg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.Run(ctx)

	task, _ := b.Submit(ctx, "review", json.RawMessage(`{"code":"bad"}`))
	final := waitForState(t, st, task.ID, broker.TaskStateDone, 5*time.Second)
	if final.State != broker.TaskStateDone {
		t.Fatalf("expected DONE, got %s", final.State)
	}

	// Verify it went through escalate (stage_history should contain escalate).
	var payload map[string]interface{}
	json.Unmarshal(final.Payload, &payload)
	if payload["action"] != "escalated" {
		t.Errorf("expected escalate output, got: %s", string(final.Payload))
	}
}

func TestConditionalRouting_RequestChangesNoCritical_RoutesToImplement(t *testing.T) {
	cfg, st, agents, reg := buildConditionalPipeline(t)

	agents["reviewer"].(*mockAgent).setHandler(func(_ context.Context, _ *broker.Task) (*broker.TaskResult, error) {
		return &broker.TaskResult{Payload: json.RawMessage(`{"status":"reviewed"}`)}, nil
	})
	agents["judge-agent"].(*mockAgent).setHandler(func(_ context.Context, _ *broker.Task) (*broker.TaskResult, error) {
		// critical_count is 0, so "output.critical_count > 0" won't match.
		// But "output.assessment == request_changes" will.
		return &broker.TaskResult{Payload: json.RawMessage(`{"assessment":"request_changes","critical_count":0,"summary":"minor"}`)}, nil
	})
	agents["implement-agent"].(*mockAgent).setHandler(func(_ context.Context, _ *broker.Task) (*broker.TaskResult, error) {
		return &broker.TaskResult{Payload: json.RawMessage(`{"status":"implemented"}`)}, nil
	})

	b := newConditionalBroker(cfg, st, agents, reg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.Run(ctx)

	task, _ := b.Submit(ctx, "review", json.RawMessage(`{"code":"ok"}`))
	final := waitForState(t, st, task.ID, broker.TaskStateDone, 5*time.Second)

	var payload map[string]interface{}
	json.Unmarshal(final.Payload, &payload)
	if payload["status"] != "implemented" {
		t.Errorf("expected implement output, got: %s", string(final.Payload))
	}
}

func TestConditionalRouting_UnrecognizedAssessment_RoutesToDefault(t *testing.T) {
	cfg, st, agents, reg := buildConditionalPipeline(t)

	agents["reviewer"].(*mockAgent).setHandler(func(_ context.Context, _ *broker.Task) (*broker.TaskResult, error) {
		return &broker.TaskResult{Payload: json.RawMessage(`{"status":"reviewed"}`)}, nil
	})
	agents["judge-agent"].(*mockAgent).setHandler(func(_ context.Context, _ *broker.Task) (*broker.TaskResult, error) {
		// Unrecognized assessment value → no condition matches → default ("done").
		return &broker.TaskResult{Payload: json.RawMessage(`{"assessment":"unknown_value","critical_count":0,"summary":"?"}`)}, nil
	})

	b := newConditionalBroker(cfg, st, agents, reg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.Run(ctx)

	task, _ := b.Submit(ctx, "review", json.RawMessage(`{"code":"x"}`))
	final := waitForState(t, st, task.ID, broker.TaskStateDone, 5*time.Second)
	if final.State != broker.TaskStateDone {
		t.Fatalf("expected DONE (default), got %s", final.State)
	}
}

// ============================================================================
// 11. Condition evaluation error at runtime → routes to on_failure
// ============================================================================

func TestConditionalRouting_EvalError_RoutesToFailure(t *testing.T) {
	dir := t.TempDir()
	anySchema := func(name string) string {
		return writeSchema(t, dir, name, map[string]any{
			"type":                 "object",
			"additionalProperties": true,
		})
	}
	s1in := anySchema("s1_in.json")
	s1out := anySchema("s1_out.json")

	// Condition: output.items contains "urgent"
	// At runtime, output.items is an integer → EvalError.
	containsCond, _ := routing.Parse(`output.items contains "urgent"`)

	cfg := &config.Config{
		Version: "1",
		SchemaRegistry: []config.SchemaEntry{
			{Name: "s_in", Version: "v1", Path: s1in},
			{Name: "s_out", Version: "v1", Path: s1out},
		},
		Pipelines: []config.Pipeline{{
			Name:        "eval-error",
			Concurrency: 1,
			Store:       "memory",
			Stages: []config.Stage{{
				ID:           "stage1",
				Agent:        "agent1",
				InputSchema:  config.StageSchemaRef{Name: "s_in", Version: "v1"},
				OutputSchema: config.StageSchemaRef{Name: "s_out", Version: "v1"},
				Timeout:      config.Duration{Duration: 5 * time.Second},
				Retry:        config.RetryPolicy{MaxAttempts: 1, Backoff: "fixed", BaseDelay: config.Duration{Duration: 10 * time.Millisecond}},
				OnSuccess: config.OnSuccessConfig{
					RouteConfig: routing.RouteConfig{
						IsConditional: true,
						Routes:        []routing.ConditionalRoute{{Condition: containsCond, Stage: "done", RawExpr: containsCond.Raw}},
						Default:       "done",
					},
				},
				OnFailure: "dead-letter",
			}},
		}},
		Agents: []config.Agent{{ID: "agent1", Provider: "mock", SystemPrompt: "test"}},
	}

	reg, _ := contract.NewRegistry(cfg.SchemaRegistry, "/")
	st := memory.New()
	agents := map[string]broker.Agent{
		"agent1": &mockAgent{id: "agent1", provider: "mock"},
	}

	agents["agent1"].(*mockAgent).setHandler(func(_ context.Context, _ *broker.Task) (*broker.TaskResult, error) {
		// Return integer for items field — "contains" will fail at eval time.
		return &broker.TaskResult{Payload: json.RawMessage(`{"items":42}`)}, nil
	})

	b := newConditionalBroker(cfg, st, agents, reg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.Run(ctx)

	task, _ := b.Submit(ctx, "eval-error", json.RawMessage(`{"input":"test"}`))
	final := waitForState(t, st, task.ID, broker.TaskStateFailed, 5*time.Second)

	if final.State != broker.TaskStateFailed {
		t.Fatalf("expected FAILED, got %s", final.State)
	}

	// Verify error describes the type mismatch.
	reason, ok := final.Metadata["failure_reason"].(string)
	if !ok {
		t.Fatalf("expected failure_reason in metadata, got: %v", final.Metadata)
	}
	if !strings.Contains(reason, "contains") {
		t.Errorf("failure reason should mention 'contains', got: %s", reason)
	}
	if !strings.Contains(reason, "routing") {
		t.Errorf("failure reason should mention 'routing', got: %s", reason)
	}
}

// ============================================================================
// 12. Backward compatibility — existing configs load and route correctly
// ============================================================================

func TestConditionalRouting_BackwardCompat_AllExistingConfigs(t *testing.T) {
	root := findRepoRoot(t)
	examples := []string{
		"config/examples/basic.yaml",
		"config/examples/multi-provider.yaml",
		"config/examples/self-hosted.yaml",
		"config/examples/code_review.yaml",
	}

	for _, ex := range examples {
		t.Run(filepath.Base(ex), func(t *testing.T) {
			cfgPath := filepath.Join(root, ex)
			if _, err := os.Stat(cfgPath); os.IsNotExist(err) {
				t.Skipf("config file %s not found", cfgPath)
			}
			cfg, err := config.Load(cfgPath)
			if err != nil {
				t.Fatalf("Load(%s): %v", ex, err)
			}

			// Verify all stages have valid OnSuccess configs.
			for _, p := range cfg.Pipelines {
				for _, s := range p.Stages {
					if s.OnSuccess.IsConditional {
						if len(s.OnSuccess.Routes) == 0 {
							t.Errorf("pipeline %q stage %q: conditional routing with zero routes", p.Name, s.ID)
						}
						if s.OnSuccess.Default == "" {
							t.Errorf("pipeline %q stage %q: conditional routing without default", p.Name, s.ID)
						}
					}
				}
			}
		})
	}
}

func TestConditionalRouting_BackwardCompat_StaticRouting(t *testing.T) {
	// Ensure existing 3-stage pipeline with string on_success works.
	cfg, st, agents, reg := buildTestEnv(t)

	// Standard handlers for linear pipeline.
	agents["agent1"].(*mockAgent).setHandler(func(_ context.Context, _ *broker.Task) (*broker.TaskResult, error) {
		return &broker.TaskResult{Payload: json.RawMessage(`{"category":"from-1"}`)}, nil
	})
	agents["agent2"].(*mockAgent).setHandler(func(_ context.Context, _ *broker.Task) (*broker.TaskResult, error) {
		return &broker.TaskResult{Payload: json.RawMessage(`{"result":"from-2"}`)}, nil
	})
	agents["agent3"].(*mockAgent).setHandler(func(_ context.Context, _ *broker.Task) (*broker.TaskResult, error) {
		return &broker.TaskResult{Payload: json.RawMessage(`{"valid":true}`)}, nil
	})

	b := newBroker(cfg, st, agents, reg)
	b.SetSleepFunc(func(_ context.Context, _ time.Duration) {})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.Run(ctx)

	task, err := b.Submit(ctx, "test-pipeline", json.RawMessage(`{"request":"test"}`))
	if err != nil {
		t.Fatal(err)
	}

	final := waitForState(t, st, task.ID, broker.TaskStateDone, 5*time.Second)
	if final.State != broker.TaskStateDone {
		t.Fatalf("expected DONE, got %s", final.State)
	}
}

// ============================================================================
// Config validation tests
// ============================================================================

func TestConfigValidation_RoutingBlockWithoutDefault_Error(t *testing.T) {
	yaml := `
version: "1"
schema_registry:
  - name: in
    version: "v1"
    path: schemas/in.json
  - name: out
    version: "v1"
    path: schemas/out.json
pipelines:
  - name: test
    concurrency: 1
    store: memory
    stages:
      - id: s1
        agent: a1
        input_schema:
          name: in
          version: "v1"
        output_schema:
          name: out
          version: "v1"
        timeout: 5s
        retry:
          max_attempts: 1
          backoff: fixed
          base_delay: 1s
        on_success:
          routes:
            - condition: 'output.x == "y"'
              stage: done
        on_failure: dead-letter
agents:
  - id: a1
    provider: mock
    system_prompt: test
`
	dir := t.TempDir()
	path := filepath.Join(dir, "test.yaml")
	os.WriteFile(path, []byte(yaml), 0644)

	// Write dummy schema files.
	os.MkdirAll(filepath.Join(dir, "schemas"), 0755)
	os.WriteFile(filepath.Join(dir, "schemas", "in.json"), []byte(`{"type":"object"}`), 0644)
	os.WriteFile(filepath.Join(dir, "schemas", "out.json"), []byte(`{"type":"object"}`), 0644)

	_, err := config.Load(path)
	if err == nil {
		t.Fatal("expected error for routing block without default")
	}
	if !strings.Contains(err.Error(), "default") {
		t.Errorf("error should mention 'default', got: %s", err)
	}
}

func TestConfigValidation_RoutingBlockZeroRoutes_Error(t *testing.T) {
	yaml := `
version: "1"
schema_registry:
  - name: in
    version: "v1"
    path: schemas/in.json
  - name: out
    version: "v1"
    path: schemas/out.json
pipelines:
  - name: test
    concurrency: 1
    store: memory
    stages:
      - id: s1
        agent: a1
        input_schema:
          name: in
          version: "v1"
        output_schema:
          name: out
          version: "v1"
        timeout: 5s
        retry:
          max_attempts: 1
          backoff: fixed
          base_delay: 1s
        on_success:
          routes: []
          default: done
        on_failure: dead-letter
agents:
  - id: a1
    provider: mock
    system_prompt: test
`
	dir := t.TempDir()
	path := filepath.Join(dir, "test.yaml")
	os.WriteFile(path, []byte(yaml), 0644)
	os.MkdirAll(filepath.Join(dir, "schemas"), 0755)
	os.WriteFile(filepath.Join(dir, "schemas", "in.json"), []byte(`{"type":"object"}`), 0644)
	os.WriteFile(filepath.Join(dir, "schemas", "out.json"), []byte(`{"type":"object"}`), 0644)

	_, err := config.Load(path)
	if err == nil {
		t.Fatal("expected error for zero routes in routing block")
	}
	if !strings.Contains(err.Error(), "at least one route") {
		t.Errorf("error should mention routes requirement, got: %s", err)
	}
}

func TestConfigValidation_InvalidConditionSyntax_Error(t *testing.T) {
	yaml := `
version: "1"
schema_registry:
  - name: in
    version: "v1"
    path: schemas/in.json
  - name: out
    version: "v1"
    path: schemas/out.json
pipelines:
  - name: test
    concurrency: 1
    store: memory
    stages:
      - id: s1
        agent: a1
        input_schema:
          name: in
          version: "v1"
        output_schema:
          name: out
          version: "v1"
        timeout: 5s
        retry:
          max_attempts: 1
          backoff: fixed
          base_delay: 1s
        on_success:
          routes:
            - condition: 'invalid..syntax'
              stage: done
          default: done
        on_failure: dead-letter
agents:
  - id: a1
    provider: mock
    system_prompt: test
`
	dir := t.TempDir()
	path := filepath.Join(dir, "test.yaml")
	os.WriteFile(path, []byte(yaml), 0644)
	os.MkdirAll(filepath.Join(dir, "schemas"), 0755)
	os.WriteFile(filepath.Join(dir, "schemas", "in.json"), []byte(`{"type":"object"}`), 0644)
	os.WriteFile(filepath.Join(dir, "schemas", "out.json"), []byte(`{"type":"object"}`), 0644)

	_, err := config.Load(path)
	if err == nil {
		t.Fatal("expected error for invalid condition syntax")
	}
}

func TestConfigValidation_RouteReferencesUnknownStage_Error(t *testing.T) {
	yaml := `
version: "1"
schema_registry:
  - name: in
    version: "v1"
    path: schemas/in.json
  - name: out
    version: "v1"
    path: schemas/out.json
pipelines:
  - name: test
    concurrency: 1
    store: memory
    stages:
      - id: s1
        agent: a1
        input_schema:
          name: in
          version: "v1"
        output_schema:
          name: out
          version: "v1"
        timeout: 5s
        retry:
          max_attempts: 1
          backoff: fixed
          base_delay: 1s
        on_success:
          routes:
            - condition: 'output.x == "y"'
              stage: nonexistent
          default: done
        on_failure: dead-letter
agents:
  - id: a1
    provider: mock
    system_prompt: test
`
	dir := t.TempDir()
	path := filepath.Join(dir, "test.yaml")
	os.WriteFile(path, []byte(yaml), 0644)
	os.MkdirAll(filepath.Join(dir, "schemas"), 0755)
	os.WriteFile(filepath.Join(dir, "schemas", "in.json"), []byte(`{"type":"object"}`), 0644)
	os.WriteFile(filepath.Join(dir, "schemas", "out.json"), []byte(`{"type":"object"}`), 0644)

	_, err := config.Load(path)
	if err == nil {
		t.Fatal("expected error for unknown stage reference")
	}
	if !strings.Contains(err.Error(), "nonexistent") {
		t.Errorf("error should mention 'nonexistent', got: %s", err)
	}
}

func TestConfigValidation_ValidConditionalRouting_OK(t *testing.T) {
	root := findRepoRoot(t)
	cfgPath := filepath.Join(root, "config", "examples", "code_review.yaml")
	_, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("valid conditional routing config failed to load: %v", err)
	}
}

// ============================================================================
// Helpers
// ============================================================================

func newConditionalBroker(cfg *config.Config, st *memory.MemoryStore, agents map[string]broker.Agent, reg *contract.Registry) *broker.Broker {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	b := broker.New(cfg, st, agents, reg, logger, nil, nil)
	b.SetSleepFunc(func(_ context.Context, _ time.Duration) {})
	return b
}

func waitForState(t *testing.T, st *memory.MemoryStore, taskID string, target broker.TaskState, timeout time.Duration) *broker.Task {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		task, err := st.GetTask(context.Background(), taskID)
		if err == nil && task.State == target {
			return task
		}
		time.Sleep(10 * time.Millisecond)
	}
	task, _ := st.GetTask(context.Background(), taskID)
	t.Fatalf("task %s did not reach state %s within %s; current state: %s", taskID, target, timeout, task.State)
	return nil
}

func findRepoRoot(t *testing.T) string {
	t.Helper()
	dir, _ := os.Getwd()
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find repo root (go.mod)")
		}
		dir = parent
	}
}
