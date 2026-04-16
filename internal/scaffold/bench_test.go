package scaffold

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// BenchmarkWriteHello measures the wall-clock cost of materializing the
// `hello` template into a fresh tempdir via the public scaffold.Write
// contract. The benchmark exists so regressions in embed-walking, template
// rendering, or the commit path show up in `go test -bench=.` reports;
// the hard time-budget gate for the full init+demo path lives in
// cmd/overlord/template_ci_test.go where runDemo is reachable.
func BenchmarkWriteHello(b *testing.B) {
	ctx := context.Background()
	for i := 0; i < b.N; i++ {
		tmp := b.TempDir()
		_, err := Write(ctx, "hello", filepath.Join(tmp, "out"), Options{})
		if err != nil {
			b.Fatalf("scaffold.Write: %v", err)
		}
	}
}

// BenchmarkWriteSummarize measures the same path for the two-stage
// summarize template (larger embed set).
func BenchmarkWriteSummarize(b *testing.B) {
	ctx := context.Background()
	for i := 0; i < b.N; i++ {
		tmp := b.TempDir()
		_, err := Write(ctx, "summarize", filepath.Join(tmp, "out"), Options{})
		if err != nil {
			b.Fatalf("scaffold.Write: %v", err)
		}
	}
}

// TestWriteTimeBudget asserts scaffold.Write alone (no demo) finishes in
// well under the worst-case budget for every shipped template. This is a
// ceiling-not-floor guard — 3 seconds is ~30x the expected <100ms budget
// so CI noise doesn't flake the test. The point is to catch a pathological
// regression (e.g. accidentally re-parsing the embed FS per file, O(n^2)
// walks). The full end-to-end scaffold+demo budget lives alongside
// runDemo in cmd/overlord/template_ci_test.go.
func TestWriteTimeBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("time-budget guard skipped in -short mode")
	}
	const budget = 3 * time.Second
	for _, name := range ListTemplates() {
		name := name
		t.Run(name, func(t *testing.T) {
			start := time.Now()
			tmp := t.TempDir()
			_, err := Write(context.Background(), name, filepath.Join(tmp, "out"), Options{})
			if err != nil {
				t.Fatalf("scaffold.Write: %v", err)
			}
			elapsed := time.Since(start)
			if elapsed > budget {
				t.Errorf("scaffold.Write(%s) took %s (budget %s)", name, elapsed, budget)
			}
			t.Logf("scaffold.Write(%s) wall-clock: %s (budget %s)", name, elapsed, budget)
		})
	}
}
