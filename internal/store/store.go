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
	// ErrTaskNotDiscardable is returned by DiscardDeadLetter when the task
	// exists but is not in a discardable state (i.e. not FAILED+dead-lettered)
	// and is not already DISCARDED. The most common cause is that a concurrent
	// replay claim moved the task out of the dead-letter state before this
	// discard landed.
	ErrTaskNotDiscardable = errors.New("task is not in a discardable state")
	// ErrTaskAlreadyDiscarded is returned by DiscardDeadLetter when the task
	// is already DISCARDED. Kept distinct from ErrTaskNotDiscardable so the
	// HTTP API can preserve its ALREADY_DISCARDED response code and a
	// genuine retry can be distinguished from a lost-race state mismatch.
	ErrTaskAlreadyDiscarded = errors.New("task is already discarded")
	// ErrTaskAlreadyTerminal is returned by CancelTask when the task exists
	// but is already in a terminal state (DONE / FAILED / DISCARDED /
	// REPLAYED). This protects the SEC2-003 race: the prior read-check-write
	// cancel shape could clobber a concurrent completion.
	ErrTaskAlreadyTerminal = errors.New("task is already in a terminal state")
)

// Store is the interface for task persistence and queuing.
type Store interface {
	EnqueueTask(ctx context.Context, stageID string, task *broker.Task) error
	DequeueTask(ctx context.Context, stageID string) (*broker.Task, error)
	// RequeueTask atomically applies update to an existing task and
	// places it onto stageID's queue. Unlike EnqueueTask (which
	// inserts a net-new task), this is the contract the broker uses
	// for routing, retry, and failure-path requeues where the same
	// task ID is being moved to a (possibly different) stage.
	//
	// The update fields and the queue placement are applied as one
	// operation per backend: callers either see the task persisted
	// onto stageID with the new update applied AND dequeueable, or
	// see an error and no state change. This is the fail-closed
	// contract the broker relies on to stop emitting terminal events
	// when persistence has not actually happened.
	//
	// Returns ErrTaskNotFound if the task does not exist.
	RequeueTask(ctx context.Context, taskID, stageID string, update broker.TaskUpdate) error
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

	// DiscardDeadLetter atomically transitions a FAILED+dead-lettered task
	// to DISCARDED. The state transition is the claim token so a concurrent
	// replay cannot silently see its REPLAY_PENDING / REPLAYED outcome
	// overwritten by a late discard — the audit called out SEC4-008d as
	// the remaining replay-vs-discard race after SEC4-008/a/b/c closed
	// the replay side.
	//
	// Returns ErrTaskNotFound if the task does not exist.
	// Returns ErrTaskAlreadyDiscarded if the task is already DISCARDED
	// (idempotent retry — the caller can treat this as success).
	// Returns ErrTaskNotDiscardable if the task is in any other state
	// (including REPLAY_PENDING, REPLAYED, DONE, or a non-dead-lettered
	// FAILED).
	DiscardDeadLetter(ctx context.Context, taskID string) error

	// CancelTask atomically transitions a non-terminal task to FAILED with a
	// "cancelled by operator" failure_reason. The state check and the write
	// are one operation so a concurrent completion cannot be overwritten by
	// a late cancel (SEC2-003).
	//
	// Returns the task's pre-cancel snapshot so callers can surface context
	// (for example, warning that a task caught in EXECUTING may still finish
	// the current stage even though further routing is stopped).
	//
	// Returns ErrTaskNotFound if the task does not exist.
	// Returns ErrTaskAlreadyTerminal if the task is already terminal
	// (DONE / FAILED / DISCARDED / REPLAYED).
	CancelTask(ctx context.Context, taskID string) (*broker.Task, error)
}
