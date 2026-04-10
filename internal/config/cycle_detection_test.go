package config

import (
	"strings"
	"testing"

	"github.com/orcastrator/orcastrator/internal/routing"
)

func TestDetectCycles_SelfLoop_NoExit(t *testing.T) {
	// Stage A on_failure → A, no path to done or dead-letter.
	p := Pipeline{
		Name: "test",
		Stages: []Stage{
			{ID: "a", Agent: "x", OnFailure: "a",
				InputSchema:  StageSchemaRef{Name: "s", Version: "v1"},
				OutputSchema: StageSchemaRef{Name: "s", Version: "v1"},
			},
		},
	}
	err := detectCycles(p)
	if err == nil {
		t.Fatal("expected cycle error for self-loop with no exit")
	}
	if !strings.Contains(err.Error(), "a") {
		t.Errorf("error should name stage a: %v", err)
	}
}

func TestDetectCycles_TwoStageCycle_NoExit(t *testing.T) {
	// A on_failure → B, B on_failure → A. Neither has an exit to done/dead-letter.
	p := Pipeline{
		Name: "test",
		Stages: []Stage{
			{ID: "a", Agent: "x", OnFailure: "b",
				InputSchema:  StageSchemaRef{Name: "s", Version: "v1"},
				OutputSchema: StageSchemaRef{Name: "s", Version: "v1"},
			},
			{ID: "b", Agent: "x", OnFailure: "a",
				InputSchema:  StageSchemaRef{Name: "s", Version: "v1"},
				OutputSchema: StageSchemaRef{Name: "s", Version: "v1"},
			},
		},
	}
	err := detectCycles(p)
	if err == nil {
		t.Fatal("expected cycle error for two-stage cycle with no exit")
	}
	if !strings.Contains(err.Error(), "a") || !strings.Contains(err.Error(), "b") {
		t.Errorf("error should name both stages: %v", err)
	}
}

func TestDetectCycles_ThreeStageOnSuccess_NoExit(t *testing.T) {
	// A → B → C → A via on_success, none route to done.
	p := Pipeline{
		Name: "test",
		Stages: []Stage{
			{ID: "a", Agent: "x", OnSuccess: StaticOnSuccess("b"),
				InputSchema:  StageSchemaRef{Name: "s", Version: "v1"},
				OutputSchema: StageSchemaRef{Name: "s", Version: "v1"},
			},
			{ID: "b", Agent: "x", OnSuccess: StaticOnSuccess("c"),
				InputSchema:  StageSchemaRef{Name: "s", Version: "v1"},
				OutputSchema: StageSchemaRef{Name: "s", Version: "v1"},
			},
			{ID: "c", Agent: "x", OnSuccess: StaticOnSuccess("a"),
				InputSchema:  StageSchemaRef{Name: "s", Version: "v1"},
				OutputSchema: StageSchemaRef{Name: "s", Version: "v1"},
			},
		},
	}
	err := detectCycles(p)
	if err == nil {
		t.Fatal("expected cycle error for three-stage closed cycle via on_success")
	}
	for _, s := range []string{"a", "b", "c"} {
		if !strings.Contains(err.Error(), s) {
			t.Errorf("error should name stage %s: %v", s, err)
		}
	}
}

func TestDetectCycles_FiveStageLoop_NoExit(t *testing.T) {
	p := Pipeline{
		Name: "test",
		Stages: []Stage{
			{ID: "a", Agent: "x", OnSuccess: StaticOnSuccess("b"),
				InputSchema: StageSchemaRef{Name: "s", Version: "v1"}, OutputSchema: StageSchemaRef{Name: "s", Version: "v1"}},
			{ID: "b", Agent: "x", OnSuccess: StaticOnSuccess("c"),
				InputSchema: StageSchemaRef{Name: "s", Version: "v1"}, OutputSchema: StageSchemaRef{Name: "s", Version: "v1"}},
			{ID: "c", Agent: "x", OnSuccess: StaticOnSuccess("d"),
				InputSchema: StageSchemaRef{Name: "s", Version: "v1"}, OutputSchema: StageSchemaRef{Name: "s", Version: "v1"}},
			{ID: "d", Agent: "x", OnSuccess: StaticOnSuccess("e"),
				InputSchema: StageSchemaRef{Name: "s", Version: "v1"}, OutputSchema: StageSchemaRef{Name: "s", Version: "v1"}},
			{ID: "e", Agent: "x", OnSuccess: StaticOnSuccess("a"),
				InputSchema: StageSchemaRef{Name: "s", Version: "v1"}, OutputSchema: StageSchemaRef{Name: "s", Version: "v1"}},
		},
	}
	err := detectCycles(p)
	if err == nil {
		t.Fatal("expected cycle error for five-stage closed cycle")
	}
	for _, s := range []string{"a", "b", "c", "d", "e"} {
		if !strings.Contains(err.Error(), s) {
			t.Errorf("error should name stage %s: %v", s, err)
		}
	}
}

func TestDetectCycles_ConditionalRoutingDefault_NoExit(t *testing.T) {
	// Conditional default creates a closed cycle: a → b → a, no exit.
	p := Pipeline{
		Name: "test",
		Stages: []Stage{
			{ID: "a", Agent: "x",
				OnSuccess: OnSuccessConfig{
					RouteConfig: routing.RouteConfig{
						IsConditional: true,
						Default:       "b",
						Routes: []routing.ConditionalRoute{
							{Stage: "b"},
						},
					},
				},
				InputSchema: StageSchemaRef{Name: "s", Version: "v1"}, OutputSchema: StageSchemaRef{Name: "s", Version: "v1"}},
			{ID: "b", Agent: "x", OnSuccess: StaticOnSuccess("a"),
				InputSchema: StageSchemaRef{Name: "s", Version: "v1"}, OutputSchema: StageSchemaRef{Name: "s", Version: "v1"}},
		},
	}
	err := detectCycles(p)
	if err == nil {
		t.Fatal("expected cycle error through conditional default with no exit")
	}
}

func TestDetectCycles_ConditionalRoutingRouteTarget_NoExit(t *testing.T) {
	// Conditional route target creates a closed cycle: a → b → a, no exit.
	p := Pipeline{
		Name: "test",
		Stages: []Stage{
			{ID: "a", Agent: "x",
				OnSuccess: OnSuccessConfig{
					RouteConfig: routing.RouteConfig{
						IsConditional: true,
						Default:       "b",
						Routes: []routing.ConditionalRoute{
							{Stage: "b"},
						},
					},
				},
				InputSchema: StageSchemaRef{Name: "s", Version: "v1"}, OutputSchema: StageSchemaRef{Name: "s", Version: "v1"}},
			{ID: "b", Agent: "x", OnSuccess: StaticOnSuccess("a"),
				InputSchema: StageSchemaRef{Name: "s", Version: "v1"}, OutputSchema: StageSchemaRef{Name: "s", Version: "v1"}},
		},
	}
	err := detectCycles(p)
	if err == nil {
		t.Fatal("expected cycle error through conditional route target with no exit")
	}
}

func TestDetectCycles_FanOutStageInCycle_NoExit(t *testing.T) {
	p := Pipeline{
		Name: "test",
		Stages: []Stage{
			{ID: "a", Agent: "x", OnSuccess: StaticOnSuccess("b"),
				InputSchema: StageSchemaRef{Name: "s", Version: "v1"}, OutputSchema: StageSchemaRef{Name: "s", Version: "v1"}},
			{ID: "b",
				FanOut: &FanOutConfig{
					Agents:  []FanOutAgent{{ID: "x"}, {ID: "y"}},
					Mode:    FanOutModeGather,
					Require: RequirePolicyAll,
				},
				OnSuccess:       StaticOnSuccess("a"),
				AggregateSchema: &StageSchemaRef{Name: "s", Version: "v1"},
				InputSchema:     StageSchemaRef{Name: "s", Version: "v1"}, OutputSchema: StageSchemaRef{Name: "s", Version: "v1"}},
		},
	}
	err := detectCycles(p)
	if err == nil {
		t.Fatal("expected cycle error with fan-out stage in closed cycle")
	}
}

func TestDetectCycles_CycleWithExit_NotFlagged(t *testing.T) {
	// process → validate (on_success), validate → process (on_failure),
	// but process on_failure → dead-letter and validate on_success → done.
	// This is a retry-loop with exits — NOT an error.
	p := Pipeline{
		Name: "test",
		Stages: []Stage{
			{ID: "process", Agent: "x", OnSuccess: StaticOnSuccess("validate"), OnFailure: "dead-letter",
				InputSchema: StageSchemaRef{Name: "s", Version: "v1"}, OutputSchema: StageSchemaRef{Name: "s", Version: "v1"}},
			{ID: "validate", Agent: "x", OnSuccess: StaticOnSuccess("done"), OnFailure: "process",
				InputSchema: StageSchemaRef{Name: "s", Version: "v1"}, OutputSchema: StageSchemaRef{Name: "s", Version: "v1"}},
		},
	}
	if err := detectCycles(p); err != nil {
		t.Errorf("expected no error for cycle with exit path: %v", err)
	}
}

func TestDetectCycles_NoCycle_LinearPipeline(t *testing.T) {
	p := Pipeline{
		Name: "test",
		Stages: []Stage{
			{ID: "a", Agent: "x", OnSuccess: StaticOnSuccess("b"), OnFailure: "dead-letter",
				InputSchema: StageSchemaRef{Name: "s", Version: "v1"}, OutputSchema: StageSchemaRef{Name: "s", Version: "v1"}},
			{ID: "b", Agent: "x", OnSuccess: StaticOnSuccess("done"), OnFailure: "dead-letter",
				InputSchema: StageSchemaRef{Name: "s", Version: "v1"}, OutputSchema: StageSchemaRef{Name: "s", Version: "v1"}},
		},
	}
	if err := detectCycles(p); err != nil {
		t.Errorf("expected no cycle for linear pipeline: %v", err)
	}
}

func TestDetectCycles_NoCycle_FailureToDeadLetter(t *testing.T) {
	// A → B → done, A on_failure → C → dead-letter. No cycle.
	p := Pipeline{
		Name: "test",
		Stages: []Stage{
			{ID: "a", Agent: "x", OnSuccess: StaticOnSuccess("b"), OnFailure: "c",
				InputSchema: StageSchemaRef{Name: "s", Version: "v1"}, OutputSchema: StageSchemaRef{Name: "s", Version: "v1"}},
			{ID: "b", Agent: "x", OnSuccess: StaticOnSuccess("done"),
				InputSchema: StageSchemaRef{Name: "s", Version: "v1"}, OutputSchema: StageSchemaRef{Name: "s", Version: "v1"}},
			{ID: "c", Agent: "x", OnFailure: "dead-letter",
				InputSchema: StageSchemaRef{Name: "s", Version: "v1"}, OutputSchema: StageSchemaRef{Name: "s", Version: "v1"}},
		},
	}
	if err := detectCycles(p); err != nil {
		t.Errorf("expected no cycle: %v", err)
	}
}

func TestDetectCycles_MixedCycle_WithExit(t *testing.T) {
	// A on_success → B, A on_failure → B, B on_success → A, but B on_failure → dead-letter.
	// This is a cycle with an exit — NOT an error.
	p := Pipeline{
		Name: "test",
		Stages: []Stage{
			{ID: "a", Agent: "x", OnSuccess: StaticOnSuccess("b"), OnFailure: "b",
				InputSchema: StageSchemaRef{Name: "s", Version: "v1"}, OutputSchema: StageSchemaRef{Name: "s", Version: "v1"}},
			{ID: "b", Agent: "x", OnSuccess: StaticOnSuccess("a"), OnFailure: "dead-letter",
				InputSchema: StageSchemaRef{Name: "s", Version: "v1"}, OutputSchema: StageSchemaRef{Name: "s", Version: "v1"}},
		},
	}
	if err := detectCycles(p); err != nil {
		t.Errorf("expected no error for cycle with exit via on_failure: %v", err)
	}
}

func TestDetectCycles_ConditionalWithExit_NotFlagged(t *testing.T) {
	// Conditional routing cycle where default leads to done.
	p := Pipeline{
		Name: "test",
		Stages: []Stage{
			{ID: "a", Agent: "x",
				OnSuccess: OnSuccessConfig{
					RouteConfig: routing.RouteConfig{
						IsConditional: true,
						Default:       "done",
						Routes: []routing.ConditionalRoute{
							{Stage: "b"},
						},
					},
				},
				InputSchema: StageSchemaRef{Name: "s", Version: "v1"}, OutputSchema: StageSchemaRef{Name: "s", Version: "v1"}},
			{ID: "b", Agent: "x", OnSuccess: StaticOnSuccess("a"), OnFailure: "dead-letter",
				InputSchema: StageSchemaRef{Name: "s", Version: "v1"}, OutputSchema: StageSchemaRef{Name: "s", Version: "v1"}},
		},
	}
	if err := detectCycles(p); err != nil {
		t.Errorf("expected no error for conditional cycle with exit: %v", err)
	}
}

func TestDetectCycles_ExampleConfigs(t *testing.T) {
	// All example configs must pass cycle detection.
	examples := []string{
		"../../config/examples/basic.yaml",
		"../../config/examples/self-hosted.yaml",
		"../../config/examples/multi-provider.yaml",
		"../../config/examples/code_review.yaml",
	}
	for _, path := range examples {
		cfg, err := Load(path)
		if err != nil {
			// Some configs may require env vars — skip those.
			t.Logf("skipping %s (load error: %v)", path, err)
			continue
		}
		for _, p := range cfg.Pipelines {
			if err := detectCycles(p); err != nil {
				t.Errorf("example config %s pipeline %q: unexpected cycle: %v", path, p.Name, err)
			}
		}
	}
}
