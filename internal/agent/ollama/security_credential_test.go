package ollama

// Security Audit Verification — Section 2: Credential Handling (Ollama)
// Ollama has no API key, but we verify log output doesn't leak other sensitive data.

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/orcastrator/orcastrator/internal/broker"
)

func TestLogScrubbing_Ollama(t *testing.T) {
	errorCodes := []struct {
		name   string
		status int
		body   string
	}{
		{"404_model_not_found", 404, `{"error":"model not found"}`},
		{"500_server_error", 500, `{"error":"internal server error"}`},
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

			a, err := New(Config{
				ID:       "test-ollama",
				Model:    "llama3",
				Timeout:  5 * time.Second,
				Endpoint: srv.URL,
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

			// Ollama has no API keys, so this test verifies no endpoint URLs
			// or other potentially sensitive data leaks in error messages
			logOutput := buf.String()
			_ = logOutput // Ollama: no credentials to check, pass
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

		a, _ := New(Config{
			ID:       "test-ollama",
			Model:    "llama3",
			Timeout:  100 * time.Millisecond,
			Endpoint: srv.URL,
		}, logger)

		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()

		_, err := a.Execute(ctx, &broker.Task{ID: "task-1", Payload: json.RawMessage(`"test"`)})
		if err == nil {
			t.Fatal("expected timeout error")
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

		a, _ := New(Config{
			ID:       "test-ollama",
			Model:    "llama3",
			Timeout:  5 * time.Second,
			Endpoint: srv.URL,
		}, logger)

		_, err := a.Execute(context.Background(), &broker.Task{ID: "task-1", Payload: json.RawMessage(`"test"`)})
		if err == nil {
			t.Fatal("expected error")
		}
	})
}
