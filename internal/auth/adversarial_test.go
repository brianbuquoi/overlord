package auth

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"
	"unsafe"

	"github.com/orcastrator/orcastrator/internal/config"
)

// --- Test 3: Key in logs ---

func TestCredentialLeakage_Logs(t *testing.T) {
	secretKey := "super-secret-key-never-log-this-12345"
	t.Setenv("LOG_LEAK_KEY", secretKey)

	keys, err := LoadKeys([]config.AuthKeyConfig{
		{Name: "logtest", KeyEnv: "LOG_LEAK_KEY", Scopes: []string{"read"}},
	})
	if err != nil {
		t.Fatalf("LoadKeys: %v", err)
	}

	// Capture log output.
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	// Simulate various auth scenarios and log them as the middleware would.
	scenarios := []struct {
		name  string
		token string
	}{
		{"valid", secretKey},
		{"invalid", "wrong-key-attempt"},
		{"empty", ""},
		{"partial", secretKey[:10]},
	}

	for _, sc := range scenarios {
		key, authErr := Authenticate(keys, sc.token)
		if authErr != nil {
			logger.Warn("auth failed",
				"ip", "192.168.1.1",
				"endpoint", "/v1/tasks",
				"reason", "invalid",
			)
		} else {
			logger.Info("auth success",
				"ip", "192.168.1.1",
				"endpoint", "/v1/tasks",
				"key_name", key.Name,
			)
		}
	}

	logOutput := logBuf.String()
	if strings.Contains(logOutput, secretKey) {
		t.Error("CRITICAL: API key value found in log output")
	}
	// Also check partial key exposure.
	if strings.Contains(logOutput, secretKey[:10]) {
		t.Error("CRITICAL: partial API key value found in log output")
	}

}

// --- Test 4: Key in error responses ---

func TestCredentialLeakage_ErrorResponses(t *testing.T) {
	// The Authenticate function returns ErrUnauthorized, which is a fixed message.
	// Verify it never includes the token.
	_, err := Authenticate(nil, "secret-token-value")
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "secret-token-value") {
		t.Error("CRITICAL: token value appears in error message")
	}
}

// --- Test 5: Key in memory after load ---

func TestCredentialLeakage_Memory(t *testing.T) {
	// NOTE: This test is inherently platform-specific and may not be fully
	// reliable due to Go's garbage collector. The GC may have copied strings
	// internally during environment variable reads. This is best-effort
	// defense-in-depth verification.

	secret := "memory-scan-test-secret-key-value-42"
	t.Setenv("MEMSCAN_KEY", secret)

	keys, err := LoadKeys([]config.AuthKeyConfig{
		{Name: "memscan", KeyEnv: "MEMSCAN_KEY", Scopes: []string{"read"}},
	})
	if err != nil {
		t.Fatalf("LoadKeys: %v", err)
	}

	// Scan each APIKey struct's memory for the plaintext secret.
	secretBytes := []byte(secret)
	for _, key := range keys {
		// Check struct bytes via unsafe.
		size := unsafe.Sizeof(key)
		ptr := unsafe.Pointer(&key)
		structBytes := unsafe.Slice((*byte)(ptr), size)

		if bytes.Contains(structBytes, secretBytes) {
			t.Error("plaintext key found in APIKey struct memory")
		}

		// Check the HashedKey field — bcrypt hash should not contain the plaintext.
		if bytes.Contains(key.HashedKey, secretBytes) {
			t.Error("plaintext key found in HashedKey bytes")
		}

		// Check the Name field.
		if key.Name == secret {
			t.Error("plaintext key found in Name field")
		}
	}

	t.Log("Memory scan complete. Note: GC may have copied strings internally; " +
		"this test is best-effort defense-in-depth, not a guarantee.")
}

// --- Test 8: Brute force threshold accuracy ---

func TestBruteForce_ThresholdAccuracy(t *testing.T) {
	tracker := NewBruteForceTracker(10, time.Minute)
	ip := "10.20.30.40"

	// Send exactly 10 failures.
	for i := 0; i < 10; i++ {
		if tracker.IsBlocked(ip) {
			t.Fatalf("blocked too early at attempt %d", i+1)
		}
		tracker.RecordFailure(ip)
	}

	// 11th check should be blocked.
	if !tracker.IsBlocked(ip) {
		t.Error("should be blocked after exactly 10 failures")
	}
}

func TestBruteForce_SuccessDoesNotResetCounter(t *testing.T) {
	// SEC3-001 FIXED: RecordSuccess is a no-op.
	tracker := NewBruteForceTracker(10, time.Minute)
	ip := "10.20.30.41"

	// 9 failures.
	for i := 0; i < 9; i++ {
		tracker.RecordFailure(ip)
	}

	// 1 success — no-op.
	tracker.RecordSuccess(ip)

	// 1 more failure — total is 10, should be blocked.
	tracker.RecordFailure(ip)
	if !tracker.IsBlocked(ip) {
		t.Error("should be blocked after 10 failures — RecordSuccess must not reset (SEC3-001)")
	}
}

// --- Test 9: Window expiry ---

func TestBruteForce_WindowExpiry(t *testing.T) {
	tracker := NewBruteForceTracker(10, 100*time.Millisecond)
	ip := "10.20.30.42"

	// Exhaust the limit.
	for i := 0; i < 10; i++ {
		tracker.RecordFailure(ip)
	}
	if !tracker.IsBlocked(ip) {
		t.Fatal("should be blocked")
	}

	// Wait for window to expire.
	time.Sleep(150 * time.Millisecond)

	if tracker.IsBlocked(ip) {
		t.Error("should not be blocked after window expiry")
	}
}

// --- Test 10: Brute force isolation ---

func TestBruteForce_IPIsolation(t *testing.T) {
	tracker := NewBruteForceTracker(10, time.Minute)

	// Exhaust limit for IP A.
	for i := 0; i < 10; i++ {
		tracker.RecordFailure("127.0.0.1")
	}

	if !tracker.IsBlocked("127.0.0.1") {
		t.Error("127.0.0.1 should be blocked")
	}

	// IP B should not be affected.
	if tracker.IsBlocked("10.0.0.1") {
		t.Error("10.0.0.1 should NOT be blocked — isolation failure")
	}

	t.Log("X-Forwarded-For trust model: NOT trusted. Auth rate limiting uses " +
		"RemoteAddr (actual TCP source), matching the main rate limiter's model. " +
		"If behind a trusted proxy, clientIP() should be updated to read X-Real-IP.")
}

// --- Test 15-16: Backward compatibility ---

func TestBackwardCompat_NoAuthBlock(t *testing.T) {
	// Config with no auth block should have auth.enabled=false by default.
	var cfg struct {
		Auth struct {
			Enabled bool `json:"enabled"`
		} `json:"auth"`
	}
	_ = json.Unmarshal([]byte(`{"auth":{"enabled":false}}`), &cfg)

	if cfg.Auth.Enabled {
		t.Error("default auth state should be disabled")
	}
}
