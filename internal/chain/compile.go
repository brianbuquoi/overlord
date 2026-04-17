package chain

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/brianbuquoi/overlord/internal/config"
	"github.com/brianbuquoi/overlord/internal/contract"
)

// Compiled is the result of lowering a chain into runtime types. The
// fields carry everything a caller needs to stand up a broker without
// the strict config loader involved:
//
//   - Config is a valid config.Config the broker accepts verbatim.
//   - Registry is a compiled contract.Registry holding the synthesized
//     schemas. It is created in memory so the compiled chain does not
//     depend on a temp directory layout.
//   - Schemas maps "name@version" to raw JSON bytes so `chain export`
//     can write them alongside the exported pipeline YAML.
//   - Templates maps step ID to the step's fully-vars-resolved prompt
//     template; {{input}} and {{steps.<id>.output}} placeholders are
//     preserved for per-task substitution by the chain-step adapter.
//   - BasePath is the directory relative fixture paths resolve against;
//     populated when Compile is called via CompileWithBase so the mock
//     adapter's constructor-time fixture validation finds files.
type Compiled struct {
	Config    *config.Config
	Registry  *contract.Registry
	Schemas   map[string][]byte
	Templates map[string]string
	BasePath  string

	// Chain is a reference back to the source chain used to compile
	// this result. It is kept so the wrapper adapter can ask for the
	// chain's declared input.type when seeding {{input}} on the first
	// step — any information the adapter needs flows through the
	// compiled record instead of a side channel.
	Chain *Chain
}

// Schema names and versions used throughout chain mode. They are
// reserved: a chain author cannot declare a step or chain ID that
// collides with these because validators gate identifiers to the
// `[a-zA-Z0-9][a-zA-Z0-9._-]*` class (period excluded as a safe
// separator; the compiler never emits stage/agent IDs that resemble
// the schema keys).
const (
	textSchemaName    = "chain_text"
	textSchemaVersion = "v1"
	jsonSchemaName    = "chain_json"
	jsonSchemaVersion = "v1"
	// inputJSONSchemaName/Version guard the first stage's input when
	// a chain declares input.type: json. It is intentionally distinct
	// from chain_json@v1 (the final-stage output contract, which an
	// author can tighten via output.schema) so tightening the output
	// does not accidentally reject the initial payload.
	inputJSONSchemaName    = "chain_input_json"
	inputJSONSchemaVersion = "v1"

	// stepPromptDefaultTimeout bounds the per-step LLM call. 120s is
	// generous for a single prompt round-trip and matches the default
	// adapter timeout used by internal/agent/anthropic.
	stepPromptDefaultTimeout = 120 * time.Second

	// stepRetryMaxAttempts is the default per-step retry count. Chains
	// are a front-door simplification; authors that need heavy retry
	// policies should graduate to pipeline mode via `chain export`.
	stepRetryMaxAttempts = 1

	// memoryStoreMaxTasks bounds the memory store for chain-mode runs.
	memoryStoreMaxTasks = 1024

	// agentIDSuffix is appended to the step ID to produce the agent ID
	// used in the compiled config. Keeping a distinct agent ID avoids
	// collisions with reserved stage IDs like "done".
	agentIDSuffix = ""
)

// textSchemaJSON validates {"text": "<string>"} — the internal wire
// format chain mode uses between stages. Plain-text LLM responses are
// wrapped into this shape by internal/agent.ParseJSONObjectOutput, so
// every built-in adapter already produces conforming output.
var textSchemaJSON = []byte(`{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "title": "chain.text",
  "description": "Internal text wire format used between chain steps.",
  "type": "object",
  "properties": {
    "text": { "type": "string" }
  },
  "required": ["text"],
  "additionalProperties": true
}`)

// jsonSchemaJSON is the final-stage schema used when a chain declares
// output.type: json. It accepts any object so the caller's prompt can
// produce whatever structured payload fits the use case; authors can
// tighten it via chain.output.schema without graduating, or graduate
// via `overlord chain export` for full schema-registry control.
var jsonSchemaJSON = []byte(`{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "title": "chain.json",
  "description": "Open object schema for chain output.type: json.",
  "type": "object"
}`)

// inputJSONSchemaJSON is the first-stage input schema used when a
// chain declares input.type: json. It accepts any object so any
// JSON-object payload the caller submits is forwarded into the
// chain. The output-side chain_json@v1 schema can be tightened by
// the author independently.
var inputJSONSchemaJSON = []byte(`{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "title": "chain.input.json",
  "description": "Open object schema for chain input.type: json.",
  "type": "object"
}`)

// Compile lowers a chain into runtime types. The returned *Compiled is
// self-contained and safe to pass to broker construction. BasePath is
// not set; use CompileWithBase when a chain references mock fixtures
// whose paths resolve against a project directory.
func Compile(ch *Chain) (*Compiled, error) {
	return CompileWithBase(ch, "")
}

// CompileWithBase is the base-path-aware compile entry point. The base
// path is recorded on the result so broker construction can pass it to
// the mock adapter, which requires an absolute directory to resolve
// fixture references.
func CompileWithBase(ch *Chain, basePath string) (*Compiled, error) {
	if err := Validate(ch); err != nil {
		return nil, err
	}
	if basePath != "" {
		abs, err := filepath.Abs(basePath)
		if err != nil {
			return nil, fmt.Errorf("resolve base path %q: %w", basePath, err)
		}
		basePath = abs
	}

	outputType := ch.OutputType()
	inputType := ch.InputType()

	schemas := map[string][]byte{
		schemaKey(textSchemaName, textSchemaVersion): cloneBytes(textSchemaJSON),
	}
	if inputType == "json" {
		schemas[schemaKey(inputJSONSchemaName, inputJSONSchemaVersion)] = cloneBytes(inputJSONSchemaJSON)
	}
	if outputType == "json" {
		jsonBytes := cloneBytes(jsonSchemaJSON)
		// An inline output.schema overrides the default open-object
		// schema. The validator already ensured the schema serializes
		// and compiles cleanly, so we treat failures here as
		// internal errors.
		if ch.Output != nil && len(ch.Output.Schema) > 0 {
			data, err := json.Marshal(ch.Output.Schema)
			if err != nil {
				return nil, fmt.Errorf("compile chain.output.schema: %w", err)
			}
			jsonBytes = data
		}
		schemas[schemaKey(jsonSchemaName, jsonSchemaVersion)] = jsonBytes
	}

	templates := make(map[string]string, len(ch.Steps))
	for _, st := range ch.Steps {
		templates[st.ID] = ResolveVarsOnly(st.Prompt, ch.Vars)
	}

	stages, err := compileStages(ch, inputType, outputType)
	if err != nil {
		return nil, err
	}

	agents, err := compileAgents(ch, templates)
	if err != nil {
		return nil, err
	}

	schemaRegistry := []config.SchemaEntry{
		{Name: textSchemaName, Version: textSchemaVersion, Path: schemaFileName(textSchemaName, textSchemaVersion)},
	}
	if inputType == "json" {
		schemaRegistry = append(schemaRegistry, config.SchemaEntry{
			Name:    inputJSONSchemaName,
			Version: inputJSONSchemaVersion,
			Path:    schemaFileName(inputJSONSchemaName, inputJSONSchemaVersion),
		})
	}
	if outputType == "json" {
		schemaRegistry = append(schemaRegistry, config.SchemaEntry{
			Name:    jsonSchemaName,
			Version: jsonSchemaVersion,
			Path:    schemaFileName(jsonSchemaName, jsonSchemaVersion),
		})
	}

	cfg := &config.Config{
		Version:        "1",
		SchemaRegistry: schemaRegistry,
		Pipelines: []config.Pipeline{{
			Name:        ch.ID,
			Concurrency: 1,
			Store:       "memory",
			Stages:      stages,
		}},
		Agents: agents,
		Stores: config.StoreConfig{
			Memory: &config.MemoryStoreConfig{MaxTasks: memoryStoreMaxTasks},
		},
	}

	rawEntries := make([]contract.RawSchemaEntry, 0, len(schemas))
	// Build in deterministic order so registry errors (if any) are stable.
	keys := make([]string, 0, len(schemas))
	for k := range schemas {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		name, version := parseSchemaKey(k)
		rawEntries = append(rawEntries, contract.RawSchemaEntry{
			Name: name, Version: version, Data: schemas[k],
		})
	}

	reg, err := contract.NewRegistryFromRaw(rawEntries)
	if err != nil {
		return nil, fmt.Errorf("compile schemas: %w", err)
	}

	return &Compiled{
		Config:    cfg,
		Registry:  reg,
		Schemas:   schemas,
		Templates: templates,
		BasePath:  basePath,
		Chain:     ch,
	}, nil
}

func compileStages(ch *Chain, inputType, outputType string) ([]config.Stage, error) {
	stages := make([]config.Stage, 0, len(ch.Steps))
	// v1: chain.output.from must reference the chain's last step (the
	// validator enforces this). The last stage therefore always routes
	// to "done" and is the one that carries the output.type schema.
	finalIdx := len(ch.Steps) - 1

	for i, st := range ch.Steps {
		timeout := stepPromptDefaultTimeout
		if st.Timeout != "" {
			d, err := time.ParseDuration(st.Timeout)
			if err != nil {
				return nil, fmt.Errorf("step %q: invalid timeout %q: %w", st.ID, st.Timeout, err)
			}
			if d <= 0 {
				return nil, fmt.Errorf("step %q: timeout must be positive", st.ID)
			}
			timeout = d
		}

		inputSchema := config.StageSchemaRef{Name: textSchemaName, Version: textSchemaVersion}
		outputSchema := config.StageSchemaRef{Name: textSchemaName, Version: textSchemaVersion}
		if i == 0 && inputType == "json" {
			// First stage accepts the raw JSON object the caller
			// submitted. Intermediate steps keep the text wire
			// format (stage N+1 reads the extractTextPayload
			// output of stage N).
			inputSchema = config.StageSchemaRef{Name: inputJSONSchemaName, Version: inputJSONSchemaVersion}
		}
		if i == finalIdx && outputType == "json" {
			outputSchema = config.StageSchemaRef{Name: jsonSchemaName, Version: jsonSchemaVersion}
		}

		var next string
		if i == finalIdx {
			next = "done"
		} else {
			next = ch.Steps[i+1].ID
		}

		stages = append(stages, config.Stage{
			ID:           st.ID,
			Agent:        agentIDForStep(st.ID),
			InputSchema:  inputSchema,
			OutputSchema: outputSchema,
			Timeout:      config.Duration{Duration: timeout},
			Retry: config.RetryPolicy{
				MaxAttempts: stepRetryMaxAttempts,
				Backoff:     "fixed",
				BaseDelay:   config.Duration{Duration: 500 * time.Millisecond},
			},
			OnSuccess: config.StaticOnSuccess(next),
			OnFailure: "dead-letter",
		})
	}
	return stages, nil
}

func compileAgents(ch *Chain, templates map[string]string) ([]config.Agent, error) {
	agents := make([]config.Agent, 0, len(ch.Steps))
	for _, st := range ch.Steps {
		provider, model, _, err := splitModel(st.Model)
		if err != nil {
			return nil, fmt.Errorf("step %q: %w", st.ID, err)
		}
		timeout := stepPromptDefaultTimeout
		if st.Timeout != "" {
			if d, perr := time.ParseDuration(st.Timeout); perr == nil {
				timeout = d
			}
		}
		ac := config.Agent{
			ID:           agentIDForStep(st.ID),
			Provider:     provider,
			Model:        model,
			Auth:         config.AuthConfig{APIKeyEnv: defaultKeyEnv(provider)},
			SystemPrompt: templates[st.ID],
			Temperature:  st.Temperature,
			MaxTokens:    st.MaxTokens,
			Timeout:      config.Duration{Duration: timeout},
		}
		if provider == "mock" {
			ac.Fixtures = map[string]string{st.ID: st.Fixture}
		}
		agents = append(agents, ac)
	}
	return agents, nil
}

// defaultKeyEnv returns the conventional API-key env var for a built-in
// provider, mirroring what pipeline scaffolds set by default.
func defaultKeyEnv(provider string) string {
	switch provider {
	case "anthropic":
		return "ANTHROPIC_API_KEY"
	case "openai", "openai-responses":
		return "OPENAI_API_KEY"
	case "google":
		return "GEMINI_API_KEY"
	default:
		return ""
	}
}

// agentIDForStep returns the agent ID the compiler binds a step to.
// Kept identical to the step ID for readability — exported pipeline
// YAML is easier to follow when stage and agent IDs match. The
// compiler reserves a layer of indirection here in case future work
// needs distinct identifiers.
func agentIDForStep(stepID string) string {
	if agentIDSuffix == "" {
		return stepID
	}
	return stepID + agentIDSuffix
}

// schemaFileName produces the conventional JSON file name for a
// synthesized schema. `chain export` writes files with exactly this
// name, and the exported pipeline YAML's schema_registry paths refer
// to them by basename.
func schemaFileName(name, version string) string {
	return strings.ReplaceAll(name, ".", "_") + "_" + version + ".json"
}

func schemaKey(name, version string) string { return name + "@" + version }

func parseSchemaKey(k string) (string, string) {
	i := strings.LastIndex(k, "@")
	if i < 0 {
		return k, ""
	}
	return k[:i], k[i+1:]
}

func cloneBytes(b []byte) []byte {
	out := make([]byte, len(b))
	copy(out, b)
	return out
}
