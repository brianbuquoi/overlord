package chain

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/brianbuquoi/overlord/internal/config"
)

// ExportFiles is the in-memory form of a chain export. Each map entry
// is a file path relative to the export root; values are the file's
// bytes. The caller (CLI or test) decides whether to write them to
// disk or stream them to stdout.
type ExportFiles struct {
	// Pipeline is the exported overlord.yaml content.
	Pipeline []byte
	// Schemas maps JSON schema file name -> bytes.
	Schemas map[string][]byte
	// Fixtures maps relative fixture path -> bytes. Populated when
	// the chain includes mock-provider steps so the exported
	// directory is fully self-contained and can be invoked by
	// `overlord exec --config <dir>/overlord.yaml` without additional
	// files being copied by hand.
	Fixtures map[string][]byte
}

// ExportLoweringError is returned by Export when a chain's prompt
// contains a placeholder that cannot be statically lowered into the
// strict-mode form. The audit found that without lowering, the
// exported config carries literal `{{steps.<id>.output}}` / `{{input}}`
// text in `config.Agent.SystemPrompt`, which real-provider adapters
// (anthropic, openai, etc.) then send to the model verbatim — the
// strict-mode broker does not apply the chain step adapter's
// runtime substitution. Export stops loudly in that case rather
// than producing a config that behaves differently from the
// workflow/chain runtime.
//
// The two lowerable patterns are (a) `{{input}}` on the first step,
// and (b) `{{steps.<prior>.output}}` where <prior> is the
// immediately preceding step. Everything else — `{{input}}` on a
// non-first step, or `{{steps.<X>.output}}` referring to a
// non-adjacent step — cannot be expressed in the strict runtime
// without adding a per-task metadata lookup layer.
type ExportLoweringError struct {
	StepID      string // step whose prompt holds the un-lowerable reference
	Placeholder string // the offending `{{...}}` text, verbatim
	Reason      string // human-readable explanation
}

func (e *ExportLoweringError) Error() string {
	return fmt.Sprintf(
		"export: step %q prompt contains %s that cannot be lowered into the strict config: %s",
		e.StepID, e.Placeholder, e.Reason,
	)
}

// Export emits the pipeline YAML and schema JSON files that a chain
// would compile to. The resulting files are a valid full-pipeline
// config — running `overlord exec --config overlord.yaml ...` against
// the exported directory produces the same broker behavior the chain
// does.
//
// Export is the canonical graduation path from chain mode to full
// pipeline mode: authors copy the output, hand-edit it (adding fan-
// out, conditional routing, split infra config, retry budgets,
// explicit schemas) as the workflow matures, and keep running on the
// existing runtime.
//
// Before emitting the YAML, Export lowers every step's system prompt
// from the chain-runtime form (with `{{input}}` /
// `{{steps.<id>.output}}` placeholders) into the strict-runtime form
// (no placeholders; adjacent-step references are subsumed by the
// broker's envelope wrapper). Non-adjacent step references are
// refused with ExportLoweringError — those workflows must either be
// restructured to be linear + adjacent or stay on the workflow
// surface.
func Export(compiled *Compiled) (*ExportFiles, error) {
	// Lower the compiled config before rendering so the exported
	// YAML never contains `{{...}}` templates that strict-mode
	// adapters would ship to the LLM verbatim. The lowering runs on
	// a cloned config so the in-memory record used by the current
	// chain/workflow broker is unaffected.
	lowered, err := loweredForExport(compiled)
	if err != nil {
		return nil, err
	}

	yamlBytes, err := renderPipelineYAML(lowered)
	if err != nil {
		return nil, err
	}

	schemas := make(map[string][]byte, len(compiled.Schemas))
	for key, data := range compiled.Schemas {
		name, version := parseSchemaKey(key)
		fname := schemaFileName(name, version)
		schemas[fname] = cloneBytes(data)
	}

	fixtures, err := collectFixtures(compiled)
	if err != nil {
		return nil, err
	}

	return &ExportFiles{
		Pipeline: yamlBytes,
		Schemas:  schemas,
		Fixtures: fixtures,
	}, nil
}

// loweredForExport returns a clone of compiled with every
// chain-generated agent's SystemPrompt rewritten to its
// strict-runtime equivalent: no `{{input}}` or
// `{{steps.<id>.output}}` placeholders. The live `compiled.Config`
// is left untouched so the currently-running chain broker's agents
// continue to render the dynamic form.
//
// Non-chain agents in the config (e.g. ones a caller added via a
// hand-rolled `Compiled`) are preserved as-is so this function
// composes with future extensions.
//
// Returns ExportLoweringError when a prompt references a
// non-adjacent step or carries `{{input}}` on a non-first step.
func loweredForExport(compiled *Compiled) (*Compiled, error) {
	if compiled == nil || compiled.Config == nil {
		return compiled, nil
	}
	var steps []Step
	if compiled.Chain != nil {
		steps = compiled.Chain.Steps
	}
	stepIdx := make(map[string]int, len(steps))
	for i, st := range steps {
		stepIdx[st.ID] = i
	}

	cfgCopy := *compiled.Config
	// Shallow-clone the Agents slice so mutations below don't leak
	// into the live config. Agent fields we don't touch share state
	// with the original (safe — they're immutable after compile).
	loweredAgents := make([]config.Agent, len(cfgCopy.Agents))
	for i, a := range cfgCopy.Agents {
		loweredAgents[i] = a
		stepI, isChainAgent := stepIdx[a.ID]
		if !isChainAgent {
			continue
		}
		priorID := ""
		if stepI > 0 {
			priorID = steps[stepI-1].ID
		}
		lowered, err := lowerStepPrompt(a.ID, a.SystemPrompt, priorID, stepI == 0)
		if err != nil {
			return nil, err
		}
		loweredAgents[i].SystemPrompt = lowered
	}
	cfgCopy.Agents = loweredAgents

	cloneRecord := *compiled
	cloneRecord.Config = &cfgCopy
	return &cloneRecord, nil
}

// chainPlaceholderRE matches the two placeholder forms the chain
// compiler leaves in step system prompts at compile time. `{{vars.*}}`
// is already resolved at compile time so it never reaches the
// lowering pass.
var chainPlaceholderRE = regexp.MustCompile(`\{\{\s*(input|steps\.[A-Za-z0-9._-]+\.output)\s*\}\}`)

// lowerStepPrompt rewrites prompt so every remaining `{{input}}` /
// `{{steps.<id>.output}}` reference is either (a) replaced by a
// natural-language narration pointing at the broker's envelope
// wrapper + user-message content, or (b) reported as an
// ExportLoweringError when the reference cannot be preserved in the
// strict runtime.
func lowerStepPrompt(stepID, prompt, priorStepID string, isFirst bool) (string, error) {
	var loweringErr *ExportLoweringError
	lowered := chainPlaceholderRE.ReplaceAllStringFunc(prompt, func(match string) string {
		if loweringErr != nil {
			// Bail early — regexp.ReplaceAllStringFunc has no short-
			// circuit so we fall through and let the outer caller
			// surface the first error.
			return match
		}
		m := chainPlaceholderRE.FindStringSubmatch(match)
		if len(m) < 2 {
			return match
		}
		ref := m[1]
		switch {
		case ref == "input":
			if isFirst {
				return "(see user-provided input)"
			}
			loweringErr = &ExportLoweringError{
				StepID:      stepID,
				Placeholder: match,
				Reason: "`{{input}}` is only lowerable on the first step; " +
					"the strict runtime does not propagate the original user input to later stages. " +
					"Rewrite the prompt to read from the prior-stage payload, or stay on the workflow surface.",
			}
			return match
		case strings.HasPrefix(ref, "steps."):
			target := strings.TrimSuffix(strings.TrimPrefix(ref, "steps."), ".output")
			if target == priorStepID {
				return "(see prior stage output above in the system context)"
			}
			loweringErr = &ExportLoweringError{
				StepID:      stepID,
				Placeholder: match,
				Reason: fmt.Sprintf(
					"references step %q which is not the immediately preceding step. "+
						"The strict runtime's envelope only carries the prior stage's output; "+
						"non-adjacent step references need the workflow/chain step adapter. "+
						"Restructure the workflow to be linear/adjacent, or stay on the workflow surface.",
					target,
				),
			}
			return match
		}
		return match
	})
	if loweringErr != nil {
		return "", loweringErr
	}
	return lowered, nil
}

// collectFixtures reads every mock-fixture file referenced by an agent
// in the compiled config, resolved relative to compiled.BasePath. The
// returned map keys are the exact relative paths the agents reference
// so the exported directory preserves the same structure the chain
// YAML uses — authors who export never have to rewrite relative paths
// by hand. A chain with no mock steps returns (nil, nil).
func collectFixtures(compiled *Compiled) (map[string][]byte, error) {
	out := map[string][]byte{}
	if compiled == nil || compiled.Config == nil {
		return out, nil
	}
	for _, a := range compiled.Config.Agents {
		if a.Provider != "mock" {
			continue
		}
		for _, rel := range a.Fixtures {
			if _, ok := out[rel]; ok {
				continue
			}
			if filepath.IsAbs(rel) {
				// Validator rejected absolute paths at compile time,
				// but belt-and-suspenders against a hand-constructed
				// Compiled struct in tests.
				return nil, fmt.Errorf("fixture %q is absolute; only relative fixture paths export cleanly", rel)
			}
			src := rel
			if compiled.BasePath != "" {
				src = filepath.Join(compiled.BasePath, rel)
			}
			data, err := os.ReadFile(src)
			if err != nil {
				return nil, fmt.Errorf("read fixture %s: %w", src, err)
			}
			out[rel] = data
		}
	}
	return out, nil
}

// WriteTo materializes the export into dir. The directory is created
// if missing; existing files are overwritten. Schemas go into a
// schemas/ subdirectory relative to dir so the layout matches what
// `overlord init summarize` produces.
func (e *ExportFiles) WriteTo(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create export dir: %w", err)
	}
	pipelinePath := filepath.Join(dir, "overlord.yaml")
	if err := os.WriteFile(pipelinePath, e.Pipeline, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", pipelinePath, err)
	}
	schemasDir := filepath.Join(dir, "schemas")
	if len(e.Schemas) > 0 {
		if err := os.MkdirAll(schemasDir, 0o755); err != nil {
			return fmt.Errorf("create schemas dir: %w", err)
		}
	}
	// Write in deterministic order for reproducibility and easy
	// diffing of snapshot tests.
	names := make([]string, 0, len(e.Schemas))
	for n := range e.Schemas {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		p := filepath.Join(schemasDir, n)
		if err := os.WriteFile(p, e.Schemas[n], 0o644); err != nil {
			return fmt.Errorf("write %s: %w", p, err)
		}
	}

	// Fixtures are written at whatever relative path the chain
	// declared — typically fixtures/<step>.json — so the exported
	// directory mirrors the source layout and `overlord exec` finds
	// them without any path rewriting.
	fixtureNames := make([]string, 0, len(e.Fixtures))
	for n := range e.Fixtures {
		fixtureNames = append(fixtureNames, n)
	}
	sort.Strings(fixtureNames)
	for _, n := range fixtureNames {
		p := filepath.Join(dir, n)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return fmt.Errorf("create fixture dir: %w", err)
		}
		if err := os.WriteFile(p, e.Fixtures[n], 0o644); err != nil {
			return fmt.Errorf("write fixture %s: %w", p, err)
		}
	}
	return nil
}

// renderPipelineYAML emits the compiled config as a human-friendly
// YAML document. Schema paths are rewritten to the schemas/ sub-
// directory so the exported directory works as a pipeline-mode
// project out of the box.
func renderPipelineYAML(compiled *Compiled) ([]byte, error) {
	// Clone the config so we can adjust schema paths for the export
	// layout without mutating the live broker config.
	cfg := *compiled.Config
	cfg.SchemaRegistry = schemaEntriesForExport(compiled)

	var buf bytes.Buffer
	header := "# Generated by `overlord export --advanced`.\n" +
		"# This is the strict-config equivalent of the workflow (or chain)\n" +
		"# that produced it. Edit freely — the runtime does not know where\n" +
		"# it came from, and the graduation is one-way by design.\n"
	// When stage/agent IDs match the workflow compiler's auto-generated
	// `step_N` shape, the author did not name their steps in the source
	// workflow. Flag that explicitly so a reader who's about to add
	// fan-out / retries / conditional routing knows to pick meaningful
	// names first — generic IDs read badly once they anchor a real DAG.
	if ids := autoStageIDs(cfg); len(ids) > 0 {
		header += "#\n"
		header += "# NOTE: the stages named " + formatAutoStageIDs(ids) + "\n"
		header += "# were auto-generated by the workflow compiler because the source\n" +
			"# workflow did not supply `id:` on each step. Rename them to\n" +
			"# something meaningful (e.g. `draft`, `review`, `score`) before\n" +
			"# adding fan-out, conditional routing, or retry budgets — the\n" +
			"# generic names stay visible in logs, metrics, and traces forever.\n"
	}
	header += "\n"
	if _, err := buf.WriteString(header); err != nil {
		return nil, err
	}
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(cfg); err != nil {
		return nil, fmt.Errorf("encode pipeline yaml: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("close yaml encoder: %w", err)
	}
	return buf.Bytes(), nil
}

// autoStageIDRE matches the workflow compiler's auto-generated step IDs
// (`step_N` and its collision-avoidance suffix `step_N_M`).
var autoStageIDRE = regexp.MustCompile(`^step_\d+(?:_\d+)?$`)

// autoStageIDs returns the set of stage IDs in the exported config that
// look like workflow-compiler auto-generated identifiers, sorted for a
// deterministic comment.
func autoStageIDs(cfg config.Config) []string {
	seen := map[string]struct{}{}
	for _, p := range cfg.Pipelines {
		for _, s := range p.Stages {
			if autoStageIDRE.MatchString(s.ID) {
				seen[s.ID] = struct{}{}
			}
		}
	}
	if len(seen) == 0 {
		return nil
	}
	out := make([]string, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// formatAutoStageIDs renders a comma-separated, backtick-quoted list
// suitable for inclusion in a YAML comment.
func formatAutoStageIDs(ids []string) string {
	if len(ids) == 0 {
		return ""
	}
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = "`" + id + "`"
	}
	return strings.Join(parts, ", ")
}

// schemaEntriesForExport rewrites the compiled schema paths into the
// schemas/<name>_<version>.json form the exported layout uses.
func schemaEntriesForExport(compiled *Compiled) []config.SchemaEntry {
	keys := make([]string, 0, len(compiled.Schemas))
	for k := range compiled.Schemas {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]config.SchemaEntry, 0, len(keys))
	for _, k := range keys {
		name, version := parseSchemaKey(k)
		out = append(out, config.SchemaEntry{
			Name:    name,
			Version: version,
			Path:    filepath.Join("schemas", schemaFileName(name, version)),
		})
	}
	return out
}
