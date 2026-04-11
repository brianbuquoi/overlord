// Package ollama implements agent.Agent for the Ollama REST API.
package ollama

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
	"strings"
	"time"

	"github.com/brianbuquoi/orcastrator/internal/agent"
	"github.com/brianbuquoi/orcastrator/internal/broker"
	"github.com/brianbuquoi/orcastrator/internal/metrics"
)

const (
	defaultEndpoint = "http://localhost:11434"
	defaultTimeout  = 120 * time.Second
	providerName    = "ollama"
)

// Config holds the adapter configuration derived from the YAML agent block.
type Config struct {
	ID           string
	Model        string
	SystemPrompt string
	Temperature  float64
	MaxTokens    int
	Timeout      time.Duration
	Endpoint     string // override for testing or OLLAMA_ENDPOINT
}

// Adapter implements agent.Agent for the Ollama REST API.
type Adapter struct {
	cfg     Config
	client  *http.Client
	logger  *slog.Logger
	metrics *metrics.Metrics
}

// New creates a new Ollama adapter.
// Endpoint precedence: Config.Endpoint > OLLAMA_ENDPOINT env var > default (localhost:11434).
func New(cfg Config, logger *slog.Logger, m ...*metrics.Metrics) (*Adapter, error) {
	if cfg.Endpoint == "" {
		cfg.Endpoint = os.Getenv("OLLAMA_ENDPOINT")
	}
	if cfg.Endpoint == "" {
		cfg.Endpoint = defaultEndpoint
	}
	cfg.Endpoint = strings.TrimRight(cfg.Endpoint, "/")
	if err := validateEndpoint(cfg.Endpoint); err != nil {
		return nil, err
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = defaultTimeout
	}
	if cfg.Model == "" {
		return nil, fmt.Errorf("ollama: model is required")
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

// validateEndpoint enforces that HTTP is only allowed for localhost endpoints.
// Remote Ollama instances must use HTTPS. While Ollama itself doesn't use API
// keys, prompts and model output should not transit the network in plaintext.
func validateEndpoint(endpoint string) error {
	u, err := url.Parse(endpoint)
	if err != nil {
		return fmt.Errorf("ollama: invalid endpoint URL: %w", err)
	}
	if u.Scheme == "https" {
		return nil
	}
	host := u.Hostname()
	if host == "localhost" || host == "127.0.0.1" || host == "::1" {
		return nil
	}
	return fmt.Errorf("ollama: HTTP is only allowed for localhost (got %s); use HTTPS for remote endpoints to protect prompt data in transit", endpoint)
}

func (a *Adapter) ID() string       { return a.cfg.ID }
func (a *Adapter) Provider() string { return providerName }

// --- Ollama /api/chat request/response types (internal only) ---

type chatRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
	Stream   bool          `json:"stream"`
	Options  *chatOptions  `json:"options,omitempty"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatOptions struct {
	Temperature *float64 `json:"temperature,omitempty"`
	NumPredict  int      `json:"num_predict,omitempty"`
}

type chatResponse struct {
	Message         chatMessage `json:"message"`
	PromptEvalCount int         `json:"prompt_eval_count"`
	EvalCount       int         `json:"eval_count"`
}

type tagsResponse struct {
	Models []tagModel `json:"models"`
}

type tagModel struct {
	Name string `json:"name"`
}

// Execute sends the task payload to the Ollama /api/chat endpoint and returns the result.
func (a *Adapter) Execute(ctx context.Context, task *broker.Task) (*broker.TaskResult, error) {
	start := time.Now()

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
		Model:    a.cfg.Model,
		Messages: messages,
		Stream:   false,
	}

	var opts chatOptions
	hasOpts := false
	if a.cfg.Temperature > 0 {
		t := a.cfg.Temperature
		opts.Temperature = &t
		hasOpts = true
	}
	if a.cfg.MaxTokens > 0 {
		opts.NumPredict = a.cfg.MaxTokens
		hasOpts = true
	}
	if hasOpts {
		reqBody.Options = &opts
	}

	var sysText, userText string
	for _, m := range messages {
		if m.Role == "system" && sysText == "" {
			sysText = m.Content
		}
		if m.Role == "user" && userText == "" {
			userText = m.Content
		}
	}
	a.logger.Debug("ollama request body",
		"agent_id", a.cfg.ID,
		"system_prompt_length", len(sysText),
		"user_message_length", len(userText),
		"system_prompt_preview", agent.Truncate(sysText, 200),
		"user_message_preview", agent.Truncate(userText, 200),
	)

	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, a.nonRetryableErr(fmt.Errorf("marshal request: %w", err))
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.cfg.Endpoint+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return nil, a.nonRetryableErr(fmt.Errorf("build request: %w", err))
	}
	req.Header.Set("Content-Type", "application/json")

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
		return nil, a.handleErrorResponse(resp.StatusCode, respBody)
	}

	var result chatResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, a.nonRetryableErr(fmt.Errorf("unmarshal response: %w", err))
	}

	output := result.Message.Content
	if output == "" {
		return nil, a.retryableErr(fmt.Errorf("empty content in response"))
	}

	a.logger.Debug("ollama raw response",
		"agent_id", a.cfg.ID,
		"raw_length", len(output),
		"raw_preview", agent.Truncate(output, 200),
	)

	a.logger.Info("ollama request completed",
		"agent_id", a.cfg.ID,
		"model", a.cfg.Model,
		"input_tokens", result.PromptEvalCount,
		"output_tokens", result.EvalCount,
		"latency_ms", latencyMs,
	)

	if a.metrics != nil {
		dur := time.Since(start).Seconds()
		a.metrics.AgentRequestDuration.WithLabelValues(providerName, a.cfg.Model).Observe(dur)
		a.metrics.AgentTokensTotal.WithLabelValues(providerName, a.cfg.Model, "input").Add(float64(result.PromptEvalCount))
		a.metrics.AgentTokensTotal.WithLabelValues(providerName, a.cfg.Model, "output").Add(float64(result.EvalCount))
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
			"input_tokens":  result.PromptEvalCount,
			"output_tokens": result.EvalCount,
			"latency_ms":    latencyMs,
		},
	}, nil
}

func (a *Adapter) handleErrorResponse(statusCode int, body []byte) *agent.AgentError {
	msg := strings.TrimSpace(string(body))
	if msg == "" {
		msg = fmt.Sprintf("HTTP %d", statusCode)
	}

	switch {
	case statusCode == http.StatusNotFound:
		return a.nonRetryableErr(fmt.Errorf("model not found: %s", msg))

	case statusCode >= 500:
		return a.retryableErr(fmt.Errorf("server error %d: %s", statusCode, msg))

	default:
		return a.nonRetryableErr(fmt.Errorf("unexpected status %d: %s", statusCode, msg))
	}
}

const healthCheckTimeout = 5 * time.Second

// HealthCheck calls GET /api/tags and verifies the configured model is available locally.
func (a *Adapter) HealthCheck(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, healthCheckTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.cfg.Endpoint+"/api/tags", nil)
	if err != nil {
		return fmt.Errorf("ollama health check: %w", err)
	}

	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("ollama health check: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ollama health check: unexpected status %d", resp.StatusCode)
	}

	var tags tagsResponse
	if err := json.NewDecoder(resp.Body).Decode(&tags); err != nil {
		return fmt.Errorf("ollama health check: failed to parse response: %w", err)
	}

	for _, m := range tags.Models {
		// Ollama model names may include a tag suffix (e.g. "llama3:latest").
		// Match if the name equals the configured model or starts with it followed by ":".
		if m.Name == a.cfg.Model || strings.HasPrefix(m.Name, a.cfg.Model+":") {
			return nil
		}
	}

	return fmt.Errorf("ollama health check: model %s not found locally — run: ollama pull %s", a.cfg.Model, a.cfg.Model)
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
