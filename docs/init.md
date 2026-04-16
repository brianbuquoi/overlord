# `overlord init` — scaffold a runnable project

`overlord init` writes a complete greenfield project from an embedded
template, then runs a sample payload through the generated pipeline using
the first-party `mock` provider. Zero credentials, zero network calls — you
see a working pipeline on the first run and decide later whether to swap in
a real LLM.

Use `init` for:

- **First-time evaluation** — go from `go install` to visible pipeline
  output without picking an LLM provider or setting an API key.
- **New project scaffolding** — every scaffolded project has sensible
  defaults (memory store, commented auth, `.gitignore` for secrets) that
  match the manual-authoring patterns documented in the main README.
- **Examples for teaching or demos** — the `hello` and `summarize`
  templates are intentionally short and easy to modify.

Use `overlord run` or `overlord exec` once you have a pipeline; `init` is
a one-time bootstrap.

## Usage

```
overlord init <template> [dir] [flags]
```

- `<template>` — required. One of the names returned by the embedded
  template catalog (see the table below).
- `[dir]` — optional. Target directory. Defaults to `./<template>`.

Missing the template argument prints the available templates and exits
with code 2. No interactive prompt — `init` is friendly to CI.

## Templates

| Template    | Shape                                           | Use when...                                                          |
|-------------|-------------------------------------------------|----------------------------------------------------------------------|
| `hello`     | Single-stage pipeline (`greet`)                 | You want the smallest runnable example — one agent, one schema pair. |
| `summarize` | Two-stage linear chain (`summarize` → `validate`) | You want to see multi-stage routing and per-stage schema handoff.    |

Every template defines two agent slots per stage: a `mock` variant
(active by default) and a commented real-provider variant using Anthropic.
The migration guide below explains the swap.

## Flags

| Flag                | Default | Effect                                                                                      |
|---------------------|---------|---------------------------------------------------------------------------------------------|
| `--force`           | off     | Scaffold into a non-empty target directory. Files that do NOT collide are written normally. |
| `--overwrite`       | off     | Requires `--force`. Colliding files are renamed to `<name>.overlord-init-bak.<timestamp>` before the new file is written. |
| `--no-run`          | off     | Write files only; skip the in-process demo. Next-steps block is still printed.              |
| `--non-interactive` | off     | Treat demo failure as a hard error (exit 4 instead of exit 0 + warning). Intended for CI.   |

`--overwrite` without `--force` is rejected. Backups are timestamped per
invocation (UTC to the second, plus a 4-character random tag if two runs
fall in the same second) so repeated `--force --overwrite` runs never
silently clobber a prior backup.

## Exit codes

| Code | Meaning                                                                                     |
|------|---------------------------------------------------------------------------------------------|
| 0    | Success. Includes soft demo failure under default (interactive) mode — files still persist. |
| 1    | Generic error (unexpected failure not classified below).                                    |
| 2    | Scaffold target invalid: no template arg, unknown template, non-empty dir without `--force`, collision without `--overwrite`, target is a symlink. |
| 3    | Write failure: permission denied, disk full, filesystem refused the rename, etc.            |
| 4    | Demo failure under `--non-interactive`.                                                     |
| 130  | SIGINT / SIGTERM received during the demo. Overrides `--non-interactive`.                   |

The matrix is intentionally distinct from `overlord exec` — init has its
own failure modes.

## What init produces

Scaffolding `hello` into `./hello` produces:

```
hello/
├── .env.example          # placeholder ANTHROPIC_API_KEY; init does NOT auto-source
├── .gitignore            # excludes .env, *.overlord-init-bak, fixtures/*.secret.json
├── overlord.yaml         # pipeline + agents + commented real-provider block + commented auth
├── sample_payload.json   # matches the first stage's input_schema
├── fixtures/
│   └── greet.json        # returned verbatim by the mock agent for the `greet` stage
└── schemas/
    ├── input_v1.json     # JSONSchema for the stage input
    └── output_v1.json    # JSONSchema for the stage output
```

File purposes:

- **`overlord.yaml`** — combined infra + pipeline config. Defines one
  pipeline, its stages, mock agent bindings, a commented real-provider
  banner block, a commented `auth:` block, and a memory store.
- **`schemas/*.json`** — versioned JSONSchema files referenced by the
  pipeline's `schema_registry`. The broker validates every stage I/O
  against these.
- **`fixtures/*.json`** — raw JSON values returned verbatim by the mock
  adapter for each stage. Validated against the stage's `output_schema`
  at agent-construction time; a broken fixture fails loudly on demo run,
  naming the offending path and schema violation.
- **`sample_payload.json`** — raw JSON value matching the first stage's
  `input_schema`. Used by the auto-run demo and surfaced in the printed
  next-steps block so you can copy the `overlord exec` command verbatim.
- **`.env.example`** — placeholder `ANTHROPIC_API_KEY` for the commented
  real-provider block. Deliberately not `sk-`-prefixed so secret scanners
  (gitleaks, GitHub push protection) will not greenlight an unchanged
  commit.
- **`.gitignore`** — excludes `.env`, `.env.*`, `*.overlord-init-bak`,
  and `fixtures/*.secret.json`; whitelists `.env.example`.

The `summarize` template has the same shape with two stages, two fixture
files, and three schemas.

## First-run flow

```
overlord init hello
        │
        ▼
  parse template + target + flags
        │
        ▼
  preflight: refuse target symlinks; check existing dir vs --force
        │
        ▼
  render embedded tree into sibling tempdir; atomic rename into target
        │
        ▼
  auto-run demo (skip under --no-run):
    load overlord.yaml → build broker → submit sample_payload.json →
    wait for terminal state (30s cap) → print result JSON to stdout
        │
        ▼
  print next-steps block to stderr:
    cd hello && overlord exec --config overlord.yaml --id hello --payload '…'
```

On demo success, the final stage payload goes to stdout and the
next-steps block goes to stderr. On demo failure, a warning banner is
printed, the next-steps block is still shown, and the exit code depends
on `--non-interactive` (see the exit-code table above).

## Migrating from mock to a real provider

Each template ships two agent definitions per stage: an active `mock`
entry (e.g. `greeter-mock`) and a commented Anthropic entry (e.g.
`greeter`) wrapped in banner markers. Swapping is a two-step edit:

1. **Uncomment the real-provider banner block.** Remove the `# ` prefix
   from every line between the banner markers:

   ```
   # === real provider: switch to this agent (uncomment + change stage reference…) ===
   # - id: greeter
   #   provider: anthropic
   #   model: claude-sonnet-4-20250514
   #   auth:
   #     api_key_env: ANTHROPIC_API_KEY
   #   …
   # === end real provider ===
   ```

2. **Change the stage's agent reference** from `<id>-mock` to `<id>`.
   For example, in the `hello` template:

   Before:
   ```yaml
   stages:
     - id: greet
       agent: greeter-mock
   ```

   After:
   ```yaml
   stages:
     - id: greet
       agent: greeter
   ```

3. **Set the API key** and re-run:

   ```bash
   export ANTHROPIC_API_KEY=your-key-here
   overlord exec --config overlord.yaml --id hello \
     --payload @sample_payload.json
   ```

You can leave the `*-mock` agent definitions in place — they are simply
unreferenced once the stage binds to the real agent. `overlord validate`
and template CI both pass after the swap.

Other provider blocks (OpenAI, Gemini, Ollama, plugins) are not
scaffolded. Add them the same way you would in a hand-authored config;
the main README documents the provider table.

## Production graduation

Scaffolded projects default to the **memory store** and a **commented
`auth:` block**. Both are deliberate first-run choices, not production
defaults.

When you are ready to expose the API beyond your laptop:

- **Switch the store backend** to Redis or Postgres. See
  [`docs/deployment.md`](deployment.md) for the store selection matrix,
  the Postgres table SQL, and multi-instance guidance.
- **Enable auth** by uncommenting the `auth:` block in `overlord.yaml`
  and adding real key entries. See
  [`docs/deployment.md#authentication`](deployment.md#authentication)
  for scopes, key generation, and CLI usage.

### Runtime auth guardrail

`overlord run` inspects `auth.enabled` and the HTTP bind address at
startup. If auth is disabled AND the bind address is not loopback
(`127.0.0.0/8`, `::1`, `localhost`), the process emits a loud `WARN`
log line naming the bind address and linking to the authentication
docs. It does not refuse to start — local-dev users often bind to LAN
addresses intentionally — but the warning is unmistakable in logs and
aggregators. This is the runtime-level mitigation for the commented
`auth:` block shipped by `init`.

## Adding a template

Templates live under `internal/scaffold/templates/<name>/` and are
embedded at build time via `//go:embed all:templates`. The `all:`
prefix is load-bearing — without it, Go's embed rules silently omit
files whose names start with `.` (namely `.env.example` and
`.gitignore`).

A new template must include every file the two shipped templates ship:
`overlord.yaml.tmpl`, a `schemas/` tree, a `fixtures/` tree,
`sample_payload.json`, `.env.example`, and `.gitignore`. Only files
with a `.tmpl` suffix are rendered through `text/template`; everything
else is copied verbatim. The rendered `.tmpl` suffix is stripped in the
output (`overlord.yaml.tmpl` → `overlord.yaml`).

Template CI under `internal/scaffold/` runs every template through
`config.Load`, a mock-mode exec, the mock-to-real migration swap, and
a goroutine-leak assertion on every PR.

## Non-goals

These are explicit scope boundaries — file a bug if you think any of
them are in scope:

- **Only `mock` and commented Anthropic.** No OpenAI, Gemini, Ollama,
  or plugin templates ship. Adding other providers is a hand-edit.
- **Only the memory store.** No Redis or Postgres templates.
- **No active `auth:` block.** The scaffolded block is commented with
  an enable-before-exposing warning; the runtime guardrail (above)
  catches the remaining footgun.
- **No in-place `--upgrade` or migration.** `init` is greenfield only;
  it refuses to scaffold into a directory with colliding files unless
  you pass `--force --overwrite`.
- **No fan-out or conditional-routing templates** in v1 — linear chains
  only.
- **No auto-discovery.** Every subsequent command (`run`, `exec`,
  `submit`, `validate`) still requires an explicit `--config` flag.
- **Mock adapter is not for production.** It is scoped to
  fixture-keyed responses, startup validation, and envelope-path
  parity for local demos and template CI. Out of scope: fuzzing,
  schema synthesis, record/replay, round-robin responses.
