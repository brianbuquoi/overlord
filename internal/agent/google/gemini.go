// Package google implements agent.Agent for the Google Gemini API.
package google

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

	"github.com/brianbuquoi/overlord/internal/agent"
	"github.com/brianbuquoi/overlord/internal/broker"
	"github.com/brianbuquoi/overlord/internal/metrics"
)

const (
	defaultBaseURL = "https://generativelanguage.googleapis.com"
	defaultTimeout = 120 * time.Second
	providerName   = "google"
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
	BaseURL      string // override for testing
}

// Adapter implements agent.Agent for the Google Gemini API.
type Adapter struct {
	cfg     Config
	client  *http.Client
	apiKey  string
	logger  *slog.Logger
	metrics *metrics.Metrics
}

// New creates a new Gemini adapter.
// API key validation is deferred to Execute/HealthCheck so that adapters can be
// constructed without credentials (e.g. during config validation or testing).
func New(cfg Config, logger *slog.Logger, m ...*metrics.Metrics) (*Adapter, error) {
	if cfg.APIKeyEnv == "" {
		cfg.APIKeyEnv = "GEMINI_API_KEY"
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
		return fmt.Errorf("google: invalid base URL: %w", err)
	}
	if u.Scheme == "https" {
		return nil
	}
	host := u.Hostname()
	if host == "localhost" || host == "127.0.0.1" || host == "::1" {
		return nil
	}
	return fmt.Errorf("google: base URL must use HTTPS (got %s); API keys must not be sent over plaintext HTTP", baseURL)
}

// resolveAPIKey returns the API key, reading from the environment on first use.
func (a *Adapter) resolveAPIKey() (string, error) {
	if a.apiKey != "" {
		return a.apiKey, nil
	}
	key := os.Getenv(a.cfg.APIKeyEnv)
	if key == "" {
		return "", fmt.Errorf("google: env var %s is not set", a.cfg.APIKeyEnv)
	}
	a.apiKey = key
	return key, nil
}

func (a *Adapter) ID() string       { return a.cfg.ID }
func (a *Adapter) Provider() string { return providerName }

// --- Gemini API request/response types (internal only) ---

type generateRequest struct {
	Contents          []content         `json:"contents"`
	SystemInstruction *content          `json:"systemInstruction,omitempty"`
	GenerationConfig  *generationConfig `json:"generationConfig,omitempty"`
}

type content struct {
	Role  string `json:"role,omitempty"`
	Parts []part `json:"parts"`
}

type part struct {
	Text string `json:"text"`
}

type generationConfig struct {
	Temperature     *float64 `json:"temperature,omitempty"`
	MaxOutputTokens int      `json:"maxOutputTokens,omitempty"`
}

type generateResponse struct {
	Candidates     []candidate     `json:"candidates"`
	UsageMetadata  *usageMetadata  `json:"usageMetadata,omitempty"`
	PromptFeedback *promptFeedback `json:"promptFeedback,omitempty"`
}

type candidate struct {
	Content      content `json:"content"`
	FinishReason string  `json:"finishReason,omitempty"`
}

type promptFeedback struct {
	BlockReason string `json:"blockReason,omitempty"`
}

type usageMetadata struct {
	PromptTokenCount     int `json:"promptTokenCount"`
	CandidatesTokenCount int `json:"candidatesTokenCount"`
}

type geminiError struct {
	Error geminiErrorDetail `json:"error"`
}

type geminiErrorDetail struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Status  string `json:"status"`
}

// Execute sends the task payload to the Gemini generateContent endpoint and returns the result.
func (a *Adapter) Execute(ctx context.Context, task *broker.Task) (*broker.TaskResult, error) {
	apiKey, err := a.resolveAPIKey()
	if err != nil {
		return nil, a.nonRetryableErr(err)
	}

	start := time.Now()

	systemPrompt := task.Prompt
	if systemPrompt == "" {
		systemPrompt = a.cfg.SystemPrompt
	}

	reqBody := generateRequest{
		Contents: []content{
			{Role: "user", Parts: []part{{Text: string(task.Payload)}}},
		},
	}
	if systemPrompt != "" {
		reqBody.SystemInstruction = &content{
			Parts: []part{{Text: systemPrompt}},
		}
	}

	genCfg := &generationConfig{MaxOutputTokens: a.cfg.MaxTokens}
	if a.cfg.Temperature > 0 {
		t := a.cfg.Temperature
		genCfg.Temperature = &t
	}
	reqBody.GenerationConfig = genCfg

	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, a.nonRetryableErr(fmt.Errorf("marshal request: %w", err))
	}

	url := fmt.Sprintf("%s/v1beta/models/%s:generateContent", a.cfg.BaseURL, a.cfg.Model)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, a.nonRetryableErr(fmt.Errorf("build request: %w", err))
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Goog-Api-Key", apiKey)

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

	var result generateResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, a.nonRetryableErr(fmt.Errorf("unmarshal response: %w", err))
	}

	if len(result.Candidates) == 0 {
		if result.PromptFeedback != nil && result.PromptFeedback.BlockReason != "" {
			return nil, a.nonRetryableErr(fmt.Errorf("response blocked by safety filter: %s", result.PromptFeedback.BlockReason))
		}
		return nil, a.nonRetryableErr(fmt.Errorf("empty candidates in response"))
	}

	if result.Candidates[0].FinishReason == "SAFETY" {
		return nil, a.nonRetryableErr(fmt.Errorf("response blocked by safety filter: candidate finish reason is SAFETY"))
	}

	var output strings.Builder
	for _, p := range result.Candidates[0].Content.Parts {
		output.WriteString(p.Text)
	}
	if output.Len() == 0 {
		return nil, a.retryableErr(fmt.Errorf("no text content in response"))
	}

	textContent := output.String()

	var inputTokens, outputTokens int
	if result.UsageMetadata != nil {
		inputTokens = result.UsageMetadata.PromptTokenCount
		outputTokens = result.UsageMetadata.CandidatesTokenCount
	}

	a.logger.Info("gemini request completed",
		"agent_id", a.cfg.ID,
		"model", a.cfg.Model,
		"input_tokens", inputTokens,
		"output_tokens", outputTokens,
		"latency_ms", latencyMs,
	)

	if a.metrics != nil {
		dur := time.Since(start).Seconds()
		a.metrics.AgentRequestDuration.WithLabelValues(providerName, a.cfg.Model).Observe(dur)
		a.metrics.AgentTokensTotal.WithLabelValues(providerName, a.cfg.Model, "input").Add(float64(inputTokens))
		a.metrics.AgentTokensTotal.WithLabelValues(providerName, a.cfg.Model, "output").Add(float64(outputTokens))
	}

	payload, parseErr := agent.ParseJSONObjectOutput(textContent)
	if parseErr != nil {
		return nil, a.retryableErr(parseErr)
	}

	return &broker.TaskResult{
		TaskID:  task.ID,
		Payload: payload,
		Metadata: map[string]any{
			"provider":      providerName,
			"model":         a.cfg.Model,
			"input_tokens":  inputTokens,
			"output_tokens": outputTokens,
			"latency_ms":    latencyMs,
		},
	}, nil
}

func (a *Adapter) handleErrorResponse(statusCode int, body []byte) *agent.AgentError {
	var errResp geminiError
	_ = json.Unmarshal(body, &errResp)

	msg := errResp.Error.Message
	status := errResp.Error.Status
	if msg == "" {
		msg = fmt.Sprintf("HTTP %d: %s", statusCode, string(body))
	}

	switch {
	case status == "RESOURCE_EXHAUSTED" || statusCode == http.StatusTooManyRequests:
		return a.retryableErr(fmt.Errorf("rate limited: %s", msg))

	case status == "INVALID_ARGUMENT" || statusCode == http.StatusBadRequest:
		return a.nonRetryableErr(fmt.Errorf("invalid argument: %s", msg))

	case statusCode == http.StatusUnauthorized || statusCode == http.StatusForbidden:
		return a.nonRetryableErr(fmt.Errorf("authentication error: %s", msg))

	case statusCode >= 500:
		return a.retryableErr(fmt.Errorf("server error %d: %s", statusCode, msg))

	default:
		return a.nonRetryableErr(fmt.Errorf("unexpected status %d: %s", statusCode, msg))
	}
}

const healthCheckTimeout = 5 * time.Second

// HealthCheck calls the GET model info endpoint for the configured model.
func (a *Adapter) HealthCheck(ctx context.Context) error {
	apiKey, err := a.resolveAPIKey()
	if err != nil {
		return fmt.Errorf("gemini health check: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, healthCheckTimeout)
	defer cancel()

	url := fmt.Sprintf("%s/v1beta/models/%s", a.cfg.BaseURL, a.cfg.Model)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("gemini health check: %w", err)
	}
	req.Header.Set("X-Goog-Api-Key", apiKey)

	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("gemini health check: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("gemini health check: unexpected status %d", resp.StatusCode)
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
