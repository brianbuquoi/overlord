# Orcastrator Deployment Guide

## Deployment Models

Orcastrator supports two deployment models: single-instance and
multi-instance. Choose based on your throughput and availability requirements.

### Single-Instance

The simplest deployment. One Orcastrator process handles all pipeline stages.
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
orcastrator run --config config.yaml
```

### Multi-Instance

Multiple Orcastrator processes share a single Postgres database. Each
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
    table: orcastrator_tasks
```

Run on each host:

```bash
# All instances use the same config and DATABASE_URL
orcastrator run --config config.yaml
```

## Multi-Instance Configuration

### Postgres Setup

Create the tasks table before starting any instances. Apply the SQL manually:

```sql
CREATE TABLE IF NOT EXISTS orcastrator_tasks (
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
    ON orcastrator_tasks (stage_id, state, created_at);
```

The `(stage_id, state, created_at)` index is critical for dequeue
performance. Without it, the `FOR UPDATE SKIP LOCKED` query degrades to a
sequential scan under load.

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
      POSTGRES_DB: orcastrator
      POSTGRES_USER: orcastrator
      POSTGRES_PASSWORD: ${POSTGRES_PASSWORD}
    volumes:
      - pgdata:/var/lib/postgresql/data
    ports:
      - "5432:5432"
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U orcastrator"]
      interval: 5s
      timeout: 3s
      retries: 5

  migrate:
    image: postgres:16-alpine
    entrypoint: ["psql", "-h", "postgres", "-U", "orcastrator", "-d", "orcastrator", "-c"]
    command:
      - |
        CREATE TABLE IF NOT EXISTS orcastrator_tasks (
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
            ON orcastrator_tasks (stage_id, state, created_at);
    environment:
      PGPASSWORD: ${POSTGRES_PASSWORD}
    depends_on:
      postgres:
        condition: service_healthy

  orcastrator-1:
    image: orcastrator:latest
    command: run --config /etc/orcastrator/config.yaml
    environment:
      # sslmode=disable is safe here because postgres is on the same Docker
      # network. For production across networks, use sslmode=require or
      # sslmode=verify-full.
      DATABASE_URL: postgres://orcastrator:${POSTGRES_PASSWORD}@postgres:5432/orcastrator?sslmode=disable
      ANTHROPIC_API_KEY: ${ANTHROPIC_API_KEY}
      OPENAI_API_KEY: ${OPENAI_API_KEY}
      ORCASTRATOR_PORT: "8080"
    volumes:
      - ./config.yaml:/etc/orcastrator/config.yaml:ro
      - ./schemas:/etc/orcastrator/schemas:ro
    depends_on:
      migrate:
        condition: service_completed_successfully
    restart: unless-stopped

  orcastrator-2:
    image: orcastrator:latest
    command: run --config /etc/orcastrator/config.yaml
    environment:
      # sslmode=disable is safe here — same Docker network. See orcastrator-1 comment.
      DATABASE_URL: postgres://orcastrator:${POSTGRES_PASSWORD}@postgres:5432/orcastrator?sslmode=disable
      ANTHROPIC_API_KEY: ${ANTHROPIC_API_KEY}
      OPENAI_API_KEY: ${OPENAI_API_KEY}
      ORCASTRATOR_PORT: "8080"
    volumes:
      - ./config.yaml:/etc/orcastrator/config.yaml:ro
      - ./schemas:/etc/orcastrator/schemas:ro
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
      - orcastrator-1
      - orcastrator-2

volumes:
  pgdata:
  caddy_data:
```

**Caddyfile** (load balancer):

```
:80 {
    reverse_proxy orcastrator-1:8080 orcastrator-2:8080 {
        lb_policy round_robin
        health_uri /v1/health
        health_interval 10s
    }
}
```

## Authentication

### Enabling auth

Add an `auth` block to your YAML config. Auth is disabled by default so
existing deployments and local development are unaffected.

```yaml
auth:
  enabled: true
  keys:
    - name: ci-pipeline
      key_env: ORCASTRATOR_CI_KEY
      scopes: [write]
    - name: monitoring
      key_env: ORCASTRATOR_MON_KEY
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
| write | All read endpoints + POST /v1/pipelines/{id}/tasks, POST /v1/dead-letter/{id}/replay, POST /v1/dead-letter/{id}/discard |
| admin | All write endpoints + POST /v1/dead-letter/replay-all, POST /v1/dead-letter/discard-all |

Write scope implies read. Admin scope implies all.

### CLI usage

Pass the API key via the `--api-key` flag or the `ORCASTRATOR_API_KEY`
environment variable:

```bash
# Environment variable (recommended)
ORCASTRATOR_API_KEY=your-key-here orcastrator submit \
  --config config.yaml --pipeline hello \
  --payload '{"request": "Say hello"}'

# Flag
orcastrator submit --api-key your-key-here \
  --config config.yaml --pipeline hello \
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

    reverse_proxy orcastrator-1:8080 orcastrator-2:8080 {
        lb_policy round_robin
        health_uri /v1/health
        health_interval 10s
    }
}

# Internal-only metrics endpoint for Prometheus
:9090 {
    reverse_proxy /metrics orcastrator-1:8080 orcastrator-2:8080
}
```

**nginx example:**

```nginx
location /metrics {
    # Allow internal Prometheus scraper
    allow 10.0.0.0/8;
    deny all;
    proxy_pass http://orcastrator;
}
```

A future enhancement could add a separate `--metrics-port` flag to serve
metrics on a dedicated port.

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
| `ORCASTRATOR_PORT` | No | `8080` | HTTP API listen port |
| `LOG_LEVEL` | No | `info` | Log level: `debug`, `info`, `warn`, `error` |
| `ANTHROPIC_BASE_URL` | No | `https://api.anthropic.com` | Override Anthropic API endpoint (testing/proxying) |

## Deployment Checklist

### Single-Instance

- [ ] Choose store backend (`memory`, `redis`, or `postgres`)
- [ ] Set required environment variables for your agents
- [ ] Run `orcastrator validate --config config.yaml` to verify config
- [ ] Apply the tasks table SQL if using Postgres
- [ ] Start with `orcastrator run --config config.yaml`

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
