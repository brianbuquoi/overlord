# Overlord

Overlord is a local-first orchestration tool for AI agent workflows.
It gives you two layers on one runtime:

- **Chain mode** — a minimal authoring layer for linear prompt
  workflows. One YAML, a handful of steps, each a prompt and a model.
  Zero schema ceremony. This is the recommended first stop.
- **Pipeline mode** — the strict, production-oriented orchestration
  layer. Multi-stage DAGs with fan-out, conditional routing, typed
  versioned schemas, retries, dead-letter replay, auth, metrics, and
  tracing. This is where you graduate when the workflow is ready.

Chain mode **compiles into** pipeline mode. The runtime is the same
broker in both cases — there is no second engine. Starting simple and
scaling up means editing YAML, not rewriting your workflow.

```bash
# Chain mode — start here.
overlord chain init write-review
overlord chain run --chain ./write-review/chain.yaml \
  --input-file ./write-review/sample_input.txt

# Pipeline mode — graduate when you need fan-out, conditional
# routing, retry budgets, or the server/dashboard.
overlord chain export --chain ./write-review/chain.yaml --out ./pipeline
overlord run --config ./pipeline/overlord.yaml
```

See [docs/chain.md](docs/chain.md) for the chain authoring reference
and the graduation path.

## Why not a general agent framework or visual workflow tool?

- **Code-reviewable by default.** One YAML file per workflow (plus
  optional schemas and fixtures). Everything is diffable, grep-able,
  and lives in your repo.
- **Local-first.** Both layers run in-process against a memory store.
  No server required until you want one.
- **Operationally trustworthy.** The runtime ships with an anti-
  injection sanitizer envelope, schema versioning, typed contracts,
  retries, dead-letter replay, auth, Prometheus metrics, and
  OpenTelemetry tracing. Chain mode inherits all of it for free.
- **No vendor lock-in on models.** Anthropic, OpenAI (Chat +
  Responses), Google Gemini, Ollama, and a built-in `mock` provider
  for offline demos. A plugin system extends this without forking.

## How it works

A **pipeline** is a directed graph of **stages**. Each stage binds to an
**agent** (an LLM adapter) and declares versioned input/output schemas.
The **broker** routes tasks between stages, validates I/O contracts against
JSONSchema, wraps agent output in an anti-injection envelope before passing
it downstream, and handles retries with configurable backoff. **Fan-out
stages** execute multiple agents in parallel and collect results using
gather (wait for all) or race (first to satisfy) modes with configurable
require policies (all/any/majority).

A **chain** is a linear pipeline authored in a minimal YAML shape:
`chain.id`, `chain.vars`, `chain.steps`, `chain.output`. Each step is a
single prompt + model reference. The chain compiler produces a normal
pipeline, one stage per step, with synthesized schemas — then the
broker runs it. See [docs/chain.md](docs/chain.md).

```
                       ┌──────────┐
  Submit task ────────>│  intake   │
                       └────┬─────┘
                            │ on_success
              ┌─────────────┼─────────────┐
              │             │             │
         ┌────▼───┐   ┌────▼───┐   ┌────▼───┐
         │ claude  │   │ gemini │   │  gpt   │   fan-out: gather
         └────┬───┘   └────┬───┘   └────┬───┘   require: majority
              │             │             │
              └─────────────┼─────────────┘
                            │ aggregate
                       ┌────▼─────┐
                       │  judge    │
                       └────┬─────┘
                            │ on_success
                          DONE
```

Tasks follow the lifecycle:

```
PENDING → ROUTING → EXECUTING → VALIDATING → (DONE | FAILED | RETRYING)

Dead-letter replay flow:
FAILED (dead-lettered) → REPLAY_PENDING → REPLAYED (original task, terminal)
                                        ↘ FAILED (dead-lettered, if rollback occurs)
                      → (new task) PENDING → ... → DONE
```

- `REPLAY_PENDING`: transitional state set atomically when a dead-lettered
  task is claimed for replay. Indicates the replay submission is in progress.
  A task stuck in this state indicates a double-failure (both the replay
  submit and its rollback failed) — use `POST /v1/tasks/{id}/recover` or
  `orcastrator recover <id>` to restore it to dead-letter status.
- `REPLAYED`: terminal audit state. The original task ended here after a
  successful replay submission; the new task carries the retry.

## Quick start

Install:

```bash
go install github.com/brianbuquoi/overlord/cmd/overlord@latest
```

### Chain mode (start here)

```bash
overlord chain init write-review
overlord chain run --chain ./write-review/chain.yaml \
  --input-file ./write-review/sample_input.txt
```

The scaffolded chain uses the built-in `mock` provider so you see a
real multi-step run with zero credentials on the first invocation. To
switch to a real LLM, open `chain.yaml`, replace the step's
`model: mock/*` with a real provider/model (e.g.
`anthropic/claude-sonnet-4-5`, `openai/gpt-4o`,
`google/gemini-2.5-pro`, `ollama/llama3`), and delete the `fixture:`
line.

See [docs/chain.md](docs/chain.md) for the chain reference, template
catalog, and graduation path to pipeline mode.

### Pipeline mode (for production workloads)

```bash
overlord init summarize
cd summarize
# init already ran the scaffolded demo against the mock provider;
# re-run with your own input:
overlord exec --config overlord.yaml --id summarize \
  --payload '{"text": "..."}'
```

The scaffolded project uses the built-in `mock` provider so you see a
working pipeline on the first run. To switch to a real LLM, open
`overlord.yaml`, uncomment the real-provider block, and change the
stage's agent reference from `<id>-mock` to `<id>`.

See [docs/init.md](docs/init.md) for the pipeline template catalog,
flag reference, exit-code matrix, and file tree.

For long-running, multi-task deployments use `overlord run` (HTTP API,
web dashboard, broker workers). See [docs/exec.md](docs/exec.md) for
the `exec` command reference and [docs/deployment.md](docs/deployment.md)
for server-mode operations.

### Graduating from chain to pipeline

```bash
overlord chain export --chain ./chain.yaml --out ./pipeline
overlord validate --config ./pipeline/overlord.yaml
overlord run      --config ./pipeline/overlord.yaml
```

`chain export` emits the full-pipeline equivalent of a chain —
`overlord.yaml`, `schemas/`, and any referenced fixtures — into a
directory that is immediately runnable under the pipeline commands.
From there, hand-edit `overlord.yaml` to add fan-out, conditional
routing, retry budgets, split infra config, or anything else pipeline
mode supports.

If you prefer to hand-author your pipeline config — for split
infra+pipeline files, custom templates, or non-scaffolded workflows —
the [Configuration reference](#configuration-reference) below walks
through the raw YAML shape.

## Configuration reference

Overlord configs can be split across two files or combined in one. The split
lets you keep stable infrastructure settings (store, agent credentials) in
one place and iterate on pipeline topology independently.

| File | Purpose | Contains |
|------|---------|----------|
| Infra config | Stable per-deployment | `agents`, `stores`, `auth`, `observability` |
| Pipeline file | Iterated per workflow | `schema_registry`, `pipelines` |

Both files use the same `version` field. Merge happens at runtime — the
pipeline file is read, its schemas and pipelines are added to the infra
config, and every agent reference is verified against the infra agents.

```bash
# Split configs:
overlord exec --config ./infra.yaml --pipeline ./pipeline.yaml \
  --id my-pipe --payload '{…}'

overlord submit --config ./infra.yaml --pipeline ./pipeline.yaml \
  --id my-pipe --payload '{…}' --wait

# Single combined file still works everywhere:
overlord exec --config ./everything.yaml --id my-pipe --payload '{…}'
overlord run  --config ./everything.yaml
```

See [`config/examples/infra.yaml`](config/examples/infra.yaml) and
[`config/examples/pipelines/code_review.yaml`](config/examples/pipelines/code_review.yaml)
for the split pattern, or [`config/examples/`](config/examples/) for
combined-file examples. [`CLAUDE.md`](CLAUDE.md) documents the full YAML
schema.

Key top-level blocks:

| Block | Purpose |
|-------|---------|
| `schema_registry` | Declares versioned JSONSchema files referenced by stages |
| `pipelines` | Pipeline definitions with stages, routing, retry policies |
| `agents` | LLM provider bindings with model, auth, and prompt config |
| `stores` | Backend configuration for memory, Redis, or Postgres |
| `auth` | API key authentication (disabled by default) |
| `dashboard` | Web UI configuration (enabled by default) |
| `plugins` | Custom provider adapter loading from .so files |

### Fan-out stages

A fan-out stage executes multiple agents in parallel on the same input.
Each agent's output is validated against `output_schema` individually, then
all results are combined into an aggregate payload validated against
`aggregate_schema` before passing to the next stage.

```yaml
stages:
  - id: multi-review
    fan_out:
      agents:
        - id: claude-reviewer
        - id: gemini-reviewer
        - id: openai-reviewer
      mode: gather       # gather | race
      timeout: 60s
      require: majority  # all | any | majority
    input_schema:  { name: review_input,     version: "v1" }
    output_schema: { name: review_output,    version: "v1" }
    aggregate_schema:
      name: review_aggregate
      version: "v1"
    timeout: 90s
    retry: { max_attempts: 1, backoff: fixed, base_delay: 1s }
    on_success: judge
    on_failure: dead-letter
```

**Gather mode** waits for all agents to complete and collects every result.
**Race mode** cancels remaining agents as soon as the require policy is
satisfied, returning only the results received so far.

Require policies:
- **all** — every agent must succeed.
- **any** — at least one agent must succeed.
- **majority** — more than half must succeed (rounds up: 2 of 3, 3 of 4).

### Authentication

API key authentication protects all endpoints except `/metrics`. Auth is
disabled by default for backward compatibility.

```yaml
auth:
  enabled: true
  keys:
    - name: ci-pipeline
      key_env: OVERLORD_CI_KEY     # env var holding the plaintext key
      scopes: [write]
    - name: monitoring
      key_env: OVERLORD_MON_KEY
      scopes: [read]
    - name: operator
      key_env: OVERLORD_ADMIN_KEY
      scopes: [admin]
```

Keys are hashed with bcrypt (cost 12) at startup and the plaintext is zeroed
in memory. Key values are always read from environment variables — never
hardcoded in YAML. Keys must be 72 bytes or fewer (bcrypt limit).

| Scope | Access |
|-------|--------|
| `read` | GET endpoints, WebSocket stream |
| `write` | All read + task submission, dead-letter replay/discard |
| `admin` | All write + bulk dead-letter operations |

### Conditional routing

Route tasks to different next stages based on field values in the
agent's output. Conditions are evaluated in order — first match wins.
A default is required and used when no condition matches.

```yaml
stages:
  - id: judge
    agent: judge-claude
    input_schema:  { name: judge_input,  version: "v1" }
    output_schema: { name: judge_output, version: "v1" }
    on_success:
      routes:
        - condition: 'output.assessment == "approve"'
          stage: done
        - condition: 'output.critical_count > 0'
          stage: escalate
        - condition: 'output.assessment == "request_changes"'
          stage: implement
      default: done
    on_failure: dead-letter
```

Supported operators: `==`, `!=`, `>`, `>=`, `<`, `<=`, `contains`.
Field paths use dot notation (`output.findings[0].severity`).
Missing fields evaluate to false. `on_failure` routing remains
unconditional — conditions apply to `on_success` only.

### Pipeline dashboard

Overlord serves a built-in web dashboard at `/dashboard`. No
separate build step or frontend tooling required — the dashboard
is embedded in the binary.

The dashboard shows:
- Live pipeline topology with stage nodes and routing edges
- Per-stage queue depth badges updated every 5 seconds
- Real-time task event feed via WebSocket
- Agent health status polled every 30 seconds

Enable or disable via config:

```yaml
dashboard:
  enabled: true    # default: true
  path: /dashboard # default: /dashboard
```

When auth is enabled, the dashboard shows a login prompt on first
load and stores the API key in `sessionStorage` (cleared on tab close).
The `/dashboard` route itself is exempt from auth middleware — only
the API calls the dashboard makes are protected.

### Retry budgets

Limit total retry attempts within a sliding time window at the
pipeline or agent level. When a budget is exhausted, tasks route
to `on_failure` immediately rather than being re-enqueued.

```yaml
pipelines:
  - name: code-review
    retry_budget:
      max_retries: 100
      window: 1h
      on_exhausted: fail   # fail | wait

agents:
  - id: reviewer-claude
    retry_budget:
      max_retries: 50
      window: 1h
      on_exhausted: fail
```

`on_exhausted: wait` holds the task until the budget window refills
(up to the task timeout) rather than failing immediately.
Budget counters are stored in the state store so they work correctly
in multi-instance deployments.

### Dead letter queue management

Inspect, replay, and discard dead-lettered tasks via the CLI and API.

```bash
# List dead-lettered tasks
overlord dead-letter list --config pipeline.yaml

# Replay a single task (re-enqueues with fresh attempt count)
overlord dead-letter replay --config pipeline.yaml --task <id>

# Replay all dead-lettered tasks for a pipeline
overlord dead-letter replay-all --config pipeline.yaml --pipeline <id>

# Discard (permanently marks as DISCARDED, keeps record for audit)
overlord dead-letter discard --config pipeline.yaml --task <id>
overlord dead-letter discard-all --config pipeline.yaml --pipeline <id>
```

Replay creates a new task with the original payload and a fresh attempt
count. The original dead-lettered task is atomically claimed
(`REPLAY_PENDING`), then marked `REPLAYED` (terminal audit state) on
successful submission; on submit failure it is rolled back to FAILED +
dead-lettered. Discarded tasks are excluded from `GET /v1/tasks` by
default (`?include_discarded=true` to include them).

Behavior:

- **Single replay** (`POST /v1/dead-letter/{id}/replay`) atomically claims
  the original task (→ `REPLAY_PENDING`), submits a new task, marks the
  original `REPLAYED` on success, or rolls back to FAILED+dead-lettered on
  submit failure.
- **replay-all** (`POST /v1/dead-letter/replay-all`) has the same per-task
  semantics, paginated, bounded at 100,000 tasks per call. Returns
  `processed`, `failed`, and `truncated` counts.
- **discard-all** (`POST /v1/dead-letter/discard-all`) discards all
  matching dead-lettered tasks, paginated, bounded at 100,000 tasks per
  call.
- **Recovery** for stranded `REPLAY_PENDING` tasks (double-failure):
  `POST /v1/tasks/{id}/recover` (write scope) or
  `orcastrator recover <id>`.

API endpoints: `GET /v1/dead-letter`, `POST /v1/dead-letter/{id}/replay`,
`POST /v1/dead-letter/{id}/discard`, and bulk variants.
Bulk operations require `admin` scope.

### Plugin system

Add custom LLM provider adapters without forking Overlord. Plugins
are Go shared libraries (`.so` files) that export a single `Plugin`
symbol implementing the `overlord.AgentPlugin` interface.

```yaml
plugins:
  dir: ./plugins          # scan directory for .so files
  # or list specific files:
  files:
    - ./plugins/myprovider.so

agents:
  - id: my-custom-agent
    provider: myprovider  # matches ProviderName() from the plugin
    model: custom-v1
    auth: { api_key_env: MY_PROVIDER_KEY }
    extra:
      custom_option: value  # passed to PluginAgentConfig.Extra
```

Plugins run in the same process with the same privileges as Overlord.
Only load plugins from trusted sources. See [SECURITY.md](SECURITY.md)
for the plugin trust model.

Plugin development: implement `overlord.AgentPlugin` from
`pkg/plugin/`, compile with `go build -buildmode=plugin`, and drop
the `.so` in the plugins directory. See
[CONTRIBUTING.md](CONTRIBUTING.md) for full instructions.

Note: Go's plugin package requires Linux or macOS with CGO enabled.

## Supported LLM providers

| Provider | Models | Auth | Status |
|----------|--------|------|--------|
| Anthropic | claude-opus-4-5, claude-sonnet-4-*, claude-haiku-4-5-* | `ANTHROPIC_API_KEY` env var | Stable |
| OpenAI | GPT-4o, Codex, o-series | `OPENAI_API_KEY` env var | Stable |
| Google Gemini | Gemini Pro, Flash | `GEMINI_API_KEY` env var | Stable |
| Ollama | Any model via Ollama REST API | None (local) | Stable |
| Mock | Fixture-keyed stub (local demos, template CI — not for production) | None | Stable |
| GitHub Copilot | — | — | Stub (waiting for public API) |

> Custom providers can be added via the plugin system without forking
> Overlord. See [Plugin system](#plugin-system) above.

## Deployment

See [`docs/deployment.md`](docs/deployment.md) for complete deployment guidance
including security hardening (metrics access restriction, Postgres TLS).
Authentication configuration: see docs/deployment.md for securing the API
in production environments.

- **Single-instance**: one process with memory, Redis, or Postgres store.
- **Multi-instance**: multiple processes sharing a Postgres database with
  `FOR UPDATE SKIP LOCKED` for safe concurrent dequeuing.

## Development

```bash
make test-unit          # go test -race ./... (no external services)
make test-integration   # requires Docker (Redis + Postgres via compose)
make check              # tests + go vet + staticcheck

# Run the code review example pipeline
make example-code-review
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
- **Redis ListTasks optimisation** — Replace SCAN-based listing with
  a sorted set index for O(log N) pagination at scale.
- **Hot-reload subprocess reuse** — Currently, all plugin subprocesses
  are restarted on SIGHUP even if their configuration is unchanged.
  Reusing subprocesses for unchanged agents is planned.

## Security

Found a vulnerability? See [SECURITY.md](SECURITY.md) for responsible disclosure.

## License

Apache 2.0 — see [LICENSE](LICENSE).
