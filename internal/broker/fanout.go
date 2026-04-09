package broker

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/orcastrator/orcastrator/internal/config"
	"github.com/orcastrator/orcastrator/internal/contract"
	"github.com/orcastrator/orcastrator/internal/metrics"
	"github.com/orcastrator/orcastrator/internal/sanitize"
)

// FanOutAgentResult holds one agent's result within a fan-out execution.
type FanOutAgentResult struct {
	AgentID    string          `json:"agent_id"`
	Output     json.RawMessage `json:"output"`
	DurationMs int64           `json:"duration_ms"`
	Succeeded  bool            `json:"succeeded"`
}

// FanOutAggregate is the aggregate payload passed to the next stage.
type FanOutAggregate struct {
	Results        []FanOutAgentResult `json:"results"`
	SucceededCount int                 `json:"succeeded_count"`
	FailedCount    int                 `json:"failed_count"`
	Mode           string              `json:"mode"`
}

// FanOutExecutor handles execution of a fan-out stage: launching agents in
// parallel, collecting results, enforcing the require policy, and building
// the aggregate payload.
type FanOutExecutor struct {
	agents    map[string]Agent
	agentCfg  map[string]*config.Agent
	validator *contract.Validator
	metrics   *metrics.Metrics
}

// NewFanOutExecutor creates a FanOutExecutor with the given dependencies.
func NewFanOutExecutor(
	agents map[string]Agent,
	agentCfg map[string]*config.Agent,
	validator *contract.Validator,
	m *metrics.Metrics,
) *FanOutExecutor {
	return &FanOutExecutor{
		agents:    agents,
		agentCfg:  agentCfg,
		validator: validator,
		metrics:   m,
	}
}

// Execute runs all agents in the fan-out stage in parallel and returns the
// aggregate payload. It respects the fan-out mode (gather/race) and require
// policy.
func (f *FanOutExecutor) Execute(
	ctx context.Context,
	pipelineID string,
	stage *config.Stage,
	task *Task,
	sanitizedPriorOutput string,
	isFirstStage bool,
) (json.RawMessage, error) {
	fo := stage.FanOut

	// Create a child context with the fan-out timeout.
	fanCtx := ctx
	var fanCancel context.CancelFunc
	if fo.Timeout.Duration > 0 {
		fanCtx, fanCancel = context.WithTimeout(ctx, fo.Timeout.Duration)
	} else if stage.Timeout.Duration > 0 {
		fanCtx, fanCancel = context.WithTimeout(ctx, stage.Timeout.Duration)
	} else {
		fanCtx, fanCancel = context.WithCancel(ctx)
	}
	defer fanCancel()

	type agentResult struct {
		AgentID   string
		Output    json.RawMessage
		Duration  time.Duration
		Succeeded bool
		Err       error
	}

	resultCh := make(chan agentResult, len(fo.Agents))
	var wg sync.WaitGroup

	// Launch one goroutine per agent.
	for _, foAgent := range fo.Agents {
		agentID := foAgent.ID
		ag, ok := f.agents[agentID]
		if !ok {
			resultCh <- agentResult{AgentID: agentID, Err: fmt.Errorf("agent not registered: %s", agentID)}
			continue
		}
		agCfg, ok := f.agentCfg[agentID]
		if !ok {
			resultCh <- agentResult{AgentID: agentID, Err: fmt.Errorf("agent config not found: %s", agentID)}
			continue
		}

		wg.Add(1)
		go func(agentID string, ag Agent, agCfg *config.Agent) {
			defer wg.Done()

			start := time.Now()

			// Build envelope prompt per agent using that agent's system prompt.
			var prompt string
			if isFirstStage {
				prompt = agCfg.SystemPrompt
			} else {
				prompt = sanitize.Wrap(agCfg.SystemPrompt, sanitizedPriorOutput)
			}

			execTask := &Task{
				ID:         task.ID,
				PipelineID: task.PipelineID,
				StageID:    task.StageID,
				Payload:    json.RawMessage(prompt),
				Metadata:   task.Metadata,
				State:      task.State,
			}

			// Recover from agent panics.
			var result *TaskResult
			var execErr error
			func() {
				defer func() {
					if r := recover(); r != nil {
						execErr = fmt.Errorf("agent panicked: %v", r)
					}
				}()
				result, execErr = ag.Execute(fanCtx, execTask)
			}()

			duration := time.Since(start)

			if execErr != nil {
				resultCh <- agentResult{
					AgentID:  agentID,
					Duration: duration,
					Err:      execErr,
				}
				return
			}

			// Validate individual output against output_schema.
			if err := f.validator.ValidateOutput(
				stage.OutputSchema.Name,
				contract.SchemaVersion(task.OutputSchemaVersion),
				contract.SchemaVersion(stage.OutputSchema.Version),
				result.Payload,
			); err != nil {
				resultCh <- agentResult{
					AgentID:  agentID,
					Duration: duration,
					Err:      fmt.Errorf("output validation: %v", err),
				}
				return
			}

			resultCh <- agentResult{
				AgentID:   agentID,
				Output:    result.Payload,
				Duration:  duration,
				Succeeded: true,
			}
		}(agentID, ag, agCfg)
	}

	// Close result channel when all goroutines finish.
	go func() {
		wg.Wait()
		close(resultCh)
	}()

	// Collect results based on mode.
	results := make([]FanOutAgentResult, 0, len(fo.Agents))
	succeededCount := 0
	failedCount := 0
	totalExpected := len(fo.Agents)
	requiredCount := f.RequiredCount(fo.Require, totalExpected)

	for ar := range resultCh {
		far := FanOutAgentResult{
			AgentID:    ar.AgentID,
			DurationMs: ar.Duration.Milliseconds(),
			Succeeded:  ar.Succeeded,
		}
		if ar.Succeeded {
			far.Output = ar.Output
			succeededCount++
		} else {
			failedCount++
		}
		results = append(results, far)

		// Record per-agent metric.
		if f.metrics != nil {
			resultLabel := "success"
			if !ar.Succeeded {
				resultLabel = "failure"
			}
			f.metrics.FanOutAgentResults.WithLabelValues(
				pipelineID, stage.ID, ar.AgentID, resultLabel,
			).Inc()
		}

		// Race mode: cancel remaining once require is satisfied.
		if fo.Mode == config.FanOutModeRace && succeededCount >= requiredCount {
			fanCancel()
			// Drain remaining results from the channel.
			for remaining := range resultCh {
				rr := FanOutAgentResult{
					AgentID:    remaining.AgentID,
					DurationMs: remaining.Duration.Milliseconds(),
					Succeeded:  remaining.Succeeded,
				}
				if remaining.Succeeded {
					rr.Output = remaining.Output
					succeededCount++
				} else {
					failedCount++
				}
				results = append(results, rr)

				if f.metrics != nil {
					resultLabel := "success"
					if !remaining.Succeeded {
						resultLabel = "failure"
					}
					f.metrics.FanOutAgentResults.WithLabelValues(
						pipelineID, stage.ID, remaining.AgentID, resultLabel,
					).Inc()
				}
			}
			break
		}
	}

	// Check require policy.
	if succeededCount < requiredCount {
		if f.metrics != nil {
			f.metrics.FanOutRequirePolicyFailures.WithLabelValues(
				pipelineID, stage.ID, string(fo.Require),
			).Inc()
		}
		return nil, fmt.Errorf(
			"fan-out require policy %q not met: need %d succeeded, got %d (failed: %d)",
			fo.Require, requiredCount, succeededCount, failedCount,
		)
	}

	// Build aggregate payload.
	aggregate := FanOutAggregate{
		Results:        results,
		SucceededCount: succeededCount,
		FailedCount:    failedCount,
		Mode:           string(fo.Mode),
	}

	aggregateJSON, err := json.Marshal(aggregate)
	if err != nil {
		return nil, fmt.Errorf("marshal aggregate: %w", err)
	}

	// Validate aggregate against aggregate_schema.
	if stage.AggregateSchema != nil {
		if err := f.validator.ValidateOutput(
			stage.AggregateSchema.Name,
			contract.SchemaVersion(stage.AggregateSchema.Version),
			contract.SchemaVersion(stage.AggregateSchema.Version),
			aggregateJSON,
		); err != nil {
			return nil, fmt.Errorf("aggregate validation: %v", err)
		}
	}

	return aggregateJSON, nil
}

// RequiredCount returns the number of agents that must succeed for the given
// require policy and total agent count.
func (f *FanOutExecutor) RequiredCount(policy config.RequirePolicy, total int) int {
	switch policy {
	case config.RequirePolicyAll:
		return total
	case config.RequirePolicyAny:
		return 1
	case config.RequirePolicyMajority:
		// Round up: majority of 3 = 2, majority of 4 = 3.
		return total/2 + 1
	default:
		return total
	}
}
