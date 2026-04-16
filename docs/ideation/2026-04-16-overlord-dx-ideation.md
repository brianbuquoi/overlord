---
date: 2026-04-16
topic: overlord-end-user-dx
focus: end-user DX, onboarding friction, CLI ergonomics, "first 15 minutes" experience, workflow intuitiveness
---

# Ideation: Overlord End-User DX

## Codebase Context

**Project shape.** Go CLI (cobra) orchestration engine for AI agent pipelines. YAML is the single source of truth. Pipelines = DAG of stages; stages bind to agents (LLM adapters); the broker routes tasks with typed I/O contracts (JSONSchema) and an anti-injection envelope. Providers: Anthropic, OpenAI, Gemini, Ollama, Copilot (stub), plus a CGO plugin SDK. Entry points: `exec` (one-shot), `run` (server + dashboard + broker), `submit`. Embedded dashboard at `/dashboard`, `/metrics`, WebSocket event stream. Version 0.4.0.

**Pain points found in the scan.**
- 4-file cold start (infra.yaml + pipeline.yaml + 2 JSONSchemas) before anything runs; no scaffolder.
- README quick-start uses `{"type":"object"}` placeholder schemas, signaling that JSONSchema authoring is friction.
- Split config (infra + pipeline) has merge semantics, relative-path resolution, and agent-ref checks that are easy to trip over.
- CLI inconsistency: README references `orcastrator recover` (typo of `overlord recover`); `exec` vs `run` vs `submit --wait` boundaries aren't obvious up front.
- Quick-start uses `memory` store, which silently breaks dead-letter / multi-instance features.
- No Docker image, no prebuilt binaries — only `go install`.
- Plugin system is CGO + Linux/macOS only — Windows users have no extension path.
- Conditional-routing DSL (`output.foo == "bar"`) has no preview/explain tool.
- No fake/dry-run provider — users burn API credit to learn the tool.
- Schema version mismatches are intentional hard errors but surface only at runtime.
- `docs/solutions/` knowledge base does not exist yet.

**Past learnings.** None — no institutional knowledge base yet.

## Ranked Ideas

### 1. `overlord init` — interactive scaffolder
**Description:** Single command that writes a runnable project (infra.yaml + pipeline.yaml + schemas/*.json + `.env.example`) from a named template. Templates: `summarize`, `code-review`, `classify`, `fan-out`, `rag-lite`. Non-interactive flags for CI. Pairs naturally with #2 (mock provider default) and #7 (project context).
**Rationale:** Collapses the 4-file cold start into one command. Kills the blank-page problem and becomes the canonical entry point for every future tutorial, blog post, or demo.
**Downsides:** Templates need CI to keep them from going stale. New surface to maintain (offset by bundled example coverage).
**Confidence:** 95%
**Complexity:** Low
**Status:** Explored (brainstormed 2026-04-16)

### 2. Built-in `mock` / `fake` provider
**Description:** First-class deterministic provider returning canned or fixture-keyed outputs, requiring no network and no API key. Supports `provider: mock` in YAML and fixture files keyed by stage ID. Default provider for `overlord init` so new users prove their DAG topology before touching a real API.
**Rationale:** Lets users learn routing, retry, fan-out semantics, envelope wrapping, and contract validation without burning tokens. Foundation for `overlord test`-style CI use cases and the `overlord dev` loop. Every tutorial and screencast rides on this.
**Downsides:** Must stay schema-aware so canned outputs validate. Minor risk users ship it to prod — mitigate with a startup warning.
**Confidence:** 90%
**Complexity:** Low
**Status:** Unexplored

### 3. Schema inference from examples (`overlord schema infer`)
**Description:** One-shot generator: `overlord schema infer <sample.json> --name foo --version v1` writes a draft JSONSchema into `schemas/` and adds it to the registry. Deliberately **not** inline-YAML inference — that would fight the "schema_registry is the canonical versioned source of truth" principle. A separate `overlord schema promote` can freeze an inferred draft into a stricter versioned file for PR review.
**Rationale:** JSONSchema authoring is the steepest cliff between "I read the README" and "I built something." Inferring from examples matches how users actually think about data while preserving the strict-contract design axis.
**Downsides:** Inferred drafts need manual refinement for stricter constraints; users may accept defaults that are too loose.
**Confidence:** 85%
**Complexity:** Low
**Status:** Unexplored

### 4. `overlord doctor` — preflight diagnostic
**Description:** One command that chains everything users should run before `exec`/`run`: YAML parse, schema-registry resolution, JSONSchema compile, agent-ref integrity, env-var presence for every bound agent, store reachability (Redis/Postgres PING, Ollama endpoint probe), per-agent `HealthCheck`, plugin file presence + CGO/platform compatibility, bcrypt key-length check. Output is a traffic-light checklist with fix-it hints ("set `ANTHROPIC_API_KEY`", "`redis-cli -u $REDIS_URL PING` failed — falling back to memory?").
**Rationale:** Moves the long tail of opaque runtime failures to one actionable up-front surface. Highest-leverage diagnostic investment for a config-heavy tool. Reuses existing `validate` + `health` code paths — bounded scope.
**Downsides:** Maintenance is coupled to every new provider/store; must not drift from actual startup behavior.
**Confidence:** 90%
**Complexity:** Low
**Status:** Unexplored

### 5. Compiler-grade error messages (scoped)
**Description:** Scope limited to the two highest-impact surfaces: (a) YAML parse errors and (b) schema_registry resolution / contract violations. Every error gets `file:line`, a 3-line context snippet with a caret, and a one-line suggested fix ("did you mean to register schema `foo` at version `v1`?"). Ship iteratively — do not try to boil the ocean. Use the existing JSONSchema AST to generate hints. Also the vehicle for fixing the `orcastrator recover` typo and standardizing verb order across commands.
**Rationale:** Most onboarding time is spent decoding cryptic errors. Compiler-quality diagnostics compound daily and serve every user tier from first-timer to operator.
**Downsides:** Scope creep risk — the "every error everywhere" version is unbounded. Requires explicit discipline to keep the shrunk scope.
**Confidence:** 85%
**Complexity:** Medium
**Status:** Unexplored

### 6. `overlord explain` / `preview`
**Description:** Two concrete capabilities behind one verb: (a) `overlord explain envelope --stage review --payload @sample.json` renders the exact envelope string an agent would receive, with sanitizer redactions highlighted; (b) `overlord explain route --stage judge --payload @sample.json` evaluates the conditional-routing DSL against the sample and shows which branch wins. Not a live debugger — a static, deterministic preview reusing broker code paths.
**Rationale:** The envelope pattern and conditional-routing DSL are Overlord's most powerful-but-invisible features. Today their effects only surface in production logs. `explain` makes them pokeable — great for PR review and authoring.
**Downsides:** Requires careful code reuse to avoid drift between preview and real execution. Two distinct sub-verbs in one command family.
**Confidence:** 80%
**Complexity:** Medium
**Status:** Unexplored

### 7. Project context — `overlord.yaml` auto-discovery
**Description:** Walk up from cwd to find an `overlord.yaml` (or `.overlord/`) pointer file that names the infra and pipeline paths — the way git and cargo do. Commands resolve `--config`/`--pipeline` from it automatically; explicit flags still override. `OVERLORD_PROJECT` env var + `overlord use <pipeline>` to switch the active pipeline per shell session. Deliberately a pure pointer file, not a merge source, to avoid a second config layer.
**Rationale:** Every command today re-specifies two paths. Auto-discovery compounds across every daily interaction and pairs elegantly with `overlord init` (init drops the pointer file; subsequent commands find it).
**Downsides:** Precedence rules need clear documentation. Pragmatism critic worried about "second config layer" — mitigated by keeping it strictly a pointer, not a merge source.
**Confidence:** 70%
**Complexity:** Low
**Status:** Unexplored

## Rejection Summary

| # | Idea | Reason Rejected |
|---|------|-----------------|
| R1 | Unified entrypoint (fold `exec`/`run`/`submit` into one verb) | Silent mode-switching is a footgun; the three verbs are intentionally distinct execution models and operators rely on the distinction |
| R2 | `overlord dev` full playground | Shrinks to a thin file-watcher that sends SIGHUP on change — not a headline item, absorbable into #1 or shipped as a 50-line utility |
| R3 | TUI / REPL (bubbletea-style) | Duplicates the embedded dashboard in a worse surface; second frontend is unaffordable at v0.4.0 |
| R4 | Docker images + prebuilt binaries + Homebrew tap | Distribution concern, not DX — belongs in a release-ops track; GoReleaser is cheap but not an ideation-tier item here |
| R5 | Pipelines-as-code Go/TS SDK | Contradicts "YAML is the single source of truth, zero runtime config" — a stated core principle |
| R6 | Cost estimator + budget caps | Real operator feature but second-15-min, not first-15-min; bundled pricing tables are a perpetual PR treadmill |
| R7 | Golden-output test framework (`overlord test`) | Durable but for mature pipelines, not the onboarding flow; revisit after #2 ships |
| R8 | Dashboard read/write YAML editing | Browser-edit of the source of truth invites silent unreviewed changes and a CMS-grade auth/authz/undo threat model |
| R9 | WASM plugin runtime (wazero) | Second plugin runtime to maintain while SEC4-007 plugin-path traversal is still open; wait for CGO plugin story to stabilize |
| R10 | Pipeline/plugin registry (central index) | Premature platform at v0.4.0 with no adopters yet — nobody is asking `overlord install` because there is nothing to install |
| R11 | Zero-config directory mode (one stage per .md file) | Second YAML dialect competing with the canonical one; fights "schema_registry is canonical, versions are explicit" |
| R12 | Schema → Go struct codegen (`overlord schema gen-go`) | Niche audience (Go plugin authors, already niche given CGO-only); worth a tiny utility, not a headline feature |
| R13 | Pipeline trace export (self-contained HTML) | Shrinks to OTLP JSON export against existing OpenTelemetry tracing; HTML Gantt viewer is a mini-frontend project |
| R14 | `overlord fix` / automated DLQ lifecycle | Blocked by SEC4-008 replay TOCTOU race; ship `--auto` transient-retry after that closes |
| R15 | `overlord tail` task stream | Useful but a plain WebSocket CLI; absorbed into `explain` / existing dashboard |
| R16 | [Synth A] "Zero to green" bundle (init + infer + mock) | Not a feature — a release-theme framing of ideas 1+2+3 |
| R17 | [Synth C] Unified diagnostics framework (doctor + error messages) | Not a feature — already captured by ideas 4+5; framework framing risks over-engineering |

## Session Log

- 2026-04-16: Initial ideation — 40 raw ideas generated across 5 frames (user pain, unmet capability, inversion/removal, assumption-breaking, leverage/compounding); 22 unique after dedupe + 2 synthesis combinations; 7 survived adversarial filter.
- 2026-04-16: Idea #1 (`overlord init` scaffolder) handed off to `ce:brainstorm` for requirements definition.
- 2026-04-16: Brainstorm for #1 initially bundled #1 + #2 + #7 per user choice, but document-review surfaced a P0 bundling concern. #7 (`overlord.yaml` auto-discovery) was unbundled during refinement and is now an Unexplored survivor awaiting its own brainstorm — it requires removing `MarkFlagRequired("config")` across 18 cobra subcommands and has its own filesystem-safety surface (walk-up stopping rules, trust boundaries, `$HOME` footgun). The `overlord init` brainstorm scope is now #1 + #2 only. Additionally, template count reduced from 4 to 2 (`hello`, `summarize`) because `code-review` fan-out and `classify` conditional routing break stage-ID fixture keying; they are deferred to v2.
