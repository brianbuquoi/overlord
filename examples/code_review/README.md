# Code Review Pipeline Example

Automated code review of a Git diff using a 3-stage Anthropic pipeline:

1. **parse** (claude-haiku-4-5) — Extracts changed files, writes a summary, flags concerns
2. **review** (claude-sonnet-4-5) — Performs detailed code review with severity-rated findings
3. **summarize** (claude-haiku-4-5) — Produces an executive summary for PR comments

## Prerequisites

```bash
export ANTHROPIC_API_KEY=sk-ant-...
```

## Run

```bash
# From the repo root:
make example-code-review

# Or manually:
go run ./cmd/orcastrator submit \
  --config config/examples/code_review.yaml \
  --pipeline code-review \
  --payload @examples/code_review/sample_input.json \
  --wait \
  --timeout 3m
```

## Sample Input

`sample_input.json` contains a realistic diff with two deliberate security issues:

- **SQL injection**: User input is interpolated directly into a SQL query via `fmt.Sprintf`
- **Hardcoded credential**: An admin token is stored as a package-level constant

The review stage should flag both as critical/high severity and set `overall_assessment` to `request_changes`.

## Expected Output

The final task payload (from the summarize stage) will contain:

```json
{
  "executive_summary": "...",
  "critical_count": 1,
  "high_count": 1,
  "action_required": true,
  "top_recommendations": ["..."]
}
```

## Live Integration Test

```bash
# Requires ANTHROPIC_API_KEY to be set
go test -race -tags live ./examples/code_review/... -v -timeout 5m
```
