// Package mock provides a test-only store.Store implementation that
// delegates to an in-memory backend by default and lets individual tests
// inject failures on a per-method basis. The default-delegating design
// means tests only need to override the methods they care about — the
// rest continue to behave like a real working store.
package mock

import (
	"context"
	"errors"
	"sync"

	"github.com/brianbuquoi/overlord/internal/broker"
	"github.com/brianbuquoi/overlord/internal/store"
	"github.com/brianbuquoi/overlord/internal/store/memory"
)

// ErrInjected is the error returned by the fault-injection constructors for
// task IDs that should fail. Tests can compare against this to distinguish
// injected failures from real store errors.
var ErrInjected = errors.New("injected failure")

// Store is a mock store.Store implementation. Each method checks its
// corresponding hook field; if the hook is non-nil it is called, otherwise
// the call is delegated to the embedded memory store.
type Store struct {
	mu sync.RWMutex

	mem *memory.MemoryStore

	OnEnqueueTask    func(ctx context.Context, stageID string, task *broker.Task) error
	OnDequeueTask    func(ctx context.Context, stageID string) (*broker.Task, error)
	OnUpdateTask     func(ctx context.Context, taskID string, update broker.TaskUpdate) error
	OnGetTask        func(ctx context.Context, taskID string) (*broker.Task, error)
	OnListTasks      func(ctx context.Context, filter broker.TaskFilter) (*broker.ListTasksResult, error)
	OnClaimForReplay func(ctx context.Context, taskID string) (*broker.Task, error)
}

// Compile-time assertion that *Store satisfies store.Store.
var _ store.Store = (*Store)(nil)

// New returns a mock store backed by an empty in-memory store with no
// hooks configured. It behaves identically to memory.New() until a caller
// sets one of the On* fields.
func New() *Store {
	return &Store{mem: memory.New()}
}

// NewWithFailingSubmit returns a mock Store where EnqueueTask returns
// ErrInjected for the specified task IDs. The match is on the task ID
// at enqueue time.
//
// Note: for handlers that generate new task IDs at submission time
// (such as replay-all), the original dead-letter task ID will not match
// the newly-generated ID. In those cases, set OnEnqueueTask directly
// with a call-count gate or other logic instead of using this constructor.
func NewWithFailingSubmit(failIDs ...string) *Store {
	m := New()
	fail := toSet(failIDs)
	m.OnEnqueueTask = func(ctx context.Context, stageID string, task *broker.Task) error {
		if _, bad := fail[task.ID]; bad {
			return ErrInjected
		}
		return m.mem.EnqueueTask(ctx, stageID, task)
	}
	return m
}

// NewWithFailingUpdate returns a mock Store where UpdateTask returns
// ErrInjected for the specified task IDs. The match is on the task ID
// at update time.
//
// Note: if the handler being tested generates or transforms task IDs
// before calling UpdateTask, set OnUpdateTask directly instead of
// using this constructor.
func NewWithFailingUpdate(failIDs ...string) *Store {
	m := New()
	fail := toSet(failIDs)
	m.OnUpdateTask = func(ctx context.Context, taskID string, update broker.TaskUpdate) error {
		if _, bad := fail[taskID]; bad {
			return ErrInjected
		}
		return m.mem.UpdateTask(ctx, taskID, update)
	}
	return m
}

// Memory exposes the underlying memory store so tests can seed or inspect
// task state directly. Useful for tests that need to prepopulate the
// store with dead-lettered tasks before exercising a handler.
func (s *Store) Memory() *memory.MemoryStore {
	return s.mem
}

func (s *Store) EnqueueTask(ctx context.Context, stageID string, task *broker.Task) error {
	s.mu.RLock()
	h := s.OnEnqueueTask
	s.mu.RUnlock()
	if h != nil {
		return h(ctx, stageID, task)
	}
	return s.mem.EnqueueTask(ctx, stageID, task)
}

func (s *Store) DequeueTask(ctx context.Context, stageID string) (*broker.Task, error) {
	s.mu.RLock()
	h := s.OnDequeueTask
	s.mu.RUnlock()
	if h != nil {
		return h(ctx, stageID)
	}
	return s.mem.DequeueTask(ctx, stageID)
}

func (s *Store) UpdateTask(ctx context.Context, taskID string, update broker.TaskUpdate) error {
	s.mu.RLock()
	h := s.OnUpdateTask
	s.mu.RUnlock()
	if h != nil {
		return h(ctx, taskID, update)
	}
	return s.mem.UpdateTask(ctx, taskID, update)
}

func (s *Store) GetTask(ctx context.Context, taskID string) (*broker.Task, error) {
	s.mu.RLock()
	h := s.OnGetTask
	s.mu.RUnlock()
	if h != nil {
		return h(ctx, taskID)
	}
	return s.mem.GetTask(ctx, taskID)
}

func (s *Store) ListTasks(ctx context.Context, filter broker.TaskFilter) (*broker.ListTasksResult, error) {
	s.mu.RLock()
	h := s.OnListTasks
	s.mu.RUnlock()
	if h != nil {
		return h(ctx, filter)
	}
	return s.mem.ListTasks(ctx, filter)
}

func (s *Store) ClaimForReplay(ctx context.Context, taskID string) (*broker.Task, error) {
	s.mu.RLock()
	h := s.OnClaimForReplay
	s.mu.RUnlock()
	if h != nil {
		return h(ctx, taskID)
	}
	return s.mem.ClaimForReplay(ctx, taskID)
}

func toSet(ids []string) map[string]struct{} {
	set := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		set[id] = struct{}{}
	}
	return set
}
