---
title: feat: `overlord init` scaffolder + first-class `mock` provider
type: feat
status: active
date: 2026-04-16
origin: docs/brainstorms/2026-04-16-overlord-init-requirements.md
deepened: 2026-04-16
---

# feat: `overlord init` scaffolder + first-class `mock` provider

## Overview

Add a new `overlord init <template> [dir]` CLI subcommand that scaffolds a runnable greenfield project (combined `overlord.yaml` + schemas + fixtures + sample payload + `.env.example` + `.gitignore`) and bundles a new first-class `mock` provider adapter in `internal/agent/mock/`. Two templates ship in v1: `hello` (single-stage) and `summarize` (2–3-stage linear chain). After writing files, `init` auto-runs a sample payload through the scaffolded pipeline using the mock provider; demo failure exits 0 with a warning (files always persist), scaffold failure exits non-zero.

Collapses the 4-file cold start into one command with zero API keys needed. Takes a user from `go install` to visible pipeline output in under 60 seconds.

## Problem Frame

A new user evaluating Overlord today must hand-author four files (`infra.yaml`, `pipeline.yaml`, two JSONSchemas), set an API key env var, and pick between three execution verbs before seeing any output. The `config/examples/` reference pipelines require copy-paste-edit and still don't produce output without a paid API key. Ideation identified this cold start as the single highest-leverage DX friction in the product.

(see origin: docs/brainstorms/2026-04-16-overlord-init-requirements.md)

## Requirements Trace

**Scaffolding**
- R1. `overlord init <template> [dir]` writes overlord.yaml + schemas/*.json + fixtures/*.json + sample_payload.json + .env.example + .gitignore
- R2. Default dir `./<template>/`; fail if target exists and is non-empty without `--force`; symlink refusal on target and on files init would create
- R3. No template arg → print available templates and exit non-zero (no interactive prompt); `--non-interactive` flag for CI
- R4. Two templates: `hello` and `summarize`

**Mock provider (first-class)**
- R5. New `internal/agent/mock/` adapter, peer to anthropic/google/openai/ollama. Construction takes the compiled `*contract.Registry` and the filtered `[]config.Stage` bindings so fixture-schema validation happens inside `New` — not through a separate interface. Every agent-construction code path (broker build, `healthCmd`, integration tests, SIGHUP hot-reload) automatically gets validation because the registry factory signature widens once.
- R6. Fixtures keyed by stage ID; schema validation against registered `output_schema` runs inside the mock adapter's constructor, so a broken fixture fails loudly at agent-construction time with the offending path and schema violation.
- R7. Mock output traverses the same sanitize/envelope path downstream (automatic via broker); per-fixture size cap of 256 KiB (generous enough for summarize demos with realistic inputs; sanitize envelope independently prevents runaway); fixture paths are resolved relative to the `overlord.yaml` file's directory — absolute paths, paths containing `..` after `filepath.Clean`, and paths that resolve outside the config directory via `strings.HasPrefix(resolved, base+os.PathSeparator)` check are rejected. The containment check is independent of fsutil symlink helpers.
- R8. Every template defaults to `provider: mock`
- R9. Mock and real-provider blocks use **distinct agent IDs** (e.g., `reviewer-mock` and `reviewer`). The scaffolded stage binding references `reviewer-mock`. The commented real-provider block is delimited by banner markers; the migration workflow is: uncomment the real block, change the stage's agent reference from `reviewer-mock` to `reviewer`. This removes the dependency on YAML-parser-rejects-duplicate-keys behavior. CI exercises the swap against a recorded Anthropic fixture tagged `integration`.
- R10. Recommended real-provider model defined in a single source of truth. The chosen constant must be verified against Anthropic's live catalog during implementation; a tagged-integration smoke test asserts the model ID resolves to a 2xx before shipping.

**Auto-run demo**
- R11. After scaffold, execute sample_payload.json through the pipeline with the mock; `--no-run` skips
- R12. Demo failure → prominent warning + files persist + exit 0 (except under `--non-interactive`: exit non-zero so CI catches it)
- R13. Print a next-steps block with a concrete `overlord exec --config ... --id ... --payload ...` command

**Credentials and safety**
- R14. `.gitignore` excludes `.env`, `.env.*`, `*.overlord-init-bak`, and `fixtures/*.secret.json`; whitelists `.env.example`. The `*.overlord-init-bak` exclusion prevents the `--force --overwrite` backup path from leaking the prior config (which may hold `api_key_env` references or debug inline credentials) into `git add .`
- R15. `.env.example` uses obviously-fake placeholder values; no auto-source
- R16. Commented `auth:` block with warning comment in generated `overlord.yaml`

**Success criteria**
- **Happy-path time-to-output:** on a reasonable development machine, `overlord init hello && cd hello` produces visible pipeline output in under 10 seconds (leaving 6× headroom against the 60-second post-install promise). A CI benchmark in Unit 6 asserts the p95 budget.
- **Demo failure behavior is soft:** if the demo fails, files persist, a clear warning is printed, and exit is 0 (except under `--non-interactive`, exit 4). The 60-second promise applies to the happy path; the trade-off is deliberate — first-impression robustness beats CI-signal strictness for interactive users, and `--non-interactive` restores CI-correct exit codes.
- **Determinism:** byte-identical **pipeline result output** across runs (excludes timing, task IDs, log lines). Byte-identical scaffolded file tree also.
- **Migration simplicity:** after uncommenting the real-provider block AND changing the stage's agent binding (distinct IDs — see R9), the same pipeline runs against the real LLM with no other edits. A CI integration test (tagged `integration`, requires `ANTHROPIC_API_KEY`) exercises this end-to-end against the live Anthropic API.
- **`overlord validate` passes** on every scaffolded config. Note: `validate` only checks YAML parse + schema-registry compile + contract compatibility; it does **not** construct agents, so a broken mock fixture will not fail `validate`. Template CI catches fixture issues via the mock-mode exec path, not via validate.
- **Template CI:** every template passes validate + mock-mode exec + the mock-to-real migration swap + a no-goroutine-leak assertion (`goleak.VerifyNone`) + a time-budget benchmark on every PR touching schemas/contract/templates.
- **Exit semantics:** scaffold failures always exit non-zero; demo failures exit 0 by default with a warning (files persist) and exit 4 under `--non-interactive`; SIGINT during the demo exits 130 regardless of flags.

## Scope Boundaries

- Only `mock` and commented Anthropic block in templates; no OpenAI, Gemini, Ollama, or plugin templates
- Only memory store in templates
- No active `auth:` block (only commented + warning)
- No `--upgrade` / in-place migration; greenfield only
- No fan-out or conditional-routing templates (defer to v2 with multi-response fixtures)
- No auto-discovery (separate brainstorm); every command still requires explicit `--config` until that lands
- Mock adapter scope: fixture-keyed responses, startup validation, envelope-path parity. Out of scope: fuzzing, schema synthesis without fixtures, record/replay, round-robin responses

## Context & Research

### Relevant Code and Patterns

- **Cobra subcommand wiring** — `cmd/overlord/main.go:84` (rootCmd) + `:117-127` (AddCommand list). New `initCmd()` factory follows the same pattern. Existing subcommands in `main.go` range 273-1902. Flag patterns: `cmd.Flags().StringVar(...)` + `cmd.MarkFlagRequired(...)` before `return cmd`.
- **Shared helpers reusable from `init`** — `main.go:145` (`newLogger`), `:158` (`loadConfig`), `:162` (`configBasePath`), `:170` (`buildContractRegistry`), `:246` (`buildStore`/`buildAgents`/`buildBroker`). The auto-run demo path should consume these the way `run` and `exec` do.
- **Agent adapter pattern** — `internal/agent/anthropic/anthropic.go` (full reference) and `internal/agent/copilot/copilot.go` (simpler mirror). Shape: exported `Config` struct, `Adapter` struct with `New(cfg, logger, m...) (*Adapter, error)`, methods `ID() / Provider() / Execute(ctx, *broker.Task) (*broker.TaskResult, error) / HealthCheck(ctx) error`. The `mock` adapter lives in `internal/agent/mock/mock.go`.
- **Agent interface** — `internal/agent/agent.go:63` (`Agent`, `Stopper`, `Drainer`, `AgentError`). Retryable/non-retryable errors via `*agent.AgentError`.
- **Registry factory** — `internal/agent/registry/registry.go:32-95`: `NewFromConfigWithPlugins(cfg config.Agent, plugins map[string]pluginapi.AgentPlugin, logger *slog.Logger, m ...*metrics.Metrics) (agent.Agent, error)`. Provider dispatch is a `switch cfg.Provider` block keyed on string literals. Does **not** receive a `*contract.Registry` — implementation unit 3 adds a post-construction validation hook rather than expanding this signature.
- **Contract registry** — `internal/contract/registry.go`: `NewRegistry(entries []config.SchemaEntry, basePath string) (*Registry, error)` and `Lookup(name, version string)`. Validator at `validator.go:45` with `ValidateOutput(name, taskVersion, stageVersion, payload)`.
- **Envelope/sanitize integration is broker-level, not adapter-level** — `internal/sanitize/envelope.go:15` (`Wrap`) plus `sanitizer.go` and `output.go`. Broker applies them at `broker/broker.go:536, 573, 641` and `broker/fanout.go:118, 155, 276`. Adapters just read `task.Prompt` — which means R7's "mock output flows through the same sanitize path" is automatic: the broker wraps whatever any adapter returns before the next stage. No special mock wiring needed.
- **First `//go:embed`** — `internal/dashboard/dashboard.go:11` embeds `dashboard.html`; single-file pattern. Scaffolder uses `//go:embed templates/*` in a new `internal/scaffold/` package.
- **Symlink rejection lives in two duplicated places** — `internal/config/config.go:39-50` and `internal/config/pipeline_file.go:38-47`. Unit 1 extracts a shared helper; init's write-side check uses it with `O_NOFOLLOW` on file creation.
- **Exit-code and typed-error pattern** — `cmd/overlord/exec.go:25-28, 106-113` defines `execExit*` constants and `execExitError`. `main.go:62-82` unwraps typed errors to set exit codes. `init` defines `initExitError` on the same pattern.
- **Default Anthropic model is NOT centralized** — `claude-opus-4-5` and `claude-sonnet-4-20250514` appear as string literals in README, CLAUDE.md, examples, and test fixtures. Unit 4 introduces a shared const for template defaults.

### Institutional Learnings

`docs/solutions/` does not exist — no institutional knowledge base yet. Implementation may be a good candidate for seeding the first entry there (embed.FS pattern choice, mock-adapter startup-validation design, symlink refusal utility).

### External References

Not used — the patterns are well-established in-repo. `embed.FS`, cobra subcommand factories, and Go `os.Rename` atomicity are standard library and well-patterned in this codebase.

## Key Technical Decisions

- **Atomic writes via tempdir + rename, with EXDEV fallback.** Scaffold writes all files into a sibling tempdir (`<parent>/.overlord-init-XXXX/`), verifies completeness, then `os.Rename` to the target. Eliminates partial-write states on Unix. Preflight `syscall.Stat` (POSIX) or `GetVolumeInformation` (Windows) compares the filesystem device of `tempdir-parent` and `target-parent`; if they differ (bind mount, tmpfs subdir, overlayfs upper straddling disk), the atomic-rename path falls back to per-file copy-and-delete. On Windows `os.Rename` fails over an existing non-empty dir, which aligns with `--force`-less semantics.
- **Symlink refusal scope: final target + init-created files only (not ancestors).** Using `os.OpenFile` with `O_NOFOLLOW` on POSIX for every file creation and an `Lstat`+`IsRegular`/`IsDir` check on the final target. On Windows (no `O_NOFOLLOW`), after opening each file, `fstat` the handle and compare the resulting inode/volume-serial to the pre-open `Lstat` — if mismatched, close + error. This closes the TOCTOU window without introducing a platform-specific flag. Ancestor symlinks are allowed because common macOS setups (`~/projects` symlinked to iCloud Drive, dotfiles) would otherwise reject legitimate usage. Rationale: flow analyzer gap #5; literal ancestor-symlink refusal would be macOS-hostile. The existing `internal/config` policy is read-only and has different threat-model semantics.
- **Exit-code matrix.** `initExitError` exposes codes: 0 (success incl. demo-fail under default), 1 (general error), 2 (scaffold-target invalid: non-empty + no `--force`, symlink refusal, template not found), 3 (write failure), 4 (demo failure under `--non-interactive`), 130 (SIGINT during demo, matches `exec.go` convention). Under `--non-interactive`, SIGINT still yields 130 (signal wins over demo-failure classification). `initExitError` implements `Unwrap()` to preserve underlying error chains for telemetry, matching `execExitError`. Reuses `execExitError` plumbing pattern from `cmd/overlord/exec.go`.
- **`--force` re-init safety: require explicit `--overwrite` for file collisions; backups are timestamped to avoid cascade.** `--force` alone lets init write into a non-empty dir that contains no files `init` itself would write. If any of the files init would create already exist, require `--force --overwrite`. Colliding files are renamed to `<name>.overlord-init-bak.YYYYMMDDHHMMSS` (UTC, monotonic suffix — add a 4-char random tag if multiple invocations fall in the same second). This means repeated `--force --overwrite` runs never silently clobber a prior backup — each run produces a new timestamped set. Rationale: flow analyzer gap #4 + adversarial finding on cascading backups.
- **In-process demo execution.** Auto-run reuses `loadConfig`, `configBasePath`, `buildContractRegistry`, `buildAgents`, `buildBroker` helpers from `main.go`. Does **not** shell out to `overlord exec` — avoids PATH dependency and matches the exec command's own in-process lifecycle.
- **Templates embedded via `//go:embed all:templates`** in a new `internal/scaffold/` package under `templates/<name>/`. The `all:` prefix is required — without it Go's embed rules silently omit files whose names start with `.` (e.g., `.env.example`, `.gitignore`), which would ship templates missing the R14/R15 credential-safety files. The existing `internal/dashboard/` pattern embeds a single non-hidden file and is not a sufficient reference for this case. Testable via CI (Unit 7) that walks the embedded tree and runs `overlord validate` + mock `exec` on each.
- **Mock startup validation via widened factory signature, not a public interface.** Earlier drafts proposed a new `agent.StartupValidator` optional interface invoked by the broker builder. Document review found that interface leaks: `overlord validate` skips it (only builds the contract registry); `healthCmd`, integration tests, and SIGHUP hot-reload paths construct agents directly via `registry.NewFromConfig` and would see a nil fixture map; and the interface exposed a permanent public contract for one implementer. Resolution: widen `registry.NewFromConfigWithPlugins` to accept `*contract.Registry` and the filtered `[]config.Stage` bindings for the agent. All four callers in `cmd/overlord/main.go` + `examples/code_review/live_test.go` + integration tests update once. Mock's constructor does fixture load + size cap + path containment check + schema validation inline — every code path that constructs the mock adapter gets validation automatically. No public interface surface created. Rationale: document-review finding #1 (consensus across feasibility, adversarial, scope-guardian, product-lens).
- **Sanitizer/envelope parity for mock is automatic.** Broker wraps all agent output before the next stage (`broker/broker.go:573`). Mock adapter has zero envelope code — it just returns a `*broker.TaskResult` like any other adapter. The 64 KiB size cap (R7) is enforced inside the mock adapter at fixture-load time; the sanitize path itself needs no change.
- **Fixture path constraints.** Fixture paths are resolved relative to the `overlord.yaml` file's directory (same as schemas). Absolute paths and `..`-prefixed relative paths are rejected at mock-adapter config parse time. Inherits the existing precedent for schema path resolution.
- **Default model constant, verified.** New `internal/scaffold/defaults.go` with `DefaultAnthropicModel`. The current codebase uses `claude-sonnet-4-20250514` in examples and tests — implementer must verify this model ID is live in Anthropic's current catalog before committing, record the verification timestamp in the Go doc comment, and add a tagged-integration smoke test (`//go:build integration`) that asserts the constant resolves to a 2xx against the live API. This is the only mitigation that actually fires before users notice staleness.
- **Deterministic output.** Warning and next-steps blocks use plain ASCII, no ANSI color codes. Template CI asserts output stability across runs. Defer color support (via `--color=auto|always|never`) to a follow-up.
- **`--non-interactive` is CI-authoritative.** Under `--non-interactive`, demo failure promotes to exit non-zero (code 4). Rationale: CI scripts assume the tool tells the truth via exit code; R12 default exit-0-on-demo-failure is a first-impression optimization for humans, not a CI contract.
- **Concurrent-init guard via `O_EXCL` on manifest.** Tempdir + rename eliminates the race on the target, but as a belt-and-suspenders the first file written to the tempdir (`overlord.yaml`) uses `O_CREATE|O_EXCL`. Rationale: flow analyzer gap #6.
- **Runtime auth guardrail (new Unit 5).** The commented `auth:` block in scaffolded configs is meaningless without a runtime check. `overlord run` emits a loud `WARN` log at startup when `auth.enabled=false` AND the HTTP bind address is not a loopback (`127.0.0.1`, `::1`, `localhost`). The warning names the bind address and points at docs/deployment.md#authentication. Does not refuse to start — that would break existing users' local-dev setups — but the warning is unmistakable in logs and aggregators. Rationale: document-review finding #10; without this guardrail, the scaffolder creates a direct path to the "graduated to public deploy with auth off" failure mode.
- **Template rendering uses `.tmpl` suffix marker.** Only files in the embedded template tree with a `.tmpl` suffix are rendered through `text/template`; all other files are copy-as-is. The `.tmpl` suffix is stripped from the output filename (`overlord.yaml.tmpl` → `overlord.yaml`). This avoids `{{`-collision hazards in JSON schemas, fixtures, or future adversarial-injection-fixture tests, and makes rendering intent explicit per-file. Rationale: document-review finding #12.
- **Broker lifecycle in the demo runner.** `runDemo` starts `b.Run(ctx)` in a goroutine with a cancelable context, submits the sample payload task, waits for terminal state via task-store polling (bounded 30s), cancels the context, `wg.Wait()`s the broker goroutine to drain (bounded 5s), then calls `registry.Stoppers(agents).Stop()` on each stopper. Unit 6's CI assertion uses `goleak.VerifyNone(t)` (or equivalent) to catch goroutine leaks that would indicate a lifecycle bug. Matches `cmd/overlord/exec.go`'s existing pattern; plan previously under-specified it.
- **Tempdir permissions and entropy.** Sibling tempdir created via `os.Mkdir(path, 0700)` with a name suffix of at least 64 bits of entropy from `crypto/rand` (`.overlord-init-<16-hex>`). On `EEXIST`, retry with fresh randomness up to 5 times then fail. Mode 0700 while render is in-flight avoids exposing pre-commit `.env.example` or fixture contents to other users on shared systems. After rename into place the target dir inherits the process umask (typically 0755); generated files are 0644.
- **Fixture size-cap enforcement.** Mock adapter uses `io.LimitReader` at fixture open time with a limit of 65536 bytes + 1 byte; if the reader is not EOF after 65536 bytes, the fixture is rejected as too large. Avoids buffering an entire oversized file before checking size.
- **`.env.example` placeholder shape.** Uses `ANTHROPIC_API_KEY=REPLACE_ME_NOT_A_REAL_KEY` — deliberately does not start with `sk-` so it cannot pattern-match a real Anthropic key and will trip secret scanners (gitleaks, GitHub) if a user accidentally commits the unmodified file. Header comment states `init` does not auto-source `.env`.
- **Backup files `.gitignore`'d in generated templates.** Every template's `.gitignore` excludes `*.overlord-init-bak` alongside `.env` / `.env.*` / `fixtures/*.secret.json`, so the `--force --overwrite` backup path cannot leak the prior config (which may have held inline credentials or `api_key_env` references) into `git add .`.

## Open Questions

### Resolved During Planning

- **Where do templates live?** → `//go:embed all:templates` in a new `internal/scaffold/` package. The `all:` prefix is required (without it, `.env.example`/`.gitignore` silently drop). Chosen over `cmd/overlord/templates/` to match the existing `internal/dashboard/` pattern for embedded assets.
- **Fixture file JSON structure?** → Fixture files are raw JSON values (e.g., `{"greeting": "Hello"}`) that become the task `Payload` directly. No wrapper, no metadata envelope. Startup validation calls `contract.Registry` → `validator.ValidateOutput(name, version, version, fixtureBytes)` on each. Adapter `Execute` returns `TaskResult{Payload: fixtureBytes}` verbatim; the broker envelope-wraps downstream.
- **Sample payload structure?** → `sample_payload.json` is a raw JSON value matching the first stage's `input_schema`. Example for `hello` template: `{"text": "Say hello to the world"}`.
- **How does the mock adapter access the compiled contract registry for startup fixture validation?** → Optional `agent.StartupValidator` interface; broker builder detects and invokes it after agents are wired. Avoids widening `NewFromConfigWithPlugins`.
- **What is the fixture file format?** → JSON, single-response-per-stage keyed by stage ID. Multi-response formats deferred to v2 with fan-out / conditional templates.
- **How is the mock agent neutralized when the user uncomments the real-provider block?** → Both blocks use the same agent `id`; YAML parser rejects duplicate keys under the same pipeline stage. Generated comment tells the user to comment out the mock block when uncommenting the real block. CI migration test asserts this flow works cleanly.
- **OS-specific file-write atomicity?** → `os.Rename` works cross-platform for our case (renaming tempdir over *nonexistent* target). `--overwrite` path performs file-by-file move-and-replace rather than dir-rename to avoid Windows `rename` limitations on non-empty dirs.
- **Model-name single source of truth?** → Go constant in `internal/scaffold/defaults.go`; template generators reference it at scaffold time (templates hold a `{{ .Model }}`-style placeholder resolved at embed-render time).

### Deferred to Implementation

- Exact help-text wording for "available templates" error (stable but implementer-authored)
- `.gitignore` wording explaining the nested-repo edge case (implementer-authored comment)

## High-Level Technical Design

> *This illustrates the intended approach and is directional guidance for review, not implementation specification. The implementing agent should treat it as context, not code to reproduce.*

### Scaffolder data flow

```
overlord init summarize [dir]
       │
       ▼
┌─ parse args ──────────────────────────────────────────────────┐
│  template = "summarize" (validated against embedded catalog)  │
│  target   = dir || "./summarize"                              │
│  flags    = {force, overwrite, no-run, non-interactive}       │
└───────────────────────────────────────────────────────────────┘
       │
       ▼
┌─ preflight (exit 2 on refusal) ───────────────────────────────┐
│  check target existence + emptiness (vs --force)              │
│  Lstat target → refuse if symlink                             │
│  if --force + --overwrite: record collision set for backup    │
│  writability probe (create + remove probe file)               │
└───────────────────────────────────────────────────────────────┘
       │
       ▼
┌─ render + write to sibling tempdir (exit 3 on write fail) ────┐
│  tempdir = <parent>/.overlord-init-<16-hex-crypto-rand>       │
│    (mode 0700; retry ≤5× on EEXIST)                           │
│  EXDEV preflight: same-filesystem check on parent vs target   │
│  walk embedded tree; .tmpl files rendered via text/template,  │
│    suffix stripped; all other files copied verbatim           │
│  O_CREATE|O_EXCL on manifest (concurrent-init guard)          │
│  POSIX: O_NOFOLLOW on all creates                             │
│  Windows: open + fstat + compare to pre-open Lstat            │
│  post-write Lstat + IsRegular as TOCTOU belt                  │
└───────────────────────────────────────────────────────────────┘
       │
       ▼
┌─ atomic commit ───────────────────────────────────────────────┐
│  same-fs target doesn't exist:   os.Rename(tempdir, target)   │
│  same-fs target exists (--force):                             │
│    collisions → <name>.overlord-init-bak.YYYYMMDDHHMMSS[rand] │
│    (repeated --force --overwrite never clobbers prior backup) │
│  cross-fs (EXDEV detected):  per-file copy then RemoveAll tmp │
└───────────────────────────────────────────────────────────────┘
       │
       ▼
┌─ auto-run demo (skip if --no-run) ────────────────────────────┐
│  loadConfig(target/overlord.yaml) → buildContractRegistry     │
│  buildAgents(cfg, plugins, logger, registry, stages)          │
│    → widened factory threads registry+stages into mock.New    │
│    → mock constructor validates fixtures inline               │
│    → any code path constructing the mock gets validation      │
│  buildStore (memory) → buildBroker                            │
│  go broker.Run(brokerCtx) in goroutine; submit sample_payload │
│  poll task → terminal state (30s timeout)                     │
│  brokerCancel; wg.Wait (5s); agent.Stopper.Stop (5s each)     │
│  demo result → stdout; any error → stderr warning             │
└───────────────────────────────────────────────────────────────┘
       │
       ▼
┌─ print next-steps block ──────────────────────────────────────┐
│  cd <target> && overlord exec --config overlord.yaml \        │
│    --id <pipeline> --payload '{"...": "..."}'                 │
│  # to switch to real Anthropic: uncomment the real provider   │
│  # block AND change stage.agent from <id>-mock to <id>        │
└───────────────────────────────────────────────────────────────┘
       │
       ▼
  exit 0 (or exit 4 if demo failed AND --non-interactive;
           or exit 130 on SIGINT during demo regardless of flags)
```

### Mock adapter construction: widened registry factory

```
// directional sketch only — widens internal/agent/registry signature

func NewFromConfigWithPlugins(
    cfg config.Agent,
    plugins map[string]pluginapi.AgentPlugin,
    logger *slog.Logger,
    registry *contract.Registry,   // NEW
    stages []config.Stage,         // NEW — filtered to bindings that reference this agent's ID
    m ...*metrics.Metrics,
) (agent.Agent, error) {
    switch cfg.Provider {
    // ... existing cases unchanged ...
    case "mock":
        return mock.New(toMockConfig(cfg), logger, registry, stages, m...)
    }
}
```

The mock adapter's `New` does all validation inline: reads each fixture referenced by `stages`, enforces the 256 KiB size cap, checks path containment against the config base (rejects absolute + `..`-containing + resolved-outside-base), calls `contract.Validator.ValidateOutput` on each fixture against its stage's `output_schema`, and stores validated `json.RawMessage` bytes for `Execute` to return verbatim. No broker-side walk, no new public interface — every code path that constructs a mock adapter gets validation automatically.

## Implementation Units

> **Note:** This plan was restructured after document-review. Old Unit 1 (fsutil extraction) is folded into Unit 2 (write primitives inlined in `internal/scaffold/`). Old Unit 3 (StartupValidator interface) is collapsed into Unit 1 (mock constructor receives the compiled registry + stages via widened factory). A new Unit 4 adds the runtime auth guardrail. Result: 7 units instead of 8, with tighter dependency graph and no public-interface surface introduced for one implementer.

- [ ] **Unit 1: Mock provider adapter with constructor-time fixture validation**

**Goal:** New first-class agent adapter at `internal/agent/mock/` that loads fixtures, validates them against registered output schemas inside its constructor, and returns fixture contents as stage output. No separate validation hook — every code path that constructs the adapter gets validation automatically via the widened registry factory signature.

**Requirements:** R5, R6, R7, R8

**Dependencies:** None

**Files:**
- Create: `internal/agent/mock/mock.go`
- Create: `internal/agent/mock/mock_test.go`
- Modify: `internal/agent/registry/registry.go` — widen `NewFromConfigWithPlugins` signature to accept `*contract.Registry` and `[]config.Stage` (bindings to the agent being constructed); add `case "mock":` branch
- Modify: `internal/agent/registry/registry.go` callers: `cmd/overlord/main.go` (both `buildAgents` and `healthCmd`), `examples/code_review/live_test.go`, `cmd/overlord/integration_test.go` — thread the registry + filtered stages through each
- Modify: `internal/config/types.go` — add `Fixtures map[string]string` to `config.Agent` (or place under existing `Extra` field if simpler)

**Approach:**
- `Config` struct mirrors minimal adapter shape: `ID`, `Provider`, `Fixtures map[string]string` (stage-id → relative path)
- `New(cfg Config, logger, registry *contract.Registry, stages []config.Stage, m ...*metrics.Metrics) (*Adapter, error)` — constructor does all work:
  1. For each stage binding, look up the fixture path; resolve relative to config base; run `filepath.Clean`; reject absolute paths, `..`-containing paths, and paths whose cleaned form does not `strings.HasPrefix(resolved, base + os.PathSeparator)`
  2. Open the fixture file via an internal `openFileNoFollow` helper (inlined, not extracted to `internal/fsutil`): on POSIX use `O_NOFOLLOW`; on Windows open normally, then `fstat` the handle and compare inode/volume-serial to a pre-open `Lstat`, closing + erroring on mismatch
  3. Read at most `256*1024 + 1` bytes via `io.LimitReader`; if the reader is not at EOF, reject as too large
  4. Call `registry.Lookup(stage.OutputSchema.Name, stage.OutputSchema.Version)` + `validator.ValidateOutput(name, version, version, bytes)`; reject with offending path + schema violation
  5. Store `fixtures map[string]json.RawMessage` on the Adapter, ready for `Execute`
- `Execute(ctx, *broker.Task) (*broker.TaskResult, error)` — look up `task.StageID` in `fixtures`. If present → return `TaskResult{Payload: fixtureBytes}` verbatim (broker envelope-wraps downstream). If missing → **non-retryable** `AgentError` (fixture was validated at construction, so a runtime miss indicates code-path drift, not transient fault).
- `HealthCheck(ctx) error` — returns nil; the constructor already guarantees fixtures are loaded and schema-valid, so `HealthCheck` has nothing further to assert.

**Patterns to follow:** `internal/agent/copilot/copilot.go` for minimal-adapter shape; `internal/agent/anthropic/anthropic.go` for Execute/AgentError conventions. For the registry factory signature-widening pattern, no direct precedent exists — this is a one-time internal-API refactor.

**Test scenarios:**
- Happy path: `New` with valid fixtures + valid schemas returns a working adapter; `Execute(task)` returns fixture bytes verbatim
- Edge case: fixture path with mid-path `..` (e.g., `subdir/../../etc/passwd`) after `filepath.Clean` → constructor fails with clear error
- Edge case: fixture path absolute → constructor fails
- Edge case: fixture file >256 KiB → constructor fails with size limit in message
- Edge case: fixture file missing → constructor fails naming the path
- Edge case: fixture file content fails schema validation → constructor fails with schema violation + fixture path
- Edge case: fixture file is a symlink on POSIX → `O_NOFOLLOW` rejects at open; on Windows → post-open inode check rejects
- Error path: `Execute` for stage ID with no fixture → non-retryable `AgentError` (should not happen in practice, but defensive)
- Integration: registry factory with `provider: "mock"` + a valid contract registry returns a working adapter; broker startup + a one-task demo run via the helper in Unit 4 succeeds end-to-end
- Integration: every other adapter (anthropic, openai, gemini, ollama, copilot) continues to work after the factory-signature widening — at least one happy-path test per adapter re-runs clean
- Integration: `overlord health --config <mock-config>` passes and `overlord validate` still passes (validate does NOT construct agents, so broken fixtures will NOT fail validate — that's intentional and tested in Unit 6)

**Verification:**
- `make test-unit` passes
- All four registry-factory call sites (`buildAgents`, `healthCmd`, `examples/code_review/live_test.go`, `cmd/overlord/integration_test.go`) are updated and their tests pass
- No new exported interface in `internal/agent/agent.go`; `StartupValidator` is NOT introduced

- [ ] **Unit 2: Embedded templates (`internal/scaffold/templates/`)**

**Goal:** Ship two runnable templates (`hello`, `summarize`) embedded via `//go:embed all:templates`. Each template is a complete project skeleton with distinct mock and real agent IDs to support the migration swap.

**Requirements:** R4, R8, R9, R10, R14, R15, R16

**Dependencies:** Unit 1 (templates reference the mock provider)

**Files:**
- Create: `internal/scaffold/templates.go` (defines `//go:embed all:templates`, exposes `FS embed.FS` and a `ListTemplates() []string` helper)
- Create: `internal/scaffold/defaults.go` (with `DefaultAnthropicModel` const + doc comment + verification timestamp)
- Create: `internal/scaffold/templates/hello/overlord.yaml.tmpl`
- Create: `internal/scaffold/templates/hello/schemas/input_v1.json`
- Create: `internal/scaffold/templates/hello/schemas/output_v1.json`
- Create: `internal/scaffold/templates/hello/fixtures/greet.json`
- Create: `internal/scaffold/templates/hello/sample_payload.json`
- Create: `internal/scaffold/templates/hello/.env.example`
- Create: `internal/scaffold/templates/hello/.gitignore`
- Create: `internal/scaffold/templates/summarize/...` (mirrors hello's shape with 2–3 linear stages)
- Create: `internal/scaffold/templates_test.go`

**Approach:**
- **Distinct agent IDs in each template** (resolves document-review Finding #4): scaffolded `overlord.yaml.tmpl` defines TWO agent entries — `reviewer-mock` (active, provider `mock`) and `reviewer` (commented, provider `anthropic`, model from `{{ .Model }}`). The stage binding references `reviewer-mock`. The commented block's banner reads `# === real provider: switch to this agent ===` / `# === end real provider ===` with an inline comment `# after uncommenting, change the stage's agent reference from reviewer-mock to reviewer`.
- **Only `.tmpl`-suffix files are rendered** through `text/template`; everything else copies verbatim. Rendered output strips the `.tmpl` suffix (`overlord.yaml.tmpl` → `overlord.yaml`). Avoids `{{`-collision in JSON fixtures/schemas and makes template-expansion intent explicit per file.
- Commented `auth:` block with an adjacent warning: `# auth: DISABLED by default. Enable before exposing beyond localhost. See docs/deployment.md#authentication.`
- `.env.example` uses `ANTHROPIC_API_KEY=REPLACE_ME_NOT_A_REAL_KEY` (no `sk-` prefix — secret scanners will trip on accidental commits) + header comment explaining `init` does not auto-source `.env`.
- `.gitignore` excludes `.env`, `.env.*`, `*.overlord-init-bak`, `fixtures/*.secret.json`; whitelists `.env.example` via `!.env.example`.
- `DefaultAnthropicModel` constant in `internal/scaffold/defaults.go`: implementer must verify against Anthropic's live catalog and record the verification date in the Go doc comment.

**Patterns to follow:** `internal/dashboard/dashboard.go:11` for `//go:embed` usage (but note the `all:` prefix is required here — not present in that reference); `config/examples/basic.yaml` for the overall config shape.

**Test scenarios:**
- Happy path: `ListTemplates()` returns `["hello", "summarize"]`
- Integration: each template's embedded `overlord.yaml.tmpl`, rendered with `DefaultAnthropicModel`, parses cleanly with `config.Load` (copied to a tempdir first)
- Edge case: every template contains every file listed in R1 — specifically `.env.example` and `.gitignore` are present (regression test for the `//go:embed all:` requirement)
- Edge case: `.gitignore` excludes `.env`, `.env.*`, `*.overlord-init-bak`, `fixtures/*.secret.json`; whitelists `.env.example`
- Edge case: commented real-provider banner delimiters are present exactly once per template; the block contains a `provider: anthropic` line that is currently commented
- Edge case: `.env.example` placeholder does NOT begin with `sk-` (secret-scanner-safe)
- Edge case: no file with a `.tmpl` suffix is present after rendering (suffix stripped correctly)
- Integration: rendered model placeholder equals `DefaultAnthropicModel`
- Integration (tagged `integration`): `DefaultAnthropicModel` resolves to a 2xx against the live Anthropic API

**Verification:**
- `go test ./internal/scaffold/...` passes
- Embedded templates produce valid YAML on render; rendered output is byte-identical across runs

- [ ] **Unit 3: Scaffold writer (`internal/scaffold/writer.go`)**

**Goal:** Render an embedded template into a target directory safely and atomically — tempdir + rename, EXDEV-safe fallback, symlink refusal (TOCTOU-safe on Windows too), collision detection with timestamped backups, writability probe.

**Requirements:** R1, R2, R14, R15

**Dependencies:** Unit 2

**Files:**
- Create: `internal/scaffold/writer.go`
- Create: `internal/scaffold/writer_test.go`
- Create: `internal/scaffold/fs_posix.go` (build tag `//go:build !windows`) — `O_NOFOLLOW` open
- Create: `internal/scaffold/fs_windows.go` (build tag `//go:build windows`) — open + `fstat` + compare to pre-open `Lstat`

**Approach:**
- `Write(ctx, template string, target string, opts Options) (Result, error)` where `Options{Force, Overwrite bool}` and `Result` names any files backed up under `--overwrite`
- **Preflight:**
  1. Validate template name via `validateID` (reuse from `internal/config`); if unknown, return error classified as exit 2
  2. `ensureNotSymlink(target)` (inline helper — `Lstat` + `ModeSymlink` check) — refuse if target exists as a symlink
  3. If target exists: `ReadDir`; if non-empty and `!Force`, return error classified as exit 2
  4. `probeWritable(parent(target))` — create + remove a probe file, surfaces permission issues early
  5. **EXDEV preflight:** `syscall.Stat_t.Dev` (POSIX) / `GetVolumeInformationW` (Windows) on `parent(target)` vs. system tempdir; if different filesystem device, the commit path will use per-file copy instead of dir-rename (see Commit)
- **Render:**
  - Create sibling tempdir `<parent>/.overlord-init-<16-hex-crypto-rand>` via `os.Mkdir` with mode 0700; retry with fresh randomness up to 5 times on EEXIST
  - Walk the embedded template tree under `templates/<name>/`
  - For each file ending in `.tmpl`: render through `text/template` against a `{Model: DefaultAnthropicModel, TemplateName: template}` context and strip the `.tmpl` suffix in the output path
  - For all other files: copy verbatim, preserving content byte-for-byte
  - Open each output file via `openFileNoFollow` (POSIX: `O_NOFOLLOW`; Windows: open + `fstat` + compare to pre-open `Lstat`, close-and-error on mismatch)
  - First file written to the tempdir (the rendered `overlord.yaml`) uses `O_CREATE|O_EXCL` as a belt-and-suspenders concurrent-init guard
  - Post-write `Lstat` + `IsRegular` verification as an additional TOCTOU belt
  - Generated file mode: 0644
- **Commit:**
  - **Same-filesystem path:** If target does not exist, `os.MkdirAll` parent, then `os.Rename(tempdir, target)`. Atomic at the directory level on POSIX.
  - **Cross-filesystem path (EXDEV detected at preflight):** `os.MkdirAll(target, 0755)`, walk tempdir and copy each file individually into target, then `os.RemoveAll(tempdir)`. Not atomic at the directory level but atomic per file.
  - **`--force` path (target exists, possibly with collisions):**
    - If any embedded file name collides with an existing file in target: require `Overwrite` flag; else return error classified as exit 2 listing collisions
    - Compute a timestamp suffix `YYYYMMDDHHMMSS` (UTC) once per call; if multiple calls within the same second would collide on the suffix, append a 4-character `crypto/rand` tag. Rename each colliding file to `<name>.overlord-init-bak.<suffix>` before writing the new file. Record backups in `Result`. Repeat runs produce new timestamped sets; no prior backup is ever clobbered.
- **Cleanup:** on any error after tempdir creation, `os.RemoveAll(tempdir)` best-effort

**Patterns to follow:** existing `os.Rename` usage in the store backends; `text/template` is stdlib. For symlink checks, mirror the read-side shape in `internal/config/config.go:39-50` but write-side inline (not extracted — see earlier decision).

**Test scenarios:**
- Happy path: fresh target on same filesystem as parent → tempdir + `os.Rename` path, writes complete project tree, empty backups
- Happy path: fresh target on different filesystem than parent (simulated by pointing `os.TempDir()` at a tmpfs mount) → EXDEV detected, per-file-copy fallback, writes complete project tree
- Happy path: rendered `overlord.yaml` has `{{ .Model }}` replaced with `DefaultAnthropicModel`; non-`.tmpl` JSON files are byte-identical to source (no template rendering)
- Edge case: target IS a symlink → refused regardless of `--force`
- Edge case: ancestor path contains a symlink (macOS `~/projects` symlinked to iCloud) → allowed (explicit ancestor permissiveness)
- Edge case: target non-empty, no `--force` → error, no files written, no tempdir leaked
- Edge case: target non-empty, `--force` without colliding files → writes into it, no backups
- Edge case: target non-empty, `--force` with colliding `overlord.yaml` but no `--overwrite` → error listing collision, no files written
- Edge case: target non-empty, `--force` and `--overwrite` with colliding `overlord.yaml` → existing file moved to `overlord.yaml.overlord-init-bak.20260416120000`, new file written
- Edge case: run `--force --overwrite` twice in a row (same second) → second run produces a distinct backup suffix (random tag appended); first backup file survives untouched
- Edge case: existing `.overlord-init-bak.*` files in target → not consumed as collisions (they aren't in the template set)
- Error path: permission-denied in parent → `probeWritable` returns clean error before any tempdir creation
- Error path: tempdir rename fails (simulated by pre-creating target between preflight and commit) → tempdir cleaned up, clear error
- Error path: POSIX-only → target file is a symlink mid-render → `O_NOFOLLOW` rejects; on Windows, the inode-compare rejects
- Integration: scaffolded `hello` config parses via `config.Load` successfully
- Integration: concurrent `Write` calls for same fresh target → exactly one succeeds (via `O_EXCL` on manifest + tempdir rename atomicity); other surfaces a clean error
- Error path: unknown template name → error classified as exit-2
- Integration (regression for `//go:embed all:`): scaffolded tree contains `.env.example` and `.gitignore`
- Integration: scaffolded tree contains no `.tmpl` suffixed files (suffix stripped)

**Verification:**
- `make test-unit` passes including new writer tests
- Template rendering yields byte-identical output across runs (determinism)
- Cross-platform CI (Linux + macOS + Windows if available) exercises the writer path

- [ ] **Unit 4: Runtime auth guardrail in `overlord run`**

**Goal:** Emit a loud startup warning when `auth.enabled=false` AND the HTTP bind address is not a loopback. This is the runtime-level mitigation for the commented `auth:` block scaffolded by `init`; without it, the comment+warning approach is inert when a user graduates to production.

**Requirements:** R16 (indirectly — enables the commented-auth pattern to be safe by default)

**Dependencies:** None (touches `overlord run`, not `overlord init`)

**Files:**
- Modify: `cmd/overlord/main.go` `runCmd` RunE (or whichever helper binds the HTTP listener) — after config load and before listener `Serve`, inspect `cfg.Auth.Enabled` and the bind address
- Modify: `internal/api/server.go` (if the bind address is computed there rather than in `main.go`)
- Create: test coverage in `cmd/overlord/main_test.go` or equivalent — one test per warning-trigger/no-trigger combination

**Approach:**
- After config load, normalize the bind address to a host part. Recognize loopback as: `127.0.0.0/8`, `::1`, `localhost`, empty string (implicit `0.0.0.0`, NOT loopback), `0.0.0.0`, `::`, or an explicit public IP.
- If `cfg.Auth.Enabled == false` AND bind is NOT loopback:
  - Emit a `logger.Warn` with fields `auth_disabled=true`, `bind_address=<host>`, `message="auth is disabled on a non-loopback bind address — enable auth before serving this instance"`, `doc="https://.../docs/deployment.md#authentication"`
  - Do NOT refuse to start — local-dev users intentionally bind to LAN addresses; refusing would break them
  - The warning is emitted once per process start, visible in logs and log aggregators
- Accessing `cfg.Auth` should not panic on `nil` — if the config omits the `auth:` block entirely (as scaffolded), treat `Enabled` as the zero value `false` and trigger the check normally

**Patterns to follow:** existing startup logging in `cmd/overlord/main.go` `runCmd`. Use the same `slog` logger; prefer structured fields over string interpolation.

**Test scenarios:**
- Happy path: auth enabled + any bind → no warning
- Happy path: auth disabled + bind `127.0.0.1:8080` → no warning (loopback explicit)
- Happy path: auth disabled + bind `localhost:8080` → no warning
- Happy path: auth disabled + bind `::1` → no warning
- Trigger: auth disabled + bind `0.0.0.0:8080` → warning emitted with bind address
- Trigger: auth disabled + bind `10.0.0.5:8080` → warning emitted
- Trigger: auth disabled + `auth:` block absent from config → warning emitted (treat absent as disabled)
- Edge case: auth enabled but all keys empty → out of scope for this unit (config validator catches this already)
- Verification: process starts successfully in all cases — the guardrail is warn-only

**Verification:**
- `make test-unit` passes
- The warning appears in logs when expected and is absent otherwise
- The server still starts and serves traffic in all tested cases

- [ ] **Unit 5: `overlord init` cobra command + auto-run demo**

**Goal:** Wire the scaffold writer and an in-process demo runner into a new `overlord init` subcommand. Demo runner has fully-specified broker lifecycle to avoid goroutine leaks.

**Requirements:** R1, R3, R11, R12, R13

**Dependencies:** Unit 3

**Files:**
- Create: `cmd/overlord/init.go`
- Create: `cmd/overlord/init_test.go`
- Modify: `cmd/overlord/main.go` (add `root.AddCommand(initCmd())` in the 117-127 block; extend the typed-error switch at `:62-82` to handle `initExitError`)

**Approach:**
- `initCmd()` factory mirrors `execCmd()` / `validateCmd()` shape
- Positional args: `template` (required), `dir` (optional)
- Flags: `--force`, `--overwrite`, `--no-run`, `--non-interactive`
- `initExitError` struct mirrors `execExitError` at `cmd/overlord/exec.go:106-113`, implements `Error()` and `Unwrap()` so wrapped errors propagate to telemetry.
- **RunE flow:**
  1. If no template arg → print the `ListTemplates()` output to stderr + "try: overlord init <name>" hint → return `initExitError{Code: 2}`
  2. Call `scaffold.Write(ctx, template, target, opts)`; on error, classify and return the right `initExitError` (code 2 for invalid target / unknown template / symlink refusal; code 3 for write failure)
  3. If `--no-run` → print next-steps block → return nil (exit 0)
  4. Otherwise: install a SIGINT handler (`signal.Notify(sigCh, os.Interrupt)`) for the demo lifetime; call `runDemo(ctx, target)` helper
  5. On demo success: print final stage result to stdout → print next-steps block → return nil (exit 0)
  6. On demo failure: print prominent warning to stderr with the underlying error → print next-steps block anyway → if `--non-interactive`, return `initExitError{Code: 4, Err: underlying}`; else return nil
  7. On SIGINT during demo: cancel the demo context, wait for teardown, return `initExitError{Code: 130}` regardless of flags
- **`runDemo(ctx, target)` helper — explicit broker lifecycle:**
  1. `loadConfig(target/overlord.yaml)` + `configBasePath`
  2. `buildContractRegistry(cfg, basePath)`
  3. `buildAgents(cfg, plugins, logger, registry)` — the widened factory from Unit 1 threads `*contract.Registry` + filtered stages into the mock constructor, triggering fixture validation there
  4. `buildStore(cfg)` — memory store for scaffolded templates
  5. `buildBroker(cfg, store, agents, registry, logger)`
  6. `brokerCtx, brokerCancel := context.WithCancel(ctx)`; `var wg sync.WaitGroup; wg.Add(1); go func() { defer wg.Done(); broker.Run(brokerCtx) }()`
  7. Read `sample_payload.json`; submit via `store.EnqueueTask` or the equivalent; poll `store.GetTask` until the task reaches a terminal state (DONE or FAILED) or the 30s timeout fires
  8. `brokerCancel()` (cancel broker context); `wg.Wait()` with a 5s timeout (the broker should drain quickly in a single-task memory-store pipeline)
  9. For each agent implementing `agent.Stopper`, call `Stop(ctx)` with a 5s timeout per agent
  10. Read the final stage result from the task record; return it (or the terminal error) to the caller
- **Deterministic output:** warning block and next-steps block use plain ASCII, no ANSI color codes. No timestamps or task IDs in the "pipeline result output" portion (which is what Success Criterion "Determinism" measures). The intermediate stage-transition log is allowed to include timings and is not part of the determinism contract.
- **Next-steps block format** (stable ASCII, no ANSI):
  ```
  cd <target> && overlord exec \
    --config overlord.yaml \
    --id <pipeline-id> \
    --payload '{"...": "..."}'
  # To use a real LLM, uncomment the real provider block in overlord.yaml
  # and change the stage's agent reference from <id>-mock to <id>.
  ```

**Patterns to follow:** `cmd/overlord/exec.go` for the in-process broker lifecycle pattern (it already does the goroutine + cancel + wait + Stop dance); `cmd/overlord/validate.go` / `main.go:875` for config-loading preflight. The SIGINT handler follows the convention at `cmd/overlord/exec.go:226`.

**Test scenarios:**
- Happy path: `overlord init hello` in a clean dir → scaffold + demo success → exit 0, stdout contains the pipeline output, stderr contains next-steps
- Happy path: `overlord init summarize my-proj --no-run` → files written, no demo → exit 0
- Edge case: no template arg → prints template list to stderr → exit 2
- Edge case: unknown template → prints available list + hint → exit 2
- Edge case: existing non-empty target → exit 2
- Edge case: `--force` into non-empty target without collisions → success
- Edge case: `--force` into target with colliding `overlord.yaml` (no `--overwrite`) → exit 2 listing collision
- Error path: demo fails (injected by test: broken fixture inside the scaffolded template directory) → scaffold-only success + warning + exit 0 by default
- Error path: demo fails under `--non-interactive` → exit 4, with `initExitError.Unwrap()` returning the underlying cause
- Edge case: SIGINT during demo → exit 130, broker context canceled, goroutine drained, no leaked goroutines
- Edge case: SIGINT during demo under `--non-interactive` → exit 130 (signal wins over demo-failure classification)
- Integration: scaffolded `hello` project passes `overlord validate` from the target dir
- Integration: next-steps block's printed `overlord exec` command runs successfully when invoked verbatim (regression test that the printed command is complete)
- Integration: `goleak.VerifyNone(t)` (or equivalent) at test cleanup — no goroutines leaked after the demo runs
- Edge case: 30s demo timeout fires → demo reported as failed, broker drained cleanly, exit 0 (non-interactive: exit 4)

**Verification:**
- `overlord init hello /tmp/overlord-smoke` produces output in under 10 seconds on CI hardware (see Unit 6 benchmark)
- Exit codes match the matrix exactly
- No goroutines leak from the demo path

- [ ] **Unit 6: Template CI coverage (benchmark + migration swap + goroutine leak)**

**Goal:** Enforce that every shipped template passes validate + mock-mode exec + the mock-to-real migration swap + a time-budget benchmark + a goroutine-leak assertion. Gates every plan-level success criterion.

**Requirements:** Template CI, Time-to-output SLA, R9 migration simplicity, Determinism

**Dependencies:** Unit 2, Unit 3, Unit 5

**Files:**
- Create: `internal/scaffold/ci_test.go`
- Create: `internal/scaffold/bench_test.go` (time-budget benchmark)
- Modify: `Makefile` (ensure `make check` runs both test files)

**Approach:**
- **Parametric CI test** iterates over `ListTemplates()`; for each:
  1. `scaffold.Write` into a fresh tempdir (`t.TempDir()`)
  2. `config.Load(target/overlord.yaml)` — asserts the config parses
  3. Run the same validate path the `overlord validate` command uses — asserts schemas compile and bindings resolve
  4. Run the in-process demo helper from Unit 5 — asserts demo produces valid output matching `output_schema`; asserts two consecutive runs produce byte-identical pipeline result output (determinism)
  5. **Mock-to-real migration swap test:** programmatically uncomment the real-provider block (strip banner-delimited comment markers from `overlord.yaml`) AND swap the stage's agent reference from `<id>-mock` to `<id>`. Re-parse with `config.Load` — assert the resulting config is valid. (Missing env var is fine; the assertion is YAML/schema/registry validity, not runtime success.)
  6. Assert the commented `auth:` block is present with the warning comment line
  7. Assert `.gitignore` excludes `.env`, `.env.*`, `*.overlord-init-bak`, `fixtures/*.secret.json` and whitelists `.env.example`
  8. Assert `.env.example` placeholder does NOT start with `sk-`
  9. `goleak.VerifyNone(t)` at cleanup — no goroutines leaked from the demo
- **Time-budget benchmark** (`bench_test.go`): `BenchmarkInitHello` runs `overlord init hello /tmp/bench-<N>` + demo in a subprocess (to include binary startup cost), reports wall-clock ns/op. A CI guard test `TestInitHelloTimeBudget` runs the benchmark once and asserts the observed time is under 10 seconds on CI hardware (a 6× safety margin against the 60-second user-machine promise). If this gate is flaky on CI, widen the budget toward 20s — but do not exceed 30s or the 60-second user-machine target is unverified.
- **Live-API integration test** (`//go:build integration`, requires `ANTHROPIC_API_KEY`): programmatically performs the mock-to-real migration swap and runs the scaffolded pipeline against the live Anthropic API. Asserts the same pipeline topology runs end-to-end with no other edits. Also asserts `DefaultAnthropicModel` resolves to a 2xx.

**Patterns to follow:** existing test helpers under `internal/config/config_test.go` for config-loading roundtrips; `github.com/anthropics/anthropic-sdk-go` (or whatever this repo already uses for Anthropic integration tests) for the live-API path.

**Test scenarios:** listed in Approach above, one test per template × nine assertions, plus the benchmark guard, plus the integration-tagged live-API migration test.

**Verification:**
- `make check` runs the CI test and the time-budget guard
- A deliberately broken fixture in a template causes the CI to fail with a clear error
- Changing a stage's schema version without updating the fixture causes the CI to fail with a clear error
- The goleak assertion catches a deliberately-introduced goroutine leak (e.g., a bug where `brokerCancel()` is forgotten in `runDemo`)
- The migration-swap test catches a bug where the mock and real agent IDs accidentally coincide
- Benchmark reports consistent wall-clock times across runs

- [ ] **Unit 7: Docs updates**

**Goal:** Teach users the new entry point and document the exit-code matrix, flags, migration path, and runtime auth guardrail.

**Requirements:** Documentation impacts

**Dependencies:** Unit 4, Unit 5

**Files:**
- Create: `docs/init.md`
- Modify: `README.md` (replace the existing 4-file quick-start with `overlord init summarize` as the new primary path; keep `go install`; note the existing manual-authoring path is still documented in the config reference)
- Modify: `CLAUDE.md` (add `init` subcommand to the CLI surface; add `mock` provider to the provider table; note `internal/scaffold/` package; note the `NewFromConfigWithPlugins` signature change; note the runtime auth guardrail in `overlord run`)
- Modify: `docs/deployment.md` (pointer: "scaffolded projects default to the memory store and commented auth; `overlord run` emits a loud warning when auth is disabled on a non-loopback bind. See docs/init.md for the graduation path to Redis/Postgres and enabled auth.")

**Approach:**
- `docs/init.md` covers: template catalog, flag reference (`--force`, `--overwrite`, `--no-run`, `--non-interactive`), exit-code matrix (0/1/2/3/4/130 with descriptions), commented-block uncomment+swap workflow (distinct agent IDs), how to add a template (pointer to `internal/scaffold/templates/`)
- **Explicit migration guidance:** "to switch from mock to Anthropic, uncomment the `# === real provider ===` block in overlord.yaml AND change the stage's agent reference from `<id>-mock` to `<id>`, then set `ANTHROPIC_API_KEY`. Scaffolded tests and `overlord validate` both pass after the swap."
- **Explicit graduation guidance:** "to move from memory store to Postgres/Redis, see docs/deployment.md; to enable auth, uncomment the `auth:` block and read docs/deployment.md#authentication. Note: if you are exposing this instance beyond localhost and leave auth disabled, `overlord run` will warn you prominently in its startup logs."
- **Cross-reference:** `CLAUDE.md` scaffolder/runtime-guardrail mentions should link to `docs/init.md` so future-agent context picks up the init workflow naturally.

**Test scenarios:** Test expectation: none — pure documentation changes.

**Verification:**
- `README.md` links resolve (manual check)
- `docs/init.md` covers every flag and exit code implemented in Unit 5
- `CLAUDE.md` mentions `mock` in the provider table, `overlord init` in the CLI list, and the new runtime auth guardrail

## System-Wide Impact

- **Interaction graph:** `registry.NewFromConfigWithPlugins` gains two required parameters (`*contract.Registry`, `[]config.Stage`). This is a one-time internal-API signature widening with four in-repo callers, all updated in Unit 1. No new public interface surface is introduced (`StartupValidator` was considered and rejected). `overlord run` gains a startup-time auth warning path in Unit 4.
- **Error propagation:** `initExitError` mirrors `execExitError` shape with an added `Unwrap()` for telemetry; `main()`'s `errors.As` switch gets one new case. Demo failures inside `init` propagate via a clear warning path without cascading into scaffold rollback. SIGINT during the demo always propagates as exit 130 regardless of `--non-interactive`.
- **State lifecycle risks:** tempdir + rename eliminates partial-write states on the happy same-filesystem path; EXDEV detected at preflight triggers per-file copy fallback. `--overwrite` uses timestamped backup names (`*.overlord-init-bak.YYYYMMDDHHMMSS`) so repeated runs never clobber prior backups. Backup files are `.gitignore`d to prevent accidental secret leaks. If the process dies mid-commit, the user sees a mix of `.overlord-init-bak.<timestamp>` files and new files — the CLI's behavior is to fail-fast, leave backups in place, and let the user diagnose. Cleaning up a failed `--overwrite` is out of scope (the user has backups).
- **API surface parity:** new CLI verb (`init`), new provider name (`mock`), new embedded asset tree. No changes to the HTTP API, WebSocket, metrics, or dashboard. `overlord run` gains one startup-time log line when the auth-disabled-on-non-loopback condition is triggered.
- **Integration coverage:** Template CI (Unit 6) is the crucial cross-layer assertion that unit tests alone cannot prove — it runs embedded templates through the real validate + mock broker paths, asserts goroutine leak freedom, exercises the mock-to-real migration swap, and runs a live-API smoke test tagged `integration`.
- **Unchanged invariants:** YAML remains the single source of truth (templates ARE YAML); `schema_registry` canonicality is preserved (templates register schemas the same way); envelope/sanitize semantics are unchanged (mock returns output, broker wraps it); agent statelessness (mock holds fixtures loaded at constructor time, same lifecycle as Anthropic's HTTP client). Auto-discovery is explicitly out of scope — `--config` remains required on every subsequent command.

## Risks & Dependencies

| Risk | Mitigation |
|------|------------|
| Template drift: a `schema_registry` change in templates silently breaks mock fixtures, surfacing in users' first impression | Unit 6 Template CI runs on every PR touching `internal/contract/`, `schemas/`, `internal/agent/mock/`, or `internal/scaffold/`. `make check` blocks the PR. |
| Symlink-refusal TOCTOU race on write path | POSIX: `O_NOFOLLOW` on every file open + post-write `Lstat` verification. Windows: open + `fstat` + compare inode/volume-serial to pre-open `Lstat`; close-and-error on mismatch. The Windows inode-check closes the residual race window the original draft called out as "documented but unmitigated." |
| `--force --overwrite` causes silent data loss on user's real project | `--overwrite` requires both flags; colliding files are backed up to `*.overlord-init-bak.<timestamp>` with per-call timestamp suffix (random tag if within same second). Repeated `--force --overwrite` runs never clobber prior backups. Backup files are `.gitignore`d to prevent `git add .` leaks. Exit codes distinct from clean-overwrite so scripts can detect. |
| Cross-filesystem tempdir rename (`EXDEV`) breaks atomicity | Preflight same-filesystem check (POSIX `Stat_t.Dev` or Windows `GetVolumeInformationW`); if mismatch, falls back to per-file copy. Non-atomic at the directory level in the cross-fs case but atomic per file, and the user-facing scaffold result is identical. Test exercises cross-fs path by pointing `os.TempDir()` at a tmpfs mount. |
| Default Anthropic model ID goes stale as Anthropic releases new models | Single-source-of-truth constant in `internal/scaffold/defaults.go` with a verification-timestamped doc comment. Unit 2's `integration`-tagged test asserts the constant resolves to a 2xx against the live Anthropic API. Bumping the model on a new release is one edit. |
| Mock adapter fixtures become a prompt-injection vector if users copy mock templates into real multi-agent pipelines | Size cap (256 KiB) + path containment check (no `..` after `filepath.Clean`, no absolute paths, no path resolving outside base) + broker envelope wraps mock output the same way it wraps real output. Documented explicitly in `docs/init.md` that mock is not for production use. |
| First-impression bug in `hello` or `summarize` template lands before CI catches it | Unit 6 Template CI + migration-swap test + goroutine-leak assertion + time-budget benchmark + live-API `integration`-tagged smoke test. Every success criterion has a corresponding CI gate. |
| User graduates scaffolded project to production without enabling auth | Unit 4 runtime auth guardrail: `overlord run` emits a loud `slog.Warn` at startup when `auth.enabled=false` AND bind is not loopback. Warning names the bind address and links to `docs/deployment.md#authentication`. Does not refuse to start (preserves local-dev usability). |
| `--non-interactive` demoting demo-failure to exit non-zero surprises interactive users | New feature — no existing users. Documented clearly in `docs/init.md`; the flag is opt-in and only affects CI-style invocations. |
| `{{`-containing fixture or schema silently breaks scaffold rendering | Only files with `.tmpl` suffix are rendered through `text/template`; everything else copies verbatim. Adversarial fixture tests are safe because they'd lack the `.tmpl` suffix. |

## Documentation / Operational Notes

- No rollout concerns: new CLI verb, no impact on running instances of Overlord.
- No monitoring changes: scaffold is a local CLI operation; no metrics needed.
- CHANGELOG entry: "Add `overlord init` scaffolder and first-class `mock` provider. Two templates: hello, summarize. See docs/init.md."
- v0.5.0 candidate feature — substantial new surface but fully backwards-compatible with existing configs.

## Sources & References

- **Origin document:** [docs/brainstorms/2026-04-16-overlord-init-requirements.md](../brainstorms/2026-04-16-overlord-init-requirements.md)
- **Upstream ideation:** [docs/ideation/2026-04-16-overlord-dx-ideation.md](../ideation/2026-04-16-overlord-dx-ideation.md) (idea #1 + idea #2; idea #7 split to a future brainstorm)
- **Related code:**
  - `cmd/overlord/main.go:84-127` (rootCmd + AddCommand pattern)
  - `cmd/overlord/exec.go:25-28, 106-113` (execExit* + execExitError)
  - `internal/agent/anthropic/anthropic.go` (adapter reference)
  - `internal/agent/registry/registry.go:32-95` (provider dispatch)
  - `internal/contract/registry.go` + `internal/contract/validator.go:45`
  - `internal/sanitize/envelope.go:15` (broker wraps all adapter output)
  - `internal/config/config.go:39-50` (symlink refusal pattern to extract)
  - `internal/dashboard/dashboard.go:11` (sole existing `//go:embed`)
- **CLAUDE.md:** architectural invariants (envelope pattern, schema versioning, "What NOT to do")
- **KNOWN_GAPS.md:** SEC4-007 (plugin path traversal — pattern we emulate for fixture paths), SEC-010 (envelope delimiter predictability — unaffected by this plan)
