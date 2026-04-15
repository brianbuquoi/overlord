# `overlord exec`

`overlord exec` runs a single task through a pipeline to completion and
exits. It does **not** start an HTTP server, does **not** bind a port, and
does **not** serve the web dashboard — it is the one-shot counterpart to
`overlord run`.

Use `exec` for:

- **Scripting and CI** — submit a task, wait, pipe the result to another
  tool, exit with a meaningful code.
- **Iterative development** — edit your pipeline YAML, rerun, repeat.
- **One-off runs** — process a single input without managing a long-lived
  server.

Use `overlord run` when you need the HTTP API, WebSocket event stream, web
dashboard, or long-running worker pools.

## Flags

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--config` | string | — (required) | Path to the infra config file. |
| `--pipeline` | string | — | Optional path to a standalone pipeline definition file, merged with `--config` at runtime. |
| `--id` | string | — (required) | Pipeline ID (the `name:` field) to run. |
| `--payload` | string | — (required) | JSON payload, or `@filepath` to read from a file. |
| `--wait` | bool | `true` | Wait for task completion before exiting. When `false`, prints the task ID and exits 0. |
| `--timeout` | duration | `5m` | Maximum time to wait for task completion. |
| `--output` | string | `pretty` | Output format: `pretty` (headers + indented JSON) or `json` (raw JSON only). |
| `--quiet` | bool | `false` | Suppress progress output on stderr; print only the final result. |

## Exit codes

| Code | Meaning |
|------|---------|
| 0 | Task completed successfully (state: `DONE`). |
| 1 | Task failed or was dead-lettered. |
| 2 | Timed out waiting for task completion, or interrupted by SIGINT. |
| 3 | Configuration or startup error (invalid YAML, missing agent, etc). |

## Payload input

`--payload` accepts either inline JSON or a file reference prefixed with
`@`:

```bash
# Inline:
overlord exec --config infra.yaml --id my-pipe \
  --payload '{"request":"hello"}'

# From file:
overlord exec --config infra.yaml --id my-pipe \
  --payload @./input.json
```

## Output formats

`--output pretty` (default) writes a human-readable header to stderr and
indented JSON to stdout:

```
→ Submitted task c4f… to pipeline code-review-loop
  [00:02] PENDING → EXECUTING (stage: review, attempt: 0/3)
  [00:18] EXECUTING → DONE (stage: review, attempt: 1/3)
✓ Completed in 18s
Result (stage: review):
{
  "approved": true,
  "issues": [],
  "summary": "Looks good."
}
```

`--output json --quiet` writes **only** the raw JSON payload to stdout, so
it can be piped directly into other tools:

```bash
overlord exec --config infra.yaml --pipeline pipe.yaml \
  --id review --payload @input.json \
  --output json --quiet \
  | jq .summary
```

## Infra / pipeline config split

A typical project keeps one infra file and multiple pipeline files:

```
repo/
├── infra.yaml                    # agents, stores, auth
├── pipelines/
│   ├── code_review.yaml          # review workflow
│   └── triage.yaml               # triage workflow
└── schemas/
    └── *.json
```

`infra.yaml`:

```yaml
version: "1"
agents:
  - id: claude-reviewer
    provider: anthropic
    model: claude-sonnet-4-20250514
    auth: { api_key_env: ANTHROPIC_API_KEY }
    timeout: 60s
stores:
  memory: { max_tasks: 10000 }
```

`pipelines/code_review.yaml`:

> Schema paths in a pipeline file are resolved **relative to the pipeline
> file's own directory**, not the infra config's directory. In the layout
> above, `code_review.yaml` lives in `pipelines/`, so the schema path
> `../schemas/code_submission_v1.json` walks up one level and then into
> `schemas/`. Use `filepath.Abs`-friendly relative paths or absolute
> paths — both work; absolute paths pass through unchanged.

```yaml
version: "1"
schema_registry:
  - name: code_submission
    version: "v1"
    path: ../schemas/code_submission_v1.json
pipelines:
  - name: code-review-loop
    concurrency: 1
    store: memory
    stages:
      - id: review
        agent: claude-reviewer           # must exist in infra.yaml
        input_schema:  { name: code_submission, version: "v1" }
        output_schema: { name: code_submission, version: "v1" }
        timeout: 60s
        retry: { max_attempts: 3, backoff: exponential, base_delay: 1s }
        on_success: done
        on_failure: dead-letter
```

Run it:

```bash
overlord exec \
  --config   ./infra.yaml \
  --pipeline ./pipelines/code_review.yaml \
  --id       code-review-loop \
  --payload  @./input.json
```

Merge rules:

- Schema conflicts (same `name@version` declared in both files) are
  rejected at load time.
- Pipeline name collisions are rejected at load time.
- Every `agent:` reference in the pipeline file must resolve to an agent
  defined in the infra config — otherwise the merge fails with the stage
  ID and missing agent ID reported in the error.

The same pattern works for `overlord submit`, using the same flag names:

```bash
overlord submit --config ./infra.yaml \
  --pipeline ./pipelines/code_review.yaml \
  --id code-review-loop \
  --payload @./input.json --wait
```

## Graceful shutdown

`exec` signals its broker to stop on return and waits up to 10 seconds for
in-flight work to drain. If you send SIGINT (Ctrl+C) while waiting, it
prints the task ID and a replay hint to stderr, then exits with code 2 —
the task may still be in-flight and can be inspected with
`overlord status` or replayed later.

## Task lifecycle on exec exit

When `overlord exec` exits due to `--timeout` expiry or SIGINT (Ctrl+C),
the underlying task continues running in the store and broker. It is
**not** cancelled. Specifically:

- The task will complete (or fail) independently of the exec process.
- The task ID is printed to stderr on exit. Look it up later with:

  ```bash
  overlord status --config ./infra.yaml --task <task-id>
  ```

- If the task fails, replay it from the dead-letter queue:

  ```bash
  overlord dead-letter replay --config ./infra.yaml --task <task-id>
  ```

For deployments using the in-memory store, tasks do **not** persist across
process restarts. If `exec` exits before the task completes (timeout,
SIGINT, or process kill), the task is lost. Use the Redis or Postgres
store for production runs where you need to recover from interrupted
exec invocations.
