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
it downstream, and handles retries with configurable backoff.

```
                    ┌──────────┐
  Submit task ─────>│  intake   │
                    └────┬─────┘
                         │ on_success
                    ┌────▼─────┐
                    │ process   │
                    └────┬─────┘
                         │ on_success
                    ┌────▼─────┐
                    │ validate  │──── on_failure ───> process (retry)
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

## Security

Found a vulnerability? See [SECURITY.md](SECURITY.md) for responsible disclosure.

## License

Apache 2.0 — see [LICENSE](LICENSE).
