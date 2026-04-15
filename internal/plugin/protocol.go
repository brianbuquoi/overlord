package plugin

import "encoding/json"

// JSON-RPC 2.0 method names used by the subprocess plugin protocol.
const (
	MethodExecute     = "execute"
	MethodHealthCheck = "health_check"
)

// JSON-RPC 2.0 error codes. We reuse the standard code space rather than
// inventing new ones so off-the-shelf JSON-RPC tooling can recognise failures.
const (
	RPCErrorInvalidParams = -32602
	RPCErrorInternal      = -32603
)

// RPCRequest is a JSON-RPC 2.0 request frame.
type RPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// RPCResponse is a JSON-RPC 2.0 response frame. Exactly one of Result / Error
// is populated.
type RPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// RPCError is the error object in a JSON-RPC 2.0 response.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// ExecuteParams is the params payload for the "execute" method.
type ExecuteParams struct {
	TaskID       string          `json:"task_id"`
	PipelineID   string          `json:"pipeline_id"`
	StageID      string          `json:"stage_id"`
	Payload      json.RawMessage `json:"payload"`
	SystemPrompt string          `json:"system_prompt,omitempty"`
}

// ExecuteResult is the result payload for the "execute" method.
type ExecuteResult struct {
	Output   json.RawMessage   `json:"output"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

// HealthCheckResult is the result payload for the "health_check" method.
type HealthCheckResult struct {
	Healthy bool   `json:"healthy"`
	Message string `json:"message,omitempty"`
}
