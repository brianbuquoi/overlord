# Overlord

Overlord runs simple AI workflows from one YAML file. Author a
workflow, run it locally, serve it as a local service, and graduate
to the strict config format only when you need fan-out, conditional
routing, retry budgets, or a long-lived production deployment.

```bash
# Scaffold a starter workflow (mock provider — zero credentials).
overlord init

# Run it locally.
cd starter && overlord run --input-file sample_input.txt

# Or serve it as a local HTTP service with the embedded dashboard.
overlord serve
```

A workflow is one file. One `workflow:` block. One linear sequence
of steps. Each step is a prompt plus a model.

```yaml
version: "1"

workflow:
  input: text
  output: text

  steps:
    - model: anthropic/claude-sonnet-4-5
      prompt: |
        Write a clear first draft.

        Request:
        {{input}}

    - model: openai/gpt-4o
      prompt: |
        Review and improve this draft for clarity, correctness, and gaps.

        Draft:
        {{prev}}
```

Overlord runs the same broker, sanitizer, schema validator, retries,
dashboard, metrics, and tracing in the background whether you author
a workflow or an advanced pipeline config — no second engine. The
workflow surface is a simpler front door, not a different runtime.

## Why pick Overlord

- **One file, one format.** A workflow is self-contained. No schema
  registry to maintain, no stages to wire, no store to pick on day
  one. Everything is diffable, grep-able, and lives in your repo.
- **Local-first.** `overlord run` executes in-process against a
  memory store. No server required until you want one.
- **One graduation path.** When you outgrow the simple surface,
  `overlord export --advanced --out ./advanced` writes the
  equivalent strict-config project (full `pipelines`, `agents`,
  `schema_registry`) and you hand-edit from there.
- **Operationally trustworthy.** Anti-injection sanitizer envelope,
  typed contracts, retries, dead-letter replay, Prometheus metrics,
  OpenTelemetry tracing. Workflows inherit all of it.
- **No vendor lock-in on models.** Anthropic, OpenAI (Chat +
  Responses), Google Gemini, Ollama, and a built-in `mock` provider
  for offline demos. Custom providers via plugins.

## Quick start

```bash
go install github.com/brianbuquoi/overlord/cmd/overlord@latest

overlord init                                    # scaffolds ./starter
cd starter
overlord run --input-file sample_input.txt       # one-shot local run
overlord serve                                   # long-running service + dashboard
```

The scaffolded starter uses the built-in `mock` provider so your
first run produces real multi-step output with zero credentials. To
switch to a real LLM, open `overlord.yaml`, change a step's
`model:` to a real provider/model (e.g. `anthropic/claude-sonnet-4-5`,
`openai/gpt-4o`, `google/gemini-2.5-pro`, `ollama/llama3`), drop the
matching `fixture:` line, and export the relevant API key
environment variable.

## Workflow authoring

### Placeholders

Steps support four placeholders in a `prompt:`:

| Placeholder             | Resolves to                                                    |
|-------------------------|----------------------------------------------------------------|
| `{{input}}`             | The workflow's initial input (text or JSON object, verbatim)   |
| `{{prev}}`              | Previous step's text output — the common case                  |
| `{{vars.<name>}}`       | A variable declared under `workflow.vars`                      |
| `{{steps.<id>.output}}` | An earlier step's output, when you've given steps explicit IDs |

Step IDs are optional — the common case is `{{prev}}`. Declare
explicit `id:` only when a later step needs to reference a specific
earlier step out of order.

### Input and output

`input: text` (the default) accepts a raw string via `--input` /
`--input-file`. `input: json` accepts a JSON object verbatim. For
both, `{{input}}` renders the full payload.

`output: text` (default) prints the final step's text. `output: json`
treats the final step's payload as structured JSON and optionally
validates it against an inline schema:

```yaml
workflow:
  output:
    type: json
    schema:
      type: object
      required: [summary, findings, verdict]
      properties:
        summary:  { type: string }
        findings: { type: array, items: { type: object } }
        verdict:  { type: string, enum: [approve, reject, revise] }
  steps:
    - ...
```

### Optional runtime block

`overlord serve` reads an optional `runtime:` block to pick the bind
address, store backend, and auth settings. Workflows without a
runtime block serve on `127.0.0.1:8080` with an in-memory store and
no auth.

```yaml
runtime:
  bind: 0.0.0.0:8080
  dashboard: true
  store:
    type: postgres
    dsn_env: DATABASE_URL
    table: overlord_tasks
  auth:
    enabled: true
    keys:
      - name: admin
        key_env: OVERLORD_ADMIN_KEY
        scopes: [admin]
```

`overlord run` ignores the runtime block — single-shot local runs
never bind a port and always use the in-memory store.

### Providers

Any provider the runtime understands is valid: `anthropic`,
`openai`, `openai-responses`, `google`, `ollama`, `mock`, `copilot`.
Write the model as `<provider>/<model>` (e.g.
`anthropic/claude-sonnet-4-5`, `openai/gpt-4o`). Mock steps require
a `fixture:` path relative to the workflow YAML's directory and can
use bare `mock` (the model field is unused).

Provider credentials come from environment variables:

| Provider   | Env var                              |
|------------|--------------------------------------|
| Anthropic  | `ANTHROPIC_API_KEY`                  |
| OpenAI     | `OPENAI_API_KEY`                     |
| Gemini     | `GEMINI_API_KEY`                     |
| Ollama     | `OLLAMA_ENDPOINT` (default localhost)|
| Mock       | none                                 |

## Graduating to the strict config

When you need fan-out across multiple reviewers, conditional routing,
per-stage retry budgets, split infra/pipeline configs, or any other
advanced capability, export the workflow:

```bash
overlord export --advanced --out ./advanced
overlord validate --config ./advanced/overlord.yaml
overlord serve --config ./advanced/overlord.yaml
```

The exported directory is a normal strict-mode project. Hand-edit
`overlord.yaml` to add fan-out stages, conditional routing, retry
budgets, or whatever else the strict surface supports. Your
workflow YAML remains the source for the simple version — `export
--advanced` is not a destructive migration.

See [docs/advanced.md](docs/advanced.md) for the strict-config
reference (`schema_registry`, `pipelines`, `agents`, `stores`,
`auth`, `dashboard`, fan-out, conditional routing, retry budgets,
dead-letter replay, plugins). Pipeline-authoring is the advanced
escape hatch — it is not the default story.

## Commands

| Command                  | Purpose                                                |
|--------------------------|--------------------------------------------------------|
| `overlord init`          | Scaffold a starter workflow project                    |
| `overlord run`           | Run a workflow locally with an input (default command) |
| `overlord serve`         | Serve a workflow as a local HTTP service + dashboard   |
| `overlord export`        | Export to the advanced strict config (`--advanced`)    |
| `overlord exec`          | (advanced) run a single strict-pipeline task           |
| `overlord submit`        | (advanced) submit a task via HTTP API                  |
| `overlord status`        | (advanced) inspect / watch a task                      |
| `overlord validate`      | (advanced) validate a strict config                    |
| `overlord health`        | (advanced) check health of configured agents           |
| `overlord pipelines`     | (advanced) list / show strict-mode pipelines           |
| `overlord dead-letter`   | (advanced) manage dead-lettered tasks                  |
| `overlord migrate`       | (advanced) schema migration tools                      |
| `overlord chain`         | legacy chain-mode authoring surface (still supported)  |

Workflows are the default. The advanced commands exist for projects
that graduated via `overlord export --advanced`.

## Deployment

See [`docs/deployment.md`](docs/deployment.md) for single-instance
and multi-instance deployment guidance, security hardening,
metrics-port separation, and Postgres TLS.

## Development

```bash
make test-unit          # go test -race ./... (no external services)
make test-integration   # requires Docker (Redis + Postgres via compose)
make check              # tests + go vet + staticcheck
```

## Roadmap

The following improvements are being tracked for future releases.
See [KNOWN_GAPS.md](KNOWN_GAPS.md) for a full list of known
limitations and their current status.

- **Authentication improvements** — Token revocation without restart,
  per-pipeline key scoping, and OAuth/OIDC integration.
- **Horizontal scaling improvements** — EventBus federation across
  instances so WebSocket clients receive events regardless of which
  instance processed the task.
- **Metrics port separation** — Serve `/metrics` on a dedicated port
  to simplify network-level access control in production.
- **Hot-reload subprocess reuse** — Currently, all plugin subprocesses
  are restarted on SIGHUP even if their configuration is unchanged.
  Reusing subprocesses for unchanged agents is planned.

## Security

Found a vulnerability? See [SECURITY.md](SECURITY.md) for responsible
disclosure.

## License

Apache 2.0 — see [LICENSE](LICENSE).
