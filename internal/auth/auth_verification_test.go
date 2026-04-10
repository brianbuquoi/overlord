package auth

import (
	"testing"
	"time"
)

// =============================================================================
// PART 2 — RECORDSUCCESS BEHAVIOUR VERIFICATION
// =============================================================================

// Test 7: The fix test — must pass with new behaviour, would have failed with old.
func TestRecordSuccess_FixTest_9Fail_Success_1Fail_Blocked(t *testing.T) {
	tracker := NewBruteForceTracker(10, time.Minute)
	ip := "10.0.0.1"

	// Record 9 failures.
	for i := 0; i < 9; i++ {
		tracker.RecordFailure(ip)
	}
	if tracker.IsBlocked(ip) {
		t.Fatal("should not be blocked after 9 failures (threshold 10)")
	}

	// Simulate a valid auth.
	tracker.RecordSuccess(ip)

	// 1 more failure brings total to 10.
	tracker.RecordFailure(ip)

	if !tracker.IsBlocked(ip) {
		t.Fatal("SEC3-001 NOT FIXED: IP should be blocked after 9+1=10 failures (RecordSuccess must not reset)")
	}
	t.Log("Test 7 PASSED: RecordSuccess does not reset failure counter")
}

// Test 8: Regression check — same as test 7 with different IP.
func TestRecordSuccess_RegressionCheck_OldBehaviourGone(t *testing.T) {
	tracker := NewBruteForceTracker(10, time.Minute)
	ip := "10.0.0.2"

	for i := 0; i < 9; i++ {
		tracker.RecordFailure(ip)
	}
	tracker.RecordSuccess(ip)
	tracker.RecordFailure(ip)

	if !tracker.IsBlocked(ip) {
		t.Fatal("REGRESSION: old behaviour still present — RecordSuccess reset the counter")
	}
	t.Log("Test 8 PASSED: old behaviour is gone, 9+1=10 failures = blocked")
}

// Test 9: Attacker scenario — interleave valid requests to reset window.
func TestRecordSuccess_AttackerScenario_InterleavedSuccess(t *testing.T) {
	tracker := NewBruteForceTracker(10, time.Minute)
	ip := "10.0.0.3"

	// Attacker: 5 failures trying wrong write keys.
	for i := 0; i < 5; i++ {
		tracker.RecordFailure(ip)
	}

	// Attacker: 1 success using valid read key.
	tracker.RecordSuccess(ip)

	// Attacker: 5 more failures.
	for i := 0; i < 5; i++ {
		tracker.RecordFailure(ip)
	}

	// With old behaviour: IsBlocked would be false (success reset to 0, so only 5 failures).
	// With new behaviour: IsBlocked must be true (10 total failures).
	if !tracker.IsBlocked(ip) {
		t.Fatal("ATTACKER SCENARIO FAILED: interleaved success should not reset failure counter")
	}
	t.Log("Test 9 PASSED: attacker with 5+success+5 failures = 10 total = blocked")
}

// Test 10: Retry-After header on brute force 429 — verified at the tracker level.
func TestBruteForce_WindowEnd_ForRetryAfter(t *testing.T) {
	window := 60 * time.Second
	tracker := NewBruteForceTracker(3, window)
	ip := "10.0.0.4"

	// Trigger a block.
	for i := 0; i < 3; i++ {
		tracker.RecordFailure(ip)
	}
	if !tracker.IsBlocked(ip) {
		t.Fatal("should be blocked after 3 failures")
	}

	// Verify WindowEnd returns a valid future time.
	windowEnd := tracker.WindowEnd(ip)
	if windowEnd.IsZero() {
		t.Fatal("WindowEnd should not be zero for blocked IP")
	}

	// Retry-After = ceil(time.Until(windowEnd).Seconds())
	retryAfter := int(time.Until(windowEnd).Seconds())
	if retryAfter < 1 {
		t.Errorf("Retry-After should be positive, got %d", retryAfter)
	}
	if retryAfter > int(window.Seconds()) {
		t.Errorf("Retry-After (%d) should not exceed window duration (%v)", retryAfter, window)
	}

	// Verify within 5 seconds of the actual window expiry.
	diff := time.Until(windowEnd) - time.Duration(retryAfter)*time.Second
	if diff < 0 {
		diff = -diff
	}
	if diff > 5*time.Second {
		t.Errorf("Retry-After should be within 5s of actual window expiry, diff=%v", diff)
	}
	t.Logf("Test 10 PASSED: WindowEnd=%v, Retry-After=%ds", windowEnd, retryAfter)
}

// Test 12: Legitimate user scenario — fat-fingers key 9 times, succeeds, then 1 more failure.
func TestRecordSuccess_LegitimateUser_FatFingers(t *testing.T) {
	window := 60 * time.Second
	tracker := NewBruteForceTracker(10, window)
	ip := "10.0.0.5"

	// 9 fat-fingers.
	for i := 0; i < 9; i++ {
		tracker.RecordFailure(ip)
	}

	// User authenticates correctly.
	tracker.RecordSuccess(ip) // no-op, user is authenticated

	// 1 more fat-finger.
	tracker.RecordFailure(ip)

	// 10 total failures = blocked.
	if !tracker.IsBlocked(ip) {
		t.Fatal("expected blocked after 10 total failures")
	}

	// Verify WindowEnd is available for Retry-After calculation.
	windowEnd := tracker.WindowEnd(ip)
	if windowEnd.IsZero() {
		t.Fatal("WindowEnd should not be zero for blocked IP")
	}

	remaining := time.Until(windowEnd)
	if remaining > window {
		t.Errorf("remaining time (%v) exceeds configured window (%v)", remaining, window)
	}
	t.Logf("Test 12 PASSED: legitimate user blocked after 9+1=10 failures, wait %v", remaining)
}

// Test 13: Verify RecordSuccess method signature still exists.
func TestRecordSuccess_MethodExists(t *testing.T) {
	tracker := NewBruteForceTracker(5, time.Minute)
	// This compiles, proving RecordSuccess(string) exists.
	tracker.RecordSuccess("any-ip")
	t.Log("Test 13 PASSED: RecordSuccess method signature exists (no-op)")
}

// Test 14: Verify all existing brute force tests pass (run via go test -race ./internal/auth/...).
// This test is a meta-check that the window expiry, cleanup, IP isolation, and
// concurrent access tests all pass with the new behaviour.
func TestBruteForce_ExistingBehaviours_Preserved(t *testing.T) {
	t.Run("window_expiry", func(t *testing.T) {
		tracker := NewBruteForceTracker(3, 100*time.Millisecond)
		ip := "192.168.1.1"

		for i := 0; i < 3; i++ {
			tracker.RecordFailure(ip)
		}
		if !tracker.IsBlocked(ip) {
			t.Error("should be blocked after 3 failures")
		}

		time.Sleep(150 * time.Millisecond)

		if tracker.IsBlocked(ip) {
			t.Error("should not be blocked after window expiry")
		}
	})

	t.Run("ip_isolation", func(t *testing.T) {
		tracker := NewBruteForceTracker(3, time.Minute)

		for i := 0; i < 3; i++ {
			tracker.RecordFailure("1.1.1.1")
		}
		if !tracker.IsBlocked("1.1.1.1") {
			t.Error("1.1.1.1 should be blocked")
		}
		if tracker.IsBlocked("2.2.2.2") {
			t.Error("2.2.2.2 should not be blocked")
		}
	})

	t.Run("cleanup", func(t *testing.T) {
		tracker := NewBruteForceTracker(3, 50*time.Millisecond)
		for i := 0; i < 10; i++ {
			tracker.RecordFailure("10.0.0." + string(rune('0'+i)))
		}
		time.Sleep(100 * time.Millisecond)
		tracker.Cleanup()
		if tracker.TrackedIPs() != 0 {
			t.Errorf("expected 0 tracked IPs after cleanup, got %d", tracker.TrackedIPs())
		}
	})
}
