// Package openai_responses implements agent.Agent for the OpenAI Responses
// API (POST /v1/responses), which is used by the Codex model family
// (codex-mini-latest and gpt-5.x-codex variants).
//
// This is a separate adapter from the existing openai package because the
// Responses API has a different request/response shape than Chat
// Completions: top-level `instructions` and `input` fields rather than a
// messages array, and an `output[].content[].text` response path rather
// than `choices[].message.content`. Sharing code with Chat Completions
// would force both adapters through a compatibility layer that obscures
// either API's specifics.
package openai_responses

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/brianbuquoi/overlord/internal/agent"
	"github.com/brianbuquoi/overlord/internal/broker"
	"github.com/brianbuquoi/overlord/internal/metrics"
)

const (
	defaultBaseURL = "https://api.openai.com"
	defaultTimeout = 120 * time.Second
	providerName   = "openai-responses"
)

// Config holds the adapter configuration derived from the YAML agent block.
type Config struct {
	ID           string
	Model        string
	APIKeyEnv    string
	SystemPrompt string
	Temperature  float64
	MaxTokens    int
	Timeout      time.Duration
	BaseURL      string
}

// Adapter implements agent.Agent for the OpenAI Responses API.
type Adapter struct {
	cfg     Config
	client  *http.Client
	apiKey  string
	logger  *slog.Logger
	metrics *metrics.Metrics
}

// New creates a new Responses API adapter. API key resolution is deferred
// to Execute/HealthCheck so construction does not require credentials.
func New(cfg Config, logger *slog.Logger, m ...*metrics.Metrics) (*Adapter, error) {
	if cfg.APIKeyEnv == "" {
		cfg.APIKeyEnv = "OPENAI_API_KEY"
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = defaultBaseURL
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	if err := requireTLS(cfg.BaseURL); err != nil {
		return nil, err
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = defaultTimeout
	}
	if logger == nil {
		logger = slog.Default()
	}
	a := &Adapter{
		cfg:    cfg,
		client: &http.Client{Timeout: cfg.Timeout},
		logger: logger,
	}
	if len(m) > 0 && m[0] != nil {
		a.metrics = m[0]
	}
	return a, nil
}

func requireTLS(baseURL string) error {
	u, err := url.Parse(baseURL)
	if err != nil {
		return fmt.Errorf("openai_responses: invalid base URL: %w", err)
	}
	if u.Scheme == "https" {
		return nil
	}
	host := u.Hostname()
	if host == "localhost" || host == "127.0.0.1" || host == "::1" {
		return nil
	}
	return fmt.Errorf("openai_responses: base URL must use HTTPS (got %s); API keys must not be sent over plaintext HTTP", baseURL)
}

func (a *Adapter) resolveAPIKey() (string, error) {
	if a.apiKey != "" {
		return a.apiKey, nil
	}
	key := os.Getenv(a.cfg.APIKeyEnv)
	if key == "" {
		return "", fmt.Errorf("openai_responses: env var %s is not set", a.cfg.APIKeyEnv)
	}
	a.apiKey = key
	return key, nil
}

func (a *Adapter) ID() string       { return a.cfg.ID }
func (a *Adapter) Provider() string { return providerName }

// responsesRequest is the body sent to POST /v1/responses. `instructions`
// carries the system prompt and `input` carries the user message as a
// string (the API also accepts a messages array; a plain string maps more
// cleanly onto Overlord's per-stage envelope).
type responsesRequest struct {
	Model           string   `json:"model"`
	Input           string   `json:"input"`
	Instructions    string   `json:"instructions,omitempty"`
	MaxOutputTokens int      `json:"max_output_tokens,omitempty"`
	Temperature     *float64 `json:"temperature,omitempty"`
}

// responsesResponse is the shape returned from POST /v1/responses.
type responsesResponse struct {
	ID     string         `json:"id"`
	Output []outputItem   `json:"output"`
	Usage  responsesUsage `json:"usage"`
}

type outputItem struct {
	Type    string         `json:"type"`
	Content []contentBlock `json:"content"`
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type responsesUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

type apiError struct {
	Error apiErrorDetail `json:"error"`
}

type apiErrorDetail struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
}

// Execute sends the task payload to POST /v1/responses and maps the
// result into a TaskResult. Error classification (retryable vs not)
// mirrors the existing openai adapter for consistency.
func (a *Adapter) Execute(ctx context.Context, task *broker.Task) (*broker.TaskResult, error) {
	apiKey, err := a.resolveAPIKey()
	if err != nil {
		return nil, a.nonRetryableErr(err)
	}

	start := time.Now()

	var temp *float64
	if a.cfg.Temperature > 0 {
		t := a.cfg.Temperature
		temp = &t
	}

	systemPrompt := task.Prompt
	if systemPrompt == "" {
		systemPrompt = a.cfg.SystemPrompt
	}

	reqBody := responsesRequest{
		Model:           a.cfg.Model,
		Input:           string(task.Payload),
		Instructions:    systemPrompt,
		MaxOutputTokens: a.cfg.MaxTokens,
		Temperature:     temp,
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, a.nonRetryableErr(fmt.Errorf("marshal request: %w", err))
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.cfg.BaseURL+"/v1/responses", bytes.NewReader(body))
	if err != nil {
		return nil, a.nonRetryableErr(fmt.Errorf("build request: %w", err))
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := a.client.Do(req)
	if err != nil {
		return nil, a.retryableErr(fmt.Errorf("http request: %w", err))
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, a.retryableErr(fmt.Errorf("read response: %w", err))
	}

	latencyMs := time.Since(start).Milliseconds()

	if resp.StatusCode != http.StatusOK {
		return nil, a.handleErrorResponse(resp, respBody)
	}

	var result responsesResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, a.nonRetryableErr(fmt.Errorf("unmarshal response: %w", err))
	}

	output, err := extractOutputText(&result)
	if err != nil {
		return nil, a.nonRetryableErr(err)
	}

	a.logger.Info("openai_responses request completed",
		"agent_id", a.cfg.ID,
		"model", a.cfg.Model,
		"input_tokens", result.Usage.InputTokens,
		"output_tokens", result.Usage.OutputTokens,
		"latency_ms", latencyMs,
	)

	if a.metrics != nil {
		dur := time.Since(start).Seconds()
		a.metrics.AgentRequestDuration.WithLabelValues(providerName, a.cfg.Model).Observe(dur)
		a.metrics.AgentTokensTotal.WithLabelValues(providerName, a.cfg.Model, "input").Add(float64(result.Usage.InputTokens))
		a.metrics.AgentTokensTotal.WithLabelValues(providerName, a.cfg.Model, "output").Add(float64(result.Usage.OutputTokens))
	}

	// Parse-then-fallback: the Responses API is often used with models that
	// aren't explicitly prompted for JSON. If the raw text isn't a JSON
	// object, wrap it as {"text": "<raw>"} so downstream schema validation
	// receives a consistent envelope shape.
	payload, err := parseOrWrap(output)
	if err != nil {
		return nil, a.nonRetryableErr(err)
	}

	return &broker.TaskResult{
		TaskID:  task.ID,
		Payload: payload,
		Metadata: map[string]any{
			"provider":      providerName,
			"model":         a.cfg.Model,
			"input_tokens":  result.Usage.InputTokens,
			"output_tokens": result.Usage.OutputTokens,
			"latency_ms":    latencyMs,
		},
	}, nil
}

// parseOrWrap returns the raw bytes of a JSON object if the text already
// decodes as one; otherwise it wraps the raw text as {"text": "<raw>"}.
// Surrounding ``` code fences are stripped before the JSON attempt so
// models that emit markdown still produce a proper object payload. Only
// JSON *objects* pass through unchanged — JSON arrays and scalars are
// wrapped so downstream schema validation (which requires an object root)
// receives a consistent shape.
func parseOrWrap(text string) (json.RawMessage, error) {
	cleaned := strings.TrimSpace(text)
	if strings.HasPrefix(cleaned, "```") {
		if idx := strings.Index(cleaned, "\n"); idx != -1 {
			cleaned = cleaned[idx+1:]
		}
		if idx := strings.LastIndex(cleaned, "```"); idx != -1 {
			cleaned = cleaned[:idx]
		}
		cleaned = strings.TrimSpace(cleaned)
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(cleaned), &obj); err == nil {
		return json.RawMessage(cleaned), nil
	}
	wrapped, err := json.Marshal(map[string]any{"text": text})
	if err != nil {
		return nil, fmt.Errorf("wrap non-JSON output: %w", err)
	}
	return wrapped, nil
}

// extractOutputText walks the nested output[].content[] shape that the
// Responses API uses and returns the first output_text block on a message
// item. Any other shape (empty output, non-message items only, no
// output_text block) is a non-retryable contract violation.
func extractOutputText(r *responsesResponse) (string, error) {
	if len(r.Output) == 0 {
		return "", fmt.Errorf("empty output array in response")
	}
	for _, item := range r.Output {
		if item.Type != "message" {
			continue
		}
		for _, c := range item.Content {
			if c.Type == "output_text" {
				return c.Text, nil
			}
		}
	}
	return "", fmt.Errorf("no message/output_text block found in response")
}

func (a *Adapter) handleErrorResponse(resp *http.Response, body []byte) *agent.AgentError {
	var errResp apiError
	_ = json.Unmarshal(body, &errResp)

	code := errResp.Error.Code
	msg := errResp.Error.Message
	if msg == "" {
		msg = fmt.Sprintf("HTTP %d: %s", resp.StatusCode, string(body))
	}

	switch {
	case resp.StatusCode == http.StatusTooManyRequests || code == "rate_limit_exceeded":
		agentErr := a.retryableErr(fmt.Errorf("rate limited: %s", msg))
		if ra := resp.Header.Get("Retry-After"); ra != "" {
			if secs, err := strconv.Atoi(ra); err == nil {
				agentErr.RetryAfter = time.Duration(secs) * time.Second
			}
		}
		return agentErr

	case code == "context_length_exceeded":
		return a.nonRetryableErr(fmt.Errorf("context length exceeded: %s", msg))

	case resp.StatusCode == http.StatusNotFound || code == "model_not_found":
		return a.nonRetryableErr(fmt.Errorf(
			"model %q not found or not accessible via the Responses API — verify the model name and that your API key has access",
			a.cfg.Model,
		))

	case resp.StatusCode >= 500:
		return a.retryableErr(fmt.Errorf("server error %d: %s", resp.StatusCode, msg))

	default:
		return a.nonRetryableErr(fmt.Errorf("unexpected status %d: %s", resp.StatusCode, msg))
	}
}

const healthCheckTimeout = 5 * time.Second

// HealthCheck calls GET /v1/models/<model> and expects a 200. This verifies
// both that the API key is valid and that the configured model is
// accessible via this endpoint.
func (a *Adapter) HealthCheck(ctx context.Context) error {
	apiKey, err := a.resolveAPIKey()
	if err != nil {
		return fmt.Errorf("openai_responses health check: %w", err)
	}
	if a.cfg.Model == "" {
		return fmt.Errorf("openai_responses health check: model is not configured")
	}

	ctx, cancel := context.WithTimeout(ctx, healthCheckTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.cfg.BaseURL+"/v1/models/"+url.PathEscape(a.cfg.Model), nil)
	if err != nil {
		return fmt.Errorf("openai_responses health check: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("openai_responses health check: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf(
			"openai_responses health check: model %q not accessible — verify the model name and that your API key has access",
			a.cfg.Model,
		)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("openai_responses health check: unexpected status %d", resp.StatusCode)
	}
	return nil
}

func (a *Adapter) retryableErr(err error) *agent.AgentError {
	return &agent.AgentError{
		Err:       err,
		AgentID:   a.cfg.ID,
		Prov:      providerName,
		Retryable: true,
	}
}

func (a *Adapter) nonRetryableErr(err error) *agent.AgentError {
	return &agent.AgentError{
		Err:       err,
		AgentID:   a.cfg.ID,
		Prov:      providerName,
		Retryable: false,
	}
}
