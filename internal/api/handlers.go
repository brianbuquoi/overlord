package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/orcastrator/orcastrator/internal/broker"
	"github.com/orcastrator/orcastrator/internal/config"
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
	Status string            `json:"status"`
	Agents map[string]string `json:"agents"`
}

type errorResponse struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}

// --- Handlers ---

// maxRequestBodySize is the maximum allowed size for incoming request bodies (1MB).
const maxRequestBodySize = 1 << 20

// maxListLimit is the maximum allowed limit for ListTasks queries.
const maxListLimit = 1000

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
			writeError(w, http.StatusNotFound, err.Error(), "PIPELINE_NOT_FOUND")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error(), "SUBMIT_FAILED")
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
		writeError(w, http.StatusInternalServerError, err.Error(), "GET_TASK_FAILED")
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
		writeError(w, http.StatusInternalServerError, err.Error(), "LIST_TASKS_FAILED")
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
	results := make(map[string]string, len(agents))
	status := "ok"

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	var mu sync.Mutex
	var wg sync.WaitGroup

	for id, ag := range agents {
		wg.Add(1)
		go func(id string, ag broker.Agent) {
			defer wg.Done()
			err := ag.HealthCheck(ctx)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				results[id] = "error: " + err.Error()
				status = "degraded"
			} else {
				results[id] = "ok"
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
	Count int `json:"count"`
}

type discardAllResponse struct {
	Count int `json:"count"`
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
		writeError(w, http.StatusInternalServerError, err.Error(), "LIST_DEAD_LETTER_FAILED")
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

	task, err := s.broker.Store().GetTask(r.Context(), taskID)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeError(w, http.StatusNotFound, "task not found", "TASK_NOT_FOUND")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error(), "GET_TASK_FAILED")
		return
	}

	if !task.RoutedToDeadLetter || task.State != broker.TaskStateFailed {
		writeError(w, http.StatusConflict, "task is not in dead-letter state", "NOT_DEAD_LETTERED")
		return
	}

	newTask, err := s.broker.Submit(r.Context(), task.PipelineID, task.Payload)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "REPLAY_FAILED")
		return
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
		writeError(w, http.StatusInternalServerError, err.Error(), "GET_TASK_FAILED")
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
		writeError(w, http.StatusInternalServerError, err.Error(), "DISCARD_FAILED")
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
	result, err := s.broker.Store().ListTasks(r.Context(), broker.TaskFilter{
		PipelineID:         &pipelineID,
		State:              &failedState,
		RoutedToDeadLetter: &deadLetter,
		Limit:              maxListLimit,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "LIST_DEAD_LETTER_FAILED")
		return
	}

	count := 0
	for _, task := range result.Tasks {
		if _, err := s.broker.Submit(r.Context(), task.PipelineID, task.Payload); err == nil {
			count++
		}
	}

	writeJSON(w, http.StatusAccepted, replayAllResponse{Count: count})
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
	result, err := s.broker.Store().ListTasks(r.Context(), broker.TaskFilter{
		PipelineID:         &pipelineID,
		State:              &failedState,
		RoutedToDeadLetter: &deadLetter,
		Limit:              maxListLimit,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "LIST_DEAD_LETTER_FAILED")
		return
	}

	count := 0
	state := broker.TaskStateDiscarded
	for _, task := range result.Tasks {
		if err := s.broker.Store().UpdateTask(r.Context(), task.ID, broker.TaskUpdate{State: &state}); err == nil {
			count++
		}
	}

	writeJSON(w, http.StatusOK, discardAllResponse{Count: count})
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
