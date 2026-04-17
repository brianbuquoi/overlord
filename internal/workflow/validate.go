package workflow

import (
	"fmt"
	"regexp"
)

// validID matches the same character class the strict config, chain
// mode, and workflow step IDs use so workflow IDs compile cleanly
// into the strict config without renaming.
var validID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

// Validate checks the workflow's shape before the compiler lowers it
// into a chain. We repeat a light identifier check here so the error
// message names the authored field (step index / id) rather than the
// synthesized chain step. The chain-layer validator runs after the
// compile pass and catches everything else (placeholder references,
// model strings, fixture requirements, etc.).
func Validate(w *Workflow) error {
	if w == nil {
		return fmt.Errorf("workflow is nil")
	}
	if w.ID == "" {
		return fmt.Errorf("workflow.id must not be empty")
	}
	if !validID.MatchString(w.ID) {
		return fmt.Errorf("workflow.id %q contains invalid characters (must match %s)", w.ID, validID.String())
	}
	if len(w.Steps) == 0 {
		return fmt.Errorf("workflow.steps must be non-empty")
	}

	switch w.InputType() {
	case "text", "json":
	default:
		return fmt.Errorf("workflow.input.type %q must be \"text\" or \"json\"", w.InputType())
	}
	switch w.OutputType() {
	case "text", "json":
	default:
		return fmt.Errorf("workflow.output.type %q must be \"text\" or \"json\"", w.OutputType())
	}
	if w.Output != nil && len(w.Output.Schema) > 0 && w.OutputType() != "json" {
		return fmt.Errorf("workflow.output.schema is only valid when workflow.output.type is \"json\"")
	}

	for name := range w.Vars {
		if !validID.MatchString(name) {
			return fmt.Errorf("workflow.vars key %q contains invalid characters", name)
		}
	}

	seen := map[string]int{}
	for i, st := range w.Steps {
		if st.ID != "" {
			if !validID.MatchString(st.ID) {
				return fmt.Errorf("step[%d].id %q contains invalid characters", i, st.ID)
			}
			if prev, dup := seen[st.ID]; dup {
				return fmt.Errorf("step[%d]: duplicate step id %q (already used by step[%d])", i, st.ID, prev)
			}
			seen[st.ID] = i
		}
		if st.Model == "" {
			label := st.ID
			if label == "" {
				label = fmt.Sprintf("step[%d]", i)
			}
			return fmt.Errorf("%s: model must not be empty", label)
		}
		if st.Prompt == "" {
			label := st.ID
			if label == "" {
				label = fmt.Sprintf("step[%d]", i)
			}
			return fmt.Errorf("%s: prompt must not be empty", label)
		}
	}
	return nil
}
