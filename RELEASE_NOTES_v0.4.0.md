# Overlord v0.4.0

First feature release under the Overlord name, building on the v0.3.0 rename.
Adds the external plugin system, a new exec command for scripting and CI,
infra/pipeline config separation, the OpenAI Responses API adapter, and a
production-hardened replay state machine — alongside six rounds of security
auditing.

## Highlights

### Run pipelines from the command line

`overlord exec` runs a single task through a pipeline and exits — no server
required. Progress to stderr, result to stdout.

```bash
overlord exec \
  --config ./infra.yaml \
  --pipeline ./pipelines/review.yaml \
  --id code-review-loop \
  --payload @./input.json \
  --output json | jq .code > fixed.go
```

### Split infra from pipeline config

Keep store and agent credentials in one stable `infra.yaml`. Iterate on
pipeline YAML files independently. Use `--pipeline` on both `exec` and
`submit` to load a standalone pipeline file.

### External plugins

Write Overlord agents in any language. Implement `execute` and `health_check`
JSON-RPC methods, define a YAML manifest, and configure with `provider: plugin`.
Plugins run as isolated subprocesses with environment isolation, configurable
restart behavior, and graceful in-flight RPC draining on hot-reload.

### OpenAI Responses API (Codex models)

New `openai-responses` provider enables `codex-mini-latest` alongside the
existing `openai` Chat Completions provider.

### Production-hardened replay

Full replay state machine with `REPLAY_PENDING` → `REPLAYED` audit trail,
safe under concurrent load. Recovery path via `overlord dead-letter recover`
and `POST /v1/tasks/{id}/recover`.

## Upgrade Notes from v0.3.0

- **Postgres:** run `migrations/003_store_parity.sql` before deploying.
- **Bulk responses:** `count` field renamed to `processed`. Update any clients.
- **`submit --pipeline`:** now accepts a pipeline file path, not a pipeline ID.
  Use `--id` for the pipeline ID. Update any scripts.
- **Redis:** existing tasks won't appear in pipeline-scoped state queries until
  their state next transitions (KG-005 — backfill script needed for full index
  consistency on live deployments).

## Known Limitations

See [KNOWN_GAPS.md](./KNOWN_GAPS.md). Multi-tenant hostile-network deployments
require additional work (per-tenant auth, plugin seccomp hardening).

## Installing

```bash
go install github.com/brianbuquoi/overlord/cmd/overlord@latest
```

See [README.md](./README.md) for the quickstart.
See [docs/exec.md](./docs/exec.md) for the exec command reference.
See [docs/plugin-security.md](./docs/plugin-security.md) for the plugin security model.
