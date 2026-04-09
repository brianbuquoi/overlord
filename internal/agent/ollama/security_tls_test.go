package ollama

// Security Audit Verification — Section 5: TLS Enforcement (Ollama)
// Ollama has a localhost exception: http://localhost and http://127.0.0.1 are OK.
// http://<any-other-host> should be rejected.

import (
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestSecurity_TLSEnforcement_Ollama(t *testing.T) {
	cases := []struct {
		name       string
		endpoint   string
		wantReject bool
	}{
		{"http_localhost_allowed", "http://localhost:11434", false},
		{"http_127_allowed", "http://127.0.0.1:11434", false},
		{"http_remote_rejected", "http://remote.example.com:11434", true},
		{"http_private_ip_rejected", "http://192.168.1.100:11434", true},
		{"https_remote_allowed", "https://ollama.example.com:11434", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(Config{
				ID:       "test",
				Model:    "llama3",
				Endpoint: tc.endpoint,
				Timeout:  1 * time.Second,
			}, slog.Default())

			if tc.wantReject {
				if err == nil {
					t.Errorf("SEC-009 BUG: non-localhost HTTP endpoint %q was accepted", tc.endpoint)
				} else if !strings.Contains(err.Error(), "HTTPS") && !strings.Contains(err.Error(), "localhost") {
					t.Errorf("SEC-009: error missing helpful message: %v", err)
				} else {
					t.Logf("SEC-009 FIXED: rejected with: %v", err)
				}
			} else if err != nil {
				t.Errorf("unexpected rejection for %q: %v", tc.endpoint, err)
			}
		})
	}
}
