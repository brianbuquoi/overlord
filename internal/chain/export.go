package chain

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"

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
func Export(compiled *Compiled) (*ExportFiles, error) {
	yamlBytes, err := renderPipelineYAML(compiled)
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
	header := "# Generated by `overlord chain export`.\n" +
		"# This is the full-pipeline equivalent of the chain that produced it.\n" +
		"# Edit freely — the runtime does not know it came from chain mode.\n\n"
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
