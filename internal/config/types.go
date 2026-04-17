package config

import (
	"fmt"
	"time"

	"github.com/brianbuquoi/overlord/internal/routing"
)

// Config is the top-level configuration, mapping 1:1 to the YAML file.
type Config struct {
	Version        string              `yaml:"version"`
	SchemaRegistry []SchemaEntry       `yaml:"schema_registry"`
	Plugins        PluginConfig        `yaml:"plugins"`
	Pipelines      []Pipeline          `yaml:"pipelines"`
	Agents         []Agent             `yaml:"agents"`
	Stores         StoreConfig         `yaml:"stores"`
	Observability  ObservabilityConfig `yaml:"observability"`
	Auth           APIAuthConfig       `yaml:"auth"`
	Dashboard      DashboardConfig     `yaml:"dashboard"`
}

// PluginConfig controls plugin discovery and loading.
type PluginConfig struct {
	Dir   string   `yaml:"dir,omitempty"`   // directory to scan for .so files
	Files []string `yaml:"files,omitempty"` // explicit list of .so file paths
}

// DashboardConfig controls the embedded web dashboard.
type DashboardConfig struct {
	Enabled *bool  `yaml:"enabled,omitempty"`
	Path    string `yaml:"path,omitempty"`
}

// DashboardEnabled returns whether the dashboard is enabled (default: true).
func (d DashboardConfig) DashboardEnabled() bool {
	if d.Enabled == nil {
		return true
	}
	return *d.Enabled
}

// DashboardPath returns the dashboard serve path (default: "/dashboard").
// The path is normalized: it always starts with "/" and never ends with "/".
func (d DashboardConfig) DashboardPath() string {
	p := d.Path
	if p == "" {
		return "/dashboard"
	}
	// Ensure leading slash.
	if p[0] != '/' {
		p = "/" + p
	}
	// Strip trailing slash (unless path is just "/").
	for len(p) > 1 && p[len(p)-1] == '/' {
		p = p[:len(p)-1]
	}
	return p
}

// APIAuthConfig holds top-level authentication settings for the HTTP API.
type APIAuthConfig struct {
	Enabled bool            `yaml:"enabled"`
	Keys    []AuthKeyConfig `yaml:"keys"`
}

// AuthKeyConfig declares an API key in the YAML config. The actual key value
// is read from the environment variable named by KeyEnv at startup.
type AuthKeyConfig struct {
	Name   string   `yaml:"name"`
	KeyEnv string   `yaml:"key_env"`
	Scopes []string `yaml:"scopes"`
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

// RetryBudgetConfig limits total retry attempts within a sliding time window.
// Tracked at pipeline level and/or agent level.
type RetryBudgetConfig struct {
	MaxRetries  int      `yaml:"max_retries"`
	Window      Duration `yaml:"window"`
	OnExhausted string   `yaml:"on_exhausted"` // "fail" (default) or "wait"
}

// DefaultMaxStageTransitions is the default limit for cross-stage transitions
// before a task is dead-lettered as a safety measure against routing cycles.
const DefaultMaxStageTransitions = 50

// Pipeline defines a named pipeline with its stages.
type Pipeline struct {
	Name                string             `yaml:"name"`
	Concurrency         int                `yaml:"concurrency"`
	Store               string             `yaml:"store"`
	Stages              []Stage            `yaml:"stages"`
	RetryBudget         *RetryBudgetConfig `yaml:"retry_budget,omitempty"`
	MaxStageTransitions int                `yaml:"max_stage_transitions,omitempty"`
}

// Stage is a single step in a pipeline. A stage is either a single-agent
// stage (Agent is set) or a fan-out stage (FanOut is set). They are mutually
// exclusive.
type Stage struct {
	ID              string          `yaml:"id"`
	Agent           string          `yaml:"agent,omitempty"`
	FanOut          *FanOutConfig   `yaml:"fan_out,omitempty"`
	InputSchema     StageSchemaRef  `yaml:"input_schema"`
	OutputSchema    StageSchemaRef  `yaml:"output_schema"`
	AggregateSchema *StageSchemaRef `yaml:"aggregate_schema,omitempty"`
	Timeout         Duration        `yaml:"timeout"`
	Retry           RetryPolicy     `yaml:"retry"`
	OnSuccess       OnSuccessConfig `yaml:"on_success"`
	OnFailure       string          `yaml:"on_failure"`
}

// FanOutMode determines how fan-out agents are executed.
type FanOutMode string

const (
	FanOutModeGather FanOutMode = "gather"
	FanOutModeRace   FanOutMode = "race"
)

// RequirePolicy determines how many agents must succeed.
type RequirePolicy string

const (
	RequirePolicyAll      RequirePolicy = "all"
	RequirePolicyAny      RequirePolicy = "any"
	RequirePolicyMajority RequirePolicy = "majority"
)

// FanOutConfig configures parallel agent execution for a stage.
type FanOutConfig struct {
	Agents  []FanOutAgent `yaml:"agents"`
	Mode    FanOutMode    `yaml:"mode"`
	Timeout Duration      `yaml:"timeout"`
	Require RequirePolicy `yaml:"require"`
}

// FanOutAgent references an agent to include in a fan-out stage.
type FanOutAgent struct {
	ID     string `yaml:"id"`
	Weight int    `yaml:"weight,omitempty"`
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
	ID           string             `yaml:"id"`
	Provider     string             `yaml:"provider"`
	Model        string             `yaml:"model"`
	Auth         AuthConfig         `yaml:"auth"`
	SystemPrompt string             `yaml:"system_prompt"`
	Temperature  float64            `yaml:"temperature"`
	MaxTokens    int                `yaml:"max_tokens"`
	Timeout      Duration           `yaml:"timeout"`
	RetryBudget  *RetryBudgetConfig `yaml:"retry_budget,omitempty"`
	Extra        map[string]any     `yaml:"extra,omitempty"` // arbitrary provider-specific config (passed to plugins)
	// Fixtures maps stage_id → fixture file path (relative to the config
	// directory) for the first-class mock provider. Ignored by every other
	// provider. Fixture files are loaded and validated against the stage's
	// output schema at agent-construction time; see
	// internal/agent/mock/mock.go for details.
	Fixtures map[string]string `yaml:"fixtures,omitempty"`
	// ManifestPath is the path to a subprocess-plugin manifest file. Only
	// consulted when Provider == "plugin". Resolved relative to the process
	// working directory.
	ManifestPath string `yaml:"manifest,omitempty"`
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

// OnSuccessConfig is a union type for the on_success field. It can be either
// a simple string (backward compatible) or a conditional routing block with
// routes and a default.
type OnSuccessConfig struct {
	routing.RouteConfig
}

// StaticOnSuccess creates an OnSuccessConfig for a simple string target.
func StaticOnSuccess(target string) OnSuccessConfig {
	return OnSuccessConfig{RouteConfig: routing.StaticRoute(target)}
}

// onSuccessYAML is the YAML representation of a conditional routing block.
type onSuccessYAML struct {
	Routes  []onSuccessRouteYAML `yaml:"routes"`
	Default string               `yaml:"default"`
}

// onSuccessRouteYAML is a single conditional route in YAML.
type onSuccessRouteYAML struct {
	Condition string `yaml:"condition"`
	Stage     string `yaml:"stage"`
}

// MarshalYAML emits on_success in the form it was declared in: a bare
// string for static routes, a routing block for conditional routes.
// Without this method, yaml.v3 falls back to the default struct
// encoding and leaks internal field names (Static, Routes,
// IsConditional) into the exported YAML, which then fails to re-load
// because UnmarshalYAML does not accept that shape. The chain-mode
// exporter and the pipelines command both rely on round-trippable
// YAML; adding MarshalYAML is the narrow fix for both.
func (o OnSuccessConfig) MarshalYAML() (interface{}, error) {
	if !o.IsConditional {
		return o.Static, nil
	}
	routes := make([]onSuccessRouteYAML, 0, len(o.Routes))
	for _, r := range o.Routes {
		routes = append(routes, onSuccessRouteYAML{
			Condition: r.RawExpr,
			Stage:     r.Stage,
		})
	}
	return onSuccessYAML{Routes: routes, Default: o.Default}, nil
}

// UnmarshalYAML handles both string and mapping forms of on_success.
func (o *OnSuccessConfig) UnmarshalYAML(unmarshal func(interface{}) error) error {
	// Try string first (backward compatible form).
	var s string
	if err := unmarshal(&s); err == nil {
		o.RouteConfig = routing.StaticRoute(s)
		return nil
	}

	// Try routing block.
	var block onSuccessYAML
	if err := unmarshal(&block); err != nil {
		return fmt.Errorf("on_success must be a string or a routing block: %w", err)
	}

	if len(block.Routes) == 0 {
		return fmt.Errorf("on_success routing block must have at least one route")
	}
	if block.Default == "" {
		return fmt.Errorf("on_success routing block requires a 'default' field")
	}

	routes := make([]routing.ConditionalRoute, 0, len(block.Routes))
	for i, r := range block.Routes {
		if r.Condition == "" {
			return fmt.Errorf("on_success route[%d]: condition must not be empty", i)
		}
		if r.Stage == "" {
			return fmt.Errorf("on_success route[%d]: stage must not be empty", i)
		}
		cond, err := routing.Parse(r.Condition)
		if err != nil {
			return fmt.Errorf("on_success route[%d]: %w", i, err)
		}
		routes = append(routes, routing.ConditionalRoute{
			Condition: cond,
			Stage:     r.Stage,
			RawExpr:   r.Condition,
		})
	}

	o.RouteConfig = routing.RouteConfig{
		Routes:        routes,
		Default:       block.Default,
		IsConditional: true,
	}
	return nil
}
