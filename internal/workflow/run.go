package workflow

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/brianbuquoi/overlord/internal/chain"
)

// RunOptions configures a one-shot local workflow run.
type RunOptions struct {
	// Input is the initial payload the workflow sees. For text
	// workflows this is the raw text; for json workflows this is a
	// JSON object string.
	Input string

	// Timeout bounds the wait for the workflow to reach a terminal
	// state. Zero means "wait forever" — callers should always set a
	// positive value for interactive use.
	Timeout time.Duration

	// Logger receives broker/agent log lines. When nil, slog.Default()
	// is used.
	Logger *slog.Logger
}

// Run compiles file and drives it through the shared broker with an
// in-memory store. Run is the beginner-friendly entry point behind
// `overlord run` for workflow-shaped configs — one workflow, one
// input, one terminal result.
//
// Workflows declaring runtime.store.type != memory are still runnable
// here; Run forces a memory store for the single-shot path so the
// CLI never requires postgres/redis credentials just to try a
// workflow locally. Use `overlord serve` when the authored store
// matters.
func Run(ctx context.Context, file *File, basePath string, opts RunOptions) (*chain.RunResult, error) {
	if file == nil || file.Workflow == nil {
		return nil, fmt.Errorf("run: empty workflow")
	}
	ch, err := toChain(file.Workflow)
	if err != nil {
		return nil, err
	}
	return chain.Run(ctx, ch, basePath, chain.RunOptions{
		Input:   opts.Input,
		Timeout: opts.Timeout,
		Logger:  opts.Logger,
	})
}
