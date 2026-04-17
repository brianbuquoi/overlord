// Package broker implements the core task routing engine. The Broker manages
// pipeline stages, dispatches tasks to agents, enforces I/O contracts, handles
// retries, and coordinates hot-reload of pipeline configuration.
package broker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/brianbuquoi/overlord/internal/budget"
	"github.com/brianbuquoi/overlord/internal/config"
	"github.com/brianbuquoi/overlord/internal/contract"
	"github.com/brianbuquoi/overlord/internal/metrics"
	"github.com/brianbuquoi/overlord/internal/routing"
	"github.com/brianbuquoi/overlord/internal/sanitize"
	"github.com/brianbuquoi/overlord/internal/tracing"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// Agent is the interface the broker uses to execute tasks. This mirrors
// agent.Agent but is defined here to avoid an import cycle (agent imports
// broker for Task types).
type Agent interface {
	ID() string
	Provider() string
	Execute(ctx context.Context, task *Task) (*TaskResult, error)
	HealthCheck(ctx context.Context) error
}

// RetryableError is implemented by errors that indicate whether the operation
// can be retried.
type RetryableError interface {
	error
	IsRetryable() bool
}

// Store is the interface the broker uses for task persistence and queuing.
// This mirrors store.Store but is defined here to avoid an import cycle
// (store imports broker for Task/TaskUpdate types).
type Store interface {
	EnqueueTask(ctx context.Context, stageID string, task *Task) error
	DequeueTask(ctx context.Context, stageID string) (*Task, error)
	// RequeueTask atomically applies update to an existing task and
	// places it back onto stageID's queue. Used by the broker for
	// routing / retry / failure-path requeues where the task row
	// already exists. See internal/store/store.go for the full
	// contract (persistence and queue placement are one operation —
	// success or error; never partial).
	RequeueTask(ctx context.Context, taskID, stageID string, update TaskUpdate) error
	UpdateTask(ctx context.Context, taskID string, update TaskUpdate) error
	GetTask(ctx context.Context, taskID string) (*Task, error)
	ListTasks(ctx context.Context, filter TaskFilter) (*ListTasksResult, error)
	// ClaimForReplay atomically validates that the task is in a replayable state
	// (state = FAILED and RoutedToDeadLetter = true), transitions the task to
	// REPLAY_PENDING, and clears RoutedToDeadLetter to false. This is the claim
	// token — only one concurrent caller can succeed.
	//
	// On success, returns the task in REPLAY_PENDING state.
	// Returns ErrTaskNotFound if the task does not exist.
	// Returns ErrTaskNotReplayable if the task is not in a claimable state
	// (including tasks already in REPLAY_PENDING from a prior concurrent claim).
	//
	// The caller is responsible for either completing the replay (Submit →
	// broker, then UpdateTask to REPLAYED) or rolling back via
	// RollbackReplayClaim if Submit fails.
	ClaimForReplay(ctx context.Context, taskID string) (*Task, error)

	// RollbackReplayClaim atomically transitions a task from REPLAY_PENDING
	// back to FAILED with RoutedToDeadLetter=true, making it visible and
	// replayable again.
	// Returns ErrTaskNotFound if the task does not exist.
	// Returns ErrTaskNotReplayPending if the task is not in REPLAY_PENDING
	// state (it may have already been completed or rolled back by another
	// caller).
	RollbackReplayClaim(ctx context.Context, taskID string) error

	// DiscardDeadLetter atomically transitions a FAILED+dead-lettered task
	// to DISCARDED. The state transition is the claim token so a late
	// discard cannot silently overwrite a concurrent replay's winning
	// state. See internal/store/store.go for the full error taxonomy
	// (ErrTaskNotFound / ErrTaskAlreadyDiscarded / ErrTaskNotDiscardable).
	DiscardDeadLetter(ctx context.Context, taskID string) error

	// CancelTask atomically transitions a non-terminal task to FAILED with
	// a "cancelled by operator" failure_reason. Returns the pre-cancel
	// task snapshot so callers can surface context. See
	// internal/store/store.go for the full error taxonomy
	// (ErrTaskNotFound / ErrTaskAlreadyTerminal).
	CancelTask(ctx context.Context, taskID string) (*Task, error)
}

// ErrQueueEmpty is returned by Store.DequeueTask when no tasks are available.
// This must use the same message as store.ErrQueueEmpty so errors.Is works
// when the store is passed as the Store interface.
var ErrQueueEmpty = errors.New("queue is empty")

// MaxBackoff is the maximum duration for any single retry backoff delay.
// Without a cap, exponential backoff with a 1s base would reach 512s by
// attempt 9, which is unreasonable.
const MaxBackoff = 60 * time.Second

// stageWorkerHandle tracks the cancel function and worker count for a running
// stage worker pool, so Reload can stop removed stages and start new ones.
type stageWorkerHandle struct {
	pipelineID string
	stageID    string
	cancel     context.CancelFunc // cancels only this stage's workers
	count      int                // number of worker goroutines
}

// Broker is the central routing engine for Overlord pipelines.
type Broker struct {
	cfg       *config.Config
	store     Store
	agents    map[string]Agent
	validator *contract.Validator
	logger    *slog.Logger
	eventBus  *EventBus
	metrics   *metrics.Metrics
	tracer    *tracing.Tracer
	budgets   *budget.Tracker

	// sleepFn is called for backoff delays during retry. Defaults to a
	// context-aware sleep. Replaced in tests via SetSleepFunc.
	sleepFn func(ctx context.Context, d time.Duration)

	// mu protects pipelines, stages, and agentCfg from concurrent access.
	// Workers acquire a read lock per iteration; Reload acquires a write lock.
	mu sync.RWMutex

	// Indexes built at construction time.
	pipelines map[string]*config.Pipeline         // pipelineID → pipeline
	stages    map[string]map[string]*config.Stage // pipelineID → stageID → stage
	agentCfg  map[string]*config.Agent            // agentID → agent config

	// taskSpans tracks active root spans for tasks by task ID, so stage
	// processing can create child spans under the correct parent.
	taskSpans map[string]trace.Span

	// runCtx is the context passed to Run(). Stored so Reload() can derive
	// child contexts for new worker pools that still respect the top-level
	// cancellation (e.g. SIGTERM).
	runCtx context.Context

	// wg tracks all worker goroutines (from Run and Reload) so Run can wait
	// for them all before returning.
	wg sync.WaitGroup

	// stageWorkers maps "pipelineID/stageID" → handle for each running
	// worker pool. Protected by mu.
	stageWorkers map[string]*stageWorkerHandle
}

// New creates a Broker from the given config and dependencies.
func New(
	cfg *config.Config,
	st Store,
	agents map[string]Agent,
	registry *contract.Registry,
	logger *slog.Logger,
	m *metrics.Metrics,
	t *tracing.Tracer,
) *Broker {
	b := &Broker{
		cfg:       cfg,
		store:     st,
		agents:    agents,
		validator: contract.NewValidator(registry),
		logger:    logger,
		eventBus:  NewEventBus(),
		metrics:   m,
		tracer:    t,
		budgets:   budget.NewTracker(),
		pipelines: make(map[string]*config.Pipeline, len(cfg.Pipelines)),
		stages:    make(map[string]map[string]*config.Stage),
		agentCfg:  make(map[string]*config.Agent, len(cfg.Agents)),
		taskSpans: make(map[string]trace.Span),
	}

	for i := range cfg.Pipelines {
		p := &cfg.Pipelines[i]
		b.pipelines[p.Name] = p
		b.stages[p.Name] = make(map[string]*config.Stage, len(p.Stages))
		for j := range p.Stages {
			s := &p.Stages[j]
			b.stages[p.Name][s.ID] = s
		}
	}
	for i := range cfg.Agents {
		a := &cfg.Agents[i]
		b.agentCfg[a.ID] = a
	}

	b.stageWorkers = make(map[string]*stageWorkerHandle)

	b.sleepFn = func(ctx context.Context, d time.Duration) {
		select {
		case <-ctx.Done():
		case <-time.After(d):
		}
	}

	return b
}

// SetSleepFunc replaces the function used for backoff delays during retry.
// Intended for testing — inject a function that records durations instead of
// actually sleeping.
func (b *Broker) SetSleepFunc(fn func(context.Context, time.Duration)) {
	b.sleepFn = fn
}

// EventBus returns the broker's event bus for subscribing to task events.
func (b *Broker) EventBus() *EventBus { return b.eventBus }

// Store returns the broker's task store.
func (b *Broker) Store() Store { return b.store }

// Agents returns the broker's registered agents.
func (b *Broker) Agents() map[string]Agent { return b.agents }

// Config returns the broker's config.
func (b *Broker) Config() *config.Config { return b.cfg }

// Metrics returns the broker's metrics collector (may be nil).
func (b *Broker) Metrics() *metrics.Metrics { return b.metrics }

// Tracer returns the broker's OpenTelemetry tracer (may be nil).
func (b *Broker) Tracer() *tracing.Tracer { return b.tracer }

// Budgets returns the broker's retry budget tracker.
func (b *Broker) Budgets() *budget.Tracker { return b.budgets }

// stageKey returns the map key used in stageWorkers.
func stageKey(pipelineID, stageID string) string {
	return pipelineID + "/" + stageID
}

// startStageWorkers launches a worker pool for a single stage. It creates a
// child context derived from parentCtx so that cancelling parentCtx stops
// these workers, but the returned cancel can also stop them independently
// (used when a stage is removed via Reload).
func (b *Broker) startStageWorkers(parentCtx context.Context, pipelineID, stageID string, concurrency int) *stageWorkerHandle {
	stageCtx, cancel := context.WithCancel(parentCtx)
	for w := 0; w < concurrency; w++ {
		b.wg.Add(1)
		go func() {
			defer b.wg.Done()
			b.workerLoop(stageCtx, pipelineID, stageID)
		}()
	}
	return &stageWorkerHandle{
		pipelineID: pipelineID,
		stageID:    stageID,
		cancel:     cancel,
		count:      concurrency,
	}
}

// Reload updates the broker's config, pipeline indexes, agent configs, and
// contract validator. It also diffs the old and new stage sets:
//   - New stages get a fresh worker pool.
//   - Removed stages have their workers signalled to drain and stop (in-flight
//     tasks finish before the worker exits).
//   - Updated stages (present in both) get their config swapped under the lock.
//
// This method is safe to call from a hot-reload callback while Run is active.
func (b *Broker) Reload(cfg *config.Config, agents map[string]Agent, validator *contract.Validator) {
	pipelines := make(map[string]*config.Pipeline, len(cfg.Pipelines))
	stages := make(map[string]map[string]*config.Stage)
	agentCfg := make(map[string]*config.Agent, len(cfg.Agents))

	// Build the new stage key set and a concurrency lookup.
	newStageKeys := make(map[string]struct{})
	newConcurrency := make(map[string]int)
	newStagePipeline := make(map[string]string) // key → pipelineID
	newStageID := make(map[string]string)       // key → stageID

	for i := range cfg.Pipelines {
		p := &cfg.Pipelines[i]
		pipelines[p.Name] = p
		stages[p.Name] = make(map[string]*config.Stage, len(p.Stages))
		conc := p.Concurrency
		if conc < 1 {
			conc = 1
		}
		for j := range p.Stages {
			s := &p.Stages[j]
			stages[p.Name][s.ID] = s
			key := stageKey(p.Name, s.ID)
			newStageKeys[key] = struct{}{}
			newConcurrency[key] = conc
			newStagePipeline[key] = p.Name
			newStageID[key] = s.ID
		}
	}
	for i := range cfg.Agents {
		a := &cfg.Agents[i]
		agentCfg[a.ID] = a
	}

	b.mu.Lock()

	// Identify removed stages (in old, not in new) and stop their workers.
	for key, handle := range b.stageWorkers {
		if _, exists := newStageKeys[key]; !exists {
			b.logger.Info("stopping workers for removed stage",
				"pipeline", handle.pipelineID, "stage", handle.stageID,
			)
			handle.cancel()
			delete(b.stageWorkers, key)
		}
	}

	// Swap config state.
	b.cfg = cfg
	b.agents = agents
	b.validator = validator
	b.pipelines = pipelines
	b.stages = stages
	b.agentCfg = agentCfg

	// Identify new stages (in new, not in old) and start worker pools.
	// runCtx is nil if Run() hasn't been called yet — skip starting workers.
	if b.runCtx != nil {
		for key := range newStageKeys {
			if _, exists := b.stageWorkers[key]; !exists {
				pID := newStagePipeline[key]
				sID := newStageID[key]
				conc := newConcurrency[key]
				b.logger.Info("starting workers for new stage",
					"pipeline", pID, "stage", sID, "concurrency", conc,
				)
				b.stageWorkers[key] = b.startStageWorkers(b.runCtx, pID, sID, conc)
			}
		}
	}

	b.mu.Unlock()
}

// Run starts worker pools for every stage in every pipeline and blocks until
// ctx is cancelled. After cancellation it waits for all in-flight tasks
// (including those started by Reload) to finish before returning.
func (b *Broker) Run(ctx context.Context) error {
	b.mu.Lock()
	b.runCtx = ctx

	for _, p := range b.cfg.Pipelines {
		for _, s := range p.Stages {
			concurrency := p.Concurrency
			if concurrency < 1 {
				concurrency = 1
			}
			key := stageKey(p.Name, s.ID)
			b.stageWorkers[key] = b.startStageWorkers(ctx, p.Name, s.ID, concurrency)
		}
	}
	b.mu.Unlock()

	<-ctx.Done()
	b.wg.Wait()
	return ctx.Err()
}

// SubmitWithCarrier creates a new task, optionally extracting W3C traceparent
// from the carrier for trace propagation.
func (b *Broker) SubmitWithCarrier(ctx context.Context, pipelineID string, payload json.RawMessage, carrier propagation.TextMapCarrier) (*Task, error) {
	if b.tracer != nil && carrier != nil {
		ctx = b.tracer.Extract(ctx, carrier)
	}
	return b.Submit(ctx, pipelineID, payload)
}

// Submit creates a new task and enqueues it to the first stage of the named pipeline.
func (b *Broker) Submit(ctx context.Context, pipelineID string, payload json.RawMessage) (*Task, error) {
	b.mu.RLock()
	p, ok := b.pipelines[pipelineID]
	b.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("pipeline not found: %s", pipelineID)
	}
	if len(p.Stages) == 0 {
		return nil, fmt.Errorf("pipeline %s has no stages", pipelineID)
	}

	first := &p.Stages[0]
	now := time.Now()
	task := &Task{
		ID:                  uuid.New().String(),
		PipelineID:          pipelineID,
		StageID:             first.ID,
		InputSchemaName:     first.InputSchema.Name,
		InputSchemaVersion:  first.InputSchema.Version,
		OutputSchemaName:    first.OutputSchema.Name,
		OutputSchemaVersion: first.OutputSchema.Version,
		Payload:             payload,
		Metadata:            make(map[string]any),
		State:               TaskStatePending,
		Attempts:            0,
		MaxAttempts:         first.Retry.MaxAttempts,
		CreatedAt:           now,
		UpdatedAt:           now,
	}

	// Start root trace span for this task.
	if b.tracer != nil {
		var span trace.Span
		ctx, span = b.tracer.StartTaskSpan(ctx, task.ID, pipelineID)
		b.mu.Lock()
		b.taskSpans[task.ID] = span
		b.mu.Unlock()
		if meta := tracing.SpanMeta(ctx); meta != nil {
			for k, v := range meta {
				task.Metadata[k] = v
			}
		}
	}

	if err := b.store.EnqueueTask(ctx, first.ID, task); err != nil {
		return nil, fmt.Errorf("enqueue task: %w", err)
	}

	if b.metrics != nil {
		b.metrics.QueueDepth.WithLabelValues(pipelineID, first.ID).Inc()
	}

	b.logger.Info("task submitted",
		"task_id", task.ID,
		"pipeline", pipelineID,
		"stage", first.ID,
		"trace_id", task.Metadata["trace_id"],
	)
	return task, nil
}

// GetTask retrieves a task by ID from the store.
func (b *Broker) GetTask(ctx context.Context, taskID string) (*Task, error) {
	return b.store.GetTask(ctx, taskID)
}

// workerLoop is the per-stage worker goroutine. It dequeues tasks and
// processes them until ctx is cancelled. The stage config is looked up
// fresh on each iteration so that hot-reloads take effect immediately.
func (b *Broker) workerLoop(ctx context.Context, pipelineID string, stageID string) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		task, err := b.store.DequeueTask(ctx, stageID)
		if err == nil && b.metrics != nil {
			b.metrics.QueueDepth.WithLabelValues(pipelineID, stageID).Dec()
		}
		if err != nil {
			if errors.Is(err, ErrQueueEmpty) {
				// Back off briefly to avoid busy-spinning on an empty queue.
				select {
				case <-ctx.Done():
					return
				case <-time.After(50 * time.Millisecond):
				}
				continue
			}
			b.logger.Error("dequeue failed", "stage", stageID, "error", err)
			continue
		}

		// Snapshot config under read lock — may have changed via Reload.
		b.mu.RLock()
		stageMap := b.stages[pipelineID]
		var stage *config.Stage
		if stageMap != nil {
			stage = stageMap[stageID]
		}
		b.mu.RUnlock()

		if stage == nil {
			b.logger.Error("stage not found after dequeue", "pipeline", pipelineID, "stage", stageID)
			continue
		}

		b.processTask(ctx, pipelineID, stage, task)
	}
}

// processTask handles a single task through the full stage pipeline:
// version check → input validation → sanitize → execute → output validation → route.
func (b *Broker) processTask(ctx context.Context, pipelineID string, stage *config.Stage, task *Task) {
	// Fan-out stages are handled by a separate code path.
	if stage.FanOut != nil {
		b.processFanOutTask(ctx, pipelineID, stage, task)
		return
	}

	// Snapshot mutable broker state under read lock for the duration of this task.
	b.mu.RLock()
	validator := b.validator
	agentCfg, agentCfgOK := b.agentCfg[stage.Agent]
	p := b.pipelines[pipelineID]
	ag, agOK := b.agents[stage.Agent]
	parentSpan := b.taskSpans[task.ID]
	b.mu.RUnlock()

	// Reconstruct trace context from the root task span if available.
	if parentSpan != nil {
		ctx = trace.ContextWithSpan(ctx, parentSpan)
	}

	// Start stage span.
	var stageSpan trace.Span
	if b.tracer != nil {
		agentID := stage.Agent
		ctx, stageSpan = b.tracer.StartStageSpan(ctx, stage.ID, agentID, task.Attempts)
		defer stageSpan.End()
	}

	b.transition(ctx, task, TaskStateRouting)

	// Track stage visits for observability and debugging loopback routes.
	if task.Metadata == nil {
		task.Metadata = make(map[string]any)
	}
	history, _ := task.Metadata["stage_history"].([]any)
	history = append(history, stage.ID)
	b.mergeMetadata(ctx, task, map[string]any{"stage_history": history})

	// Version compatibility is enforced at routing time in routeSuccess,
	// before the task's schema fields are updated to the next stage.
	// By the time a task is dequeued here, the versions already match.

	// --- Input contract validation ---
	if err := validator.ValidateInput(
		stage.InputSchema.Name,
		contract.SchemaVersion(task.InputSchemaVersion),
		contract.SchemaVersion(stage.InputSchema.Version),
		task.Payload,
	); err != nil {
		b.logger.Warn("input validation failed",
			"task_id", task.ID, "stage", stage.ID, "error", err,
		)
		b.failTask(ctx, pipelineID, stage, task, fmt.Sprintf("input validation: %v", err))
		return
	}

	// --- Sanitize prior output ---
	sanitizedOutput, warnings := sanitize.Sanitize(string(task.Payload))
	if len(warnings) > 0 {
		b.logger.Warn("sanitizer warnings",
			"task_id", task.ID, "stage", stage.ID, "count", len(warnings),
		)
		b.logger.Debug("sanitizer redacted content",
			"task_id", task.ID,
			"stage", stage.ID,
			"warning_count", len(warnings),
			"patterns_matched", warningPatterns(warnings),
		)
		b.mergeMetadata(ctx, task, map[string]any{
			"sanitizer_warnings": sanitize.WarningsToJSON(warnings),
		})
		if b.metrics != nil {
			for _, w := range warnings {
				b.metrics.SanitizerRedactions.WithLabelValues(pipelineID, stage.ID, w.Pattern).Inc()
			}
		}
	}

	// --- Build envelope prompt ---
	ok := agentCfgOK

	if !ok {
		b.failTask(ctx, pipelineID, stage, task, fmt.Sprintf("agent not found: %s", stage.Agent))
		return
	}
	// First stage has no prior agent output — deliver only the system prompt
	// without envelope wrapping. The envelope pattern protects against prompt
	// injection from prior AGENT output; the first stage's payload is
	// validated user input, not agent output.
	isFirstStage := len(p.Stages) > 0 && p.Stages[0].ID == stage.ID
	var prompt string
	if isFirstStage {
		prompt = agentCfg.SystemPrompt
	} else {
		prompt = sanitize.Wrap(agentCfg.SystemPrompt, sanitizedOutput)
	}

	b.logger.Debug("envelope built",
		"task_id", task.ID,
		"stage", stage.ID,
		"is_first_stage", isFirstStage,
		"prior_output_length", len(sanitizedOutput),
		"prompt_length", len(prompt),
		"sanitizer_warnings", len(warnings),
	)

	// --- Execute agent ---
	b.transition(ctx, task, TaskStateExecuting)

	if !agOK {
		b.failTask(ctx, pipelineID, stage, task, fmt.Sprintf("agent not registered: %s", stage.Agent))
		return
	}

	// Apply per-stage timeout if configured.
	execCtx := ctx
	if stage.Timeout.Duration > 0 {
		var cancel context.CancelFunc
		execCtx, cancel = context.WithTimeout(ctx, stage.Timeout.Duration)
		defer cancel()
	}

	execTask := &Task{
		ID:         task.ID,
		PipelineID: task.PipelineID,
		StageID:    task.StageID,
		Payload:    task.Payload,
		Prompt:     prompt,
		Metadata:   task.Metadata,
		State:      task.State,
	}

	// Start agent span for tracing.
	if b.tracer != nil {
		var agSpan trace.Span
		execCtx, agSpan = b.tracer.StartAgentSpan(execCtx, ag.Provider(), agentCfg.Model)
		defer agSpan.End()
	}

	// Recover from agent panics — a panicking agent must not crash the worker
	// goroutine or the broker. Treated as a non-retryable error.
	var result *TaskResult
	var execErr error
	func() {
		defer func() {
			if r := recover(); r != nil {
				execErr = fmt.Errorf("agent panicked: %v", r)
			}
		}()
		result, execErr = ag.Execute(execCtx, execTask)
	}()

	if execErr != nil {
		b.handleAgentError(ctx, pipelineID, stage, task, execErr)
		return
	}

	// --- Output validation (defense-in-depth) ---
	// Scan the model's response for instruction-like patterns that may
	// indicate a successful prompt injection slipped past the input
	// sanitizer. Warnings are recorded but do not fail the task — this
	// mirrors the input-sanitizer policy of attach-and-continue.
	if outWarnings := sanitize.ValidateOutput(string(result.Payload)); len(outWarnings) > 0 {
		b.logger.Warn("sanitizer output warnings",
			"task_id", task.ID, "stage", stage.ID, "count", len(outWarnings),
		)
		b.mergeMetadata(ctx, task, map[string]any{
			"sanitizer_output_warnings": sanitize.WarningsToJSON(outWarnings),
		})
		if b.metrics != nil {
			for _, w := range outWarnings {
				b.metrics.SanitizerRedactions.WithLabelValues(pipelineID, stage.ID, w.Pattern).Inc()
			}
		}
	}

	// --- Output contract validation ---
	b.transition(ctx, task, TaskStateValidating)

	if err := validator.ValidateOutput(
		stage.OutputSchema.Name,
		contract.SchemaVersion(task.OutputSchemaVersion),
		contract.SchemaVersion(stage.OutputSchema.Version),
		result.Payload,
	); err != nil {
		b.logger.Warn("output validation failed",
			"task_id", task.ID, "stage", stage.ID, "error", err,
		)
		b.failTask(ctx, pipelineID, stage, task, fmt.Sprintf("output validation: %v", err))
		return
	}

	// Merge any result metadata, filtering out broker-reserved keys.
	// Agents must not be able to overwrite security-relevant metadata
	// set by the broker (e.g. sanitizer warnings, failure reasons).
	if result.Metadata != nil {
		filtered := filterReservedMetadata(result.Metadata)
		if len(filtered) > 0 {
			b.mergeMetadata(ctx, task, filtered)
		}
	}

	// --- Route to next stage ---
	b.routeSuccess(ctx, pipelineID, stage, task, result.Payload)
}

// processFanOutTask handles a fan-out stage: version check, input validation,
// sanitize once, then delegate to FanOutExecutor for parallel agent execution.
func (b *Broker) processFanOutTask(ctx context.Context, pipelineID string, stage *config.Stage, task *Task) {
	b.mu.RLock()
	validator := b.validator
	agents := b.agents
	agentCfg := b.agentCfg
	p := b.pipelines[pipelineID]
	parentSpan := b.taskSpans[task.ID]
	m := b.metrics
	b.mu.RUnlock()

	if parentSpan != nil {
		ctx = trace.ContextWithSpan(ctx, parentSpan)
	}

	var stageSpan trace.Span
	if b.tracer != nil {
		ctx, stageSpan = b.tracer.StartStageSpan(ctx, stage.ID, "fan-out", task.Attempts)
		defer stageSpan.End()
	}

	b.transition(ctx, task, TaskStateRouting)

	if task.Metadata == nil {
		task.Metadata = make(map[string]any)
	}
	history, _ := task.Metadata["stage_history"].([]any)
	history = append(history, stage.ID)
	b.mergeMetadata(ctx, task, map[string]any{"stage_history": history})

	// Version compatibility is enforced at routing time in routeSuccess,
	// before the task's schema fields are updated to the next stage.
	// By the time a task is dequeued here, the versions already match.

	// --- Input contract validation ---
	if err := validator.ValidateInput(
		stage.InputSchema.Name,
		contract.SchemaVersion(task.InputSchemaVersion),
		contract.SchemaVersion(stage.InputSchema.Version),
		task.Payload,
	); err != nil {
		b.failTask(ctx, pipelineID, stage, task, fmt.Sprintf("input validation: %v", err))
		return
	}

	// --- Sanitize prior output (once) ---
	sanitizedOutput, warnings := sanitize.Sanitize(string(task.Payload))
	if len(warnings) > 0 {
		b.mergeMetadata(ctx, task, map[string]any{
			"sanitizer_warnings": sanitize.WarningsToJSON(warnings),
		})
		if m != nil {
			for _, w := range warnings {
				m.SanitizerRedactions.WithLabelValues(pipelineID, stage.ID, w.Pattern).Inc()
			}
		}
	}

	isFirstStage := len(p.Stages) > 0 && p.Stages[0].ID == stage.ID

	// --- Execute fan-out ---
	b.transition(ctx, task, TaskStateExecuting)

	executor := NewFanOutExecutor(agents, agentCfg, validator, m)
	aggregatePayload, err := executor.Execute(ctx, pipelineID, stage, task, sanitizedOutput, isFirstStage)
	if err != nil {
		b.failTask(ctx, pipelineID, stage, task, fmt.Sprintf("fan-out: %v", err))
		return
	}

	// --- Validate aggregate ---
	b.transition(ctx, task, TaskStateValidating)

	// --- Route to next stage ---
	b.routeSuccess(ctx, pipelineID, stage, task, aggregatePayload)
}

// handleAgentError inspects the error for retryability and either retries or
// routes to the failure path.
func (b *Broker) handleAgentError(ctx context.Context, pipelineID string, stage *config.Stage, task *Task, err error) {
	var retryable RetryableError
	if errors.As(err, &retryable) && retryable.IsRetryable() {
		task.Attempts++
		if task.Attempts < stage.Retry.MaxAttempts {
			// Atomically check+increment both budgets before allowing the retry.
			exhaustedKey, exhaustedBudget := b.tryAcquireBudgets(ctx, pipelineID, stage, task)
			if exhaustedBudget != nil {
				if exhaustedBudget.OnExhausted == "wait" {
					b.waitForBudget(ctx, pipelineID, stage, task)
					return
				}
				b.logger.Warn("retry budget exhausted",
					"task_id", task.ID, "budget_key", exhaustedKey,
				)
				b.failTask(ctx, pipelineID, stage, task, "retry_budget_exhausted")
				return
			}

			b.logger.Info("retrying task",
				"task_id", task.ID, "stage", stage.ID,
				"attempt", task.Attempts, "max", stage.Retry.MaxAttempts,
			)
			b.retryTask(ctx, stage, task)
			return
		}
		b.logger.Warn("max retries exceeded",
			"task_id", task.ID, "stage", stage.ID, "attempts", task.Attempts,
		)
	} else {
		b.logger.Error("agent execution failed (non-retryable)",
			"task_id", task.ID, "stage", stage.ID, "error", err,
		)
	}
	b.failTask(ctx, pipelineID, stage, task, fmt.Sprintf("agent error: %v", err))
}

// budgetConfigs returns the pipeline and agent budget configs for a stage.
func (b *Broker) budgetConfigs(pipelineID string, stage *config.Stage) (pipelineBudget *budget.Budget, agentBudget *budget.Budget, agentID string) {
	b.mu.RLock()
	p := b.pipelines[pipelineID]
	agentID = stage.Agent
	var agentCfg *config.Agent
	if agentID != "" {
		agentCfg = b.agentCfg[agentID]
	}
	b.mu.RUnlock()

	if p != nil {
		pipelineBudget = budget.FromConfig(p.RetryBudget)
	}
	if agentCfg != nil {
		agentBudget = budget.FromConfig(agentCfg.RetryBudget)
	}
	return
}

// tryAcquireBudgets atomically checks and increments both pipeline and agent
// budgets. If either is exhausted, no counters are incremented and the
// exhausted key+budget are returned. If both have capacity, both are
// incremented and ("", nil) is returned.
func (b *Broker) tryAcquireBudgets(ctx context.Context, pipelineID string, stage *config.Stage, task *Task) (string, *budget.Budget) {
	pb, ab, agentID := b.budgetConfigs(pipelineID, stage)

	// Try pipeline budget.
	if pb != nil {
		pKey := budget.PipelineKey(pipelineID)
		allowed, _, _ := b.budgets.IncrementIfNotExhausted(ctx, pKey, pb)
		if !allowed {
			b.logger.Warn("pipeline retry budget exhausted",
				"task_id", task.ID, "pipeline", pipelineID, "budget_key", pKey,
			)
			if b.metrics != nil {
				b.metrics.RetryBudgetExhaustionsTotal.WithLabelValues(pipelineID, agentID, "pipeline").Inc()
			}
			return pKey, pb
		}
	}

	// Try agent budget.
	if ab != nil {
		aKey := budget.AgentKey(agentID)
		allowed, _, _ := b.budgets.IncrementIfNotExhausted(ctx, aKey, ab)
		if !allowed {
			// Roll back the pipeline increment we just did.
			if pb != nil {
				b.budgets.Decrement(ctx, budget.PipelineKey(pipelineID))
			}
			b.logger.Warn("agent retry budget exhausted",
				"task_id", task.ID, "agent", agentID, "budget_key", aKey,
			)
			if b.metrics != nil {
				b.metrics.RetryBudgetExhaustionsTotal.WithLabelValues(pipelineID, agentID, "agent").Inc()
			}
			return aKey, ab
		}
	}

	return "", nil
}

// waitForBudget holds a task in WAITING state until both budgets have capacity,
// then atomically acquires them and retries. Re-checks every 30s or the
// shortest budget window, whichever is shorter.
func (b *Broker) waitForBudget(ctx context.Context, pipelineID string, stage *config.Stage, task *Task) {
	b.transition(ctx, task, TaskStateWaiting)

	pb, ab, _ := b.budgetConfigs(pipelineID, stage)

	// Determine check interval from the shortest window.
	checkInterval := 30 * time.Second
	if pb != nil && pb.Window < checkInterval {
		checkInterval = pb.Window
	}
	if ab != nil && ab.Window < checkInterval {
		checkInterval = ab.Window
	}

	b.logger.Info("task waiting for budget refill",
		"task_id", task.ID, "check_interval", checkInterval,
	)

	ticker := time.NewTicker(checkInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			b.failTask(ctx, pipelineID, stage, task, "context cancelled while waiting for budget")
			return
		case <-ticker.C:
			// Re-check BOTH budgets atomically.
			exhaustedKey, exhaustedBudget := b.tryAcquireBudgets(ctx, pipelineID, stage, task)
			if exhaustedBudget == nil {
				b.logger.Info("budget refilled, retrying task",
					"task_id", task.ID,
				)
				b.retryTask(ctx, stage, task)
				return
			}
			// If the blocking budget's policy is "fail", stop waiting.
			if exhaustedBudget.OnExhausted == "fail" {
				b.logger.Warn("budget exhausted with on_exhausted=fail, giving up wait",
					"task_id", task.ID, "budget_key", exhaustedKey,
				)
				b.failTask(ctx, pipelineID, stage, task, "retry_budget_exhausted")
				return
			}
			b.logger.Debug("budget still exhausted",
				"task_id", task.ID, "budget_key", exhaustedKey,
			)
		}
	}
}

// retryTask applies backoff with jitter and re-enqueues the task.
// Persistence is fail-closed: a store error during the RETRYING
// transition or the final requeue leaves the task visible in the
// store for redelivery and increments broker_store_errors_total
// instead of emitting a success event.
//
// The state-writing store calls use a bounded persistence context
// derived from Background() (not the worker ctx). Shutdown cancels
// the worker ctx, but the RETRYING→PENDING bookkeeping must still
// land so a task mid-backoff is not stranded in RETRYING forever.
// The intermediate sleep keeps honoring the worker ctx so shutdown
// still aborts the backoff wait — only the durable writes are
// isolated from caller cancel.
func (b *Broker) retryTask(ctx context.Context, stage *config.Stage, task *Task) {
	if b.metrics != nil {
		b.metrics.TaskRetriesTotal.WithLabelValues(task.PipelineID, stage.ID).Inc()
	}

	delay := b.calculateBackoff(stage.Retry, task.Attempts-1)

	retrying := TaskStateRetrying
	attempts := task.Attempts
	upCtx, upCancel := b.persistCtx()
	err := b.store.UpdateTask(upCtx, task.ID, TaskUpdate{
		State:    &retrying,
		Attempts: &attempts,
	})
	upCancel()
	if err != nil {
		b.recordStoreError("update_task", task.PipelineID, stage.ID, err)
		return
	}

	b.logger.Info("backoff before retry",
		"task_id", task.ID, "stage", stage.ID, "delay", delay,
	)

	b.sleepFn(ctx, delay)

	pending := TaskStatePending
	rqCtx, rqCancel := b.persistCtx()
	err = b.store.RequeueTask(rqCtx, task.ID, stage.ID, TaskUpdate{
		State:    &pending,
		Attempts: &attempts,
	})
	rqCancel()
	if err != nil {
		b.recordStoreError("requeue_task", task.PipelineID, stage.ID, err)
		return
	}
	if b.metrics != nil {
		b.metrics.QueueDepth.WithLabelValues(task.PipelineID, stage.ID).Inc()
	}
}

// calculateBackoff returns the backoff duration with ±10% jitter.
func (b *Broker) calculateBackoff(policy config.RetryPolicy, attempt int) time.Duration {
	base := policy.BaseDelay.Duration
	if base <= 0 {
		base = time.Second
	}

	var delay time.Duration
	switch policy.Backoff {
	case "exponential":
		delay = base * (1 << uint(attempt))
	case "linear":
		delay = base * time.Duration(attempt+1)
	default: // "fixed" or unspecified
		delay = base
	}

	// Cap to prevent unreasonably long delays. Without this, exponential
	// backoff with a 1s base reaches 512s by attempt 9.
	if delay > MaxBackoff {
		delay = MaxBackoff
	}

	// Apply ±10% jitter.
	jitter := float64(delay) * 0.1 * (2*rand.Float64() - 1)
	delay = time.Duration(float64(delay) + jitter)
	if delay < 0 {
		delay = 0
	}
	return delay
}

// maxStageTransitions returns the configured transition limit for a pipeline,
// defaulting to config.DefaultMaxStageTransitions.
func (b *Broker) maxStageTransitions(pipelineID string) int {
	b.mu.RLock()
	p := b.pipelines[pipelineID]
	b.mu.RUnlock()
	if p != nil && p.MaxStageTransitions > 0 {
		return p.MaxStageTransitions
	}
	return config.DefaultMaxStageTransitions
}

// checkTransitionLimit increments CrossStageTransitions and dead-letters the
// task if the limit is exceeded. Returns true if the task was dead-lettered.
// Terminal event + dead-letter metric are only emitted after the store
// UpdateTask succeeds — a persistence failure returns true (treat as
// dead-lettered from the caller's perspective — the caller's own routing
// path must not proceed) but does not publish the event, so downstream
// WebSocket / metric consumers only see terminal state when the durable
// record matches.
func (b *Broker) checkTransitionLimit(ctx context.Context, pipelineID string, stage *config.Stage, task *Task) bool {
	task.CrossStageTransitions++
	limit := b.maxStageTransitions(pipelineID)
	if task.CrossStageTransitions > limit {
		b.mergeMetadata(ctx, task, map[string]any{
			"failure_reason": "max_stage_transitions_exceeded",
			"transitions":    task.CrossStageTransitions - 1,
		})
		from := task.State
		state := TaskStateFailed
		deadLetter := true
		transitions := task.CrossStageTransitions
		if err := b.store.UpdateTask(ctx, task.ID, TaskUpdate{
			State:                 &state,
			RoutedToDeadLetter:    &deadLetter,
			CrossStageTransitions: &transitions,
		}); err != nil {
			b.recordStoreError("update_task", pipelineID, stage.ID, err)
			return true
		}
		b.logger.Warn("task exceeded max stage transitions",
			"task_id", task.ID, "pipeline", pipelineID,
			"transitions", task.CrossStageTransitions-1, "limit", limit,
		)
		b.eventBus.Publish(TaskEvent{
			Event:      "state_change",
			TaskID:     task.ID,
			PipelineID: pipelineID,
			StageID:    stage.ID,
			From:       from,
			To:         TaskStateFailed,
			Timestamp:  time.Now(),
		})
		if b.metrics != nil {
			b.metrics.TasksDeadLettered.WithLabelValues(pipelineID, stage.ID).Inc()
		}
		b.recordTerminalMetrics(task, pipelineID, stage.ID, "FAILED")
		return true
	}
	return false
}

// routeSuccess sends the task to the next stage or marks it done.
// Persistence is fail-closed throughout: the broker does not publish
// terminal events or record success metrics until the store write
// succeeds. On store error the broker increments
// broker_store_errors_total, logs, and returns — the task stays
// visible for redelivery.
func (b *Broker) routeSuccess(ctx context.Context, pipelineID string, stage *config.Stage, task *Task, outputPayload json.RawMessage) {
	next, err := routing.Resolve(stage.OnSuccess.RouteConfig, outputPayload)
	if err != nil {
		b.logger.Error("conditional routing failed",
			"task_id", task.ID, "stage", stage.ID, "error", err,
		)
		b.failTask(ctx, pipelineID, stage, task, fmt.Sprintf("routing error: %v", err))
		return
	}
	if next == "done" || next == "" {
		from := task.State
		state := TaskStateDone
		if err := b.store.UpdateTask(ctx, task.ID, TaskUpdate{
			State:   &state,
			Payload: &outputPayload,
		}); err != nil {
			b.recordStoreError("update_task", pipelineID, stage.ID, err)
			return
		}
		b.logger.Info("task completed",
			"task_id", task.ID, "pipeline", pipelineID, "stage", stage.ID,
		)
		b.eventBus.Publish(TaskEvent{
			Event:      "state_change",
			TaskID:     task.ID,
			PipelineID: pipelineID,
			StageID:    stage.ID,
			From:       from,
			To:         TaskStateDone,
			Timestamp:  time.Now(),
		})
		b.recordTerminalMetrics(task, pipelineID, stage.ID, "DONE")
		return
	}

	// Check cross-stage transition limit before routing.
	if b.checkTransitionLimit(ctx, pipelineID, stage, task) {
		return
	}

	b.mu.RLock()
	nextStage, ok := b.stages[pipelineID][next]
	b.mu.RUnlock()
	if !ok {
		b.logger.Error("next stage not found", "task_id", task.ID, "next_stage", next)
		state := TaskStateFailed
		if err := b.store.UpdateTask(ctx, task.ID, TaskUpdate{State: &state}); err != nil {
			b.recordStoreError("update_task", pipelineID, stage.ID, err)
		}
		return
	}

	// Version compatibility check — must happen BEFORE updating task schema
	// fields. Compare what this stage just produced against what the next
	// stage expects. After the UpdateTask below, task.OutputSchemaVersion
	// will be overwritten and this check would always pass.
	compatible, compatErr := contract.IsCompatible(
		contract.SchemaVersion(task.OutputSchemaVersion),
		contract.SchemaVersion(nextStage.InputSchema.Version),
	)
	if compatErr != nil || !compatible {
		msg := fmt.Sprintf(
			"version mismatch: stage %q outputs %s@%s but stage %q expects %s@%s",
			task.StageID, task.OutputSchemaName, task.OutputSchemaVersion,
			next, nextStage.InputSchema.Name, nextStage.InputSchema.Version,
		)
		b.logger.Warn("routing version mismatch",
			"task_id", task.ID,
			"from_stage", task.StageID,
			"to_stage", next,
			"output_version", task.OutputSchemaVersion,
			"expected_version", nextStage.InputSchema.Version,
		)
		b.mergeMetadata(ctx, task, map[string]any{
			"version_mismatch": map[string]any{
				"from_stage":       task.StageID,
				"to_stage":         next,
				"output_schema":    task.OutputSchemaName,
				"output_version":   task.OutputSchemaVersion,
				"expected_schema":  nextStage.InputSchema.Name,
				"expected_version": nextStage.InputSchema.Version,
			},
		})
		b.failTask(ctx, pipelineID, stage, task, msg)
		return
	}

	// Atomically advance the task onto the next stage: the update +
	// queue placement is one store operation per backend. On
	// persistence failure the in-memory task struct is NOT mutated so
	// redelivery picks up the pre-routing state and the downstream
	// stage never sees a partially-transitioned task.
	state := TaskStatePending
	attempts := 0
	maxAttempts := nextStage.Retry.MaxAttempts
	transitions := task.CrossStageTransitions
	if err := b.store.RequeueTask(ctx, task.ID, next, TaskUpdate{
		State:                 &state,
		StageID:               &next,
		Payload:               &outputPayload,
		Attempts:              &attempts,
		InputSchemaName:       &nextStage.InputSchema.Name,
		InputSchemaVersion:    &nextStage.InputSchema.Version,
		OutputSchemaName:      &nextStage.OutputSchema.Name,
		OutputSchemaVersion:   &nextStage.OutputSchema.Version,
		MaxAttempts:           &maxAttempts,
		CrossStageTransitions: &transitions,
	}); err != nil {
		b.recordStoreError("requeue_task", pipelineID, next, err)
		return
	}

	task.StageID = next
	task.Payload = outputPayload
	task.State = TaskStatePending
	task.Attempts = 0
	task.InputSchemaName = nextStage.InputSchema.Name
	task.InputSchemaVersion = nextStage.InputSchema.Version
	task.OutputSchemaName = nextStage.OutputSchema.Name
	task.OutputSchemaVersion = nextStage.OutputSchema.Version
	task.MaxAttempts = nextStage.Retry.MaxAttempts

	if b.metrics != nil {
		b.metrics.QueueDepth.WithLabelValues(pipelineID, next).Inc()
	}

	b.logger.Info("task routed",
		"task_id", task.ID, "from", stage.ID, "to", next,
	)
}

// failTask routes a task to the failure path or marks it failed.
// Persistence is fail-closed: dead-letter metrics and terminal
// events fire only after the durable state change succeeds. On
// store error the task stays in its pre-failure state in the store;
// broker_store_errors_total is incremented so operators see the
// divergence without the dead-letter counter silently ticking up.
//
// Store writes use a bounded persistence context derived from
// Background() so a worker ctx that was cancelled (shutdown, or the
// waitForBudget ctx.Done branch) does not prevent the terminal
// state transition from landing. The worker ctx is still threaded
// into mergeMetadata/UpdateTask wrappers below, but each call
// overrides its own ctx with persistCtx — see comment in
// retryTask for the overall model.
func (b *Broker) failTask(ctx context.Context, pipelineID string, stage *config.Stage, task *Task, reason string) {
	mdCtx, mdCancel := b.persistCtx()
	b.mergeMetadata(mdCtx, task, map[string]any{"failure_reason": reason})
	mdCancel()

	next := stage.OnFailure
	if next == "" || next == "dead-letter" {
		from := task.State
		state := TaskStateFailed
		deadLetter := true
		upCtx, upCancel := b.persistCtx()
		err := b.store.UpdateTask(upCtx, task.ID, TaskUpdate{State: &state, RoutedToDeadLetter: &deadLetter})
		upCancel()
		if err != nil {
			b.recordStoreError("update_task", pipelineID, stage.ID, err)
			return
		}
		b.logger.Warn("task failed",
			"task_id", task.ID, "stage", stage.ID, "reason", reason,
		)
		b.eventBus.Publish(TaskEvent{
			Event:      "state_change",
			TaskID:     task.ID,
			PipelineID: pipelineID,
			StageID:    stage.ID,
			From:       from,
			To:         TaskStateFailed,
			Timestamp:  time.Now(),
		})
		if b.metrics != nil {
			b.metrics.TasksDeadLettered.WithLabelValues(pipelineID, stage.ID).Inc()
		}
		b.recordTerminalMetrics(task, pipelineID, stage.ID, "FAILED")
		return
	}

	// Check cross-stage transition limit before routing to failure stage.
	if b.checkTransitionLimit(ctx, pipelineID, stage, task) {
		return
	}

	// Route to an explicit failure stage.
	b.mu.RLock()
	nextStage, ok := b.stages[pipelineID][next]
	b.mu.RUnlock()
	if !ok {
		state := TaskStateFailed
		upCtx, upCancel := b.persistCtx()
		err := b.store.UpdateTask(upCtx, task.ID, TaskUpdate{State: &state})
		upCancel()
		if err != nil {
			b.recordStoreError("update_task", pipelineID, stage.ID, err)
			return
		}
		b.logger.Error("failure stage not found",
			"task_id", task.ID, "stage", stage.ID, "target", next,
		)
		b.recordTerminalMetrics(task, pipelineID, stage.ID, "FAILED")
		return
	}

	state := TaskStatePending
	attempts := 0
	maxAttempts := nextStage.Retry.MaxAttempts
	transitions := task.CrossStageTransitions
	rqCtx, rqCancel := b.persistCtx()
	err := b.store.RequeueTask(rqCtx, task.ID, next, TaskUpdate{
		State:                 &state,
		StageID:               &next,
		Attempts:              &attempts,
		InputSchemaName:       &nextStage.InputSchema.Name,
		InputSchemaVersion:    &nextStage.InputSchema.Version,
		OutputSchemaName:      &nextStage.OutputSchema.Name,
		OutputSchemaVersion:   &nextStage.OutputSchema.Version,
		MaxAttempts:           &maxAttempts,
		CrossStageTransitions: &transitions,
	})
	rqCancel()
	if err != nil {
		b.recordStoreError("requeue_task", pipelineID, next, err)
		return
	}

	task.StageID = next
	task.State = TaskStatePending
	task.Attempts = 0
	task.InputSchemaName = nextStage.InputSchema.Name
	task.InputSchemaVersion = nextStage.InputSchema.Version
	task.OutputSchemaName = nextStage.OutputSchema.Name
	task.OutputSchemaVersion = nextStage.OutputSchema.Version
	task.MaxAttempts = nextStage.Retry.MaxAttempts

	if b.metrics != nil {
		b.metrics.QueueDepth.WithLabelValues(pipelineID, next).Inc()
	}

	b.logger.Info("task routed to failure path",
		"task_id", task.ID, "from", stage.ID, "to", next,
	)
}

// transition atomically updates the task state in the store and emits an event.
// The in-memory task.State mutation is kept on store success; on
// store error the transition is rolled back in memory and the event
// is not published, so downstream consumers never see a state the
// durable record disagrees with.
func (b *Broker) transition(ctx context.Context, task *Task, state TaskState) {
	from := task.State
	if err := b.store.UpdateTask(ctx, task.ID, TaskUpdate{State: &state}); err != nil {
		b.recordStoreError("update_task", task.PipelineID, task.StageID, err)
		return
	}
	task.State = state
	b.logger.Debug("state transition",
		"task_id", task.ID, "state", state,
	)
	b.eventBus.Publish(TaskEvent{
		Event:      "state_change",
		TaskID:     task.ID,
		PipelineID: task.PipelineID,
		StageID:    task.StageID,
		From:       from,
		To:         state,
		Timestamp:  time.Now(),
	})
}

// recordStoreError is the single place broker state-transition
// helpers route a store-write failure. Logs at error level with
// enough context to identify the stage and operation; increments
// broker_store_errors_total so failures are observable even when
// tests or operators don't tail logs. Callers should return
// immediately after recording so they do not emit success events or
// mutate in-memory task state.
func (b *Broker) recordStoreError(operation, pipelineID, stageID string, err error) {
	b.logger.Error("broker store operation failed",
		"operation", operation,
		"pipeline_id", pipelineID,
		"stage_id", stageID,
		"error", err,
	)
	if b.metrics != nil {
		b.metrics.BrokerStoreErrorsTotal.WithLabelValues(pipelineID, stageID, operation).Inc()
	}
}

// reservedMetadataKeys are metadata keys set by the broker that agents must
// not overwrite. This prevents a malicious agent from clearing sanitizer
// warnings, overwriting failure reasons, or tampering with stage history.
var reservedMetadataKeys = map[string]struct{}{
	"sanitizer_warnings":        {},
	"sanitizer_output_warnings": {},
	"failure_reason":            {},
	"stage_history":             {},
	"version_mismatch":          {},
	"trace_id":                  {},
	"span_id":                   {},
}

// warningPatterns extracts just the Pattern field from each sanitizer warning
// so debug logs can report which detection rules fired without dumping the
// full span offsets.
func warningPatterns(warnings []sanitize.SanitizeWarning) []string {
	if len(warnings) == 0 {
		return nil
	}
	out := make([]string, len(warnings))
	for i, w := range warnings {
		out[i] = w.Pattern
	}
	return out
}

// filterReservedMetadata returns a copy of meta with broker-reserved keys removed.
func filterReservedMetadata(meta map[string]any) map[string]any {
	filtered := make(map[string]any, len(meta))
	for k, v := range meta {
		if _, reserved := reservedMetadataKeys[k]; !reserved {
			filtered[k] = v
		}
	}
	return filtered
}

// recordTerminalMetrics records metrics when a task reaches DONE or FAILED,
// and ends the root trace span.
func (b *Broker) recordTerminalMetrics(task *Task, pipelineID, stageID, finalState string) {
	if b.metrics != nil {
		b.metrics.TasksTotal.WithLabelValues(pipelineID, stageID, finalState).Inc()
		duration := time.Since(task.CreatedAt).Seconds()
		b.metrics.TaskDuration.WithLabelValues(pipelineID, stageID).Observe(duration)
	}
	// End root trace span.
	b.mu.Lock()
	if span, ok := b.taskSpans[task.ID]; ok {
		span.End()
		delete(b.taskSpans, task.ID)
	}
	b.mu.Unlock()
}

// defaultPersistTimeout bounds every terminal/requeue store write
// that uses the broker's persistence context. Five seconds is long
// enough for any reasonable store operation to complete and short
// enough that a wedged store does not hang shutdown past the normal
// drain window. Kept a package-level constant rather than a config
// field — this is a backstop for fail-closed bookkeeping, not a
// policy lever.
const defaultPersistTimeout = 5 * time.Second

// persistCtx returns a bounded context for durable bookkeeping that
// is intentionally divorced from any caller context. The worker ctx
// is cancelled on shutdown or during a waitForBudget abort; terminal
// writes and retry-requeue writes must still land or the task is
// stranded in a non-terminal state until operator intervention.
//
// Derived from context.Background() with defaultPersistTimeout, so a
// wedged store cannot pin shutdown past that window. Callers MUST
// call the returned CancelFunc once the store call returns — the
// usual defer pattern is fine.
//
// This is the implementation half of the audit finding that
// shutdown/cancel paths were calling failTask / retryTask with an
// already-cancelled ctx and silently losing the terminal state
// transition.
func (b *Broker) persistCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), defaultPersistTimeout)
}

// mergeMetadata updates task metadata both in-memory and in the
// store. Metadata writes are not routing-critical — callers still
// make progress on the task if the store drops the update — but a
// failure is surfaced via broker_store_errors_total so operators can
// tell when observability metadata (failure_reason, sanitizer
// warnings, stage history) silently stops flowing to the durable
// record.
func (b *Broker) mergeMetadata(ctx context.Context, task *Task, meta map[string]any) {
	if task.Metadata == nil {
		task.Metadata = make(map[string]any)
	}
	for k, v := range meta {
		task.Metadata[k] = v
	}
	if err := b.store.UpdateTask(ctx, task.ID, TaskUpdate{Metadata: meta}); err != nil {
		b.recordStoreError("merge_metadata", task.PipelineID, task.StageID, err)
	}
}
