package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/brianbuquoi/overlord/internal/chain"
)

func sortStrings(s []string) { sort.Strings(s) }

// chainCmd is the top-level `overlord chain` command. It groups the
// authoring-layer subcommands (`run`, `init`, `inspect`, `export`)
// behind a single namespace so the existing pipeline-mode commands —
// run, exec, init, validate, dead-letter, etc. — remain untouched.
//
// The distinction is deliberate:
//
//   - `overlord run` / `overlord exec` / `overlord init` serve full
//     pipeline mode (strict schemas, agents, stages, retries).
//   - `overlord chain run` / `overlord chain init` serve chain mode
//     (a minimal authoring shape compiled into the same runtime).
//
// No command is aliased across the two layers; users pick their layer
// by picking the verb.
func chainCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "chain",
		Short: "Lightweight prompt-chain authoring layer",
		Long: `Chain mode is Overlord's simple authoring layer for linear prompt workflows.

Chains are a thin front door on top of the existing pipeline runtime:
authors write a short YAML with steps (each a prompt and a model), and
the chain compiles into a full pipeline the broker already knows how
to run. Graduating to full pipeline mode is an explicit ` + "`" + `chain export` + "`" + `
step — you never lose access to retries, dead-letter replay, or the
dashboard; you just opt into them when you are ready.

Subcommands:
  run       Run a chain end-to-end with a single input.
  init      Scaffold a starter chain project.
  inspect   Show the compiled interpretation of a chain.
  export    Emit the equivalent full-pipeline YAML and schemas.`,
	}
	cmd.AddCommand(chainRunCmd())
	cmd.AddCommand(chainInitCmd())
	cmd.AddCommand(chainInspectCmd())
	cmd.AddCommand(chainExportCmd())
	return cmd
}

// ---------- chain run ----------

func chainRunCmd() *cobra.Command {
	var (
		chainPath string
		input     string
		inputFile string
		outputFmt string
		timeout   time.Duration
		quiet     bool
	)
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Run a chain end-to-end with a single input",
		Long: `Run compiles the chain file, builds an in-process broker with a memory
store, submits a single task carrying the given input, and prints the
final step's output.

Examples:
  overlord chain run --chain ./chain.yaml --input "summarize this"
  overlord chain run --chain ./chain.yaml --input-file ./prompt.txt
  overlord chain run --chain ./chain.yaml --input "..." --output json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if outputFmt != "text" && outputFmt != "json" {
				return fmt.Errorf("--output must be text or json, got %q", outputFmt)
			}
			if input != "" && inputFile != "" {
				return fmt.Errorf("--input and --input-file are mutually exclusive")
			}
			if inputFile != "" {
				var (
					data []byte
					err  error
				)
				if inputFile == "-" {
					// Documented stdin sentinel — read until EOF so the
					// chain can be driven by pipes and heredocs without
					// touching the filesystem.
					data, err = io.ReadAll(cmd.InOrStdin())
					if err != nil {
						return fmt.Errorf("read stdin: %w", err)
					}
				} else {
					data, err = os.ReadFile(inputFile)
					if err != nil {
						return fmt.Errorf("read input file: %w", err)
					}
				}
				input = string(data)
			}

			ch, err := chain.Load(chainPath)
			if err != nil {
				return err
			}

			basePath, err := filepath.Abs(filepath.Dir(chainPath))
			if err != nil {
				return fmt.Errorf("resolve chain base path: %w", err)
			}

			stderr := cmd.ErrOrStderr()
			stdout := cmd.OutOrStdout()
			if !quiet {
				fmt.Fprintf(stderr, "chain: compiling %s\n", ch.ID)
			}

			runCtx, cancel := context.WithCancel(cmd.Context())
			defer cancel()
			result, err := chain.Run(runCtx, ch, basePath, chain.RunOptions{
				Input:   input,
				Timeout: timeout,
				Logger:  newLogger(),
			})
			if err != nil {
				return err
			}

			if !quiet {
				fmt.Fprintf(stderr, "chain: task %s completed in state %s\n", result.Task.ID, result.Task.State)
			}
			writeChainOutput(stdout, outputFmt, result)
			return nil
		},
	}
	cmd.Flags().StringVar(&chainPath, "chain", "", "path to chain YAML file (required)")
	cmd.Flags().StringVar(&input, "input", "", "chain input text (or use --input-file)")
	cmd.Flags().StringVar(&inputFile, "input-file", "", "read chain input from a file (use \"-\" for stdin)")
	cmd.Flags().StringVar(&outputFmt, "output", "text", "output format: text | json")
	cmd.Flags().DurationVar(&timeout, "timeout", 5*time.Minute, "maximum time to wait for terminal state")
	cmd.Flags().BoolVar(&quiet, "quiet", false, "suppress progress output")
	_ = cmd.MarkFlagRequired("chain")
	return cmd
}

func writeChainOutput(w io.Writer, format string, r *chain.RunResult) {
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

// ---------- chain init ----------

func chainInitCmd() *cobra.Command {
	var (
		force bool
	)
	cmd := &cobra.Command{
		Use:   "init [template] [dir]",
		Short: "Scaffold a new chain project",
		Long: `Scaffold an embedded chain template into a directory. The scaffolded
project uses the built-in mock provider by default so you see a real
chain run with zero credentials — run it with:

  overlord chain run --chain <dir>/chain.yaml --input-file <dir>/sample_input.txt

Available templates: ` + strings.Join(chain.ListTemplates(), ", "),
		Args: cobra.MaximumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := "write-review"
			if len(args) >= 1 && args[0] != "" {
				name = args[0]
			}
			target := "./" + name
			if len(args) >= 2 && args[1] != "" {
				target = args[1]
			}
			res, err := chain.Scaffold(name, target, chain.ScaffoldOptions{Force: force})
			if err != nil {
				return err
			}
			stderr := cmd.ErrOrStderr()
			fmt.Fprintf(stderr, "chain: scaffolded %s into %s\n", name, res.Target)
			for _, f := range res.Files {
				fmt.Fprintf(stderr, "  %s\n", f)
			}
			fmt.Fprintln(stderr, "\nNext steps:")
			fmt.Fprintf(stderr, "  overlord chain run --chain %s/chain.yaml --input-file %s/sample_input.txt\n",
				res.Target, res.Target)
			fmt.Fprintln(stderr, "\nSwap mock/* in chain.yaml for a real provider (anthropic/claude-sonnet-4-5,")
			fmt.Fprintln(stderr, "openai/gpt-4o, google/gemini-2.5-pro, ollama/llama3, etc.) and drop the `fixture:`")
			fmt.Fprintln(stderr, "line to run against a live LLM.")
			return nil
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "scaffold into a non-empty directory")
	return cmd
}

// ---------- chain inspect ----------

func chainInspectCmd() *cobra.Command {
	var chainPath string
	cmd := &cobra.Command{
		Use:   "inspect",
		Short: "Show the compiled interpretation of a chain",
		Long: `Inspect loads a chain YAML, compiles it, and prints a human-readable
summary of what the runtime will do: the synthesized pipeline name,
each step's agent binding (provider + model), input/output schemas,
and the resolved system-prompt template.

Use this to understand how a chain lowers into the strict runtime
before running it.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ch, err := chain.Load(chainPath)
			if err != nil {
				return err
			}
			basePath := filepath.Dir(chainPath)
			compiled, err := chain.CompileWithBase(ch, basePath)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			renderInspect(out, ch, compiled)
			return nil
		},
	}
	cmd.Flags().StringVar(&chainPath, "chain", "", "path to chain YAML file (required)")
	_ = cmd.MarkFlagRequired("chain")
	return cmd
}

func renderInspect(w io.Writer, ch *chain.Chain, compiled *chain.Compiled) {
	fmt.Fprintf(w, "Chain: %s\n", ch.ID)
	fmt.Fprintf(w, "  input.type:  %s\n", ch.InputType())
	fmt.Fprintf(w, "  output.type: %s\n", ch.OutputType())
	fmt.Fprintf(w, "  output.from: steps.%s.output\n", ch.OutputFrom())
	fmt.Fprintf(w, "  vars:        %d\n", len(ch.Vars))
	fmt.Fprintln(w)

	fmt.Fprintf(w, "Compiles to pipeline %q with %d stages:\n\n", compiled.Config.Pipelines[0].Name, len(compiled.Config.Pipelines[0].Stages))
	stages := compiled.Config.Pipelines[0].Stages
	agents := map[string]string{}
	for _, a := range compiled.Config.Agents {
		agents[a.ID] = a.Provider + "/" + a.Model
		if a.Model == "" {
			agents[a.ID] = a.Provider
		}
	}
	for i, s := range stages {
		fmt.Fprintf(w, "  [%d] stage: %s\n", i+1, s.ID)
		fmt.Fprintf(w, "      agent:   %s (%s)\n", s.Agent, agents[s.Agent])
		fmt.Fprintf(w, "      input:   %s@%s\n", s.InputSchema.Name, s.InputSchema.Version)
		fmt.Fprintf(w, "      output:  %s@%s\n", s.OutputSchema.Name, s.OutputSchema.Version)
		next := "done"
		if !s.OnSuccess.IsConditional {
			next = s.OnSuccess.Static
		}
		fmt.Fprintf(w, "      next:    %s\n", next)
		fmt.Fprintf(w, "      prompt:  %s\n", indent(truncate(compiled.Templates[s.ID], 240), "        "))
		fmt.Fprintln(w)
	}

	fmt.Fprintln(w, "Synthesized schemas:")
	for _, e := range compiled.Config.SchemaRegistry {
		fmt.Fprintf(w, "  %s@%s\n", e.Name, e.Version)
	}
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + " …"
}

func indent(s, prefix string) string {
	lines := strings.Split(s, "\n")
	for i, ln := range lines {
		if i == 0 {
			continue
		}
		lines[i] = prefix + ln
	}
	return strings.Join(lines, "\n")
}

// ---------- chain export ----------

func chainExportCmd() *cobra.Command {
	var (
		chainPath string
		outDir    string
		stdout    bool
	)
	cmd := &cobra.Command{
		Use:   "export",
		Short: "Emit the equivalent full-pipeline YAML and schemas",
		Long: `Export is the graduation path from chain mode to full pipeline mode.
It compiles the chain, writes the equivalent overlord.yaml plus the
synthesized schema files into --out, and leaves the chain unchanged.

After export, you can hand-edit overlord.yaml to add fan-out stages,
conditional routing, retry budgets, or split the infra/pipeline
configs — everything the full pipeline mode supports — and run it with
` + "`" + `overlord run --config <dir>/overlord.yaml` + "`" + ` or
` + "`" + `overlord exec --config <dir>/overlord.yaml --id <chain-id> --payload '{…}'` + "`" + `.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ch, err := chain.Load(chainPath)
			if err != nil {
				return err
			}
			basePath := filepath.Dir(chainPath)
			compiled, err := chain.CompileWithBase(ch, basePath)
			if err != nil {
				return err
			}
			files, err := chain.Export(compiled)
			if err != nil {
				return err
			}
			if stdout {
				out := cmd.OutOrStdout()
				fmt.Fprintln(out, "# --- pipeline yaml ---")
				out.Write(files.Pipeline)
				if len(files.Schemas) > 0 {
					for name, data := range files.Schemas {
						fmt.Fprintf(out, "\n# --- schema: %s ---\n", name)
						out.Write(data)
					}
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
			fmt.Fprintf(stderr, "chain: exported %s to %s\n", ch.ID, outDir)
			fmt.Fprintf(stderr, "  overlord.yaml\n")
			schemaNames := make([]string, 0, len(files.Schemas))
			for name := range files.Schemas {
				schemaNames = append(schemaNames, name)
			}
			sortStrings(schemaNames)
			for _, name := range schemaNames {
				fmt.Fprintf(stderr, "  schemas/%s\n", name)
			}
			fixtureNames := make([]string, 0, len(files.Fixtures))
			for name := range files.Fixtures {
				fixtureNames = append(fixtureNames, name)
			}
			sortStrings(fixtureNames)
			for _, name := range fixtureNames {
				fmt.Fprintf(stderr, "  %s\n", name)
			}
			fmt.Fprintln(stderr, "\nNext steps:")
			fmt.Fprintf(stderr, "  overlord validate --config %s/overlord.yaml\n", outDir)
			fmt.Fprintf(stderr, "  overlord run      --config %s/overlord.yaml\n", outDir)
			return nil
		},
	}
	cmd.Flags().StringVar(&chainPath, "chain", "", "path to chain YAML file (required)")
	cmd.Flags().StringVar(&outDir, "out", "", "output directory")
	cmd.Flags().BoolVar(&stdout, "stdout", false, "write the pipeline yaml and schemas to stdout")
	_ = cmd.MarkFlagRequired("chain")
	return cmd
}

