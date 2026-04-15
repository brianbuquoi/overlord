package plugin

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration wraps time.Duration so it can be unmarshaled from YAML duration
// strings like "30s" or "5m". It is intentionally local to the subprocess
// plugin manifest so that its parsing rules (non-negative, required non-zero)
// can evolve independently of the top-level config.Duration type.
type Duration struct {
	time.Duration
}

// UnmarshalYAML parses a string like "30s" into a Duration.
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return fmt.Errorf("duration must be a string: %w", err)
	}
	if s == "" {
		d.Duration = 0
		return nil
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	if parsed < 0 {
		return fmt.Errorf("negative duration not allowed: %q", s)
	}
	d.Duration = parsed
	return nil
}

// OnFailure constants describe how execution errors returned by a plugin
// subprocess are surfaced to the broker's retry logic.
const (
	OnFailureRetryable    = "retryable"
	OnFailureNonRetryable = "non_retryable"
)

// Manifest describes a subprocess-based plugin. The YAML on disk is the
// canonical source of truth; the manifestDir field is populated by
// LoadManifest so relative Binary paths can be resolved later.
type Manifest struct {
	Name            string            `yaml:"name"`
	Version         string            `yaml:"version,omitempty"`
	Description     string            `yaml:"description,omitempty"`
	Binary          string            `yaml:"binary"`
	Args            []string          `yaml:"args,omitempty"`
	Env             []string          `yaml:"env,omitempty"`
	OnFailure       string            `yaml:"on_failure,omitempty"`
	RPCTimeout      Duration          `yaml:"rpc_timeout,omitempty"`
	ShutdownTimeout Duration          `yaml:"shutdown_timeout,omitempty"`
	MaxRestarts     int               `yaml:"max_restarts,omitempty"`
	Metadata        map[string]string `yaml:"metadata,omitempty"`

	// manifestDir is the directory containing the manifest file, used to
	// resolve a relative Binary path when the subprocess is started. It is
	// never exposed via YAML.
	manifestDir string `yaml:"-"`
}

// Validate checks required fields and value ranges on a manifest.
func (m *Manifest) Validate() error {
	if strings.TrimSpace(m.Name) == "" {
		return fmt.Errorf("manifest: name is required")
	}
	if strings.ContainsAny(m.Name, "/\\") {
		return fmt.Errorf("manifest: name %q must not contain path separators", m.Name)
	}
	if strings.TrimSpace(m.Binary) == "" {
		return fmt.Errorf("manifest: binary is required")
	}
	switch m.OnFailure {
	case "", OnFailureRetryable, OnFailureNonRetryable:
	default:
		return fmt.Errorf("manifest: on_failure must be %q or %q (got %q)", OnFailureRetryable, OnFailureNonRetryable, m.OnFailure)
	}
	if m.RPCTimeout.Duration < 0 {
		return fmt.Errorf("manifest: rpc_timeout must be non-negative")
	}
	if m.ShutdownTimeout.Duration < 0 {
		return fmt.Errorf("manifest: shutdown_timeout must be non-negative")
	}
	if m.MaxRestarts < 0 {
		return fmt.Errorf("manifest: max_restarts must be >= 0 (0 means unlimited)")
	}
	return nil
}

// OnFailureOrDefault returns m.OnFailure, defaulting to OnFailureRetryable
// when unset.
func (m *Manifest) OnFailureOrDefault() string {
	if m.OnFailure == "" {
		return OnFailureRetryable
	}
	return m.OnFailure
}

// ShutdownTimeoutOrDefault returns ShutdownTimeout, defaulting to 5s.
func (m *Manifest) ShutdownTimeoutOrDefault() time.Duration {
	if m.ShutdownTimeout.Duration <= 0 {
		return 5 * time.Second
	}
	return m.ShutdownTimeout.Duration
}

// ManifestDir returns the directory the manifest file was loaded from, used
// to resolve relative binary paths.
func (m *Manifest) ManifestDir() string {
	return m.manifestDir
}

// LoadManifest reads, parses, and validates a plugin manifest from path. The
// returned Manifest remembers its directory so relative Binary entries can be
// resolved at subprocess launch time.
func LoadManifest(path string) (*Manifest, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("manifest %q: resolve absolute path: %w", path, err)
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return nil, fmt.Errorf("manifest %q: read: %w", path, err)
	}
	var m Manifest
	if err := yaml.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("manifest %q: parse yaml: %w", path, err)
	}
	if err := m.Validate(); err != nil {
		return nil, fmt.Errorf("manifest %q: %w", path, err)
	}
	m.manifestDir = filepath.Dir(abs)
	return &m, nil
}

// ResolveBinary returns the absolute binary path: if Binary is absolute it is
// returned as-is, otherwise it is resolved relative to the manifest's
// directory.
func (m *Manifest) ResolveBinary() string {
	if filepath.IsAbs(m.Binary) {
		return m.Binary
	}
	if m.manifestDir == "" {
		return m.Binary
	}
	return filepath.Join(m.manifestDir, m.Binary)
}
