// Orcastrator is a YAML-driven orchestration engine for AI agent pipelines.
// This binary provides the CLI for running pipelines, submitting tasks,
// validating configuration, and managing schema migrations.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	exmigrations "github.com/orcastrator/orcastrator/examples/code_review/migrations"
	"github.com/orcastrator/orcastrator/internal/agent"
	"github.com/orcastrator/orcastrator/internal/agent/registry"
	"github.com/orcastrator/orcastrator/internal/api"
	"github.com/orcastrator/orcastrator/internal/broker"
	"github.com/orcastrator/orcastrator/internal/config"
	"github.com/orcastrator/orcastrator/internal/contract"
	"github.com/orcastrator/orcastrator/internal/metrics"
	"github.com/orcastrator/orcastrator/internal/migration"
	"github.com/orcastrator/orcastrator/internal/store/memory"
	pgstore "github.com/orcastrator/orcastrator/internal/store/postgres"
	redisstore "github.com/orcastrator/orcastrator/internal/store/redis"
	"github.com/orcastrator/orcastrator/internal/tracing"

	"github.com/jackc/pgx/v5/pgxpool"
	goredis "github.com/redis/go-redis/v9"
)

func main() {
	root := rootCmd()
	registerCompletions(root)
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}

func rootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "orcastrator",
		Short: "AI Agent Orchestration Platform",
		Long: `Orcastrator is an orchestration engine for AI agent pipelines.

Pipelines are defined in YAML. Each pipeline has stages that route tasks
through LLM agents (Anthropic, OpenAI, Google, Ollama) with typed,
versioned I/O contracts and automatic prompt injection sanitization.

Quick start:
  # Validate your pipeline config
  orcastrator validate --config pipeline.yaml

  # Start the engine
  orcastrator run --config pipeline.yaml

  # Submit a task
  orcastrator submit --config pipeline.yaml --pipeline my-pipeline \
    --payload '{"request": "hello"}'

  # Check task status
  orcastrator status --config pipeline.yaml --task <task-id>

  # Watch a task until it completes
  orcastrator status --config pipeline.yaml --task <task-id> --watch`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	root.AddCommand(runCmd())
	root.AddCommand(submitCmd())
	root.AddCommand(statusCmd())
	root.AddCommand(validateCmd())
	root.AddCommand(healthCmd())
	root.AddCommand(migrateCmd())
	root.AddCommand(cancelCmd())
	root.AddCommand(pipelinesCmd())
	root.AddCommand(completionCmd())

	return root
}

// --- Shared helpers ---

func newLogger() *slog.Logger {
	level := slog.LevelInfo
	switch strings.ToLower(os.Getenv("LOG_LEVEL")) {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
}

func loadConfig(path string) (*config.Config, error) {
	return config.Load(path)
}

func configBasePath(configPath string) string {
	abs, err := filepath.Abs(configPath)
	if err != nil {
		return filepath.Dir(configPath)
	}
	return filepath.Dir(abs)
}

func buildContractRegistry(cfg *config.Config, basePath string) (*contract.Registry, error) {
	return contract.NewRegistry(cfg.SchemaRegistry, basePath)
}

func buildStore(cfg *config.Config, logger *slog.Logger) (broker.Store, error) {
	// Determine which store to use from the first pipeline's store field,
	// falling back to what's configured.
	storeType := "memory"
	for _, p := range cfg.Pipelines {
		if p.Store != "" {
			storeType = p.Store
			break
		}
	}

	switch storeType {
	case "memory":
		logger.Info("using memory store")
		return memory.New(), nil

	case "redis":
		if cfg.Stores.Redis == nil {
			return nil, fmt.Errorf("pipeline uses store: redis but no stores.redis section found in config\nHint: add a stores.redis block with url_env and key_prefix")
		}
		urlEnv := cfg.Stores.Redis.URLEnv
		redisURL := os.Getenv(urlEnv)
		if redisURL == "" {
			return nil, fmt.Errorf("environment variable %s is not set (required by stores.redis.url_env)\nHint: export %s=redis://localhost:6379", urlEnv, urlEnv)
		}
		opts, err := goredis.ParseURL(redisURL)
		if err != nil {
			return nil, fmt.Errorf("parse redis URL: %w", err)
		}
		client := goredis.NewClient(opts)
		prefix := cfg.Stores.Redis.KeyPrefix
		ttl := cfg.Stores.Redis.TaskTTL.Duration
		logger.Info("using redis store", "prefix", prefix)
		return redisstore.New(client, prefix, ttl), nil

	case "postgres":
		if cfg.Stores.Postgres == nil {
			return nil, fmt.Errorf("pipeline uses store: postgres but no stores.postgres section found in config\nHint: add a stores.postgres block with dsn_env and table")
		}
		dsnEnv := cfg.Stores.Postgres.DSNEnv
		dsn := os.Getenv(dsnEnv)
		if dsn == "" {
			return nil, fmt.Errorf("environment variable %s is not set (required by stores.postgres.dsn_env)\nHint: export %s=postgres://user:pass@localhost:5432/db", dsnEnv, dsnEnv)
		}
		pool, err := pgxpool.New(context.Background(), dsn)
		if err != nil {
			return nil, fmt.Errorf("connect to postgres: %w", err)
		}
		table := cfg.Stores.Postgres.Table
		if table == "" {
			table = "orcastrator_tasks"
		}
		logger.Info("using postgres store", "table", table)
		return pgstore.New(pool, table)

	default:
		return nil, fmt.Errorf("unknown store type %q in pipeline config\nHint: valid store types are memory, redis, postgres", storeType)
	}
}

func buildAgents(cfg *config.Config, logger *slog.Logger, m *metrics.Metrics) (map[string]broker.Agent, error) {
	agents := make(map[string]broker.Agent, len(cfg.Agents))
	for _, ac := range cfg.Agents {
		a, err := registry.NewFromConfig(ac, logger, m)
		if err != nil {
			return nil, fmt.Errorf("agent %q: %w", ac.ID, err)
		}
		agents[ac.ID] = a.(broker.Agent)
	}
	return agents, nil
}

func buildBroker(cfg *config.Config, configPath string, logger *slog.Logger, m *metrics.Metrics, t *tracing.Tracer) (*broker.Broker, error) {
	basePath := configBasePath(configPath)

	reg, err := buildContractRegistry(cfg, basePath)
	if err != nil {
		return nil, fmt.Errorf("contract registry: %w", err)
	}

	st, err := buildStore(cfg, logger)
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}

	agents, err := buildAgents(cfg, logger, m)
	if err != nil {
		return nil, fmt.Errorf("agents: %w", err)
	}

	return broker.New(cfg, st, agents, reg, logger, m, t), nil
}

// --- run command ---

func runCmd() *cobra.Command {
	var configPath string
	var port string

	cmd := &cobra.Command{
		Use:   "run",
		Short: "Start the orchestration engine",
		RunE: func(cmd *cobra.Command, args []string) error {
			logger := newLogger()

			cfg, err := loadConfig(configPath)
			if err != nil {
				return fmtConfigError(configPath, err)
			}

			basePath := configBasePath(configPath)

			// Validate schema_registry files exist.
			for _, entry := range cfg.SchemaRegistry {
				p := entry.Path
				if !filepath.IsAbs(p) {
					p = filepath.Join(basePath, p)
				}
				if _, err := os.Stat(p); err != nil {
					return fmt.Errorf("schema file not found: %s (referenced by %s@%s)\nHint: check the path in schema_registry or run 'orcastrator validate --config %s'",
						p, entry.Name, entry.Version, configPath)
				}
			}

			// Initialize observability.
			m := metrics.New()

			// Read OTLP auth headers from env var if configured (never from YAML value).
			var otlpHeaders string
			if env := cfg.Observability.Tracing.OTLPHeadersEnv; env != "" {
				otlpHeaders = os.Getenv(env)
			}
			tracerCfg := tracing.Config{
				Enabled:      cfg.Observability.Tracing.Enabled,
				Exporter:     cfg.Observability.Tracing.Exporter,
				OTLPEndpoint: cfg.Observability.Tracing.OTLPEndpoint,
				OTLPInsecure: cfg.Observability.Tracing.OTLPInsecure,
				OTLPHeaders:  otlpHeaders,
			}
			t, err := tracing.New(context.Background(), tracerCfg)
			if err != nil {
				return fmt.Errorf("tracing: %w", err)
			}
			defer t.Shutdown(context.Background())

			b, err := buildBroker(cfg, configPath, logger, m, t)
			if err != nil {
				return err
			}

			logger.Info("orcastrator starting",
				"config", configPath,
				"pipelines", len(cfg.Pipelines),
				"agents", len(cfg.Agents),
				"schemas", len(cfg.SchemaRegistry),
				"store", pipelineStoreType(cfg),
				"port", port,
				"tracing_enabled", cfg.Observability.Tracing.Enabled,
			)

			// Start broker workers.
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			var wg sync.WaitGroup
			wg.Add(1)
			go func() {
				defer wg.Done()
				b.Run(ctx)
			}()

			// Start HTTP/WS API.
			metricsPath := cfg.Observability.MetricsPath
			srv := api.NewServer(b, logger, m, metricsPath)
			ln, err := net.Listen("tcp", ":"+port)
			if err != nil {
				cancel()
				return fmt.Errorf("listen: %w", err)
			}
			logger.Info("API server listening", "addr", ln.Addr().String())

			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
					logger.Error("API server error", "error", err)
				}
			}()

			// Hot-reload on SIGHUP.
			if err := config.Watch(configPath, func(newCfg *config.Config) {
				newBasePath := configBasePath(configPath)
				newReg, err := buildContractRegistry(newCfg, newBasePath)
				if err != nil {
					logger.Error("hot-reload: contract registry failed", "error", err)
					return
				}
				newAgents, err := buildAgents(newCfg, logger, m)
				if err != nil {
					logger.Error("hot-reload: agents failed", "error", err)
					return
				}
				b.Reload(newCfg, newAgents, contract.NewValidator(newReg))
				logger.Info("config reloaded",
					"pipelines", len(newCfg.Pipelines),
					"agents", len(newCfg.Agents),
					"schemas", len(newCfg.SchemaRegistry),
				)
			}); err != nil {
				logger.Warn("config watch failed", "error", err)
			}

			// Graceful shutdown on SIGTERM/SIGINT.
			sigCh := make(chan os.Signal, 1)
			signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
			sig := <-sigCh
			logger.Info("shutting down", "signal", sig.String())

			shutCtx, shutCancel := context.WithTimeout(context.Background(), api.ShutdownTimeout)
			defer shutCancel()
			srv.Shutdown(shutCtx)

			cancel() // Stop broker workers.
			wg.Wait()
			logger.Info("shutdown complete")
			return nil
		},
	}

	cmd.Flags().StringVar(&configPath, "config", "", "path to pipeline YAML config file")
	cmd.Flags().StringVar(&port, "port", envOrDefault("ORCASTRATOR_PORT", "8080"), "HTTP server port")
	cmd.MarkFlagRequired("config")
	return cmd
}

func pipelineStoreType(cfg *config.Config) string {
	for _, p := range cfg.Pipelines {
		if p.Store != "" {
			return p.Store
		}
	}
	return "memory"
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// --- submit command ---

func submitCmd() *cobra.Command {
	var configPath string
	var pipelineID string
	var payload string
	var wait bool
	var dryRun bool
	var timeout time.Duration

	cmd := &cobra.Command{
		Use:   "submit",
		Short: "Submit a task to a pipeline",
		Long: `Submit a task to a pipeline. The payload can be inline JSON or a file
reference prefixed with @.

Use --dry-run to validate the payload against the first stage's input schema
without actually submitting the task. This is useful for debugging schema
issues without consuming API quota.`,
		Example: `  orcastrator submit --config pipeline.yaml --pipeline my-pipeline \
    --payload '{"request": "hello"}'

  orcastrator submit --config pipeline.yaml --pipeline my-pipeline \
    --payload @input.json --wait

  orcastrator submit --config pipeline.yaml --pipeline my-pipeline \
    --payload '{"request": "test"}' --dry-run`,
		RunE: func(cmd *cobra.Command, args []string) error {
			logger := newLogger()

			cfg, err := loadConfig(configPath)
			if err != nil {
				return fmtConfigError(configPath, err)
			}

			// Parse payload: @file or inline JSON.
			var payloadBytes json.RawMessage
			if strings.HasPrefix(payload, "@") {
				filePath := strings.TrimPrefix(payload, "@")
				data, err := os.ReadFile(filePath)
				if err != nil {
					return fmt.Errorf("cannot read payload file %q: %w", filePath, err)
				}
				payloadBytes = json.RawMessage(data)
			} else {
				payloadBytes = json.RawMessage(payload)
			}

			if !json.Valid(payloadBytes) {
				return fmt.Errorf("payload is not valid JSON\nHint: use --payload @file.json to read from a file")
			}

			// Dry-run: validate against first stage's input schema only.
			if dryRun {
				return dryRunSubmit(cfg, configPath, pipelineID, payloadBytes, cmd.OutOrStdout())
			}

			b, err := buildBroker(cfg, configPath, logger, nil, nil)
			if err != nil {
				return err
			}

			ctx := context.Background()
			task, err := b.Submit(ctx, pipelineID, payloadBytes)
			if err != nil {
				return fmt.Errorf("submit to pipeline %q failed: %w", pipelineID, err)
			}

			fmt.Fprintln(cmd.OutOrStdout(), task.ID)

			if wait {
				brokerCtx, brokerCancel := context.WithCancel(ctx)
				defer brokerCancel()
				go b.Run(brokerCtx)

				return pollTask(ctx, b, task.ID, timeout)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&configPath, "config", "", "path to pipeline YAML config file")
	cmd.Flags().StringVar(&pipelineID, "pipeline", "", "pipeline ID to submit to")
	cmd.Flags().StringVar(&payload, "payload", "", "JSON payload or @file")
	cmd.Flags().BoolVar(&wait, "wait", false, "poll until task completes")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "validate payload against first stage schema without submitting")
	cmd.Flags().DurationVar(&timeout, "timeout", 5*time.Minute, "max wait time when --wait is set")
	cmd.MarkFlagRequired("config")
	cmd.MarkFlagRequired("pipeline")
	cmd.MarkFlagRequired("payload")
	return cmd
}

// dryRunSubmit validates the payload against the first stage's input schema.
func dryRunSubmit(cfg *config.Config, configPath, pipelineID string, payload json.RawMessage, w interface{ Write([]byte) (int, error) }) error {
	var pipeline *config.Pipeline
	for i := range cfg.Pipelines {
		if cfg.Pipelines[i].Name == pipelineID {
			pipeline = &cfg.Pipelines[i]
			break
		}
	}
	if pipeline == nil {
		available := make([]string, len(cfg.Pipelines))
		for i, p := range cfg.Pipelines {
			available[i] = p.Name
		}
		return fmt.Errorf("pipeline %q not found\nAvailable pipelines: %s", pipelineID, strings.Join(available, ", "))
	}

	if len(pipeline.Stages) == 0 {
		return fmt.Errorf("pipeline %q has no stages", pipelineID)
	}

	stage := pipeline.Stages[0]
	basePath := configBasePath(configPath)
	reg, err := buildContractRegistry(cfg, basePath)
	if err != nil {
		return fmt.Errorf("schema registry: %w", err)
	}

	validator := contract.NewValidator(reg)
	schemaVer := contract.SchemaVersion(stage.InputSchema.Version)
	if valErr := validator.ValidateInput(stage.InputSchema.Name, schemaVer, schemaVer, payload); valErr != nil {
		return fmt.Errorf("payload validation failed for pipeline %q, stage %q (schema %s@%s):\n  %w",
			pipelineID, stage.ID, stage.InputSchema.Name, stage.InputSchema.Version, valErr)
	}

	fmt.Fprintf(w, "Payload valid for pipeline %q, stage %q (schema %s@%s)\n",
		pipelineID, stage.ID, stage.InputSchema.Name, stage.InputSchema.Version)
	return nil
}

func pollTask(ctx context.Context, b *broker.Broker, taskID string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("timeout waiting for task %s", taskID)
		case <-ticker.C:
			task, err := b.GetTask(ctx, taskID)
			if err != nil {
				continue
			}
			if task.State == broker.TaskStateDone || task.State == broker.TaskStateFailed {
				out, _ := json.MarshalIndent(task, "", "  ")
				fmt.Println(string(out))
				if task.State == broker.TaskStateFailed {
					return fmt.Errorf("task %s failed", taskID)
				}
				return nil
			}
		}
	}
}

// --- status command ---

func statusCmd() *cobra.Command {
	var configPath string
	var taskID string
	var watch bool

	cmd := &cobra.Command{
		Use:   "status",
		Short: "Get the status of a task",
		Long: `Display the current status of a task, including its stage, state,
attempt count, schema versions, and any sanitizer warnings or failure reasons.

Use --watch to poll every 2 seconds until the task reaches a terminal state
(DONE or FAILED), printing state changes as they happen.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			logger := newLogger()

			cfg, err := loadConfig(configPath)
			if err != nil {
				return fmtConfigError(configPath, err)
			}

			b, err := buildBroker(cfg, configPath, logger, nil, nil)
			if err != nil {
				return err
			}

			if watch {
				return watchTask(cmd.Context(), b, taskID, cmd.OutOrStdout())
			}

			task, err := b.GetTask(cmd.Context(), taskID)
			if err != nil {
				return fmt.Errorf("task %q not found: %w\nHint: verify the task ID with 'orcastrator submit'", taskID, err)
			}

			printTaskStatus(cmd.OutOrStdout(), task)
			return nil
		},
	}

	cmd.Flags().StringVar(&configPath, "config", "", "path to pipeline YAML config file")
	cmd.Flags().StringVar(&taskID, "task", "", "task ID")
	cmd.Flags().BoolVar(&watch, "watch", false, "poll every 2s until the task reaches a terminal state")
	cmd.MarkFlagRequired("config")
	cmd.MarkFlagRequired("task")
	return cmd
}

// printTaskStatus writes a formatted task status to the given writer.
func printTaskStatus(w interface{ Write([]byte) (int, error) }, task *broker.Task) {
	fmt.Fprintf(w, "Task:     %s\n", task.ID)
	fmt.Fprintf(w, "Pipeline: %s\n", task.PipelineID)
	fmt.Fprintf(w, "Stage:    %s\n", task.StageID)
	fmt.Fprintf(w, "Attempts: %d/%d\n", task.Attempts, task.MaxAttempts)
	fmt.Fprintf(w, "Input:    %s@%s\n", task.InputSchemaName, task.InputSchemaVersion)
	fmt.Fprintf(w, "Output:   %s@%s\n", task.OutputSchemaName, task.OutputSchemaVersion)

	// Show failure reason prominently before state for FAILED tasks.
	if task.State == broker.TaskStateFailed {
		reason := "unknown"
		if r, ok := task.Metadata["failure_reason"]; ok {
			reason = fmt.Sprintf("%v", r)
		}
		fmt.Fprintf(w, "\n*** FAILED: %s ***\n\n", reason)
	}

	fmt.Fprintf(w, "State:    %s\n", task.State)

	if warnings, ok := task.Metadata["sanitizer_warnings"]; ok {
		fmt.Fprintf(w, "Sanitizer Warnings: %v\n", warnings)
	}
}

// watchTask polls task status every 2 seconds, printing state changes.
func watchTask(ctx context.Context, b *broker.Broker, taskID string, w interface{ Write([]byte) (int, error) }) error {
	task, err := b.GetTask(ctx, taskID)
	if err != nil {
		return fmt.Errorf("task %q not found: %w\nHint: verify the task ID with 'orcastrator submit'", taskID, err)
	}

	printTaskStatus(w, task)
	lastState := task.State
	lastStage := task.StageID

	if isTerminal(task.State) {
		return nil
	}

	fmt.Fprintf(w, "\nWatching for changes (every 2s)...\n")

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			task, err = b.GetTask(ctx, taskID)
			if err != nil {
				fmt.Fprintf(w, "  error fetching task: %v\n", err)
				continue
			}

			if task.State != lastState || task.StageID != lastStage {
				ts := time.Now().Format("15:04:05")
				fmt.Fprintf(w, "  [%s] %s/%s → %s/%s (attempt %d/%d)\n",
					ts, lastStage, lastState, task.StageID, task.State,
					task.Attempts, task.MaxAttempts)
				lastState = task.State
				lastStage = task.StageID
			}

			if isTerminal(task.State) {
				fmt.Fprintf(w, "\n")
				printTaskStatus(w, task)
				return nil
			}
		}
	}
}

func isTerminal(s broker.TaskState) bool {
	return s == broker.TaskStateDone || s == broker.TaskStateFailed
}

// --- validate command ---

func validateCmd() *cobra.Command {
	var configPath string

	cmd := &cobra.Command{
		Use:   "validate",
		Short: "Validate YAML config and schema files",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(configPath)
			if err != nil {
				return fmtConfigError(configPath, err)
			}

			basePath := configBasePath(configPath)
			var errs []string

			// Check all schema_registry files exist and are valid JSONSchema.
			for _, entry := range cfg.SchemaRegistry {
				p := entry.Path
				if !filepath.IsAbs(p) {
					p = filepath.Join(basePath, p)
				}
				if _, err := os.Stat(p); err != nil {
					errs = append(errs, fmt.Sprintf("schema %s@%s: file not found: %s", entry.Name, entry.Version, p))
					continue
				}
			}

			// Try to compile the full registry.
			if _, err := buildContractRegistry(cfg, basePath); err != nil {
				errs = append(errs, err.Error())
			}

			if len(errs) > 0 {
				return fmt.Errorf("validation errors:\n%s", strings.Join(errs, "\n"))
			}

			fmt.Println("config valid")
			return nil
		},
	}

	cmd.Flags().StringVar(&configPath, "config", "", "path to pipeline YAML config file")
	cmd.MarkFlagRequired("config")
	return cmd
}

// --- health command ---

func healthCmd() *cobra.Command {
	var configPath string

	cmd := &cobra.Command{
		Use:   "health",
		Short: "Check health of all configured agents",
		RunE: func(cmd *cobra.Command, args []string) error {
			logger := newLogger()

			cfg, err := loadConfig(configPath)
			if err != nil {
				return fmtConfigError(configPath, err)
			}

			agents := make(map[string]agent.Agent, len(cfg.Agents))
			for _, ac := range cfg.Agents {
				a, err := registry.NewFromConfig(ac, logger)
				if err != nil {
					return fmt.Errorf("agent %q: %w", ac.ID, err)
				}
				agents[ac.ID] = a
			}

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			type result struct {
				id       string
				provider string
				model    string
				status   string
			}

			results := make([]result, 0, len(cfg.Agents))
			var mu sync.Mutex
			var wg sync.WaitGroup

			for _, ac := range cfg.Agents {
				ac := ac
				a := agents[ac.ID]
				wg.Add(1)
				go func() {
					defer wg.Done()
					status := "ok"
					if err := a.HealthCheck(ctx); err != nil {
						status = "error: " + err.Error()
					}
					mu.Lock()
					results = append(results, result{
						id:       ac.ID,
						provider: ac.Provider,
						model:    ac.Model,
						status:   status,
					})
					mu.Unlock()
				}()
			}
			wg.Wait()

			// Print table.
			fmt.Printf("%-20s %-12s %-30s %s\n", "AGENT ID", "PROVIDER", "MODEL", "STATUS")
			fmt.Printf("%-20s %-12s %-30s %s\n", "--------", "--------", "-----", "------")
			anyUnhealthy := false
			for _, r := range results {
				fmt.Printf("%-20s %-12s %-30s %s\n", r.id, r.provider, r.model, r.status)
				if r.status != "ok" {
					anyUnhealthy = true
				}
			}

			if anyUnhealthy {
				return fmt.Errorf("one or more agents are unhealthy")
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&configPath, "config", "", "path to pipeline YAML config file")
	cmd.MarkFlagRequired("config")
	return cmd
}

// --- migrate command ---

// defaultMigrationRegistry returns a registry with all known migrations registered.
func defaultMigrationRegistry() *migration.Registry {
	r := migration.NewRegistry()
	exmigrations.Register(r)
	return r
}

func migrateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "Schema migration tools for in-flight and historical tasks",
	}

	cmd.AddCommand(migrateListCmd())
	cmd.AddCommand(migrateRunCmd())
	cmd.AddCommand(migrateValidateCmd())
	return cmd
}

func migrateListCmd() *cobra.Command {
	var configPath string

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List all registered migrations",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(configPath)
			if err != nil {
				return fmtConfigError(configPath, err)
			}

			reg := defaultMigrationRegistry()
			all := reg.ListAll()

			if len(all) == 0 {
				fmt.Println("No migrations registered.")
				return nil
			}

			fmt.Printf("%-25s %-10s %-10s %s\n", "SCHEMA", "FROM", "TO", "PATH EXISTS")
			fmt.Printf("%-25s %-10s %-10s %s\n", "------", "----", "--", "-----------")

			for _, m := range all {
				// Check if a path exists from this migration's FromVersion to
				// the schema's current registered version.
				pathExists := "n/a"
				for _, entry := range cfg.SchemaRegistry {
					if entry.Name == m.SchemaName() {
						_, pathErr := reg.ResolvePath(m.SchemaName(), m.FromVersion(), entry.Version)
						if pathErr == nil {
							pathExists = "yes (→" + entry.Version + ")"
						} else {
							pathExists = "no (→" + entry.Version + ")"
						}
						break
					}
				}
				fmt.Printf("%-25s %-10s %-10s %s\n", m.SchemaName(), m.FromVersion(), m.ToVersion(), pathExists)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&configPath, "config", "", "path to pipeline YAML config file")
	cmd.MarkFlagRequired("config")
	return cmd
}

func migrateRunCmd() *cobra.Command {
	var (
		configPath string
		pipelineID string
		schemaName string
		fromVer    string
		toVer      string
		dryRun     bool
		batchSize  int
	)

	cmd := &cobra.Command{
		Use:   "run",
		Short: "Migrate task payloads from one schema version to another",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runMigration(configPath, pipelineID, schemaName, fromVer, toVer, dryRun, batchSize)
		},
	}

	cmd.Flags().StringVar(&configPath, "config", "", "path to pipeline YAML config file")
	cmd.Flags().StringVar(&pipelineID, "pipeline", "", "pipeline ID")
	cmd.Flags().StringVar(&schemaName, "schema", "", "schema name to migrate")
	cmd.Flags().StringVar(&fromVer, "from", "", "source schema version")
	cmd.Flags().StringVar(&toVer, "to", "", "target schema version")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print what would be migrated without writing")
	cmd.Flags().IntVar(&batchSize, "batch-size", 100, "number of tasks to process at a time")
	cmd.MarkFlagRequired("config")
	cmd.MarkFlagRequired("pipeline")
	cmd.MarkFlagRequired("schema")
	cmd.MarkFlagRequired("from")
	cmd.MarkFlagRequired("to")
	return cmd
}

func migrateValidateCmd() *cobra.Command {
	var (
		configPath string
		pipelineID string
		schemaName string
		fromVer    string
		toVer      string
	)

	cmd := &cobra.Command{
		Use:   "validate",
		Short: "Validate that all tasks can be migrated without errors",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runMigration(configPath, pipelineID, schemaName, fromVer, toVer, true, 100)
		},
	}

	cmd.Flags().StringVar(&configPath, "config", "", "path to pipeline YAML config file")
	cmd.Flags().StringVar(&pipelineID, "pipeline", "", "pipeline ID")
	cmd.Flags().StringVar(&schemaName, "schema", "", "schema name to validate migration for")
	cmd.Flags().StringVar(&fromVer, "from", "", "source schema version")
	cmd.Flags().StringVar(&toVer, "to", "", "target schema version")
	cmd.MarkFlagRequired("config")
	cmd.MarkFlagRequired("pipeline")
	cmd.MarkFlagRequired("schema")
	cmd.MarkFlagRequired("from")
	cmd.MarkFlagRequired("to")
	return cmd
}

func runMigration(configPath, pipelineID, schemaName, fromVer, toVer string, dryRun bool, batchSize int) error {
	logger := newLogger()

	cfg, err := loadConfig(configPath)
	if err != nil {
		return fmtConfigError(configPath, err)
	}

	// Build store (we don't need agents or a full broker).
	st, err := buildStore(cfg, logger)
	if err != nil {
		return fmt.Errorf("store: %w", err)
	}

	// Build contract registry for post-migration validation.
	basePath := configBasePath(configPath)
	contractReg, err := buildContractRegistry(cfg, basePath)
	if err != nil {
		return fmt.Errorf("contract registry: %w", err)
	}
	validator := contract.NewValidator(contractReg)

	// Resolve migration path.
	migReg := defaultMigrationRegistry()
	steps, err := migReg.ResolvePath(schemaName, fromVer, toVer)
	if err != nil {
		return err
	}
	if len(steps) == 0 {
		fmt.Println("Source and target versions are the same. Nothing to do.")
		return nil
	}

	fmt.Printf("Migration path: %s", fromVer)
	for _, s := range steps {
		fmt.Printf(" → %s", s.ToVersion())
	}
	fmt.Println()

	if dryRun {
		fmt.Println("DRY RUN — no changes will be written.")
	}

	ctx := context.Background()

	// Process matching tasks in batches.
	var migrated, failed, total int
	offset := 0

	for {
		filter := broker.TaskFilter{
			PipelineID: &pipelineID,
			Limit:      batchSize,
			Offset:     offset,
		}
		result, err := st.ListTasks(ctx, filter)
		if err != nil {
			return fmt.Errorf("list tasks: %w", err)
		}

		if len(result.Tasks) == 0 {
			break
		}

		for _, task := range result.Tasks {
			// Match tasks that use the old schema version on either input or output.
			if !taskMatchesSchema(task, schemaName, fromVer) {
				continue
			}
			total++

			// Apply migration chain.
			newPayload, err := migReg.Chain(ctx, schemaName, fromVer, toVer, task.Payload)
			if err != nil {
				failed++
				fmt.Printf("FAIL task %s: %v\n", task.ID, err)
				continue
			}

			// Validate migrated payload against target schema.
			schemaVersion := contract.SchemaVersion(toVer)
			valErr := validator.ValidateInput(schemaName, schemaVersion, schemaVersion, newPayload)
			if valErr != nil {
				valErr2 := validator.ValidateOutput(schemaName, schemaVersion, schemaVersion, newPayload)
				if valErr2 != nil {
					failed++
					fmt.Printf("FAIL task %s: migrated payload fails validation: %v\n", task.ID, valErr)
					continue
				}
			}

			if !dryRun {
				update := broker.TaskUpdate{
					Payload: &newPayload,
				}
				toVerStr := toVer
				if task.InputSchemaName == schemaName && task.InputSchemaVersion == fromVer {
					update.InputSchemaVersion = &toVerStr
				}
				if task.OutputSchemaName == schemaName && task.OutputSchemaVersion == fromVer {
					update.OutputSchemaVersion = &toVerStr
				}

				if err := st.UpdateTask(ctx, task.ID, update); err != nil {
					failed++
					fmt.Printf("FAIL task %s: store update failed: %v\n", task.ID, err)
					continue
				}
			}

			migrated++
			if migrated%10 == 0 || migrated == 1 {
				verb := "Migrated"
				if dryRun {
					verb = "Would migrate"
				}
				fmt.Printf("%s %d tasks...\n", verb, migrated)
			}
		}

		if len(result.Tasks) < batchSize {
			break
		}
		offset += batchSize
	}

	if total == 0 {
		fmt.Println("No tasks found matching the specified schema and version.")
		return nil
	}

	verb := "Migrated"
	if dryRun {
		verb = "Would migrate"
	}
	fmt.Printf("%s %d/%d tasks", verb, migrated, total)
	if failed > 0 {
		fmt.Printf(" (%d failed)", failed)
	}
	fmt.Println()

	if failed > 0 {
		return fmt.Errorf("%d tasks failed migration", failed)
	}
	return nil
}

func taskMatchesSchema(task *broker.Task, schemaName, version string) bool {
	if task.InputSchemaName == schemaName && task.InputSchemaVersion == version {
		return true
	}
	if task.OutputSchemaName == schemaName && task.OutputSchemaVersion == version {
		return true
	}
	return false
}

// --- cancel command ---

func cancelCmd() *cobra.Command {
	var configPath string
	var taskID string

	cmd := &cobra.Command{
		Use:   "cancel",
		Short: "Cancel an in-flight task",
		Long: `Cancel a task by setting its state to FAILED with the reason
"cancelled by operator".

If the task is currently EXECUTING, cancellation takes effect on the next
state check. The current execution may complete first (at-least-once
semantics). The task will not be routed to any further stages.

If the task is already in a terminal state (DONE or FAILED), an error
is returned.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			logger := newLogger()

			cfg, err := loadConfig(configPath)
			if err != nil {
				return fmtConfigError(configPath, err)
			}

			b, err := buildBroker(cfg, configPath, logger, nil, nil)
			if err != nil {
				return err
			}

			return cancelTask(cmd.Context(), b, taskID, cmd.OutOrStdout())
		},
	}

	cmd.Flags().StringVar(&configPath, "config", "", "path to pipeline YAML config file")
	cmd.Flags().StringVar(&taskID, "task", "", "task ID to cancel")
	cmd.MarkFlagRequired("config")
	cmd.MarkFlagRequired("task")
	return cmd
}

func cancelTask(ctx context.Context, b *broker.Broker, taskID string, w interface{ Write([]byte) (int, error) }) error {
	task, err := b.GetTask(ctx, taskID)
	if err != nil {
		return fmt.Errorf("task %q not found: %w", taskID, err)
	}

	if isTerminal(task.State) {
		return fmt.Errorf("task %s is already in terminal state %s and cannot be cancelled", taskID, task.State)
	}

	failedState := broker.TaskStateFailed
	update := broker.TaskUpdate{
		State: &failedState,
		Metadata: map[string]any{
			"failure_reason": "cancelled by operator",
		},
	}

	if err := b.Store().UpdateTask(ctx, taskID, update); err != nil {
		return fmt.Errorf("failed to cancel task %s: %w", taskID, err)
	}

	fmt.Fprintf(w, "Task %s cancelled.\n", taskID)
	if task.State == broker.TaskStateExecuting {
		fmt.Fprintf(w, "Note: task was EXECUTING. The current execution may complete, but the task\nwill not be routed to further stages.\n")
	}
	return nil
}

// --- pipelines command ---

func pipelinesCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "pipelines",
		Short: "Inspect configured pipelines",
	}

	cmd.AddCommand(pipelinesListCmd())
	cmd.AddCommand(pipelinesShowCmd())
	return cmd
}

func pipelinesListCmd() *cobra.Command {
	var configPath string

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List all configured pipelines",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(configPath)
			if err != nil {
				return fmtConfigError(configPath, err)
			}

			w := cmd.OutOrStdout()
			if len(cfg.Pipelines) == 0 {
				fmt.Fprintln(w, "No pipelines configured.")
				return nil
			}

			fmt.Fprintf(w, "%-25s %-8s %-10s %s\n", "PIPELINE", "STAGES", "STORE", "AGENTS")
			fmt.Fprintf(w, "%-25s %-8s %-10s %s\n", "--------", "------", "-----", "------")
			for _, p := range cfg.Pipelines {
				agents := make([]string, 0, len(p.Stages))
				seen := make(map[string]bool)
				for _, st := range p.Stages {
					if !seen[st.Agent] {
						agents = append(agents, st.Agent)
						seen[st.Agent] = true
					}
				}
				fmt.Fprintf(w, "%-25s %-8d %-10s %s\n",
					p.Name, len(p.Stages), p.Store, strings.Join(agents, ", "))
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&configPath, "config", "", "path to pipeline YAML config file")
	cmd.MarkFlagRequired("config")
	return cmd
}

func pipelinesShowCmd() *cobra.Command {
	var configPath string
	var pipelineID string

	cmd := &cobra.Command{
		Use:   "show",
		Short: "Show full topology of a pipeline",
		Long: `Display the full topology of a pipeline as a readable tree, showing each
stage's agent, schema versions, retry policy, and routing targets.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(configPath)
			if err != nil {
				return fmtConfigError(configPath, err)
			}

			var pipeline *config.Pipeline
			for i := range cfg.Pipelines {
				if cfg.Pipelines[i].Name == pipelineID {
					pipeline = &cfg.Pipelines[i]
					break
				}
			}
			if pipeline == nil {
				available := make([]string, len(cfg.Pipelines))
				for i, p := range cfg.Pipelines {
					available[i] = p.Name
				}
				return fmt.Errorf("pipeline %q not found\nAvailable pipelines: %s", pipelineID, strings.Join(available, ", "))
			}

			w := cmd.OutOrStdout()
			fmt.Fprintf(w, "Pipeline: %s\n", pipeline.Name)
			fmt.Fprintf(w, "Concurrency: %d\n", pipeline.Concurrency)
			fmt.Fprintf(w, "Store: %s\n", pipeline.Store)
			fmt.Fprintf(w, "\n")

			for i, st := range pipeline.Stages {
				prefix := "├── "
				childPrefix := "│   "
				if i == len(pipeline.Stages)-1 {
					prefix = "└── "
					childPrefix = "    "
				}

				fmt.Fprintf(w, "%s[%s] agent=%s\n", prefix, st.ID, st.Agent)
				fmt.Fprintf(w, "%sInput:      %s@%s\n", childPrefix, st.InputSchema.Name, st.InputSchema.Version)
				fmt.Fprintf(w, "%sOutput:     %s@%s\n", childPrefix, st.OutputSchema.Name, st.OutputSchema.Version)
				fmt.Fprintf(w, "%sTimeout:    %s\n", childPrefix, st.Timeout.Duration)
				fmt.Fprintf(w, "%sRetry:      max=%d backoff=%s delay=%s\n", childPrefix,
					st.Retry.MaxAttempts, st.Retry.Backoff, st.Retry.BaseDelay.Duration)
				fmt.Fprintf(w, "%sOn success: %s\n", childPrefix, st.OnSuccess)
				fmt.Fprintf(w, "%sOn failure: %s\n", childPrefix, st.OnFailure)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&configPath, "config", "", "path to pipeline YAML config file")
	cmd.Flags().StringVar(&pipelineID, "pipeline", "", "pipeline ID to show")
	cmd.MarkFlagRequired("config")
	cmd.MarkFlagRequired("pipeline")
	return cmd
}

// --- completion command ---

func completionCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "completion [bash|zsh]",
		Short: "Generate shell completion scripts",
		Long: `Generate shell completion scripts for orcastrator.

To load completions:

  # Bash (add to ~/.bashrc for persistence)
  source <(orcastrator completion bash)

  # Zsh (add to ~/.zshrc for persistence)
  source <(orcastrator completion zsh)`,
		Args:      cobra.ExactArgs(1),
		ValidArgs: []string{"bash", "zsh"},
		RunE: func(cmd *cobra.Command, args []string) error {
			switch args[0] {
			case "bash":
				return cmd.Root().GenBashCompletionV2(cmd.OutOrStdout(), true)
			case "zsh":
				return cmd.Root().GenZshCompletion(cmd.OutOrStdout())
			default:
				return fmt.Errorf("unsupported shell %q: use bash or zsh", args[0])
			}
		},
	}
	return cmd
}

// registerCompletions adds custom flag completions for --config and --pipeline.
func registerCompletions(root *cobra.Command) {
	root.PersistentFlags().Lookup("config")

	// Walk all commands and register completions for known flags.
	registerCompletionsRecursive(root)
}

func registerCompletionsRecursive(cmd *cobra.Command) {
	if f := cmd.Flags().Lookup("config"); f != nil {
		cmd.RegisterFlagCompletionFunc("config", func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
			return []string{"yaml", "yml"}, cobra.ShellCompDirectiveFilterFileExt
		})
	}

	if f := cmd.Flags().Lookup("pipeline"); f != nil {
		cmd.RegisterFlagCompletionFunc("pipeline", completePipelineIDs)
	}

	for _, sub := range cmd.Commands() {
		registerCompletionsRecursive(sub)
	}
}

// completePipelineIDs reads the config file from --config and returns pipeline names.
func completePipelineIDs(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	configPath, _ := cmd.Flags().GetString("config")
	if configPath == "" {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}

	cfg, err := loadConfig(configPath)
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}

	names := make([]string, 0, len(cfg.Pipelines))
	for _, p := range cfg.Pipelines {
		if strings.HasPrefix(p.Name, toComplete) {
			names = append(names, p.Name)
		}
	}
	return names, cobra.ShellCompDirectiveNoFileComp
}

// --- error formatting helpers (Scenario 6) ---

// fmtConfigError wraps config loading errors with actionable hints.
func fmtConfigError(configPath string, err error) error {
	if os.IsNotExist(err) || strings.Contains(err.Error(), "no such file") || strings.Contains(err.Error(), "config file not found") {
		return fmt.Errorf("config file not found: %s\nHint: check the path or create a config from config/examples/", configPath)
	}
	if strings.Contains(err.Error(), "symlink") || strings.Contains(err.Error(), "not a regular file") {
		return fmt.Errorf("config path rejected: %w\nHint: config must be a regular file, not a symlink or special file", err)
	}
	if strings.Contains(err.Error(), "yaml:") || strings.Contains(err.Error(), "invalid YAML structure") {
		return fmt.Errorf("invalid YAML in %s: %w\nHint: validate syntax with 'orcastrator validate --config %s'", configPath, err, configPath)
	}
	return fmt.Errorf("failed to load config %s: %w", configPath, err)
}
