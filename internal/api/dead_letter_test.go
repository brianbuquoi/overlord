package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/orcastrator/orcastrator/internal/auth"
	"github.com/orcastrator/orcastrator/internal/broker"
	"github.com/orcastrator/orcastrator/internal/config"
	"github.com/orcastrator/orcastrator/internal/contract"
	"github.com/orcastrator/orcastrator/internal/store/memory"
	"golang.org/x/crypto/bcrypt"
)

// newDeadLetterTestServer creates a test server and populates it with tasks.
func newDeadLetterTestServer(t *testing.T) (*Server, *broker.Broker, *memory.MemoryStore) {
	t.Helper()
	cfg := testConfig()
	st := memory.New()
	agents := map[string]broker.Agent{
		"agent-1": &stubAgent{id: "agent-1", provider: "anthropic", healthy: true},
		"agent-2": &stubAgent{id: "agent-2", provider: "openai", healthy: true},
	}
	reg, _ := contract.NewRegistry(nil, "")
	logger := slog.Default()
	b := broker.New(cfg, st, agents, reg, logger, nil, nil)
	srv := NewServer(b, logger, nil, "")
	return srv, b, st
}

// seedDeadLetterTasks creates n dead-lettered tasks and m successful tasks.
func seedDeadLetterTasks(t *testing.T, b *broker.Broker, st *memory.MemoryStore, nFailed, nSuccess int) (failedIDs, successIDs []string) {
	t.Helper()
	ctx := context.Background()

	for i := 0; i < nFailed; i++ {
		task, err := b.Submit(ctx, "test-pipeline", json.RawMessage(fmt.Sprintf(`{"input":"fail-%d"}`, i)))
		if err != nil {
			t.Fatal(err)
		}
		// Manually set to FAILED + dead-lettered.
		state := broker.TaskStateFailed
		dl := true
		st.UpdateTask(ctx, task.ID, broker.TaskUpdate{
			State:              &state,
			RoutedToDeadLetter: &dl,
			Metadata:           map[string]any{"failure_reason": fmt.Sprintf("test failure %d", i)},
		})
		failedIDs = append(failedIDs, task.ID)
	}

	for i := 0; i < nSuccess; i++ {
		task, err := b.Submit(ctx, "test-pipeline", json.RawMessage(fmt.Sprintf(`{"input":"ok-%d"}`, i)))
		if err != nil {
			t.Fatal(err)
		}
		state := broker.TaskStateDone
		st.UpdateTask(ctx, task.ID, broker.TaskUpdate{State: &state})
		successIDs = append(successIDs, task.ID)
	}

	return failedIDs, successIDs
}

// Test 7: GET /v1/dead-letter returns only dead-lettered tasks.
func TestDeadLetterList_Accuracy(t *testing.T) {
	srv, b, st := newDeadLetterTestServer(t)
	defer srv.Shutdown(context.Background())

	failedIDs, _ := seedDeadLetterTasks(t, b, st, 3, 5)

	req := httptest.NewRequest(http.MethodGet, "/v1/dead-letter", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp deadLetterListResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}

	if resp.Total != 3 {
		t.Fatalf("expected 3 dead-lettered tasks, got %d", resp.Total)
	}

	// Verify the returned IDs match.
	returnedIDs := make(map[string]bool)
	for _, task := range resp.Tasks {
		returnedIDs[task.ID] = true
	}
	for _, id := range failedIDs {
		if !returnedIDs[id] {
			t.Fatalf("expected dead-lettered task %s in results", id)
		}
	}

	// GET /v1/tasks should return all 8.
	req = httptest.NewRequest(http.MethodGet, "/v1/tasks", nil)
	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	var tasksResp listTasksResponse
	json.Unmarshal(w.Body.Bytes(), &tasksResp)
	if tasksResp.Total != 8 {
		t.Fatalf("expected 8 total tasks, got %d", tasksResp.Total)
	}

	// Filter by non-existent pipeline.
	req = httptest.NewRequest(http.MethodGet, "/v1/dead-letter?pipeline_id=nonexistent", nil)
	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	var emptyResp deadLetterListResponse
	json.Unmarshal(w.Body.Bytes(), &emptyResp)
	if emptyResp.Total != 0 {
		t.Fatalf("expected 0 dead-lettered tasks for nonexistent pipeline, got %d", emptyResp.Total)
	}
}

// Test 8: Replay preserves payload, creates new task, original unchanged.
func TestDeadLetterReplay_PreservesPayload(t *testing.T) {
	srv, b, st := newDeadLetterTestServer(t)
	defer srv.Shutdown(context.Background())

	failedIDs, _ := seedDeadLetterTasks(t, b, st, 1, 0)
	taskID := failedIDs[0]

	// Get original task to know payload.
	origTask, _ := st.GetTask(context.Background(), taskID)
	origPayload := string(origTask.Payload)

	// Replay it.
	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/v1/dead-letter/%s/replay", taskID), nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", w.Code, w.Body.String())
	}

	var resp replayResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.TaskID == "" {
		t.Fatal("expected new task ID")
	}
	if resp.TaskID == taskID {
		t.Fatal("expected new task ID, got original")
	}

	// Verify new task has same payload.
	newTask, err := st.GetTask(context.Background(), resp.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if string(newTask.Payload) != origPayload {
		t.Fatalf("expected same payload %q, got %q", origPayload, string(newTask.Payload))
	}

	// Verify new task has fresh attempt count.
	if newTask.Attempts != 0 {
		t.Fatalf("expected 0 attempts on replayed task, got %d", newTask.Attempts)
	}

	// Verify original task is unchanged.
	origAfter, _ := st.GetTask(context.Background(), taskID)
	if origAfter.State != broker.TaskStateFailed {
		t.Fatalf("original task should still be FAILED, got %s", origAfter.State)
	}
}

// Test 10: Discard immutability.
func TestDeadLetterDiscard_Immutability(t *testing.T) {
	srv, b, st := newDeadLetterTestServer(t)
	defer srv.Shutdown(context.Background())

	failedIDs, _ := seedDeadLetterTasks(t, b, st, 1, 0)
	taskID := failedIDs[0]

	// Discard the task.
	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/v1/dead-letter/%s/discard", taskID), nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Verify state is DISCARDED.
	task, _ := st.GetTask(context.Background(), taskID)
	if task.State != broker.TaskStateDiscarded {
		t.Fatalf("expected DISCARDED, got %s", task.State)
	}

	// Attempt to replay → 409.
	req = httptest.NewRequest(http.MethodPost, fmt.Sprintf("/v1/dead-letter/%s/replay", taskID), nil)
	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 for replay of discarded task, got %d", w.Code)
	}

	// Attempt to discard again → 409.
	req = httptest.NewRequest(http.MethodPost, fmt.Sprintf("/v1/dead-letter/%s/discard", taskID), nil)
	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 for re-discard, got %d", w.Code)
	}

	// Verify excluded from default GET /v1/tasks.
	req = httptest.NewRequest(http.MethodGet, "/v1/tasks", nil)
	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	var tasksResp listTasksResponse
	json.Unmarshal(w.Body.Bytes(), &tasksResp)
	for _, tk := range tasksResp.Tasks {
		if tk.ID == taskID {
			t.Fatal("discarded task should not appear in default GET /v1/tasks")
		}
	}

	// Verify included with include_discarded=true.
	req = httptest.NewRequest(http.MethodGet, "/v1/tasks?include_discarded=true", nil)
	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	var allResp listTasksResponse
	json.Unmarshal(w.Body.Bytes(), &allResp)
	found := false
	for _, tk := range allResp.Tasks {
		if tk.ID == taskID {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("discarded task should appear with include_discarded=true")
	}
}

// Test 11: replay-all replays only the target pipeline.
func TestDeadLetterReplayAll(t *testing.T) {
	srv, b, st := newDeadLetterTestServer(t)
	defer srv.Shutdown(context.Background())

	seedDeadLetterTasks(t, b, st, 3, 0)

	req := httptest.NewRequest(http.MethodPost, "/v1/dead-letter/replay-all?pipeline_id=test-pipeline", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", w.Code, w.Body.String())
	}

	var resp replayAllResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Count != 3 {
		t.Fatalf("expected 3 replayed tasks, got %d", resp.Count)
	}
}

// Test 12: replay-all rate limit.
func TestDeadLetterReplayAll_RateLimit(t *testing.T) {
	srv, b, st := newDeadLetterTestServer(t)
	defer srv.Shutdown(context.Background())

	seedDeadLetterTasks(t, b, st, 1, 0)

	// First call succeeds.
	req := httptest.NewRequest(http.MethodPost, "/v1/dead-letter/replay-all?pipeline_id=test-pipeline", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d", w.Code)
	}

	// Second call within 1 minute → 429.
	req = httptest.NewRequest(http.MethodPost, "/v1/dead-letter/replay-all?pipeline_id=test-pipeline", nil)
	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d: %s", w.Code, w.Body.String())
	}
}

// Test: Replay non-dead-lettered task → 409.
func TestDeadLetterReplay_NotDeadLettered(t *testing.T) {
	srv, b, st := newDeadLetterTestServer(t)
	defer srv.Shutdown(context.Background())

	_, successIDs := seedDeadLetterTasks(t, b, st, 0, 1)
	taskID := successIDs[0]

	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/v1/dead-letter/%s/replay", taskID), nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", w.Code, w.Body.String())
	}
}

// Test: discard-all discards only the target pipeline.
func TestDeadLetterDiscardAll(t *testing.T) {
	srv, b, st := newDeadLetterTestServer(t)
	defer srv.Shutdown(context.Background())

	failedIDs, _ := seedDeadLetterTasks(t, b, st, 3, 0)

	req := httptest.NewRequest(http.MethodPost, "/v1/dead-letter/discard-all?pipeline_id=test-pipeline", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp discardAllResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Count != 3 {
		t.Fatalf("expected 3 discarded, got %d", resp.Count)
	}

	// Verify all are DISCARDED.
	for _, id := range failedIDs {
		task, _ := st.GetTask(context.Background(), id)
		if task.State != broker.TaskStateDiscarded {
			t.Fatalf("expected DISCARDED for %s, got %s", id, task.State)
		}
	}
}

// Test 13: Scope enforcement for dead letter endpoints.
func TestDeadLetterEndpoints_ScopeEnforcement(t *testing.T) {
	cfg := testConfig()
	cfg.Auth = config.APIAuthConfig{Enabled: true}

	st := memory.New()
	agents := map[string]broker.Agent{
		"agent-1": &stubAgent{id: "agent-1", provider: "anthropic", healthy: true},
		"agent-2": &stubAgent{id: "agent-2", provider: "openai", healthy: true},
	}
	reg, _ := contract.NewRegistry(nil, "")
	logger := slog.Default()
	b := broker.New(cfg, st, agents, reg, logger, nil, nil)

	// Create API keys with different scopes.
	readHash, _ := bcrypt.GenerateFromPassword([]byte("read-key"), bcrypt.MinCost)
	writeHash, _ := bcrypt.GenerateFromPassword([]byte("write-key"), bcrypt.MinCost)
	adminHash, _ := bcrypt.GenerateFromPassword([]byte("admin-key"), bcrypt.MinCost)

	keys := []auth.APIKey{
		{Name: "reader", HashedKey: readHash, Scopes: auth.ScopeSet{auth.ScopeRead: true}},
		{Name: "writer", HashedKey: writeHash, Scopes: auth.ScopeSet{auth.ScopeWrite: true}},
		{Name: "admin", HashedKey: adminHash, Scopes: auth.ScopeSet{auth.ScopeAdmin: true}},
	}

	srv := NewServerWithContext(context.Background(), b, logger, nil, "", keys)
	defer srv.Shutdown(context.Background())

	// Seed a dead-lettered task.
	task, _ := b.Submit(context.Background(), "test-pipeline", json.RawMessage(`{"input":"test"}`))
	failedState := broker.TaskStateFailed
	dl := true
	st.UpdateTask(context.Background(), task.ID, broker.TaskUpdate{
		State: &failedState, RoutedToDeadLetter: &dl,
	})

	tests := []struct {
		name       string
		method     string
		path       string
		token      string
		wantStatus int
	}{
		// GET /v1/dead-letter requires read scope.
		{"list-read-ok", http.MethodGet, "/v1/dead-letter", "read-key", http.StatusOK},
		{"list-no-auth", http.MethodGet, "/v1/dead-letter", "", http.StatusUnauthorized},

		// POST /v1/dead-letter/{id}/replay requires write scope.
		{"replay-write-ok", http.MethodPost, fmt.Sprintf("/v1/dead-letter/%s/replay", task.ID), "write-key", http.StatusAccepted},
		{"replay-read-forbidden", http.MethodPost, fmt.Sprintf("/v1/dead-letter/%s/replay", task.ID), "read-key", http.StatusForbidden},

		// POST /v1/dead-letter/{id}/discard requires write scope.
		{"discard-write-ok", http.MethodPost, fmt.Sprintf("/v1/dead-letter/%s/discard", task.ID), "write-key", http.StatusOK},
		{"discard-read-forbidden", http.MethodPost, fmt.Sprintf("/v1/dead-letter/%s/discard", task.ID), "read-key", http.StatusForbidden},

		// POST /v1/dead-letter/replay-all requires admin scope.
		{"replay-all-admin-ok", http.MethodPost, "/v1/dead-letter/replay-all?pipeline_id=test-pipeline", "admin-key", http.StatusAccepted},
		{"replay-all-write-forbidden", http.MethodPost, "/v1/dead-letter/replay-all?pipeline_id=test-pipeline", "write-key", http.StatusForbidden},
		{"replay-all-read-forbidden", http.MethodPost, "/v1/dead-letter/replay-all?pipeline_id=test-pipeline", "read-key", http.StatusForbidden},

		// POST /v1/dead-letter/discard-all requires admin scope.
		{"discard-all-admin-ok", http.MethodPost, "/v1/dead-letter/discard-all?pipeline_id=test-pipeline", "admin-key", http.StatusOK},
		{"discard-all-write-forbidden", http.MethodPost, "/v1/dead-letter/discard-all?pipeline_id=test-pipeline", "write-key", http.StatusForbidden},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Re-seed task state before each test that modifies it.
			// Replay/discard may change the task state, so re-set it.
			st.UpdateTask(context.Background(), task.ID, broker.TaskUpdate{
				State: &failedState, RoutedToDeadLetter: &dl,
			})

			req := httptest.NewRequest(tt.method, tt.path, nil)
			if tt.token != "" {
				req.Header.Set("Authorization", "Bearer "+tt.token)
			}
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, req)

			if w.Code != tt.wantStatus {
				t.Errorf("expected status %d, got %d: %s", tt.wantStatus, w.Code, w.Body.String())
			}
		})
	}
}

// Test: replay-all missing pipeline_id → 400.
func TestDeadLetterReplayAll_MissingPipelineID(t *testing.T) {
	srv, _, _ := newDeadLetterTestServer(t)
	defer srv.Shutdown(context.Background())

	req := httptest.NewRequest(http.MethodPost, "/v1/dead-letter/replay-all", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

// Test: discard-all missing pipeline_id → 400.
func TestDeadLetterDiscardAll_MissingPipelineID(t *testing.T) {
	srv, _, _ := newDeadLetterTestServer(t)
	defer srv.Shutdown(context.Background())

	req := httptest.NewRequest(http.MethodPost, "/v1/dead-letter/discard-all", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

// --- Bug fix regression tests ---

// Bug #3 regression: replay-all and discard-all with a nonexistent pipeline_id
// must return 404 instead of silently returning count=0 and leaking a rate-limit
// map entry.
func TestDeadLetterReplayAll_NonexistentPipeline(t *testing.T) {
	srv, _, _ := newDeadLetterTestServer(t)
	defer srv.Shutdown(context.Background())

	req := httptest.NewRequest(http.MethodPost, "/v1/dead-letter/replay-all?pipeline_id=does-not-exist", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for nonexistent pipeline, got %d: %s", w.Code, w.Body.String())
	}

	var resp errorResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Code != "PIPELINE_NOT_FOUND" {
		t.Fatalf("expected PIPELINE_NOT_FOUND code, got %s", resp.Code)
	}
}

func TestDeadLetterDiscardAll_NonexistentPipeline(t *testing.T) {
	srv, _, _ := newDeadLetterTestServer(t)
	defer srv.Shutdown(context.Background())

	req := httptest.NewRequest(http.MethodPost, "/v1/dead-letter/discard-all?pipeline_id=does-not-exist", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for nonexistent pipeline, got %d: %s", w.Code, w.Body.String())
	}
}

// Bug #3 regression: the replayAllLimiter must not grow without bound.
// Expired entries should be pruned, and a hard cap should prevent OOM.
func TestReplayAllLimiter_PrunesExpiredEntries(t *testing.T) {
	limiter := newReplayAllRateLimiter()

	// Manually insert an expired entry (>1 minute ago).
	limiter.mu.Lock()
	limiter.lastCall["old-pipeline"] = time.Now().Add(-2 * time.Minute)
	limiter.mu.Unlock()

	// allow() should prune the expired entry.
	limiter.allow("new-pipeline")

	limiter.mu.Lock()
	_, oldExists := limiter.lastCall["old-pipeline"]
	mapLen := len(limiter.lastCall)
	limiter.mu.Unlock()

	if oldExists {
		t.Fatal("expired entry should have been pruned")
	}
	if mapLen != 1 {
		t.Fatalf("expected 1 entry after pruning, got %d", mapLen)
	}
}

func TestReplayAllLimiter_HardCap(t *testing.T) {
	limiter := newReplayAllRateLimiter()

	// Fill to the cap with non-expired entries.
	limiter.mu.Lock()
	for i := 0; i < maxReplayAllEntries; i++ {
		limiter.lastCall[fmt.Sprintf("pipeline-%d", i)] = time.Now()
	}
	limiter.mu.Unlock()

	// Next call with a new key should be rejected.
	if limiter.allow("one-more-pipeline") {
		t.Fatal("expected rejection when at hard cap")
	}
}
