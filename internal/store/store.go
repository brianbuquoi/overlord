// Package store defines the Store interface for task persistence. Backends
// (memory, Redis, Postgres) implement this interface to provide task enqueueing,
// dequeueing, filtering, and atomic state updates.
package store

import (
	"context"
	"errors"

	"github.com/brianbuquoi/overlord/internal/broker"
)

// Sentinel errors for store operations.
var (
	ErrTaskNotFound = errors.New("task not found")
	// ErrQueueEmpty is an alias for broker.ErrQueueEmpty so the broker can
	// check dequeue errors without importing the store package.
	ErrQueueEmpty = broker.ErrQueueEmpty
	// ErrTaskNotReplayable is returned by ClaimForReplay when the task
	// exists but is not in a replayable state (i.e. not FAILED+dead-lettered).
	ErrTaskNotReplayable = errors.New("task is not in a replayable state")
	// ErrTaskNotReplayPending is returned by RollbackReplayClaim when the task
	// exists but is not in REPLAY_PENDING — it may have already been completed
	// or rolled back by another caller.
	ErrTaskNotReplayPending = errors.New("task is not in REPLAY_PENDING state")
)

// Store is the interface for task persistence and queuing.
type Store interface {
	EnqueueTask(ctx context.Context, stageID string, task *broker.Task) error
	DequeueTask(ctx context.Context, stageID string) (*broker.Task, error)
	UpdateTask(ctx context.Context, taskID string, update broker.TaskUpdate) error
	GetTask(ctx context.Context, taskID string) (*broker.Task, error)
	ListTasks(ctx context.Context, filter broker.TaskFilter) (*broker.ListTasksResult, error)
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
	ClaimForReplay(ctx context.Context, taskID string) (*broker.Task, error)

	// RollbackReplayClaim atomically transitions a task from REPLAY_PENDING
	// back to FAILED with RoutedToDeadLetter=true, making it visible and
	// replayable again.
	// Returns ErrTaskNotFound if the task does not exist.
	// Returns ErrTaskNotReplayPending if the task is not in REPLAY_PENDING
	// state (it may have already been completed or rolled back by another
	// caller).
	RollbackReplayClaim(ctx context.Context, taskID string) error
}
