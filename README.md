# Orcastrator

Orcastrator is a YAML-driven orchestration engine for AI agent pipelines.
Define multi-stage workflows that route tasks through LLM providers
(Anthropic, OpenAI, Google Gemini, Ollama) with typed I/O contracts,
automatic prompt injection sanitization, and schema versioning — all
configured in a single YAML file.

## How it works

A **pipeline** is a directed graph of **stages**. Each stage binds to an
**agent** (an LLM adapter) and declares versioned input/output schemas.
The **broker** routes tasks between stages, validates I/O contracts against
JSONSchema, wraps agent output in an anti-injection envelope before passing
it downstream, and handles retries with configurable backoff. **Fan-out
stages** execute multiple agents in parallel and collect results using
gather (wait for all) or race (first to satisfy) modes with configurable
require policies (all/any/majority).

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

Tasks follow the lifecycle: `PENDING -> ROUTING -> EXECUTING -> VALIDATING -> DONE | FAILED | RETRYING`.

## Quick start

Install:

```bash
go install github.com/orcastrator/orcastrator/cmd/orcastrator@latest
```

Create `pipeline.yaml`:

```yaml
version: "1"

schema_registry:
  - name: input
    version: "v1"
    path: input_schema.json
  - name: output
    version: "v1"
    path: output_schema.json

pipelines:
  - name: hello
    concurrency: 2
    store: memory
    stages:
      - id: greet
        agent: claude
        input_schema:  { name: input,  version: "v1" }
        output_schema: { name: output, version: "v1" }
        timeout: 30s
        retry: { max_attempts: 2, backoff: exponential, base_delay: 1s }
        on_success: done
        on_failure: dead-letter

agents:
  - id: claude
    provider: anthropic
    model: claude-sonnet-4-20250514
    auth: { api_key_env: ANTHROPIC_API_KEY }
    system_prompt: "You are a helpful assistant."
    temperature: 0.3
    max_tokens: 1024
    timeout: 30s

stores:
  memory: { max_tasks: 10000 }
```

Create minimal schemas (`input_schema.json` and `output_schema.json`):

```json
{ "type": "object" }
```

Run:

```bash
export ANTHROPIC_API_KEY=your-key-here

# Validate config
orcastrator validate --config pipeline.yaml

# Start the engine
orcastrator run --config pipeline.yaml

# In another terminal — submit a task
orcastrator submit --config pipeline.yaml --pipeline hello \
  --payload '{"request": "Say hello"}'
```

## Configuration reference

See [`config/examples/`](config/examples/) for complete pipeline examples and
[`CLAUDE.md`](CLAUDE.md) for the full YAML schema specification.

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
      key_env: ORCASTRATOR_CI_KEY     # env var holding the plaintext key
      scopes: [write]
    - name: monitoring
      key_env: ORCASTRATOR_MON_KEY
      scopes: [read]
    - name: operator
      key_env: ORCASTRATOR_ADMIN_KEY
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

Orcastrator serves a built-in web dashboard at `/dashboard`. No
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
orcastrator dead-letter list --config pipeline.yaml

# Replay a single task (re-enqueues with fresh attempt count)
orcastrator dead-letter replay --config pipeline.yaml --task <id>

# Replay all dead-lettered tasks for a pipeline
orcastrator dead-letter replay-all --config pipeline.yaml --pipeline <id>

# Discard (permanently marks as DISCARDED, keeps record for audit)
orcastrator dead-letter discard --config pipeline.yaml --task <id>
orcastrator dead-letter discard-all --config pipeline.yaml --pipeline <id>
```

Replay creates a new task with the original payload and a fresh
attempt count — the original dead-lettered task is preserved.
Discarded tasks are excluded from `GET /v1/tasks` by default
(`?include_discarded=true` to include them).

API endpoints: `GET /v1/dead-letter`, `POST /v1/dead-letter/{id}/replay`,
`POST /v1/dead-letter/{id}/discard`, and bulk variants.
Bulk operations require `admin` scope.

### Plugin system

Add custom LLM provider adapters without forking Orcastrator. Plugins
are Go shared libraries (`.so` files) that export a single `Plugin`
symbol implementing the `orcastrator.AgentPlugin` interface.

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

Plugins run in the same process with the same privileges as Orcastrator.
Only load plugins from trusted sources. See [SECURITY.md](SECURITY.md)
for the plugin trust model.

Plugin development: implement `orcastrator.AgentPlugin` from
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
| GitHub Copilot | — | — | Stub (waiting for public API) |

> Custom providers can be added via the plugin system without forking
> Orcastrator. See [Plugin system](#plugin-system) above.

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
- **Hot-reload topology changes** — Currently, adding or removing
  stages requires a restart. Full topology changes via SIGHUP are
  planned.

## Security

Found a vulnerability? See [SECURITY.md](SECURITY.md) for responsible disclosure.

## License

Apache 2.0 — see [LICENSE](LICENSE).
