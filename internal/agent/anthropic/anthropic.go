// Package anthropic implements agent.Agent for the Anthropic Messages API.
package anthropic

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
	defaultBaseURL = "https://api.anthropic.com"
	apiVersion     = "2023-06-01"
	defaultTimeout = 120 * time.Second
	providerName   = "anthropic"
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

// Adapter implements agent.Agent for Anthropic's Messages API.
type Adapter struct {
	cfg     Config
	client  *http.Client
	apiKey  string
	logger  *slog.Logger
	metrics *metrics.Metrics
}

// New creates a new Anthropic adapter.
// API key validation is deferred to Execute/HealthCheck so that adapters can be
// constructed without credentials (e.g. during config validation or testing).
func New(cfg Config, logger *slog.Logger, m ...*metrics.Metrics) (*Adapter, error) {
	if cfg.APIKeyEnv == "" {
		cfg.APIKeyEnv = "ANTHROPIC_API_KEY"
	}
	if cfg.BaseURL == "" {
		if envURL := os.Getenv("ANTHROPIC_BASE_URL"); envURL != "" {
			cfg.BaseURL = envURL
		} else {
			cfg.BaseURL = defaultBaseURL
		}
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

// resolveAPIKey returns the API key, reading from the environment on first use.
func (a *Adapter) resolveAPIKey() (string, error) {
	if a.apiKey != "" {
		return a.apiKey, nil
	}
	key := os.Getenv(a.cfg.APIKeyEnv)
	if key == "" {
		return "", fmt.Errorf("anthropic: env var %s is not set", a.cfg.APIKeyEnv)
	}
	a.apiKey = key
	return key, nil
}

func (a *Adapter) ID() string       { return a.cfg.ID }
func (a *Adapter) Provider() string { return providerName }

// messagesRequest is the body sent to POST /v1/messages.
type messagesRequest struct {
	Model       string    `json:"model"`
	MaxTokens   int       `json:"max_tokens"`
	System      string    `json:"system,omitempty"`
	Messages    []message `json:"messages"`
	Temperature *float64  `json:"temperature,omitempty"`
}

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// messagesResponse is the response from POST /v1/messages.
type messagesResponse struct {
	ID      string         `json:"id"`
	Content []contentBlock `json:"content"`
	Usage   usage          `json:"usage"`
	Error   *apiError      `json:"error,omitempty"`
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

type apiError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

type errorResponse struct {
	Error apiError `json:"error"`
}

// Execute sends the task payload to the Anthropic Messages API and returns the result.
func (a *Adapter) Execute(ctx context.Context, task *broker.Task) (*broker.TaskResult, error) {
	apiKey, err := a.resolveAPIKey()
	if err != nil {
		return nil, a.nonRetryableErr(err)
	}

	start := time.Now()

	// Build the request body. The payload arrives already envelope-wrapped by the broker.
	var temp *float64
	if a.cfg.Temperature > 0 {
		t := a.cfg.Temperature
		temp = &t
	}
	reqBody := messagesRequest{
		Model:       a.cfg.Model,
		MaxTokens:   a.cfg.MaxTokens,
		System:      a.cfg.SystemPrompt,
		Temperature: temp,
		Messages: []message{
			{Role: "user", Content: string(task.Payload)},
		},
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, a.nonRetryableErr(fmt.Errorf("marshal request: %w", err))
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.cfg.BaseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return nil, a.nonRetryableErr(fmt.Errorf("build request: %w", err))
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", apiKey)
	req.Header.Set("Anthropic-Version", apiVersion)

	resp, err := a.client.Do(req)
	if err != nil {
		// Timeouts and network errors are retryable.
		return nil, a.retryableErr(fmt.Errorf("http request: %w", err))
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, a.retryableErr(fmt.Errorf("read response: %w", err))
	}

	latencyMs := time.Since(start).Milliseconds()

	// Handle error status codes before parsing.
	if resp.StatusCode != http.StatusOK {
		return nil, a.handleErrorResponse(resp, respBody)
	}

	var result messagesResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, a.nonRetryableErr(fmt.Errorf("unmarshal response: %w", err))
	}

	// Extract text from content blocks.
	if len(result.Content) == 0 {
		return nil, a.nonRetryableErr(fmt.Errorf("empty content in response"))
	}
	var output strings.Builder
	for _, block := range result.Content {
		if block.Type == "text" {
			output.WriteString(block.Text)
		}
	}
	if output.Len() == 0 {
		return nil, a.nonRetryableErr(fmt.Errorf("no text content in response (unsupported content type)"))
	}

	a.logger.Info("anthropic request completed",
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

	payload, parseErr := agent.ParseJSONObjectOutput(output.String())
	if parseErr != nil {
		return nil, a.nonRetryableErr(parseErr)
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

func (a *Adapter) handleErrorResponse(resp *http.Response, body []byte) *agent.AgentError {
	var errResp errorResponse
	_ = json.Unmarshal(body, &errResp) // best-effort parse

	msg := errResp.Error.Message
	if msg == "" {
		msg = fmt.Sprintf("HTTP %d: %s", resp.StatusCode, string(body))
	}

	switch resp.StatusCode {
	case http.StatusTooManyRequests:
		agentErr := a.retryableErr(fmt.Errorf("rate limited: %s", msg))
		if ra := resp.Header.Get("Retry-After"); ra != "" {
			if secs, err := strconv.Atoi(ra); err == nil {
				agentErr.RetryAfter = time.Duration(secs) * time.Second
			}
		}
		return agentErr

	case http.StatusBadRequest:
		if errResp.Error.Type == "invalid_request_error" && strings.Contains(msg, "token") {
			return a.nonRetryableErr(fmt.Errorf("context window exceeded: %s", msg))
		}
		return a.nonRetryableErr(fmt.Errorf("bad request: %s", msg))

	case http.StatusUnauthorized:
		return a.nonRetryableErr(fmt.Errorf("authentication error: %s", msg))

	case http.StatusForbidden:
		return a.nonRetryableErr(fmt.Errorf("permission denied: %s", msg))

	case 529: // Anthropic overloaded
		return a.retryableErr(fmt.Errorf("overloaded: %s", msg))

	case http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
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
		return fmt.Errorf("anthropic health check: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, healthCheckTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.cfg.BaseURL+"/v1/models", nil)
	if err != nil {
		return fmt.Errorf("anthropic health check: %w", err)
	}
	req.Header.Set("X-API-Key", apiKey)
	req.Header.Set("Anthropic-Version", apiVersion)

	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("anthropic health check: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("anthropic health check: unexpected status %d", resp.StatusCode)
	}
	return nil
}

// requireTLS rejects non-HTTPS base URLs for cloud providers. API keys must
// not be sent over plaintext HTTP. Test servers (httptest) use http:// with
// 127.0.0.1 or localhost, so those are allowed for testing.
func requireTLS(baseURL string) error {
	u, err := url.Parse(baseURL)
	if err != nil {
		return fmt.Errorf("anthropic: invalid base URL: %w", err)
	}
	if u.Scheme == "https" {
		return nil
	}
	host := u.Hostname()
	if host == "localhost" || host == "127.0.0.1" || host == "::1" {
		return nil // allow http for test servers
	}
	return fmt.Errorf("anthropic: base URL must use HTTPS (got %s); API keys must not be sent over plaintext HTTP", baseURL)
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
