// Reproduces known limitations listed in KNOWN_GAPS.md.
// Each test is either skipped (with a reference to the gap number) until the
// gap is fixed, or documents the current (accepted) behaviour.
package sec2_verification_test

import (
	"testing"
	"time"

	"github.com/brianbuquoi/overlord/internal/auth"
	"github.com/brianbuquoi/overlord/internal/sanitize"
)

// =============================================================================
// SEC-010: Predictable envelope delimiters
// The envelope delimiters are static strings. A per-task random nonce would
// provide stronger defense-in-depth.
// =============================================================================

func TestKnownGap_SEC010_PredictableEnvelopeDelimiters(t *testing.T) {
	t.Skip("SEC-010 OPEN: Envelope delimiters are static. Fix: use per-task random nonce.")

	// When fixed, this test should verify that two Wrap() calls produce
	// different delimiter strings (nonce-based).
	out1 := sanitize.Wrap("task1", "data1")
	out2 := sanitize.Wrap("task2", "data2")

	// Currently these use the same delimiter strings.
	// After fix: delimiters should differ between calls.
	_ = out1
	_ = out2
}

// =============================================================================
// SEC3-001: RecordSuccess resets brute force window indefinitely
// An attacker with one valid key can reset their brute force window by
// authenticating successfully every N-1 attempts.
// =============================================================================

func TestKnownGap_SEC3001_RecordSuccessResetsBruteForce(t *testing.T) {
	// SEC3-001 RESOLVED: RecordSuccess is now a no-op. Failures accumulate
	// regardless of intervening successes and expire via the sliding window.

	restore := auth.SetCostForTesting(4)
	defer restore()

	tracker := auth.NewBruteForceTracker(5, 60*time.Second, auth.WithMaxIPs(1000))

	ip := "10.0.0.1"

	// Record 4 failures (just below the threshold of 5).
	for i := 0; i < 4; i++ {
		tracker.RecordFailure(ip)
	}

	// IP should NOT be blocked yet (4 < 5).
	if tracker.IsBlocked(ip) {
		t.Fatal("expected IP not blocked with 4 failures")
	}

	// Attacker uses a valid key — RecordSuccess is now a no-op.
	tracker.RecordSuccess(ip)

	// One more failure brings total to 5 → blocked.
	tracker.RecordFailure(ip)

	if !tracker.IsBlocked(ip) {
		t.Fatal("SEC3-001 NOT FIXED: RecordSuccess still resets the failure counter")
	}
	t.Log("SEC3-001 RESOLVED: RecordSuccess is a no-op, failures accumulate correctly")
}

// SEC4-003 RESOLVED: WebSocket connections now configure a read deadline that
// is refreshed on every pong, and the write pump emits pings every wsPingPeriod.
// Regression coverage is TestWSKeepaliveInvariants in
// internal/api/websocket_test.go.

// =============================================================================
// SEC4-006: No config-level size limit on system_prompt
// =============================================================================

func TestKnownGap_SEC4006_UnboundedSystemPrompt(t *testing.T) {
	t.Skip("SEC4-006 OPEN: No config-level size limit on system_prompt. " +
		"Fix: enforce max length (e.g. 512KB) during config validation.")

	// When fixed: create config with a system_prompt > 512KB and verify
	// validation rejects it.
}

// =============================================================================
// SEC4-008: Replay dead-letter TOCTOU race
// =============================================================================

func TestKnownGap_SEC4008_ReplayTOCTOU(t *testing.T) {
	t.Skip("SEC4-008 OPEN: GetTask → state check → Submit is not atomic. " +
		"Concurrent discard between check and submit can replay a discarded task.")

	// When fixed: race two goroutines (replay and discard) on the same task
	// and verify exactly one succeeds.
}

// SEC4-010 RESOLVED: BruteForceTracker.normalizeIP masks IPv6 addresses to /64
// before tracking so an attacker rotating through a /64 prefix aggregates into
// one counter. The regression test is TestBruteForce_IPv6SharedPrefix in
// internal/auth/auth_test.go.

// SEC-013 RESOLVED: wsHub.register enforces a maxWSClients cap. The regression
// test is TestWSHubRegisterRefusesAtMaxClients in
// internal/api/websocket_test.go.

// =============================================================================
// SEC-014: Token bucket cleanup goroutine leak
// =============================================================================

func TestKnownGap_SEC014_TokenBucketCleanupLeak(t *testing.T) {
	t.Skip("SEC-014 OPEN: Rate limiter cleanup goroutine runs forever with no stop mechanism. " +
		"Fix: accept context parameter and stop on cancellation.")
}

// SEC2-003 RESOLVED: CancelTask is now an atomic CAS across memory /
// Redis / Postgres that refuses to act on a task already in a terminal
// state. Regression coverage lives in the store conformance suite
// (TestMemoryStoreConformance/CancelTask_*) plus concurrent-winner
// coverage in internal/store/memory/memory_test.go.

// =============================================================================
// SEC2-005: Migration lacks concurrency protection against live broker
// =============================================================================

func TestKnownGap_SEC2005_MigrationLiveBroker(t *testing.T) {
	t.Skip("SEC2-005 OPEN: migrate run against a live pipeline can cause tasks " +
		"to be processed with wrong schema version.")
}

// SEC4-007 RESOLVED: Plugin load paths are now manifest-validated (the plugin
// name may not contain path separators) and the subprocess provider runs with
// explicit environment allow-listing. See docs/plugin-security.md.

// SEC-012 RESOLVED: Redis UpdateTask is now served by an atomic Lua script
// that merges updates server-side in one round-trip. Regression coverage is in
// the store conformance suite (internal/store/store_conformance_test.go) and
// the per-backend tests.

// =============================================================================
// SEC4-009: UpdateTask allows arbitrary state transitions (Accepted)
// =============================================================================

func TestKnownGap_SEC4009_ArbitraryStateTransitions(t *testing.T) {
	// This is an "Accepted" informational gap. The store does not validate
	// state transitions — only the broker enforces valid transitions.
	// This test documents the behaviour.
	t.Log("SEC4-009 ACCEPTED: Store implementations accept any state value. " +
		"The broker (not store) enforces valid transitions via hardcoded logic.")
}
