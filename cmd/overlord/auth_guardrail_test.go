package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/brianbuquoi/overlord/internal/config"
)

// captureLogger returns a slog.Logger writing JSON records to a shared
// bytes.Buffer. Tests inspect the buffer contents after the code under
// test emits (or does not emit) a warning.
func captureLogger(t *testing.T) (*slog.Logger, *bytes.Buffer) {
	t.Helper()
	buf := &bytes.Buffer{}
	h := slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	return slog.New(h), buf
}

// warningRecords scans the captured JSON-log buffer for records emitted
// by checkAuthGuardrail (matched by the canonical message prefix) and
// returns them decoded. Unrelated log lines are ignored so callers can
// assert a clean count.
func warningRecords(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(buf.String(), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("unmarshal log line %q: %v", line, err)
		}
		msg, _ := rec["msg"].(string)
		if strings.HasPrefix(msg, "auth is disabled on a non-loopback bind address") {
			out = append(out, rec)
		}
	}
	return out
}

func TestCheckAuthGuardrail_NoWarning(t *testing.T) {
	cases := []struct {
		name     string
		authOn   bool
		bindAddr string
	}{
		{"auth enabled + 0.0.0.0", true, "0.0.0.0:8080"},
		{"auth enabled + 127.0.0.1", true, "127.0.0.1:8080"},
		{"auth enabled + LAN IP", true, "10.0.0.5:8080"},
		{"auth disabled + 127.0.0.1", false, "127.0.0.1:8080"},
		{"auth disabled + 127.0.0.1 bare", false, "127.0.0.1"},
		{"auth disabled + localhost", false, "localhost:8080"},
		{"auth disabled + localhost bare", false, "localhost"},
		{"auth disabled + ::1 bracketed", false, "[::1]:8080"},
		{"auth disabled + ::1 bare", false, "::1"},
		{"auth disabled + 127.0.0.2 (loopback /8)", false, "127.0.0.2:8080"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logger, buf := captureLogger(t)
			cfg := &config.Config{Auth: config.APIAuthConfig{Enabled: tc.authOn}}

			checkAuthGuardrail(logger, cfg, tc.bindAddr)

			if got := warningRecords(t, buf); len(got) != 0 {
				t.Fatalf("expected no warning records; got %d: %v", len(got), got)
			}
		})
	}
}

// TestCheckAuthGuardrail_Warning asserts the warn-emit behavior. Note
// runCmd now only invokes this function when --allow-public-noauth is
// set; the warn path is the opt-in case, not the default. See
// TestRefusePublicNoauth for the default-refuse matrix.
func TestCheckAuthGuardrail_Warning(t *testing.T) {
	cases := []struct {
		name     string
		bindAddr string
	}{
		{"0.0.0.0 all-interfaces", "0.0.0.0:8080"},
		{"LAN IP", "10.0.0.5:8080"},
		{"empty host implicit all-interfaces", ":8080"},
		{"IPv6 unspecified :: explicit", "[::]:8080"},
		{"public IPv4", "203.0.113.5:8080"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logger, buf := captureLogger(t)
			cfg := &config.Config{Auth: config.APIAuthConfig{Enabled: false}}

			checkAuthGuardrail(logger, cfg, tc.bindAddr)

			records := warningRecords(t, buf)
			if len(records) != 1 {
				t.Fatalf("expected exactly one warning; got %d: %v", len(records), records)
			}
			rec := records[0]

			if level, _ := rec["level"].(string); level != "WARN" {
				t.Errorf("expected level=WARN, got %q", level)
			}
			if got, _ := rec["auth_disabled"].(bool); !got {
				t.Errorf("expected auth_disabled=true, got %v", rec["auth_disabled"])
			}
			if got, _ := rec["bind_address"].(string); got != tc.bindAddr {
				t.Errorf("expected bind_address=%q, got %q", tc.bindAddr, got)
			}
			if got, _ := rec["doc"].(string); got != authGuardrailDocURL {
				t.Errorf("expected doc=%q, got %q", authGuardrailDocURL, got)
			}
			msg, _ := rec["msg"].(string)
			if !strings.Contains(msg, "enable auth before serving this instance") {
				t.Errorf("warning message missing canonical guidance: %q", msg)
			}
		})
	}
}

// TestCheckAuthGuardrail_AuthBlockAbsent asserts that a config with no
// auth: block (zero-value APIAuthConfig, Enabled=false) triggers the
// warning on a non-loopback bind — matching what the scaffolder emits.
func TestCheckAuthGuardrail_AuthBlockAbsent(t *testing.T) {
	logger, buf := captureLogger(t)
	// Zero-value config — no auth block set. Enabled defaults to false.
	cfg := &config.Config{}

	checkAuthGuardrail(logger, cfg, "0.0.0.0:8080")

	records := warningRecords(t, buf)
	if len(records) != 1 {
		t.Fatalf("expected warning when auth block absent; got %d records", len(records))
	}
	if got, _ := records[0]["auth_disabled"].(bool); !got {
		t.Errorf("expected auth_disabled=true for absent auth block")
	}
}

// TestCheckAuthGuardrail_NilConfig asserts the helper does not panic when
// handed a nil *config.Config — defensive guard for call-site refactors.
func TestCheckAuthGuardrail_NilConfig(t *testing.T) {
	logger, buf := captureLogger(t)

	checkAuthGuardrail(logger, nil, "0.0.0.0:8080")

	if got := warningRecords(t, buf); len(got) != 0 {
		t.Fatalf("expected no warnings for nil config; got %d", len(got))
	}
}

// TestRefusePublicNoauth asserts shouldRefusePublicNoauth returns true
// exactly when the bind address is non-loopback AND auth is disabled
// AND --allow-public-noauth was not set.
func TestRefusePublicNoauth(t *testing.T) {
	cases := []struct {
		name        string
		bindAddr    string
		authEnabled bool
		allow       bool
		wantRefuse  bool
	}{
		{"loopback + auth off", "127.0.0.1:8080", false, false, false},
		{"loopback + auth on", "127.0.0.1:8080", true, false, false},
		{"public + auth on", "0.0.0.0:8080", true, false, false},
		{"public + auth off (danger)", "0.0.0.0:8080", false, false, true},
		{"public + auth off + allow", "0.0.0.0:8080", false, true, false},
		{"LAN + auth off (danger)", "10.0.0.5:8080", false, false, true},
		{"LAN + auth off + allow", "10.0.0.5:8080", false, true, false},
		{"implicit all-interfaces + auth off (danger)", ":8080", false, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{Auth: config.APIAuthConfig{Enabled: tc.authEnabled}}
			got := shouldRefusePublicNoauth(cfg, tc.bindAddr, tc.allow)
			if got != tc.wantRefuse {
				t.Errorf("shouldRefusePublicNoauth(bind=%q, authOn=%v, allow=%v) = %v, want %v",
					tc.bindAddr, tc.authEnabled, tc.allow, got, tc.wantRefuse)
			}
		})
	}
}

// TestRefuseInsecureTransport asserts the bearer-over-plaintext
// guardrail: when auth is enabled and the bind is non-loopback,
// Overlord refuses to start unless the operator explicitly opts in
// via --allow-insecure-transport. This is the companion to
// TestRefusePublicNoauth; the two guardrails together cover the
// matrix of (loopback, public) × (auth on, auth off).
func TestRefuseInsecureTransport(t *testing.T) {
	cases := []struct {
		name        string
		bindAddr    string
		authEnabled bool
		allow       bool
		wantRefuse  bool
	}{
		// Loopback is safe regardless — keys never leave the box.
		{"loopback + auth on", "127.0.0.1:8080", true, false, false},
		{"loopback + auth off", "127.0.0.1:8080", false, false, false},
		// Public + auth enabled is the exact footgun the audit
		// reproduced: bearer keys flow over plaintext HTTP.
		{"public + auth on (danger)", "0.0.0.0:8080", true, false, true},
		{"public + auth on + allow", "0.0.0.0:8080", true, true, false},
		{"LAN + auth on (danger)", "10.0.0.5:8080", true, false, true},
		{"LAN + auth on + allow", "10.0.0.5:8080", true, true, false},
		// Implicit all-interfaces bind must be treated as public.
		{"implicit all-interfaces + auth on (danger)", ":8080", true, false, true},
		// Public + auth off is the OTHER guardrail's concern;
		// this one must not double-fire.
		{"public + auth off", "0.0.0.0:8080", false, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{Auth: config.APIAuthConfig{Enabled: tc.authEnabled}}
			got := shouldRefuseInsecureTransport(cfg, tc.bindAddr, tc.allow)
			if got != tc.wantRefuse {
				t.Errorf("shouldRefuseInsecureTransport(bind=%q, authOn=%v, allow=%v) = %v, want %v",
					tc.bindAddr, tc.authEnabled, tc.allow, got, tc.wantRefuse)
			}
		})
	}
}

// TestIsLoopbackHost covers the loopback classifier directly.
func TestIsLoopbackHost(t *testing.T) {
	cases := []struct {
		host string
		want bool
	}{
		{"", false},
		{"0.0.0.0", false},
		{"::", false},
		{"localhost", true},
		{"127.0.0.1", true},
		{"127.0.0.2", true},
		{"127.255.255.254", true},
		{"::1", true},
		{"10.0.0.5", false},
		{"203.0.113.5", false},
		{"2001:db8::1", false},
		{"not-an-ip", false},
	}
	for _, tc := range cases {
		t.Run(tc.host, func(t *testing.T) {
			if got := isLoopbackHost(tc.host); got != tc.want {
				t.Errorf("isLoopbackHost(%q) = %v, want %v", tc.host, got, tc.want)
			}
		})
	}
}

// TestBindHost covers address-parsing edge cases.
func TestBindHost(t *testing.T) {
	cases := []struct {
		addr string
		want string
	}{
		{":8080", ""},
		{"0.0.0.0:8080", "0.0.0.0"},
		{"127.0.0.1:8080", "127.0.0.1"},
		{"[::1]:8080", "::1"},
		{"[::]:8080", "::"},
		{"localhost:8080", "localhost"},
		{"localhost", "localhost"},
		{"127.0.0.1", "127.0.0.1"},
	}
	for _, tc := range cases {
		t.Run(tc.addr, func(t *testing.T) {
			if got := bindHost(tc.addr); got != tc.want {
				t.Errorf("bindHost(%q) = %q, want %q", tc.addr, got, tc.want)
			}
		})
	}
}
