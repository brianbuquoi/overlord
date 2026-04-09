package code_review_test

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"

	"github.com/orcastrator/orcastrator/internal/agent"
	"github.com/orcastrator/orcastrator/internal/broker"
	"github.com/orcastrator/orcastrator/internal/config"
	"github.com/orcastrator/orcastrator/internal/contract"
	"github.com/orcastrator/orcastrator/internal/metrics"
	"github.com/orcastrator/orcastrator/internal/store/memory"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

// repoRoot returns the absolute path to the repository root.
func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	return root
}

// --- Mock Agent ---

var _ broker.Agent = (*mockAgent)(nil)

type mockAgent struct {
	id       string
	provider string
	mu       sync.Mutex
	handler  func(ctx context.Context, task *broker.Task) (*broker.TaskResult, error)
}

func (m *mockAgent) ID() string                          { return m.id }
func (m *mockAgent) Provider() string                    { return m.provider }
func (m *mockAgent) HealthCheck(_ context.Context) error { return nil }

func (m *mockAgent) Execute(ctx context.Context, task *broker.Task) (*broker.TaskResult, error) {
	m.mu.Lock()
	h := m.handler
	m.mu.Unlock()
	return h(ctx, task)
}

func (m *mockAgent) setHandler(h func(ctx context.Context, task *broker.Task) (*broker.TaskResult, error)) {
	m.mu.Lock()
	m.handler = h
	m.mu.Unlock()
}

// waitForTaskState polls until the task reaches the desired state or timeout.
func waitForTaskState(t *testing.T, st *memory.MemoryStore, taskID string, want broker.TaskState, timeout time.Duration) *broker.Task {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		task, err := st.GetTask(context.Background(), taskID)
		if err == nil && task.State == want {
			return task
		}
		time.Sleep(20 * time.Millisecond)
	}
	task, _ := st.GetTask(context.Background(), taskID)
	t.Fatalf("task %s did not reach state %s within %v; current: %v", taskID, want, timeout, task)
	return nil
}

// --- Schema paths ---

var schemaFiles = []string{
	"schemas/parse_input_v1.json",
	"schemas/parse_output_v1.json",
	"schemas/review_input_v1.json",
	"schemas/review_output_v1.json",
	"schemas/summarize_input_v1.json",
	"schemas/summarize_output_v1.json",
}

// compileSchemaFile compiles a JSONSchema file and returns the compiled schema.
func compileSchemaFile(t *testing.T, path string) *jsonschema.Schema {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read schema %s: %v", path, err)
	}
	unmarshal, err := jsonschema.UnmarshalJSON(strings.NewReader(string(data)))
	if err != nil {
		t.Fatalf("unmarshal %s: %v", path, err)
	}
	c := jsonschema.NewCompiler()
	url := "file://" + filepath.ToSlash(path)
	if err := c.AddResource(url, unmarshal); err != nil {
		t.Fatalf("add resource %s: %v", path, err)
	}
	compiled, err := c.Compile(url)
	if err != nil {
		t.Fatalf("compile %s: %v", path, err)
	}
	return compiled
}

// ============================================================================
// TEST 1: Schema validation — all schema files are valid JSONSchema with
// required fields and additionalProperties: false
// ============================================================================

func TestSchemaValidity(t *testing.T) {
	root := repoRoot(t)

	for _, f := range schemaFiles {
		t.Run(f, func(t *testing.T) {
			path := filepath.Join(root, f)
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read schema: %v", err)
			}

			var schema map[string]any
			if err := json.Unmarshal(data, &schema); err != nil {
				t.Fatalf("invalid JSON: %v", err)
			}

			// Verify it compiles as valid JSONSchema.
			compileSchemaFile(t, path)

			// Check required fields exist.
			required, ok := schema["required"]
			if !ok {
				t.Error("schema missing 'required' field")
			}
			reqArr, ok := required.([]any)
			if !ok || len(reqArr) == 0 {
				t.Error("'required' must be a non-empty array")
			}

			// Check additionalProperties: false.
			ap, ok := schema["additionalProperties"]
			if !ok {
				t.Error("schema missing 'additionalProperties' field")
			} else if ap != false {
				t.Errorf("additionalProperties should be false, got %v", ap)
			}
		})
	}
}

// TestSampleInputValidatesAgainstParseInputSchema verifies sample_input.json
// validates against the parse_input_v1 schema.
func TestSampleInputValidatesAgainstParseInputSchema(t *testing.T) {
	root := repoRoot(t)
	compiled := compileSchemaFile(t, filepath.Join(root, "schemas/parse_input_v1.json"))

	payloadData, err := os.ReadFile(filepath.Join(root, "examples/code_review/sample_input.json"))
	if err != nil {
		t.Fatal(err)
	}

	var payload any
	if err := json.Unmarshal(payloadData, &payload); err != nil {
		t.Fatal(err)
	}

	if err := compiled.Validate(payload); err != nil {
		t.Errorf("sample_input.json does not validate against parse_input_v1: %v", err)
	}
}

// ============================================================================
// TEST 2: Schema strictness — extra fields are rejected by all output schemas
// ============================================================================

func TestSchemaStrictness_RejectsExtraFields(t *testing.T) {
	root := repoRoot(t)

	cases := []struct {
		schema  string
		payload string
	}{
		{
			"schemas/parse_output_v1.json",
			`{"files_changed":["a.go"],"summary":"test","areas_of_concern":["x"],"smuggled_data":"pwned"}`,
		},
		{
			"schemas/review_output_v1.json",
			`{"findings":[],"overall_assessment":"approve","smuggled_data":"pwned"}`,
		},
		{
			"schemas/summarize_output_v1.json",
			`{"executive_summary":"ok","critical_count":0,"high_count":0,"action_required":false,"top_recommendations":[],"smuggled_data":"pwned"}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.schema, func(t *testing.T) {
			compiled := compileSchemaFile(t, filepath.Join(root, tc.schema))

			var payload any
			if err := json.Unmarshal([]byte(tc.payload), &payload); err != nil {
				t.Fatal(err)
			}

			if err := compiled.Validate(payload); err == nil {
				t.Error("expected validation to reject payload with extra fields, but it passed")
			}
		})
	}
}

// ============================================================================
// TEST 3: Schema chain compatibility — parse output → review input,
// review output → summarize input
// ============================================================================

func TestSchemaChainCompatibility(t *testing.T) {
	root := repoRoot(t)

	loadRequiredFields := func(schemaPath string) []string {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(root, schemaPath))
		if err != nil {
			t.Fatal(err)
		}
		var schema map[string]any
		if err := json.Unmarshal(data, &schema); err != nil {
			t.Fatal(err)
		}
		reqRaw, ok := schema["required"].([]any)
		if !ok {
			t.Fatalf("%s has no required array", schemaPath)
		}
		var fields []string
		for _, r := range reqRaw {
			fields = append(fields, r.(string))
		}
		return fields
	}

	loadPropertyNames := func(schemaPath string) map[string]bool {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(root, schemaPath))
		if err != nil {
			t.Fatal(err)
		}
		var schema map[string]any
		if err := json.Unmarshal(data, &schema); err != nil {
			t.Fatal(err)
		}
		props, ok := schema["properties"].(map[string]any)
		if !ok {
			t.Fatalf("%s has no properties", schemaPath)
		}
		names := make(map[string]bool, len(props))
		for k := range props {
			names[k] = true
		}
		return names
	}

	t.Run("parse_output→review_input", func(t *testing.T) {
		parseOutputProps := loadPropertyNames("schemas/parse_output_v1.json")
		reviewInputRequired := loadRequiredFields("schemas/review_input_v1.json")
		for _, field := range reviewInputRequired {
			if !parseOutputProps[field] {
				t.Errorf("review_input requires field %q but parse_output does not produce it", field)
			}
		}
	})

	t.Run("review_output→summarize_input", func(t *testing.T) {
		reviewOutputProps := loadPropertyNames("schemas/review_output_v1.json")
		summarizeInputRequired := loadRequiredFields("schemas/summarize_input_v1.json")
		for _, field := range summarizeInputRequired {
			if !reviewOutputProps[field] {
				t.Errorf("summarize_input requires field %q but review_output does not produce it", field)
			}
		}
	})
}

// ============================================================================
// TEST 4: Pipeline config loads cleanly via config.Load()
// ============================================================================

func TestConfigLoads(t *testing.T) {
	root := repoRoot(t)
	configPath := filepath.Join(root, "config", "examples", "code_review.yaml")

	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("config.Load failed: %v", err)
	}

	if cfg.Version != "1" {
		t.Errorf("expected version '1', got %q", cfg.Version)
	}
}

// ============================================================================
// TEST 5: All schema_registry paths exist on disk
// ============================================================================

func TestSchemaRegistryFilesExist(t *testing.T) {
	root := repoRoot(t)
	configPath := filepath.Join(root, "config", "examples", "code_review.yaml")

	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}

	basePath := filepath.Dir(configPath)
	for _, entry := range cfg.SchemaRegistry {
		schemaPath := entry.Path
		if !filepath.IsAbs(schemaPath) {
			schemaPath = filepath.Join(basePath, schemaPath)
		}
		if _, err := os.Stat(schemaPath); os.IsNotExist(err) {
			t.Errorf("schema %s@%s: file not found at %s", entry.Name, entry.Version, schemaPath)
		}
	}

	// Also verify the contract registry compiles all schemas.
	_, err = contract.NewRegistry(cfg.SchemaRegistry, basePath)
	if err != nil {
		t.Errorf("contract.NewRegistry failed: %v", err)
	}
}

// ============================================================================
// TEST 6: Pipeline has exactly 3 stages in correct order with correct routing
// ============================================================================

func TestPipelineStageTopology(t *testing.T) {
	root := repoRoot(t)
	configPath := filepath.Join(root, "config", "examples", "code_review.yaml")

	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}

	var pipeline *config.Pipeline
	for i := range cfg.Pipelines {
		if cfg.Pipelines[i].Name == "code-review" {
			pipeline = &cfg.Pipelines[i]
			break
		}
	}
	if pipeline == nil {
		t.Fatal("pipeline 'code-review' not found")
	}

	if len(pipeline.Stages) != 3 {
		t.Fatalf("expected 3 stages, got %d", len(pipeline.Stages))
	}

	expected := []struct {
		id, onSuccess, onFailure string
	}{
		{"parse", "review", "dead-letter"},
		{"review", "summarize", "dead-letter"},
		{"summarize", "done", "dead-letter"},
	}

	for i, want := range expected {
		got := pipeline.Stages[i]
		if got.ID != want.id {
			t.Errorf("stage[%d]: id=%q, want %q", i, got.ID, want.id)
		}
		if got.OnSuccess != want.onSuccess {
			t.Errorf("stage[%d] (%s): on_success=%q, want %q", i, want.id, got.OnSuccess, want.onSuccess)
		}
		if got.OnFailure != want.onFailure {
			t.Errorf("stage[%d] (%s): on_failure=%q, want %q", i, want.id, got.OnFailure, want.onFailure)
		}
	}
}

// ============================================================================
// Helpers for mocked end-to-end tests
// ============================================================================

func buildCodeReviewBroker(t *testing.T, m *metrics.Metrics) (
	*broker.Broker,
	*memory.MemoryStore,
	map[string]*mockAgent,
) {
	t.Helper()
	root := repoRoot(t)
	configPath := filepath.Join(root, "config", "examples", "code_review.yaml")
	basePath := filepath.Dir(configPath)

	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}

	reg, err := contract.NewRegistry(cfg.SchemaRegistry, basePath)
	if err != nil {
		t.Fatal(err)
	}

	st := memory.New()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	mocks := map[string]*mockAgent{
		"haiku-parse":     {id: "haiku-parse", provider: "mock"},
		"sonnet-review":   {id: "sonnet-review", provider: "mock"},
		"haiku-summarize": {id: "haiku-summarize", provider: "mock"},
	}
	agents := map[string]broker.Agent{
		"haiku-parse":     mocks["haiku-parse"],
		"sonnet-review":   mocks["sonnet-review"],
		"haiku-summarize": mocks["haiku-summarize"],
	}

	b := broker.New(cfg, st, agents, reg, logger, m, nil)
	b.SetSleepFunc(func(_ context.Context, _ time.Duration) {}) // no-op sleep for tests

	return b, st, mocks
}

// buildCodeReviewBrokerWithRetryOverride is like buildCodeReviewBroker but
// overrides the max_attempts for a specific stage.
func buildCodeReviewBrokerWithRetryOverride(t *testing.T, m *metrics.Metrics, stageID string, maxAttempts int) (
	*broker.Broker,
	*memory.MemoryStore,
	map[string]*mockAgent,
) {
	t.Helper()
	root := repoRoot(t)
	configPath := filepath.Join(root, "config", "examples", "code_review.yaml")
	basePath := filepath.Dir(configPath)

	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}

	// Override max_attempts for the target stage.
	for i := range cfg.Pipelines {
		for j := range cfg.Pipelines[i].Stages {
			if cfg.Pipelines[i].Stages[j].ID == stageID {
				cfg.Pipelines[i].Stages[j].Retry.MaxAttempts = maxAttempts
			}
		}
	}

	reg, err := contract.NewRegistry(cfg.SchemaRegistry, basePath)
	if err != nil {
		t.Fatal(err)
	}

	st := memory.New()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	mocks := map[string]*mockAgent{
		"haiku-parse":     {id: "haiku-parse", provider: "mock"},
		"sonnet-review":   {id: "sonnet-review", provider: "mock"},
		"haiku-summarize": {id: "haiku-summarize", provider: "mock"},
	}
	agents := map[string]broker.Agent{
		"haiku-parse":     mocks["haiku-parse"],
		"sonnet-review":   mocks["sonnet-review"],
		"haiku-summarize": mocks["haiku-summarize"],
	}

	b := broker.New(cfg, st, agents, reg, logger, m, nil)
	b.SetSleepFunc(func(_ context.Context, _ time.Duration) {})

	return b, st, mocks
}

func loadSamplePayload(t *testing.T) json.RawMessage {
	t.Helper()
	root := repoRoot(t)
	data, err := os.ReadFile(filepath.Join(root, "examples/code_review/sample_input.json"))
	if err != nil {
		t.Fatal(err)
	}
	return json.RawMessage(data)
}

// Valid mock payloads for each stage.
var (
	validParseOutput = json.RawMessage(`{
		"files_changed": ["internal/api/users.go", "internal/api/middleware.go"],
		"summary": "Adds user lookup endpoint with SQL query and auth middleware.",
		"areas_of_concern": ["SQL injection in users.go", "Hardcoded admin token"]
	}`)

	validReviewOutput = json.RawMessage(`{
		"findings": [
			{
				"file": "internal/api/users.go",
				"severity": "critical",
				"description": "SQL injection via string interpolation in GetUser",
				"suggestion": "Use parameterized queries with $1 placeholders"
			},
			{
				"file": "internal/api/users.go",
				"severity": "critical",
				"description": "Hardcoded admin token exposed in source code",
				"suggestion": "Move to environment variable or secrets manager"
			}
		],
		"overall_assessment": "request_changes"
	}`)

	validSummarizeOutput = json.RawMessage(`{
		"executive_summary": "Critical security issues found: SQL injection and hardcoded credentials.",
		"critical_count": 2,
		"high_count": 0,
		"action_required": true,
		"top_recommendations": [
			"Replace string interpolation with parameterized SQL queries",
			"Move hardcoded admin token to environment variable"
		]
	}`)
)

// setAllMockHandlers sets the standard happy-path handlers on all three mock agents.
func setAllMockHandlers(mocks map[string]*mockAgent) {
	mocks["haiku-parse"].setHandler(func(_ context.Context, _ *broker.Task) (*broker.TaskResult, error) {
		return &broker.TaskResult{Payload: validParseOutput}, nil
	})
	mocks["sonnet-review"].setHandler(func(_ context.Context, _ *broker.Task) (*broker.TaskResult, error) {
		return &broker.TaskResult{Payload: validReviewOutput}, nil
	})
	mocks["haiku-summarize"].setHandler(func(_ context.Context, _ *broker.Task) (*broker.TaskResult, error) {
		return &broker.TaskResult{Payload: validSummarizeOutput}, nil
	})
}

// ============================================================================
// TEST 7: Mocked end-to-end happy path — all 3 stages complete
// ============================================================================

func TestMockedEndToEnd_HappyPath(t *testing.T) {
	b, st, mocks := buildCodeReviewBroker(t, nil)
	setAllMockHandlers(mocks)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.Run(ctx)

	task, err := b.Submit(ctx, "code-review", loadSamplePayload(t))
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	finalTask := waitForTaskState(t, st, task.ID, broker.TaskStateDone, 10*time.Second)

	var summary struct {
		ExecutiveSummary   string   `json:"executive_summary"`
		CriticalCount      int      `json:"critical_count"`
		HighCount          int      `json:"high_count"`
		ActionRequired     bool     `json:"action_required"`
		TopRecommendations []string `json:"top_recommendations"`
	}
	if err := json.Unmarshal(finalTask.Payload, &summary); err != nil {
		t.Fatalf("unmarshal final payload: %v", err)
	}

	if summary.ExecutiveSummary == "" {
		t.Error("executive_summary should not be empty")
	}
	if summary.CriticalCount != 2 {
		t.Errorf("expected critical_count=2, got %d", summary.CriticalCount)
	}
	if !summary.ActionRequired {
		t.Error("expected action_required=true")
	}

	// No sanitizer warnings expected from clean mock output.
	if warnings, ok := finalTask.Metadata["sanitizer_warnings"]; ok {
		t.Errorf("expected no sanitizer warnings, got: %v", warnings)
	}
}

// ============================================================================
// TEST 8: Planted security issue detection — review mock catches SQL injection
// ============================================================================

func TestMockedEndToEnd_SecurityIssueDetection(t *testing.T) {
	b, st, mocks := buildCodeReviewBroker(t, nil)

	mocks["haiku-parse"].setHandler(func(_ context.Context, _ *broker.Task) (*broker.TaskResult, error) {
		return &broker.TaskResult{Payload: validParseOutput}, nil
	})

	mocks["sonnet-review"].setHandler(func(_ context.Context, _ *broker.Task) (*broker.TaskResult, error) {
		return &broker.TaskResult{Payload: json.RawMessage(`{
			"findings": [
				{
					"file": "internal/api/users.go",
					"severity": "critical",
					"description": "SQL injection: user input interpolated directly into SQL query via fmt.Sprintf",
					"suggestion": "Use parameterized queries: db.QueryRow('SELECT ... WHERE id = $1', id)"
				},
				{
					"file": "internal/api/users.go",
					"severity": "critical",
					"description": "Hardcoded admin API token in source code",
					"suggestion": "Use environment variables or a secrets manager"
				}
			],
			"overall_assessment": "request_changes"
		}`)}, nil
	})

	mocks["haiku-summarize"].setHandler(func(_ context.Context, _ *broker.Task) (*broker.TaskResult, error) {
		return &broker.TaskResult{Payload: json.RawMessage(`{
			"executive_summary": "2 critical security vulnerabilities: SQL injection and hardcoded credentials.",
			"critical_count": 2,
			"high_count": 0,
			"action_required": true,
			"top_recommendations": [
				"Replace string interpolation with parameterized SQL queries",
				"Remove hardcoded admin token and use environment variables"
			]
		}`)}, nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.Run(ctx)

	task, err := b.Submit(ctx, "code-review", loadSamplePayload(t))
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	finalTask := waitForTaskState(t, st, task.ID, broker.TaskStateDone, 10*time.Second)

	var summary struct {
		CriticalCount  int  `json:"critical_count"`
		ActionRequired bool `json:"action_required"`
	}
	if err := json.Unmarshal(finalTask.Payload, &summary); err != nil {
		t.Fatal(err)
	}

	if summary.CriticalCount == 0 {
		t.Error("expected critical_count > 0 for planted SQL injection")
	}
	if !summary.ActionRequired {
		t.Error("expected action_required=true for security findings")
	}
}

// ============================================================================
// TEST 9: Schema violation handling — missing required field in parse output
// ============================================================================

func TestMockedEndToEnd_SchemaViolation(t *testing.T) {
	b, st, mocks := buildCodeReviewBroker(t, nil)

	// Parse stage returns output missing "areas_of_concern".
	mocks["haiku-parse"].setHandler(func(_ context.Context, _ *broker.Task) (*broker.TaskResult, error) {
		return &broker.TaskResult{Payload: json.RawMessage(`{
			"files_changed": ["a.go"],
			"summary": "some changes"
		}`)}, nil
	})

	// These should never be reached.
	mocks["sonnet-review"].setHandler(func(_ context.Context, _ *broker.Task) (*broker.TaskResult, error) {
		t.Error("review stage should not be reached after schema violation")
		return nil, fmt.Errorf("should not be called")
	})
	mocks["haiku-summarize"].setHandler(func(_ context.Context, _ *broker.Task) (*broker.TaskResult, error) {
		t.Error("summarize stage should not be reached after schema violation")
		return nil, fmt.Errorf("should not be called")
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.Run(ctx)

	task, err := b.Submit(ctx, "code-review", loadSamplePayload(t))
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	finalTask := waitForTaskState(t, st, task.ID, broker.TaskStateFailed, 10*time.Second)

	reason, ok := finalTask.Metadata["failure_reason"].(string)
	if !ok {
		t.Fatalf("expected failure_reason in metadata, got: %v", finalTask.Metadata)
	}
	if !strings.Contains(reason, "areas_of_concern") {
		t.Errorf("failure reason should mention 'areas_of_concern', got: %s", reason)
	}
	if !strings.Contains(reason, "output validation") {
		t.Errorf("failure reason should mention 'output validation', got: %s", reason)
	}
}

// ============================================================================
// TEST 10: Retry behavior — review stage fails twice then succeeds
// ============================================================================

func TestMockedEndToEnd_RetryBehavior(t *testing.T) {
	m := metrics.New()
	b, st, mocks := buildCodeReviewBrokerWithRetryOverride(t, m, "review", 4)

	mocks["haiku-parse"].setHandler(func(_ context.Context, _ *broker.Task) (*broker.TaskResult, error) {
		return &broker.TaskResult{Payload: validParseOutput}, nil
	})

	// Review: fail twice with retryable error, then succeed.
	var reviewCalls atomic.Int32
	mocks["sonnet-review"].setHandler(func(_ context.Context, _ *broker.Task) (*broker.TaskResult, error) {
		call := reviewCalls.Add(1)
		if call <= 2 {
			return nil, &agent.AgentError{
				Err:       fmt.Errorf("temporary API error (attempt %d)", call),
				AgentID:   "sonnet-review",
				Prov:      "mock",
				Retryable: true,
			}
		}
		return &broker.TaskResult{Payload: validReviewOutput}, nil
	})

	mocks["haiku-summarize"].setHandler(func(_ context.Context, _ *broker.Task) (*broker.TaskResult, error) {
		return &broker.TaskResult{Payload: validSummarizeOutput}, nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.Run(ctx)

	task, err := b.Submit(ctx, "code-review", loadSamplePayload(t))
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	finalTask := waitForTaskState(t, st, task.ID, broker.TaskStateDone, 15*time.Second)

	if finalTask.State != broker.TaskStateDone {
		t.Fatalf("expected DONE, got %s", finalTask.State)
	}

	// Verify the review agent was called 3 times (2 failures + 1 success).
	if got := reviewCalls.Load(); got != 3 {
		t.Errorf("expected 3 review calls, got %d", got)
	}

	// Verify retry metric incremented by 2.
	retryCounter, err := m.TaskRetriesTotal.GetMetricWithLabelValues("code-review", "review")
	if err != nil {
		t.Fatalf("get retry metric: %v", err)
	}
	var metric dto.Metric
	if err := retryCounter.Write(&metric); err != nil {
		t.Fatalf("write metric: %v", err)
	}
	if got := metric.GetCounter().GetValue(); got != 2 {
		t.Errorf("expected orcastrator_task_retries_total=2 for review stage, got %v", got)
	}
}

// ============================================================================
// TEST 13: Makefile example-code-review target exists
// ============================================================================

func TestMakefileTargetExists(t *testing.T) {
	root := repoRoot(t)
	data, err := os.ReadFile(filepath.Join(root, "Makefile"))
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	if !strings.Contains(string(data), "example-code-review:") {
		t.Error("Makefile missing 'example-code-review' target")
	}
}
