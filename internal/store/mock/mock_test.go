package mock

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/brianbuquoi/overlord/internal/broker"
)

func seedTask(t *testing.T, ctx context.Context, s interface {
	EnqueueTask(context.Context, string, *broker.Task) error
}, id string) *broker.Task {
	t.Helper()
	task := &broker.Task{
		ID:         id,
		PipelineID: "p",
		StageID:    "stage-1",
		State:      broker.TaskStatePending,
		Payload:    json.RawMessage(`{}`),
	}
	if err := s.EnqueueTask(ctx, "stage-1", task); err != nil {
		t.Fatalf("seed %s: %v", id, err)
	}
	return task
}

func TestMock_DefaultsDelegateToMemory(t *testing.T) {
	ctx := context.Background()
	m := New()

	seedTask(t, ctx, m, "task-1")

	got, err := m.GetTask(ctx, "task-1")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.ID != "task-1" {
		t.Fatalf("GetTask: id=%q want task-1", got.ID)
	}

	deq, err := m.DequeueTask(ctx, "stage-1")
	if err != nil {
		t.Fatalf("DequeueTask: %v", err)
	}
	if deq.ID != "task-1" {
		t.Fatalf("DequeueTask: id=%q want task-1", deq.ID)
	}

	state := broker.TaskStateDone
	if err := m.UpdateTask(ctx, "task-1", broker.TaskUpdate{State: &state}); err != nil {
		t.Fatalf("UpdateTask: %v", err)
	}

	res, err := m.ListTasks(ctx, broker.TaskFilter{Limit: 10})
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if res.Total != 1 {
		t.Fatalf("ListTasks total=%d want 1", res.Total)
	}
}

func TestMock_HookOverridesDefault(t *testing.T) {
	ctx := context.Background()
	m := New()
	seedTask(t, ctx, m, "task-1")

	sentinel := errors.New("boom")
	m.OnGetTask = func(_ context.Context, _ string) (*broker.Task, error) {
		return nil, sentinel
	}

	_, err := m.GetTask(ctx, "task-1")
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected hook sentinel, got %v", err)
	}
}

func TestMock_NewWithFailingSubmit(t *testing.T) {
	ctx := context.Background()
	m := NewWithFailingSubmit("bad-1", "bad-2")

	// Good ID delegates to memory and succeeds.
	good := &broker.Task{ID: "good", PipelineID: "p", StageID: "s", Payload: json.RawMessage(`{}`)}
	if err := m.EnqueueTask(ctx, "s", good); err != nil {
		t.Fatalf("good enqueue: %v", err)
	}
	if _, err := m.GetTask(ctx, "good"); err != nil {
		t.Fatalf("good GetTask: %v", err)
	}

	// Bad ID returns ErrInjected.
	bad := &broker.Task{ID: "bad-1", PipelineID: "p", StageID: "s", Payload: json.RawMessage(`{}`)}
	err := m.EnqueueTask(ctx, "s", bad)
	if !errors.Is(err, ErrInjected) {
		t.Fatalf("bad enqueue: want ErrInjected, got %v", err)
	}
	// bad-1 was not enqueued.
	if _, err := m.GetTask(ctx, "bad-1"); err == nil {
		t.Fatal("bad-1 should not be in store after injected failure")
	}
}

func TestMock_NewWithFailingUpdate(t *testing.T) {
	ctx := context.Background()
	m := NewWithFailingUpdate("bad-1")
	seedTask(t, ctx, m, "good")
	seedTask(t, ctx, m, "bad-1")

	state := broker.TaskStateDone
	if err := m.UpdateTask(ctx, "good", broker.TaskUpdate{State: &state}); err != nil {
		t.Fatalf("good update: %v", err)
	}
	if err := m.UpdateTask(ctx, "bad-1", broker.TaskUpdate{State: &state}); !errors.Is(err, ErrInjected) {
		t.Fatalf("bad update: want ErrInjected, got %v", err)
	}
}
