package broker_test

import (
	"testing"

	"github.com/brianbuquoi/overlord/internal/broker"
)

func TestTaskState_IsTerminal(t *testing.T) {
	terminal := []broker.TaskState{
		broker.TaskStateDone,
		broker.TaskStateFailed,
		broker.TaskStateDiscarded,
		broker.TaskStateReplayed,
	}
	for _, s := range terminal {
		if !s.IsTerminal() {
			t.Errorf("%s should be terminal", s)
		}
	}

	nonTerminal := []broker.TaskState{
		broker.TaskStatePending,
		broker.TaskStateRouting,
		broker.TaskStateExecuting,
		broker.TaskStateValidating,
		broker.TaskStateRetrying,
		broker.TaskStateWaiting,
		broker.TaskStateReplayPending,
		broker.TaskState(""),
		broker.TaskState("UNKNOWN"),
	}
	for _, s := range nonTerminal {
		if s.IsTerminal() {
			t.Errorf("%s should not be terminal", s)
		}
	}
}
