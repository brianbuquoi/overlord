package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/brianbuquoi/overlord/internal/broker"
)

// pollTerminalSetup stands up a broker (memory store, no workers), submits
// a single task, transitions it to the requested terminal state, and
// returns the task ID — so a test can call pollTask and assert how a
// given terminal state maps to exit-code semantics.
func pollTerminalSetup(t *testing.T, finalState broker.TaskState, routedToDeadLetter bool) (*broker.Broker, string) {
	t.Helper()
	configPath := writeTestYAML(t)
	cfg, err := loadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	b, err := buildBroker(cfg, nil, configPath, newLogger(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	payload := json.RawMessage(`{"request":"test"}`)
	task, err := b.Submit(context.Background(), "test-pipeline", payload)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	upd := broker.TaskUpdate{State: &finalState}
	if routedToDeadLetter {
		dl := true
		upd.RoutedToDeadLetter = &dl
	}
	if err := b.Store().UpdateTask(context.Background(), task.ID, upd); err != nil {
		t.Fatalf("force terminal state: %v", err)
	}
	return b, task.ID
}

func TestPollTask_Done(t *testing.T) {
	b, id := pollTerminalSetup(t, broker.TaskStateDone, false)
	if err := pollTask(context.Background(), b, id, 5*time.Second); err != nil {
		t.Fatalf("expected nil for DONE, got %v", err)
	}
}

func TestPollTask_Failed(t *testing.T) {
	b, id := pollTerminalSetup(t, broker.TaskStateFailed, false)
	err := pollTask(context.Background(), b, id, 5*time.Second)
	var sw *submitWaitError
	if !errors.As(err, &sw) || sw.ExitCode() != 1 {
		t.Fatalf("expected exit-code 1 submitWaitError for FAILED, got %v", err)
	}
}

func TestPollTask_DeadLetter(t *testing.T) {
	b, id := pollTerminalSetup(t, broker.TaskStateFailed, true)
	err := pollTask(context.Background(), b, id, 5*time.Second)
	var sw *submitWaitError
	if !errors.As(err, &sw) || sw.ExitCode() != 1 {
		t.Fatalf("expected exit 1 for dead-lettered, got %v", err)
	}
	if !strings.Contains(sw.Error(), "dead-letter") {
		t.Errorf("expected dead-letter mention, got %q", sw.Error())
	}
}

func TestPollTask_Discarded(t *testing.T) {
	b, id := pollTerminalSetup(t, broker.TaskStateDiscarded, false)
	err := pollTask(context.Background(), b, id, 5*time.Second)
	var sw *submitWaitError
	if !errors.As(err, &sw) || sw.ExitCode() != 1 {
		t.Fatalf("expected exit 1 for DISCARDED, got %v", err)
	}
	if !strings.Contains(sw.Error(), "discard") {
		t.Errorf("expected discarded mention, got %q", sw.Error())
	}
}

func TestPollTask_Replayed(t *testing.T) {
	b, id := pollTerminalSetup(t, broker.TaskStateReplayed, false)
	if err := pollTask(context.Background(), b, id, 5*time.Second); err != nil {
		t.Fatalf("expected nil for REPLAYED, got %v", err)
	}
}

func TestPollTask_Timeout(t *testing.T) {
	configPath := writeTestYAML(t)
	cfg, err := loadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	b, err := buildBroker(cfg, nil, configPath, newLogger(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	task, err := b.Submit(context.Background(), "test-pipeline", json.RawMessage(`{"request":"test"}`))
	if err != nil {
		t.Fatal(err)
	}
	err = pollTask(context.Background(), b, task.ID, 200*time.Millisecond)
	var sw *submitWaitError
	if !errors.As(err, &sw) || sw.ExitCode() != 2 {
		t.Fatalf("expected exit-code 2 timeout, got %v", err)
	}
}
