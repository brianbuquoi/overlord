package workflow

import (
	"github.com/brianbuquoi/overlord/internal/chain"
)

// Export writes the full-pipeline equivalent of a compiled workflow.
// Thin wrapper over chain.Export — the workflow-to-chain translation
// already produced a canonical chain.Compiled, so the exported
// project is a normal pipeline-mode project the strict runtime
// accepts.
//
// Export is the canonical escape hatch from workflow mode. Authors
// hand-edit the exported overlord.yaml to add fan-out, conditional
// routing, retry budgets, split infra config, or anything else the
// strict surface supports; their original workflow.yaml remains the
// source for the simple version.
func Export(c *Compiled) (*chain.ExportFiles, error) {
	if c == nil {
		return nil, nil
	}
	return chain.Export(c.Compiled)
}
