package broker_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/brianbuquoi/overlord/internal/broker"
	"github.com/brianbuquoi/overlord/internal/metrics"
	"github.com/brianbuquoi/overlord/internal/store/memory"
)

// ctxRecordingStore wraps MemoryStore and records, for each terminal
// store write, whether the context the broker passed in was already
// cancelled. The persistence-context fix requires these writes to
// run under a fresh Background()-derived context so shutdown
// cancellation does not strand tasks mid-transition.
type ctxRecordingStore struct {
	*memory.MemoryStore
	failedTerminal   atomic.Bool // set when a failTask UpdateTask ran with ctx.Err() != nil
	requeueTerminal  atomic.Bool // set when a retry RequeueTask ran with ctx.Err() != nil
	observedUpdate  atomic.Bool // UpdateTask with state=FAILED was observed
	observedRequeue atomic.Bool // RequeueTask during retry was observed
}

func (c *ctxRecordingStore) UpdateTask(ctx context.Context, taskID string, u broker.TaskUpdate) error {
	if u.State != nil && *u.State == broker.TaskStateFailed {
		c.observedUpdate.Store(true)
		if ctx.Err() != nil {
			c.failedTerminal.Store(true)
		}
	}
	return c.MemoryStore.UpdateTask(ctx, taskID, u)
}

func (c *ctxRecordingStore) RequeueTask(ctx context.Context, taskID, stageID string, u broker.TaskUpdate) error {
	// A requeue with state=PENDING and attempts > 0 is a retry requeue.
	if u.State != nil && *u.State == broker.TaskStatePending && u.Attempts != nil && *u.Attempts > 0 {
		c.observedRequeue.Store(true)
		if ctx.Err() != nil {
			c.requeueTerminal.Store(true)
		}
	}
	return c.MemoryStore.RequeueTask(ctx, taskID, stageID, u)
}

// TestBroker_PersistCtxSurvivesWorkerCancel verifies that terminal
// store writes driven by failTask still land when the worker ctx
// that reached failTask is already cancelled. The prior shape of
// the broker threaded the worker ctx through to the store call, so
// shutdown-timed cancellation would skip the terminal UpdateTask
// and strand the task in a non-terminal state — the audit flagged
// this as a follow-up to SEC-016.
//
// We drive the condition by cancelling the broker's Run context
// immediately after Submit so the worker picks the task up, the
// agent returns an error, and failTask sees ctx.Err() != nil. The
// store wrapper records whether the ctx it received on the terminal
// UpdateTask call was cancelled; with the persistCtx fix the
// received ctx must be fresh even though the upstream ctx is not.
func TestBroker_PersistCtxSurvivesWorkerCancel(t *testing.T) {
	cfg, _, agents, reg := buildTestEnv(t)

	// Force agent1 to return a non-retryable error on every call so
	// failTask runs promptly (no sleep, no retry ladder in the way).
	agents["agent1"].(*mockAgent).setHandler(func(_ context.Context, _ *broker.Task) (*broker.TaskResult, error) {
		return nil, errors.New("boom")
	})
	agents["agent2"].(*mockAgent).setHandler(func(_ context.Context, _ *broker.Task) (*broker.TaskResult, error) {
		return &broker.TaskResult{Payload: json.RawMessage(`{}`)}, nil
	})
	agents["agent3"].(*mockAgent).setHandler(func(_ context.Context, _ *broker.Task) (*broker.TaskResult, error) {
		return &broker.TaskResult{Payload: json.RawMessage(`{}`)}, nil
	})

	st := &ctxRecordingStore{MemoryStore: memory.New()}
	m := metrics.New()
	b := broker.New(cfg, st, agents, reg, warnLogger(), m, nil)

	runCtx, cancel := context.WithCancel(context.Background())
	go b.Run(runCtx)

	submitCtx, submitCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer submitCancel()
	if _, err := b.Submit(submitCtx, "test-pipeline", json.RawMessage(`{"request":"hi"}`)); err != nil {
		t.Fatalf("submit: %v", err)
	}

	// Give the worker a moment to pick up and call agent once so
	// failTask is the next thing in line, then cancel so the ctx it
	// receives is already Done.
	time.Sleep(50 * time.Millisecond)
	cancel()

	// Wait long enough for failTask's persistCtx write to land.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if st.observedUpdate.Load() {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}

	if !st.observedUpdate.Load() {
		t.Fatal("failTask UpdateTask with state=FAILED was never observed — persistCtx may have timed out")
	}
	if st.failedTerminal.Load() {
		t.Error("terminal UpdateTask received a cancelled ctx — persistCtx is not being used in failTask")
	}
}

// TestBroker_PersistCtxSurvivesRetrySleepCancel verifies the retry
// path: once the broker has transitioned a task to RETRYING and
// started its backoff sleep, cancelling the worker ctx mid-sleep
// must not strand the task in RETRYING. The backoff itself still
// aborts (that is correct — the worker is shutting down), but the
// subsequent RequeueTask → PENDING bookkeeping uses persistCtx and
// must still land.
func TestBroker_PersistCtxSurvivesRetrySleepCancel(t *testing.T) {
	cfg, _, agents, reg := buildTestEnv(t)

	// agent1 must fail with a retryable error so the broker enters
	// retryTask. A plain error is classified non-retryable by the
	// broker, so we implement the RetryableError interface explicitly.
	var agentCalls atomic.Int32
	agents["agent1"].(*mockAgent).setHandler(func(_ context.Context, _ *broker.Task) (*broker.TaskResult, error) {
		agentCalls.Add(1)
		return nil, &retryableErr{msg: "temporary failure"}
	})
	agents["agent2"].(*mockAgent).setHandler(func(_ context.Context, _ *broker.Task) (*broker.TaskResult, error) {
		return &broker.TaskResult{Payload: json.RawMessage(`{}`)}, nil
	})
	agents["agent3"].(*mockAgent).setHandler(func(_ context.Context, _ *broker.Task) (*broker.TaskResult, error) {
		return &broker.TaskResult{Payload: json.RawMessage(`{}`)}, nil
	})

	// Force a long backoff so the cancel arrives mid-sleep.
	// Stage1 retry budget is already set by buildTestEnv; we just
	// need base_delay to be larger than our cancel window.
	cfg.Pipelines[0].Stages[0].Retry.BaseDelay.Duration = 300 * time.Millisecond
	cfg.Pipelines[0].Stages[0].Retry.MaxAttempts = 3

	st := &ctxRecordingStore{MemoryStore: memory.New()}
	m := metrics.New()
	b := broker.New(cfg, st, agents, reg, warnLogger(), m, nil)

	runCtx, cancel := context.WithCancel(context.Background())
	go b.Run(runCtx)

	submitCtx, submitCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer submitCancel()
	if _, err := b.Submit(submitCtx, "test-pipeline", json.RawMessage(`{"request":"retry"}`)); err != nil {
		t.Fatalf("submit: %v", err)
	}

	// Let the first attempt fail and the broker enter the retry
	// backoff, then cancel mid-sleep.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if agentCalls.Load() >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond) // enter sleep
	cancel()

	// Wait for the requeue to land under persistCtx.
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if st.observedRequeue.Load() {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}

	if !st.observedRequeue.Load() {
		t.Fatal("retry RequeueTask never observed — persistCtx did not survive the cancelled worker ctx")
	}
	if st.requeueTerminal.Load() {
		t.Error("retry RequeueTask received a cancelled ctx — persistCtx is not being used in retryTask")
	}
}
