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
)

// Store is the interface for task persistence and queuing.
type Store interface {
	EnqueueTask(ctx context.Context, stageID string, task *broker.Task) error
	DequeueTask(ctx context.Context, stageID string) (*broker.Task, error)
	UpdateTask(ctx context.Context, taskID string, update broker.TaskUpdate) error
	GetTask(ctx context.Context, taskID string) (*broker.Task, error)
	ListTasks(ctx context.Context, filter broker.TaskFilter) (*broker.ListTasksResult, error)
	// ClaimForReplay atomically validates that the task is in a replayable
	// state (state = FAILED and RoutedToDeadLetter = true) and clears
	// RoutedToDeadLetter to false to prevent duplicate replay submissions.
	// The task state is not changed. Returns the task (with RoutedToDeadLetter
	// now false) on success.
	// Returns ErrTaskNotFound if the task does not exist.
	// Returns ErrTaskNotReplayable if the task exists but is not in a
	// replayable state (including tasks already claimed by a concurrent caller).
	ClaimForReplay(ctx context.Context, taskID string) (*broker.Task, error)
}
