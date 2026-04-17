package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/brianbuquoi/overlord/internal/agent/registry"
	"github.com/brianbuquoi/overlord/internal/broker"
	"github.com/brianbuquoi/overlord/internal/scaffold"
	"github.com/brianbuquoi/overlord/internal/workflow"
)

// Exit codes returned by `overlord init`. The matrix is intentionally
// distinct from `overlord exec` because init has its own failure modes
// (scaffold target invalid, write failure, demo failure). See the plan's
// exit-code-matrix decision.
const (
	initExitSuccess       = 0
	initExitGeneralError  = 1
	initExitInvalidTarget = 2
	initExitWriteFailure  = 3
	initExitDemoFailure   = 4
	initExitInterrupted   = 130
)

// demoTimeout is the hard cap on a scaffolded demo run. 30 seconds is
// generous for a single mock-backed pipeline task; anything longer
// suggests a broker wedge and is reported as demo failure so the user
// still sees the next-steps block. Declared as a var (not a const) so
// tests can override it to exercise the polling-timeout branch without
// waiting 30s; production paths never mutate it.
var demoTimeout = 30 * time.Second

// demoDrainTimeout bounds how long runDemo waits for the broker
// goroutine to exit after brokerCancel. Memory-store scaffolded demos
// drain in milliseconds; 5s is a generous safety margin.
const demoDrainTimeout = 5 * time.Second

// initExitError carries an exit code out of RunE without leaking cobra's
// usage banner. main() type-asserts and maps Code → os.Exit.
type initExitError struct {
	Code int
	Msg  string
	Err  error
}

// Error implements the error interface. If an underlying cause is set,
// the message includes it so terminals see the full failure chain.
func (e *initExitError) Error() string {
	if e == nil {
		return ""
	}
	if e.Err != nil && e.Msg != "" {
		return fmt.Sprintf("%s: %v", e.Msg, e.Err)
	}
	if e.Err != nil {
		return e.Err.Error()
	}
	return e.Msg
}

// Unwrap exposes the underlying cause for errors.Is / errors.As walks
// so telemetry and test assertions can see the original failure.
func (e *initExitError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// initCmd wires the scaffold writer and in-process demo runner into a
// cobra command. Usage: `overlord init <template> [dir] [flags]`.
func initCmd() *cobra.Command {
	var (
		force          bool
		overwrite      bool
		noRun          bool
		nonInteractive bool
	)

	cmd := &cobra.Command{
		Use:   "init [template] [dir]",
		Short: "Scaffold a new Overlord project and run a sample workflow",
		Long: `Scaffold a new project from an embedded template, then run a sample
input through the generated workflow using the first-party mock provider
(no credentials, no network). After the demo completes, a next-steps block
tells you how to swap in a real LLM.

Template arg is optional — the default template ("starter") scaffolds the
simple workflow: format. Pass an explicit template name for the strict
pipeline templates (hello, summarize). The optional positional dir
argument controls the target directory; defaults to "./<template>"
(or "." when no template is given).

Flags:
  --force             scaffold into a non-empty directory
  --overwrite         back up and replace colliding files (requires --force)
  --no-run            write files only; skip the demo
  --non-interactive   treat demo failure as a hard error (exit 4)

Exit codes:
  0    success (or demo failure in interactive mode — files still persist)
  1    generic error
  2    invalid target / unknown template / collision without --overwrite
  3    write failure (permission denied, disk full, etc.)
  4    demo failure under --non-interactive
  130  SIGINT / SIGTERM during the demo`,
		Example: `  overlord init hello
  overlord init summarize ./my-project --no-run
  overlord init hello ./my-demo --force --overwrite`,
		// Accept 1 or 2 positional args. We don't use cobra.ExactArgs
		// because we want the "missing template" case to print the
		// template list ourselves, not cobra's default error.
		Args: cobra.MaximumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runInit(cmd, args, initArgs{
				Force:          force,
				Overwrite:      overwrite,
				NoRun:          noRun,
				NonInteractive: nonInteractive,
			})
		},
	}

	cmd.Flags().BoolVar(&force, "force", false, "scaffold into a non-empty directory")
	cmd.Flags().BoolVar(&overwrite, "overwrite", false, "back up and replace colliding files (requires --force)")
	cmd.Flags().BoolVar(&noRun, "no-run", false, "skip the in-process demo after scaffolding")
	cmd.Flags().BoolVar(&nonInteractive, "non-interactive", false, "treat demo failure as a hard error (exit 4)")
	return cmd
}

// initArgs bundles the parsed flags. Extracted into a struct so runInit
// is easily unit-testable without standing up a full cobra command.
type initArgs struct {
	Force          bool
	Overwrite      bool
	NoRun          bool
	NonInteractive bool
}

// writeRollbackErrors prints any partial-rollback failures attached to
// a scaffold.WriteError. No-op when errs is empty.
func writeRollbackErrors(w io.Writer, errs []error) {
	if len(errs) == 0 {
		return
	}
	fmt.Fprintf(w, "rollback reported %d partial failure(s):\n", len(errs))
	for _, e := range errs {
		fmt.Fprintf(w, "  %v\n", e)
	}
}

// writeCleanupWarnings prints post-commit best-effort failures (tempdir
// removal, etc.) collected on a scaffold.Result. No-op when warnings is
// empty. The scaffold itself succeeded when this runs.
func writeCleanupWarnings(w io.Writer, warnings []string) {
	if len(warnings) == 0 {
		return
	}
	fmt.Fprintf(w, "post-commit cleanup warnings (%d):\n", len(warnings))
	for _, m := range warnings {
		fmt.Fprintf(w, "  %s\n", m)
	}
}

// runInit is the RunE body. Kept package-private so tests can drive it
// directly with a fake *cobra.Command.
func runInit(cmd *cobra.Command, args []string, a initArgs) error {
	stderr := cmd.ErrOrStderr()
	stdout := cmd.OutOrStdout()

	// 1. Template name defaults to the workflow starter. Explicit names
	//    still work for the strict-pipeline templates (hello, summarize).
	template := defaultInitTemplate
	if len(args) >= 1 && strings.TrimSpace(args[0]) != "" {
		template = args[0]
	}

	// 2. Unknown template — print the list + "unknown template: ..." + code 2.
	if !isKnownTemplate(template) {
		fmt.Fprintln(stderr, templateHelpMessage(template))
		return &initExitError{Code: initExitInvalidTarget, Msg: fmt.Sprintf("unknown template: %s", template)}
	}

	// 3. Compute target.
	target := "./" + template
	if len(args) >= 2 && strings.TrimSpace(args[1]) != "" {
		target = args[1]
	}

	// 4. Call scaffold.Write. Map WriteError.Code → initExitError.Code.
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	result, err := scaffold.Write(ctx, template, target, scaffold.Options{
		Force:     a.Force,
		Overwrite: a.Overwrite,
	})
	if err != nil {
		var werr *scaffold.WriteError
		if errors.As(err, &werr) {
			// Surface mid-merge rollback failures so the operator can see
			// which backup or copied file could not be unwound. Primary
			// error still classifies the exit code.
			writeRollbackErrors(stderr, werr.RollbackErrors)
			return &initExitError{Code: werr.Code, Msg: werr.Msg, Err: werr.Err}
		}
		return &initExitError{Code: initExitGeneralError, Msg: "scaffold write failed", Err: err}
	}

	// 5. Note backups (Force+Overwrite path).
	if len(result.Backups) > 0 {
		fmt.Fprintf(stderr, "Backed up %d colliding file(s):\n", len(result.Backups))
		for _, b := range result.Backups {
			fmt.Fprintf(stderr, "  %s -> %s\n", b.Original, b.Backup)
		}
	}

	// Post-commit best-effort failures (e.g. tempdir not fully removed)
	// — the scaffold succeeded but something cosmetic is left behind.
	writeCleanupWarnings(stderr, result.CleanupWarnings)

	fmt.Fprintf(stderr, "Scaffolded %s into %s\n", template, result.Target)

	// 6. --no-run: print next-steps and exit 0.
	if a.NoRun {
		nextSteps, err := formatNextSteps(result.Target)
		if err != nil {
			// We can't even parse the freshly-written config — that's a
			// scaffold-writer bug rather than a user error. Fall back to
			// a generic next-steps hint so the user isn't left empty-handed.
			fmt.Fprintf(stderr, "warning: could not read scaffolded config for next-steps: %v\n", err)
			fmt.Fprintln(stderr, fallbackNextSteps(result.Target))
			return nil
		}
		fmt.Fprintln(stderr, nextSteps)
		return nil
	}

	// 7. Install SIGINT/SIGTERM handler for the demo lifetime, run the demo.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	demoCtx, demoCancel := context.WithCancel(ctx)
	defer demoCancel()

	// Translate SIGINT → ctx cancellation in a helper goroutine so
	// runDemo can observe it via ctx.Err() without taking an extra
	// channel. signalReceived is set so the outer branch can distinguish
	// "signal arrived" from "demo failed on its own".
	var signalReceived atomic.Bool
	sigDone := make(chan struct{})
	go func() {
		defer close(sigDone)
		select {
		case <-sigCh:
			signalReceived.Store(true)
			demoCancel()
		case <-demoCtx.Done():
		}
	}()

	resultBytes, demoErr := runDemo(demoCtx, result.Target, stderr)

	// Drain the signal goroutine before reading signalReceived.
	demoCancel()
	<-sigDone

	// 8. SIGINT wins over demo-failure classification.
	if signalReceived.Load() {
		fmt.Fprintln(stderr, "Interrupted.")
		return &initExitError{Code: initExitInterrupted, Msg: "interrupted"}
	}

	nextSteps, nsErr := formatNextSteps(result.Target)
	if nsErr != nil {
		nextSteps = fallbackNextSteps(result.Target)
	}

	if demoErr != nil {
		// 9. Demo failure — always print the warning + next-steps. Code
		//    depends on --non-interactive: default is exit 0 (files
		//    persist, operator still gets guidance); --non-interactive
		//    means CI wants a hard fail.
		fmt.Fprintln(stderr, "")
		fmt.Fprintln(stderr, "!!! Demo run failed — scaffold files are still in place.")
		fmt.Fprintf(stderr, "    Error: %v\n", demoErr)
		fmt.Fprintln(stderr, "")
		fmt.Fprintln(stderr, nextSteps)
		if a.NonInteractive {
			return &initExitError{Code: initExitDemoFailure, Msg: "demo failed", Err: demoErr}
		}
		return nil
	}

	// 10. Demo success — pipeline result to stdout, next-steps to stderr.
	if len(resultBytes) > 0 {
		// Pretty-print the result so the user sees valid JSON.
		fmt.Fprintln(stdout, prettyJSON(resultBytes))
	}
	fmt.Fprintln(stderr, nextSteps)
	return nil
}

// isKnownTemplate reports whether name appears in the embedded catalog.
func isKnownTemplate(name string) bool {
	for _, n := range scaffold.ListTemplates() {
		if n == name {
			return true
		}
	}
	return false
}

// templateHelpMessage renders the usage hint + sorted list of templates.
// When unknown is non-empty, the trailing line surfaces the rejected name
// so CI logs can distinguish "no arg" from "typo in arg".
func templateHelpMessage(unknown string) string {
	var b strings.Builder
	b.WriteString("usage: overlord init <template> [dir] [flags]\n")
	b.WriteString("available templates:\n")
	for _, name := range scaffold.ListTemplates() {
		b.WriteString("  ")
		b.WriteString(name)
		b.WriteString(" — ")
		b.WriteString(templateDescription(name))
		b.WriteByte('\n')
	}
	if unknown != "" {
		b.WriteString("unknown template: ")
		b.WriteString(unknown)
	}
	return strings.TrimRight(b.String(), "\n")
}

// defaultInitTemplate is the template `overlord init` scaffolds when
// the caller supplies no template arg. It must stay in sync with the
// embedded templates/ tree and must always be a workflow-shaped
// project so the default first-run experience matches the simple
// product story: one YAML file, no strict-config concepts.
const defaultInitTemplate = "starter"

// templateDescription returns a one-line description for each embedded
// template. Hardcoded so `overlord init` can render help without parsing
// the scaffolded YAML at runtime.
func templateDescription(name string) string {
	switch name {
	case "starter":
		return "simple workflow project (default — run with `overlord run`)"
	case "hello":
		return "strict-mode single-stage pipeline (advanced)"
	case "summarize":
		return "strict-mode 2-stage linear pipeline (advanced)"
	default:
		return "(scaffolded project)"
	}
}

// runDemo runs one payload through the scaffolded project with a
// fully-specified broker lifecycle (goroutine + cancelable context +
// wg.Wait + Stopper.Stop), mirroring cmd/overlord/exec.go. Returns the
// final stage payload on success, or a non-nil error describing the
// failure mode (timeout, FAILED state, build error, etc.).
//
// Workflow-shaped scaffolds (the default) go through the workflow
// runner so the beginner path never requires a hand-built
// sample_payload.json. Strict-pipeline scaffolds keep the original
// sample_payload.json + broker.Submit flow.
//
// The broker is always drained before returning, even on error paths.
func runDemo(ctx context.Context, target string, stderr io.Writer) (payload json.RawMessage, runErr error) {
	configPath := filepath.Join(target, "overlord.yaml")

	if workflow.IsWorkflowFile(configPath) {
		return runDemoWorkflow(ctx, target, stderr)
	}

	cfg, err := loadConfig(configPath)
	if err != nil {
		return nil, fmt.Errorf("load scaffolded config: %w", err)
	}
	if len(cfg.Pipelines) == 0 {
		return nil, fmt.Errorf("scaffolded config has no pipelines")
	}
	pipelineID := cfg.Pipelines[0].Name

	// Broker workers run on their own goroutines and share the demo's
	// stderr writer with this goroutine. Wrap in a mutex so concurrent
	// writers (broker log lines + runDemo's own progress fprintfs) are
	// serialized — os.Stderr is safe by kernel contract but tests wire
	// a plain bytes.Buffer that is not concurrency-safe.
	stderr = &syncWriter{w: stderr}

	// Route broker / agent logs to stderr so stdout stays clean for the
	// pipeline result payload. The deterministic-output success criterion
	// in the plan applies to stdout specifically.
	logger := newDemoLogger(stderr)

	// buildBroker wires contract registry + memory store + agents (with
	// mock fixture validation) + the broker itself. Any of those can
	// return a typed error; they surface here as a single build failure.
	b, err := buildBroker(cfg, nil, configPath, logger, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("build broker: %w", err)
	}

	brokerCtx, brokerCancel := context.WithCancel(ctx)
	// Capacity-1 channel so the goroutine never blocks publishing its
	// error even if the drain path never reads it.
	brokerErrCh := make(chan error, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		brokerErrCh <- b.Run(brokerCtx)
	}()

	// Always drain the broker goroutine and stop any agents that manage
	// external processes, even on early-return paths. If the broker
	// itself returned a non-context-cancel error, fold it into runErr so
	// broker-start failures surface immediately instead of being masked
	// by the 30s polling timeout.
	defer func() {
		brokerCancel()
		done := make(chan struct{})
		go func() {
			wg.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(demoDrainTimeout):
			fmt.Fprintln(stderr, "warning: broker drain exceeded 5s; proceeding with Stop()")
		}
		// Drain the error channel non-blockingly. It's buffered, so if
		// the goroutine finished it has already written. If the drain
		// timed out the channel is still empty; that's fine.
		select {
		case err := <-brokerErrCh:
			// context.Canceled is the expected exit when we cancel
			// brokerCtx during normal cleanup; anything else is a
			// real broker-side failure. Only surface it if runDemo
			// didn't already fail with a more specific error.
			if err != nil && !errors.Is(err, context.Canceled) && runErr == nil {
				runErr = fmt.Errorf("broker run: %w", err)
			}
		default:
		}
		for _, s := range registry.Stoppers(b.Agents()) {
			if err := s.Stop(); err != nil {
				fmt.Fprintf(stderr, "agent stop error: %v\n", err)
			}
		}
	}()

	// Read sample_payload.json (written by Unit 2 templates).
	payloadPath := filepath.Join(target, "sample_payload.json")
	payloadBytes, err := os.ReadFile(payloadPath)
	if err != nil {
		return nil, fmt.Errorf("read sample payload: %w", err)
	}
	if !json.Valid(payloadBytes) {
		return nil, fmt.Errorf("sample payload at %s is not valid JSON", payloadPath)
	}

	// Submit exactly one task to the first pipeline. Inherit from the
	// demo context so a SIGINT during submit propagates into cancellation
	// instead of waiting out the 5-second timeout.
	submitCtx, submitCancel := context.WithTimeout(ctx, 5*time.Second)
	defer submitCancel()
	task, err := b.Submit(submitCtx, pipelineID, json.RawMessage(payloadBytes))
	if err != nil {
		return nil, fmt.Errorf("submit task: %w", err)
	}
	fmt.Fprintf(stderr, "Submitted task %s to pipeline %s\n", task.ID, pipelineID)

	// Poll until terminal state, 30s timeout, or ctx cancellation.
	pollCtx, pollCancel := context.WithTimeout(ctx, demoTimeout)
	defer pollCancel()

	final, outcome := waitForDemoTerminal(pollCtx, b, task.ID)
	switch outcome {
	case demoOutcomeTimeout:
		return nil, fmt.Errorf("demo timed out after %s (task %s still %s)", demoTimeout, task.ID, orDash(lastKnownState(final)))
	case demoOutcomeCanceled:
		// Context canceled — either the parent ctx or a signal. Return
		// ctx.Err() so the caller can distinguish from a real failure.
		return nil, context.Canceled
	}

	if final == nil {
		return nil, fmt.Errorf("task %s disappeared before reaching terminal state", task.ID)
	}

	switch final.State {
	case broker.TaskStateDone, broker.TaskStateReplayed:
		return final.Payload, nil
	case broker.TaskStateFailed:
		reason := "unknown"
		if r, ok := final.Metadata["failure_reason"]; ok {
			reason = fmt.Sprintf("%v", r)
		}
		return nil, fmt.Errorf("task %s failed: %s", task.ID, reason)
	case broker.TaskStateDiscarded:
		return nil, fmt.Errorf("task %s was discarded", task.ID)
	default:
		return nil, fmt.Errorf("task %s ended in unexpected state %s", task.ID, final.State)
	}
}

// runDemoWorkflow is the workflow-shape counterpart of runDemo. It
// loads sample_input.txt, runs the workflow to a terminal state via
// workflow.Run, and returns the final payload so the init command
// prints it on stdout. The rest of the init flow (next-steps,
// SIGINT handling, exit codes) does not care which variant ran.
func runDemoWorkflow(ctx context.Context, target string, stderr io.Writer) (json.RawMessage, error) {
	configPath := filepath.Join(target, "overlord.yaml")
	file, err := workflow.Load(configPath)
	if err != nil {
		return nil, fmt.Errorf("load scaffolded workflow: %w", err)
	}
	inputPath := filepath.Join(target, "sample_input.txt")
	sample, err := os.ReadFile(inputPath)
	if err != nil {
		// sample_input.txt is optional — workflows with no external
		// input still run. Tolerate missing-file errors here and feed
		// an empty string.
		sample = nil
	}
	basePath, absErr := filepath.Abs(target)
	if absErr != nil {
		basePath = target
	}
	logger := newDemoLogger(stderr)
	fmt.Fprintf(stderr, "Running workflow %s (mock provider, no credentials)…\n", file.Workflow.ID)
	result, runErr := workflow.Run(ctx, file, basePath, workflow.RunOptions{
		Input:   string(sample),
		Timeout: demoTimeout,
		Logger:  logger,
	})
	if runErr != nil {
		return nil, runErr
	}
	if result == nil || result.Task == nil {
		return nil, fmt.Errorf("workflow produced no result")
	}
	if result.Task.State != broker.TaskStateDone && result.Task.State != broker.TaskStateReplayed {
		return nil, fmt.Errorf("workflow ended in state %s", result.Task.State)
	}
	return result.Task.Payload, nil
}

// demoOutcome enumerates the reasons waitForDemoTerminal returns early.
type demoOutcome int

const (
	demoOutcomeTerminal demoOutcome = iota
	demoOutcomeTimeout
	demoOutcomeCanceled
)

// waitForDemoTerminal polls the broker store until the task reaches a
// terminal state, the context expires, or the parent context is
// canceled. The returned *broker.Task is the last successful GetTask
// observation (may be nil if the task was never readable).
func waitForDemoTerminal(ctx context.Context, b *broker.Broker, taskID string) (*broker.Task, demoOutcome) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	var last *broker.Task
	for {
		select {
		case <-ctx.Done():
			// Distinguish ctx deadline (timeout) from parent cancel.
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return last, demoOutcomeTimeout
			}
			return last, demoOutcomeCanceled
		case <-ticker.C:
			task, err := b.GetTask(context.Background(), taskID)
			if err != nil {
				continue
			}
			last = task
			if task.State.IsTerminal() {
				return task, demoOutcomeTerminal
			}
		}
	}
}

// lastKnownState safely extracts the State field for diagnostic messages.
func lastKnownState(t *broker.Task) broker.TaskState {
	if t == nil {
		return ""
	}
	return t.State
}

// formatNextSteps reads the scaffolded config and renders the stable
// ASCII next-steps block. Plain text only — no ANSI colors. Workflow
// scaffolds get the beginner-friendly `overlord run` block; strict
// pipelines keep the `overlord exec --payload` block.
func formatNextSteps(absTarget string) (string, error) {
	configPath := filepath.Join(absTarget, "overlord.yaml")
	if workflow.IsWorkflowFile(configPath) {
		return formatNextStepsWorkflow(absTarget), nil
	}
	cfg, err := loadConfig(configPath)
	if err != nil {
		return "", fmt.Errorf("load scaffolded config: %w", err)
	}
	if len(cfg.Pipelines) == 0 {
		return "", fmt.Errorf("scaffolded config has no pipelines")
	}
	pipelineID := cfg.Pipelines[0].Name

	// Example payload — pulled verbatim from the scaffolded
	// sample_payload.json so copy-paste works without prior knowledge.
	payloadPath := filepath.Join(absTarget, "sample_payload.json")
	sample, err := os.ReadFile(payloadPath)
	if err != nil {
		return "", fmt.Errorf("read sample payload: %w", err)
	}
	// Compact the sample so it fits on a single CLI line.
	var compact []byte
	if json.Valid(sample) {
		var anyVal interface{}
		if err := json.Unmarshal(sample, &anyVal); err == nil {
			compact, _ = json.Marshal(anyVal)
		}
	}
	if len(compact) == 0 {
		compact = []byte(`{}`)
	}

	quotedTarget := shellQuote(absTarget)

	var b strings.Builder
	b.WriteString("Next steps:\n")
	fmt.Fprintf(&b, "  cd %s && overlord exec \\\n", quotedTarget)
	b.WriteString("    --config overlord.yaml \\\n")
	fmt.Fprintf(&b, "    --id %s \\\n", pipelineID)
	fmt.Fprintf(&b, "    --payload '%s'\n", string(compact))
	b.WriteString("  # To use a real LLM, uncomment the real provider block in overlord.yaml\n")
	b.WriteString("  # and change the stage's agent reference from <id>-mock to <id>.")
	return b.String(), nil
}

// formatNextStepsWorkflow renders the beginner-friendly next-steps
// block. `overlord run --input-file sample_input.txt` is the core
// verb; the `serve` hint is mentioned so operators who want a
// long-running service know where to look.
func formatNextStepsWorkflow(absTarget string) string {
	quoted := shellQuote(absTarget)
	var b strings.Builder
	b.WriteString("Next steps:\n")
	fmt.Fprintf(&b, "  cd %s && overlord run --input-file sample_input.txt\n", quoted)
	b.WriteString("  # Serve the workflow as a local service:\n")
	b.WriteString("  #   overlord serve\n")
	b.WriteString("  # Swap mock/* for a real provider (e.g. anthropic/claude-sonnet-4-5,\n")
	b.WriteString("  # openai/gpt-4o, google/gemini-2.5-pro) in overlord.yaml and remove\n")
	b.WriteString("  # the matching fixture: line to run against a live LLM.\n")
	b.WriteString("  # Graduate to the advanced strict format when you need fan-out or\n")
	b.WriteString("  # conditional routing:\n")
	b.WriteString("  #   overlord export --advanced --out ./advanced")
	return b.String()
}

// fallbackNextSteps is used when the scaffolded config can't be parsed
// (shouldn't happen on the happy path, but we must still emit a useful
// message so the user isn't stranded).
func fallbackNextSteps(absTarget string) string {
	var b strings.Builder
	b.WriteString("Next steps:\n")
	fmt.Fprintf(&b, "  cd %s && overlord exec \\\n", shellQuote(absTarget))
	b.WriteString("    --config overlord.yaml \\\n")
	b.WriteString("    --id <pipeline-id> \\\n")
	b.WriteString("    --payload @sample_payload.json\n")
	b.WriteString("  # To use a real LLM, uncomment the real provider block in overlord.yaml\n")
	b.WriteString("  # and change the stage's agent reference from <id>-mock to <id>.")
	return b.String()
}

// shellQuote wraps s in POSIX single-quotes so it can be copy-pasted
// into bash/zsh verbatim, even if the path contains spaces or shell
// metacharacters. Single-quotes inside s are handled by closing, escaping,
// and reopening: abc'def → 'abc'\''def'. Paths without any shell-special
// characters are returned unquoted for readability.
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	safe := true
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= 'a' && c <= 'z') ||
			(c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') ||
			c == '/' || c == '.' || c == '_' || c == '-') {
			safe = false
			break
		}
	}
	if safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// syncWriter serializes concurrent writes to a delegate writer behind
// a mutex. Used by runDemo to keep broker log lines from interleaving
// with the demo's own progress output, and to satisfy the race detector
// when the underlying writer is not concurrency-safe (e.g. bytes.Buffer
// in tests).
type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

// newDemoLogger returns a JSON slog.Logger that writes to the given
// writer. Used by runDemo so broker/agent log lines go to stderr and
// don't contaminate the stdout-channel used for the pipeline result.
// Log level honors LOG_LEVEL, same as the shared newLogger helper.
func newDemoLogger(w io.Writer) *slog.Logger {
	level := slog.LevelInfo
	switch strings.ToLower(os.Getenv("LOG_LEVEL")) {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level}))
}

// prettyJSON re-indents a JSON byte slice for terminal display.
// Falls back to the raw input on parse error so the user still sees
// SOMETHING even if the payload is malformed.
func prettyJSON(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var v interface{}
	if err := json.Unmarshal(raw, &v); err != nil {
		return strings.TrimRight(string(raw), "\n")
	}
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return strings.TrimRight(string(raw), "\n")
	}
	return string(out)
}
