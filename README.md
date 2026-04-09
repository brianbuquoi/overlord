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

## Supported LLM providers

| Provider | Models | Auth | Status |
|----------|--------|------|--------|
| Anthropic | claude-opus-4-5, claude-sonnet-4-*, claude-haiku-4-5-* | `ANTHROPIC_API_KEY` env var | Stable |
| OpenAI | GPT-4o, Codex, o-series | `OPENAI_API_KEY` env var | Stable |
| Google Gemini | Gemini Pro, Flash | `GEMINI_API_KEY` env var | Stable |
| Ollama | Any model via Ollama REST API | None (local) | Stable |
| GitHub Copilot | — | — | Stub (waiting for public API) |

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

The following features are planned for future releases. Build prompts
for each are available in `.claude/prompts/build/` for contributors
using Claude Code.

- **Conditional routing** — Route tasks to different next stages
  based on field values in the agent's output. Supports ==, !=, >,
  >=, <, <=, and contains operators against output payload fields.

- **Pipeline dashboard** — Single-page web UI served by Orcastrator,
  showing live pipeline topology, per-stage queue depths, task event
  feed, and agent health status. No separate build step required.

- **Retry budgets** — Pipeline-level and agent-level caps on total
  retry attempts within a sliding time window. Prevents runaway API
  consumption from a single bad task.

- **Dead letter queue management** — Inspect, replay, and discard
  dead-lettered tasks via the CLI and API. Includes bulk replay and
  discard with confirmation prompts.

- **Plugin system** — Load custom LLM provider adapters as Go shared
  libraries without forking Orcastrator. Drop a .so file in the
  plugins directory and reference the provider name in YAML.

## Security

Found a vulnerability? See [SECURITY.md](SECURITY.md) for responsible disclosure.

## License

Apache 2.0 — see [LICENSE](LICENSE).
