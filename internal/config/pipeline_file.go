package config

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// PipelineFile is a standalone pipeline definition file. It can be loaded
// alongside an infra config to define pipelines without modifying the
// main config file.
type PipelineFile struct {
	// Version must match the main config version.
	Version string `yaml:"version"`

	// SchemaRegistry defines schemas used by stages in this file.
	// Merged with any schemas defined in the main config.
	// Conflicts (same name+version) are an error.
	SchemaRegistry []SchemaEntry `yaml:"schema_registry,omitempty"`

	// Pipelines defines one or more pipeline topologies.
	Pipelines []Pipeline `yaml:"pipelines"`
}

// LoadPipelineFile loads and validates a pipeline definition file. It does
// not validate agent bindings — those require the full infra config and
// are checked during MergeInto.
//
// Returns the parsed file alongside the canonical absolute path it was
// loaded from. Callers must thread the returned path into MergeInto so
// relative schema paths can be rebased against the pipeline file's own
// directory rather than the infra config's directory.
func LoadPipelineFile(path string) (*PipelineFile, string, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, "", fmt.Errorf("pipeline file not found: %s", path)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return nil, "", fmt.Errorf("pipeline file must not be a symlink: %s", path)
	}
	if !fi.Mode().IsRegular() {
		return nil, "", fmt.Errorf("pipeline path is not a regular file: %s", path)
	}

	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, "", fmt.Errorf("resolving pipeline file path: %w", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, "", fmt.Errorf("reading pipeline file: %w", err)
	}

	var pf PipelineFile
	if err := yaml.Unmarshal(data, &pf); err != nil {
		errMsg := err.Error()
		if strings.Contains(errMsg, "!!") || strings.Contains(errMsg, "cannot unmarshal") {
			log.Printf("pipeline file parse error detail (debug): %v", err)
			return nil, "", fmt.Errorf("pipeline file parse error in %s: invalid YAML structure", path)
		}
		return nil, "", fmt.Errorf("parsing pipeline file %s: %w", path, err)
	}

	if len(pf.Pipelines) == 0 {
		return nil, "", fmt.Errorf("pipeline file %s: must define at least one pipeline", path)
	}

	return &pf, absPath, nil
}

// MergeInto merges the pipeline file's schemas and pipelines into cfg.
// Returns an error on schema conflicts, duplicate pipeline names, or
// references to agents not defined in the infra config.
//
// pipelineFilePath is the absolute path to the pipeline file (as returned
// by LoadPipelineFile). Relative schema paths in the pipeline file are
// rebased against the pipeline file's directory, then converted to
// absolute paths so they resolve correctly regardless of the infra
// config's location. Absolute paths pass through unchanged. Each rebased
// path is stat-checked so a missing schema produces a clear error here
// (referencing the pipeline file) instead of a cryptic file-not-found at
// schema compile time.
//
// After a successful merge, the Config is re-validated so stage references
// and schema bindings are checked against the combined state.
func (pf *PipelineFile) MergeInto(cfg *Config, pipelineFilePath string) error {
	if pf.Version != "" && cfg.Version != "" && pf.Version != cfg.Version {
		return fmt.Errorf("pipeline file version %q does not match infra config version %q", pf.Version, cfg.Version)
	}

	pipelineDir := filepath.Dir(pipelineFilePath)

	// Index existing schemas by name@version.
	existingSchemas := make(map[string]bool, len(cfg.SchemaRegistry))
	for _, e := range cfg.SchemaRegistry {
		existingSchemas[schemaKey(e.Name, e.Version)] = true
	}
	for _, e := range pf.SchemaRegistry {
		key := schemaKey(e.Name, e.Version)
		if existingSchemas[key] {
			return fmt.Errorf("pipeline file schema %s@%s conflicts with a schema already defined in the infra config", e.Name, e.Version)
		}
		existingSchemas[key] = true

		if !filepath.IsAbs(e.Path) {
			abs, err := filepath.Abs(filepath.Join(pipelineDir, e.Path))
			if err != nil {
				return fmt.Errorf("pipeline file schema %s@%s: cannot resolve path %q relative to pipeline file %s: %w", e.Name, e.Version, e.Path, pipelineFilePath, err)
			}
			e.Path = abs
		}
		if _, err := os.Stat(e.Path); err != nil {
			return fmt.Errorf("pipeline file schema %s@%s: schema file %q (resolved relative to pipeline file %s) is not accessible: %v", e.Name, e.Version, e.Path, pipelineFilePath, err)
		}
		cfg.SchemaRegistry = append(cfg.SchemaRegistry, e)
	}

	// Index existing pipeline names.
	existingPipelines := make(map[string]bool, len(cfg.Pipelines))
	for _, p := range cfg.Pipelines {
		existingPipelines[p.Name] = true
	}

	// Index agents defined in the infra config.
	infraAgents := make(map[string]bool, len(cfg.Agents))
	for _, a := range cfg.Agents {
		infraAgents[a.ID] = true
	}

	for _, p := range pf.Pipelines {
		if existingPipelines[p.Name] {
			return fmt.Errorf("pipeline file defines pipeline %q which already exists in the infra config", p.Name)
		}
		existingPipelines[p.Name] = true

		for _, s := range p.Stages {
			if s.Agent != "" && !infraAgents[s.Agent] {
				return fmt.Errorf("pipeline file error: stage %q references agent %q which is not defined in the infra config", s.ID, s.Agent)
			}
			if s.FanOut != nil {
				for _, fa := range s.FanOut.Agents {
					if !infraAgents[fa.ID] {
						return fmt.Errorf("pipeline file error: stage %q fan_out references agent %q which is not defined in the infra config", s.ID, fa.ID)
					}
				}
			}
		}

		cfg.Pipelines = append(cfg.Pipelines, p)
	}

	// Re-validate the merged config.
	if err := validate(cfg); err != nil {
		return fmt.Errorf("merged config invalid: %w", err)
	}

	return nil
}
