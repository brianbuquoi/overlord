package workflow

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Load reads a workflow YAML file from path and returns the parsed
// workflow. The file must have a top-level `workflow:` block — files
// that look like strict pipeline configs (schema_registry / pipelines
// at the top level) are rejected so the product's workflow-first
// story stays sharp.
//
// The workflow's ID is left as-authored; callers that need a default
// (e.g. the base name of the file) should call DefaultID after load.
func Load(path string) (*File, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("workflow file not found: %s", path)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("workflow file must not be a symlink: %s", path)
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("workflow path is not a regular file: %s", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading workflow file: %w", err)
	}
	return Parse(data, path)
}

// Parse parses workflow YAML bytes. source is used for error messages
// and may be any human-readable label.
func Parse(data []byte, source string) (*File, error) {
	var file File
	if err := yaml.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("parse workflow %s: %w", source, err)
	}
	if file.Version == "" {
		return nil, fmt.Errorf("workflow %s: missing version field", source)
	}
	if file.Version != "1" {
		return nil, fmt.Errorf("workflow %s: unsupported version %q (expected \"1\")", source, file.Version)
	}
	if file.Workflow == nil {
		return nil, fmt.Errorf("workflow %s: missing `workflow:` block", source)
	}
	if file.Workflow.ID == "" {
		file.Workflow.ID = defaultIDFromSource(source)
	}
	if err := Validate(file.Workflow); err != nil {
		return nil, fmt.Errorf("workflow %s: %w", source, err)
	}
	return &file, nil
}

// defaultIDFromSource derives a workflow ID from a filesystem source.
// For non-path sources (bytes without a filename) it falls back to
// "workflow".
func defaultIDFromSource(source string) string {
	base := filepath.Base(source)
	if base == "" || base == "." || base == "/" {
		return "workflow"
	}
	ext := filepath.Ext(base)
	if ext != "" {
		base = strings.TrimSuffix(base, ext)
	}
	// Normalize to the shared identifier character class: first char must
	// be alnum, subsequent chars may include `.`, `-`, `_`.
	var b strings.Builder
	for i, r := range base {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
		case r == '_' || r == '-' || r == '.':
			if i == 0 {
				// Skip leading separators so the result still starts
				// with an alphanumeric character.
				continue
			}
			b.WriteRune(r)
		}
	}
	out := b.String()
	if out == "" {
		return "workflow"
	}
	return out
}

// IsWorkflowShape returns true when the YAML bytes declare a top-level
// `workflow:` block. Used by `overlord run` / `overlord serve` /
// `overlord export` to route to the workflow path when the config file
// is a workflow rather than a strict pipeline config.
//
// The check is intentionally structural: we only look for the
// `workflow:` key at the document root. A workflow file that also
// carries stray pipeline-only keys still detects as a workflow so the
// caller surfaces a clean "cannot mix" error rather than silently
// running the strict-pipeline code path.
func IsWorkflowShape(data []byte) bool {
	var probe struct {
		Workflow any `yaml:"workflow"`
	}
	if err := yaml.Unmarshal(data, &probe); err != nil {
		return false
	}
	return probe.Workflow != nil
}

// IsWorkflowFile returns true when the file at path carries a
// `workflow:` block. Non-existent files and read errors return false
// so callers can fall back to the strict-pipeline path without
// treating a typo as a workflow.
func IsWorkflowFile(path string) bool {
	fi, err := os.Lstat(path)
	if err != nil {
		return false
	}
	if !fi.Mode().IsRegular() || fi.Mode()&os.ModeSymlink != 0 {
		return false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return IsWorkflowShape(data)
}
