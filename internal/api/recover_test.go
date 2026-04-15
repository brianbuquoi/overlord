package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/brianbuquoi/overlord/internal/auth"
	"github.com/brianbuquoi/overlord/internal/broker"
	"github.com/brianbuquoi/overlord/internal/config"
	"github.com/brianbuquoi/overlord/internal/contract"
	"github.com/brianbuquoi/overlord/internal/store/memory"
	"golang.org/x/crypto/bcrypt"
)

// seedReplayPendingTask creates a task and atomically transitions it into
// REPLAY_PENDING via ClaimForReplay — the same path the replay handler takes.
func seedReplayPendingTask(t *testing.T, b *broker.Broker, st *memory.MemoryStore) string {
	t.Helper()
	ctx := context.Background()
	task, err := b.Submit(ctx, "test-pipeline", json.RawMessage(`{"input":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	failed := broker.TaskStateFailed
	dl := true
	if err := st.UpdateTask(ctx, task.ID, broker.TaskUpdate{State: &failed, RoutedToDeadLetter: &dl}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ClaimForReplay(ctx, task.ID); err != nil {
		t.Fatalf("ClaimForReplay: %v", err)
	}
	return task.ID
}

func TestRecoverTask_HappyPath(t *testing.T) {
	srv, b, st := newDeadLetterTestServer(t)
	defer srv.Shutdown(context.Background())

	taskID := seedReplayPendingTask(t, b, st)

	req := httptest.NewRequest(http.MethodPost, "/v1/tasks/"+taskID+"/recover", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200: %s", w.Code, w.Body.String())
	}

	var resp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["task_id"] != taskID || resp["status"] != "recovered" || resp["message"] == "" {
		t.Fatalf("unexpected response: %+v", resp)
	}

	got, err := st.GetTask(context.Background(), taskID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != broker.TaskStateFailed {
		t.Errorf("state: got %s want FAILED", got.State)
	}
	if !got.RoutedToDeadLetter {
		t.Errorf("RoutedToDeadLetter: got false want true")
	}
}

func TestRecoverTask_NotFound(t *testing.T) {
	srv, _, _ := newDeadLetterTestServer(t)
	defer srv.Shutdown(context.Background())

	req := httptest.NewRequest(http.MethodPost, "/v1/tasks/does-not-exist/recover", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status: got %d want 404: %s", w.Code, w.Body.String())
	}
	var resp errorResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Code != "TASK_NOT_FOUND" {
		t.Errorf("code: got %q want TASK_NOT_FOUND", resp.Code)
	}
}

func TestRecoverTask_WrongState(t *testing.T) {
	srv, b, st := newDeadLetterTestServer(t)
	defer srv.Shutdown(context.Background())

	// Seed a FAILED+DL task (not REPLAY_PENDING).
	failedIDs, _ := seedDeadLetterTasks(t, b, st, 1, 0)
	taskID := failedIDs[0]

	req := httptest.NewRequest(http.MethodPost, "/v1/tasks/"+taskID+"/recover", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("status: got %d want 409: %s", w.Code, w.Body.String())
	}
	var resp errorResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Code != "TASK_NOT_REPLAY_PENDING" {
		t.Errorf("code: got %q want TASK_NOT_REPLAY_PENDING", resp.Code)
	}
}

func TestRecoverTask_RequiresWriteScope(t *testing.T) {
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

	readHash, _ := bcrypt.GenerateFromPassword([]byte("read-key"), bcrypt.MinCost)
	writeHash, _ := bcrypt.GenerateFromPassword([]byte("write-key"), bcrypt.MinCost)

	keys := []auth.APIKey{
		{Name: "reader", HashedKey: readHash, Scopes: auth.ScopeSet{auth.ScopeRead: true}},
		{Name: "writer", HashedKey: writeHash, Scopes: auth.ScopeSet{auth.ScopeWrite: true}},
	}

	srv := NewServerWithContext(context.Background(), b, logger, nil, "", keys)
	defer srv.Shutdown(context.Background())

	taskID := seedReplayPendingTask(t, b, st)

	// read-scoped key → 403
	reqRead := httptest.NewRequest(http.MethodPost, "/v1/tasks/"+taskID+"/recover", nil)
	reqRead.Header.Set("Authorization", "Bearer read-key")
	wRead := httptest.NewRecorder()
	srv.Handler().ServeHTTP(wRead, reqRead)
	if wRead.Code != http.StatusForbidden {
		t.Fatalf("read-scope: got %d want 403: %s", wRead.Code, wRead.Body.String())
	}

	// write-scoped key → 200
	reqWrite := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/v1/tasks/%s/recover", taskID), nil)
	reqWrite.Header.Set("Authorization", "Bearer write-key")
	wWrite := httptest.NewRecorder()
	srv.Handler().ServeHTTP(wWrite, reqWrite)
	if wWrite.Code != http.StatusOK {
		t.Fatalf("write-scope: got %d want 200: %s", wWrite.Code, wWrite.Body.String())
	}
}
