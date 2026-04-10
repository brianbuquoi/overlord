// Package budget implements retry budget tracking with sliding time windows.
// Budgets limit the total number of retry attempts allowed within a window,
// preventing runaway API consumption at the pipeline or agent level.
package budget

import (
	"context"
	"sync"
	"time"

	"github.com/orcastrator/orcastrator/internal/config"
)

// Budget holds the configuration for a single retry budget.
type Budget struct {
	MaxRetries  int
	Window      time.Duration
	OnExhausted string // "fail" (default) or "wait"
}

// FromConfig creates a Budget from a config.RetryBudgetConfig.
func FromConfig(cfg *config.RetryBudgetConfig) *Budget {
	if cfg == nil {
		return nil
	}
	onExhausted := cfg.OnExhausted
	if onExhausted == "" {
		onExhausted = "fail"
	}
	return &Budget{
		MaxRetries:  cfg.MaxRetries,
		Window:      cfg.Window.Duration,
		OnExhausted: onExhausted,
	}
}

// Tracker tracks retry budget consumption using an in-memory sliding window.
// Thread-safe for concurrent access.
type Tracker struct {
	mu      sync.Mutex
	entries map[string][]time.Time // key → sorted timestamps of retry events
	nowFn   func() time.Time       // replaceable for testing
}

// NewTracker creates a new budget tracker.
func NewTracker() *Tracker {
	return &Tracker{
		entries: make(map[string][]time.Time),
		nowFn:   time.Now,
	}
}

// SetNowFunc replaces the time source. For testing only.
func (t *Tracker) SetNowFunc(fn func() time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.nowFn = fn
}

// PipelineKey returns the budget key for a pipeline.
func PipelineKey(pipelineID string) string {
	return "budget:pipeline:" + pipelineID
}

// AgentKey returns the budget key for an agent.
func AgentKey(agentID string) string {
	return "budget:agent:" + agentID
}

// IsExhausted checks if the budget for the given key is exhausted without
// incrementing the counter.
func (t *Tracker) IsExhausted(_ context.Context, key string, budget *Budget) (bool, error) {
	if budget == nil {
		return false, nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	t.pruneExpired(key, budget.Window)
	return len(t.entries[key]) >= budget.MaxRetries, nil
}

// Increment records a retry event for the given key and returns the current
// count within the window (after increment).
func (t *Tracker) Increment(_ context.Context, key string, window time.Duration) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.pruneExpired(key, window)
	t.entries[key] = append(t.entries[key], t.nowFn())
	return len(t.entries[key]), nil
}

// IncrementIfNotExhausted atomically checks whether the budget is exhausted
// and, only if not, records a retry event. Returns (allowed, currentCount, err).
// This eliminates the TOCTOU race between a separate IsExhausted + Increment.
func (t *Tracker) IncrementIfNotExhausted(_ context.Context, key string, budget *Budget) (bool, int, error) {
	if budget == nil {
		return true, 0, nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	t.pruneExpired(key, budget.Window)
	if len(t.entries[key]) >= budget.MaxRetries {
		return false, len(t.entries[key]), nil
	}
	t.entries[key] = append(t.entries[key], t.nowFn())
	return true, len(t.entries[key]), nil
}

// Current returns the current retry count within the window for the given key.
func (t *Tracker) Current(_ context.Context, key string, window time.Duration) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.pruneExpired(key, window)
	return len(t.entries[key]), nil
}

// Decrement removes the most recent entry for the given key. Used to roll back
// a pipeline budget increment when the subsequent agent budget check fails.
func (t *Tracker) Decrement(_ context.Context, key string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	entries := t.entries[key]
	if len(entries) > 0 {
		t.entries[key] = entries[:len(entries)-1]
	}
}

// pruneExpired removes entries outside the sliding window. Must hold t.mu.
func (t *Tracker) pruneExpired(key string, window time.Duration) {
	entries := t.entries[key]
	if len(entries) == 0 {
		return
	}
	cutoff := t.nowFn().Add(-window)
	// Find first entry within window using linear scan (entries are sorted).
	i := 0
	for i < len(entries) && entries[i].Before(cutoff) {
		i++
	}
	if i > 0 {
		t.entries[key] = entries[i:]
	}
}
