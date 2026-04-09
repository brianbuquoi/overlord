package anthropic

// Security Audit Verification — Section 5: TLS Enforcement
// Test 18: Verify non-localhost http:// URLs are rejected for cloud providers.

import (
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestSecurity_TLSEnforcement_Anthropic(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "test-key")

	cases := []struct {
		name       string
		baseURL    string
		wantReject bool
	}{
		{"https_allowed", "https://api.anthropic.com", false},
		{"http_remote_rejected", "http://remote.example.com", true},
		{"http_ip_rejected", "http://192.168.1.100", true},
		{"http_localhost_allowed", "http://localhost:8080", false}, // test server exception
		{"http_127_allowed", "http://127.0.0.1:8080", false},       // test server exception
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(Config{
				ID:      "test",
				Model:   "claude-sonnet-4-5",
				BaseURL: tc.baseURL,
				Timeout: 1 * time.Second,
			}, slog.Default())

			if tc.wantReject {
				if err == nil {
					t.Errorf("SEC-009 BUG: http:// URL %q was accepted", tc.baseURL)
				} else if !strings.Contains(err.Error(), "HTTPS") {
					t.Errorf("SEC-009: error does not mention HTTPS requirement: %v", err)
				} else {
					t.Logf("SEC-009 FIXED: rejected with: %v", err)
				}
			} else {
				if err != nil {
					t.Errorf("unexpected rejection for %q: %v", tc.baseURL, err)
				}
			}
		})
	}
}
