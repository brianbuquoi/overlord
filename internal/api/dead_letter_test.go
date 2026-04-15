package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/brianbuquoi/overlord/internal/auth"
	"github.com/brianbuquoi/overlord/internal/broker"
	"github.com/brianbuquoi/overlord/internal/config"
	"github.com/brianbuquoi/overlord/internal/contract"
	"github.com/brianbuquoi/overlord/internal/store/memory"
	mock "github.com/brianbuquoi/overlord/internal/testutil/storemock"
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

	// After a successful replay, the original task is marked REPLAYED
	// (terminal audit state) with RoutedToDeadLetter cleared.
	origAfter, _ := st.GetTask(context.Background(), taskID)
	if origAfter.State != broker.TaskStateReplayed {
		t.Fatalf("original task should be REPLAYED after successful replay, got %s", origAfter.State)
	}
	if origAfter.RoutedToDeadLetter {
		t.Fatal("original task RoutedToDeadLetter should be cleared after a successful replay")
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

// Test 11: replay-all replays only the target pipeline and leaves each
// original task in its terminal FAILED+dead-lettered state.
func TestDeadLetterReplayAll(t *testing.T) {
	srv, b, st := newDeadLetterTestServer(t)
	defer srv.Shutdown(context.Background())

	failedIDs, _ := seedDeadLetterTasks(t, b, st, 3, 0)

	req := httptest.NewRequest(http.MethodPost, "/v1/dead-letter/replay-all?pipeline_id=test-pipeline", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", w.Code, w.Body.String())
	}

	rawBody := w.Body.String()
	var resp replayAllResponse
	json.Unmarshal([]byte(rawBody), &resp)
	if resp.Processed != 3 {
		t.Fatalf("expected 3 replayed tasks, got %d", resp.Processed)
	}
	if resp.Failed != 0 {
		t.Errorf("expected failed=0, got %d", resp.Failed)
	}
	// The `failed` field must always be present on the wire (no omitempty).
	if !strings.Contains(rawBody, `"failed":0`) {
		t.Errorf(`response body should contain "failed":0 literally: %s`, rawBody)
	}

	for _, id := range failedIDs {
		orig, err := st.GetTask(context.Background(), id)
		if err != nil {
			t.Fatalf("fetch original %s: %v", id, err)
		}
		if orig.State != broker.TaskStateReplayed {
			t.Errorf("original %s: state=%s want REPLAYED after replay-all", id, orig.State)
		}
		if orig.RoutedToDeadLetter {
			t.Errorf("original %s: RoutedToDeadLetter should be cleared after replay-all", id)
		}
	}

	// A second replay-all call (under a fresh rate-limit window) must find
	// zero dead-lettered tasks because the first call's ClaimForReplay flipped
	// RoutedToDeadLetter to false on every claimed task.
	srv.replayAllLimiter = newReplayAllRateLimiter()
	req = httptest.NewRequest(http.MethodPost, "/v1/dead-letter/replay-all?pipeline_id=test-pipeline", nil)
	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("second replay-all: expected 202, got %d: %s", w.Code, w.Body.String())
	}
	var resp2 replayAllResponse
	json.Unmarshal(w.Body.Bytes(), &resp2)
	if resp2.Processed != 0 {
		t.Fatalf("second replay-all: expected 0 claimed tasks, got %d", resp2.Processed)
	}
}

// replay-all must page past the first maxListLimit (1000) tasks.
// Seeds 1050 dead-letter tasks and asserts all are replayed.
func TestDeadLetterReplayAll_BeyondFirstPage(t *testing.T) {
	srv, b, st := newDeadLetterTestServer(t)
	defer srv.Shutdown(context.Background())

	const seeded = 1050
	seedDeadLetterTasks(t, b, st, seeded, 0)

	req := httptest.NewRequest(http.MethodPost, "/v1/dead-letter/replay-all?pipeline_id=test-pipeline", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", w.Code, w.Body.String())
	}

	var resp replayAllResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Processed != seeded {
		t.Fatalf("expected all %d tasks replayed, got %d", seeded, resp.Processed)
	}
	if resp.Truncated {
		t.Fatalf("did not expect truncation at %d tasks, got truncated=true", seeded)
	}
}

// discard-all must page past the first maxListLimit (1000) tasks.
func TestDeadLetterDiscardAll_BeyondFirstPage(t *testing.T) {
	srv, b, st := newDeadLetterTestServer(t)
	defer srv.Shutdown(context.Background())

	const seeded = 1050
	seedDeadLetterTasks(t, b, st, seeded, 0)

	req := httptest.NewRequest(http.MethodPost, "/v1/dead-letter/discard-all?pipeline_id=test-pipeline", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp discardAllResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Processed != seeded {
		t.Fatalf("expected all %d tasks discarded, got %d", seeded, resp.Processed)
	}
	if resp.Truncated {
		t.Fatalf("did not expect truncation at %d tasks, got truncated=true", seeded)
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

// Concurrent replay requests for the same dead-lettered task must produce
// exactly one winner. ClaimForReplay atomically flips RoutedToDeadLetter,
// so N-1 concurrent callers see 409 Conflict once the winner's flip lands.
// Exactly one new task is submitted into the pipeline — operators cannot
// accidentally flood the pipeline by double-clicking or scripting retries.
func TestReplayDeadLetter_Concurrent(t *testing.T) {
	srv, b, st := newDeadLetterTestServer(t)
	defer srv.Shutdown(context.Background())

	failedIDs, _ := seedDeadLetterTasks(t, b, st, 1, 0)
	taskID := failedIDs[0]

	const N = 20
	var wg sync.WaitGroup
	codes := make([]int, N)
	newIDs := make([]string, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodPost, "/v1/dead-letter/"+taskID+"/replay", nil)
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, req)
			codes[i] = w.Code
			if w.Code == http.StatusAccepted {
				var resp replayResponse
				_ = json.Unmarshal(w.Body.Bytes(), &resp)
				newIDs[i] = resp.TaskID
			}
		}(i)
	}
	wg.Wait()

	accepted, conflict, other := 0, 0, 0
	unique := make(map[string]struct{})
	for i, c := range codes {
		switch c {
		case http.StatusAccepted:
			accepted++
			if newIDs[i] == "" {
				t.Errorf("accepted replay %d missing new task ID", i)
			}
			if newIDs[i] == taskID {
				t.Errorf("replay %d returned the original task ID %s", i, taskID)
			}
			unique[newIDs[i]] = struct{}{}
		case http.StatusConflict:
			conflict++
		default:
			other++
		}
	}
	if accepted != 1 || conflict != N-1 || other != 0 {
		t.Fatalf("codes: accepted=%d conflict=%d other=%d; want 1/%d/0", accepted, conflict, N-1, other)
	}
	if len(unique) != 1 {
		t.Fatalf("expected exactly 1 new task ID, got %d", len(unique))
	}

	orig, err := st.GetTask(context.Background(), taskID)
	if err != nil {
		t.Fatalf("fetch original: %v", err)
	}
	if orig.State != broker.TaskStateReplayed {
		t.Fatalf("original task state after concurrent replays: got %s, want REPLAYED", orig.State)
	}
	if orig.RoutedToDeadLetter {
		t.Fatal("original task RoutedToDeadLetter should be cleared after a successful replay")
	}

	// Exactly one new task was created: the store should hold the original
	// FAILED task plus exactly one new PENDING task for that payload.
	ctx := context.Background()
	result, err := st.ListTasks(ctx, broker.TaskFilter{Limit: 100})
	if err != nil {
		t.Fatalf("list tasks: %v", err)
	}
	if result.Total != 2 {
		t.Fatalf("expected 2 tasks in store (original + 1 replay), got %d", result.Total)
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

	rawBody := w.Body.String()
	var resp discardAllResponse
	json.Unmarshal([]byte(rawBody), &resp)
	if resp.Processed != 3 {
		t.Fatalf("expected 3 discarded, got %d", resp.Processed)
	}
	if resp.Failed != 0 {
		t.Errorf("expected failed=0, got %d", resp.Failed)
	}
	if !strings.Contains(rawBody, `"failed":0`) {
		t.Errorf(`response body should contain "failed":0 literally: %s`, rawBody)
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

// newDeadLetterTestServerWithStore builds a test server backed by an
// arbitrary broker.Store (e.g. the mock). Seeding helpers that target the
// memory-backed tasks keep working because the mock delegates to memory by
// default.
func newDeadLetterTestServerWithStore(t *testing.T, st broker.Store, logger *slog.Logger) (*Server, *broker.Broker) {
	t.Helper()
	cfg := testConfig()
	agents := map[string]broker.Agent{
		"agent-1": &stubAgent{id: "agent-1", provider: "anthropic", healthy: true},
		"agent-2": &stubAgent{id: "agent-2", provider: "openai", healthy: true},
	}
	reg, _ := contract.NewRegistry(nil, "")
	b := broker.New(cfg, st, agents, reg, logger, nil, nil)
	srv := NewServer(b, logger, nil, "")
	return srv, b
}

// seedDeadLetterTasksInStore seeds the mock store with dead-lettered tasks
// via the broker and the mock's memory backend. Returns the seeded task IDs.
func seedDeadLetterTasksInStore(t *testing.T, b *broker.Broker, m *memory.MemoryStore, n int) []string {
	t.Helper()
	ctx := context.Background()
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		task, err := b.Submit(ctx, "test-pipeline", json.RawMessage(fmt.Sprintf(`{"input":"fail-%d"}`, i)))
		if err != nil {
			t.Fatal(err)
		}
		state := broker.TaskStateFailed
		dl := true
		if err := m.UpdateTask(ctx, task.ID, broker.TaskUpdate{State: &state, RoutedToDeadLetter: &dl}); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, task.ID)
	}
	return ids
}

// TestReplayAll_PerTaskFailure verifies the replay-all handler rolls back
// failed submits so the original tasks are recoverable, and marks successful
// ones as REPLAYED. An injected store persistently fails the submits for
// payloads "fail-0" and "fail-1"; the remaining three payloads succeed.
func TestReplayAll_PerTaskFailure(t *testing.T) {
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	mstore := mock.New()
	srv, b := newDeadLetterTestServerWithStore(t, mstore, logger)
	defer srv.Shutdown(context.Background())

	ids := seedDeadLetterTasksInStore(t, b, mstore.Memory(), 5)
	if len(ids) != 5 {
		t.Fatalf("seeded %d tasks, want 5", len(ids))
	}

	// Match by payload content — Submit generates fresh task IDs, but the
	// payload is preserved from the original dead-letter task, so failing on
	// payload gives us a persistent per-original-task failure across retries.
	mem := mstore.Memory()
	mstore.OnEnqueueTask = func(ctx context.Context, stageID string, task *broker.Task) error {
		payload := string(task.Payload)
		if strings.Contains(payload, `"fail-0"`) || strings.Contains(payload, `"fail-1"`) {
			return mock.ErrInjected
		}
		return mem.EnqueueTask(ctx, stageID, task)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/dead-letter/replay-all?pipeline_id=test-pipeline", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("status: got %d want 202: %s", w.Code, w.Body.String())
	}
	var resp replayAllResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Processed != 3 {
		t.Errorf("processed: got %d want 3", resp.Processed)
	}
	if resp.Failed != 2 {
		t.Errorf("failed: got %d want 2", resp.Failed)
	}
	if resp.Truncated {
		t.Errorf("truncated: got true want false")
	}

	// Each failing task ID must appear in the Warn log exactly once —
	// confirming the failedIDs guard prevented retries across pages.
	for i := 0; i <= 1; i++ {
		id := ids[i]
		occurrences := strings.Count(logBuf.String(), `"task_id":"`+id+`"`)
		if occurrences != 1 {
			t.Errorf("failing task %s logged %d times, want 1", id, occurrences)
		}
	}

	// The 3 successful originals must be REPLAYED; the 2 failing originals
	// must be rolled back to FAILED+RoutedToDeadLetter=true.
	replayed, rolledBack := 0, 0
	ctx := context.Background()
	for i, id := range ids {
		tk, err := mem.GetTask(ctx, id)
		if err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
		expectFail := i == 0 || i == 1
		switch {
		case expectFail:
			if tk.State == broker.TaskStateFailed && tk.RoutedToDeadLetter {
				rolledBack++
			} else {
				t.Errorf("task %d: expected rolled back to FAILED+DL=true, got state=%s dl=%v",
					i, tk.State, tk.RoutedToDeadLetter)
			}
		default:
			if tk.State == broker.TaskStateReplayed {
				replayed++
			} else {
				t.Errorf("task %d: expected REPLAYED, got state=%s", i, tk.State)
			}
		}
	}
	if replayed != 3 {
		t.Errorf("replayed count: got %d want 3", replayed)
	}
	if rolledBack != 2 {
		t.Errorf("rolled back count: got %d want 2", rolledBack)
	}

	// Exactly 2 Warn lines for the failed submits — one per failing task, no retries.
	warnCount := strings.Count(logBuf.String(), `"msg":"replay-all: failed to submit replay task"`)
	if warnCount != 2 {
		t.Errorf("replay-all warn log lines: got %d want 2 (logs: %s)", warnCount, logBuf.String())
	}
}

// TestReplayAll_RollbackDoesNotInflateCount verifies that when a failed
// submit rolls back and the task reappears on subsequent pages, replay-all's
// failedIDs set prevents retrying and double-counting the task.
func TestReplayAll_RollbackDoesNotInflateCount(t *testing.T) {
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	mstore := mock.New()
	srv, b := newDeadLetterTestServerWithStore(t, mstore, logger)
	defer srv.Shutdown(context.Background())

	ids := seedDeadLetterTasksInStore(t, b, mstore.Memory(), 3)
	if len(ids) != 3 {
		t.Fatalf("seeded %d tasks, want 3", len(ids))
	}
	failingID := ids[0]

	mem := mstore.Memory()
	mstore.OnEnqueueTask = func(ctx context.Context, stageID string, task *broker.Task) error {
		if strings.Contains(string(task.Payload), `"fail-0"`) {
			return mock.ErrInjected
		}
		return mem.EnqueueTask(ctx, stageID, task)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/dead-letter/replay-all?pipeline_id=test-pipeline", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("status: got %d want 202: %s", w.Code, w.Body.String())
	}
	var resp replayAllResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Processed != 2 {
		t.Errorf("processed: got %d want 2", resp.Processed)
	}
	if resp.Failed != 1 {
		t.Errorf("failed: got %d want 1 (distinct failing task)", resp.Failed)
	}
	if resp.Truncated {
		t.Errorf("truncated: got true want false")
	}

	// Failing task should be logged exactly once — the failedIDs guard
	// prevents retries on subsequent page fetches.
	occurrences := strings.Count(logBuf.String(), `"task_id":"`+failingID+`"`)
	if occurrences != 1 {
		t.Errorf("failing task %s logged %d times, want 1 (logs: %s)", failingID, occurrences, logBuf.String())
	}

	ctx := context.Background()
	fail, err := mem.GetTask(ctx, failingID)
	if err != nil {
		t.Fatal(err)
	}
	if fail.State != broker.TaskStateFailed || !fail.RoutedToDeadLetter {
		t.Errorf("failing task: got state=%s dl=%v; want FAILED+DL=true",
			fail.State, fail.RoutedToDeadLetter)
	}

	for _, id := range ids[1:] {
		tk, err := mem.GetTask(ctx, id)
		if err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
		if tk.State != broker.TaskStateReplayed {
			t.Errorf("task %s: got state=%s want REPLAYED", id, tk.State)
		}
	}
}

// TestReplayAll_SubmitAndRollbackFail verifies that when BOTH submit and
// rollback fail, the handler emits an Error log with submit_error and
// rollback_error so an operator can manually recover the stranded task.
func TestReplayAll_SubmitAndRollbackFail(t *testing.T) {
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	mstore := mock.New()
	srv, b := newDeadLetterTestServerWithStore(t, mstore, logger)
	defer srv.Shutdown(context.Background())

	ids := seedDeadLetterTasksInStore(t, b, mstore.Memory(), 1)
	if len(ids) != 1 {
		t.Fatalf("seeded %d tasks, want 1", len(ids))
	}
	mem := mstore.Memory()
	mstore.OnEnqueueTask = func(ctx context.Context, stageID string, task *broker.Task) error {
		if strings.Contains(string(task.Payload), `"fail-0"`) {
			return mock.ErrInjected
		}
		return mem.EnqueueTask(ctx, stageID, task)
	}
	mstore.OnRollbackReplayClaim = func(ctx context.Context, taskID string) error {
		return mock.ErrInjected
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/dead-letter/replay-all?pipeline_id=test-pipeline", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("status: got %d want 202: %s", w.Code, w.Body.String())
	}

	logs := logBuf.String()
	if !strings.Contains(logs, `"msg":"replay-all rollback failed: task may be stranded in REPLAY_PENDING"`) {
		t.Errorf("expected rollback-failed Error log, got: %s", logs)
	}
	if !strings.Contains(logs, `"submit_error"`) || !strings.Contains(logs, `"rollback_error"`) {
		t.Errorf("expected submit_error and rollback_error in log, got: %s", logs)
	}
	if !strings.Contains(logs, `"task_id":"`+ids[0]+`"`) {
		t.Errorf("expected task_id %q in log, got: %s", ids[0], logs)
	}
}

// TestReplayDeadLetter_SubmitFails verifies single-task replay rolls back to
// FAILED+RoutedToDeadLetter=true when Submit fails, leaving the task visible
// and recoverable by subsequent replay operations.
func TestReplayDeadLetter_SubmitFails(t *testing.T) {
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	mstore := mock.New()
	srv, b := newDeadLetterTestServerWithStore(t, mstore, logger)
	defer srv.Shutdown(context.Background())

	ids := seedDeadLetterTasksInStore(t, b, mstore.Memory(), 1)
	taskID := ids[0]

	mem := mstore.Memory()
	mstore.OnEnqueueTask = func(ctx context.Context, stageID string, task *broker.Task) error {
		// Fail only the replay submission: the task we're re-submitting has
		// the payload of the dead-letter task ("fail-0").
		if strings.Contains(string(task.Payload), `"fail-0"`) {
			return mock.ErrInjected
		}
		return mem.EnqueueTask(ctx, stageID, task)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/dead-letter/"+taskID+"/replay", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d want 500: %s", w.Code, w.Body.String())
	}

	// Task must be rolled back.
	tk, err := mem.GetTask(context.Background(), taskID)
	if err != nil {
		t.Fatal(err)
	}
	if tk.State != broker.TaskStateFailed {
		t.Errorf("state after failed submit: got %s want FAILED", tk.State)
	}
	if !tk.RoutedToDeadLetter {
		t.Errorf("RoutedToDeadLetter after failed submit: got false want true")
	}

	// Task must be replayable again.
	req2 := httptest.NewRequest(http.MethodPost, "/v1/dead-letter/"+taskID+"/replay", nil)
	w2 := httptest.NewRecorder()
	// Lift the injected failure so the retry can succeed.
	mstore.OnEnqueueTask = nil
	srv.Handler().ServeHTTP(w2, req2)
	if w2.Code != http.StatusAccepted {
		t.Fatalf("retry: got %d want 202: %s", w2.Code, w2.Body.String())
	}
}

// TestReplayDeadLetter_SubmitAndRollbackFail verifies the Error log contains
// both submit_error and rollback_error when both fail.
func TestReplayDeadLetter_SubmitAndRollbackFail(t *testing.T) {
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	mstore := mock.New()
	srv, b := newDeadLetterTestServerWithStore(t, mstore, logger)
	defer srv.Shutdown(context.Background())

	ids := seedDeadLetterTasksInStore(t, b, mstore.Memory(), 1)
	taskID := ids[0]

	mem := mstore.Memory()
	mstore.OnEnqueueTask = func(ctx context.Context, stageID string, task *broker.Task) error {
		if strings.Contains(string(task.Payload), `"fail-0"`) {
			return mock.ErrInjected
		}
		return mem.EnqueueTask(ctx, stageID, task)
	}
	mstore.OnRollbackReplayClaim = func(ctx context.Context, taskID string) error {
		return mock.ErrInjected
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/dead-letter/"+taskID+"/replay", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d want 500", w.Code)
	}

	logs := logBuf.String()
	if !strings.Contains(logs, `"msg":"replay double-failure: task stranded in REPLAY_PENDING — recover via POST /v1/tasks/{id}/recover"`) {
		t.Errorf("expected stranded-task Error log, got: %s", logs)
	}
	if !strings.Contains(logs, `"submit_error"`) || !strings.Contains(logs, `"rollback_error"`) {
		t.Errorf("expected submit_error and rollback_error in log, got: %s", logs)
	}
	if !strings.Contains(logs, `"task_id":"`+taskID+`"`) {
		t.Errorf("expected task_id in log, got: %s", logs)
	}
}

// TestDiscardAll_PerTaskFailure verifies the discard-all handler reports
// per-task update failures in `failed` and continues processing.
func TestDiscardAll_PerTaskFailure(t *testing.T) {
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	mstore := mock.New()
	srv, b := newDeadLetterTestServerWithStore(t, mstore, logger)
	defer srv.Shutdown(context.Background())

	ids := seedDeadLetterTasksInStore(t, b, mstore.Memory(), 5)
	if len(ids) != 5 {
		t.Fatalf("seeded %d tasks, want 5", len(ids))
	}

	// Pick 2 specific task IDs to fail on UpdateTask (only the discard
	// transition: we filter on the target state).
	failSet := map[string]struct{}{ids[0]: {}, ids[1]: {}}
	mem := mstore.Memory()
	mstore.OnUpdateTask = func(ctx context.Context, taskID string, update broker.TaskUpdate) error {
		if update.State != nil && *update.State == broker.TaskStateDiscarded {
			if _, bad := failSet[taskID]; bad {
				return mock.ErrInjected
			}
		}
		return mem.UpdateTask(ctx, taskID, update)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/dead-letter/discard-all?pipeline_id=test-pipeline", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200: %s", w.Code, w.Body.String())
	}
	var resp discardAllResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Processed != 3 {
		t.Errorf("processed: got %d want 3", resp.Processed)
	}
	// Handler tracks failedIDs so tasks that reappear on subsequent pages
	// (because a failed discard does not remove them from the filter) are
	// not retried. failed is the count of distinct failing IDs.
	if resp.Failed != 2 {
		t.Errorf("failed: got %d want 2", resp.Failed)
	}
	if resp.Truncated {
		t.Errorf("truncated: got true want false")
	}

	// Each failing task ID must be logged exactly once — the failedIDs guard
	// prevents a retry that would emit a second warn line.
	warnCount := strings.Count(logBuf.String(), `"msg":"discard-all: failed to discard task"`)
	if warnCount != 2 {
		t.Errorf("discard-all warn log lines: got %d want 2 (logs: %s)", warnCount, logBuf.String())
	}
	for id := range failSet {
		occurrences := strings.Count(logBuf.String(), `"task_id":"`+id+`"`)
		if occurrences != 1 {
			t.Errorf("failed task %s: warn log occurrences got %d want 1", id, occurrences)
		}
	}
}
