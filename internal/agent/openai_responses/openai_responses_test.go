package openai_responses

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/brianbuquoi/overlord/internal/agent"
	"github.com/brianbuquoi/overlord/internal/broker"
)

func newTestAdapter(t *testing.T, serverURL string) *Adapter {
	t.Helper()
	t.Setenv("OPENAI_API_KEY", "test-key")
	a, err := New(Config{
		ID:           "test-codex",
		Model:        "codex-mini-latest",
		SystemPrompt: "You are a test assistant.",
		MaxTokens:    256,
		Timeout:      5 * time.Second,
		BaseURL:      serverURL,
	}, slog.Default())
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}
	return a
}

func testTask() *broker.Task {
	return &broker.Task{
		ID:      "task-1",
		Payload: json.RawMessage(`"Hello, world"`),
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// Asserts that a successful Responses API call is parsed correctly and the
// request uses the right endpoint, headers, and top-level instructions/input
// fields.
func TestExecute_Success(t *testing.T) {
	var gotBody responsesRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			t.Errorf("path: got %s, want /v1/responses", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("missing or wrong Authorization header: %q", r.Header.Get("Authorization"))
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		writeJSON(w, http.StatusOK, responsesResponse{
			ID: "resp_1",
			Output: []outputItem{{
				Type: "message",
				Content: []contentBlock{{
					Type: "output_text",
					Text: `{"result":"ok"}`,
				}},
			}},
			Usage: responsesUsage{InputTokens: 42, OutputTokens: 7},
		})
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL)
	res, err := a.Execute(context.Background(), testTask())
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if gotBody.Model != "codex-mini-latest" {
		t.Errorf("model: got %q", gotBody.Model)
	}
	if gotBody.Instructions != "You are a test assistant." {
		t.Errorf("instructions: got %q", gotBody.Instructions)
	}
	if gotBody.Input == "" {
		t.Error("input must be populated")
	}
	if string(res.Payload) != `{"result":"ok"}` {
		t.Errorf("payload: got %s", string(res.Payload))
	}
	if res.Metadata["input_tokens"].(int) != 42 || res.Metadata["output_tokens"].(int) != 7 {
		t.Errorf("metadata tokens wrong: %v", res.Metadata)
	}
}

func TestExecute_RateLimitIsRetryable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusTooManyRequests, apiError{Error: apiErrorDetail{
			Message: "slow down",
			Code:    "rate_limit_exceeded",
		}})
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL)
	_, err := a.Execute(context.Background(), testTask())
	var agentErr *agent.AgentError
	if !errors.As(err, &agentErr) {
		t.Fatalf("expected agent.AgentError, got %T: %v", err, err)
	}
	if !agentErr.Retryable {
		t.Error("rate limit should be retryable")
	}
}

func TestExecute_ContextLengthIsNonRetryable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusBadRequest, apiError{Error: apiErrorDetail{
			Message: "too long",
			Code:    "context_length_exceeded",
		}})
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL)
	_, err := a.Execute(context.Background(), testTask())
	var agentErr *agent.AgentError
	if !errors.As(err, &agentErr) || agentErr.Retryable {
		t.Fatalf("expected non-retryable AgentError, got %v", err)
	}
}

func TestExecute_ModelNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotFound, apiError{Error: apiErrorDetail{
			Message: "model not found",
			Code:    "model_not_found",
		}})
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL)
	_, err := a.Execute(context.Background(), testTask())
	var agentErr *agent.AgentError
	if !errors.As(err, &agentErr) || agentErr.Retryable {
		t.Fatalf("expected non-retryable AgentError, got %v", err)
	}
	if !strings.Contains(agentErr.Err.Error(), "codex-mini-latest") {
		t.Errorf("error should name the model: %v", agentErr.Err)
	}
	if !strings.Contains(agentErr.Err.Error(), "access") {
		t.Errorf("error should hint at API-key access: %v", agentErr.Err)
	}
}

func TestExecute_EmptyOutputArrayNonRetryable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, responsesResponse{ID: "r"})
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL)
	_, err := a.Execute(context.Background(), testTask())
	var agentErr *agent.AgentError
	if !errors.As(err, &agentErr) || agentErr.Retryable {
		t.Fatalf("expected non-retryable AgentError, got %v", err)
	}
}

func TestExecute_MalformedJSONNonRetryable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"output":[{"type":"mess`))
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL)
	_, err := a.Execute(context.Background(), testTask())
	var agentErr *agent.AgentError
	if !errors.As(err, &agentErr) || agentErr.Retryable {
		t.Fatalf("expected non-retryable AgentError on malformed JSON, got %v", err)
	}
}

func TestExecute_5xxIsRetryable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusBadGateway, apiError{Error: apiErrorDetail{Message: "upstream dead"}})
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL)
	_, err := a.Execute(context.Background(), testTask())
	var agentErr *agent.AgentError
	if !errors.As(err, &agentErr) || !agentErr.Retryable {
		t.Fatalf("expected retryable AgentError on 5xx, got %v", err)
	}
}

func TestExecute_RequestTimeoutIsRetryable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
		writeJSON(w, http.StatusOK, responsesResponse{})
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL)
	a.client.Timeout = 50 * time.Millisecond

	_, err := a.Execute(context.Background(), testTask())
	var agentErr *agent.AgentError
	if !errors.As(err, &agentErr) || !agentErr.Retryable {
		t.Fatalf("expected retryable AgentError on timeout, got %v", err)
	}
}

// A response whose output text is not valid JSON is a model-contract
// violation surfaced as a retryable AgentError — same behaviour as the
// sibling openai, anthropic, google, and ollama adapters. No silent
// wrapping into {"text": "..."}; stage schema validation does the
// enforcement.
func TestExecute_NonJSONTextIsRetryable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, responsesResponse{
			ID: "r",
			Output: []outputItem{{
				Type: "message",
				Content: []contentBlock{{Type: "output_text", Text: "hello world"}},
			}},
		})
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL)
	_, err := a.Execute(context.Background(), testTask())
	var agentErr *agent.AgentError
	if !errors.As(err, &agentErr) || !agentErr.Retryable {
		t.Fatalf("expected retryable AgentError on non-JSON text, got %v", err)
	}
}

func TestHealthCheck_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/v1/models/") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": "codex-mini-latest"})
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL)
	if err := a.HealthCheck(context.Background()); err != nil {
		t.Fatalf("health check: %v", err)
	}
}

func TestHealthCheck_ModelNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotFound, apiError{Error: apiErrorDetail{Message: "no such model"}})
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL)
	err := a.HealthCheck(context.Background())
	if err == nil {
		t.Fatal("expected health check error")
	}
	if !strings.Contains(err.Error(), "codex-mini-latest") {
		t.Errorf("error should name the model: %v", err)
	}
}
