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
	"go.opentelemetry.io/otel/propagation"
)

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
	OnSuccess    string        `json:"on_success"`
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
				OnSuccess: st.OnSuccess,
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

// --- Helpers ---

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
