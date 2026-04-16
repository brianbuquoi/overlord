---
date: 2026-04-16
topic: overlord-init-scaffolder
---

# `overlord init` — One-Command Onboarding

## Problem Frame

A new user evaluating Overlord today must hand-author four files (`infra.yaml`, `pipeline.yaml`, and two JSONSchemas), set an API key env var, and pick between three execution verbs (`exec` / `run` / `submit`) before seeing any output. The six examples in `config/examples/` are the unofficial onboarding path — they require copy-paste-edit and still don't produce output without a paid API key. The ideation scan identified this 4-file cold start as the single highest-leverage DX friction in the product.

The target outcome: once Overlord is installed, a user can run `overlord init summarize && cd summarize` and see real pipeline output in under 60 seconds, with zero API key, and from there has a trivially-commented path to their real provider.

This brainstorm bundles **two** ideation survivors into one feature:
- Idea #1 — `overlord init` scaffolder
- Idea #2 — built-in `mock` provider (default for scaffolded projects)

**Previously bundled but now split out:** idea #7 (`overlord.yaml` auto-discovery) is orthogonal — it affects every Overlord command, requires removing `MarkFlagRequired("config")` across 18 cobra subcommands, and has its own filesystem-safety surface (walk-up stopping rules, trust boundaries, `$HOME` footgun). It will be handled in a separate brainstorm so this feature can ship on its own schedule.

## First-60-Seconds Flow

```
┌─ user installs overlord ──────────────────────────────────────────────┐
│  $ go install github.com/brianbuquoi/overlord/cmd/overlord@latest     │
│  (60-second promise starts after this step completes)                 │
└───────────────────────────────────────────────────────────────────────┘
                                  │
                                  ▼
┌─ user picks a template ───────────────────────────────────────────────┐
│  $ overlord init summarize                                            │
│  (runs `overlord init` with no args prints available templates)       │
└───────────────────────────────────────────────────────────────────────┘
                                  │
                                  ▼
┌─ scaffold writes files ───────────────────────────────────────────────┐
│  ./summarize/                                                         │
│    overlord.yaml        # combined config                             │
│    schemas/*.json       # one per stage I/O                           │
│    fixtures/*.json      # mock-provider responses, keyed by stage id  │
│    sample_payload.json  # input used by auto-run                      │
│    .env.example         # placeholder-valued credential template      │
│    .gitignore           # excludes .env, .env.* (but keeps .env.example)
└───────────────────────────────────────────────────────────────────────┘
                                  │
                                  ▼
┌─ auto-run demo (unless --no-run; exits 0 even if demo fails) ─────────┐
│  ✓ Running sample payload through mock provider…                      │
│    intake → summarize-mock → validate → done                          │
│    {"summary": "..."}                                                 │
│  (on demo failure: warning printed, files persist, exit 0)            │
└───────────────────────────────────────────────────────────────────────┘
                                  │
                                  ▼
┌─ next steps printed ──────────────────────────────────────────────────┐
│  cd summarize                                                         │
│  overlord exec --config overlord.yaml --id summarize \                │
│    --payload '{"text": "..."}'                                        │
│  # To use a real LLM, uncomment the provider block in overlord.yaml   │
└───────────────────────────────────────────────────────────────────────┘
```

## Requirements

**Scaffolding**
- **R1.** `overlord init <template> [dir]` writes a self-contained project directory containing: `overlord.yaml` (combined config), `schemas/*.json` (input/output schema per stage), `fixtures/*.json` (one per mock-provider stage), `sample_payload.json` (the input payload used by the auto-run demo), `.env.example`, and `.gitignore`.
- **R2.** If `[dir]` is omitted, default to `./<template>/`. Fail if the target directory exists and is non-empty unless the user passes `--force`. The target directory path, any ancestor, or any file init would create must not be a symlink — aligning with the existing `internal/config` symlink-rejection policy. `--force` does not override symlink refusal.
- **R3.** If no template arg is provided, fail with a clear error listing available templates (`hello`, `summarize`). No interactive prompt in v1 — the set is small enough that the error message itself is the selection UI. An explicit `--non-interactive` flag is available for CI scripts that want deterministic behavior regardless of TTY.
- **R4.** Two templates ship in v1: `hello` (single-stage minimal) and `summarize` (2–3-stage linear chain). `code-review` (fan-out) and `classify` (conditional routing) are deferred to v2 because they exercise fixture-keying patterns (per-agent fan-out, multi-branch conditionals) that warrant their own design.

**Mock provider (first-class)**
- **R5.** A first-class `provider: mock` agent adapter lives in `internal/agent/mock/` as a peer to the existing provider adapters. It is registered in the agent registry and is usable in any pipeline, not only scaffolded ones. Concrete second consumer: Overlord's own test suite can replace per-adapter mock-HTTP-server setup with the mock provider, justifying its first-class status.
- **R6.** The mock returns fixture-keyed canned outputs (keyed by stage ID) that validate against the stage's registered `output_schema`. Fixture validation happens at startup, not at request time. If a fixture is missing, malformed, or fails schema validation, the mock fails loudly with the offending file path and the schema violation.
- **R7.** Mock output traverses the same `internal/sanitize` envelope path as real agent output before downstream stages consume it. No bypass — the mock is indistinguishable from a real provider downstream. Per-fixture size cap (e.g., 64 KiB) applies. Fixture file references are resolved relative to the config file's directory; absolute paths and `..` traversal are rejected.
- **R8.** Every scaffolded template uses `provider: mock` as its active agent so the auto-run demo works with zero API key.
- **R9.** Every scaffolded template also includes a commented-out real-provider block (defaulting to Anthropic + the latest Sonnet model). The commented block is delimited with a clear `# === real provider ===` / `# === end real provider ===` banner so users can uncomment cleanly. When uncommented, the mock agent block is commented out automatically or the YAML key collision is treated as a config error. A CI job exercises the commented-to-uncommented migration path against a recorded fixture of the real provider.
- **R10.** The recommended real-provider model is sourced from a single constant in the generator, so updating the default is one edit, not N edits across N templates. Documented in-scaffold comment: "Model set at scaffold time — see latest at <docs link>."

**Auto-run demo**
- **R11.** After writing files, `overlord init` immediately executes `sample_payload.json` through the scaffolded pipeline using the mock provider and prints the result. `--no-run` skips this step.
- **R12.** If the demo fails (validation, routing, mock execution, or any environmental cause), `init` prints a prominent warning block with the error detail but **exits 0**. Files persist on disk. The scaffold is considered successful if all files are written correctly; demo correctness is a separate question the user can re-run (`overlord exec ...`) or investigate. This decouples scaffold success from runtime success.
- **R13.** After a successful demo, `init` prints a short "next steps" block: one concrete `overlord exec` command (with `--config`, `--id`, and `--payload` flags already filled in for copy-paste), and a pointer to the commented real-provider block in `overlord.yaml`. No dashboard reference — `overlord run` is out of scope for `init`'s onboarding narrative.

**Credentials and safety**
- **R14.** Generated `.gitignore` excludes `.env`, `.env.*`, and `fixtures/*.secret.json`, but explicitly whitelists `.env.example` (so users commit the template).
- **R15.** Generated `.env.example` uses obviously-fake placeholder values (e.g., `ANTHROPIC_API_KEY=sk-REPLACE-ME`), not credential-shaped strings. The file header comment states that `init` does **not** auto-source `.env` — users must export vars themselves.
- **R16.** Generated `overlord.yaml` includes a commented-out `auth:` block (similar to the commented real-provider block) with an adjacent comment warning that auth must be enabled before exposing the API/WebSocket beyond localhost. This addresses the foreseeable "scaffold → real provider → production deploy without auth" failure mode.

**Non-goals (v1)**
- **R17.** No Redis or Postgres store scaffolding. Memory store only; users graduate to real stores via `docs/deployment.md`.
- **R18.** No plugin providers in shipped templates. Only built-in providers (`mock` and the commented Anthropic block) appear in generated configs.
- **R19.** No `overlord init --upgrade` or in-place migration of existing projects. v1 is strictly greenfield.
- **R20.** No auto-discovery (idea #7) — deferred to a separate brainstorm. Scaffolded `overlord.yaml` is still referenced by every command via explicit `--config` until auto-discovery ships.
- **R21.** No fan-out or conditional-routing templates in v1 (see R4). These require fixture-format decisions (per-agent keys for fan-out, multi-response support for conditionals) that belong in a later brainstorm.

## Success Criteria

- **Time to first output:** once Overlord is installed, a user can run `overlord init summarize && cd summarize` and see visible pipeline output in under 60 seconds, with no API key set. The 60-second promise is measured from `overlord init` invocation, not from `go install` (which depends on network, module proxy, and OS first-launch costs we can't control).
- **Determinism:** two consecutive runs of the demo produce byte-identical **pipeline result output** (the JSON shown as the final stage's output). Non-deterministic framing (timing, log lines, task IDs) is excluded from this claim.
- **Migration simplicity:** after uncommenting the real-provider block and setting the env var, the same pipeline runs against the real LLM with no other edits. CI exercises this migration path.
- **Self-validation:** every scaffolded `overlord.yaml` passes `overlord validate` on first write.
- **Template CI:** every shipped template passes `overlord validate` and a mock-mode `overlord exec` end-to-end on every PR that touches `internal/contract/`, `schemas/`, or the template generators.
- **CI-friendly:** `overlord init <template> <dir> --no-run --non-interactive` is idempotent, non-interactive, and suitable for scripted use.
- **Exit semantics:** `init` exits non-zero only for scaffold failures (file write error, symlink refusal, non-empty target without `--force`). Demo failures exit zero with a warning.

## Scope Boundaries

- Only built-in providers in v1: `mock` and the Anthropic block (commented).
- Only the `memory` store. Redis/Postgres scaffolding is deferred.
- No `auth:` block is active in generated configs — only commented with a warning.
- No existing-project upgrades; `init` refuses non-empty target dirs without `--force`, and always refuses symlinks.
- Mock provider scope is bounded to: fixture-keyed responses, startup validation, envelope-path parity. Out of scope: fuzzing, schema-driven synthesis without fixtures, record/replay, round-robin responses per stage.
- Does not re-litigate the three-verb CLI (`exec`/`run`/`submit`); the scaffolded next-steps text names one verb.
- Does not ship auto-discovery — every command still requires `--config` until that feature lands.
- Does not ship fan-out or conditional-routing templates.

## Key Decisions

- **Combined single-file config (`overlord.yaml`)**, not split infra/pipeline. Matches the "simplest to understand on day one" goal; advanced users can split later.
- **Auto-run the demo by default, but exit 0 on demo failure.** Scaffold success and runtime success are decoupled. Files always persist. First impression survives environmental quirks (port conflicts, AV scans, macOS Gatekeeper).
- **Mock provider is a first-class agent adapter** living in `internal/agent/mock/`. Justified not only by init but by the test-suite's mock-HTTP-server patterns, which it can replace.
- **Mock output flows through the envelope/sanitizer.** Fixtures are not a bypass; they're data going through the same wrapper a real agent would. Size-capped and path-constrained.
- **Two templates for v1: `hello` and `summarize`.** Covers single-stage and linear-chain mental models. Fan-out and conditional-routing templates defer because they need richer fixture keying that isn't designed yet.
- **Commented real-provider block with banner delimiters and a single-source-of-truth model name.** CI exercises the commented → uncommented path.
- **Commented `auth:` block with a warning comment.** Pre-empts the "graduated to real provider, still has auth off, now exposed" failure mode.
- **Generated `.gitignore` and `.env.example` have explicit content requirements** (R14, R15). Secret handling is taught correctly from file zero.
- **Non-interactive-by-default.** No TTY prompt; the error message for missing template arg is the selection UI. Keeps CI/non-CI behavior identical.
- **Symlinks refused at init write time**, mirroring the existing `internal/config` policy.
- **Auto-discovery split out to its own brainstorm.** Preserves scope; `--config` remains explicit until that feature lands.

## Dependencies / Assumptions

- Assumes the `internal/agent/` adapter interface accommodates a new `mock` provider without interface changes (existing `Execute` and `HealthCheck` signatures suffice). Mock `HealthCheck` returns nil when fixtures are loaded and schema-valid; returns an error if fixture validation failed at startup.
- Assumes the current YAML parser tolerates the combined single-file shape (README already documents it as supported).
- Assumes the `internal/sanitize` envelope can wrap synthetic agent output the same way it wraps real output.

## Outstanding Questions

### Resolve Before Planning
_None — product decisions are resolved._

### Deferred to Planning
- [Affects R1][Technical] Where do templates live? Candidates: embedded in the binary via `embed.FS`, checked in under `cmd/overlord/templates/`, or reused from `config/examples/`. Each has different testability and update semantics.
- [Affects R6][Technical] The mock adapter needs access to the compiled `contract.Registry` (or the registered schema paths) to validate fixtures at startup. The current `agent.registry.NewFromConfigWithPlugins(cfg, plugins, logger, ...)` factory does not take a registry — either the factory signature changes, a post-construction validation pass is added, or the mock loads schemas itself from the paths it receives. This is a cross-cutting architectural choice for the planner.
- [Affects R6][Technical] Exact fixture file format (JSON assumed) and whether a single fixture per stage ID is the only supported form for v1. Multi-response / round-robin fixtures are deferred to v2 with the fan-out and conditional-routing templates.
- [Affects R9][Technical] When the user uncomments the real-provider block, how is the mock agent block neutralized? Options: (a) both blocks are same-id so YAML parser rejects duplicate key (forces the user to comment mock out); (b) init uses a placeholder with two commented sections and the user picks one; (c) a single `# active:` marker above the active block.
- [Affects R2][Needs research] OS-specific file-write atomicity (Windows file locking, tempdir-rename-on-completion). Worth investigating whether `init` should write to a tempdir and rename, so a partial failure never leaves a half-written project.
- [Affects R5][Technical] Model-name single-source-of-truth: a Go constant, a template-generation-time variable, or a runtime lookup. Affects how "update the default Anthropic model" becomes one edit.

## Next Steps

→ `/ce:plan` for structured implementation planning.
