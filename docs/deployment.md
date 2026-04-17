# Overlord Deployment Guide

## Deployment Models

Overlord supports two deployment models: single-instance and
multi-instance. Choose based on your throughput and availability requirements.

### Single-Instance

The simplest deployment. One Overlord process handles all pipeline stages.
Suitable for development, low-throughput production, and environments where
downtime during deploys is acceptable.

**Store options:** `memory`, `redis`, or `postgres`.

- **memory** — zero dependencies, data lost on restart. Good for development.
- **redis** — persists tasks across restarts. Good for single-instance
  production where you want durability.
- **postgres** — full durability with transactional guarantees. Required if
  you plan to scale to multi-instance later.

```yaml
# config.yaml — single instance with memory store
pipelines:
  - name: my-pipeline
    concurrency: 4
    store: memory
    stages: [...]
```

Run:

```bash
overlord serve --config config.yaml
```

`overlord serve` is the long-running service command. `overlord run`
exists for one-shot local workflow runs and is not meant as a
production entry point — use `overlord serve` for any deployment that
expects to stay up.

### Multi-Instance

Multiple Overlord processes share a single Postgres database. Each
instance runs identical config and processes tasks from all stages. There is
no leader election — any instance can dequeue any task from any stage.

**Store requirement:** `postgres` (mandatory). The `SELECT ... FOR UPDATE
SKIP LOCKED` pattern ensures each task is dequeued by exactly one instance.

**How it works:**

1. All instances connect to the same Postgres database and table.
2. When a worker calls `DequeueTask`, the Postgres query atomically selects
   and locks the next PENDING task using `FOR UPDATE SKIP LOCKED`. Other
   instances skip that row and grab the next one.
3. `UpdateTask` uses a transaction with `SELECT ... FOR UPDATE` to prevent
   lost updates from concurrent writes.
4. Each instance runs its own worker pool. With `concurrency: 4` and 3
   instances, you get up to 12 concurrent workers per stage.

```yaml
# config.yaml — multi-instance with Postgres
pipelines:
  - name: my-pipeline
    concurrency: 4
    store: postgres
    stages: [...]

stores:
  postgres:
    dsn_env: DATABASE_URL
    table: overlord_tasks
```

Run on each host:

```bash
# All instances use the same config and DATABASE_URL
overlord serve --config config.yaml
```

## Multi-Instance Configuration

### Postgres Setup

Create the tasks table before starting any instances. The canonical
schema — matching what the Postgres store conformance tests create,
and what the live Postgres store code reads — is:

```sql
CREATE TABLE IF NOT EXISTS overlord_tasks (
    id                      TEXT PRIMARY KEY,
    pipeline_id             TEXT NOT NULL,
    stage_id                TEXT NOT NULL,
    input_schema_name       TEXT NOT NULL DEFAULT '',
    input_schema_version    TEXT NOT NULL DEFAULT '',
    output_schema_name      TEXT NOT NULL DEFAULT '',
    output_schema_version   TEXT NOT NULL DEFAULT '',
    payload                 JSONB,
    metadata                JSONB,
    state                   TEXT NOT NULL DEFAULT 'PENDING',
    attempts                INTEGER NOT NULL DEFAULT 0,
    max_attempts            INTEGER NOT NULL DEFAULT 1,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at              TIMESTAMPTZ,
    routed_to_dead_letter   BOOLEAN NOT NULL DEFAULT FALSE,
    cross_stage_transitions INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_tasks_stage_state_created
    ON overlord_tasks (stage_id, state, created_at);

CREATE INDEX IF NOT EXISTS idx_tasks_dead_letter
    ON overlord_tasks (state, routed_to_dead_letter)
    WHERE routed_to_dead_letter = TRUE;
```

If you are upgrading an older deployment that predates the
`routed_to_dead_letter` / `cross_stage_transitions` columns, apply
them with `ALTER TABLE`:

```sql
ALTER TABLE overlord_tasks
    ADD COLUMN IF NOT EXISTS routed_to_dead_letter   BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS cross_stage_transitions INTEGER NOT NULL DEFAULT 0;
```

The `(stage_id, state, created_at)` index is critical for dequeue
performance — without it, the `FOR UPDATE SKIP LOCKED` query degrades
to a sequential scan under load. The partial
`(state, routed_to_dead_letter)` index keeps dead-letter listing cheap.

**About `overlord migrate`.** The `migrate` subcommand handles task
*payload* schema migrations, not SQL schema setup. It is the tool for
re-shaping stored task payloads when a pipeline's JSON schema version
changes; it does not create or alter Postgres tables. SQL schema
setup is a one-time `CREATE TABLE` that you run with `psql` (or any
migration tool you already use) before the first instance starts.

### Concurrency Tuning

Each instance spawns `concurrency` workers per stage. With N instances and C
concurrency, you get `N * C` concurrent workers per stage.

```
Total workers per stage = instances * concurrency
```

Set `concurrency` based on your Postgres connection pool size. Each worker
holds a connection briefly during dequeue. A good starting point:

```
concurrency = postgres_max_connections / (num_instances * num_stages * 2)
```

### Load Balancing

Place a load balancer in front of the HTTP API for task submission and
status queries. All instances serve identical API endpoints.

## Known Limitations of Multi-Instance Deployment

### 1. EventBus is per-instance

The EventBus is an in-process pub/sub system. WebSocket clients connected to
instance A only see events from tasks processed by instance A. They do NOT
see events from tasks processed by instances B or C.

**Impact:** Real-time task monitoring via `/v1/stream` shows a partial view.
A client must connect to all instances (or use a shared event bus like Redis
Pub/Sub or NATS) to see all events.

**Workaround:** For full visibility, poll `GET /v1/tasks/{id}` against any
instance — task state is stored in Postgres and is consistent across all
instances.

### 2. Health endpoint is per-instance

`GET /v1/health` reports agent health for the agents registered on that
specific instance. If all instances run identical config (recommended), the
health responses are equivalent. If instances run different agent
configurations, each only reports its own agents.

### 3. Hot-reload requires per-instance SIGHUP

`SIGHUP` triggers config reload on the instance that receives it. Other
instances continue running the old config until they also receive `SIGHUP`.

**Recommendation:** Send `SIGHUP` to all instances when updating config. In
container orchestrators, this typically means a rolling restart.

### 4. No automatic re-delivery of in-flight tasks

When an instance shuts down (context cancelled), tasks that are mid-execution
are marked FAILED — not re-enqueued. The Postgres store only dequeues tasks
in PENDING state. There is no visibility timeout that automatically returns
ROUTING or EXECUTING tasks to the queue.

**Impact:** During a rolling restart, a small number of tasks may fail if
they were being actively processed by the shutting-down instance.

**Mitigation strategies:**

- **Retry at the caller level.** Re-submit tasks that reach FAILED state
  after a deploy.
- **Drain before shutdown.** Stop submitting new tasks, wait for in-flight
  tasks to complete, then shut down the instance.
- **Accept the failure rate.** For idempotent pipelines, failed tasks can be
  resubmitted without side effects.
- **Future improvement.** A visibility timeout on ROUTING/EXECUTING states
  would enable automatic re-delivery. This requires a background reaper that
  resets stale tasks to PENDING after a configurable timeout.

### 5. Metrics are per-instance

Prometheus metrics (`/metrics`) are collected per instance. Use Prometheus
federation or a shared push gateway to aggregate across instances.

### 6. Trace spans are per-instance

OpenTelemetry root task spans are stored in-memory per instance. If a task
is processed across stages by different instances, the trace spans from each
instance are separate. Use a shared trace collector (Jaeger, Tempo) to
correlate them via the task's `trace_id` metadata.

## Example: Docker Compose (2-Instance Production)

```yaml
# docker-compose.yml
services:
  postgres:
    image: postgres:16-alpine
    environment:
      POSTGRES_DB: overlord
      POSTGRES_USER: overlord
      POSTGRES_PASSWORD: ${POSTGRES_PASSWORD}
    volumes:
      - pgdata:/var/lib/postgresql/data
    ports:
      - "5432:5432"
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U overlord"]
      interval: 5s
      timeout: 3s
      retries: 5

  migrate:
    image: postgres:16-alpine
    entrypoint: ["psql", "-h", "postgres", "-U", "overlord", "-d", "overlord", "-c"]
    command:
      - |
        CREATE TABLE IF NOT EXISTS overlord_tasks (
            id                    TEXT PRIMARY KEY,
            pipeline_id           TEXT NOT NULL,
            stage_id              TEXT NOT NULL,
            input_schema_name     TEXT NOT NULL DEFAULT '',
            input_schema_version  TEXT NOT NULL DEFAULT '',
            output_schema_name    TEXT NOT NULL DEFAULT '',
            output_schema_version TEXT NOT NULL DEFAULT '',
            payload               JSONB,
            metadata              JSONB,
            state                 TEXT NOT NULL DEFAULT 'PENDING',
            attempts              INTEGER NOT NULL DEFAULT 0,
            max_attempts          INTEGER NOT NULL DEFAULT 1,
            created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
            updated_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
            expires_at            TIMESTAMPTZ
        );
        CREATE INDEX IF NOT EXISTS idx_tasks_stage_state_created
            ON overlord_tasks (stage_id, state, created_at);
    environment:
      PGPASSWORD: ${POSTGRES_PASSWORD}
    depends_on:
      postgres:
        condition: service_healthy

  overlord-1:
    image: overlord:latest
    command: serve --config /etc/overlord/config.yaml --bind 0.0.0.0 --allow-insecure-transport
    environment:
      # sslmode=disable is safe here because postgres is on the same Docker
      # network. For production across networks, use sslmode=require or
      # sslmode=verify-full.
      DATABASE_URL: postgres://overlord:${POSTGRES_PASSWORD}@postgres:5432/overlord?sslmode=disable
      ANTHROPIC_API_KEY: ${ANTHROPIC_API_KEY}
      OPENAI_API_KEY: ${OPENAI_API_KEY}
      OVERLORD_PORT: "8080"
    volumes:
      - ./config.yaml:/etc/overlord/config.yaml:ro
      - ./schemas:/etc/overlord/schemas:ro
    depends_on:
      migrate:
        condition: service_completed_successfully
    restart: unless-stopped

  overlord-2:
    image: overlord:latest
    command: serve --config /etc/overlord/config.yaml --bind 0.0.0.0 --allow-insecure-transport
    environment:
      # sslmode=disable is safe here — same Docker network. See overlord-1 comment.
      DATABASE_URL: postgres://overlord:${POSTGRES_PASSWORD}@postgres:5432/overlord?sslmode=disable
      ANTHROPIC_API_KEY: ${ANTHROPIC_API_KEY}
      OPENAI_API_KEY: ${OPENAI_API_KEY}
      OVERLORD_PORT: "8080"
    volumes:
      - ./config.yaml:/etc/overlord/config.yaml:ro
      - ./schemas:/etc/overlord/schemas:ro
    depends_on:
      migrate:
        condition: service_completed_successfully
    restart: unless-stopped

  caddy:
    image: caddy:2-alpine
    ports:
      - "80:80"
      - "443:443"
    volumes:
      - ./Caddyfile:/etc/caddy/Caddyfile:ro
      - caddy_data:/data
    depends_on:
      - overlord-1
      - overlord-2

volumes:
  pgdata:
  caddy_data:
```

**Caddyfile** (load balancer with TLS termination):

```
# TLS termination happens here — bearer keys MUST NOT be sent to
# Overlord over plaintext HTTP. Replace overlord.example.com with
# your real hostname (Caddy auto-provisions a certificate via Let's
# Encrypt) or use a local-TLS directive for self-hosted CAs.
overlord.example.com {
    reverse_proxy overlord-1:8080 overlord-2:8080 {
        lb_policy round_robin
        health_uri /v1/health
        health_interval 10s
    }
}

# Redirect all plaintext requests so clients cannot accidentally
# ship bearer keys in the clear.
http://overlord.example.com {
    redir https://{host}{uri} permanent
}
```

When auth is enabled, the compose file above passes
`--allow-insecure-transport` to each Overlord container because TLS
terminates at Caddy, not at Overlord. Without that flag, Overlord
would refuse to start on the non-loopback bind — see
[Bearer tokens require TLS](#bearer-tokens-require-tls) below.

## Authentication

> **Scaffolded projects:** `overlord init` generates projects with the
> memory store and a commented `auth:` block so the first-run demo works
> without credentials. The `overlord run` default bind is `127.0.0.1`
> (loopback-only), so a scaffolded project is not exposed outside the
> host. If you pass `--bind` to select a non-loopback address while
> `auth.enabled=false`, `overlord run` **refuses to start** unless you
> also pass `--allow-public-noauth`. This makes the "commented auth +
> public bind" footgun unreachable by default. See
> [docs/init.md](init.md) for the full graduation path (swap memory →
> Redis/Postgres, uncomment the `auth:` block, add real keys, then drop
> the override flag).

### Bind address

Use `--bind` to control which address `overlord serve` listens on.
Accepts either a bare host (`--bind 0.0.0.0`) combined with `--port`,
or a full `host:port` string (`--bind 10.0.0.5:8080`). Defaults to
`127.0.0.1`. The `OVERLORD_BIND` environment variable sets the flag
default.

```bash
# Loopback-only (default) — safest; external clients cannot reach the API.
overlord serve --config overlord.yaml

# Listen on all interfaces. Requires auth.enabled=true AND a TLS
# terminator upstream (see next section), or --allow-public-noauth.
overlord serve --config overlord.yaml --bind 0.0.0.0

# Opt-in to the dangerous combo (bind public + auth off). Logs a warning.
overlord serve --config overlord.yaml --bind 0.0.0.0 --allow-public-noauth
```

When `--bind` resolves to a non-loopback address and `auth.enabled=false`,
startup fails with:

> refusing to start: bind=<addr> is non-loopback and auth.enabled=false —
> enable auth or pass --allow-public-noauth

### Bearer tokens require TLS

Overlord does not terminate TLS. When auth is enabled, bearer keys
flow over whatever transport the client uses to reach the listener —
on a non-loopback HTTP bind, that is plaintext. Anyone on the wire
can capture or replay the key.

Startup enforces this:

> refusing to start: bind=<addr> is non-loopback with auth.enabled=true
> but Overlord does not terminate TLS — front this listener with an
> HTTPS reverse proxy or local TLS terminator, then pass
> `--allow-insecure-transport` to acknowledge that transport encryption
> is handled upstream

The flag name is deliberately uncomfortable: it exists to acknowledge
"bearer keys are on the plaintext wire between the proxy and Overlord"
when the proxy handles TLS. Do not set it on a listener that is
directly reachable from outside the host boundary.

Production deployments should:

1. Terminate TLS at a reverse proxy (Caddy, nginx, HAProxy, envoy,
   cloud load balancer, etc.) — the Caddy example above is the
   reference shape.
2. Keep the Overlord→proxy hop on a private network or localhost.
3. Pass `--allow-insecure-transport` only when the operator owns the
   transport between the terminator and Overlord.

### Enabling auth

Add an `auth` block to your YAML config. Auth is disabled by default so
existing deployments and local development are unaffected.

```yaml
auth:
  enabled: true
  keys:
    - name: ci-pipeline
      key_env: OVERLORD_CI_KEY
      scopes: [write]
    - name: monitoring
      key_env: OVERLORD_MON_KEY
      scopes: [read]
```

When `auth.enabled` is `true`, all API endpoints except `/metrics` require a
valid Bearer token. At least one key must be configured.

### Generating API keys

Generate a 64-character key (well under the 72-byte bcrypt limit):

```bash
openssl rand -base64 48 | tr -d '=' | head -c 64
```

**Important:** bcrypt truncates inputs longer than 72 bytes. Keys exceeding
this limit are rejected at startup with a clear error message. Always use
keys of 72 bytes or fewer.

### Scope reference table

| Scope | Endpoints |
|-------|-----------|
| read  | GET /v1/tasks, GET /v1/tasks/{id}, GET /v1/pipelines, GET /v1/health, WS /v1/stream, GET /v1/dead-letter |
| write | All read endpoints + POST /v1/pipelines/{id}/tasks, POST /v1/dead-letter/{id}/replay, POST /v1/dead-letter/{id}/discard, POST /v1/tasks/{id}/recover |
| admin | All write endpoints + POST /v1/dead-letter/replay-all, POST /v1/dead-letter/discard-all |

Write scope implies read. Admin scope implies all.

### CLI usage

Pass the API key via the `--api-key` flag or the `OVERLORD_API_KEY`
environment variable:

```bash
# Environment variable (recommended)
OVERLORD_API_KEY=your-key-here overlord submit \
  --config config.yaml --id hello \
  --payload '{"request": "Say hello"}'

# Flag
overlord submit --api-key your-key-here \
  --config config.yaml --id hello \
  --payload '{"request": "Say hello"}'
```

### Multi-instance auth

All instances authenticate independently — key lists are not shared via the
store. Configure every instance with the same keys (or a superset). If
instance A has keys [ci, mon] and instance B has only [ci], requests with
the mon key will succeed on A but fail on B.

### Metrics endpoint

`/metrics` is exempt from authentication per SEC2-NEW-002. Restrict access
to `/metrics` at the network or reverse proxy level in production. See the
[Security Hardening](#security-hardening) section below.

### Critical metrics for operators

Three series are worth watching on every deployment:

| Metric                                  | What it means                                                                                                          |
|-----------------------------------------|------------------------------------------------------------------------------------------------------------------------|
| `overlord_broker_store_errors_total`    | Non-zero means the broker tried to persist a state transition and the store refused. Sustained growth means tasks are stuck mid-transition or the store is failing — both are fail-closed per SEC-016, but the task remains in a non-terminal state until the next opportunity. Alert on any increase. The `operation` label distinguishes `update_task`, `requeue_task`, and `enqueue_task`. |
| `overlord_dead_letter_tasks_total`      | How many tasks have fully exhausted retries and landed in the dead-letter queue. A steady-state DLQ is fine; a rising one means upstream provider failures or contract mismatches need investigation — use `overlord dead-letter list` to triage. |
| `overlord_task_duration_seconds`        | Histogram of end-to-end task latency. Long tails typically mean a retry storm or a stuck stage. Pair with agent-level duration metrics to attribute.                                                                                           |

The `broker_store_errors_total` counter is new since the
fail-closed store-write fix (SEC-016). Operators migrating from a
pre-SEC-016 build should add an alert rule on this series before
cutting over — it is the signal that a previously-silent
persistence failure is now visible.

## Security Hardening

### Restrict access to /metrics

The `/metrics` endpoint is served on the same port as the REST API (default
8080). It exposes pipeline topology, agent names, and throughput data. In
production, restrict external access to `/metrics` at the reverse proxy or
firewall level while allowing internal scraping from your metrics
infrastructure.

**Caddy example:**

```
:80 {
    @metrics path /metrics
    respond @metrics 403

    reverse_proxy overlord-1:8080 overlord-2:8080 {
        lb_policy round_robin
        health_uri /v1/health
        health_interval 10s
    }
}

# Internal-only metrics endpoint for Prometheus
:9090 {
    reverse_proxy /metrics overlord-1:8080 overlord-2:8080
}
```

**nginx example:**

```nginx
location /metrics {
    # Allow internal Prometheus scraper
    allow 10.0.0.0/8;
    deny all;
    proxy_pass http://overlord;
}
```

A future enhancement could add a separate `--metrics-port` flag to serve
metrics on a dedicated port.

### Injection stress testing

Before deploying to production or upgrading model versions, run the
injection stress test to verify the envelope pattern and sanitizer are
functioning correctly:

```bash
for i in 1 2 3 4 5 6 7 8; do
  echo "=== VECTOR $i ==="
  overlord submit \
    --config config/examples/05-injection-stress-test.yaml \
    --id injection-stress-test \
    --payload test-inputs/05-vector-$i.json \
    --wait 2>&1 | grep -E "injection_detected|severity|sanitizer_warnings"
  echo ""
done
```

Expected result: all 8 vectors return `injection_detected: false`.
Vectors 1 and 2 should show `sanitizer_warnings` with active redaction.
Vectors 3-8 passing without warnings is expected — they rely on model
alignment as a second line of defense (see KNOWN_GAPS.md SAN-001).

If any vector returns `injection_detected: true`, do not deploy until
the vector is investigated and the sanitizer is hardened for that class.

### Postgres TLS

When deploying across networks, always use `sslmode=require` or
`sslmode=verify-full` in the `DATABASE_URL`. The `sslmode=disable` in the
docker-compose example is only safe on the Docker bridge network.

## Environment Variable Reference

| Variable | Required | Default | Description |
|----------|----------|---------|-------------|
| `DATABASE_URL` | Multi-instance | — | Postgres connection string |
| `REDIS_URL` | If using Redis store | — | Redis connection string |
| `ANTHROPIC_API_KEY` | If using Anthropic agents | — | Anthropic API key |
| `GEMINI_API_KEY` | If using Google agents | — | Google Gemini API key |
| `OPENAI_API_KEY` | If using OpenAI agents | — | OpenAI API key |
| `OLLAMA_ENDPOINT` | If using Ollama agents | `http://localhost:11434` | Ollama REST API endpoint |
| `OVERLORD_PORT` | No | `8080` | HTTP API listen port |
| `LOG_LEVEL` | No | `info` | Log level: `debug`, `info`, `warn`, `error` |
| `ANTHROPIC_BASE_URL` | No | `https://api.anthropic.com` | Override Anthropic API endpoint (testing/proxying) |

## Deployment Checklist

### Single-Instance

- [ ] Choose store backend (`memory`, `redis`, or `postgres`)
- [ ] Set required environment variables for your agents
- [ ] Run `overlord validate --config config.yaml` to verify config
- [ ] Apply the tasks table SQL if using Postgres
- [ ] Start with `overlord run --config config.yaml`

### Multi-Instance

- [ ] Use `postgres` store (mandatory)
- [ ] Provision Postgres with adequate `max_connections` for your instance count
- [ ] Apply the tasks table SQL once before starting instances
- [ ] Deploy identical config to all instances
- [ ] Place a load balancer in front of all instances
- [ ] Set up Prometheus scraping for each instance's `/metrics` endpoint
- [ ] Configure trace collector if using OpenTelemetry
- [ ] Note: WebSocket streams show per-instance events only — use task polling for full visibility
- [ ] Note: Send `SIGHUP` to each instance separately for config reload
