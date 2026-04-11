package anthropic

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
	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	a, err := New(Config{
		ID:           "test-anthropic",
		Model:        "claude-sonnet-4-5",
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
	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	a, err := New(Config{
		ID:           "test-anthropic",
		Model:        "claude-sonnet-4-5",
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

func successResponse() messagesResponse {
	return messagesResponse{
		ID:      "msg_123",
		Content: []contentBlock{{Type: "text", Text: `{"reply": "Hello back!"}`}},
		Usage:   usage{InputTokens: 10, OutputTokens: 5},
	}
}

// --- Existing tests ---

func TestExecute_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("X-API-Key") != "test-key" {
			t.Errorf("missing API key header")
		}
		if r.Header.Get("Anthropic-Version") != apiVersion {
			t.Errorf("missing version header")
		}

		var reqBody messagesRequest
		if err := json.NewDecoder(r.Body).Decode(&reqBody); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if reqBody.Model != "claude-sonnet-4-5" {
			t.Errorf("unexpected model: %s", reqBody.Model)
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
	if result.Metadata["input_tokens"] != 10 {
		t.Errorf("unexpected input_tokens: %v", result.Metadata["input_tokens"])
	}
	if result.Metadata["output_tokens"] != 5 {
		t.Errorf("unexpected output_tokens: %v", result.Metadata["output_tokens"])
	}
}

// --- 1. Error classification tests ---

func TestExecute_RateLimited_WithRetryAfter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
		json.NewEncoder(w).Encode(errorResponse{
			Error: apiError{Type: "rate_limit_error", Message: "Too many requests"},
		})
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL)
	_, err := a.Execute(context.Background(), testTask())
	ae := mustAgentError(t, err)

	if !ae.Retryable {
		t.Error("429 must be retryable")
	}
	if ae.RetryAfter != 30*time.Second {
		t.Errorf("RetryAfter: want 30s, got %v", ae.RetryAfter)
	}
}

func TestExecute_RateLimited_WithoutRetryAfter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		json.NewEncoder(w).Encode(errorResponse{
			Error: apiError{Type: "rate_limit_error", Message: "Too many requests"},
		})
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL)
	_, err := a.Execute(context.Background(), testTask())
	ae := mustAgentError(t, err)

	if !ae.Retryable {
		t.Error("429 without Retry-After must still be retryable")
	}
	if ae.RetryAfter != 0 {
		t.Errorf("RetryAfter should be 0 when header absent, got %v", ae.RetryAfter)
	}
}

func TestExecute_529_Overloaded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(529)
		json.NewEncoder(w).Encode(errorResponse{
			Error: apiError{Type: "overloaded_error", Message: "Overloaded"},
		})
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL)
	_, err := a.Execute(context.Background(), testTask())
	ae := mustAgentError(t, err)

	if !ae.Retryable {
		t.Error("529 overloaded must be retryable")
	}
}

func TestExecute_500_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(errorResponse{
			Error: apiError{Type: "api_error", Message: "Internal server error"},
		})
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL)
	_, err := a.Execute(context.Background(), testTask())
	ae := mustAgentError(t, err)

	if !ae.Retryable {
		t.Error("500 must be retryable")
	}
}

func TestExecute_400_BadRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(errorResponse{
			Error: apiError{Type: "invalid_request_error", Message: "invalid model"},
		})
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL)
	_, err := a.Execute(context.Background(), testTask())
	ae := mustAgentError(t, err)

	if ae.Retryable {
		t.Error("400 bad request must be non-retryable")
	}
}

func TestExecute_401_AuthError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(errorResponse{
			Error: apiError{Type: "authentication_error", Message: "Invalid API key"},
		})
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL)
	_, err := a.Execute(context.Background(), testTask())
	ae := mustAgentError(t, err)

	if ae.Retryable {
		t.Error("401 auth error must be non-retryable")
	}
}

func TestExecute_403_PermissionDenied(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(errorResponse{
			Error: apiError{Type: "permission_error", Message: "Permission denied"},
		})
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL)
	_, err := a.Execute(context.Background(), testTask())
	ae := mustAgentError(t, err)

	if ae.Retryable {
		t.Error("403 permission denied must be non-retryable")
	}
}

func TestExecute_ContextWindowExceeded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(errorResponse{
			Error: apiError{Type: "invalid_request_error", Message: "prompt is too long: 200000 tokens > 100000 maximum"},
		})
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL)
	_, err := a.Execute(context.Background(), testTask())
	ae := mustAgentError(t, err)

	if ae.Retryable {
		t.Error("context window exceeded must be non-retryable")
	}
}

// --- 2. Retry-After header propagation ---

func TestExecute_RetryAfter_FieldInspection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
		json.NewEncoder(w).Encode(errorResponse{
			Error: apiError{Type: "rate_limit_error", Message: "Too many requests"},
		})
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL)
	_, err := a.Execute(context.Background(), testTask())
	ae := mustAgentError(t, err)

	// Inspect all fields, not just IsRetryable().
	if ae.AgentID != "test-anthropic" {
		t.Errorf("AgentID: want test-anthropic, got %s", ae.AgentID)
	}
	if ae.Prov != "anthropic" {
		t.Errorf("Prov: want anthropic, got %s", ae.Prov)
	}
	if !ae.Retryable {
		t.Error("Retryable: want true")
	}
	if ae.RetryAfter != 30*time.Second {
		t.Errorf("RetryAfter: want 30s, got %v", ae.RetryAfter)
	}
	if ae.Err == nil {
		t.Error("Err: want non-nil underlying error")
	}
	if !strings.Contains(ae.Error(), "agent test-anthropic (anthropic)") {
		t.Errorf("Error() format unexpected: %s", ae.Error())
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
	if rec.Message != "anthropic request completed" {
		t.Errorf("log message: want 'anthropic request completed', got %q", rec.Message)
	}

	requiredKeys := []string{"model", "input_tokens", "output_tokens", "latency_ms"}
	for _, key := range requiredKeys {
		if _, ok := rec.Attrs[key]; !ok {
			t.Errorf("missing log attribute: %s", key)
		}
	}

	// latency_ms must be a positive integer (int64 from time.Since().Milliseconds())
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
		// Return response with no usage data — Go zero values.
		json.NewEncoder(w).Encode(messagesResponse{
			ID:      "msg_456",
			Content: []contentBlock{{Type: "text", Text: `{"ok": true}`}},
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

	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	a, err := New(Config{
		ID:      "test-anthropic",
		Model:   "claude-sonnet-4-5",
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

	// Check for goroutine leak — allow some slack for GC/runtime goroutines.
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
		// Block until the test signals completion so the server doesn't respond.
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

	// Wait for the server to receive the request, then cancel.
	<-requestReceived
	cancel()

	// Execute must return promptly (within 100ms of cancel).
	select {
	case err := <-errCh:
		ae := mustAgentError(t, err)
		if !ae.Retryable {
			t.Error("context cancellation must be retryable")
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Execute did not return within 100ms of context cancellation")
	}

	// Unblock the server handler so srv.Close() doesn't hang.
	close(testDone)

	// Check for goroutine leak.
	time.Sleep(200 * time.Millisecond)
	goroutinesAfter := runtime.NumGoroutine()
	if goroutinesAfter > goroutinesBefore+2 {
		t.Errorf("possible goroutine leak: before=%d after=%d", goroutinesBefore, goroutinesAfter)
	}
}

// --- 7. Response parsing: empty/unsupported content ---

func TestExecute_EmptyContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(messagesResponse{
			ID:      "msg_789",
			Content: []contentBlock{},
			Usage:   usage{InputTokens: 5, OutputTokens: 0},
		})
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL)
	_, err := a.Execute(context.Background(), testTask())
	ae := mustAgentError(t, err)

	if ae.Retryable {
		t.Error("empty content must be non-retryable")
	}
}

func TestExecute_ToolUseOnly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(messagesResponse{
			ID: "msg_tool",
			Content: []contentBlock{
				{Type: "tool_use", Text: ""},
			},
			Usage: usage{InputTokens: 5, OutputTokens: 10},
		})
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL)
	_, err := a.Execute(context.Background(), testTask())
	ae := mustAgentError(t, err)

	if ae.Retryable {
		t.Error("tool_use-only response must be non-retryable (Orcastrator doesn't support tool calls)")
	}
}

// --- 8. Streaming response detection ---

func TestExecute_StreamingResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\"}\n\n")
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL)
	_, err := a.Execute(context.Background(), testTask())
	ae := mustAgentError(t, err)

	if ae.Retryable {
		t.Error("streaming response must be non-retryable (adapter expects non-streaming)")
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
		// Completely unexpected body — HTML, garbage, whatever. It's 200, so it's healthy.
		fmt.Fprint(w, `<html>load balancer ok</html>`)
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
	// HealthCheck has its own 5s timeout. It should return well before the
	// agent's 120s default, and definitely before 10s.
	if elapsed > 10*time.Second {
		t.Errorf("HealthCheck took %v — should have its own short timeout", elapsed)
	}
}

// --- 11. JSON output parsing (model-returned text must be a JSON object) ---

func jsonTextResponse(text string) string {
	// Build a messagesResponse JSON literal containing the given model output text.
	escaped, _ := json.Marshal(text)
	return fmt.Sprintf(`{"id":"msg_j","content":[{"type":"text","text":%s}],"usage":{"input_tokens":1,"output_tokens":1}}`, string(escaped))
}

func TestExecute_JSONOutput_ValidObject(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, jsonTextResponse(`{"approved": false, "summary": "test"}`))
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
	if out["approved"] != false {
		t.Errorf("approved: want false, got %v", out["approved"])
	}
	if out["summary"] != "test" {
		t.Errorf("summary: want test, got %v", out["summary"])
	}
}

func TestExecute_JSONOutput_MarkdownFenced(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, jsonTextResponse("```json\n{\"key\": \"value\"}\n```"))
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
		fmt.Fprint(w, jsonTextResponse("This is the review"))
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
		fmt.Fprint(w, jsonTextResponse(""))
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
		fmt.Fprint(w, jsonTextResponse("[1, 2, 3]"))
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL)
	_, err := a.Execute(context.Background(), testTask())
	ae := mustAgentError(t, err)
	if ae.Retryable {
		t.Error("array output must be non-retryable (must be an object)")
	}
}

func TestExecute_MalformedJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{not valid json`)
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL)
	_, err := a.Execute(context.Background(), testTask())
	ae := mustAgentError(t, err)

	if ae.Retryable {
		t.Error("malformed JSON must be non-retryable")
	}
}
