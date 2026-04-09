package config

import (
	"strings"
	"testing"
)

// Base config with fan-out support. Used by all fan-out validation tests.
const validFanOutConfig = `
version: "1"

schema_registry:
  - name: input_a
    version: "v1"
    path: schemas/input_a_v1.json
  - name: output_a
    version: "v1"
    path: schemas/output_a_v1.json
  - name: agg_a
    version: "v1"
    path: schemas/agg_a_v1.json

pipelines:
  - name: fanout-pipeline
    concurrency: 3
    store: memory
    stages:
      - id: fan-stage
        fan_out:
          agents:
            - id: agent-1
            - id: agent-2
          mode: gather
          timeout: 30s
          require: all
        input_schema:
          name: input_a
          version: "v1"
        output_schema:
          name: output_a
          version: "v1"
        aggregate_schema:
          name: agg_a
          version: "v1"
        timeout: 30s
        retry:
          max_attempts: 1
          backoff: fixed
          base_delay: 1s
        on_success: done
        on_failure: dead-letter

agents:
  - id: agent-1
    provider: anthropic
    model: claude-sonnet-4-20250514
    auth:
      api_key_env: ANTHROPIC_API_KEY
    system_prompt: "Agent 1"
    timeout: 10s
  - id: agent-2
    provider: google
    model: gemini-2.0-flash
    auth:
      api_key_env: GEMINI_API_KEY
    system_prompt: "Agent 2"
    timeout: 10s

stores:
  memory:
    max_tasks: 1000
`

func TestFanOut_ValidConfig(t *testing.T) {
	path := writeTestConfig(t, validFanOutConfig)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	stage := cfg.Pipelines[0].Stages[0]
	if stage.FanOut == nil {
		t.Fatal("expected FanOut to be set")
	}
	if len(stage.FanOut.Agents) != 2 {
		t.Errorf("expected 2 fan-out agents, got %d", len(stage.FanOut.Agents))
	}
	if stage.FanOut.Mode != FanOutModeGather {
		t.Errorf("expected mode gather, got %q", stage.FanOut.Mode)
	}
	if stage.FanOut.Require != RequirePolicyAll {
		t.Errorf("expected require all, got %q", stage.FanOut.Require)
	}
	if stage.AggregateSchema == nil {
		t.Fatal("expected AggregateSchema to be set")
	}
}

func TestFanOut_BothAgentAndFanOut(t *testing.T) {
	cfg := strings.Replace(validFanOutConfig,
		"      - id: fan-stage\n        fan_out:",
		"      - id: fan-stage\n        agent: agent-1\n        fan_out:", 1)
	path := writeTestConfig(t, cfg)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for stage with both agent and fan_out")
	}
	if !strings.Contains(err.Error(), "must have either agent or fan_out, not both") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestFanOut_NeitherAgentNorFanOut(t *testing.T) {
	// Remove the fan_out block entirely to leave neither agent nor fan_out.
	cfg := `
version: "1"

schema_registry:
  - name: input_a
    version: "v1"
    path: schemas/input_a_v1.json
  - name: output_a
    version: "v1"
    path: schemas/output_a_v1.json

pipelines:
  - name: p1
    concurrency: 1
    store: memory
    stages:
      - id: s1
        input_schema:
          name: input_a
          version: "v1"
        output_schema:
          name: output_a
          version: "v1"
        timeout: 10s
        on_success: done
        on_failure: dead-letter

agents:
  - id: agent-1
    provider: anthropic
    model: claude-sonnet-4-20250514
    auth:
      api_key_env: ANTHROPIC_API_KEY
    timeout: 10s
`
	path := writeTestConfig(t, cfg)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for stage with neither agent nor fan_out")
	}
	if !strings.Contains(err.Error(), "must have either agent or fan_out") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestFanOut_OnlyOneAgent(t *testing.T) {
	cfg := strings.Replace(validFanOutConfig,
		"            - id: agent-1\n            - id: agent-2",
		"            - id: agent-1", 1)
	path := writeTestConfig(t, cfg)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for fan_out with only one agent")
	}
	if !strings.Contains(err.Error(), "at least 2 agents") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestFanOut_DuplicateAgentIDs(t *testing.T) {
	cfg := strings.Replace(validFanOutConfig,
		"            - id: agent-1\n            - id: agent-2",
		"            - id: agent-1\n            - id: agent-1", 1)
	path := writeTestConfig(t, cfg)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for duplicate agent IDs in fan_out")
	}
	if !strings.Contains(err.Error(), "duplicate agent ID") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestFanOut_MissingAggregateSchema(t *testing.T) {
	// Remove aggregate_schema from a fan_out stage.
	cfg := strings.Replace(validFanOutConfig,
		"        aggregate_schema:\n          name: agg_a\n          version: \"v1\"\n", "", 1)
	path := writeTestConfig(t, cfg)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for fan_out without aggregate_schema")
	}
	if !strings.Contains(err.Error(), "aggregate_schema is required") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestFanOut_AggregateSchemaOnSingleAgentStage(t *testing.T) {
	// Use a single-agent stage but add aggregate_schema — must be rejected.
	cfg := `
version: "1"

schema_registry:
  - name: input_a
    version: "v1"
    path: schemas/input_a_v1.json
  - name: output_a
    version: "v1"
    path: schemas/output_a_v1.json
  - name: agg_a
    version: "v1"
    path: schemas/agg_a_v1.json

pipelines:
  - name: p1
    concurrency: 1
    store: memory
    stages:
      - id: s1
        agent: agent-1
        input_schema:
          name: input_a
          version: "v1"
        output_schema:
          name: output_a
          version: "v1"
        aggregate_schema:
          name: agg_a
          version: "v1"
        timeout: 10s
        on_success: done
        on_failure: dead-letter

agents:
  - id: agent-1
    provider: anthropic
    model: claude-sonnet-4-20250514
    auth:
      api_key_env: ANTHROPIC_API_KEY
    timeout: 10s
`
	path := writeTestConfig(t, cfg)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for aggregate_schema on single-agent stage")
	}
	if !strings.Contains(err.Error(), "aggregate_schema is only allowed with fan_out") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestFanOut_UnknownAgentInFanOut(t *testing.T) {
	cfg := strings.Replace(validFanOutConfig,
		"- id: agent-2", "- id: nonexistent-agent", 1)
	path := writeTestConfig(t, cfg)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for unknown agent in fan_out")
	}
	if !strings.Contains(err.Error(), "unknown agent") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestFanOut_InvalidMode(t *testing.T) {
	cfg := strings.Replace(validFanOutConfig, "mode: gather", "mode: invalid", 1)
	path := writeTestConfig(t, cfg)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for invalid fan_out mode")
	}
	if !strings.Contains(err.Error(), "fan_out mode must be") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestFanOut_InvalidMode_Scatter(t *testing.T) {
	cfg := strings.Replace(validFanOutConfig, "mode: gather", "mode: scatter", 1)
	path := writeTestConfig(t, cfg)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for unknown mode 'scatter'")
	}
	errMsg := err.Error()
	if !strings.Contains(errMsg, "fan_out mode must be") {
		t.Errorf("unexpected error: %v", err)
	}
	// Error should name the offending stage.
	if !strings.Contains(errMsg, "fan-stage") {
		t.Errorf("error should name the offending stage 'fan-stage', got: %v", err)
	}
}

func TestFanOut_InvalidRequire(t *testing.T) {
	cfg := strings.Replace(validFanOutConfig, "require: all", "require: invalid", 1)
	path := writeTestConfig(t, cfg)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for invalid require policy")
	}
	if !strings.Contains(err.Error(), "fan_out require must be") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestFanOut_InvalidRequire_Unanimous(t *testing.T) {
	cfg := strings.Replace(validFanOutConfig, "require: all", "require: unanimous", 1)
	path := writeTestConfig(t, cfg)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for unknown require 'unanimous'")
	}
	errMsg := err.Error()
	if !strings.Contains(errMsg, "fan_out require must be") {
		t.Errorf("unexpected error: %v", err)
	}
	if !strings.Contains(errMsg, "fan-stage") {
		t.Errorf("error should name the offending stage 'fan-stage', got: %v", err)
	}
}

// §1: Verify all config validation errors name the offending stage.
func TestFanOut_ErrorsNameStage(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(string) string
		wantErr string
	}{
		{
			name: "both agent and fan_out",
			mutate: func(c string) string {
				return strings.Replace(c,
					"      - id: fan-stage\n        fan_out:",
					"      - id: fan-stage\n        agent: agent-1\n        fan_out:", 1)
			},
			wantErr: "fan-stage",
		},
		{
			name: "only one agent",
			mutate: func(c string) string {
				return strings.Replace(c,
					"            - id: agent-1\n            - id: agent-2",
					"            - id: agent-1", 1)
			},
			wantErr: "fan-stage",
		},
		{
			name: "duplicate agent ID",
			mutate: func(c string) string {
				return strings.Replace(c,
					"            - id: agent-1\n            - id: agent-2",
					"            - id: agent-1\n            - id: agent-1", 1)
			},
			wantErr: "fan-stage",
		},
		{
			name: "missing aggregate_schema",
			mutate: func(c string) string {
				return strings.Replace(c,
					"        aggregate_schema:\n          name: agg_a\n          version: \"v1\"\n", "", 1)
			},
			wantErr: "fan-stage",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := tt.mutate(validFanOutConfig)
			path := writeTestConfig(t, cfg)
			_, err := Load(path)
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error should name stage %q, got: %v", tt.wantErr, err)
			}
		})
	}
}

// §2: Valid config with all three require policies loads without error.
func TestFanOut_AllRequirePolicies(t *testing.T) {
	for _, policy := range []string{"all", "any", "majority"} {
		t.Run(policy, func(t *testing.T) {
			cfg := strings.Replace(validFanOutConfig, "require: all", "require: "+policy, 1)
			path := writeTestConfig(t, cfg)
			_, err := Load(path)
			if err != nil {
				t.Fatalf("policy %q should load without error, got: %v", policy, err)
			}
		})
	}
}
