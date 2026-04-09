package contract

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/orcastrator/orcastrator/internal/config"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

// CompiledSchema holds a compiled JSON schema ready for validation.
type CompiledSchema struct {
	Name    string
	Version string
	Schema  *jsonschema.Schema
}

// registryKey is the cache key: (name, version).
type registryKey struct {
	Name    string
	Version string
}

// Registry holds all compiled schemas from the schema_registry config block.
// Schemas are compiled eagerly at construction time.
type Registry struct {
	schemas map[registryKey]*CompiledSchema
}

// NewRegistry loads and compiles all schemas declared in the config's
// schema_registry block. It fails fast if any schema file is missing or
// malformed. basePath is the directory from which relative schema paths
// are resolved.
func NewRegistry(entries []config.SchemaEntry, basePath string) (*Registry, error) {
	r := &Registry{
		schemas: make(map[registryKey]*CompiledSchema, len(entries)),
	}

	for _, entry := range entries {
		key := registryKey{Name: entry.Name, Version: entry.Version}
		if _, exists := r.schemas[key]; exists {
			return nil, fmt.Errorf("duplicate schema registry entry: %s@%s", entry.Name, entry.Version)
		}

		schemaPath := entry.Path
		if !filepath.IsAbs(schemaPath) {
			schemaPath = filepath.Join(basePath, schemaPath)
		}

		data, err := os.ReadFile(schemaPath)
		if err != nil {
			return nil, fmt.Errorf("failed to read schema %s@%s from %s: %w", entry.Name, entry.Version, schemaPath, err)
		}

		compiled, err := compileSchema(schemaPath, data)
		if err != nil {
			return nil, fmt.Errorf("failed to compile schema %s@%s: %w", entry.Name, entry.Version, err)
		}

		r.schemas[key] = &CompiledSchema{
			Name:    entry.Name,
			Version: entry.Version,
			Schema:  compiled,
		}
	}

	return r, nil
}

// Lookup returns the compiled schema for the given name and version.
func (r *Registry) Lookup(name, version string) (*CompiledSchema, error) {
	key := registryKey{Name: name, Version: version}
	cs, ok := r.schemas[key]
	if !ok {
		return nil, fmt.Errorf("schema not found in registry: %s@%s", name, version)
	}
	return cs, nil
}

func compileSchema(path string, data []byte) (*jsonschema.Schema, error) {
	unmarshal, err := jsonschema.UnmarshalJSON(strings.NewReader(string(data)))
	if err != nil {
		return nil, err
	}

	c := jsonschema.NewCompiler()
	url := "file://" + filepath.ToSlash(path)
	if err := c.AddResource(url, unmarshal); err != nil {
		return nil, err
	}
	return c.Compile(url)
}
