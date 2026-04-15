package broker

import (
	"encoding/json"
	"time"
)

// TaskState represents the lifecycle state of a task.
type TaskState string

const (
	TaskStatePending    TaskState = "PENDING"
	TaskStateRouting    TaskState = "ROUTING"
	TaskStateExecuting  TaskState = "EXECUTING"
	TaskStateValidating TaskState = "VALIDATING"
	TaskStateDone       TaskState = "DONE"
	TaskStateFailed     TaskState = "FAILED"
	TaskStateRetrying   TaskState = "RETRYING"
	TaskStateWaiting    TaskState = "WAITING"
	TaskStateDiscarded  TaskState = "DISCARDED"

	// TaskStateReplayPending is set atomically by ClaimForReplay. The task
	// remains in this state until Submit succeeds (→ REPLAYED) or the handler
	// rolls back (→ FAILED with RoutedToDeadLetter=true). A task stuck in
	// REPLAY_PENDING indicates a double-failure (Submit failed AND
	// RollbackReplayClaim failed).
	//
	// Recovery: POST /v1/tasks/{id}/recover (write scope required), or the
	// equivalent `overlord dead-letter recover --task <id>` CLI command.
	// This transitions the task back to FAILED+RoutedToDeadLetter=true,
	// making it visible in the dead-letter queue and replayable again.
	TaskStateReplayPending TaskState = "REPLAY_PENDING"

	// TaskStateReplayed is the terminal state for a task that was successfully
	// replayed. The original task is preserved for audit; the new task
	// carries the retry.
	TaskStateReplayed TaskState = "REPLAYED"
)

// Task is the unit of work that flows through a pipeline.
type Task struct {
	ID                    string          `json:"id"`
	PipelineID            string          `json:"pipeline_id"`
	StageID               string          `json:"stage_id"`
	InputSchemaName       string          `json:"input_schema_name"`
	InputSchemaVersion    string          `json:"input_schema_version"`
	OutputSchemaName      string          `json:"output_schema_name"`
	OutputSchemaVersion   string          `json:"output_schema_version"`
	Payload               json.RawMessage `json:"payload"`
	Prompt                string          `json:"prompt,omitempty"`
	Metadata              map[string]any  `json:"metadata,omitempty"`
	State                 TaskState       `json:"state"`
	Attempts              int             `json:"attempts"`
	MaxAttempts           int             `json:"max_attempts"`
	CreatedAt             time.Time       `json:"created_at"`
	UpdatedAt             time.Time       `json:"updated_at"`
	ExpiresAt             time.Time       `json:"expires_at,omitempty"`
	RoutedToDeadLetter    bool            `json:"routed_to_dead_letter,omitempty"`
	CrossStageTransitions int             `json:"cross_stage_transitions,omitempty"`
}

// TaskResult holds the output from an agent execution.
type TaskResult struct {
	TaskID   string          `json:"task_id"`
	Payload  json.RawMessage `json:"payload"`
	Metadata map[string]any  `json:"metadata,omitempty"`
	Error    string          `json:"error,omitempty"`
}

// TaskUpdate carries partial updates to apply to a task.
type TaskUpdate struct {
	State                 *TaskState       `json:"state,omitempty"`
	StageID               *string          `json:"stage_id,omitempty"`
	Payload               *json.RawMessage `json:"payload,omitempty"`
	Metadata              map[string]any   `json:"metadata,omitempty"`
	Attempts              *int             `json:"attempts,omitempty"`
	InputSchemaName       *string          `json:"input_schema_name,omitempty"`
	InputSchemaVersion    *string          `json:"input_schema_version,omitempty"`
	OutputSchemaName      *string          `json:"output_schema_name,omitempty"`
	OutputSchemaVersion   *string          `json:"output_schema_version,omitempty"`
	MaxAttempts           *int             `json:"max_attempts,omitempty"`
	RoutedToDeadLetter    *bool            `json:"routed_to_dead_letter,omitempty"`
	CrossStageTransitions *int             `json:"cross_stage_transitions,omitempty"`
}

// TaskFilter constrains which tasks are returned by ListTasks.
type TaskFilter struct {
	PipelineID         *string    `json:"pipeline_id,omitempty"`
	StageID            *string    `json:"stage_id,omitempty"`
	State              *TaskState `json:"state,omitempty"`
	Limit              int        `json:"limit,omitempty"`
	Offset             int        `json:"offset,omitempty"`
	IncludeDiscarded   bool       `json:"include_discarded,omitempty"`
	RoutedToDeadLetter *bool      `json:"routed_to_dead_letter,omitempty"`
}

// ListTasksResult holds both the page of tasks and the total matching count.
type ListTasksResult struct {
	Tasks []*Task `json:"tasks"`
	Total int     `json:"total"`
}
