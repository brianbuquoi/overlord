// Package contract provides JSONSchema-based I/O validation for pipeline stages.
// It enforces strict input/output contracts and schema version compatibility,
// rejecting mismatches before tasks reach agents.
package contract

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// Direction indicates whether a contract violation occurred on input or output.
type Direction string

const (
	Input  Direction = "input"
	Output Direction = "output"
)

// ContractError is returned when a payload fails schema validation.
type ContractError struct {
	SchemaName string
	Version    string
	Direction  Direction
	Failures   []string
}

func (e *ContractError) Error() string {
	return fmt.Sprintf(
		"contract violation (%s) for schema %s@%s: %s",
		e.Direction, e.SchemaName, e.Version,
		strings.Join(e.Failures, "; "),
	)
}

// Validator validates task payloads against registered schemas, enforcing
// version compatibility before schema validation.
type Validator struct {
	registry *Registry
}

// NewValidator creates a Validator backed by the given Registry.
func NewValidator(registry *Registry) *Validator {
	return &Validator{registry: registry}
}

// ValidateInput checks that the payload conforms to the input schema for the
// given name and version. taskVersion is the version the task was created
// under; stageVersion is the version the stage expects. Version compatibility
// is checked first — a mismatch returns ErrVersionMismatch, not a schema error.
func (v *Validator) ValidateInput(name string, taskVersion, stageVersion SchemaVersion, payload json.RawMessage) error {
	return v.validate(name, taskVersion, stageVersion, payload, Input)
}

// ValidateOutput checks that the payload conforms to the output schema.
func (v *Validator) ValidateOutput(name string, taskVersion, stageVersion SchemaVersion, payload json.RawMessage) error {
	return v.validate(name, taskVersion, stageVersion, payload, Output)
}

func (v *Validator) validate(name string, taskVersion, stageVersion SchemaVersion, payload json.RawMessage, dir Direction) error {
	compatible, err := IsCompatible(taskVersion, stageVersion)
	if err != nil {
		return err
	}
	if !compatible {
		return &ErrVersionMismatch{
			TaskVersion:  taskVersion,
			StageVersion: stageVersion,
			SchemaName:   name,
		}
	}

	cs, err := v.registry.Lookup(name, string(stageVersion))
	if err != nil {
		return err
	}

	var value any
	if err := json.Unmarshal(payload, &value); err != nil {
		return &ContractError{
			SchemaName: name,
			Version:    string(stageVersion),
			Direction:  dir,
			Failures:   []string{fmt.Sprintf("invalid JSON: %v", err)},
		}
	}

	validationErr := cs.Schema.Validate(value)
	if validationErr == nil {
		return nil
	}

	ve, ok := validationErr.(*jsonschema.ValidationError)
	if !ok {
		return &ContractError{
			SchemaName: name,
			Version:    string(stageVersion),
			Direction:  dir,
			Failures:   []string{validationErr.Error()},
		}
	}

	return &ContractError{
		SchemaName: name,
		Version:    string(stageVersion),
		Direction:  dir,
		Failures:   collectFailures(ve),
	}
}

func collectFailures(ve *jsonschema.ValidationError) []string {
	if len(ve.Causes) == 0 {
		return []string{ve.Error()}
	}
	var failures []string
	for _, cause := range ve.Causes {
		failures = append(failures, collectFailures(cause)...)
	}
	return failures
}
