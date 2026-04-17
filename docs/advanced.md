# Advanced (strict) configuration reference

Overlord's default authoring surface is the workflow format — see the
[README](../README.md) and [docs/init.md](init.md). This document is
the reference for the **advanced / strict** configuration format that
`overlord export --advanced` produces and that pipelines graduated
past the workflow surface use directly.

Use the strict format when you need any of:

- **Fan-out stages** across multiple reviewers in parallel
- **Conditional routing** based on an agent's output
- **Versioned schemas** per stage via `schema_registry`
- **Per-pipeline or per-agent retry budgets**
- **Dead-letter replay / discard** operations
- **Split infra + pipeline configs** (stable infra, iterating pipelines)
- **Long-running service deployment** with API keys and per-scope access

Workflows that only need a linear prompt chain should stay on the
workflow surface — the strict format trades simplicity for capability.

## Split-file layout

Strict configs can be split across two files or combined in one. The
split lets you keep stable infrastructure settings (store, agent
credentials) in one place and iterate on pipeline topology
independently.

| File          | Purpose                  | Contains                                        |
|---------------|--------------------------|-------------------------------------------------|
| Infra config  | Stable per-deployment    | `agents`, `stores`, `auth`, `observability`     |
| Pipeline file | Iterated per workflow    | `schema_registry`, `pipelines`                  |

Both files use the same `version` field. Merge happens at runtime —
the pipeline file is read, its schemas and pipelines are added to the
infra config, and every agent reference is verified against the infra
agents.

```bash
# Split configs:
overlord exec --config ./infra.yaml --pipeline ./pipeline.yaml \
  --id my-pipe --payload '{…}'

overlord submit --config ./infra.yaml --pipeline ./pipeline.yaml \
  --id my-pipe --payload '{…}' --wait

# Single combined file still works everywhere:
overlord exec --config ./everything.yaml --id my-pipe --payload '{…}'
overlord serve --config ./everything.yaml
```

See [`config/examples/infra.yaml`](../config/examples/infra.yaml) and
[`config/examples/pipelines/code_review.yaml`](../config/examples/pipelines/code_review.yaml)
for the split pattern, or [`config/examples/`](../config/examples/)
for combined-file examples. [`CLAUDE.md`](../CLAUDE.md) documents the
full YAML schema.

## Top-level blocks

| Block             | Purpose                                                               |
|-------------------|-----------------------------------------------------------------------|
| `schema_registry` | Declares versioned JSONSchema files referenced by stages              |
| `pipelines`       | Pipeline definitions with stages, routing, retry policies             |
| `agents`          | LLM provider bindings with model, auth, and prompt config             |
| `stores`          | Backend configuration for memory, Redis, or Postgres                  |
| `auth`            | API key authentication (disabled by default)                          |
| `dashboard`       | Web UI configuration (enabled by default)                             |
| `plugins`         | Custom provider adapter loading from .so files                        |
| `observability`   | Prometheus metrics path + OpenTelemetry tracing config                |

## Stage mechanics

Each stage is either a single-agent stage (`agent: <id>`) or a fan-out
stage (`fan_out: {...}`). Both declare `input_schema` and
`output_schema`; fan-out stages additionally declare
`aggregate_schema`. The broker validates each payload against the
declared schemas before and after agent execution.

### Fan-out stages

A fan-out stage executes multiple agents in parallel on the same
input. Each agent's output is validated against `output_schema`
individually, then all results are combined into an aggregate payload
validated against `aggregate_schema` before passing to the next stage.

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

**Gather mode** waits for all agents to complete and collects every
result. **Race mode** cancels remaining agents as soon as the require
policy is satisfied, returning only the results received so far.

Require policies:

- **all** — every agent must succeed.
- **any** — at least one agent must succeed.
- **majority** — more than half must succeed (rounds up: 2 of 3, 3 of 4).

### Conditional routing

Route tasks to different next stages based on field values in the
agent's output. Conditions are evaluated in order — first match wins.
A `default` target is required and used when no condition matches.

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
Field paths use dot notation (`output.findings[0].severity`). Missing
fields evaluate to false. `on_failure` routing remains unconditional —
conditions apply to `on_success` only.

### Retry budgets

Limit total retry attempts within a sliding time window at the
pipeline or agent level. When a budget is exhausted, tasks route to
`on_failure` immediately rather than being re-enqueued.

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
(up to the task timeout) rather than failing immediately. Budget
counters are stored in the state store so they work correctly in
multi-instance deployments.

## Authentication

API key authentication protects all endpoints except `/metrics`. Auth
is disabled by default for backward compatibility; enable it before
serving beyond loopback.

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

Keys are hashed with bcrypt (cost 12) at startup and the plaintext is
zeroed in memory. Key values are always read from environment
variables — never hardcoded in YAML. Keys must be 72 bytes or fewer
(bcrypt limit).

| Scope   | Access                                                            |
|---------|-------------------------------------------------------------------|
| `read`  | GET endpoints, WebSocket stream                                   |
| `write` | All read + task submission, dead-letter replay/discard            |
| `admin` | All write + bulk dead-letter operations                           |

`overlord serve` refuses to start when the resolved bind is
non-loopback AND `auth.enabled=false` unless the operator opts in via
`--allow-public-noauth`. See
[deployment.md](deployment.md#authentication) for the full threat
model.

## Dashboard

Overlord serves a built-in web dashboard at `/dashboard`. No separate
build step or frontend tooling required — the dashboard is embedded
in the binary.

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

When auth is enabled, the dashboard shows a login prompt on first load
and stores the API key in `sessionStorage` (cleared on tab close). The
`/dashboard` route itself is exempt from auth middleware — only the
API calls the dashboard makes are protected.

## Dead-letter queue management

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

Replay creates a new task with the original payload and a fresh
attempt count. The original dead-lettered task is atomically claimed
(`REPLAY_PENDING`), then marked `REPLAYED` (terminal audit state) on
successful submission; on submit failure it is rolled back to FAILED +
dead-lettered. Discarded tasks are excluded from `GET /v1/tasks` by
default (`?include_discarded=true` to include them).

Behavior:

- **Single replay** (`POST /v1/dead-letter/{id}/replay`) atomically
  claims the original task (→ `REPLAY_PENDING`), submits a new task,
  marks the original `REPLAYED` on success, or rolls back to
  FAILED+dead-lettered on submit failure.
- **replay-all** (`POST /v1/dead-letter/replay-all`) has the same
  per-task semantics, paginated, bounded at 100,000 tasks per call.
  Returns `processed`, `failed`, and `truncated` counts.
- **discard-all** (`POST /v1/dead-letter/discard-all`) discards all
  matching dead-lettered tasks, paginated, bounded at 100,000 tasks
  per call.
- **Recovery** for stranded `REPLAY_PENDING` tasks (double-failure):
  `POST /v1/tasks/{id}/recover` (write scope) or
  `overlord dead-letter recover --task <id>`.

API endpoints: `GET /v1/dead-letter`,
`POST /v1/dead-letter/{id}/replay`,
`POST /v1/dead-letter/{id}/discard`, and bulk variants. Bulk
operations require `admin` scope.

## Plugin system

Add custom LLM provider adapters without forking Overlord. Plugins are
Go shared libraries (`.so` files) that export a single `Plugin` symbol
implementing the `overlord.AgentPlugin` interface.

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
Only load plugins from trusted sources. See
[SECURITY.md](../SECURITY.md) for the plugin trust model.

Plugin development: implement `overlord.AgentPlugin` from
`pkg/plugin/`, compile with `go build -buildmode=plugin`, and drop the
`.so` in the plugins directory. See
[CONTRIBUTING.md](../CONTRIBUTING.md) for full instructions.

Note: Go's plugin package requires Linux or macOS with CGO enabled.

## Supported LLM providers

| Provider       | Models                                                            | Auth                        | Status                     |
|----------------|-------------------------------------------------------------------|-----------------------------|----------------------------|
| Anthropic      | claude-opus-4-5, claude-sonnet-4-*, claude-haiku-4-5-*            | `ANTHROPIC_API_KEY` env var | Stable                     |
| OpenAI         | GPT-4o, Codex, o-series                                           | `OPENAI_API_KEY` env var    | Stable                     |
| Google Gemini  | Gemini Pro, Flash                                                 | `GEMINI_API_KEY` env var    | Stable                     |
| Ollama         | Any model via Ollama REST API                                     | None (local)                | Stable                     |
| Mock           | Fixture-keyed stub (local demos, template CI — not for production)| None                        | Stable                     |
| GitHub Copilot | —                                                                 | —                           | Stub (waiting public API)  |

Custom providers can be added via the plugin system without forking
Overlord. See [Plugin system](#plugin-system) above.

## Related docs

- [README.md](../README.md) — workflow-first product story
- [docs/init.md](init.md) — `overlord init` scaffold reference
- [docs/exec.md](exec.md) — `overlord exec` single-task execution
- [docs/deployment.md](deployment.md) — single- and multi-instance deployment
- [docs/chain.md](chain.md) — legacy chain-mode authoring surface
