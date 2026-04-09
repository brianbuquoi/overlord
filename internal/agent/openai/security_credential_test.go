package openai

// Security Audit Verification — Section 2: Credential Handling (OpenAI)

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

func TestLogScrubbing_OpenAI(t *testing.T) {
	errorCodes := []struct {
		name   string
		status int
		body   string
	}{
		{"401_unauthorized", 401, `{"error":{"code":"invalid_api_key","message":"invalid api key"}}`},
		{"429_rate_limit", 429, `{"error":{"code":"rate_limit_exceeded","message":"rate limited"}}`},
		{"500_server_error", 500, `{"error":{"code":"server_error","message":"internal error"}}`},
	}

	for _, ec := range errorCodes {
		t.Run(ec.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(ec.status)
				w.Write([]byte(ec.body))
			}))
			defer srv.Close()

			var buf bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

			t.Setenv("TEST_OPENAI_CANARY", canaryKey)
			a, err := New(Config{
				ID:        "test-openai",
				Model:     "gpt-4o",
				APIKeyEnv: "TEST_OPENAI_CANARY",
				Timeout:   5 * time.Second,
				BaseURL:   srv.URL,
			}, logger)
			if err != nil {
				t.Fatal(err)
			}

			_, execErr := a.Execute(context.Background(), &broker.Task{
				ID:      "task-1",
				Payload: json.RawMessage(`"test"`),
			})
			if execErr == nil {
				t.Fatal("expected error")
			}

			if strings.Contains(buf.String(), canarySubstring) {
				t.Errorf("SECURITY: API key canary found in logs for %s:\n%s", ec.name, buf.String())
			}
		})
	}

	// Timeout
	t.Run("timeout", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(10 * time.Second)
		}))
		defer srv.Close()

		var buf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

		t.Setenv("TEST_OPENAI_CANARY", canaryKey)
		a, _ := New(Config{
			ID:        "test-openai",
			Model:     "gpt-4o",
			APIKeyEnv: "TEST_OPENAI_CANARY",
			Timeout:   100 * time.Millisecond,
			BaseURL:   srv.URL,
		}, logger)

		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()

		_, err := a.Execute(ctx, &broker.Task{ID: "task-1", Payload: json.RawMessage(`"test"`)})
		if err == nil {
			t.Fatal("expected error")
		}

		if strings.Contains(buf.String(), canarySubstring) {
			t.Errorf("SECURITY: API key canary found in timeout logs:\n%s", buf.String())
		}
	})

	// Malformed response
	t.Run("malformed_response", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(200)
			w.Write([]byte(`{broken`))
		}))
		defer srv.Close()

		var buf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

		t.Setenv("TEST_OPENAI_CANARY", canaryKey)
		a, _ := New(Config{
			ID:        "test-openai",
			Model:     "gpt-4o",
			APIKeyEnv: "TEST_OPENAI_CANARY",
			Timeout:   5 * time.Second,
			BaseURL:   srv.URL,
		}, logger)

		_, err := a.Execute(context.Background(), &broker.Task{ID: "task-1", Payload: json.RawMessage(`"test"`)})
		if err == nil {
			t.Fatal("expected error")
		}

		if strings.Contains(buf.String(), canarySubstring) {
			t.Errorf("SECURITY: API key canary found in malformed response logs:\n%s", buf.String())
		}
	})
}

func TestErrorMessageScrubbing_OpenAI(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		w.Write([]byte(`{"error":{"code":"invalid_api_key","message":"invalid api key"}}`))
	}))
	defer srv.Close()

	t.Setenv("TEST_OPENAI_CANARY", canaryKey)
	a, _ := New(Config{
		ID:        "test-openai",
		Model:     "gpt-4o",
		APIKeyEnv: "TEST_OPENAI_CANARY",
		Timeout:   5 * time.Second,
		BaseURL:   srv.URL,
	}, slog.Default())

	_, err := a.Execute(context.Background(), &broker.Task{ID: "task-1", Payload: json.RawMessage(`"test"`)})
	if err == nil {
		t.Fatal("expected error")
	}

	errMsg := err.Error()
	if strings.Contains(errMsg, canarySubstring) {
		t.Fatalf("SECURITY: API key canary in error message: %s", errMsg)
	}
}
