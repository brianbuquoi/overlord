package budget_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/orcastrator/orcastrator/internal/budget"
)

func TestTracker_IncrementAndCurrent(t *testing.T) {
	tr := budget.NewTracker()
	ctx := context.Background()

	count, err := tr.Increment(ctx, "test-key", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("expected count 1, got %d", count)
	}

	count, err = tr.Increment(ctx, "test-key", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("expected count 2, got %d", count)
	}

	cur, err := tr.Current(ctx, "test-key", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if cur != 2 {
		t.Fatalf("expected current 2, got %d", cur)
	}
}

func TestTracker_SlidingWindowExpiry(t *testing.T) {
	tr := budget.NewTracker()
	ctx := context.Background()

	now := time.Now()
	var mu sync.Mutex
	tr.SetNowFunc(func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	})

	// Add 5 entries within a 200ms window.
	for i := 0; i < 5; i++ {
		tr.Increment(ctx, "test", 200*time.Millisecond)
	}

	cur, _ := tr.Current(ctx, "test", 200*time.Millisecond)
	if cur != 5 {
		t.Fatalf("expected 5, got %d", cur)
	}

	// Advance time past the window.
	mu.Lock()
	now = now.Add(250 * time.Millisecond)
	mu.Unlock()

	cur, _ = tr.Current(ctx, "test", 200*time.Millisecond)
	if cur != 0 {
		t.Fatalf("expected 0 after window expired, got %d", cur)
	}
}

func TestTracker_IsExhausted(t *testing.T) {
	tr := budget.NewTracker()
	ctx := context.Background()

	b := &budget.Budget{MaxRetries: 3, Window: time.Hour, OnExhausted: "fail"}

	// Not exhausted initially.
	exhausted, _ := tr.IsExhausted(ctx, "key", b)
	if exhausted {
		t.Fatal("should not be exhausted initially")
	}

	// Add 3 entries.
	for i := 0; i < 3; i++ {
		tr.Increment(ctx, "key", time.Hour)
	}

	// Now exhausted.
	exhausted, _ = tr.IsExhausted(ctx, "key", b)
	if !exhausted {
		t.Fatal("should be exhausted at max_retries")
	}
}

func TestTracker_NilBudget(t *testing.T) {
	tr := budget.NewTracker()
	ctx := context.Background()

	exhausted, err := tr.IsExhausted(ctx, "key", nil)
	if err != nil {
		t.Fatal(err)
	}
	if exhausted {
		t.Fatal("nil budget should never be exhausted")
	}
}

func TestTracker_ConcurrentAtomicity(t *testing.T) {
	tr := budget.NewTracker()
	ctx := context.Background()

	const goroutines = 20
	var wg sync.WaitGroup

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tr.Increment(ctx, "concurrent", time.Hour)
		}()
	}
	wg.Wait()

	cur, _ := tr.Current(ctx, "concurrent", time.Hour)
	if cur != goroutines {
		t.Fatalf("expected %d, got %d", goroutines, cur)
	}
}

func TestTracker_ConcurrentBudgetEnforcement(t *testing.T) {
	tr := budget.NewTracker()
	ctx := context.Background()

	b := &budget.Budget{MaxRetries: 10, Window: time.Hour, OnExhausted: "fail"}

	const goroutines = 20
	var allowed atomic.Int32
	var rejected atomic.Int32
	var wg sync.WaitGroup

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			exhausted, _ := tr.IsExhausted(ctx, "race", b)
			if exhausted {
				rejected.Add(1)
				return
			}
			tr.Increment(ctx, "race", time.Hour)
			allowed.Add(1)
		}()
	}
	wg.Wait()

	// With race conditions, the exact split may vary, but the total
	// count should not exceed the budget by more than the goroutine count
	// (due to check-then-act).
	cur, _ := tr.Current(ctx, "race", time.Hour)
	t.Logf("allowed=%d rejected=%d final_count=%d", allowed.Load(), rejected.Load(), cur)
	// The budget tracker is advisory — under concurrent load, some extra
	// entries may slip through due to check-then-act race. That's documented.
}

// Test IncrementIfNotExhausted: atomic check+increment prevents TOCTOU.
func TestTracker_IncrementIfNotExhausted_Atomic(t *testing.T) {
	tr := budget.NewTracker()
	ctx := context.Background()

	b := &budget.Budget{MaxRetries: 10, Window: time.Hour, OnExhausted: "fail"}

	const goroutines = 50
	var allowed atomic.Int32
	var rejected atomic.Int32
	var wg sync.WaitGroup

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, _, _ := tr.IncrementIfNotExhausted(ctx, "atomic", b)
			if ok {
				allowed.Add(1)
			} else {
				rejected.Add(1)
			}
		}()
	}
	wg.Wait()

	// With atomic check+increment, exactly 10 should be allowed.
	if allowed.Load() != 10 {
		t.Fatalf("expected exactly 10 allowed, got %d (rejected=%d)", allowed.Load(), rejected.Load())
	}
	if rejected.Load() != goroutines-10 {
		t.Fatalf("expected exactly %d rejected, got %d", goroutines-10, rejected.Load())
	}

	cur, _ := tr.Current(ctx, "atomic", time.Hour)
	if cur != 10 {
		t.Fatalf("expected final count 10, got %d", cur)
	}
}

// Test IncrementIfNotExhausted: nil budget always allowed.
func TestTracker_IncrementIfNotExhausted_NilBudget(t *testing.T) {
	tr := budget.NewTracker()
	ctx := context.Background()

	ok, count, err := tr.IncrementIfNotExhausted(ctx, "key", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("nil budget should always allow")
	}
	if count != 0 {
		t.Fatalf("expected count 0 for nil budget, got %d", count)
	}
}

// Test Decrement rolls back the most recent entry.
func TestTracker_Decrement(t *testing.T) {
	tr := budget.NewTracker()
	ctx := context.Background()

	tr.Increment(ctx, "key", time.Hour)
	tr.Increment(ctx, "key", time.Hour)
	tr.Increment(ctx, "key", time.Hour)

	cur, _ := tr.Current(ctx, "key", time.Hour)
	if cur != 3 {
		t.Fatalf("expected 3, got %d", cur)
	}

	tr.Decrement(ctx, "key")
	cur, _ = tr.Current(ctx, "key", time.Hour)
	if cur != 2 {
		t.Fatalf("expected 2 after decrement, got %d", cur)
	}
}

// Test Decrement on empty key is a no-op.
func TestTracker_Decrement_Empty(t *testing.T) {
	tr := budget.NewTracker()
	ctx := context.Background()

	// Should not panic on empty key.
	tr.Decrement(ctx, "nonexistent")

	cur, _ := tr.Current(ctx, "nonexistent", time.Hour)
	if cur != 0 {
		t.Fatalf("expected 0, got %d", cur)
	}
}

func TestPipelineKey(t *testing.T) {
	if got := budget.PipelineKey("my-pipe"); got != "budget:pipeline:my-pipe" {
		t.Fatalf("unexpected key: %s", got)
	}
}

func TestAgentKey(t *testing.T) {
	if got := budget.AgentKey("my-agent"); got != "budget:agent:my-agent" {
		t.Fatalf("unexpected key: %s", got)
	}
}
