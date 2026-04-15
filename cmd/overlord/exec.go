package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/brianbuquoi/overlord/internal/broker"
	"github.com/brianbuquoi/overlord/internal/config"
	"github.com/brianbuquoi/overlord/internal/metrics"
)

// Exit codes returned by `overlord exec`.
const (
	execExitDone    = 0
	execExitFailed  = 1
	execExitTimeout = 2
	execExitConfig  = 3
)

// execCmd runs a single task through a pipeline to completion and exits.
// It does not start the HTTP API server and does not bind any port.
func execCmd() *cobra.Command {
	var (
		configPath   string
		pipelineFile string
		pipelineID   string
		payload      string
		wait         bool
		timeout      time.Duration
		output       string
		quiet        bool
	)

	cmd := &cobra.Command{
		Use:   "exec",
		Short: "Run a single pipeline task to completion and exit",
		Long: `Run a single task through a pipeline to completion and exit.

Unlike 'overlord run', exec does not start the HTTP API server and does
not bind a port. It is designed for scripting, CI pipelines, and one-off
interactive runs.

Progress is printed to stderr as the task transitions between stages;
the final result is written to stdout.

Exit codes:
  0  Task completed successfully (state: DONE)
  1  Task failed or was dead-lettered
  2  Timed out waiting for task completion
  3  Configuration or startup error`,
		Example: `  overlord exec --config ./infra.yaml --pipeline ./pipeline.yaml \
    --id code-review --payload '{"language":"go","code":"..."}'

  overlord exec --config ./infra.yaml --id my-pipeline \
    --payload @./input.json --output json --quiet | jq .`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runExec(cmd, execArgs{
				configPath:   configPath,
				pipelineFile: pipelineFile,
				pipelineID:   pipelineID,
				payload:      payload,
				wait:         wait,
				timeout:      timeout,
				output:       output,
				quiet:        quiet,
			})
		},
	}

	cmd.Flags().StringVar(&configPath, "config", "", "path to infra YAML config file (required)")
	cmd.Flags().StringVar(&pipelineFile, "pipeline", "", "path to pipeline definition file (optional)")
	cmd.Flags().StringVar(&pipelineID, "id", "", "pipeline ID to run (required)")
	cmd.Flags().StringVar(&payload, "payload", "", "JSON payload string, or @filepath to read from a file (required)")
	cmd.Flags().BoolVar(&wait, "wait", true, "wait for task completion before exiting")
	cmd.Flags().DurationVar(&timeout, "timeout", 5*time.Minute, "maximum time to wait for task completion")
	cmd.Flags().StringVar(&output, "output", "pretty", "output format: pretty | json")
	cmd.Flags().BoolVar(&quiet, "quiet", false, "suppress progress output, print only the final result")
	cmd.MarkFlagRequired("config")
	cmd.MarkFlagRequired("id")
	cmd.MarkFlagRequired("payload")
	return cmd
}

type execArgs struct {
	configPath   string
	pipelineFile string
	pipelineID   string
	payload      string
	wait         bool
	timeout      time.Duration
	output       string
	quiet        bool
}

// execExitError signals a specific exit code to main without leaking cobra's
// usage print. It is converted to os.Exit in runExec's caller.
type execExitError struct {
	code int
	msg  string
}

func (e *execExitError) Error() string { return e.msg }

func runExec(cmd *cobra.Command, a execArgs) error {
	stderr := cmd.ErrOrStderr()
	stdout := cmd.OutOrStdout()

	if a.output != "pretty" && a.output != "json" {
		return &execExitError{code: execExitConfig, msg: fmt.Sprintf("invalid --output %q: must be \"pretty\" or \"json\"", a.output)}
	}

	cfg, err := loadConfig(a.configPath)
	if err != nil {
		return &execExitError{code: execExitConfig, msg: fmtConfigError(a.configPath, err).Error()}
	}

	if a.pipelineFile != "" {
		pf, pfPath, err := config.LoadPipelineFile(a.pipelineFile)
		if err != nil {
			return &execExitError{code: execExitConfig, msg: err.Error()}
		}
		if err := pf.MergeInto(cfg, pfPath); err != nil {
			return &execExitError{code: execExitConfig, msg: err.Error()}
		}
	}

	pipelineExists := false
	for _, p := range cfg.Pipelines {
		if p.Name == a.pipelineID {
			pipelineExists = true
			break
		}
	}
	if !pipelineExists {
		available := make([]string, 0, len(cfg.Pipelines))
		for _, p := range cfg.Pipelines {
			available = append(available, p.Name)
		}
		return &execExitError{code: execExitConfig, msg: fmt.Sprintf("pipeline %q not found (available: %s)", a.pipelineID, strings.Join(available, ", "))}
	}

	payloadBytes, err := parsePayload(a.payload)
	if err != nil {
		return &execExitError{code: execExitConfig, msg: err.Error()}
	}

	logger := newLogger()
	m := metrics.New()

	b, err := buildBroker(cfg, nil, a.configPath, logger, m, nil)
	if err != nil {
		return &execExitError{code: execExitConfig, msg: err.Error()}
	}

	brokerCtx, brokerCancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		b.Run(brokerCtx)
	}()
	defer func() {
		brokerCancel()
		drainDone := make(chan struct{})
		go func() {
			wg.Wait()
			close(drainDone)
		}()
		select {
		case <-drainDone:
		case <-time.After(10 * time.Second):
		}
	}()

	submitCtx := context.Background()
	task, err := b.Submit(submitCtx, a.pipelineID, payloadBytes)
	if err != nil {
		return &execExitError{code: execExitConfig, msg: fmt.Sprintf("submit to pipeline %q failed: %v", a.pipelineID, err)}
	}

	if !a.wait {
		fmt.Fprintln(stdout, task.ID)
		return nil
	}

	if !a.quiet && a.output != "json" {
		fmt.Fprintf(stderr, "→ Submitted task %s to pipeline %s\n", task.ID, a.pipelineID)
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	pollCtx, pollCancel := context.WithTimeout(context.Background(), a.timeout)
	defer pollCancel()

	start := time.Now()
	final, outcome := waitForTerminal(pollCtx, sigCh, b, task.ID, start, stderr, a.quiet, a.output)

	switch outcome {
	case execExitTimeout:
		fmt.Fprintf(stderr, "⏱ Task %s did not complete within %s\n", task.ID, a.timeout)
		fmt.Fprintf(stderr, "  Task ID: %s — check with: overlord status --config %s --task %s\n", task.ID, a.configPath, task.ID)
		return &execExitError{code: execExitTimeout, msg: "timeout"}
	case -1: // SIGINT
		fmt.Fprintf(stderr, "\n✗ Interrupted. Task %s is still in-flight.\n", task.ID)
		fmt.Fprintf(stderr, "  Check state:  overlord status --config %s --task %s\n", a.configPath, task.ID)
		fmt.Fprintf(stderr, "  Replay later: overlord dead-letter replay --config %s --task %s\n", a.configPath, task.ID)
		return &execExitError{code: execExitTimeout, msg: "interrupted"}
	}

	if final == nil {
		return &execExitError{code: execExitFailed, msg: "task not found after completion"}
	}

	writeExecOutput(stdout, stderr, final, a.output, a.quiet, start)

	switch final.State {
	case broker.TaskStateDone, broker.TaskStateReplayed:
		return nil
	}

	if final.RoutedToDeadLetter {
		fmt.Fprintf(stderr, "  Task ID: %s — replay with: overlord dead-letter replay --config %s --task %s\n", final.ID, a.configPath, final.ID)
	}
	return &execExitError{code: execExitFailed, msg: "task did not complete successfully"}
}

// waitForTerminal polls the store until the task reaches a terminal state,
// the context expires, or SIGINT is received. Returns (final task, outcome).
// outcome is 0 for terminal state reached, execExitTimeout for timeout, -1
// for SIGINT.
func waitForTerminal(ctx context.Context, sigCh <-chan os.Signal, b *broker.Broker, taskID string, start time.Time, stderr io.Writer, quiet bool, output string) (*broker.Task, int) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	var lastState broker.TaskState
	var lastStage string
	var lastAttempts int
	showProgress := !quiet && output != "json"

	for {
		select {
		case <-ctx.Done():
			return nil, execExitTimeout
		case <-sigCh:
			return nil, -1
		case <-ticker.C:
			task, err := b.GetTask(context.Background(), taskID)
			if err != nil {
				continue
			}

			if showProgress && (task.State != lastState || task.StageID != lastStage || task.Attempts != lastAttempts) {
				fmt.Fprintf(stderr, "  [%s] %s → %s (stage: %s, attempt: %d/%d)\n",
					elapsed(start), orDash(lastState), task.State, task.StageID, task.Attempts, task.MaxAttempts)
				lastState = task.State
				lastStage = task.StageID
				lastAttempts = task.Attempts
			}

			if task.State.IsTerminal() {
				return task, 0
			}
		}
	}
}

func writeExecOutput(stdout, stderr io.Writer, task *broker.Task, format string, quiet bool, start time.Time) {
	switch format {
	case "json":
		stdout.Write(append(indentJSON(task.Payload), '\n'))
	default:
		if !quiet {
			switch task.State {
			case broker.TaskStateDone:
				fmt.Fprintf(stderr, "✓ Completed in %s\n", elapsed(start))
			case broker.TaskStateReplayed:
				fmt.Fprintf(stderr, "↻ Task %s was replayed (a new task carries the retry)\n", task.ID)
			case broker.TaskStateDiscarded:
				fmt.Fprintf(stderr, "✗ Task %s was discarded\n", task.ID)
			default:
				fmt.Fprintf(stderr, "✗ Task %s (state: %s)\n", task.ID, task.State)
				if reason, ok := task.Metadata["failure_reason"]; ok {
					fmt.Fprintf(stderr, "  Reason: %v\n", reason)
				}
			}
			fmt.Fprintf(stdout, "Result (stage: %s):\n", task.StageID)
		}
		stdout.Write(append(indentJSON(task.Payload), '\n'))
	}
}

func indentJSON(raw json.RawMessage) []byte {
	if len(raw) == 0 {
		return []byte("null")
	}
	var buf []byte
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return []byte(raw)
	}
	buf, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return []byte(raw)
	}
	return buf
}

func elapsed(start time.Time) string {
	d := time.Since(start).Round(time.Second)
	m := int(d.Minutes())
	s := int(d.Seconds()) % 60
	return fmt.Sprintf("%02d:%02d", m, s)
}

func orDash(s broker.TaskState) string {
	if s == "" {
		return "—"
	}
	return string(s)
}

func parsePayload(payload string) (json.RawMessage, error) {
	var raw json.RawMessage
	if strings.HasPrefix(payload, "@") {
		data, err := readPayloadFile(strings.TrimPrefix(payload, "@"))
		if err != nil {
			return nil, err
		}
		raw = json.RawMessage(data)
	} else {
		raw = json.RawMessage(payload)
	}
	if !json.Valid(raw) {
		return nil, fmt.Errorf("payload is not valid JSON (use --payload @file.json to read from a file)")
	}
	return raw, nil
}

// readPayloadFile reads a payload from a file path with the same hardening
// applied to config and pipeline file loading: rejects symlinks and
// non-regular files. Errors are human-readable rather than wrapped Go
// stat errors.
func readPayloadFile(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("payload file not found: %s", path)
		}
		return nil, fmt.Errorf("cannot access payload file %s: %v", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("payload file must not be a symlink: %s", path)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("payload file must be a regular file: %s", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("cannot read payload file %s: %v", path, err)
	}
	return data, nil
}
