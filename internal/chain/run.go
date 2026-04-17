package chain

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/brianbuquoi/overlord/internal/agent"
	"github.com/brianbuquoi/overlord/internal/agent/registry"
	"github.com/brianbuquoi/overlord/internal/broker"
	"github.com/brianbuquoi/overlord/internal/metrics"
	"github.com/brianbuquoi/overlord/internal/store/memory"
)

// RunResult carries the outcome of a chain run.
type RunResult struct {
	// Task is the terminal task returned by the broker. Callers that
	// need full state (metadata, attempts, etc.) should inspect this
	// directly.
	Task *broker.Task
	// Output is the chain's final payload, extracted according to the
	// chain's declared output.type:
	//   - text: the string at the "text" field of the final stage's
	//           payload, or the raw JSON if no such field exists.
	//   - json: the final stage's payload JSON, verbatim.
	Output string
}

// RunOptions configures a single chain run.
type RunOptions struct {
	// Input is the chain's initial payload. For text input chains it
	// is the raw text; for json input chains it is a JSON object
	// string. Empty is allowed (chains that ignore input can run on
	// {{vars.*}} alone).
	Input string

	// Timeout bounds the time Run waits for the task to reach a
	// terminal state. Zero means "wait forever" — callers should
	// always set a positive value for interactive use.
	Timeout time.Duration

	// Logger is the slog.Logger used by broker and adapters. When
	// nil, slog.Default() is used.
	Logger *slog.Logger
}

// Run compiles ch, builds an in-process broker with a memory store,
// submits a single task, and waits for it to reach a terminal state.
// Run is the programmatic entry point behind `overlord chain run`.
//
// basePath is the directory relative fixture paths resolve against,
// typically the directory containing the chain YAML. Pass empty when
// a chain has no mock-provider steps.
func Run(ctx context.Context, ch *Chain, basePath string, opts RunOptions) (*RunResult, error) {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}

	compiled, err := CompileWithBase(ch, basePath)
	if err != nil {
		return nil, fmt.Errorf("compile chain: %w", err)
	}

	b, err := BuildBroker(compiled, logger, metrics.New())
	if err != nil {
		return nil, err
	}

	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		b.Run(runCtx)
	}()

	payload, err := BuildInitialPayload(ch, opts.Input)
	if err != nil {
		return nil, err
	}

	task, err := b.Submit(ctx, ch.ID, payload)
	if err != nil {
		return nil, fmt.Errorf("submit chain task: %w", err)
	}

	waitCtx := ctx
	if opts.Timeout > 0 {
		var wc context.CancelFunc
		waitCtx, wc = context.WithTimeout(ctx, opts.Timeout)
		defer wc()
	}

	final, err := waitForTerminal(waitCtx, b, task.ID)
	// Stop the broker before returning so goroutines do not leak.
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		// Broker refused to drain — surface via logger and move on;
		// leaked goroutines here would affect only long-lived hosts,
		// not the one-shot `chain run` use case.
		logger.Warn("chain broker did not drain within 5s")
	}

	if err != nil {
		return nil, err
	}

	out := extractFinalOutput(ch, final)
	return &RunResult{Task: final, Output: out}, nil
}

// BuildBroker builds a broker backed by a memory store and chain-
// wrapped agents. Exposed so CLI code and tests can drive the broker
// without going through the full Run() flow.
func BuildBroker(compiled *Compiled, logger *slog.Logger, m *metrics.Metrics) (*broker.Broker, error) {
	baseAgents, err := buildBaseAgents(compiled, logger, m)
	if err != nil {
		return nil, err
	}
	wrapped := wrapAgents(compiled, baseAgents)
	st := memory.New()
	return broker.New(compiled.Config, st, wrapped, compiled.Registry, logger, m, nil), nil
}

// BuildInitialPayload serializes the chain input into the JSON form
// the first stage expects. Text chains wrap the raw input as
// {"text": "..."}; JSON chains validate-and-pass the input as an
// object.
func BuildInitialPayload(ch *Chain, input string) (json.RawMessage, error) {
	switch ch.InputType() {
	case "text":
		if input == "" {
			// An empty text input is legal — a chain that ignores
			// {{input}} (e.g. one driven purely by {{vars.*}}) will
			// still run, but the broker's schema contract requires a
			// string-valued "text" field.
			input = ""
		}
		b, err := json.Marshal(map[string]string{"text": input})
		if err != nil {
			return nil, fmt.Errorf("marshal text input: %w", err)
		}
		return b, nil
	case "json":
		if input == "" {
			return nil, fmt.Errorf("chain declared input.type: json but --input was empty")
		}
		raw := json.RawMessage(input)
		if !json.Valid(raw) {
			return nil, fmt.Errorf("chain input is not valid JSON")
		}
		// Ensure it is an object — open-object schema accepts any
		// object but not arrays/scalars.
		var probe map[string]any
		if err := json.Unmarshal(raw, &probe); err != nil {
			return nil, fmt.Errorf("chain input must be a JSON object (got non-object)")
		}
		return raw, nil
	default:
		return nil, fmt.Errorf("unknown input.type %q", ch.InputType())
	}
}

func buildBaseAgents(compiled *Compiled, logger *slog.Logger, m *metrics.Metrics) (map[string]agent.Agent, error) {
	base := make(map[string]agent.Agent, len(compiled.Config.Agents))
	for _, ac := range compiled.Config.Agents {
		stages := registry.StagesForAgent(compiled.Config.Pipelines, ac.ID)
		a, err := registry.NewFromConfigWithPlugins(ac, nil, logger, compiled.Registry, compiled.BasePath, stages, m)
		if err != nil {
			return nil, fmt.Errorf("build agent %q: %w", ac.ID, err)
		}
		base[ac.ID] = a
	}
	return base, nil
}

func wrapAgents(compiled *Compiled, base map[string]agent.Agent) map[string]broker.Agent {
	wrapped := make(map[string]broker.Agent, len(base))
	// Derive the stepID for each agent by scanning the single pipeline's
	// stages — in chain mode a stage binds to exactly one agent.
	stepByAgent := map[string]string{}
	for _, p := range compiled.Config.Pipelines {
		for _, s := range p.Stages {
			if s.Agent != "" {
				stepByAgent[s.Agent] = s.ID
			}
		}
	}
	inputType := "text"
	if compiled.Chain != nil {
		inputType = compiled.Chain.InputType()
	}
	for id, b := range base {
		stepID := stepByAgent[id]
		wrapped[id] = NewStepAdapter(b, id, stepID, inputType).(broker.Agent)
	}
	return wrapped
}

// waitForTerminal polls the broker's store until the task reaches a
// terminal state or ctx expires.
func waitForTerminal(ctx context.Context, b *broker.Broker, taskID string) (*broker.Task, error) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("chain timed out waiting for terminal state")
		case <-ticker.C:
			task, err := b.GetTask(context.Background(), taskID)
			if err != nil {
				continue
			}
			if task.State.IsTerminal() {
				return task, nil
			}
		}
	}
}

// extractFinalOutput reduces a terminal task's payload to the surface
// text/JSON the CLI prints. text chains unwrap the "text" field;
// json chains return the payload verbatim.
func extractFinalOutput(ch *Chain, task *broker.Task) string {
	if task == nil || len(task.Payload) == 0 {
		return ""
	}
	switch ch.OutputType() {
	case "text":
		return extractTextPayload(task.Payload)
	case "json":
		return string(task.Payload)
	}
	return string(task.Payload)
}

