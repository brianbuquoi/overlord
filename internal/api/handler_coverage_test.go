package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/brianbuquoi/overlord/internal/broker"
	"github.com/brianbuquoi/overlord/internal/contract"
	"github.com/brianbuquoi/overlord/internal/store/memory"
)

// -----------------------------------------------------------------------------
// Novel tests requested by the coverage-gap task. The following items from the
// original request are INTENTIONALLY NOT DUPLICATED here — they are already
// covered elsewhere in this package:
//
//   - SubmitTask_InvalidPipelineID  → TestSubmitTask_PipelineNotFound (server_test.go)
//                                     and TestErrorResponses_AllPaths row
//                                     "404 unknown pipeline ID on submit"
//                                     (coverage_gap_test.go).
//   - SubmitTask_InvalidPayload     → TestErrorResponses_AllPaths row
//                                     "400 payload is not object"
//                                     (INVALID_PAYLOAD_TYPE) and the related
//                                     MISSING_PAYLOAD / INVALID_JSON rows.
//   - GetTask_NotFound              → TestGetTask_NotFound (server_test.go)
//                                     and TestErrorResponses_AllPaths row
//                                     "404 unknown task ID".
//   - RateLimit_Enforced            → TestRateLimiter_BlocksAfterBurst
//                                     (server_test.go): 100 OK, 101st 429,
//                                     different IP still OK — uses frozen
//                                     clock via srv.limiter.now.
// -----------------------------------------------------------------------------

// --- /v1/health with EVERY agent unhealthy ---------------------------------
//
// TestHealth_Degraded in server_test.go covers the mixed case (one healthy,
// one unhealthy). This test covers the all-unhealthy path: the response must
// still be a well-formed 200 with status=degraded — NOT a 500.

func TestHandler_Health_AllAgentsDegraded(t *testing.T) {
	cfg := testConfig()
	st := memory.New()
	agents := map[string]broker.Agent{
		"agent-1": &stubAgent{id: "agent-1", provider: "anthropic", healthy: false, healthErr: "dial tcp: i/o timeout PROVIDER_X"},
		"agent-2": &stubAgent{id: "agent-2", provider: "openai", healthy: false, healthErr: "401 unauthorized from upstream SECRET_TOKEN"},
	}
	reg, _ := contract.NewRegistry(nil, "")
	logger := slog.Default()
	b := broker.New(cfg, st, agents, reg, logger, nil, nil)
	srv := NewServer(b, logger, nil, "")
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })

	req := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 (not 500) on all-unhealthy agents, got %d: %s", w.Code, w.Body.String())
	}

	var resp healthResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if resp.Status != "degraded" {
		t.Fatalf("expected status=degraded, got %q", resp.Status)
	}
	if len(resp.Agents) != 2 {
		t.Fatalf("expected 2 agent entries, got %d", len(resp.Agents))
	}
	for id, h := range resp.Agents {
		if h.Status != "error" {
			t.Fatalf("agent %s: expected status=error, got %q", id, h.Status)
		}
		if h.Message != "health check failed" {
			t.Fatalf("agent %s: expected opaque message, got %q", id, h.Message)
		}
	}

	// No raw provider error string should leak.
	body := w.Body.String()
	for _, leak := range []string{"dial tcp", "i/o timeout", "PROVIDER_X", "401 unauthorized", "SECRET_TOKEN"} {
		if strings.Contains(body, leak) {
			t.Fatalf("health body leaked internal detail %q: %s", leak, body)
		}
	}
}

// --- GET /v1/tasks with every query-param filter applied --------------------
//
// Existing tests cover individual filters (state, pipeline_id+limit). This
// test exercises pipeline_id + stage_id + state + limit + offset together
// and asserts each filter narrows the result set.

func TestHandler_ListTasks_AllQueryParams(t *testing.T) {
	srv, b := newTestServer(t)
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	handler := srv.Handler()

	// Seed tasks. Submit puts each in the intake stage; we then manually
	// adjust stage/state on some so filters can discriminate.
	ctx := context.Background()
	var ids []string
	for i := 0; i < 6; i++ {
		tk, err := b.Submit(ctx, "test-pipeline", json.RawMessage(`{"x":1}`))
		if err != nil {
			t.Fatalf("submit %d: %v", i, err)
		}
		ids = append(ids, tk.ID)
	}

	// ids[0..2]: leave as-is (stage=intake, state=PENDING).
	// ids[3]: stage=intake, state=FAILED.
	// ids[4..5]: stage=other-stage, state=PENDING.
	failed := broker.TaskStateFailed
	if err := b.Store().UpdateTask(ctx, ids[3], broker.TaskUpdate{State: &failed}); err != nil {
		t.Fatalf("update ids[3]: %v", err)
	}
	otherStage := "other-stage"
	for _, id := range ids[4:] {
		if err := b.Store().UpdateTask(ctx, id, broker.TaskUpdate{StageID: &otherStage}); err != nil {
			t.Fatalf("update stage: %v", err)
		}
	}

	// pipeline_id filter: unknown pipeline → 0 results.
	t.Run("pipeline_id_filter", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/tasks?pipeline_id=no-such-pipeline", nil)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("status: %d", w.Code)
		}
		var resp listTasksResponse
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
		if len(resp.Tasks) != 0 {
			t.Fatalf("expected 0 tasks for unknown pipeline, got %d", len(resp.Tasks))
		}
	})

	// stage_id filter: only the 2 tasks at other-stage.
	t.Run("stage_id_filter", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/tasks?stage_id=other-stage", nil)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		var resp listTasksResponse
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
		if len(resp.Tasks) != 2 {
			t.Fatalf("expected 2 tasks at other-stage, got %d", len(resp.Tasks))
		}
		for _, tk := range resp.Tasks {
			if tk.StageID != "other-stage" {
				t.Fatalf("unexpected stage_id %q", tk.StageID)
			}
		}
	})

	// state filter: exactly the one FAILED task.
	t.Run("state_filter", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/tasks?state=FAILED", nil)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		var resp listTasksResponse
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
		if len(resp.Tasks) != 1 {
			t.Fatalf("expected 1 FAILED task, got %d", len(resp.Tasks))
		}
		if resp.Tasks[0].State != broker.TaskStateFailed {
			t.Fatalf("expected state=FAILED, got %q", resp.Tasks[0].State)
		}
	})

	// limit filter.
	t.Run("limit_filter", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/tasks?limit=2", nil)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		var resp listTasksResponse
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
		if len(resp.Tasks) != 2 {
			t.Fatalf("expected limit=2 to yield 2 tasks, got %d", len(resp.Tasks))
		}
		if resp.Total != 6 {
			t.Fatalf("expected total=6 regardless of limit, got %d", resp.Total)
		}
	})

	// offset filter: offset past remaining returns fewer.
	t.Run("offset_filter", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/tasks?offset=5", nil)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		var resp listTasksResponse
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
		if len(resp.Tasks) != 1 {
			t.Fatalf("expected offset=5 to yield 1 task, got %d", len(resp.Tasks))
		}
	})

	// Combined filters: pipeline_id + stage_id + limit + offset.
	t.Run("combined_filters", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet,
			"/v1/tasks?pipeline_id=test-pipeline&stage_id=other-stage&limit=1&offset=0", nil)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		var resp listTasksResponse
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
		if len(resp.Tasks) != 1 {
			t.Fatalf("expected 1 task with combined filters, got %d", len(resp.Tasks))
		}
		if resp.Tasks[0].StageID != "other-stage" {
			t.Fatalf("expected stage_id=other-stage, got %q", resp.Tasks[0].StageID)
		}
		if resp.Total != 2 {
			t.Fatalf("expected total=2 matching stage_id=other-stage, got %d", resp.Total)
		}
	})
}

// --- 5xx paths MUST NOT leak internal error strings -------------------------
//
// For every handler that routes through Server.writeInternalError, inject a
// store that returns an error containing a highly distinctive marker and
// assert the marker does NOT appear in the response body. The client gets
// the stable public code and opaque message; operators correlate via the
// request_id that IS in the body and in server logs.

// internalLeakMarker is a distinctive, easily-grepped string embedded in
// every error returned by failingStore. If it ever appears in a response
// body, writeInternalError is leaking internals.
const internalLeakMarker = "INTERNAL_LEAK_MARKER_9a7f3c2e_db_password_swordfish"

// failingStore returns internalLeakMarker-bearing errors on demand for every
// Store method a handler might call. Fields toggle which operations fail.
type failingStore struct {
	failGet      bool
	failList     bool
	failUpdate   bool
	failEnqueue  bool
	failClaim    bool
	failRollback bool

	// If non-nil, GetTask returns this task (used so handleDiscardDeadLetter
	// gets past the initial GetTask lookup and into the UpdateTask path).
	getTaskResult *broker.Task

	// If non-nil, ClaimForReplay returns this task (used so replay
	// proceeds to Submit/EnqueueTask, whose failure path is what we want
	// to observe).
	claimResult *broker.Task
}

func (f *failingStore) EnqueueTask(_ context.Context, _ string, _ *broker.Task) error {
	if f.failEnqueue {
		return errors.New(internalLeakMarker + " enqueue exploded")
	}
	return nil
}
func (f *failingStore) RequeueTask(_ context.Context, _ string, _ string, _ broker.TaskUpdate) error {
	if f.failEnqueue {
		return errors.New(internalLeakMarker + " requeue exploded")
	}
	return nil
}
func (f *failingStore) DequeueTask(_ context.Context, _ string) (*broker.Task, error) {
	return nil, broker.ErrQueueEmpty
}
func (f *failingStore) UpdateTask(_ context.Context, _ string, _ broker.TaskUpdate) error {
	if f.failUpdate {
		return errors.New(internalLeakMarker + " update exploded")
	}
	return nil
}
func (f *failingStore) GetTask(_ context.Context, taskID string) (*broker.Task, error) {
	if f.failGet {
		return nil, errors.New(internalLeakMarker + " get exploded")
	}
	if f.getTaskResult != nil {
		// Return a fresh copy with the requested ID.
		cp := *f.getTaskResult
		if cp.ID == "" {
			cp.ID = taskID
		}
		return &cp, nil
	}
	return nil, errors.New("not found")
}
func (f *failingStore) ListTasks(_ context.Context, _ broker.TaskFilter) (*broker.ListTasksResult, error) {
	if f.failList {
		return nil, errors.New(internalLeakMarker + " list exploded")
	}
	return &broker.ListTasksResult{Tasks: []*broker.Task{}, Total: 0}, nil
}
func (f *failingStore) ClaimForReplay(_ context.Context, taskID string) (*broker.Task, error) {
	if f.failClaim {
		return nil, errors.New(internalLeakMarker + " claim exploded")
	}
	if f.claimResult != nil {
		cp := *f.claimResult
		if cp.ID == "" {
			cp.ID = taskID
		}
		return &cp, nil
	}
	return nil, errors.New(internalLeakMarker + " claim unexpected")
}
func (f *failingStore) RollbackReplayClaim(_ context.Context, _ string) error {
	if f.failRollback {
		return errors.New(internalLeakMarker + " rollback exploded")
	}
	return nil
}

// newTestServerWithStore builds a Server whose broker uses the given Store.
// Agents are healthy stubs; registry is empty; logger is the default.
func newTestServerWithStore(t *testing.T, st broker.Store) *Server {
	t.Helper()
	cfg := testConfig()
	agents := map[string]broker.Agent{
		"agent-1": &stubAgent{id: "agent-1", provider: "anthropic", healthy: true},
		"agent-2": &stubAgent{id: "agent-2", provider: "openai", healthy: true},
	}
	reg, _ := contract.NewRegistry(nil, "")
	logger := slog.Default()
	b := broker.New(cfg, st, agents, reg, logger, nil, nil)
	srv := NewServer(b, logger, nil, "")
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	return srv
}

func TestHandler_WriteError_NoInternalDetails(t *testing.T) {
	// Build a reusable dead-lettered task template used by handlers that
	// GetTask before the failing op (DiscardDeadLetter's UpdateTask path).
	deadLetterTask := &broker.Task{
		ID:                 "dl-task-1",
		PipelineID:         "test-pipeline",
		StageID:            "intake",
		State:              broker.TaskStateFailed,
		RoutedToDeadLetter: true,
	}

	// Claim-successful task used to drive handleReplayDeadLetter past the
	// ClaimForReplay step so we can observe the Submit/EnqueueTask failure
	// arm (REPLAY_SUBMIT_FAILED).
	claimedTask := &broker.Task{
		ID:         "dl-task-2",
		PipelineID: "test-pipeline",
		StageID:    "intake",
		State:      broker.TaskStateReplayPending,
		Payload:    json.RawMessage(`{"x":1}`),
	}

	cases := []struct {
		name         string
		method       string
		path         string
		body         string
		store        *failingStore
		expectStatus int
		expectCode   string
	}{
		{
			name:         "GET_TASK_FAILED",
			method:       http.MethodGet,
			path:         "/v1/tasks/any-id",
			store:        &failingStore{failGet: true},
			expectStatus: http.StatusInternalServerError,
			expectCode:   "GET_TASK_FAILED",
		},
		{
			name:         "LIST_TASKS_FAILED",
			method:       http.MethodGet,
			path:         "/v1/tasks",
			store:        &failingStore{failList: true},
			expectStatus: http.StatusInternalServerError,
			expectCode:   "LIST_TASKS_FAILED",
		},
		{
			name:         "LIST_DEAD_LETTER_FAILED",
			method:       http.MethodGet,
			path:         "/v1/dead-letter",
			store:        &failingStore{failList: true},
			expectStatus: http.StatusInternalServerError,
			expectCode:   "LIST_DEAD_LETTER_FAILED",
		},
		{
			// GetTask succeeds and returns a dead-lettered task; UpdateTask
			// (the discard write) then fails → DISCARD_FAILED.
			name:         "DISCARD_FAILED",
			method:       http.MethodPost,
			path:         "/v1/dead-letter/dl-task-1/discard",
			store:        &failingStore{getTaskResult: deadLetterTask, failUpdate: true},
			expectStatus: http.StatusInternalServerError,
			expectCode:   "DISCARD_FAILED",
		},
		{
			// GetTask fails with a non-"not found" error inside the discard
			// handler → GET_TASK_FAILED from that handler.
			name:         "GET_TASK_FAILED_via_discard",
			method:       http.MethodPost,
			path:         "/v1/dead-letter/dl-task-1/discard",
			store:        &failingStore{failGet: true},
			expectStatus: http.StatusInternalServerError,
			expectCode:   "GET_TASK_FAILED",
		},
		{
			// ClaimForReplay returns a non-sentinel error → REPLAY_FAILED.
			name:         "REPLAY_FAILED",
			method:       http.MethodPost,
			path:         "/v1/dead-letter/dl-task-2/replay",
			store:        &failingStore{failClaim: true},
			expectStatus: http.StatusInternalServerError,
			expectCode:   "REPLAY_FAILED",
		},
		{
			// Claim succeeds; subsequent broker.Submit fails because
			// EnqueueTask is rigged to fail → REPLAY_SUBMIT_FAILED.
			name:         "REPLAY_SUBMIT_FAILED",
			method:       http.MethodPost,
			path:         "/v1/dead-letter/dl-task-2/replay",
			store:        &failingStore{claimResult: claimedTask, failEnqueue: true},
			expectStatus: http.StatusInternalServerError,
			expectCode:   "REPLAY_SUBMIT_FAILED",
		},
		{
			// RollbackReplayClaim returns a non-sentinel error → RECOVER_FAILED.
			name:         "RECOVER_FAILED",
			method:       http.MethodPost,
			path:         "/v1/tasks/any-id/recover",
			store:        &failingStore{failRollback: true},
			expectStatus: http.StatusInternalServerError,
			expectCode:   "RECOVER_FAILED",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newTestServerWithStore(t, tc.store)
			var req *http.Request
			if tc.body != "" {
				req = httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
				req.Header.Set("Content-Type", "application/json")
			} else {
				req = httptest.NewRequest(tc.method, tc.path, nil)
			}
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, req)

			if w.Code != tc.expectStatus {
				t.Fatalf("status: expected %d, got %d (body: %s)", tc.expectStatus, w.Code, w.Body.String())
			}

			body := w.Body.String()
			if strings.Contains(body, internalLeakMarker) {
				t.Fatalf("5xx response leaked internal marker %q in body: %s", internalLeakMarker, body)
			}

			var resp errorResponse
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("body is not valid JSON error: %v (body: %s)", err, body)
			}
			if resp.Code != tc.expectCode {
				t.Fatalf("expected code %q, got %q", tc.expectCode, resp.Code)
			}
			if resp.Error == "" {
				t.Fatal("expected non-empty public error message")
			}
			if strings.Contains(resp.Error, internalLeakMarker) {
				t.Fatalf("public error message leaked marker: %q", resp.Error)
			}
			// request_id is required for operator correlation on 5xx.
			if resp.RequestID == "" {
				t.Fatal("expected request_id on 5xx response for log correlation")
			}
		})
	}
}
