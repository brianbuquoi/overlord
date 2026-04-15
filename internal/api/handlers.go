package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/brianbuquoi/overlord/internal/broker"
	"github.com/brianbuquoi/overlord/internal/config"
	"github.com/brianbuquoi/overlord/internal/store"
	"go.opentelemetry.io/otel/propagation"
)

// onSuccessJSON converts an OnSuccessConfig to a JSON-serializable value.
// Static configs return the string directly; conditional configs return
// a structured object with routes and default.
func onSuccessJSON(cfg config.OnSuccessConfig) interface{} {
	if !cfg.IsConditional {
		return cfg.Static
	}
	type routeJSON struct {
		Condition string `json:"condition"`
		Stage     string `json:"stage"`
	}
	type conditionalJSON struct {
		Routes  []routeJSON `json:"routes"`
		Default string      `json:"default"`
	}
	routes := make([]routeJSON, 0, len(cfg.Routes))
	for _, r := range cfg.Routes {
		routes = append(routes, routeJSON{Condition: r.RawExpr, Stage: r.Stage})
	}
	return conditionalJSON{Routes: routes, Default: cfg.Default}
}

// --- Request/Response types ---

type submitTaskRequest struct {
	Payload json.RawMessage `json:"payload"`
}

type submitTaskResponse struct {
	TaskID string `json:"task_id"`
	State  string `json:"state"`
}

type listTasksResponse struct {
	Tasks []*broker.Task `json:"tasks"`
	Total int            `json:"total"`
}

type pipelineSummary struct {
	Name        string         `json:"name"`
	Concurrency int            `json:"concurrency"`
	Store       string         `json:"store"`
	Stages      []stageSummary `json:"stages"`
}

type stageSummary struct {
	ID           string        `json:"id"`
	Agent        string        `json:"agent"`
	InputSchema  schemaRefJSON `json:"input_schema"`
	OutputSchema schemaRefJSON `json:"output_schema"`
	OnSuccess    interface{}   `json:"on_success"`
	OnFailure    string        `json:"on_failure"`
}

type schemaRefJSON struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type healthResponse struct {
	Status string                 `json:"status"`
	Agents map[string]agentHealth `json:"agents"`
}

type agentHealth struct {
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

type errorResponse struct {
	Error     string `json:"error"`
	Code      string `json:"code"`
	RequestID string `json:"request_id,omitempty"`
}

// --- Handlers ---

// maxRequestBodySize is the maximum allowed size for incoming request bodies (1MB).
const maxRequestBodySize = 1 << 20

// maxListLimit is the maximum allowed limit for ListTasks queries.
const maxListLimit = 1000

// maxBulkOperationTasks bounds replay-all and discard-all per-call so a
// runaway operation cannot walk an unbounded dead-letter backlog. Callers
// that hit the ceiling receive `"truncated": true` in the response and can
// reissue the call to drain the remainder.
const maxBulkOperationTasks = 100000

func (s *Server) handleSubmitTask(w http.ResponseWriter, r *http.Request) {
	pipelineID := pathParam(r.URL.Path, "/v1/pipelines/", "/tasks")
	if pipelineID == "" {
		writeError(w, http.StatusBadRequest, "missing pipeline_id in path", "INVALID_PATH")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodySize)
	var req submitTaskRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body", "INVALID_JSON")
		return
	}
	if req.Payload == nil {
		writeError(w, http.StatusBadRequest, "payload is required", "MISSING_PAYLOAD")
		return
	}

	// Payload must be a JSON object, not a string, number, array, or boolean.
	trimmed := bytes.TrimSpace(req.Payload)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		writeError(w, http.StatusBadRequest, "payload must be a JSON object", "INVALID_PAYLOAD_TYPE")
		return
	}

	// Extract W3C traceparent header for trace propagation.
	carrier := propagation.HeaderCarrier(r.Header)
	task, err := s.broker.SubmitWithCarrier(r.Context(), pipelineID, req.Payload, carrier)
	if err != nil {
		if strings.Contains(err.Error(), "pipeline not found") {
			s.writeInternalError(w, r, http.StatusNotFound, "pipeline not found", "PIPELINE_NOT_FOUND", err)
			return
		}
		s.writeInternalError(w, r, http.StatusInternalServerError, "failed to submit task", "SUBMIT_FAILED", err)
		return
	}

	writeJSON(w, http.StatusAccepted, submitTaskResponse{
		TaskID: task.ID,
		State:  string(task.State),
	})
}

func (s *Server) handleGetTask(w http.ResponseWriter, r *http.Request) {
	taskID := strings.TrimPrefix(r.URL.Path, "/v1/tasks/")
	if taskID == "" || strings.Contains(taskID, "/") {
		writeError(w, http.StatusBadRequest, "missing task_id in path", "INVALID_PATH")
		return
	}

	task, err := s.broker.Store().GetTask(r.Context(), taskID)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeError(w, http.StatusNotFound, "task not found", "TASK_NOT_FOUND")
			return
		}
		s.writeInternalError(w, r, http.StatusInternalServerError, "failed to fetch task", "GET_TASK_FAILED", err)
		return
	}

	writeJSON(w, http.StatusOK, task)
}

func (s *Server) handleListTasks(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	filter := broker.TaskFilter{
		Limit:  50,
		Offset: 0,
	}

	if v := q.Get("pipeline_id"); v != "" {
		filter.PipelineID = &v
	}
	if v := q.Get("stage_id"); v != "" {
		filter.StageID = &v
	}
	if v := q.Get("state"); v != "" {
		state := broker.TaskState(v)
		filter.State = &state
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "limit must be a number", "INVALID_LIMIT")
			return
		}
		if n < 1 {
			writeError(w, http.StatusBadRequest, "limit must be at least 1", "INVALID_LIMIT")
			return
		}
		if n > maxListLimit {
			writeError(w, http.StatusBadRequest,
				fmt.Sprintf("limit must not exceed %d", maxListLimit), "INVALID_LIMIT")
			return
		}
		filter.Limit = n
	}
	if v := q.Get("offset"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "offset must be a number", "INVALID_OFFSET")
			return
		}
		if n < 0 {
			writeError(w, http.StatusBadRequest, "offset must not be negative", "INVALID_OFFSET")
			return
		}
		filter.Offset = n
	}
	if q.Get("include_discarded") == "true" {
		filter.IncludeDiscarded = true
	}

	result, err := s.broker.Store().ListTasks(r.Context(), filter)
	if err != nil {
		s.writeInternalError(w, r, http.StatusInternalServerError, "failed to list tasks", "LIST_TASKS_FAILED", err)
		return
	}
	tasks := result.Tasks
	if tasks == nil {
		tasks = []*broker.Task{}
	}

	writeJSON(w, http.StatusOK, listTasksResponse{
		Tasks: tasks,
		Total: result.Total,
	})
}

func (s *Server) handleListPipelines(w http.ResponseWriter, r *http.Request) {
	cfg := s.broker.Config()
	pipelines := make([]pipelineSummary, 0, len(cfg.Pipelines))

	for _, p := range cfg.Pipelines {
		stages := make([]stageSummary, 0, len(p.Stages))
		for _, st := range p.Stages {
			stages = append(stages, stageSummary{
				ID:    st.ID,
				Agent: st.Agent,
				InputSchema: schemaRefJSON{
					Name:    st.InputSchema.Name,
					Version: st.InputSchema.Version,
				},
				OutputSchema: schemaRefJSON{
					Name:    st.OutputSchema.Name,
					Version: st.OutputSchema.Version,
				},
				OnSuccess: onSuccessJSON(st.OnSuccess),
				OnFailure: st.OnFailure,
			})
		}
		pipelines = append(pipelines, pipelineSummary{
			Name:        p.Name,
			Concurrency: p.Concurrency,
			Store:       p.Store,
			Stages:      stages,
		})
	}

	writeJSON(w, http.StatusOK, pipelines)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	agents := s.broker.Agents()
	results := make(map[string]agentHealth, len(agents))
	status := "ok"

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	var mu sync.Mutex
	var wg sync.WaitGroup

	logger := s.logger
	if logger == nil {
		logger = slog.Default()
	}

	for id, ag := range agents {
		wg.Add(1)
		go func(id string, ag broker.Agent) {
			defer wg.Done()
			err := ag.HealthCheck(ctx)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				logger.Error("agent health check failed",
					"agent_id", id,
					"provider", ag.Provider(),
					"error", err,
				)
				results[id] = agentHealth{Status: "error", Message: "health check failed"}
				status = "degraded"
			} else {
				results[id] = agentHealth{Status: "ok"}
			}
		}(id, ag)
	}
	wg.Wait()

	writeJSON(w, http.StatusOK, healthResponse{
		Status: status,
		Agents: results,
	})
}

// --- Dead Letter Handlers ---

type deadLetterListResponse struct {
	Tasks []*broker.Task `json:"tasks"`
	Total int            `json:"total"`
}

type replayResponse struct {
	TaskID string `json:"task_id"`
}

type replayAllResponse struct {
	Processed int  `json:"processed"`
	Failed    int  `json:"failed,omitempty"`
	Truncated bool `json:"truncated,omitempty"`
}

type discardAllResponse struct {
	Processed int  `json:"processed"`
	Failed    int  `json:"failed,omitempty"`
	Truncated bool `json:"truncated,omitempty"`
}

func (s *Server) handleListDeadLetter(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	filter := broker.TaskFilter{
		Limit:  50,
		Offset: 0,
	}

	deadLetter := true
	filter.RoutedToDeadLetter = &deadLetter
	failedState := broker.TaskStateFailed
	filter.State = &failedState

	if v := q.Get("pipeline_id"); v != "" {
		filter.PipelineID = &v
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			writeError(w, http.StatusBadRequest, "limit must be a positive number", "INVALID_LIMIT")
			return
		}
		if n > maxListLimit {
			writeError(w, http.StatusBadRequest,
				fmt.Sprintf("limit must not exceed %d", maxListLimit), "INVALID_LIMIT")
			return
		}
		filter.Limit = n
	}
	if v := q.Get("offset"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "offset must be a non-negative number", "INVALID_OFFSET")
			return
		}
		filter.Offset = n
	}

	result, err := s.broker.Store().ListTasks(r.Context(), filter)
	if err != nil {
		s.writeInternalError(w, r, http.StatusInternalServerError, "failed to list dead-letter tasks", "LIST_DEAD_LETTER_FAILED", err)
		return
	}
	tasks := result.Tasks
	if tasks == nil {
		tasks = []*broker.Task{}
	}
	writeJSON(w, http.StatusOK, deadLetterListResponse{Tasks: tasks, Total: result.Total})
}

func (s *Server) handleReplayDeadLetter(w http.ResponseWriter, r *http.Request) {
	taskID := pathParam(r.URL.Path, "/v1/dead-letter/", "/replay")
	if taskID == "" {
		writeError(w, http.StatusBadRequest, "missing task_id in path", "INVALID_PATH")
		return
	}

	logger := s.logger
	if logger == nil {
		logger = slog.Default()
	}

	task, err := s.broker.Store().ClaimForReplay(r.Context(), taskID)
	if errors.Is(err, store.ErrTaskNotFound) {
		writeError(w, http.StatusNotFound, "task not found", "TASK_NOT_FOUND")
		return
	}
	if errors.Is(err, store.ErrTaskNotReplayable) {
		writeError(w, http.StatusConflict, "task is not in a replayable state", "TASK_NOT_REPLAYABLE")
		return
	}
	if err != nil {
		s.writeInternalError(w, r, http.StatusInternalServerError, "failed to claim task for replay", "REPLAY_FAILED", err)
		return
	}

	newTask, err := s.broker.Submit(r.Context(), task.PipelineID, task.Payload)
	if err != nil {
		if rbErr := s.broker.Store().RollbackReplayClaim(r.Context(), task.ID); rbErr != nil {
			logger.Error("replay rollback failed: task may be stranded in REPLAY_PENDING",
				"task_id", task.ID,
				"submit_error", err.Error(),
				"rollback_error", rbErr.Error(),
			)
		}
		s.writeInternalError(w, r, http.StatusInternalServerError, "failed to submit replay task", "REPLAY_SUBMIT_FAILED", err)
		return
	}

	replayed := broker.TaskStateReplayed
	if markErr := s.broker.Store().UpdateTask(r.Context(), task.ID, broker.TaskUpdate{State: &replayed}); markErr != nil {
		logger.Warn("replay: failed to mark original task as REPLAYED",
			"task_id", task.ID,
			"error", markErr.Error(),
		)
	}

	writeJSON(w, http.StatusAccepted, replayResponse{TaskID: newTask.ID})
}

func (s *Server) handleDiscardDeadLetter(w http.ResponseWriter, r *http.Request) {
	taskID := pathParam(r.URL.Path, "/v1/dead-letter/", "/discard")
	if taskID == "" {
		writeError(w, http.StatusBadRequest, "missing task_id in path", "INVALID_PATH")
		return
	}

	task, err := s.broker.Store().GetTask(r.Context(), taskID)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeError(w, http.StatusNotFound, "task not found", "TASK_NOT_FOUND")
			return
		}
		s.writeInternalError(w, r, http.StatusInternalServerError, "failed to fetch task", "GET_TASK_FAILED", err)
		return
	}

	if task.State == broker.TaskStateDiscarded {
		writeError(w, http.StatusConflict, "task is already discarded", "ALREADY_DISCARDED")
		return
	}
	if !task.RoutedToDeadLetter || task.State != broker.TaskStateFailed {
		writeError(w, http.StatusConflict, "task is not in dead-letter state", "NOT_DEAD_LETTERED")
		return
	}

	state := broker.TaskStateDiscarded
	if err := s.broker.Store().UpdateTask(r.Context(), taskID, broker.TaskUpdate{State: &state}); err != nil {
		s.writeInternalError(w, r, http.StatusInternalServerError, "failed to discard task", "DISCARD_FAILED", err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "discarded"})
}

func (s *Server) handleReplayAllDeadLetter(w http.ResponseWriter, r *http.Request) {
	pipelineID := r.URL.Query().Get("pipeline_id")
	if pipelineID == "" {
		writeError(w, http.StatusBadRequest, "pipeline_id query parameter is required", "MISSING_PIPELINE_ID")
		return
	}

	// Validate pipeline exists before consuming a rate-limit slot.
	if !s.pipelineExists(pipelineID) {
		writeError(w, http.StatusNotFound, "pipeline not found: "+pipelineID, "PIPELINE_NOT_FOUND")
		return
	}

	// Rate limit: 1 call per minute per pipeline.
	if !s.replayAllLimiter.allow(pipelineID) {
		w.Header().Set("Retry-After", "60")
		writeError(w, http.StatusTooManyRequests, "replay-all rate limited to 1 call per minute per pipeline", "RATE_LIMITED")
		return
	}

	deadLetter := true
	failedState := broker.TaskStateFailed

	// Replay leaves the original task in Failed+dead-letter state and submits
	// a new PENDING task. Since items do not drop out of the filter, we
	// advance the offset by the page size as we go.
	logger := s.logger
	if logger == nil {
		logger = slog.Default()
	}

	count := 0
	failed := 0
	truncated := false
	// ClaimForReplay flips RoutedToDeadLetter to false, so claimed tasks
	// do not reappear on subsequent pages. No failedIDs guard is needed here.
	for count < maxBulkOperationTasks {
		// Successfully replayed tasks drop out of the filter because the
		// claim flipped RoutedToDeadLetter. We fetch offset=0 each iteration.
		page, err := s.broker.Store().ListTasks(r.Context(), broker.TaskFilter{
			PipelineID:         &pipelineID,
			State:              &failedState,
			RoutedToDeadLetter: &deadLetter,
			Limit:              maxListLimit,
		})
		if err != nil {
			s.writeInternalError(w, r, http.StatusInternalServerError, "failed to list dead-letter tasks", "LIST_DEAD_LETTER_FAILED", err)
			return
		}
		if len(page.Tasks) == 0 {
			break
		}
		progressed := false
		for _, task := range page.Tasks {
			if count >= maxBulkOperationTasks {
				truncated = true
				break
			}
			claimed, err := s.broker.Store().ClaimForReplay(r.Context(), task.ID)
			if err != nil {
				// Concurrent claim or state drift — log and move on so a
				// transient skip does not block the rest of the page.
				logger.Warn("replay-all: failed to claim task",
					"task_id", task.ID,
					"pipeline_id", task.PipelineID,
					"error", err.Error(),
				)
				failed++
				continue
			}
			if _, err := s.broker.Submit(r.Context(), claimed.PipelineID, claimed.Payload); err != nil {
				logger.Warn("replay-all: failed to submit replay task",
					"task_id", task.ID,
					"pipeline_id", task.PipelineID,
					"error", err.Error(),
				)
				if rbErr := s.broker.Store().RollbackReplayClaim(r.Context(), task.ID); rbErr != nil {
					logger.Error("replay-all rollback failed: task may be stranded in REPLAY_PENDING",
						"task_id", task.ID,
						"submit_error", err.Error(),
						"rollback_error", rbErr.Error(),
					)
				}
				failed++
				continue
			}
			replayed := broker.TaskStateReplayed
			if markErr := s.broker.Store().UpdateTask(r.Context(), task.ID, broker.TaskUpdate{State: &replayed}); markErr != nil {
				logger.Warn("replay-all: failed to mark original task as REPLAYED",
					"task_id", task.ID,
					"error", markErr.Error(),
				)
			}
			count++
			progressed = true
		}
		if !progressed {
			// Every task in this page failed to be processed. Break to
			// avoid a tight loop over the same unclaimable entries.
			break
		}
	}
	if count >= maxBulkOperationTasks {
		truncated = true
		logger.Warn("replay-all hit per-call ceiling",
			"pipeline_id", pipelineID,
			"ceiling", maxBulkOperationTasks,
			"count", count,
		)
	}

	writeJSON(w, http.StatusAccepted, replayAllResponse{Processed: count, Failed: failed, Truncated: truncated})
}

func (s *Server) handleDiscardAllDeadLetter(w http.ResponseWriter, r *http.Request) {
	pipelineID := r.URL.Query().Get("pipeline_id")
	if pipelineID == "" {
		writeError(w, http.StatusBadRequest, "pipeline_id query parameter is required", "MISSING_PIPELINE_ID")
		return
	}

	if !s.pipelineExists(pipelineID) {
		writeError(w, http.StatusNotFound, "pipeline not found: "+pipelineID, "PIPELINE_NOT_FOUND")
		return
	}

	deadLetter := true
	failedState := broker.TaskStateFailed

	// Discarded tasks drop out of the filter (state transitions Failed→Discarded),
	// so we fetch offset=0 each iteration instead of advancing the offset.
	logger := s.logger
	if logger == nil {
		logger = slog.Default()
	}

	count := 0
	truncated := false
	state := broker.TaskStateDiscarded
	// failedIDs tracks task IDs that have already failed so that tasks
	// which reappear on subsequent pages (because discard did not remove
	// them from the filter) are not retried or double-counted.
	failedIDs := make(map[string]struct{})
	for count < maxBulkOperationTasks {
		page, err := s.broker.Store().ListTasks(r.Context(), broker.TaskFilter{
			PipelineID:         &pipelineID,
			State:              &failedState,
			RoutedToDeadLetter: &deadLetter,
			Limit:              maxListLimit,
		})
		if err != nil {
			s.writeInternalError(w, r, http.StatusInternalServerError, "failed to list dead-letter tasks", "LIST_DEAD_LETTER_FAILED", err)
			return
		}
		if len(page.Tasks) == 0 {
			break
		}
		progressed := false
		for _, task := range page.Tasks {
			if count >= maxBulkOperationTasks {
				truncated = true
				break
			}
			if _, already := failedIDs[task.ID]; already {
				continue
			}
			if err := s.broker.Store().UpdateTask(r.Context(), task.ID, broker.TaskUpdate{State: &state}); err != nil {
				logger.Warn("discard-all: failed to discard task",
					"task_id", task.ID,
					"pipeline_id", task.PipelineID,
					"error", err.Error(),
				)
				failedIDs[task.ID] = struct{}{}
				continue
			}
			count++
			progressed = true
		}
		// If no task in the current page could be updated, break to avoid an
		// infinite loop (e.g. a permissions or store error that won't recover).
		if !progressed {
			break
		}
	}
	if count >= maxBulkOperationTasks {
		truncated = true
		logger.Warn("discard-all hit per-call ceiling",
			"pipeline_id", pipelineID,
			"ceiling", maxBulkOperationTasks,
			"count", count,
		)
	}

	writeJSON(w, http.StatusOK, discardAllResponse{Processed: count, Failed: len(failedIDs), Truncated: truncated})
}

// --- Helpers ---

// pipelineExists returns true if the named pipeline is in the broker's config.
func (s *Server) pipelineExists(pipelineID string) bool {
	for _, p := range s.broker.Config().Pipelines {
		if p.Name == pipelineID {
			return true
		}
	}
	return false
}

// wsTokenResponse is the body returned by POST /v1/ws-token: a short-lived
// single-use token for authenticating a subsequent WebSocket upgrade.
type wsTokenResponse struct {
	Token     string `json:"token"`
	ExpiresIn int    `json:"expires_in"`
}

// handleIssueWSToken mints a short-lived WebSocket session token. The caller
// must be authenticated with a normal API key via the Authorization header;
// auth scope is enforced by the existing middleware (read scope is enough,
// since the WebSocket stream is read-only).
//
// Rationale: WebSockets cannot carry custom headers from browsers, so the
// dashboard used to append ?token=<apiKey> to the WebSocket URL. API keys
// travelling in URLs leak into proxy logs, browser history, and TLS
// debugging tooling. By minting a fresh, one-shot token scoped only to the
// next WS upgrade we keep the long-lived API key off the URL entirely.
func (s *Server) handleIssueWSToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "METHOD_NOT_ALLOWED")
		return
	}
	if s.wsTokens == nil {
		writeError(w, http.StatusServiceUnavailable, "ws-token not configured", "WS_TOKEN_UNAVAILABLE")
		return
	}
	token, ttl, err := s.wsTokens.issue()
	if err != nil {
		s.writeInternalError(w, r, http.StatusInternalServerError, "failed to issue ws token", "WS_TOKEN_FAILED", err)
		return
	}
	logger := s.logger
	if logger == nil {
		logger = slog.Default()
	}
	logger.Info("ws-token issued",
		"request_id", r.Header.Get(requestIDHeader),
		"expires_in", ttl,
	)
	writeJSON(w, http.StatusOK, wsTokenResponse{Token: token, ExpiresIn: ttl})
}

// pathParam extracts a parameter from a URL path between prefix and suffix.
// E.g. pathParam("/v1/pipelines/my-pipe/tasks", "/v1/pipelines/", "/tasks") = "my-pipe"
func pathParam(path, prefix, suffix string) string {
	if !strings.HasPrefix(path, prefix) {
		return ""
	}
	rest := strings.TrimPrefix(path, prefix)
	if suffix != "" {
		if !strings.HasSuffix(rest, suffix) {
			return ""
		}
		rest = strings.TrimSuffix(rest, suffix)
	}
	if rest == "" || strings.Contains(rest, "/") {
		return ""
	}
	return rest
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, message, code string) {
	writeJSON(w, status, errorResponse{Error: message, Code: code})
}

// writeInternalError logs the full internal error server-side and writes a
// stable, opaque error message to the client. The request ID is included in
// both the structured log line and the response body so operators can
// correlate a client-visible failure with server logs without leaking the
// underlying error string (which may expose store/provider internals).
func (s *Server) writeInternalError(w http.ResponseWriter, r *http.Request, status int, publicMsg, code string, internalErr error) {
	rid := r.Header.Get(requestIDHeader)
	if rid == "" {
		rid = shortRequestID()
	}
	logger := s.logger
	if logger == nil {
		logger = slog.Default()
	}
	logger.Error("handler error",
		"request_id", rid,
		"path", r.URL.Path,
		"method", r.Method,
		"status", status,
		"code", code,
		"public_msg", publicMsg,
		"error", internalErr,
	)
	writeJSON(w, status, errorResponse{Error: publicMsg, Code: code, RequestID: rid})
}

// shortRequestID returns a short random hex string for correlating a
// response with server logs when no upstream request ID was set.
func shortRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "unknown"
	}
	return hex.EncodeToString(b[:])
}
