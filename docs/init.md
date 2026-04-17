# `overlord init` — scaffold a runnable project

`overlord init` writes a complete greenfield project from an embedded
template, then runs a sample input through the generated workflow using
the first-party `mock` provider. Zero credentials, zero network calls — you
see working output on the first run and decide later whether to swap in
a real LLM.

Use `init` for:

- **First-time evaluation** — go from `go install` to visible output
  without picking an LLM provider or setting an API key.
- **New project scaffolding** — the default starter produces a minimal
  workflow file (`overlord.yaml` + `sample_input.txt` + fixtures) you can
  run immediately with `overlord run`.
- **Examples for teaching or demos** — the advanced templates (`hello`,
  `summarize`) are short strict-pipeline examples that illustrate the
  escape-hatch format.

Use `overlord run` once you have a workflow; `init` is a one-time
bootstrap.

## Usage

```
overlord init [template] [dir] [flags]
```

- `[template]` — optional. Defaults to `starter`, which scaffolds the
  simple workflow format. Pass `hello` or `summarize` for the advanced
  strict-pipeline templates.
- `[dir]` — optional. Target directory. Defaults to `./<template>`.

Omitting the template argument writes the starter workflow into
`./starter`.

## Templates

| Template    | Shape                                           | Use when...                                                                |
|-------------|-------------------------------------------------|----------------------------------------------------------------------------|
| `starter`   | **Default.** Workflow format (one YAML file, two steps, mock provider) | You want the simple `overlord run --input-file sample_input.txt` story.    |
| `hello`     | Strict-mode single-stage pipeline (`greet`)     | You want the smallest strict-pipeline example — one agent, one schema pair. |
| `summarize` | Strict-mode two-stage linear pipeline (`summarize` → `validate`) | You want to see multi-stage routing and per-stage schema handoff in the strict format. |

The starter template is the default for a reason: it is the format most
new users should author against. The strict-pipeline templates remain
available for projects that need fan-out, conditional routing, retry
budgets, or the full `schema_registry` surface — or for projects that
have already graduated via `overlord export --advanced`.

Each strict-pipeline template defines two agent slots per stage: a `mock`
variant (active by default) and a commented real-provider variant using
Anthropic. The migration guide below explains the swap.

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

### Starter (default)

Running `overlord init` (no args) produces:

```
starter/
├── overlord.yaml        # workflow file — two steps, mock provider
├── sample_input.txt     # plain text fed in via `--input-file`
└── fixtures/
    ├── draft.json       # mock response for step 1
    └── review.json      # mock response for step 2
```

The starter is intentionally minimal — no strict-pipeline artifacts
(`schemas/`, `.env.example`, `.gitignore`) because workflow mode
synthesizes its schemas at compile time. To run the scaffold:

```bash
cd starter
overlord run --input-file sample_input.txt
```

To swap in a real LLM, change a step's `model:` to a real provider/model
(e.g. `anthropic/claude-sonnet-4-5`) and delete the matching `fixture:`
line. Provider credentials come from standard environment variables
(`ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, `GEMINI_API_KEY`).

### Advanced templates

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
- **Pick a bind address.** When you move off loopback (e.g. deploying
  behind a reverse proxy or listening on a specific interface), enable
  `auth:` first. `overlord run` refuses to start with a non-loopback
  bind while `auth.enabled=false` unless you pass `--allow-public-noauth`
  as an explicit opt-in.

### Runtime bind and auth guardrail

`overlord run` defaults to binding `127.0.0.1:8080` — a loopback-only
listener that nothing outside the host can reach. This is the reason
the scaffolded `auth:` block can safely ship commented-out. You choose
a different address with `--bind host[:port]` or by setting
`OVERLORD_BIND`.

Two layers make this safe:

1. **Loopback default.** Until you pass `--bind`, nothing off-host
   can hit the API — even if your firewall is open.
2. **Refusal on non-loopback + auth-off.** When `--bind` points at a
   non-loopback address and `auth.enabled=false`, `overlord run`
   refuses to start. Pass `--allow-public-noauth` if you deliberately
   want that combination (the warning still logs).

Both layers are removed automatically once you uncomment the `auth:`
block and add real keys. See
[docs/deployment.md#bind-address](deployment.md#bind-address) and
[docs/deployment.md#authentication](deployment.md#authentication).

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
