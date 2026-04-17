// Package deadletter provides bulk dead-letter operations (replay-all,
// discard-all, count) shared by the HTTP API handlers and the CLI.
//
// Centralising the pagination loop, failedIDs guard, ceiling enforcement,
// and structured logging in one place prevents the two call sites from
// drifting apart as semantics tighten over time.
package deadletter

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/brianbuquoi/overlord/internal/broker"
	"github.com/brianbuquoi/overlord/internal/store"
)

// DefaultMaxTasks is the per-call ceiling applied when the caller passes
// maxTasks <= 0. It matches the API handler limit so CLI and API behave
// identically by default.
const DefaultMaxTasks = 100000

// pageSize is the page size used when iterating over dead-letter tasks.
// It matches the API's maxListLimit.
const pageSize = 1000

// BulkResult summarises a bulk dead-letter operation.
type BulkResult struct {
	Processed int
	Failed    int
	Truncated bool
}

// ProgressFunc is called after each task is processed during a bulk operation.
// taskID is the original dead-lettered task ID.
// newTaskID is the ID of the newly submitted task on success, or empty string
// on failure and for operations (like discard) that do not produce a new task.
// err is non-nil if the task failed to process.
// Callers that do not need progress output can pass nil.
type ProgressFunc func(taskID string, newTaskID string, err error)

// Service encapsulates bulk dead-letter operations. It is safe for use from
// any goroutine that calls the underlying Store and Broker safely; it has
// no state of its own.
type Service struct {
	store  broker.Store
	broker *broker.Broker
	logger *slog.Logger
}

// New constructs a Service. logger may be nil; a default is used in that case.
func New(s broker.Store, b *broker.Broker, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{store: s, broker: b, logger: logger}
}

func clampMax(maxTasks int) int {
	if maxTasks <= 0 {
		return DefaultMaxTasks
	}
	return maxTasks
}

// ReplayAll replays all dead-lettered tasks matching the filter.
// pipelineID may be empty to replay across all pipelines.
// maxTasks caps the total number of tasks processed; use 0 for DefaultMaxTasks.
// onProgress is called after each task is processed (may be nil).
func (s *Service) ReplayAll(ctx context.Context, pipelineID string, maxTasks int, onProgress ProgressFunc) (BulkResult, error) {
	ceiling := clampMax(maxTasks)

	deadLetter := true
	failedState := broker.TaskStateFailed
	filter := broker.TaskFilter{
		State:              &failedState,
		RoutedToDeadLetter: &deadLetter,
		Limit:              pageSize,
	}
	if pipelineID != "" {
		filter.PipelineID = &pipelineID
	}

	count := 0
	truncated := false
	// failedIDs tracks task IDs that have already failed so that tasks
	// which reappear on subsequent pages (because RollbackReplayClaim
	// restored them to dead-letter status) are not retried or double-counted.
	failedIDs := make(map[string]struct{})

	for count < ceiling {
		// Successfully replayed tasks drop out of the filter because the
		// claim flipped RoutedToDeadLetter. We fetch offset=0 each iteration.
		page, err := s.store.ListTasks(ctx, filter)
		if err != nil {
			return BulkResult{Processed: count, Failed: len(failedIDs), Truncated: truncated}, fmt.Errorf("list dead-letter tasks: %w", err)
		}
		if len(page.Tasks) == 0 {
			break
		}
		progressed := false
		for _, task := range page.Tasks {
			if count >= ceiling {
				truncated = true
				break
			}
			if _, already := failedIDs[task.ID]; already {
				continue
			}
			newTaskID, err := s.replayOne(ctx, task)
			if err != nil {
				failedIDs[task.ID] = struct{}{}
				if onProgress != nil {
					onProgress(task.ID, "", err)
				}
				continue
			}
			count++
			progressed = true
			if onProgress != nil {
				onProgress(task.ID, newTaskID, nil)
			}
		}
		if !progressed {
			// Every task in this page failed or was already-failed. Break
			// to avoid a tight loop over the same unclaimable entries.
			break
		}
	}
	if count >= ceiling {
		truncated = true
		s.logger.Warn("replay-all hit per-call ceiling",
			"pipeline_id", pipelineID,
			"ceiling", ceiling,
			"count", count,
		)
	}

	return BulkResult{Processed: count, Failed: len(failedIDs), Truncated: truncated}, nil
}

// replayOne performs the atomic ClaimForReplay → Submit →
// RollbackReplayClaim-on-failure → mark REPLAYED sequence for a single task.
// It returns a non-nil error on any failure so the caller can track the task
// in failedIDs. Log output matches the original API handler line-for-line so
// operator-facing observability is preserved.
func (s *Service) replayOne(ctx context.Context, task *broker.Task) (string, error) {
	claimed, err := s.store.ClaimForReplay(ctx, task.ID)
	if err != nil {
		s.logger.Warn("replay-all: failed to claim task",
			"task_id", task.ID,
			"pipeline_id", task.PipelineID,
			"error", err.Error(),
		)
		return "", err
	}
	newTask, err := s.broker.Submit(ctx, claimed.PipelineID, claimed.Payload)
	if err != nil {
		s.logger.Warn("replay-all: failed to submit replay task",
			"task_id", task.ID,
			"pipeline_id", task.PipelineID,
			"error", err.Error(),
		)
		if rbErr := s.store.RollbackReplayClaim(ctx, task.ID); rbErr != nil {
			s.logger.Error("replay-all rollback failed: task may be stranded in REPLAY_PENDING",
				"task_id", task.ID,
				"submit_error", err.Error(),
				"rollback_error", rbErr.Error(),
			)
		}
		return "", err
	}
	replayed := broker.TaskStateReplayed
	if markErr := s.store.UpdateTask(ctx, task.ID, broker.TaskUpdate{State: &replayed}); markErr != nil {
		s.logger.Warn("replay-all: failed to mark original task as REPLAYED",
			"task_id", task.ID,
			"error", markErr.Error(),
		)
	}
	return newTask.ID, nil
}

// DiscardAll discards all dead-lettered tasks matching the filter.
// pipelineID may be empty to discard across all pipelines.
// maxTasks caps the total number of tasks processed; use 0 for DefaultMaxTasks.
// onProgress is called after each task is processed (may be nil).
//
// Each individual discard goes through Store.DiscardDeadLetter so a task
// a concurrent replay claimed between our list page and our write fails
// cleanly with ErrTaskNotDiscardable instead of silently overwriting the
// winning replay's state. Tasks that are already DISCARDED (e.g. because
// a parallel bulk discard raced us) are counted as processed for the
// caller's idempotent-retry shape.
func (s *Service) DiscardAll(ctx context.Context, pipelineID string, maxTasks int, onProgress ProgressFunc) (BulkResult, error) {
	ceiling := clampMax(maxTasks)

	deadLetter := true
	failedState := broker.TaskStateFailed
	filter := broker.TaskFilter{
		State:              &failedState,
		RoutedToDeadLetter: &deadLetter,
		Limit:              pageSize,
	}
	if pipelineID != "" {
		filter.PipelineID = &pipelineID
	}

	count := 0
	truncated := false
	// failedIDs tracks task IDs that failed to discard so a reappearance
	// on a subsequent page is not retried or double-counted.
	failedIDs := make(map[string]struct{})

	for count < ceiling {
		page, err := s.store.ListTasks(ctx, filter)
		if err != nil {
			return BulkResult{Processed: count, Failed: len(failedIDs), Truncated: truncated}, fmt.Errorf("list dead-letter tasks: %w", err)
		}
		if len(page.Tasks) == 0 {
			break
		}
		progressed := false
		for _, task := range page.Tasks {
			if count >= ceiling {
				truncated = true
				break
			}
			if _, already := failedIDs[task.ID]; already {
				continue
			}
			err := s.store.DiscardDeadLetter(ctx, task.ID)
			switch {
			case err == nil, errors.Is(err, store.ErrTaskAlreadyDiscarded):
				// Count already-discarded as processed — a concurrent
				// bulk discard won this task, so the caller's aggregate
				// still reflects reality.
				count++
				progressed = true
				if onProgress != nil {
					onProgress(task.ID, "", nil)
				}
			case errors.Is(err, store.ErrTaskNotDiscardable):
				// Task left the dead-letter state between our list and
				// our discard (most likely a concurrent replay claim).
				// Not a user-facing error — skip without counting.
				s.logger.Debug("discard-all: task left dead-letter state before discard",
					"task_id", task.ID,
					"pipeline_id", task.PipelineID,
				)
				failedIDs[task.ID] = struct{}{}
			default:
				s.logger.Warn("discard-all: failed to discard task",
					"task_id", task.ID,
					"pipeline_id", task.PipelineID,
					"error", err.Error(),
				)
				failedIDs[task.ID] = struct{}{}
				if onProgress != nil {
					onProgress(task.ID, "", err)
				}
			}
		}
		if !progressed {
			// If no task in the current page could be updated, break to
			// avoid an infinite loop (e.g. a permissions or store error
			// that will not recover).
			break
		}
	}
	if count >= ceiling {
		truncated = true
		s.logger.Warn("discard-all hit per-call ceiling",
			"pipeline_id", pipelineID,
			"ceiling", ceiling,
			"count", count,
		)
	}

	return BulkResult{Processed: count, Failed: len(failedIDs), Truncated: truncated}, nil
}

// Count returns the number of dead-lettered tasks matching the filter.
// pipelineID may be empty to count across all pipelines. This uses a
// ListTasks call with Limit:1 to read Total cheaply.
func (s *Service) Count(ctx context.Context, pipelineID string) (int, error) {
	deadLetter := true
	failedState := broker.TaskStateFailed
	filter := broker.TaskFilter{
		State:              &failedState,
		RoutedToDeadLetter: &deadLetter,
		Limit:              1,
	}
	if pipelineID != "" {
		filter.PipelineID = &pipelineID
	}
	result, err := s.store.ListTasks(ctx, filter)
	if err != nil {
		return 0, err
	}
	return result.Total, nil
}
