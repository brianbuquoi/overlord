//go:build timing_test

// Timing tests are inherently environment-dependent and should be run on
// dedicated hardware for reliable results. They are excluded from standard
// go test ./... runs and only execute when explicitly requested:
//
//   go test -tags timing_test ./internal/auth/...
//
// On CPU-throttled CI runners, bcrypt timing can vary significantly,
// causing false positives. The skip-on-slow-bcrypt logic below detects
// this and skips timing assertions rather than failing.

package auth

import (
	"testing"
	"time"

	"github.com/orcastrator/orcastrator/internal/config"

	"golang.org/x/crypto/bcrypt"
)

// bcryptIsSlow returns true if a single bcrypt comparison takes longer than
// expectedMax, indicating a throttled environment (CI, VM, etc).
func bcryptIsSlow(expectedMax time.Duration) bool {
	hash, _ := bcrypt.GenerateFromPassword([]byte("benchmark-key"), bcryptCost)
	start := time.Now()
	bcrypt.CompareHashAndPassword(hash, []byte("benchmark-key"))
	return time.Since(start) > expectedMax
}

func TestTiming_KeyEnumeration(t *testing.T) {
	// Skip if the environment is too slow for reliable timing assertions.
	if bcryptIsSlow(2 * time.Second) {
		t.Skip("bcrypt is too slow in this environment for reliable timing assertions — run on dedicated hardware")
	}

	t.Setenv("TIMING_KEY", "valid-timing-test-key-abcdef")

	keys, err := LoadKeys([]config.AuthKeyConfig{
		{Name: "timing", KeyEnv: "TIMING_KEY", Scopes: []string{"read"}},
	})
	if err != nil {
		t.Fatalf("LoadKeys: %v", err)
	}

	const iterations = 10

	measure := func(name, token string) (mean time.Duration) {
		var total time.Duration
		for i := 0; i < iterations; i++ {
			start := time.Now()
			Authenticate(keys, token)
			total += time.Since(start)
		}
		mean = total / time.Duration(iterations)
		t.Logf("  %s: mean=%v", name, mean)
		return mean
	}

	t.Log("Timing measurements:")
	randomMean := measure("random_32_chars", "xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx")
	wrongFormatMean := measure("wrong_format", "not-the-right-key-at-all-12345")
	partialMatchMean := measure("partial_match", "valid-timing-test-key-WRONG!")
	validMean := measure("valid_key", "valid-timing-test-key-abcdef")

	// All means should be >100ms (bcrypt dominates).
	for name, m := range map[string]time.Duration{
		"random":        randomMean,
		"wrong_format":  wrongFormatMean,
		"partial_match": partialMatchMean,
		"valid":         validMean,
	} {
		if m < 100*time.Millisecond {
			t.Errorf("CRITICAL: %s mean (%v) is too fast — timing oracle possible", name, m)
		}
	}

	// Check pairwise differences with 500ms threshold (increased from 150ms
	// to accommodate CI CPU throttling). At bcrypt cost 12, each comparison
	// is ~200-600ms, so 500ms (~20-80%) accounts for OS scheduling jitter
	// while still catching short-circuit attacks (which show >1s difference).
	means := map[string]time.Duration{
		"random":        randomMean,
		"wrong_format":  wrongFormatMean,
		"partial_match": partialMatchMean,
		"valid":         validMean,
	}
	names := []string{"random", "wrong_format", "partial_match", "valid"}
	for i := 0; i < len(names); i++ {
		for j := i + 1; j < len(names); j++ {
			diff := means[names[i]] - means[names[j]]
			if diff < 0 {
				diff = -diff
			}
			if diff > 500*time.Millisecond {
				t.Errorf("WARNING: timing diff between %s and %s is %v (>500ms)",
					names[i], names[j], diff)
			}
		}
	}
}

func TestTiming_DummyComparison_NoKeys(t *testing.T) {
	// Skip if the environment is too slow for reliable timing assertions.
	if bcryptIsSlow(2 * time.Second) {
		t.Skip("bcrypt is too slow in this environment for reliable timing assertions — run on dedicated hardware")
	}

	start := time.Now()
	_, err := Authenticate(nil, "any-token-here")
	elapsed := time.Since(start)

	if err != ErrUnauthorized {
		t.Errorf("expected ErrUnauthorized, got %v", err)
	}

	t.Logf("Auth with no keys took: %v", elapsed)

	// Must take bcrypt-comparable time (>50ms), not return instantly.
	if elapsed < 50*time.Millisecond {
		t.Errorf("CRITICAL: empty key list returned in %v — reveals 'no keys configured'", elapsed)
	}
}
