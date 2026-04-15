package plugin

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/brianbuquoi/overlord/internal/agent"
	"github.com/brianbuquoi/overlord/internal/broker"
)

// Default RPC timeout when the manifest does not specify one.
const defaultRPCTimeout = 30 * time.Second

// maxPluginLineSize bounds the bufio.Scanner line buffer for reading plugin
// stdout JSON responses. Large enough for typical JSON payloads but finite so
// a runaway plugin cannot exhaust memory.
const maxPluginLineSize = 10 * 1024 * 1024 // 10 MB

// Agent is a subprocess-based plugin agent. It implements agent.Agent by
// spawning a child process declared in the plugin manifest and exchanging
// newline-delimited JSON-RPC 2.0 frames over its stdin/stdout.
//
// Agents are created via New (which does NOT start the subprocess) and then
// lazily started on the first Execute or HealthCheck call. Subsequent calls
// reuse the same subprocess. If the subprocess dies, the next call triggers
// a single restart attempt subject to Manifest.MaxRestarts.
type Agent struct {
	id           string
	systemPrompt string
	manifest     *Manifest
	logger       *slog.Logger

	// mu serializes all JSON-RPC calls to the subprocess. Plugin subprocesses
	// process exactly one request at a time. For high-throughput pipeline
	// stages, deploy multiple plugin agent instances (separate agent IDs
	// with the same manifest) and distribute load across them via stage
	// configuration. See docs/plugin-security.md for capacity planning
	// guidance.
	mu     sync.Mutex
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	lines  chan scanLine // responses from stdout reader goroutine
	exited chan struct{} // closed when Wait() returns

	nextID       int64
	restartCount int
	started      bool
	unhealthy    bool // set when restart budget exhausted
}

// scanLine carries a single stdout line (or error) from the reader goroutine.
type scanLine struct {
	data []byte
	err  error
}

// New creates a new subprocess Agent. It validates the manifest but does NOT
// start the subprocess — that happens lazily on the first call.
func New(agentID string, systemPrompt string, manifest *Manifest, logger *slog.Logger) (*Agent, error) {
	if manifest == nil {
		return nil, fmt.Errorf("plugin agent %q: manifest is required", agentID)
	}
	if err := manifest.Validate(); err != nil {
		return nil, fmt.Errorf("plugin agent %q: %w", agentID, err)
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Agent{
		id:           agentID,
		systemPrompt: systemPrompt,
		manifest:     manifest,
		logger:       logger.With("agent_id", agentID, "provider", "plugin", "plugin_name", manifest.Name),
	}, nil
}

// ID returns the agent ID.
func (a *Agent) ID() string { return a.id }

// Provider returns the constant "plugin".
func (a *Agent) Provider() string { return "plugin" }

// start launches the subprocess. Must be called with a.mu held.
func (a *Agent) start() error {
	if a.started {
		return nil
	}
	if a.unhealthy {
		return &agent.AgentError{
			Err:       fmt.Errorf("plugin subprocess is unhealthy (restart budget exhausted)"),
			AgentID:   a.id,
			Prov:      "plugin",
			Retryable: false,
		}
	}

	binPath := a.manifest.ResolveBinary()
	cmd := exec.Command(binPath, a.manifest.Args...) //nolint:gosec // path comes from validated manifest

	// Build an allow-listed environment. The subprocess inherits ONLY the
	// env vars explicitly named in manifest.Env — nothing else.
	env := make([]string, 0, len(a.manifest.Env))
	for _, name := range a.manifest.Env {
		if v, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+v)
		}
	}
	cmd.Env = env

	// Working directory defaults to the manifest directory so a plugin's
	// relative data files resolve predictably.
	if dir := a.manifest.ManifestDir(); dir != "" {
		cmd.Dir = dir
	}

	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("plugin %q: stdin pipe: %w", a.id, err)
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdinPipe.Close()
		return fmt.Errorf("plugin %q: stdout pipe: %w", a.id, err)
	}
	// Stderr → Overlord's stderr with a clear prefix so plugin diagnostics
	// land in operator logs alongside everything else.
	cmd.Stderr = &prefixedWriter{prefix: fmt.Sprintf("[plugin:%s] ", a.id), w: os.Stderr}

	if err := cmd.Start(); err != nil {
		_ = stdinPipe.Close()
		_ = stdoutPipe.Close()
		return fmt.Errorf("plugin %q: start %q: %w", a.id, binPath, err)
	}

	// Best-effort syscall restriction on Linux. Non-fatal; the function
	// itself logs its status.
	if err := applySeccompProfile(cmd.Process.Pid); err != nil {
		a.logger.Warn("seccomp profile application failed; continuing without syscall restriction", "err", err)
	}

	a.cmd = cmd
	a.stdin = stdinPipe
	a.lines = make(chan scanLine, 16)
	a.exited = make(chan struct{})
	a.started = true

	// Reader goroutine: feeds stdout lines into a channel so callRPC can
	// select between lines, ctx cancellation, and RPC timeout.
	go func(lines chan<- scanLine, r io.ReadCloser) {
		scanner := bufio.NewScanner(r)
		scanner.Buffer(make([]byte, 64*1024), maxPluginLineSize)
		for scanner.Scan() {
			// Copy bytes: Scanner reuses its internal buffer.
			b := append([]byte(nil), scanner.Bytes()...)
			lines <- scanLine{data: b}
		}
		if err := scanner.Err(); err != nil {
			lines <- scanLine{err: err}
		}
		close(lines)
	}(a.lines, stdoutPipe)

	// Wait goroutine: marks the process as exited when the subprocess
	// terminates for any reason.
	go func(exited chan struct{}) {
		_ = cmd.Wait() // error is surfaced via stderr and next RPC failure
		close(exited)
	}(a.exited)

	a.logger.Info("plugin subprocess started", "binary", binPath, "pid", cmd.Process.Pid)
	return nil
}

// restart tears down the subprocess state and calls start() again. Must be
// called with a.mu held.
func (a *Agent) restart() error {
	if a.manifest.MaxRestarts > 0 && a.restartCount >= a.manifest.MaxRestarts {
		a.unhealthy = true
		return &agent.AgentError{
			Err:       fmt.Errorf("plugin restart budget exhausted (%d/%d)", a.restartCount, a.manifest.MaxRestarts),
			AgentID:   a.id,
			Prov:      "plugin",
			Retryable: false,
		}
	}
	a.restartCount++
	a.logger.Warn("restarting plugin subprocess",
		"restart_count", a.restartCount,
		"max_restarts", a.manifest.MaxRestarts)
	// Drain any pending state.
	a.started = false
	if a.stdin != nil {
		_ = a.stdin.Close()
	}
	a.stdin = nil
	a.cmd = nil
	// Let lingering goroutines finish; their channels will close.
	return a.start()
}

// processExited reports whether the subprocess has terminated.
func (a *Agent) processExited() bool {
	select {
	case <-a.exited:
		return true
	default:
		return false
	}
}

// callRPC sends a JSON-RPC request and waits for the matching response. It
// serializes access via a.mu so a single subprocess only has one in-flight
// request at a time.
func (a *Agent) callRPC(ctx context.Context, method string, params any) (json.RawMessage, *RPCError, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if err := a.start(); err != nil {
		return nil, nil, err
	}

	// If the subprocess already died between calls, try one restart before
	// giving up on this request.
	if a.processExited() {
		if err := a.restart(); err != nil {
			return nil, nil, err
		}
	}

	res, rerr, err := a.doOneCall(ctx, method, params)
	if err == nil {
		return res, rerr, nil
	}
	// Single retry on process-exit style errors.
	if errors.Is(err, errProcessExited) {
		if rerr := a.restart(); rerr != nil {
			return nil, nil, rerr
		}
		return a.doOneCall(ctx, method, params)
	}
	return nil, nil, err
}

// errProcessExited marks a round-trip failure caused by the subprocess
// terminating. callRPC uses it to trigger a single restart+retry.
var errProcessExited = errors.New("plugin subprocess exited")

// doOneCall performs a single request/response exchange without attempting
// to restart on failure.
func (a *Agent) doOneCall(ctx context.Context, method string, params any) (json.RawMessage, *RPCError, error) {
	var paramsRaw json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return nil, nil, fmt.Errorf("marshal params: %w", err)
		}
		paramsRaw = b
	}

	id := atomic.AddInt64(&a.nextID, 1)
	req := RPCRequest{
		JSONRPC: "2.0",
		ID:      id,
		Method:  method,
		Params:  paramsRaw,
	}
	line, err := json.Marshal(req)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal request: %w", err)
	}
	line = append(line, '\n')

	// Compute an effective deadline: min(ctx deadline, RPCTimeout).
	timeout := a.manifest.RPCTimeout.Duration
	if timeout <= 0 {
		timeout = defaultRPCTimeout
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if _, werr := a.stdin.Write(line); werr != nil {
		// A write failure almost always means the subprocess has died —
		// classify as process-exit so callRPC triggers the restart path.
		return nil, nil, errProcessExited
	}

	for {
		select {
		case <-callCtx.Done():
			// If ctx parent was cancelled, propagate that; otherwise
			// a timeout indicates an unresponsive plugin and we prefer
			// to kill+restart on next call. Don't kill here — just
			// return so upper layers can retry.
			if errors.Is(ctx.Err(), context.Canceled) {
				return nil, nil, ctx.Err()
			}
			return nil, nil, fmt.Errorf("rpc call %q: %w", method, callCtx.Err())
		case <-a.exited:
			return nil, nil, errProcessExited
		case sl, ok := <-a.lines:
			if !ok {
				return nil, nil, errProcessExited
			}
			if sl.err != nil {
				return nil, nil, fmt.Errorf("read rpc response: %w", sl.err)
			}
			var resp RPCResponse
			if err := json.Unmarshal(sl.data, &resp); err != nil {
				// Non-JSON output on stdout. Log and keep reading —
				// it may be a spurious print before the real frame.
				a.logger.Warn("plugin emitted non-JSON on stdout; ignoring", "line_len", len(sl.data))
				continue
			}
			if resp.ID != id {
				// Mismatched frame; ignore and keep waiting.
				a.logger.Warn("plugin emitted rpc response with unexpected id", "want", id, "got", resp.ID)
				continue
			}
			return resp.Result, resp.Error, nil
		}
	}
}

// Execute implements agent.Agent. It invokes the plugin's "execute" method
// and maps the result back into a broker.TaskResult.
func (a *Agent) Execute(ctx context.Context, task *broker.Task) (*broker.TaskResult, error) {
	params := ExecuteParams{
		TaskID:       task.ID,
		PipelineID:   task.PipelineID,
		StageID:      task.StageID,
		Payload:      task.Payload,
		SystemPrompt: a.systemPrompt,
	}

	result, rpcErr, err := a.callRPC(ctx, MethodExecute, params)
	if err != nil {
		return nil, a.mapCallError(err)
	}
	if rpcErr != nil {
		return nil, a.mapRPCError(rpcErr)
	}

	var exec ExecuteResult
	if err := json.Unmarshal(result, &exec); err != nil {
		return nil, &agent.AgentError{
			Err:       fmt.Errorf("decode execute result: %w", err),
			AgentID:   a.id,
			Prov:      "plugin",
			Retryable: true,
		}
	}

	// Convert string-valued metadata to map[string]any for broker.
	var metadata map[string]any
	if len(exec.Metadata) > 0 {
		metadata = make(map[string]any, len(exec.Metadata))
		for k, v := range exec.Metadata {
			metadata[k] = v
		}
	}

	return &broker.TaskResult{
		TaskID:   task.ID,
		Payload:  exec.Output,
		Metadata: metadata,
	}, nil
}

// HealthCheck implements agent.Agent.
func (a *Agent) HealthCheck(ctx context.Context) error {
	result, rpcErr, err := a.callRPC(ctx, MethodHealthCheck, nil)
	if err != nil {
		return a.mapCallError(err)
	}
	if rpcErr != nil {
		return a.mapRPCError(rpcErr)
	}
	var hc HealthCheckResult
	if err := json.Unmarshal(result, &hc); err != nil {
		return fmt.Errorf("plugin %q: decode health result: %w", a.id, err)
	}
	if !hc.Healthy {
		return fmt.Errorf("plugin %q unhealthy: %s", a.id, hc.Message)
	}
	return nil
}

// mapCallError turns a transport-level error into an AgentError. Most of
// these are retryable so the broker can retry on a fresh subprocess.
func (a *Agent) mapCallError(err error) error {
	// Non-retryable unhealthy errors from restart() are already AgentErrors.
	var ae *agent.AgentError
	if errors.As(err, &ae) {
		return ae
	}
	return &agent.AgentError{
		Err:       err,
		AgentID:   a.id,
		Prov:      "plugin",
		Retryable: true,
	}
}

// mapRPCError maps a JSON-RPC error code into a broker AgentError, honoring
// the manifest's on_failure policy for internal errors.
func (a *Agent) mapRPCError(rerr *RPCError) error {
	switch rerr.Code {
	case RPCErrorInvalidParams:
		return &agent.AgentError{
			Err:       fmt.Errorf("plugin invalid params: %s", rerr.Message),
			AgentID:   a.id,
			Prov:      "plugin",
			Retryable: false,
		}
	case RPCErrorInternal:
		retryable := a.manifest.OnFailureOrDefault() == OnFailureRetryable
		return &agent.AgentError{
			Err:       fmt.Errorf("plugin internal error: %s", rerr.Message),
			AgentID:   a.id,
			Prov:      "plugin",
			Retryable: retryable,
		}
	default:
		// Unknown code — retryable by default.
		return &agent.AgentError{
			Err:       fmt.Errorf("plugin rpc error %d: %s", rerr.Code, rerr.Message),
			AgentID:   a.id,
			Prov:      "plugin",
			Retryable: true,
		}
	}
}

// Stop terminates the subprocess: SIGTERM, wait up to ShutdownTimeout, then
// SIGKILL if still alive. Safe to call multiple times.
func (a *Agent) Stop() error {
	a.mu.Lock()
	if !a.started || a.cmd == nil || a.cmd.Process == nil {
		a.mu.Unlock()
		return nil
	}
	cmd := a.cmd
	exited := a.exited
	stdin := a.stdin
	timeout := a.manifest.ShutdownTimeoutOrDefault()
	a.started = false
	a.mu.Unlock()

	if stdin != nil {
		_ = stdin.Close()
	}
	// Graceful SIGTERM first.
	_ = cmd.Process.Signal(syscall.SIGTERM)

	select {
	case <-exited:
		return nil
	case <-time.After(timeout):
		a.logger.Warn("plugin subprocess did not exit in time; sending SIGKILL", "timeout", timeout)
		_ = cmd.Process.Kill()
		<-exited
		return nil
	}
}

// prefixedWriter prepends prefix to every Write call, useful for tagging a
// subprocess's stderr stream.
type prefixedWriter struct {
	prefix string
	w      io.Writer
}

func (p *prefixedWriter) Write(b []byte) (int, error) {
	// Simple approach: write prefix + data. Line boundaries are preserved
	// by the subprocess.
	if _, err := io.WriteString(p.w, p.prefix); err != nil {
		return 0, err
	}
	return p.w.Write(b)
}
