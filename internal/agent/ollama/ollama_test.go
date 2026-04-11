package ollama

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
	a, err := New(Config{
		ID:           "test-ollama",
		Model:        "llama3",
		SystemPrompt: "You are a test assistant.",
		Timeout:      5 * time.Second,
		Endpoint:     serverURL,
	}, slog.Default())
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}
	return a
}

func newTestAdapterWithLogger(t *testing.T, serverURL string, logger *slog.Logger) *Adapter {
	t.Helper()
	a, err := New(Config{
		ID:           "test-ollama",
		Model:        "llama3",
		SystemPrompt: "You are a test assistant.",
		Timeout:      5 * time.Second,
		Endpoint:     serverURL,
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
		Message:         chatMessage{Role: "assistant", Content: `{"reply": "Hello back!"}`},
		PromptEvalCount: 10,
		EvalCount:       5,
	}
}

// --- Tests ---

func TestExecute_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chat" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}

		var reqBody chatRequest
		if err := json.NewDecoder(r.Body).Decode(&reqBody); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if reqBody.Model != "llama3" {
			t.Errorf("unexpected model: %s", reqBody.Model)
		}
		if reqBody.Stream {
			t.Error("stream should be false")
		}
		if len(reqBody.Messages) != 2 {
			t.Errorf("expected 2 messages (system + user), got %d", len(reqBody.Messages))
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

func TestExecute_NoSystemPrompt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var reqBody chatRequest
		if err := json.NewDecoder(r.Body).Decode(&reqBody); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if len(reqBody.Messages) != 1 {
			t.Errorf("expected 1 message (user only), got %d", len(reqBody.Messages))
		}
		json.NewEncoder(w).Encode(successResponse())
	}))
	defer srv.Close()

	a, err := New(Config{
		ID:       "test-ollama",
		Model:    "llama3",
		Timeout:  5 * time.Second,
		Endpoint: srv.URL,
	}, slog.Default())
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}

	_, err = a.Execute(context.Background(), testTask())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// --- Error classification ---

func TestExecute_ModelNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `model "llama3" not found`)
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL)
	_, err := a.Execute(context.Background(), testTask())
	ae := mustAgentError(t, err)

	if ae.Retryable {
		t.Error("404 model not found must be non-retryable")
	}
}

func TestExecute_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, "internal server error")
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL)
	_, err := a.Execute(context.Background(), testTask())
	ae := mustAgentError(t, err)

	if !ae.Retryable {
		t.Error("500 must be retryable")
	}
}

// --- Field inspection ---

func TestExecute_ErrorFieldInspection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, "boom")
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL)
	_, err := a.Execute(context.Background(), testTask())
	ae := mustAgentError(t, err)

	if ae.AgentID != "test-ollama" {
		t.Errorf("AgentID: want test-ollama, got %s", ae.AgentID)
	}
	if ae.Prov != "ollama" {
		t.Errorf("Prov: want ollama, got %s", ae.Prov)
	}
	if !ae.Retryable {
		t.Error("Retryable: want true")
	}
	if ae.Err == nil {
		t.Error("Err: want non-nil underlying error")
	}
	if !strings.Contains(ae.Error(), "agent test-ollama (ollama)") {
		t.Errorf("Error() format unexpected: %s", ae.Error())
	}
}

// --- Token logging ---

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
	if rec.Message != "ollama request completed" {
		t.Errorf("log message: want 'ollama request completed', got %q", rec.Message)
	}

	requiredKeys := []string{"model", "input_tokens", "output_tokens", "latency_ms"}
	for _, key := range requiredKeys {
		if _, ok := rec.Attrs[key]; !ok {
			t.Errorf("missing log attribute: %s", key)
		}
	}
}

func TestExecute_TokenLogging_ZeroWhenOmitted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(chatResponse{
			Message: chatMessage{Role: "assistant", Content: `{"ok": true}`},
			// Token counts intentionally omitted — zero values.
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
}

// --- Timeout ---

func TestExecute_Timeout_Retryable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	a, err := New(Config{
		ID:       "test-ollama",
		Model:    "llama3",
		Timeout:  100 * time.Millisecond,
		Endpoint: srv.URL,
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

	time.Sleep(200 * time.Millisecond)
	goroutinesAfter := runtime.NumGoroutine()
	if goroutinesAfter > goroutinesBefore+2 {
		t.Errorf("possible goroutine leak: before=%d after=%d", goroutinesBefore, goroutinesAfter)
	}
}

// --- Context cancellation ---

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

// --- Response parsing ---

func TestExecute_EmptyContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(chatResponse{
			Message: chatMessage{Role: "assistant", Content: ""},
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

// --- HealthCheck ---

func TestHealthCheck_Success_ModelFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tags" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Method != http.MethodGet {
			t.Errorf("unexpected method: %s", r.Method)
		}
		json.NewEncoder(w).Encode(tagsResponse{
			Models: []tagModel{
				{Name: "llama3:latest"},
				{Name: "mistral:latest"},
			},
		})
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL)
	if err := a.HealthCheck(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestHealthCheck_Success_ExactMatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(tagsResponse{
			Models: []tagModel{
				{Name: "llama3"},
			},
		})
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL)
	if err := a.HealthCheck(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestHealthCheck_ModelNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(tagsResponse{
			Models: []tagModel{
				{Name: "mistral:latest"},
				{Name: "phi:latest"},
			},
		})
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL)
	err := a.HealthCheck(context.Background())
	if err == nil {
		t.Fatal("expected error when model not found")
	}
	expected := "model llama3 not found locally — run: ollama pull llama3"
	if !strings.Contains(err.Error(), expected) {
		t.Errorf("error should contain actionable message.\nwant substring: %s\ngot: %s", expected, err.Error())
	}
}

func TestHealthCheck_Failure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL)
	if err := a.HealthCheck(context.Background()); err == nil {
		t.Fatal("expected error for failed health check")
	}
}

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

// --- Config ---

func TestNew_ModelRequired(t *testing.T) {
	_, err := New(Config{
		ID:       "test",
		Endpoint: "http://localhost:11434",
	}, slog.Default())
	if err == nil {
		t.Fatal("expected error when model is empty")
	}
}

func TestNew_EndpointFromEnv(t *testing.T) {
	t.Setenv("OLLAMA_ENDPOINT", "https://custom:9999")
	a, err := New(Config{
		ID:    "test",
		Model: "llama3",
	}, slog.Default())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if a.cfg.Endpoint != "https://custom:9999" {
		t.Errorf("endpoint: want https://custom:9999, got %s", a.cfg.Endpoint)
	}
}

// --- Item 4: HealthCheck model not found — actionable error ---

func TestHealthCheck_ModelNotFound_ActionableError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(tagsResponse{
			Models: []tagModel{
				{Name: "codellama:7b"},
				{Name: "mistral:latest"},
				{Name: "phi:latest"},
			},
		})
	}))
	defer srv.Close()

	a, err := New(Config{
		ID:       "test",
		Model:    "deepseek-r1",
		Endpoint: srv.URL,
	}, slog.Default())
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}

	err = a.HealthCheck(context.Background())
	if err == nil {
		t.Fatal("expected error when model not found")
	}

	errMsg := err.Error()

	// Must contain the missing model name so the user knows what's wrong.
	if !strings.Contains(errMsg, "deepseek-r1") {
		t.Errorf("error must contain the missing model name.\ngot: %s", errMsg)
	}

	// Must contain the exact pull command so the user can copy-paste it.
	if !strings.Contains(errMsg, "ollama pull deepseek-r1") {
		t.Errorf("error must contain the exact 'ollama pull <model>' command.\ngot: %s", errMsg)
	}
}

// --- Item 5: Verify adapter sends stream: false ---

func TestExecute_StreamFalse(t *testing.T) {
	// Ollama's /api/chat returns newline-delimited JSON by default (streaming).
	// The adapter MUST send stream: false to get a single JSON response.
	// This is critical because the adapter parses a single chatResponse, not
	// a stream of chunks.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var reqBody chatRequest
		if err := json.NewDecoder(r.Body).Decode(&reqBody); err != nil {
			t.Fatalf("decode request: %v", err)
		}

		// This is the key assertion: stream must be explicitly false.
		if reqBody.Stream {
			t.Error("adapter MUST send stream: false — Ollama defaults to streaming NDJSON which the adapter cannot parse")
		}

		json.NewEncoder(w).Encode(successResponse())
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL)
	_, err := a.Execute(context.Background(), testTask())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// --- Item 6: Endpoint configuration precedence ---

func TestNew_EndpointPrecedence_ConfigOverEnv(t *testing.T) {
	// Precedence: Config.Endpoint > OLLAMA_ENDPOINT env var > default.
	// If both are set, the explicit config value wins. This matches the principle
	// that YAML is the single source of truth — env vars are fallbacks only.
	t.Setenv("OLLAMA_ENDPOINT", "https://env-host:9999")

	a, err := New(Config{
		ID:       "test",
		Model:    "llama3",
		Endpoint: "https://config-host:8888",
	}, slog.Default())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if a.cfg.Endpoint != "https://config-host:8888" {
		t.Errorf("config endpoint should take precedence over env var.\nwant: https://config-host:8888\ngot:  %s", a.cfg.Endpoint)
	}
}

func TestNew_EndpointPrecedence_EnvOverDefault(t *testing.T) {
	t.Setenv("OLLAMA_ENDPOINT", "https://env-host:9999")

	a, err := New(Config{
		ID:    "test",
		Model: "llama3",
		// Endpoint intentionally empty — env var should be used.
	}, slog.Default())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if a.cfg.Endpoint != "https://env-host:9999" {
		t.Errorf("env var should take precedence over default.\nwant: https://env-host:9999\ngot:  %s", a.cfg.Endpoint)
	}
}

func TestNew_EndpointPrecedence_DefaultFallback(t *testing.T) {
	t.Setenv("OLLAMA_ENDPOINT", "") // Explicitly unset.

	a, err := New(Config{
		ID:    "test",
		Model: "llama3",
	}, slog.Default())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if a.cfg.Endpoint != "http://localhost:11434" {
		t.Errorf("should fall back to default endpoint.\nwant: http://localhost:11434\ngot:  %s", a.cfg.Endpoint)
	}
}

// --- JSON output parsing ---

func jsonTextResponse(text string) chatResponse {
	return chatResponse{
		Message:         chatMessage{Role: "assistant", Content: text},
		PromptEvalCount: 1,
		EvalCount:       1,
	}
}

func TestExecute_JSONOutput_ValidObject(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(jsonTextResponse(`{"approved": false, "summary": "test"}`))
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
		json.NewEncoder(w).Encode(jsonTextResponse("```json\n{\"key\": \"value\"}\n```"))
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
		json.NewEncoder(w).Encode(jsonTextResponse("This is the review"))
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
		json.NewEncoder(w).Encode(jsonTextResponse(""))
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
		json.NewEncoder(w).Encode(jsonTextResponse("[1, 2, 3]"))
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL)
	_, err := a.Execute(context.Background(), testTask())
	ae := mustAgentError(t, err)
	if ae.Retryable {
		t.Error("array output must be non-retryable (must be an object)")
	}
}
