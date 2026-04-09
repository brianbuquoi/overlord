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
)

// Task is the unit of work that flows through a pipeline.
type Task struct {
	ID                  string          `json:"id"`
	PipelineID          string          `json:"pipeline_id"`
	StageID             string          `json:"stage_id"`
	InputSchemaName     string          `json:"input_schema_name"`
	InputSchemaVersion  string          `json:"input_schema_version"`
	OutputSchemaName    string          `json:"output_schema_name"`
	OutputSchemaVersion string          `json:"output_schema_version"`
	Payload             json.RawMessage `json:"payload"`
	Metadata            map[string]any  `json:"metadata,omitempty"`
	State               TaskState       `json:"state"`
	Attempts            int             `json:"attempts"`
	MaxAttempts         int             `json:"max_attempts"`
	CreatedAt           time.Time       `json:"created_at"`
	UpdatedAt           time.Time       `json:"updated_at"`
	ExpiresAt           time.Time       `json:"expires_at,omitempty"`
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
	State               *TaskState       `json:"state,omitempty"`
	StageID             *string          `json:"stage_id,omitempty"`
	Payload             *json.RawMessage `json:"payload,omitempty"`
	Metadata            map[string]any   `json:"metadata,omitempty"`
	Attempts            *int             `json:"attempts,omitempty"`
	InputSchemaName     *string          `json:"input_schema_name,omitempty"`
	InputSchemaVersion  *string          `json:"input_schema_version,omitempty"`
	OutputSchemaName    *string          `json:"output_schema_name,omitempty"`
	OutputSchemaVersion *string          `json:"output_schema_version,omitempty"`
	MaxAttempts         *int             `json:"max_attempts,omitempty"`
}

// TaskFilter constrains which tasks are returned by ListTasks.
type TaskFilter struct {
	PipelineID *string    `json:"pipeline_id,omitempty"`
	StageID    *string    `json:"stage_id,omitempty"`
	State      *TaskState `json:"state,omitempty"`
	Limit      int        `json:"limit,omitempty"`
	Offset     int        `json:"offset,omitempty"`
}

// ListTasksResult holds both the page of tasks and the total matching count.
type ListTasksResult struct {
	Tasks []*Task `json:"tasks"`
	Total int     `json:"total"`
}
