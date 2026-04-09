package contract

import (
	"fmt"
	"strings"
)

// SchemaVersion is a version string like "v1", "v1.2", etc.
type SchemaVersion string

// MajorVersion extracts the major version component.
// "v1" → "v1", "v1.2" → "v1", "v2.3.1" → "v2".
func MajorVersion(v SchemaVersion) string {
	s := string(v)
	if i := strings.Index(s, "."); i >= 0 {
		return s[:i]
	}
	return s
}

// ErrEmptyVersion is returned when a version string is empty.
var ErrEmptyVersion = fmt.Errorf("schema version must not be empty")

// IsCompatible returns true if the task's schema version is compatible with
// the stage's expected version. Compatibility means same major version.
// Returns an error if either version is empty.
func IsCompatible(taskVersion, stageVersion SchemaVersion) (bool, error) {
	if taskVersion == "" || stageVersion == "" {
		return false, ErrEmptyVersion
	}
	return MajorVersion(taskVersion) == MajorVersion(stageVersion), nil
}

// ErrVersionMismatch is returned when a task's schema version is incompatible
// with the stage's expected version.
type ErrVersionMismatch struct {
	TaskVersion  SchemaVersion
	StageVersion SchemaVersion
	SchemaName   string
}

func (e *ErrVersionMismatch) Error() string {
	return fmt.Sprintf(
		"schema version mismatch for %q: task has %s, stage expects %s (major versions differ: %s vs %s)",
		e.SchemaName,
		e.TaskVersion, e.StageVersion,
		MajorVersion(e.TaskVersion), MajorVersion(e.StageVersion),
	)
}
