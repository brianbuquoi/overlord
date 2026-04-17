package broker_test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/brianbuquoi/overlord/internal/broker"
	"github.com/brianbuquoi/overlord/internal/metrics"
	"github.com/brianbuquoi/overlord/internal/store/memory"
)

// requeueFailingStore wraps MemoryStore and injects a failure on
// RequeueTask calls for a specific destination stage. Used to
// confirm the broker's fail-closed routing contract: on persistence
// failure, no success metric increments and the
// broker_store_errors_total counter is incremented.
type requeueFailingStore struct {
	*memory.MemoryStore
	failStage string
	calls     atomic.Int32
}

func (r *requeueFailingStore) RequeueTask(ctx context.Context, taskID, stageID string, update broker.TaskUpdate) error {
	if stageID == r.failStage {
		r.calls.Add(1)
		return errors.New("simulated requeue failure")
	}
	return r.MemoryStore.RequeueTask(ctx, taskID, stageID, update)
}

func warnLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

// counterSum walks every sample on the given metric family and
// returns the sum of samples matching the operation label. Returns
// 0 when the family / label is absent — tests use that to prove a
// metric did NOT tick.
func counterSum(t *testing.T, m *metrics.Metrics, family, labelName, labelValue string) float64 {
	t.Helper()
	mf, err := m.Registry.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	var sum float64
	for _, fam := range mf {
		if fam.GetName() != family {
			continue
		}
		for _, sample := range fam.GetMetric() {
			match := false
			for _, label := range sample.GetLabel() {
				if label.GetName() == labelName && label.GetValue() == labelValue {
					match = true
					break
				}
			}
			if match {
				sum += sample.GetCounter().GetValue()
			}
		}
	}
	return sum
}

// TestBroker_FailClosedOnRequeueError documents the primary
// invariant for the Store contract split: a RequeueTask persistence
// failure during routing must increment
// broker_store_errors_total{operation=requeue_task} and must NOT
// increment the success-case TasksTotal counter for the downstream
// stage.
func TestBroker_FailClosedOnRequeueError(t *testing.T) {
	cfg, _, agents, reg := buildTestEnv(t)

	st := &requeueFailingStore{MemoryStore: memory.New(), failStage: "stage2"}

	agents["agent1"].(*mockAgent).setHandler(func(_ context.Context, _ *broker.Task) (*broker.TaskResult, error) {
		return &broker.TaskResult{Payload: json.RawMessage(`{"category":"test"}`)}, nil
	})
	agents["agent2"].(*mockAgent).setHandler(func(_ context.Context, _ *broker.Task) (*broker.TaskResult, error) {
		return &broker.TaskResult{Payload: json.RawMessage(`{"result":"ok"}`)}, nil
	})
	agents["agent3"].(*mockAgent).setHandler(func(_ context.Context, _ *broker.Task) (*broker.TaskResult, error) {
		return &broker.TaskResult{Payload: json.RawMessage(`{"valid":true}`)}, nil
	})

	m := metrics.New()
	b := broker.New(cfg, st, agents, reg, warnLogger(), m, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go b.Run(ctx)

	_, err := b.Submit(ctx, "test-pipeline", json.RawMessage(`{"request":"fail-closed-test"}`))
	if err != nil {
		t.Fatal(err)
	}

	// Wait for stage1 → stage2 routing attempt to fail.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && st.calls.Load() == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if st.calls.Load() == 0 {
		t.Fatal("broker never attempted RequeueTask into the failing stage")
	}
	// Let the broker's goroutines settle so any (unexpected) success
	// metric increments would have fired by now.
	time.Sleep(150 * time.Millisecond)

	// broker_store_errors_total{operation=requeue_task} > 0.
	gotErrs := counterSum(t, m, "overlord_broker_store_errors_total", "operation", "requeue_task")
	if gotErrs == 0 {
		t.Error("expected overlord_broker_store_errors_total{operation=requeue_task} > 0")
	}

	// TasksTotal{final_state=DONE} must NOT tick — the task never
	// reached DONE because routing failed at stage2.
	gotDone := counterSum(t, m, "overlord_tasks_total", "final_state", "DONE")
	if gotDone > 0 {
		t.Errorf("tasks_total{DONE} = %v; routing failed so no DONE should have been recorded", gotDone)
	}
}

// TestBroker_FailClosedOnDoneUpdateError covers the terminal DONE
// branch: if the store UpdateTask to DONE fails, TasksTotal{DONE}
// must not tick and broker_store_errors_total{update_task} must.
func TestBroker_FailClosedOnDoneUpdateError(t *testing.T) {
	cfg, _, agents, reg := buildTestEnv(t)

	seen := atomic.Int32{}
	st := &doneFailingStore{MemoryStore: memory.New(), failOnState: broker.TaskStateDone, seen: &seen}

	// Drive the pipeline end-to-end so routeSuccess eventually hits
	// the DONE branch on stage3.
	for _, id := range []string{"agent1", "agent2", "agent3"} {
		id := id
		agents[id].(*mockAgent).setHandler(func(_ context.Context, _ *broker.Task) (*broker.TaskResult, error) {
			if id == "agent3" {
				return &broker.TaskResult{Payload: json.RawMessage(`{"valid":true}`)}, nil
			}
			return &broker.TaskResult{Payload: json.RawMessage(`{"result":"ok"}`)}, nil
		})
	}
	// stage1's agent needs a structured "category" field for the
	// default routing to move through stages.
	agents["agent1"].(*mockAgent).setHandler(func(_ context.Context, _ *broker.Task) (*broker.TaskResult, error) {
		return &broker.TaskResult{Payload: json.RawMessage(`{"category":"fast"}`)}, nil
	})

	m := metrics.New()
	b := broker.New(cfg, st, agents, reg, warnLogger(), m, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go b.Run(ctx)

	_, err := b.Submit(ctx, "test-pipeline", json.RawMessage(`{"request":"done-fail"}`))
	if err != nil {
		t.Fatal(err)
	}

	// Wait for the DONE update attempt to land on the failing store.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && seen.Load() == 0 {
		time.Sleep(25 * time.Millisecond)
	}
	if seen.Load() == 0 {
		t.Fatal("broker never attempted to transition a task to DONE")
	}
	time.Sleep(150 * time.Millisecond)

	gotErrs := counterSum(t, m, "overlord_broker_store_errors_total", "operation", "update_task")
	if gotErrs == 0 {
		t.Error("expected overlord_broker_store_errors_total{operation=update_task} > 0")
	}
	gotDone := counterSum(t, m, "overlord_tasks_total", "final_state", "DONE")
	if gotDone > 0 {
		t.Errorf("tasks_total{DONE} = %v despite UpdateTask(DONE) failure", gotDone)
	}
}

// doneFailingStore fails UpdateTask when the requested new state is
// exactly failOnState, and delegates otherwise.
type doneFailingStore struct {
	*memory.MemoryStore
	failOnState broker.TaskState
	seen        *atomic.Int32
}

func (d *doneFailingStore) UpdateTask(ctx context.Context, taskID string, update broker.TaskUpdate) error {
	if update.State != nil && *update.State == d.failOnState {
		d.seen.Add(1)
		return errors.New("simulated update failure")
	}
	return d.MemoryStore.UpdateTask(ctx, taskID, update)
}

// TestBroker_MergeMetadataFailureIsObservable confirms a metadata
// persistence failure is visible via broker_store_errors_total
// rather than silently swallowed.
func TestBroker_MergeMetadataFailureIsObservable(t *testing.T) {
	cfg, _, agents, reg := buildTestEnv(t)

	st := &metadataFailingStore{MemoryStore: memory.New()}
	st.failNext.Store(true)

	// agent1 errors so failTask runs mergeMetadata with
	// failure_reason — that is where the injected failure fires.
	agents["agent1"].(*mockAgent).setHandler(func(_ context.Context, _ *broker.Task) (*broker.TaskResult, error) {
		return nil, errors.New("boom")
	})
	agents["agent2"].(*mockAgent).setHandler(func(_ context.Context, _ *broker.Task) (*broker.TaskResult, error) {
		return &broker.TaskResult{Payload: json.RawMessage(`{}`)}, nil
	})
	agents["agent3"].(*mockAgent).setHandler(func(_ context.Context, _ *broker.Task) (*broker.TaskResult, error) {
		return &broker.TaskResult{Payload: json.RawMessage(`{}`)}, nil
	})

	m := metrics.New()
	b := broker.New(cfg, st, agents, reg, warnLogger(), m, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.Run(ctx)

	_, err := b.Submit(ctx, "test-pipeline", json.RawMessage(`{"request":"metadata-fail"}`))
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(400 * time.Millisecond)

	// Accept merge_metadata OR update_task as the operation label —
	// mergeMetadata uses merge_metadata; but adjacent UpdateTask
	// calls in failTask may also succeed-then-fail depending on
	// timing, so just confirm at least one store-error was emitted.
	mf, err := m.Registry.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	found := false
	for _, fam := range mf {
		if fam.GetName() != "overlord_broker_store_errors_total" {
			continue
		}
		for _, sample := range fam.GetMetric() {
			for _, label := range sample.GetLabel() {
				if label.GetName() == "operation" && strings.Contains(label.GetValue(), "metadata") {
					if sample.GetCounter().GetValue() > 0 {
						found = true
					}
				}
			}
		}
	}
	if !found {
		t.Error("expected overlord_broker_store_errors_total{operation=merge_metadata} > 0")
	}
}

// metadataFailingStore fails the first UpdateTask carrying a
// non-empty Metadata map; later calls pass through.
type metadataFailingStore struct {
	*memory.MemoryStore
	failNext atomic.Bool
}

func (m *metadataFailingStore) UpdateTask(ctx context.Context, taskID string, update broker.TaskUpdate) error {
	if len(update.Metadata) > 0 && m.failNext.Load() {
		m.failNext.Store(false)
		return errors.New("simulated metadata update failure")
	}
	return m.MemoryStore.UpdateTask(ctx, taskID, update)
}
