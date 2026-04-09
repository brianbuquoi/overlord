// Package store defines the Store interface for task persistence. Backends
// (memory, Redis, Postgres) implement this interface to provide task enqueueing,
// dequeueing, filtering, and atomic state updates.
package store

import (
	"context"
	"errors"

	"github.com/orcastrator/orcastrator/internal/broker"
)

// Sentinel errors for store operations.
var (
	ErrTaskNotFound = errors.New("task not found")
	// ErrQueueEmpty is an alias for broker.ErrQueueEmpty so the broker can
	// check dequeue errors without importing the store package.
	ErrQueueEmpty = broker.ErrQueueEmpty
)

// Store is the interface for task persistence and queuing.
type Store interface {
	EnqueueTask(ctx context.Context, stageID string, task *broker.Task) error
	DequeueTask(ctx context.Context, stageID string) (*broker.Task, error)
	UpdateTask(ctx context.Context, taskID string, update broker.TaskUpdate) error
	GetTask(ctx context.Context, taskID string) (*broker.Task, error)
	ListTasks(ctx context.Context, filter broker.TaskFilter) (*broker.ListTasksResult, error)
}
