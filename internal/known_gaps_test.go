// Reproduces known limitations listed in KNOWN_GAPS.md.
// Each test is either skipped (with a reference to the gap number) until the
// gap is fixed, or documents the current (accepted) behaviour.
package sec2_verification_test

import (
	"testing"
	"time"

	"github.com/orcastrator/orcastrator/internal/auth"
	"github.com/orcastrator/orcastrator/internal/sanitize"
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

// =============================================================================
// SEC4-003: WebSocket connections lack ping/pong keepalive
// =============================================================================

func TestKnownGap_SEC4003_WebSocketNoPingPong(t *testing.T) {
	t.Skip("SEC4-003 OPEN: WebSocket connections lack ping/pong keepalive. " +
		"Zombie connections persist indefinitely.")

	// When fixed: connect a WebSocket, wait beyond the pong deadline without
	// responding to pings, verify the server closes the connection.
}

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

// =============================================================================
// SEC4-010: IPv6 brute force tracking per /128 (not /64)
// =============================================================================

func TestKnownGap_SEC4010_IPv6PerAddress(t *testing.T) {
	// This test documents the current behaviour: each /128 is tracked separately.
	restore := auth.SetCostForTesting(4)
	defer restore()

	tracker := auth.NewBruteForceTracker(3, 60*time.Second, auth.WithMaxIPs(1000))

	// Simulate an attacker with a /64 block using different /128 addresses.
	for i := 0; i < 5; i++ {
		// Each unique IP gets its own failure counter.
		ip := "2001:db8::1:" + string(rune('a'+i))
		tracker.RecordFailure(ip)
		tracker.RecordFailure(ip)
		tracker.RecordFailure(ip)
	}

	// Each IP is individually blocked, but no aggregate tracking.
	// An attacker with 2^64 addresses can exhaust the tracker's 100k IP cap.
	t.Log("SEC4-010 CONFIRMED: IPv6 tracked per /128. An attacker with a /64 block " +
		"can use unique IPs to bypass brute force protection.")
}

// =============================================================================
// SEC-013: Unbounded WebSocket client count
// =============================================================================

func TestKnownGap_SEC013_UnboundedWebSocketClients(t *testing.T) {
	t.Skip("SEC-013 OPEN: No limit on total connected WebSocket clients. " +
		"Fix: add maximum client count to the hub.")
}

// =============================================================================
// SEC-014: Token bucket cleanup goroutine leak
// =============================================================================

func TestKnownGap_SEC014_TokenBucketCleanupLeak(t *testing.T) {
	t.Skip("SEC-014 OPEN: Rate limiter cleanup goroutine runs forever with no stop mechanism. " +
		"Fix: accept context parameter and stop on cancellation.")
}

// =============================================================================
// SEC2-003: cancel command TOCTOU race
// =============================================================================

func TestKnownGap_SEC2003_CancelTOCTOU(t *testing.T) {
	t.Skip("SEC2-003 OPEN: cancelTask performs read-then-write. " +
		"Between GetTask and UpdateTask, the broker can complete the task.")
}

// =============================================================================
// SEC2-005: Migration lacks concurrency protection against live broker
// =============================================================================

func TestKnownGap_SEC2005_MigrationLiveBroker(t *testing.T) {
	t.Skip("SEC2-005 OPEN: migrate run against a live pipeline can cause tasks " +
		"to be processed with wrong schema version.")
}

// =============================================================================
// SEC4-007: Plugin file paths not validated against directory traversal
// =============================================================================

func TestKnownGap_SEC4007_PluginPathTraversal(t *testing.T) {
	t.Skip("SEC4-007 OPEN: Plugin files: entries not validated against ../ sequences. " +
		"Fix: reject paths containing '..' or resolve to absolute and verify within allowed dir.")
}

// =============================================================================
// SEC-012: Redis UpdateTask is not atomic
// =============================================================================

func TestKnownGap_SEC012_RedisNonAtomicUpdate(t *testing.T) {
	t.Skip("SEC-012 OPEN: Redis UpdateTask uses GET→modify→SET without locking. " +
		"Concurrent updates may cause lost writes. Fix: use Lua script or WATCH/MULTI.")
}

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
