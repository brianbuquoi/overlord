package config

import (
	"fmt"
	"time"
)

// Config is the top-level configuration, mapping 1:1 to the YAML file.
type Config struct {
	Version        string              `yaml:"version"`
	SchemaRegistry []SchemaEntry       `yaml:"schema_registry"`
	Pipelines      []Pipeline          `yaml:"pipelines"`
	Agents         []Agent             `yaml:"agents"`
	Stores         StoreConfig         `yaml:"stores"`
	Observability  ObservabilityConfig `yaml:"observability"`
}

// ObservabilityConfig holds metrics and tracing settings.
type ObservabilityConfig struct {
	MetricsPath string        `yaml:"metrics_path"`
	Tracing     TracingConfig `yaml:"tracing"`
}

// TracingConfig controls OpenTelemetry trace export.
type TracingConfig struct {
	Enabled        bool   `yaml:"enabled"`
	Exporter       string `yaml:"exporter"`
	OTLPEndpoint   string `yaml:"otlp_endpoint"`
	OTLPInsecure   bool   `yaml:"otlp_insecure"`
	OTLPHeadersEnv string `yaml:"otlp_headers_env"`
}

// SchemaEntry declares a named, versioned schema in the registry.
type SchemaEntry struct {
	Name    string `yaml:"name"`
	Version string `yaml:"version"`
	Path    string `yaml:"path"`
}

// Pipeline defines a named pipeline with its stages.
type Pipeline struct {
	Name        string  `yaml:"name"`
	Concurrency int     `yaml:"concurrency"`
	Store       string  `yaml:"store"`
	Stages      []Stage `yaml:"stages"`
}

// Stage is a single step in a pipeline.
type Stage struct {
	ID           string         `yaml:"id"`
	Agent        string         `yaml:"agent"`
	InputSchema  StageSchemaRef `yaml:"input_schema"`
	OutputSchema StageSchemaRef `yaml:"output_schema"`
	Timeout      Duration       `yaml:"timeout"`
	Retry        RetryPolicy    `yaml:"retry"`
	OnSuccess    string         `yaml:"on_success"`
	OnFailure    string         `yaml:"on_failure"`
}

// StageSchemaRef references a schema_registry entry by name+version.
type StageSchemaRef struct {
	Name    string `yaml:"name"`
	Version string `yaml:"version"`
}

// RetryPolicy configures retry behavior for a stage.
type RetryPolicy struct {
	MaxAttempts int      `yaml:"max_attempts"`
	Backoff     string   `yaml:"backoff"`
	BaseDelay   Duration `yaml:"base_delay"`
}

// Agent defines an LLM provider adapter.
type Agent struct {
	ID           string     `yaml:"id"`
	Provider     string     `yaml:"provider"`
	Model        string     `yaml:"model"`
	Auth         AuthConfig `yaml:"auth"`
	SystemPrompt string     `yaml:"system_prompt"`
	Temperature  float64    `yaml:"temperature"`
	MaxTokens    int        `yaml:"max_tokens"`
	Timeout      Duration   `yaml:"timeout"`
}

// AuthConfig holds authentication settings.
type AuthConfig struct {
	APIKeyEnv string `yaml:"api_key_env"`
}

// StoreConfig holds configuration for all store backends.
type StoreConfig struct {
	Redis    *RedisStoreConfig    `yaml:"redis,omitempty"`
	Postgres *PostgresStoreConfig `yaml:"postgres,omitempty"`
	Memory   *MemoryStoreConfig   `yaml:"memory,omitempty"`
}

// RedisStoreConfig configures the Redis store backend.
type RedisStoreConfig struct {
	URLEnv    string   `yaml:"url_env"`
	KeyPrefix string   `yaml:"key_prefix"`
	TaskTTL   Duration `yaml:"task_ttl"`
}

// PostgresStoreConfig configures the Postgres store backend.
type PostgresStoreConfig struct {
	DSNEnv string `yaml:"dsn_env"`
	Table  string `yaml:"table"`
}

// MemoryStoreConfig configures the in-memory store backend.
type MemoryStoreConfig struct {
	MaxTasks int `yaml:"max_tasks"`
}

// Duration wraps time.Duration for YAML unmarshaling.
type Duration struct {
	time.Duration
}

func (d *Duration) UnmarshalYAML(unmarshal func(interface{}) error) error {
	var s string
	if err := unmarshal(&s); err != nil {
		return err
	}
	if s == "" {
		d.Duration = 0
		return nil
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	if parsed < 0 {
		return fmt.Errorf("negative duration not allowed: %q", s)
	}
	d.Duration = parsed
	return nil
}

func (d Duration) MarshalYAML() (interface{}, error) {
	return d.Duration.String(), nil
}
