package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/spf13/cobra"

	"github.com/brianbuquoi/overlord/internal/chain"
	"github.com/brianbuquoi/overlord/internal/workflow"
)

// workflowRunArgs carries the flags `overlord run` passes into the
// workflow execution path.
type workflowRunArgs struct {
	input     string
	inputFile string
	outputFmt string
	timeout   time.Duration
	quiet     bool
}

// runWorkflow executes a workflow file once and prints the final
// output. Thin wrapper over workflow.Run — mirrors the shape of
// `overlord chain run` so documentation can share language.
func runWorkflow(cmd *cobra.Command, configPath string, a workflowRunArgs) error {
	if a.outputFmt != "text" && a.outputFmt != "json" {
		return fmt.Errorf("--output must be text or json, got %q", a.outputFmt)
	}
	input := a.input
	if input != "" && a.inputFile != "" {
		return fmt.Errorf("--input and --input-file are mutually exclusive")
	}
	if a.inputFile != "" {
		data, err := readInputFile(a.inputFile, cmd.InOrStdin())
		if err != nil {
			return err
		}
		input = string(data)
	}

	file, err := workflow.Load(configPath)
	if err != nil {
		return err
	}

	basePath, err := filepath.Abs(filepath.Dir(configPath))
	if err != nil {
		return fmt.Errorf("resolve base path: %w", err)
	}

	stderr := cmd.ErrOrStderr()
	stdout := cmd.OutOrStdout()
	if !a.quiet {
		fmt.Fprintf(stderr, "workflow: running %s\n", file.Workflow.ID)
	}

	runCtx, cancel := context.WithCancel(cmd.Context())
	defer cancel()
	result, err := workflow.Run(runCtx, file, basePath, workflow.RunOptions{
		Input:   input,
		Timeout: a.timeout,
		Logger:  newLogger(),
	})
	if err != nil {
		return err
	}

	if !a.quiet {
		fmt.Fprintf(stderr, "workflow: task %s completed in state %s\n", result.Task.ID, result.Task.State)
	}
	writeWorkflowOutput(stdout, a.outputFmt, result)
	return nil
}

// readInputFile honors the stdin sentinel "-" so pipes and heredocs
// flow straight through without the caller writing a temp file.
func readInputFile(path string, stdin io.Reader) ([]byte, error) {
	if path == "-" {
		data, err := io.ReadAll(stdin)
		if err != nil {
			return nil, fmt.Errorf("read stdin: %w", err)
		}
		return data, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read input file: %w", err)
	}
	return data, nil
}

// writeWorkflowOutput prints the workflow's final output in the
// caller-selected format. `text` prints the bare payload; `json`
// emits a small envelope that names the task state so tooling can
// chain without reading the process exit code.
func writeWorkflowOutput(w io.Writer, format string, r *chain.RunResult) {
	if r == nil {
		return
	}
	switch format {
	case "json":
		envelope := map[string]any{
			"task_id": r.Task.ID,
			"state":   string(r.Task.State),
			"output":  r.Output,
		}
		if raw := r.Task.Payload; len(raw) > 0 {
			envelope["payload"] = json.RawMessage(raw)
		}
		b, _ := json.MarshalIndent(envelope, "", "  ")
		fmt.Fprintln(w, string(b))
	default:
		fmt.Fprintln(w, r.Output)
	}
}

// --- serve command ---

// serveCmd is the default long-running service entrypoint. For
// workflow-shaped configs it compiles the workflow into the strict
// runtime config (applying the author's runtime: block) and hands it
// to the shared server loop. For strict pipeline configs it behaves
// exactly like `overlord run --config <pipeline.yaml>` did.
func serveCmd() *cobra.Command {
	var configPath string
	var port string
	var bindFlag string
	var allowPublicNoauth bool

	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Serve a workflow as a long-running local service",
		Long: `Serve starts the full broker, HTTP API, and embedded dashboard for
either a workflow file or a strict pipeline config. The runtime: block
in a workflow file controls bind address, store backend (memory |
postgres | redis), dashboard, and auth — use it to graduate from
one-shot local runs to a long-lived service without changing file
formats.

Examples:
  overlord serve
  overlord serve --config ./overlord.yaml
  overlord serve --bind 0.0.0.0:8080`,
		RunE: func(cmd *cobra.Command, args []string) error {
			effectiveConfig := configPath
			if effectiveConfig == "" {
				effectiveConfig = "overlord.yaml"
			}

			if workflow.IsWorkflowFile(effectiveConfig) {
				file, err := workflow.Load(effectiveConfig)
				if err != nil {
					return err
				}
				basePath, err := filepath.Abs(filepath.Dir(effectiveConfig))
				if err != nil {
					return fmt.Errorf("resolve base path: %w", err)
				}
				compiled, err := workflow.Compile(file, basePath)
				if err != nil {
					return err
				}
				// Author-selected bind wins when the flag is unset;
				// explicit flags still override so operators can
				// temporarily rebind without editing the file.
				effectiveBind := bindFlag
				if effectiveBind == "" && compiled.Runtime != nil {
					effectiveBind = compiled.Runtime.Bind
				}
				return runServerFromConfig(cmd, serverArgs{
					configPath:        effectiveConfig,
					port:              port,
					bindFlag:          effectiveBind,
					allowPublicNoauth: allowPublicNoauth,
					hotReload:         false,
					compiledCfg:       compiled.Config,
					compiledRegistry:  compiled.Registry,
					baseDir:           basePath,
				})
			}

			return runServerFromConfig(cmd, serverArgs{
				configPath:        effectiveConfig,
				port:              port,
				bindFlag:          bindFlag,
				allowPublicNoauth: allowPublicNoauth,
				hotReload:         true,
			})
		},
	}

	cmd.Flags().StringVar(&configPath, "config", "", "path to overlord.yaml (defaults to ./overlord.yaml)")
	cmd.Flags().StringVar(&port, "port", envOrDefault("OVERLORD_PORT", "8080"), "HTTP server port")
	cmd.Flags().StringVar(&bindFlag, "bind", envOrDefault("OVERLORD_BIND", ""), "HTTP bind address (host or host:port)")
	cmd.Flags().BoolVar(&allowPublicNoauth, "allow-public-noauth", false, "explicitly allow non-loopback bind with auth disabled")
	return cmd
}

// --- export command ---

// exportCmd writes the full-pipeline equivalent of a workflow. This
// is the graduation path for authors who need advanced capabilities
// (fan-out, conditional routing, retry budgets, explicit schemas,
// split infra/pipeline configs) — the exported directory is a normal
// overlord.yaml + schemas/ + fixtures/ project the strict runtime
// accepts.
func exportCmd() *cobra.Command {
	var (
		configPath string
		outDir     string
		advanced   bool
		stdoutFlag bool
	)
	cmd := &cobra.Command{
		Use:   "export",
		Short: "Export a workflow to the advanced strict config format",
		Long: `Export compiles a workflow and emits the equivalent full-pipeline
project — ` + "`" + `overlord.yaml` + "`" + ` plus ` + "`" + `schemas/` + "`" + ` and any referenced fixtures —
into --out. The exported directory runs unchanged under ` + "`" + `overlord run` + "`" + `
/ ` + "`" + `overlord exec` + "`" + ` and can be hand-edited to add fan-out stages,
conditional routing, retry budgets, or split infra/pipeline configs.

The --advanced flag is required as an explicit acknowledgement that
this is the escape hatch from the beginner-friendly workflow surface.

Examples:
  overlord export --advanced --out ./advanced
  overlord export --advanced --config ./overlord.yaml --stdout`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !advanced {
				return fmt.Errorf("export requires --advanced (this is the graduation path from workflows to the strict config format)")
			}
			effectiveConfig := configPath
			if effectiveConfig == "" {
				effectiveConfig = "overlord.yaml"
			}
			if !workflow.IsWorkflowFile(effectiveConfig) {
				return fmt.Errorf("export is only supported for workflow-shaped configs; %s already looks like a strict pipeline config", effectiveConfig)
			}
			file, err := workflow.Load(effectiveConfig)
			if err != nil {
				return err
			}
			basePath, err := filepath.Abs(filepath.Dir(effectiveConfig))
			if err != nil {
				return fmt.Errorf("resolve base path: %w", err)
			}
			compiled, err := workflow.Compile(file, basePath)
			if err != nil {
				return err
			}
			files, err := workflow.Export(compiled)
			if err != nil {
				return err
			}
			if stdoutFlag {
				out := cmd.OutOrStdout()
				fmt.Fprintln(out, "# --- pipeline yaml ---")
				out.Write(files.Pipeline)
				names := sortedKeys(files.Schemas)
				for _, name := range names {
					fmt.Fprintf(out, "\n# --- schema: %s ---\n", name)
					out.Write(files.Schemas[name])
				}
				return nil
			}
			if outDir == "" {
				return fmt.Errorf("--out is required (or pass --stdout)")
			}
			if err := files.WriteTo(outDir); err != nil {
				return err
			}
			stderr := cmd.ErrOrStderr()
			fmt.Fprintf(stderr, "export: wrote advanced config for workflow %q to %s\n", file.Workflow.ID, outDir)
			fmt.Fprintln(stderr, "  overlord.yaml")
			for _, name := range sortedKeys(files.Schemas) {
				fmt.Fprintf(stderr, "  schemas/%s\n", name)
			}
			for _, name := range sortedKeys(files.Fixtures) {
				fmt.Fprintf(stderr, "  %s\n", name)
			}
			fmt.Fprintln(stderr, "\nNext steps:")
			fmt.Fprintf(stderr, "  overlord validate --config %s/overlord.yaml\n", outDir)
			fmt.Fprintf(stderr, "  overlord serve --config %s/overlord.yaml\n", outDir)
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "path to overlord.yaml (defaults to ./overlord.yaml)")
	cmd.Flags().StringVar(&outDir, "out", "", "output directory")
	cmd.Flags().BoolVar(&advanced, "advanced", false, "required — acknowledge the escape hatch")
	cmd.Flags().BoolVar(&stdoutFlag, "stdout", false, "write the exported yaml and schemas to stdout")
	return cmd
}

// sortedKeys returns the map keys in deterministic order so the
// export writer and CLI listing match across runs.
func sortedKeys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
