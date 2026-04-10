// Package memory implements an in-memory Store backend for development and
// testing. Data does not survive process restarts.
package memory

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/orcastrator/orcastrator/internal/broker"
	"github.com/orcastrator/orcastrator/internal/store"
)

// MemoryStore is a thread-safe, in-memory implementation of store.Store.
type MemoryStore struct {
	mu        sync.RWMutex
	tasks     map[string]*broker.Task // taskID → task
	queues    map[string][]string     // stageID → ordered task IDs
	taskOrder []string                // insertion-ordered task IDs for deterministic listing
}

// copyTask returns a deep copy of a task, including Metadata map and Payload slice.
func copyTask(t *broker.Task) *broker.Task {
	cp := *t
	if t.Metadata != nil {
		cp.Metadata = make(map[string]any, len(t.Metadata))
		for k, v := range t.Metadata {
			cp.Metadata[k] = v
		}
	}
	if t.Payload != nil {
		cp.Payload = make(json.RawMessage, len(t.Payload))
		copy(cp.Payload, t.Payload)
	}
	return &cp
}

// New creates a new MemoryStore.
func New() *MemoryStore {
	return &MemoryStore{
		tasks:     make(map[string]*broker.Task),
		queues:    make(map[string][]string),
		taskOrder: nil,
	}
}

func (m *MemoryStore) EnqueueTask(_ context.Context, stageID string, task *broker.Task) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Store a deep copy to prevent external mutation.
	_, exists := m.tasks[task.ID]
	m.tasks[task.ID] = copyTask(task)
	if !exists {
		m.taskOrder = append(m.taskOrder, task.ID)
	}
	m.queues[stageID] = append(m.queues[stageID], task.ID)
	return nil
}

func (m *MemoryStore) DequeueTask(_ context.Context, stageID string) (*broker.Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now()
	q := m.queues[stageID]

	for len(q) > 0 {
		id := q[0]
		q = q[1:]

		task, ok := m.tasks[id]
		if !ok {
			continue
		}

		// Skip expired tasks.
		if !task.ExpiresAt.IsZero() && now.After(task.ExpiresAt) {
			delete(m.tasks, id)
			continue
		}

		m.queues[stageID] = q
		return copyTask(task), nil
	}

	m.queues[stageID] = q
	return nil, store.ErrQueueEmpty
}

func (m *MemoryStore) UpdateTask(_ context.Context, taskID string, update broker.TaskUpdate) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	task, ok := m.tasks[taskID]
	if !ok {
		return store.ErrTaskNotFound
	}

	if update.State != nil {
		task.State = *update.State
	}
	if update.StageID != nil {
		task.StageID = *update.StageID
	}
	if update.Payload != nil {
		task.Payload = *update.Payload
	}
	if update.Metadata != nil {
		if task.Metadata == nil {
			task.Metadata = make(map[string]any)
		}
		for k, v := range update.Metadata {
			task.Metadata[k] = v
		}
	}
	if update.Attempts != nil {
		task.Attempts = *update.Attempts
	}
	if update.InputSchemaName != nil {
		task.InputSchemaName = *update.InputSchemaName
	}
	if update.InputSchemaVersion != nil {
		task.InputSchemaVersion = *update.InputSchemaVersion
	}
	if update.OutputSchemaName != nil {
		task.OutputSchemaName = *update.OutputSchemaName
	}
	if update.OutputSchemaVersion != nil {
		task.OutputSchemaVersion = *update.OutputSchemaVersion
	}
	if update.MaxAttempts != nil {
		task.MaxAttempts = *update.MaxAttempts
	}
	if update.RoutedToDeadLetter != nil {
		task.RoutedToDeadLetter = *update.RoutedToDeadLetter
	}
	if update.CrossStageTransitions != nil {
		task.CrossStageTransitions = *update.CrossStageTransitions
	}
	task.UpdatedAt = time.Now()
	return nil
}

func (m *MemoryStore) GetTask(_ context.Context, taskID string) (*broker.Task, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	task, ok := m.tasks[taskID]
	if !ok {
		return nil, store.ErrTaskNotFound
	}

	// Skip expired tasks.
	if !task.ExpiresAt.IsZero() && time.Now().After(task.ExpiresAt) {
		return nil, store.ErrTaskNotFound
	}

	return copyTask(task), nil
}

func (m *MemoryStore) ListTasks(_ context.Context, filter broker.TaskFilter) (*broker.ListTasksResult, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	now := time.Now()

	// First pass: collect all matching task IDs in insertion order.
	var matching []string
	for _, id := range m.taskOrder {
		task, ok := m.tasks[id]
		if !ok {
			continue
		}
		if !task.ExpiresAt.IsZero() && now.After(task.ExpiresAt) {
			continue
		}
		if filter.PipelineID != nil && task.PipelineID != *filter.PipelineID {
			continue
		}
		if filter.StageID != nil && task.StageID != *filter.StageID {
			continue
		}
		if filter.State != nil && task.State != *filter.State {
			continue
		}
		// Exclude DISCARDED tasks by default unless explicitly included.
		if !filter.IncludeDiscarded && task.State == broker.TaskStateDiscarded {
			continue
		}
		if filter.RoutedToDeadLetter != nil && task.RoutedToDeadLetter != *filter.RoutedToDeadLetter {
			continue
		}
		matching = append(matching, id)
	}

	total := len(matching)

	// Apply offset.
	if filter.Offset > 0 && filter.Offset < len(matching) {
		matching = matching[filter.Offset:]
	} else if filter.Offset >= len(matching) {
		matching = nil
	}

	// Apply limit.
	if filter.Limit > 0 && len(matching) > filter.Limit {
		matching = matching[:filter.Limit]
	}

	results := make([]*broker.Task, 0, len(matching))
	for _, id := range matching {
		results = append(results, copyTask(m.tasks[id]))
	}

	return &broker.ListTasksResult{Tasks: results, Total: total}, nil
}
