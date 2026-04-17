package chain

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Load reads a chain YAML file from path and returns the parsed chain
// after structural validation. The file must contain a top-level
// `chain:` block; files that look like full pipeline configs
// (schema_registry, pipelines, etc. at the top level) are rejected so
// that the two-layer product keeps a sharp boundary.
func Load(path string) (*Chain, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("chain file not found: %s", path)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("chain file must not be a symlink: %s", path)
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("chain path is not a regular file: %s", path)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading chain file: %w", err)
	}

	return Parse(data, path)
}

// Parse parses chain YAML bytes. source is used in error messages and
// may be any human-readable label; callers without a filename should
// pass "<chain>" or similar.
func Parse(data []byte, source string) (*Chain, error) {
	var file File
	if err := yaml.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("parse chain %s: %w", source, err)
	}
	if file.Version == "" {
		return nil, fmt.Errorf("chain %s: missing version field", source)
	}
	if file.Version != "1" {
		return nil, fmt.Errorf("chain %s: unsupported version %q (expected \"1\")", source, file.Version)
	}
	if file.Chain == nil {
		// Surface a friendly hint when users accidentally feed a full
		// pipeline config into the chain command: detect the two
		// pipeline-only keys most likely to be present.
		var probe struct {
			SchemaRegistry any `yaml:"schema_registry"`
			Pipelines      any `yaml:"pipelines"`
		}
		_ = yaml.Unmarshal(data, &probe)
		if probe.SchemaRegistry != nil || probe.Pipelines != nil {
			return nil, fmt.Errorf(
				"chain %s: no top-level `chain:` block found — this file looks like a full pipeline config; use `overlord run` / `overlord exec` instead",
				source,
			)
		}
		return nil, fmt.Errorf("chain %s: missing `chain:` block", source)
	}
	if err := Validate(file.Chain); err != nil {
		return nil, fmt.Errorf("chain %s: %w", source, err)
	}
	return file.Chain, nil
}
