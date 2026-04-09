// Package migration provides schema migration support for transforming task
// payloads between schema versions within a pipeline's store.
package migration

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
)

// Migration transforms a payload from one schema version to the next.
type Migration interface {
	FromVersion() string
	ToVersion() string
	SchemaName() string
	Migrate(ctx context.Context, payload json.RawMessage) (json.RawMessage, error)
}

// registryKey identifies a migration by schema name and source version.
type registryKey struct {
	SchemaName  string
	FromVersion string
}

// Registry holds registered migrations and resolves chains between versions.
type Registry struct {
	mu         sync.RWMutex
	migrations map[registryKey]Migration
}

// NewRegistry creates an empty migration registry.
func NewRegistry() *Registry {
	return &Registry{
		migrations: make(map[registryKey]Migration),
	}
}

// Register adds a migration to the registry. It returns an error if a
// migration for the same (schema_name, from_version) is already registered.
func (r *Registry) Register(m Migration) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	key := registryKey{SchemaName: m.SchemaName(), FromVersion: m.FromVersion()}
	if _, exists := r.migrations[key]; exists {
		return fmt.Errorf("migration already registered: %s from %s to %s",
			m.SchemaName(), m.FromVersion(), m.ToVersion())
	}
	r.migrations[key] = m
	return nil
}

// MustRegister is like Register but panics on error. Intended for init().
func (r *Registry) MustRegister(m Migration) {
	if err := r.Register(m); err != nil {
		panic(err)
	}
}

// Lookup returns a single migration for the given schema and source version.
func (r *Registry) Lookup(schemaName, fromVersion string) (Migration, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	m, ok := r.migrations[registryKey{SchemaName: schemaName, FromVersion: fromVersion}]
	return m, ok
}

// Chain finds and applies the migration path from fromVersion to toVersion
// for the named schema. It returns an error if no complete path exists.
func (r *Registry) Chain(ctx context.Context, schemaName string, fromVersion, toVersion string, payload json.RawMessage) (json.RawMessage, error) {
	steps, err := r.ResolvePath(schemaName, fromVersion, toVersion)
	if err != nil {
		return nil, err
	}

	current := payload
	for _, m := range steps {
		current, err = m.Migrate(ctx, current)
		if err != nil {
			return nil, fmt.Errorf("migration %s %s→%s failed: %w",
				schemaName, m.FromVersion(), m.ToVersion(), err)
		}
	}
	return current, nil
}

// ResolvePath finds the ordered list of migrations from fromVersion to
// toVersion. Returns an error if the path is incomplete.
func (r *Registry) ResolvePath(schemaName, fromVersion, toVersion string) ([]Migration, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if fromVersion == toVersion {
		return nil, nil
	}

	var steps []Migration
	current := fromVersion
	visited := make(map[string]bool)

	for current != toVersion {
		if visited[current] {
			return nil, fmt.Errorf("migration cycle detected for schema %q at version %s", schemaName, current)
		}
		visited[current] = true

		m, ok := r.migrations[registryKey{SchemaName: schemaName, FromVersion: current}]
		if !ok {
			return nil, &ErrNoMigrationPath{
				SchemaName:  schemaName,
				FromVersion: fromVersion,
				ToVersion:   toVersion,
				GapAt:       current,
			}
		}
		steps = append(steps, m)
		current = m.ToVersion()
	}

	return steps, nil
}

// ListAll returns all registered migrations.
func (r *Registry) ListAll() []Migration {
	r.mu.RLock()
	defer r.mu.RUnlock()

	result := make([]Migration, 0, len(r.migrations))
	for _, m := range r.migrations {
		result = append(result, m)
	}
	return result
}

// ErrNoMigrationPath is returned when no chain of migrations can connect
// the source version to the target version.
type ErrNoMigrationPath struct {
	SchemaName  string
	FromVersion string
	ToVersion   string
	GapAt       string
}

func (e *ErrNoMigrationPath) Error() string {
	if e.GapAt == e.FromVersion {
		return fmt.Sprintf("no migration registered for schema %q from version %s (target: %s)",
			e.SchemaName, e.FromVersion, e.ToVersion)
	}
	return fmt.Sprintf("incomplete migration path for schema %q: %s→%s (no migration from %s)",
		e.SchemaName, e.FromVersion, e.ToVersion, e.GapAt)
}

// FuncMigration is a convenience type that implements Migration using a function.
type FuncMigration struct {
	Schema string
	From   string
	To     string
	Fn     func(ctx context.Context, payload json.RawMessage) (json.RawMessage, error)
}

func (m *FuncMigration) SchemaName() string  { return m.Schema }
func (m *FuncMigration) FromVersion() string { return m.From }
func (m *FuncMigration) ToVersion() string   { return m.To }
func (m *FuncMigration) Migrate(ctx context.Context, payload json.RawMessage) (json.RawMessage, error) {
	return m.Fn(ctx, payload)
}
