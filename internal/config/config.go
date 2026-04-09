// Package config handles YAML pipeline configuration loading, validation,
// and hot-reload. Config files are the single source of truth for pipeline
// topology, agent bindings, schema references, and retry policies.
package config

import (
	"fmt"
	"log"
	"os"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// validID matches safe identifiers for pipeline names, stage IDs, and agent IDs.
// Prevents Redis key collisions (colon), broker stageKey collisions (slash),
// and other injection vectors (null bytes, spaces, control chars).
var validID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

// validateID checks that an identifier matches the safe character set.
func validateID(kind, id string) error {
	if id == "" {
		return fmt.Errorf("%s must not be empty", kind)
	}
	if !validID.MatchString(id) {
		return fmt.Errorf("%s %q contains invalid characters (must match %s)", kind, id, validID.String())
	}
	return nil
}

// ValidateIDExported is the exported version of validateID for testing.
func ValidateIDExported(kind, id string) error {
	return validateID(kind, id)
}

// Load reads a YAML config file and returns a validated Config.
func Load(path string) (*Config, error) {
	// SEC2-NEW-001: Reject symlinks and non-regular files to prevent
	// file content leaks via YAML parse error messages.
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("config file not found: %s", path)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("config file must not be a symlink: %s", path)
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("config path is not a regular file: %s", path)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config file: %w", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		// SEC2-NEW-001: Strip raw file content from YAML error messages.
		// yaml.Unmarshal errors that contain "!!str", "!!int", etc. include
		// raw values from the file — sanitize those completely. Other errors
		// (e.g. from custom UnmarshalYAML) are safe to pass through.
		errMsg := err.Error()
		if strings.Contains(errMsg, "!!") || strings.Contains(errMsg, "cannot unmarshal") {
			log.Printf("config parse error detail (debug): %v", err)
			return nil, fmt.Errorf("config parse error in %s: invalid YAML structure", path)
		}
		return nil, fmt.Errorf("parsing config file %s: %w", path, err)
	}

	if err := validate(&cfg); err != nil {
		return nil, err
	}

	return &cfg, nil
}

func validate(cfg *Config) error {
	agentIDs, err := collectAgentIDs(cfg.Agents)
	if err != nil {
		return err
	}

	schemaIndex, err := collectSchemaRegistry(cfg.SchemaRegistry)
	if err != nil {
		return err
	}

	if err := validateAuth(cfg.Auth); err != nil {
		return err
	}

	pipelineNames := make(map[string]bool)
	for _, p := range cfg.Pipelines {
		if err := validateID("pipeline name", p.Name); err != nil {
			return err
		}
		if pipelineNames[p.Name] {
			return fmt.Errorf("duplicate pipeline name: %q", p.Name)
		}
		pipelineNames[p.Name] = true

		if err := validatePipeline(p, agentIDs, schemaIndex); err != nil {
			return fmt.Errorf("pipeline %q: %w", p.Name, err)
		}
	}

	return nil
}

func collectAgentIDs(agents []Agent) (map[string]bool, error) {
	ids := make(map[string]bool, len(agents))
	for _, a := range agents {
		if err := validateID("agent ID", a.ID); err != nil {
			return nil, err
		}
		if ids[a.ID] {
			return nil, fmt.Errorf("duplicate agent ID: %q", a.ID)
		}
		ids[a.ID] = true
	}
	return ids, nil
}

// schemaKey returns a unique key for a schema registry entry.
func schemaKey(name, version string) string {
	return name + "@" + version
}

func collectSchemaRegistry(entries []SchemaEntry) (map[string]string, error) {
	// full maps name@version -> path for stage lookups.
	// Multiple versions of the same name are allowed — that's the point of
	// schema versioning. We only reject exact duplicates (same name + version).
	full := make(map[string]string, len(entries))
	// nameVersions tracks all registered versions per name for error messages.
	nameVersions := make(map[string][]string, len(entries))

	for _, e := range entries {
		key := schemaKey(e.Name, e.Version)
		if _, exists := full[key]; exists {
			return nil, fmt.Errorf("duplicate schema registry entry: %s@%s", e.Name, e.Version)
		}
		full[key] = e.Path
		nameVersions[e.Name] = append(nameVersions[e.Name], e.Version)
	}

	// Store version lists for error messages in validateSchemaRef.
	for name, versions := range nameVersions {
		full["__versions__:"+name] = fmt.Sprintf("%v", versions)
	}

	return full, nil
}

func validatePipeline(p Pipeline, agentIDs map[string]bool, schemaIndex map[string]string) error {
	if len(p.Stages) == 0 {
		return fmt.Errorf("pipeline must have at least one stage")
	}

	stageIDs := make(map[string]bool, len(p.Stages))
	for _, s := range p.Stages {
		if err := validateID("stage ID", s.ID); err != nil {
			return err
		}
		if stageIDs[s.ID] {
			return fmt.Errorf("duplicate stage ID: %q", s.ID)
		}
		stageIDs[s.ID] = true
	}

	for _, s := range p.Stages {
		// A stage must have exactly one of: agent or fan_out.
		hasAgent := s.Agent != ""
		hasFanOut := s.FanOut != nil
		if hasAgent && hasFanOut {
			return fmt.Errorf("stage %q: must have either agent or fan_out, not both", s.ID)
		}
		if !hasAgent && !hasFanOut {
			return fmt.Errorf("stage %q: must have either agent or fan_out", s.ID)
		}

		if hasAgent {
			if !agentIDs[s.Agent] {
				return fmt.Errorf("stage %q references unknown agent: %q", s.ID, s.Agent)
			}
		}

		if hasFanOut {
			if err := validateFanOut(s.ID, s.FanOut, agentIDs); err != nil {
				return fmt.Errorf("stage %q: %w", s.ID, err)
			}
		}

		// aggregate_schema is required when fan_out is present, forbidden otherwise.
		if hasFanOut && s.AggregateSchema == nil {
			return fmt.Errorf("stage %q: aggregate_schema is required when fan_out is present", s.ID)
		}
		if !hasFanOut && s.AggregateSchema != nil {
			return fmt.Errorf("stage %q: aggregate_schema is only allowed with fan_out stages", s.ID)
		}

		if err := validateSchemaRef("input_schema", s.InputSchema, schemaIndex); err != nil {
			return fmt.Errorf("stage %q: %w", s.ID, err)
		}
		if err := validateSchemaRef("output_schema", s.OutputSchema, schemaIndex); err != nil {
			return fmt.Errorf("stage %q: %w", s.ID, err)
		}
		if s.AggregateSchema != nil {
			if err := validateSchemaRef("aggregate_schema", *s.AggregateSchema, schemaIndex); err != nil {
				return fmt.Errorf("stage %q: %w", s.ID, err)
			}
		}

		if err := validateStageTarget("on_success", s.OnSuccess, stageIDs); err != nil {
			return fmt.Errorf("stage %q: %w", s.ID, err)
		}
		if err := validateStageTarget("on_failure", s.OnFailure, stageIDs); err != nil {
			return fmt.Errorf("stage %q: %w", s.ID, err)
		}
	}

	return nil
}

func validateFanOut(stageID string, fo *FanOutConfig, agentIDs map[string]bool) error {
	if len(fo.Agents) < 2 {
		return fmt.Errorf("fan_out must have at least 2 agents, got %d", len(fo.Agents))
	}

	seen := make(map[string]bool, len(fo.Agents))
	for _, a := range fo.Agents {
		if a.ID == "" {
			return fmt.Errorf("fan_out agent ID must not be empty")
		}
		if seen[a.ID] {
			return fmt.Errorf("fan_out contains duplicate agent ID: %q", a.ID)
		}
		seen[a.ID] = true
		if !agentIDs[a.ID] {
			return fmt.Errorf("fan_out references unknown agent: %q", a.ID)
		}
	}

	switch fo.Mode {
	case FanOutModeGather, FanOutModeRace:
		// valid
	default:
		return fmt.Errorf("fan_out mode must be %q or %q, got %q", FanOutModeGather, FanOutModeRace, fo.Mode)
	}

	switch fo.Require {
	case RequirePolicyAll, RequirePolicyAny, RequirePolicyMajority:
		// valid
	default:
		return fmt.Errorf("fan_out require must be %q, %q, or %q, got %q", RequirePolicyAll, RequirePolicyAny, RequirePolicyMajority, fo.Require)
	}

	return nil
}

func validateSchemaRef(field string, ref StageSchemaRef, schemaIndex map[string]string) error {
	if ref.Name == "" {
		return fmt.Errorf("%s: name is required", field)
	}
	if ref.Version == "" {
		return fmt.Errorf("%s: version is required for schema %q", field, ref.Name)
	}

	key := schemaKey(ref.Name, ref.Version)
	if _, ok := schemaIndex[key]; ok {
		return nil
	}

	// Check if the name exists but with a different version.
	if registered, ok := schemaIndex["__versions__:"+ref.Name]; ok {
		return fmt.Errorf("%s: schema %q version %q is not registered (registered versions: %s)", field, ref.Name, ref.Version, registered)
	}

	return fmt.Errorf("%s: schema %q is not declared in schema_registry", field, ref.Name)
}

func validateStageTarget(field, target string, stageIDs map[string]bool) error {
	if target == "" {
		return nil
	}
	if target == "done" || target == "dead-letter" {
		return nil
	}
	if !stageIDs[target] {
		return fmt.Errorf("%s references unknown stage: %q", field, target)
	}
	return nil
}

// validScopes defines the allowed scope strings for API key authentication.
var validScopes = map[string]bool{
	"read":  true,
	"write": true,
	"admin": true,
}

func validateAuth(auth APIAuthConfig) error {
	if !auth.Enabled {
		return nil
	}

	if len(auth.Keys) == 0 {
		return fmt.Errorf("auth.enabled is true but auth.keys is empty — at least one key is required")
	}

	names := make(map[string]bool, len(auth.Keys))
	for i, k := range auth.Keys {
		if k.Name == "" {
			return fmt.Errorf("auth.keys[%d]: name is required", i)
		}
		if names[k.Name] {
			return fmt.Errorf("auth.keys: duplicate key name %q", k.Name)
		}
		names[k.Name] = true

		if k.KeyEnv == "" {
			return fmt.Errorf("auth.keys[%d] (%s): key_env is required", i, k.Name)
		}

		if len(k.Scopes) == 0 {
			return fmt.Errorf("auth.keys[%d] (%s): at least one scope is required", i, k.Name)
		}
		for _, s := range k.Scopes {
			if !validScopes[s] {
				return fmt.Errorf("auth.keys[%d] (%s): invalid scope %q (valid: read, write, admin)", i, k.Name, s)
			}
		}
	}
	return nil
}
