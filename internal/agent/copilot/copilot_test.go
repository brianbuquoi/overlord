package copilot

import (
	"context"
	"errors"
	"testing"

	"github.com/orcastrator/orcastrator/internal/agent"
	"github.com/orcastrator/orcastrator/internal/broker"
)

// --- Item 7: Compile-time interface check ---
// This is a compile-time assertion that *Adapter satisfies agent.Agent.
// If *Adapter ever drops a method, this line causes a build failure.
var _ agent.Agent = (*Adapter)(nil)

func TestExecute_ReturnsErrCopilotNotAvailable(t *testing.T) {
	a := New(Config{ID: "test-copilot", Model: "copilot-chat"})

	_, err := a.Execute(context.Background(), &broker.Task{ID: "task-1"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	ae, ok := err.(*agent.AgentError)
	if !ok {
		t.Fatalf("expected *agent.AgentError, got %T: %v", err, err)
	}

	if ae.Retryable {
		t.Error("copilot stub error must be non-retryable")
	}
	if ae.AgentID != "test-copilot" {
		t.Errorf("AgentID: want test-copilot, got %s", ae.AgentID)
	}
	if ae.Prov != "copilot" {
		t.Errorf("Prov: want copilot, got %s", ae.Prov)
	}

	if !errors.Is(ae.Err, ErrCopilotNotAvailable) {
		t.Errorf("underlying error should be ErrCopilotNotAvailable, got: %v", ae.Err)
	}

	expectedMsg := "github copilot agentic API is not yet publicly available"
	if ae.Err.Error() != expectedMsg {
		t.Errorf("error message mismatch.\nwant: %s\ngot:  %s", expectedMsg, ae.Err.Error())
	}
}

func TestHealthCheck_ReturnsErrCopilotNotAvailable(t *testing.T) {
	a := New(Config{ID: "test-copilot", Model: "copilot-chat"})

	err := a.HealthCheck(context.Background())
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	if !errors.Is(err, ErrCopilotNotAvailable) {
		t.Errorf("health check error should wrap ErrCopilotNotAvailable, got: %v", err)
	}
}

// --- Item 8: Stable error message assertion ---

func TestErrCopilotNotAvailable_StableMessage(t *testing.T) {
	// This error message appears in logs and user-facing output.
	// Assert the exact string so any accidental change is caught by CI.
	const expected = "github copilot agentic API is not yet publicly available"

	if ErrCopilotNotAvailable.Error() != expected {
		t.Errorf("ErrCopilotNotAvailable message changed (this is user-facing and must be stable).\nwant: %s\ngot:  %s",
			expected, ErrCopilotNotAvailable.Error())
	}
}

func TestID_And_Provider(t *testing.T) {
	a := New(Config{ID: "my-copilot", Model: "copilot-chat"})

	if a.ID() != "my-copilot" {
		t.Errorf("ID: want my-copilot, got %s", a.ID())
	}
	if a.Provider() != "copilot" {
		t.Errorf("Provider: want copilot, got %s", a.Provider())
	}
}
