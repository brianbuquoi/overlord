package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/brianbuquoi/orcastrator/internal/agent"
	"github.com/brianbuquoi/orcastrator/internal/broker"
)

func newTestAdapter(t *testing.T, serverURL string) *Adapter {
	t.Helper()
	t.Setenv("OPENAI_API_KEY", "test-key")
	a, err := New(Config{
		ID:           "test-openai",
		Model:        "gpt-4o",
		SystemPrompt: "You are a test assistant.",
		MaxTokens:    1024,
		Timeout:      5 * time.Second,
		BaseURL:      serverURL,
	}, slog.Default())
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}
	return a
}

func newTestAdapterWithLogger(t *testing.T, serverURL string, logger *slog.Logger) *Adapter {
	t.Helper()
	t.Setenv("OPENAI_API_KEY", "test-key")
	a, err := New(Config{
		ID:           "test-openai",
		Model:        "gpt-4o",
		SystemPrompt: "You are a test assistant.",
		MaxTokens:    1024,
		Timeout:      5 * time.Second,
		BaseURL:      serverURL,
	}, logger)
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

func mustAgentError(t *testing.T, err error) *agent.AgentError {
	t.Helper()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	ae, ok := err.(*agent.AgentError)
	if !ok {
		t.Fatalf("expected *agent.AgentError, got %T: %v", err, err)
	}
	return ae
}

func successResponse() chatResponse {
	return chatResponse{
		ID:      "chatcmpl-123",
		Choices: []choice{{Message: chatMessage{Role: "assistant", Content: `{"reply": "Hello back!"}`}}},
		Usage:   usage{PromptTokens: 12, CompletionTokens: 4},
	}
}

// --- Existing tests ---

func TestExecute_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("missing or wrong Authorization header")
		}

		var reqBody chatRequest
		if err := json.NewDecoder(r.Body).Decode(&reqBody); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if reqBody.Model != "gpt-4o" {
			t.Errorf("unexpected model: %s", reqBody.Model)
		}
		if len(reqBody.Messages) != 2 {
			t.Errorf("expected 2 messages, got %d", len(reqBody.Messages))
		}

		json.NewEncoder(w).Encode(successResponse())
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL)
	result, err := a.Execute(context.Background(), testTask())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.TaskID != "task-1" {
		t.Errorf("unexpected task ID: %s", result.TaskID)
	}

	var output map[string]any
	if err := json.Unmarshal(result.Payload, &output); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if output["reply"] != "Hello back!" {
		t.Errorf("unexpected output: %v", output)
	}
	if result.Metadata["input_tokens"] != 12 {
		t.Errorf("unexpected input_tokens: %v", result.Metadata["input_tokens"])
	}
	if result.Metadata["output_tokens"] != 4 {
		t.Errorf("unexpected output_tokens: %v", result.Metadata["output_tokens"])
	}
}

// --- 1. Error classification tests ---

func TestExecute_RateLimitExceeded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "15")
		w.WriteHeader(http.StatusTooManyRequests)
		json.NewEncoder(w).Encode(apiError{
			Error: apiErrorDetail{
				Code:    "rate_limit_exceeded",
				Message: "Rate limit exceeded",
				Type:    "tokens",
			},
		})
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL)
	_, err := a.Execute(context.Background(), testTask())
	ae := mustAgentError(t, err)

	if !ae.Retryable {
		t.Error("rate_limit_exceeded must be retryable")
	}
	if ae.RetryAfter != 15*time.Second {
		t.Errorf("RetryAfter: want 15s, got %v", ae.RetryAfter)
	}
}

func TestExecute_ContextLengthExceeded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(apiError{
			Error: apiErrorDetail{
				Code:    "context_length_exceeded",
				Message: "This model's maximum context length is 128000 tokens",
				Type:    "invalid_request_error",
			},
		})
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL)
	_, err := a.Execute(context.Background(), testTask())
	ae := mustAgentError(t, err)

	if ae.Retryable {
		t.Error("context_length_exceeded must be non-retryable")
	}
}

func TestExecute_InvalidAPIKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(apiError{
			Error: apiErrorDetail{
				Code:    "invalid_api_key",
				Message: "Incorrect API key provided",
				Type:    "invalid_request_error",
			},
		})
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL)
	_, err := a.Execute(context.Background(), testTask())
	ae := mustAgentError(t, err)

	if ae.Retryable {
		t.Error("invalid_api_key must be non-retryable")
	}
}

func TestExecute_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(apiError{
			Error: apiErrorDetail{
				Code:    "server_error",
				Message: "The server had an error",
				Type:    "server_error",
			},
		})
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL)
	_, err := a.Execute(context.Background(), testTask())
	ae := mustAgentError(t, err)

	if !ae.Retryable {
		t.Error("server_error must be retryable")
	}
}

func TestExecute_503_NoBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		// No body at all.
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL)
	_, err := a.Execute(context.Background(), testTask())
	ae := mustAgentError(t, err)

	if !ae.Retryable {
		t.Error("503 with no body must be retryable")
	}
}

// --- 2. Retry-After header propagation (covered above but verify struct fields) ---

func TestExecute_RetryAfter_FieldInspection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "45")
		w.WriteHeader(http.StatusTooManyRequests)
		json.NewEncoder(w).Encode(apiError{
			Error: apiErrorDetail{Code: "rate_limit_exceeded", Message: "slow down"},
		})
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL)
	_, err := a.Execute(context.Background(), testTask())
	ae := mustAgentError(t, err)

	if ae.AgentID != "test-openai" {
		t.Errorf("AgentID: want test-openai, got %s", ae.AgentID)
	}
	if ae.Prov != "openai" {
		t.Errorf("Prov: want openai, got %s", ae.Prov)
	}
	if !ae.Retryable {
		t.Error("Retryable: want true")
	}
	if ae.RetryAfter != 45*time.Second {
		t.Errorf("RetryAfter: want 45s, got %v", ae.RetryAfter)
	}
	if ae.Err == nil {
		t.Error("Err: want non-nil underlying error")
	}
}

// --- 3. Token logging with captured slog ---

type capturedRecord struct {
	Message string
	Attrs   map[string]any
}

type capturingHandler struct {
	mu      sync.Mutex
	records []capturedRecord
}

func (h *capturingHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }
func (h *capturingHandler) WithAttrs(_ []slog.Attr) slog.Handler         { return h }
func (h *capturingHandler) WithGroup(_ string) slog.Handler              { return h }
func (h *capturingHandler) Handle(_ context.Context, r slog.Record) error {
	rec := capturedRecord{Message: r.Message, Attrs: make(map[string]any)}
	r.Attrs(func(a slog.Attr) bool {
		rec.Attrs[a.Key] = a.Value.Any()
		return true
	})
	h.mu.Lock()
	h.records = append(h.records, rec)
	h.mu.Unlock()
	return nil
}

func TestExecute_TokenLogging(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(successResponse())
	}))
	defer srv.Close()

	handler := &capturingHandler{}
	logger := slog.New(handler)
	a := newTestAdapterWithLogger(t, srv.URL, logger)

	_, err := a.Execute(context.Background(), testTask())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	handler.mu.Lock()
	defer handler.mu.Unlock()

	if len(handler.records) == 0 {
		t.Fatal("expected at least one log record")
	}

	rec := handler.records[len(handler.records)-1]
	if rec.Message != "openai request completed" {
		t.Errorf("log message: want 'openai request completed', got %q", rec.Message)
	}

	requiredKeys := []string{"model", "input_tokens", "output_tokens", "latency_ms"}
	for _, key := range requiredKeys {
		if _, ok := rec.Attrs[key]; !ok {
			t.Errorf("missing log attribute: %s", key)
		}
	}

	latency, ok := rec.Attrs["latency_ms"]
	if !ok {
		t.Fatal("latency_ms not in log")
	}
	latencyVal, ok := latency.(int64)
	if !ok {
		t.Fatalf("latency_ms type: want int64, got %T", latency)
	}
	if latencyVal < 0 {
		t.Errorf("latency_ms must be non-negative, got %d", latencyVal)
	}
}

// --- 4. Token fields 0 when usage omitted ---

func TestExecute_TokenLogging_ZeroWhenOmitted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(chatResponse{
			ID:      "chatcmpl-456",
			Choices: []choice{{Message: chatMessage{Role: "assistant", Content: `{"ok": true}`}}},
			// Usage intentionally omitted — zero struct.
		})
	}))
	defer srv.Close()

	handler := &capturingHandler{}
	logger := slog.New(handler)
	a := newTestAdapterWithLogger(t, srv.URL, logger)

	_, err := a.Execute(context.Background(), testTask())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	handler.mu.Lock()
	defer handler.mu.Unlock()

	if len(handler.records) == 0 {
		t.Fatal("expected log record even with zero usage")
	}

	rec := handler.records[len(handler.records)-1]
	inputTokens, ok := rec.Attrs["input_tokens"]
	if !ok {
		t.Fatal("input_tokens missing from log when usage omitted")
	}
	if inputTokens.(int64) != 0 {
		t.Errorf("input_tokens should be 0, got %v", inputTokens)
	}
	outputTokens, ok := rec.Attrs["output_tokens"]
	if !ok {
		t.Fatal("output_tokens missing from log when usage omitted")
	}
	if outputTokens.(int64) != 0 {
		t.Errorf("output_tokens should be 0, got %v", outputTokens)
	}
}

// --- 5. Request timeout ---

func TestExecute_Timeout_Retryable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	t.Setenv("OPENAI_API_KEY", "test-key")
	a, err := New(Config{
		ID:      "test-openai",
		Model:   "gpt-4o",
		Timeout: 100 * time.Millisecond,
		BaseURL: srv.URL,
	}, slog.Default())
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}

	goroutinesBefore := runtime.NumGoroutine()

	_, execErr := a.Execute(context.Background(), testTask())
	ae := mustAgentError(t, execErr)

	if !ae.Retryable {
		t.Error("timeout must be retryable")
	}
	if !strings.Contains(ae.Error(), "timeout") && !strings.Contains(ae.Error(), "deadline") &&
		!strings.Contains(ae.Error(), "Timeout") && !strings.Contains(ae.Error(), "Client.Timeout") {
		t.Errorf("error message should mention timeout, got: %s", ae.Error())
	}

	time.Sleep(200 * time.Millisecond)
	goroutinesAfter := runtime.NumGoroutine()
	if goroutinesAfter > goroutinesBefore+2 {
		t.Errorf("possible goroutine leak: before=%d after=%d", goroutinesBefore, goroutinesAfter)
	}
}

// --- 6. Context cancellation mid-request ---

func TestExecute_ContextCancellation(t *testing.T) {
	requestReceived := make(chan struct{})
	testDone := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(requestReceived)
		<-testDone
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL)
	ctx, cancel := context.WithCancel(context.Background())

	goroutinesBefore := runtime.NumGoroutine()

	errCh := make(chan error, 1)
	go func() {
		_, err := a.Execute(ctx, testTask())
		errCh <- err
	}()

	<-requestReceived
	cancel()

	select {
	case err := <-errCh:
		ae := mustAgentError(t, err)
		if !ae.Retryable {
			t.Error("context cancellation must be retryable")
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Execute did not return within 100ms of context cancellation")
	}

	close(testDone)

	time.Sleep(200 * time.Millisecond)
	goroutinesAfter := runtime.NumGoroutine()
	if goroutinesAfter > goroutinesBefore+2 {
		t.Errorf("possible goroutine leak: before=%d after=%d", goroutinesBefore, goroutinesAfter)
	}
}

// --- 7. Response parsing: empty choices ---

func TestExecute_EmptyChoices(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(chatResponse{
			ID:      "chatcmpl-789",
			Choices: []choice{},
			Usage:   usage{PromptTokens: 5, CompletionTokens: 0},
		})
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL)
	_, err := a.Execute(context.Background(), testTask())
	ae := mustAgentError(t, err)

	if ae.Retryable {
		t.Error("empty choices must be non-retryable")
	}
}

// --- 8. Streaming response detection ---

func TestExecute_StreamingResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "data: {\"id\":\"chatcmpl-1\",\"choices\":[{\"delta\":{\"content\":\"Hi\"}}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL)
	_, err := a.Execute(context.Background(), testTask())

	// The adapter should fail — it tries to unmarshal the SSE text as JSON,
	// which will either fail to parse or produce empty choices.
	if err == nil {
		t.Fatal("expected error for streaming response")
	}
	ae := mustAgentError(t, err)
	if ae.Retryable {
		t.Error("streaming response must be non-retryable")
	}
}

// --- 9. HealthCheck with unexpected body ---

func TestHealthCheck_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Method != http.MethodGet {
			t.Errorf("unexpected method: %s", r.Method)
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("missing Authorization header on health check")
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"data":[]}`)
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL)
	if err := a.HealthCheck(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestHealthCheck_UnexpectedBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `<html>nginx upstream ok</html>`)
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL)
	if err := a.HealthCheck(context.Background()); err != nil {
		t.Errorf("HealthCheck should return nil for 200 with unexpected body, got: %v", err)
	}
}

func TestHealthCheck_Failure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL)
	if err := a.HealthCheck(context.Background()); err == nil {
		t.Fatal("expected error for failed health check")
	}
}

// --- 10. HealthCheck timeout ---

func TestHealthCheck_Timeout(t *testing.T) {
	testDone := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-testDone
	}))
	defer srv.Close()
	defer close(testDone)

	a := newTestAdapter(t, srv.URL)

	start := time.Now()
	err := a.HealthCheck(context.Background())
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error from health check timeout")
	}
	if elapsed > 10*time.Second {
		t.Errorf("HealthCheck took %v — should have its own short timeout", elapsed)
	}
}

func TestExecute_MalformedJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{broken json!!!`)
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL)
	_, err := a.Execute(context.Background(), testTask())
	ae := mustAgentError(t, err)

	if ae.Retryable {
		t.Error("malformed JSON must be non-retryable")
	}
}

// --- 11. JSON output parsing (model-returned text must be a JSON object) ---

func jsonContentResponse(text string) chatResponse {
	return chatResponse{
		ID:      "chatcmpl-j",
		Choices: []choice{{Message: chatMessage{Role: "assistant", Content: text}}},
		Usage:   usage{PromptTokens: 1, CompletionTokens: 1},
	}
}

func TestExecute_JSONOutput_ValidObject(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(jsonContentResponse(`{"approved": false, "summary": "test"}`))
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL)
	result, err := a.Execute(context.Background(), testTask())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(result.Payload, &out); err != nil {
		t.Fatalf("payload is not a JSON object: %v", err)
	}
	if out["approved"] != false || out["summary"] != "test" {
		t.Errorf("unexpected output: %v", out)
	}
}

func TestExecute_JSONOutput_MarkdownFenced(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(jsonContentResponse("```json\n{\"key\": \"value\"}\n```"))
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL)
	result, err := a.Execute(context.Background(), testTask())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(result.Payload, &out); err != nil {
		t.Fatalf("payload is not a JSON object: %v", err)
	}
	if out["key"] != "value" {
		t.Errorf("key: want value, got %v", out["key"])
	}
}

func TestExecute_JSONOutput_PlainText_Rejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(jsonContentResponse("This is the review"))
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL)
	_, err := a.Execute(context.Background(), testTask())
	ae := mustAgentError(t, err)
	if ae.Retryable {
		t.Error("plain-text output must be non-retryable")
	}
	if !strings.Contains(ae.Error(), "JSON") && !strings.Contains(ae.Error(), "json") {
		t.Errorf("error should mention JSON, got: %s", ae.Error())
	}
}

func TestExecute_JSONOutput_Empty_Rejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(jsonContentResponse(""))
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL)
	_, err := a.Execute(context.Background(), testTask())
	ae := mustAgentError(t, err)
	if ae.Retryable {
		t.Error("empty output must be non-retryable")
	}
}

func TestExecute_JSONOutput_Array_Rejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(jsonContentResponse("[1, 2, 3]"))
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL)
	_, err := a.Execute(context.Background(), testTask())
	ae := mustAgentError(t, err)
	if ae.Retryable {
		t.Error("array output must be non-retryable (must be an object)")
	}
}

func TestExecute_ConfigurableBaseURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(successResponse())
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL)
	result, err := a.Execute(context.Background(), testTask())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.TaskID != "task-1" {
		t.Errorf("unexpected task ID: %s", result.TaskID)
	}
}
