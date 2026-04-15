package google

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

	"github.com/brianbuquoi/overlord/internal/agent"
	"github.com/brianbuquoi/overlord/internal/broker"
)

func newTestAdapter(t *testing.T, serverURL string) *Adapter {
	t.Helper()
	t.Setenv("GEMINI_API_KEY", "test-key")
	a, err := New(Config{
		ID:           "test-gemini",
		Model:        "gemini-2.5-pro",
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
	t.Setenv("GEMINI_API_KEY", "test-key")
	a, err := New(Config{
		ID:           "test-gemini",
		Model:        "gemini-2.5-pro",
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

func successResponse() generateResponse {
	return generateResponse{
		Candidates: []candidate{
			{Content: content{Parts: []part{{Text: `{"reply": "Hello back!"}`}}}},
		},
		UsageMetadata: &usageMetadata{PromptTokenCount: 10, CandidatesTokenCount: 5},
	}
}

// --- Tests ---

func TestExecute_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/v1beta/models/gemini-2.5-pro:generateContent") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("X-Goog-Api-Key") != "test-key" {
			t.Errorf("missing API key in X-Goog-Api-Key header")
		}
		if r.URL.Query().Get("key") != "" {
			t.Errorf("API key must not appear in URL query params")
		}

		var reqBody generateRequest
		if err := json.NewDecoder(r.Body).Decode(&reqBody); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if len(reqBody.Contents) != 1 {
			t.Errorf("expected 1 content, got %d", len(reqBody.Contents))
		}
		if reqBody.SystemInstruction == nil {
			t.Error("expected system instruction")
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
		var reqBody generateRequest
		if err := json.NewDecoder(r.Body).Decode(&reqBody); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if reqBody.SystemInstruction != nil {
			t.Error("system instruction should be nil when no system prompt configured")
		}
		json.NewEncoder(w).Encode(successResponse())
	}))
	defer srv.Close()

	t.Setenv("GEMINI_API_KEY", "test-key")
	a, err := New(Config{
		ID:      "test-gemini",
		Model:   "gemini-2.0-flash",
		Timeout: 5 * time.Second,
		BaseURL: srv.URL,
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

func TestExecute_ResourceExhausted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		json.NewEncoder(w).Encode(geminiError{
			Error: geminiErrorDetail{Code: 429, Status: "RESOURCE_EXHAUSTED", Message: "Quota exceeded"},
		})
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL)
	_, err := a.Execute(context.Background(), testTask())
	ae := mustAgentError(t, err)

	if !ae.Retryable {
		t.Error("RESOURCE_EXHAUSTED must be retryable")
	}
}

func TestExecute_InvalidArgument(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(geminiError{
			Error: geminiErrorDetail{Code: 400, Status: "INVALID_ARGUMENT", Message: "Invalid value"},
		})
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL)
	_, err := a.Execute(context.Background(), testTask())
	ae := mustAgentError(t, err)

	if ae.Retryable {
		t.Error("INVALID_ARGUMENT must be non-retryable")
	}
}

func TestExecute_AuthError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(geminiError{
			Error: geminiErrorDetail{Code: 401, Status: "UNAUTHENTICATED", Message: "Invalid API key"},
		})
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL)
	_, err := a.Execute(context.Background(), testTask())
	ae := mustAgentError(t, err)

	if ae.Retryable {
		t.Error("401 must be non-retryable")
	}
}

func TestExecute_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(geminiError{
			Error: geminiErrorDetail{Code: 500, Status: "INTERNAL", Message: "Internal error"},
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

// --- Retry-After field inspection ---

func TestExecute_RetryAfter_FieldInspection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		json.NewEncoder(w).Encode(geminiError{
			Error: geminiErrorDetail{Code: 429, Status: "RESOURCE_EXHAUSTED", Message: "Quota exceeded"},
		})
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL)
	_, err := a.Execute(context.Background(), testTask())
	ae := mustAgentError(t, err)

	if ae.AgentID != "test-gemini" {
		t.Errorf("AgentID: want test-gemini, got %s", ae.AgentID)
	}
	if ae.Prov != "google" {
		t.Errorf("Prov: want google, got %s", ae.Prov)
	}
	if !ae.Retryable {
		t.Error("Retryable: want true")
	}
	if ae.Err == nil {
		t.Error("Err: want non-nil underlying error")
	}
	if !strings.Contains(ae.Error(), "agent test-gemini (google)") {
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
	if rec.Message != "gemini request completed" {
		t.Errorf("log message: want 'gemini request completed', got %q", rec.Message)
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
		json.NewEncoder(w).Encode(generateResponse{
			Candidates: []candidate{
				{Content: content{Parts: []part{{Text: `{"ok": true}`}}}},
			},
			// UsageMetadata intentionally omitted.
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

	t.Setenv("GEMINI_API_KEY", "test-key")
	a, err := New(Config{
		ID:      "test-gemini",
		Model:   "gemini-2.5-pro",
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

func TestExecute_EmptyCandidates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(generateResponse{
			Candidates: []candidate{},
		})
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL)
	_, err := a.Execute(context.Background(), testTask())
	ae := mustAgentError(t, err)

	if ae.Retryable {
		t.Error("empty candidates must be non-retryable")
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

func TestHealthCheck_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/v1beta/models/gemini-2.5-pro") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Method != http.MethodGet {
			t.Errorf("unexpected method: %s", r.Method)
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"name":"models/gemini-2.5-pro"}`)
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL)
	if err := a.HealthCheck(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestHealthCheck_Failure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL)
	if err := a.HealthCheck(context.Background()); err == nil {
		t.Fatal("expected error for failed health check")
	}
}

// --- Item 1: Realistic Gemini response with multi-part content ---

func TestExecute_RealisticMultiPartResponse(t *testing.T) {
	// A realistic Gemini API response with multiple text parts in a single candidate,
	// usage metadata, and a finishReason — matching the actual API shape.
	realisticJSON := `{
		"candidates": [
			{
				"content": {
					"parts": [
						{"text": "{\"summary\":"},
						{"text": " \"analysis complete\","},
						{"text": " \"records\": 3,"},
						{"text": " \"action\": \"proceed\"}"}
					],
					"role": "model"
				},
				"finishReason": "STOP",
				"index": 0,
				"safetyRatings": [
					{"category": "HARM_CATEGORY_SEXUALLY_EXPLICIT", "probability": "NEGLIGIBLE"},
					{"category": "HARM_CATEGORY_HATE_SPEECH", "probability": "NEGLIGIBLE"},
					{"category": "HARM_CATEGORY_HARASSMENT", "probability": "NEGLIGIBLE"},
					{"category": "HARM_CATEGORY_DANGEROUS_CONTENT", "probability": "NEGLIGIBLE"}
				]
			}
		],
		"usageMetadata": {
			"promptTokenCount": 42,
			"candidatesTokenCount": 87,
			"totalTokenCount": 129
		}
	}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, realisticJSON)
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL)
	result, err := a.Execute(context.Background(), testTask())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var output map[string]any
	if err := json.Unmarshal(result.Payload, &output); err != nil {
		t.Fatalf("payload is not a JSON object: %v", err)
	}

	// Verify the FULL text is assembled from all parts, not just the first one —
	// the parsed object is only valid if every part was concatenated.
	if output["summary"] != "analysis complete" {
		t.Errorf("summary: want 'analysis complete', got %v", output["summary"])
	}
	if output["action"] != "proceed" {
		t.Errorf("action: want 'proceed', got %v", output["action"])
	}
	if records, ok := output["records"].(float64); !ok || records != 3 {
		t.Errorf("records: want 3, got %v", output["records"])
	}

	if result.Metadata["input_tokens"] != 42 {
		t.Errorf("input_tokens: want 42, got %v", result.Metadata["input_tokens"])
	}
	if result.Metadata["output_tokens"] != 87 {
		t.Errorf("output_tokens: want 87, got %v", result.Metadata["output_tokens"])
	}
}

// --- Item 2: Safety filter — no candidates (promptFeedback.blockReason) ---

func TestExecute_SafetyFilter_NoCandidates(t *testing.T) {
	// Gemini returns no candidates when the prompt itself is blocked by safety filters.
	// The response includes promptFeedback with a blockReason.
	safetyJSON := `{
		"promptFeedback": {
			"blockReason": "SAFETY",
			"safetyRatings": [
				{"category": "HARM_CATEGORY_SEXUALLY_EXPLICIT", "probability": "HIGH"},
				{"category": "HARM_CATEGORY_HATE_SPEECH", "probability": "NEGLIGIBLE"},
				{"category": "HARM_CATEGORY_HARASSMENT", "probability": "NEGLIGIBLE"},
				{"category": "HARM_CATEGORY_DANGEROUS_CONTENT", "probability": "NEGLIGIBLE"}
			]
		}
	}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, safetyJSON)
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL)
	_, err := a.Execute(context.Background(), testTask())
	ae := mustAgentError(t, err)

	if ae.Retryable {
		t.Error("safety filter must be non-retryable")
	}
	if !strings.Contains(ae.Error(), "safety filter") {
		t.Errorf("error should indicate safety filtering, got: %s", ae.Error())
	}
	if !strings.Contains(ae.Error(), "SAFETY") {
		t.Errorf("error should contain the block reason, got: %s", ae.Error())
	}
}

// --- Item 2b: Safety filter — candidate with finishReason: SAFETY ---

func TestExecute_SafetyFilter_CandidateFinishReason(t *testing.T) {
	// Gemini may return a candidate with finishReason SAFETY and truncated/empty content.
	safetyJSON := `{
		"candidates": [
			{
				"content": {
					"parts": [{"text": ""}],
					"role": "model"
				},
				"finishReason": "SAFETY",
				"safetyRatings": [
					{"category": "HARM_CATEGORY_DANGEROUS_CONTENT", "probability": "HIGH"}
				]
			}
		]
	}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, safetyJSON)
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL)
	_, err := a.Execute(context.Background(), testTask())
	ae := mustAgentError(t, err)

	if ae.Retryable {
		t.Error("safety-filtered candidate must be non-retryable")
	}
	if !strings.Contains(ae.Error(), "safety filter") {
		t.Errorf("error should indicate safety filtering, got: %s", ae.Error())
	}
}

// --- Item 3: RESOURCE_EXHAUSTED with actual Gemini error JSON ---

func TestExecute_ResourceExhausted_GeminiErrorFormat(t *testing.T) {
	// The actual Gemini API returns a structured error body with status RESOURCE_EXHAUSTED.
	geminiErrorJSON := `{
		"error": {
			"code": 429,
			"message": "Resource has been exhausted (e.g. check quota).",
			"status": "RESOURCE_EXHAUSTED",
			"details": [
				{
					"@type": "type.googleapis.com/google.rpc.RetryInfo",
					"retryDelay": "36s"
				},
				{
					"@type": "type.googleapis.com/google.rpc.ErrorInfo",
					"reason": "RATE_LIMIT_EXCEEDED",
					"domain": "googleapis.com",
					"metadata": {
						"service": "generativelanguage.googleapis.com",
						"consumer": "projects/12345"
					}
				}
			]
		}
	}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, geminiErrorJSON)
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL)
	_, err := a.Execute(context.Background(), testTask())
	ae := mustAgentError(t, err)

	if !ae.Retryable {
		t.Error("RESOURCE_EXHAUSTED must be retryable")
	}
	if !strings.Contains(ae.Error(), "rate limited") {
		t.Errorf("error should indicate rate limiting, got: %s", ae.Error())
	}
	if !strings.Contains(ae.Error(), "Resource has been exhausted") {
		t.Errorf("error should preserve original message, got: %s", ae.Error())
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

// --- JSON output parsing ---

func jsonTextResponse(text string) generateResponse {
	return generateResponse{
		Candidates:    []candidate{{Content: content{Parts: []part{{Text: text}}}}},
		UsageMetadata: &usageMetadata{PromptTokenCount: 1, CandidatesTokenCount: 1},
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

func TestExecute_JSONOutput_PlainText_Wrapped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(jsonTextResponse("This is the review"))
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL)
	res, err := a.Execute(context.Background(), testTask())
	if err != nil {
		t.Fatalf("non-JSON text should wrap, not fail: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(res.Payload, &out); err != nil {
		t.Fatalf("payload not an object: %v", err)
	}
	if out["text"] != "This is the review" {
		t.Errorf("wrapped text: got %v", out["text"])
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
	if !ae.Retryable {
		t.Error("empty output must be retryable")
	}
}

func TestExecute_JSONOutput_Array_Wrapped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(jsonTextResponse("[1, 2, 3]"))
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL)
	res, err := a.Execute(context.Background(), testTask())
	if err != nil {
		t.Fatalf("array should wrap, not fail: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(res.Payload, &out); err != nil {
		t.Fatalf("payload not an object: %v", err)
	}
	if out["text"] != "[1, 2, 3]" {
		t.Errorf("wrapped text: got %v", out["text"])
	}
}
