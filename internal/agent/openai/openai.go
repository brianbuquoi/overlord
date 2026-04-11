// Package openai implements agent.Agent for the OpenAI Chat Completions API.
package openai

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

	"github.com/brianbuquoi/orcastrator/internal/agent"
	"github.com/brianbuquoi/orcastrator/internal/broker"
	"github.com/brianbuquoi/orcastrator/internal/metrics"
)

const (
	defaultBaseURL = "https://api.openai.com"
	defaultTimeout = 120 * time.Second
	providerName   = "openai"
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
	BaseURL      string // configurable for OpenAI-compatible endpoints
}

// Adapter implements agent.Agent for the OpenAI Chat Completions API.
type Adapter struct {
	cfg     Config
	client  *http.Client
	apiKey  string
	logger  *slog.Logger
	metrics *metrics.Metrics
}

// New creates a new OpenAI adapter.
// API key validation is deferred to Execute/HealthCheck so that adapters can be
// constructed without credentials (e.g. during config validation or testing).
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
	if cfg.MaxTokens == 0 {
		cfg.MaxTokens = 4096
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

// requireTLS rejects non-HTTPS base URLs for cloud providers. Allows localhost
// for test servers.
func requireTLS(baseURL string) error {
	u, err := url.Parse(baseURL)
	if err != nil {
		return fmt.Errorf("openai: invalid base URL: %w", err)
	}
	if u.Scheme == "https" {
		return nil
	}
	host := u.Hostname()
	if host == "localhost" || host == "127.0.0.1" || host == "::1" {
		return nil
	}
	return fmt.Errorf("openai: base URL must use HTTPS (got %s); API keys must not be sent over plaintext HTTP", baseURL)
}

// resolveAPIKey returns the API key, reading from the environment on first use.
func (a *Adapter) resolveAPIKey() (string, error) {
	if a.apiKey != "" {
		return a.apiKey, nil
	}
	key := os.Getenv(a.cfg.APIKeyEnv)
	if key == "" {
		return "", fmt.Errorf("openai: env var %s is not set", a.cfg.APIKeyEnv)
	}
	a.apiKey = key
	return key, nil
}

func (a *Adapter) ID() string       { return a.cfg.ID }
func (a *Adapter) Provider() string { return providerName }

// chatRequest is the body sent to POST /v1/chat/completions.
type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	MaxTokens   int           `json:"max_tokens,omitempty"`
	Temperature *float64      `json:"temperature,omitempty"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// chatResponse is the response from POST /v1/chat/completions.
type chatResponse struct {
	ID      string   `json:"id"`
	Choices []choice `json:"choices"`
	Usage   usage    `json:"usage"`
}

type choice struct {
	Message chatMessage `json:"message"`
}

type usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

type apiError struct {
	Error apiErrorDetail `json:"error"`
}

type apiErrorDetail struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
}

// Execute sends the task payload to the OpenAI Chat Completions API and returns the result.
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

	messages := []chatMessage{}
	if systemPrompt != "" {
		messages = append(messages, chatMessage{Role: "system", Content: systemPrompt})
	}
	messages = append(messages, chatMessage{Role: "user", Content: string(task.Payload)})

	reqBody := chatRequest{
		Model:       a.cfg.Model,
		Messages:    messages,
		MaxTokens:   a.cfg.MaxTokens,
		Temperature: temp,
	}

	var systemPreview, userPreview string
	var systemLen, userLen int
	for _, m := range messages {
		if m.Role == "system" && systemPreview == "" {
			systemPreview = agent.Truncate(m.Content, 200)
			systemLen = len(m.Content)
		}
		if m.Role == "user" && userPreview == "" {
			userPreview = agent.Truncate(m.Content, 200)
			userLen = len(m.Content)
		}
	}
	a.logger.Debug("openai request body",
		"agent_id", a.cfg.ID,
		"system_prompt_length", systemLen,
		"user_message_length", userLen,
		"system_prompt_preview", systemPreview,
		"user_message_preview", userPreview,
	)

	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, a.nonRetryableErr(fmt.Errorf("marshal request: %w", err))
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.cfg.BaseURL+"/v1/chat/completions", bytes.NewReader(body))
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

	var result chatResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, a.nonRetryableErr(fmt.Errorf("unmarshal response: %w", err))
	}

	if len(result.Choices) == 0 {
		return nil, a.nonRetryableErr(fmt.Errorf("empty choices in response"))
	}

	output := result.Choices[0].Message.Content

	a.logger.Debug("openai raw response",
		"agent_id", a.cfg.ID,
		"raw_length", len(output),
		"raw_preview", agent.Truncate(output, 200),
	)

	a.logger.Info("openai request completed",
		"agent_id", a.cfg.ID,
		"model", a.cfg.Model,
		"input_tokens", result.Usage.PromptTokens,
		"output_tokens", result.Usage.CompletionTokens,
		"latency_ms", latencyMs,
	)

	if a.metrics != nil {
		dur := time.Since(start).Seconds()
		a.metrics.AgentRequestDuration.WithLabelValues(providerName, a.cfg.Model).Observe(dur)
		a.metrics.AgentTokensTotal.WithLabelValues(providerName, a.cfg.Model, "input").Add(float64(result.Usage.PromptTokens))
		a.metrics.AgentTokensTotal.WithLabelValues(providerName, a.cfg.Model, "output").Add(float64(result.Usage.CompletionTokens))
	}

	payload, parseErr := agent.ParseJSONObjectOutput(output)
	if parseErr != nil {
		return nil, a.retryableErr(parseErr)
	}

	return &broker.TaskResult{
		TaskID:  task.ID,
		Payload: payload,
		Metadata: map[string]any{
			"provider":      providerName,
			"model":         a.cfg.Model,
			"input_tokens":  result.Usage.PromptTokens,
			"output_tokens": result.Usage.CompletionTokens,
			"latency_ms":    latencyMs,
		},
	}, nil
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

	case resp.StatusCode >= 500:
		return a.retryableErr(fmt.Errorf("server error %d: %s", resp.StatusCode, msg))

	default:
		return a.nonRetryableErr(fmt.Errorf("unexpected status %d: %s", resp.StatusCode, msg))
	}
}

const healthCheckTimeout = 5 * time.Second

// HealthCheck calls GET /v1/models and expects a 200.
func (a *Adapter) HealthCheck(ctx context.Context) error {
	apiKey, err := a.resolveAPIKey()
	if err != nil {
		return fmt.Errorf("openai health check: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, healthCheckTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.cfg.BaseURL+"/v1/models", nil)
	if err != nil {
		return fmt.Errorf("openai health check: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("openai health check: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("openai health check: unexpected status %d", resp.StatusCode)
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
