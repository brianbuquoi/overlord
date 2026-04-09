package anthropic

// Security Audit Verification — Section 2: Credential Handling (Anthropic)
//
// Tests 5–6: Log scrubbing and error message scrubbing for the Anthropic adapter.

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/orcastrator/orcastrator/internal/broker"
)

const canaryKey = "sk-test-CANARY-VALUE-12345"
const canarySubstring = "CANARY-VALUE"

// logCapture captures slog output for inspection.
type logCapture struct {
	buf bytes.Buffer
}

func newLogCapture() (*logCapture, *slog.Logger) {
	lc := &logCapture{}
	handler := slog.NewTextHandler(&lc.buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	return lc, slog.New(handler)
}

func (lc *logCapture) contains(s string) bool {
	return strings.Contains(lc.buf.String(), s)
}

func newCanaryAdapter(t *testing.T, serverURL string, logger *slog.Logger) *Adapter {
	t.Helper()
	t.Setenv("TEST_ANTHROPIC_CANARY", canaryKey)
	a, err := New(Config{
		ID:           "test-anthropic",
		Model:        "claude-sonnet-4-5",
		APIKeyEnv:    "TEST_ANTHROPIC_CANARY",
		SystemPrompt: "test",
		MaxTokens:    1024,
		Timeout:      5 * time.Second,
		BaseURL:      serverURL,
	}, logger)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// Test 5 (Anthropic): Log scrubbing — verify canary never appears in logs.
func TestLogScrubbing_Anthropic(t *testing.T) {
	errorCodes := []struct {
		name   string
		status int
		body   string
	}{
		{"401_unauthorized", 401, `{"error":{"type":"authentication_error","message":"invalid api key"}}`},
		{"429_rate_limit", 429, `{"error":{"type":"rate_limit_error","message":"rate limited"}}`},
		{"500_server_error", 500, `{"error":{"type":"server_error","message":"internal error"}}`},
		{"400_bad_request", 400, `{"error":{"type":"invalid_request_error","message":"bad request"}}`},
	}

	for _, ec := range errorCodes {
		t.Run(ec.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(ec.status)
				w.Write([]byte(ec.body))
			}))
			defer srv.Close()

			lc, logger := newLogCapture()
			a := newCanaryAdapter(t, srv.URL, logger)

			_, err := a.Execute(context.Background(), &broker.Task{
				ID:      "task-1",
				Payload: json.RawMessage(`"test"`),
			})
			if err == nil {
				t.Fatal("expected error")
			}

			if lc.contains(canarySubstring) {
				t.Errorf("SECURITY: API key canary %q found in log output for %s error:\n%s",
					canarySubstring, ec.name, lc.buf.String())
			}
		})
	}

	// Test timeout error path
	t.Run("timeout", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(10 * time.Second)
		}))
		defer srv.Close()

		lc, logger := newLogCapture()
		t.Setenv("TEST_ANTHROPIC_CANARY", canaryKey)
		a, _ := New(Config{
			ID:        "test-anthropic",
			Model:     "claude-sonnet-4-5",
			APIKeyEnv: "TEST_ANTHROPIC_CANARY",
			Timeout:   100 * time.Millisecond,
			BaseURL:   srv.URL,
		}, logger)

		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()

		_, err := a.Execute(ctx, &broker.Task{ID: "task-1", Payload: json.RawMessage(`"test"`)})
		if err == nil {
			t.Fatal("expected timeout error")
		}

		if lc.contains(canarySubstring) {
			t.Errorf("SECURITY: API key canary found in log output for timeout error:\n%s", lc.buf.String())
		}
	})

	// Test malformed response
	t.Run("malformed_response", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(200)
			w.Write([]byte(`{not valid json`))
		}))
		defer srv.Close()

		lc, logger := newLogCapture()
		a := newCanaryAdapter(t, srv.URL, logger)

		_, err := a.Execute(context.Background(), &broker.Task{
			ID:      "task-1",
			Payload: json.RawMessage(`"test"`),
		})
		if err == nil {
			t.Fatal("expected error")
		}

		if lc.contains(canarySubstring) {
			t.Errorf("SECURITY: API key canary found in log output for malformed response:\n%s", lc.buf.String())
		}
	})
}

// Test 6 (Anthropic): Error message scrubbing — verify AgentError messages
// contain useful info but not the API key.
func TestErrorMessageScrubbing_Anthropic(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		w.Write([]byte(`{"error":{"type":"authentication_error","message":"invalid api key"}}`))
	}))
	defer srv.Close()

	_, logger := newLogCapture()
	a := newCanaryAdapter(t, srv.URL, logger)

	_, err := a.Execute(context.Background(), &broker.Task{
		ID:      "task-1",
		Payload: json.RawMessage(`"test"`),
	})
	if err == nil {
		t.Fatal("expected error")
	}

	errMsg := err.Error()

	// Must contain useful information
	if !strings.Contains(errMsg, "authentication") && !strings.Contains(errMsg, "unauthorized") && !strings.Contains(errMsg, "401") {
		t.Errorf("error message lacks useful auth failure info: %s", errMsg)
	}

	// Must NOT contain the API key value
	if strings.Contains(errMsg, canarySubstring) {
		t.Fatalf("SECURITY: API key canary %q found in error message: %s", canarySubstring, errMsg)
	}
	if strings.Contains(errMsg, canaryKey) {
		t.Fatalf("SECURITY: full API key found in error message: %s", errMsg)
	}
}
