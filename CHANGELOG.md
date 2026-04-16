# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased] — `overlord init` scaffolder + first-class `mock` provider

### Added
- `overlord init <template> [dir]` scaffolds a runnable greenfield project
  (combined `overlord.yaml`, schemas, fixtures, sample payload,
  `.env.example`, `.gitignore`) and auto-runs a sample payload through the
  generated pipeline. Zero credentials on first run. See
  [docs/init.md](docs/init.md).
- Two embedded templates ship in v1: `hello` (single-stage) and
  `summarize` (2-stage linear chain). Templates live under
  `internal/scaffold/templates/` and are embedded via
  `//go:embed all:templates`.
- First-party `mock` provider adapter (`internal/agent/mock/`). Loads
  fixture files keyed by stage ID and returns their contents verbatim
  from `Execute`. Fixtures are validated against the stage's
  `output_schema` at agent-construction time (path containment check,
  256 KiB size cap, schema validation). No broker-side hook; every code
  path that constructs an agent gets validation automatically.
- `init` flags: `--force`, `--overwrite`, `--no-run`,
  `--non-interactive`. Exit codes 0/1/2/3/4/130 with the matrix
  documented in `docs/init.md`.
- `--overwrite` backs up colliding files to
  `<name>.overlord-init-bak.<YYYYMMDDHHMMSS>[-<rand>]`. Repeated
  `--force --overwrite` runs never silently clobber a prior backup.
- Atomic scaffold: writes to a sibling tempdir + `os.Rename` on the
  happy path; cross-filesystem fallback copies per file.
- Symlink refusal on the target and on init-created files
  (`O_NOFOLLOW` on POSIX; Windows uses open + fstat + inode compare).

### Changed
- `registry.NewFromConfigWithPlugins` signature widened to accept
  `*contract.Registry`, `basePath string`, and
  `[]config.Stage` (filtered to bindings referencing the agent being
  constructed). Consumed by the mock adapter for inline fixture
  validation; ignored by every other built-in provider. All in-repo
  callers updated.
- `overlord run` now emits a startup `WARN` when `auth.enabled=false`
  AND the HTTP bind address is not loopback
  (`127.0.0.0/8`, `::1`, `localhost`). Warn-only — does not refuse to
  start. Runtime-level mitigation for the commented `auth:` block in
  scaffolded projects.

### Documentation
- `docs/init.md` covers the template catalog, flag reference, exit
  codes, file tree, first-run flow, mock-to-real migration, and the
  production graduation path (including the runtime auth guardrail).
- `README.md` quickstart rewritten to lead with `overlord init` (zero
  API key on first run). The manual-authoring path remains in the
  Configuration reference section for users who need split configs or
  custom templates.
- `CLAUDE.md` updated with the `init` CLI surface, `mock` provider,
  `internal/scaffold/` package, widened factory signature, and
  scaffolded-project runtime auth guardrail.
- `docs/deployment.md` cross-references `docs/init.md` for the
  memory → Postgres/Redis and commented-auth → enabled-auth graduation
  path.

### Testing
- Template CI (`internal/scaffold/ci_test.go`) runs every shipped
  template through `config.Load`, a mock-mode exec, the mock-to-real
  migration swap, and `goleak.VerifyNone`. Assertions cover
  `.gitignore` rules, `.env.example` placeholder shape, and
  banner-delimited real-provider block presence.
- Time-budget benchmark (`internal/scaffold/bench_test.go`) asserts
  `overlord init hello` + demo completes under 10 seconds on CI
  hardware.

## [0.4.0] — Security Hardening + Plugin System + Exec Command

### Security
- Removed prompt and payload preview logging from all provider adapters (prevented credential leakage via debug logs)
- Replaced raw `err.Error()` HTTP responses with stable opaque error messages; full errors logged server-side with request ID
- `/v1/health` no longer leaks provider or environment failure details
- WebSocket auth: API key removed from query string; replaced with short-lived single-use session tokens via `POST /v1/ws-token`
- IPv6 brute-force tracking normalized to /64 prefix; evicts at 90% capacity rather than failing open
- `--payload @file` rejects symlinks and non-regular files on both `exec` and `submit`
- Plugin environment isolation: plugins receive only explicitly listed env vars
- Sanitizer extended to 6 of 8 prompt injection vector classes with active detection
- Output validation layer: model responses checked for instruction-like patterns as defense-in-depth

### Store
- Redis `UpdateTask` made atomic via Lua script; eliminates lost-update races
- Per-state secondary index (`tasks:state:{STATE}`) for O(log N) state-filtered listings
- Two-dimensional state×pipeline index (`tasks:state:{STATE}:pipeline:{PIPELINE_ID}`) for O(log N) pipeline-scoped queries
- Postgres `ClaimForReplay` and `RollbackReplayClaim` collapsed into single-CTE statements eliminating TOCTOU window
- Postgres store brought to full contract parity: `RoutedToDeadLetter`, `CrossStageTransitions`, `IncludeDiscarded` correctly applied
- Store conformance suite covers `ClaimForReplay`, `RollbackReplayClaim`, concurrent contention across all backends
- `internal/store/mock` moved to `internal/testutil/storemock` with test-only package doc

### Replay State Machine
- `REPLAY_PENDING` (transitional) and `REPLAYED` (terminal audit state) introduced
- `ClaimForReplay` atomically transitions task to `REPLAY_PENDING`, preventing duplicate replay under concurrent requests
- `RollbackReplayClaim` atomically restores `FAILED+RoutedToDeadLetter=true` if Submit fails after claim
- Handler rolls back claim on Submit failure; logs Error with task_id and recovery command if rollback also fails
- `POST /v1/tasks/{id}/recover` endpoint for operator recovery of tasks stranded in `REPLAY_PENDING`
- `overlord dead-letter recover` CLI command added

### Dead-Letter Operations
- `replay-all` and `discard-all` paginate through full dead-letter set (was capped at 1000 tasks)
- `failedIDs` deduplication prevents rolled-back tasks from inflating failure counts across pages
- `failed` count in bulk responses reflects distinct failing task IDs; field always present (no `omitempty`)
- `processed` field standardized on bulk responses (renamed from `count`)
- Per-task failure logging with task_id, pipeline_id, error

### External Plugin System
- New provider `plugin`: runs external binaries as persistent subprocesses via stdin/stdout JSON-RPC 2.0
- YAML manifest: `name`, `version`, `binary`, `on_failure`, `rpc_timeout`, `shutdown_timeout`, `max_restarts`, `env`
- Environment isolation: plugins receive only explicitly listed env vars
- Automatic restart with configurable `max_restarts`; agent marked unhealthy at limit
- Hot-reload: in-flight RPCs complete before subprocess stopped; `Drainer` interface, 10s drain grace period
- `Agent.Stop()` sends SIGTERM before closing stdin; SIGKILL after `shutdown_timeout`
- Plugin binary validated at startup; missing/non-executable binary produces exit 3 (config error)
- `docs/plugin-security.md` documents isolation model, env isolation, shutdown sequence, capacity planning

### OpenAI Responses API Adapter
- New provider `openai-responses` targeting `POST /v1/responses` (enables `codex-mini-latest`)
- Coexists with existing `openai` Chat Completions provider
- Parse-then-fallback: non-JSON output wrapped as `{"text": "<raw>"}` rather than failing

### `overlord exec` Command
- Runs a single task to completion and exits — no HTTP server, no port binding
- Progress to stderr, result to stdout; `--output json` for machine-readable piping
- Exit codes: 0=done, 1=failed/dead-lettered, 2=timeout, 3=config error
- `--payload @filepath` reads payload from file
- Graceful broker drain and plugin subprocess cleanup on exit and SIGINT

### Infra/Pipeline Config Separation
- `--pipeline` flag accepts standalone pipeline definition YAML on both `exec` and `submit`
- `--id` flag for pipeline ID (standardized across both commands)
- Relative schema paths in pipeline files resolve from the pipeline file's own directory
- Example configs in `config/examples/`

### API
- `POST /v1/ws-token`: short-lived single-use WebSocket session token (64-char hex)
- `POST /v1/tasks/{id}/recover`: transitions `REPLAY_PENDING` back to `FAILED+RoutedToDeadLetter=true`
- New task states in `GET /v1/tasks/{id}`: `REPLAY_PENDING`, `REPLAYED`

### Observability
- ws-token logs include `client_ip` and `request_id`; token values never logged
- Plugin restart events logged at Warn with agent_id, restart_count, max_restarts
- Double-failure Error log for stranded `REPLAY_PENDING` includes task_id and recovery command

### Testing
- `go test -race -count=3` passes across all packages
- Fault-injection tests for replay-all/discard-all via store mock
- Plugin tests: crash/restart, env isolation, RPC timeout, SIGTERM sentinel
- Comprehensive sweep: Redis index consistency, broker retry policy, schema validation, API error isolation, dead-letter full lifecycle

### Documentation
- README rewritten to lead with `overlord exec` as primary entry point
- `docs/exec.md`: exec command, config split pattern, exit codes, @filepath syntax
- `docs/plugin-security.md`: full isolation model, capacity planning, shutdown sequence
- `docs/api.md`: all new endpoints documented

### Known Limitations
- Redis 2D index not backfilled for pre-v0.4.0 tasks (KG-005); backfill needed for live upgrades
- Plugin seccomp deferred; bwrap/landlock-exec recommended for full Linux syscall isolation
- Prompt injection classes 7 and 8 rely on model alignment
- Unchanged plugin agents restarted on hot-reload (subprocess reuse optimization deferred)
- Hot-reload drain grace period hardcoded at 10s
- Multi-tenant authorization boundaries not yet implemented

## [0.3.0] - 2026-04-11

### Changed
- Project renamed from Orcastrator to Overlord
- Go module path changed from `github.com/brianbuquoi/orcastrator`
  to `github.com/brianbuquoi/overlord`
- CLI binary renamed from `orcastrator` to `overlord`
- Environment variable prefix changed from `ORCASTRATOR_` to `OVERLORD_`

### Breaking Changes
- Redis key prefix changed from `orcastrator:` to `overlord:`
  Existing Redis deployments must flush keys or migrate data.
- Postgres table renamed from `orcastrator_tasks` to `overlord_tasks`
  Run migrations/002_rename_table.sql to migrate existing data.
- Environment variables renamed: `ORCASTRATOR_PORT` → `OVERLORD_PORT`,
  `ORCASTRATOR_API_KEY` → `OVERLORD_API_KEY`

### Migration
For existing deployments upgrading from v0.2.x:
1. Stop all Orcastrator instances
2. Run migrations/002_rename_table.sql against your Postgres database
3. Flush or migrate Redis keys (see docs/migration.md)
4. Update environment variable names in your deployment config
5. Update any scripts using the `orcastrator` binary to use `overlord`

## [0.2.0] - 2026-04-09

### Added
- Fan-out stage support: execute multiple agents in parallel with
  gather and race modes, configurable require policies (all/any/majority)
- API key authentication with Bearer tokens, named keys, and three
  scope levels (read/write/admin)
- Brute force protection with per-IP failure tracking and sliding
  window cleanup
- Aggregate schema validation for fan-out stage outputs
- `overlord_fanout_agent_results_total` and
  `overlord_fanout_require_policy_failures_total` metrics
- 72-byte API key length validation with clear startup error

### Security
- SEC3-001: RecordSuccess resets brute force window — RecordSuccess is
  now a no-op; failures expire via sliding window
- SEC3-002: Non-uniform 401 responses — all auth failures now return
  identical response body
- SEC3-003: 403 response scope information leak — forbidden responses
  no longer reveal scope details
- Auth timing oracle fixed: full key iteration prevents response
  time from revealing key count or position
- BruteForceTracker memory exhaustion fixed: cleanup goroutine
  sweeps expired entries every 5 minutes
- API key zeroing improved: unsafe used to zero original backing
  memory, not just the copied slice

## [0.2.3] - 2026-04-11

### Fixed
- Schema version mismatch now correctly detected at routing time. Previously
  the check ran after task schema fields were already updated to the next
  stage's versions, so IsCompatible always saw matching versions and mismatches
  were never caught. The check now runs in routeSuccess before UpdateTask,
  comparing the current stage's output version against the next stage's
  expected input version. Adds version_mismatch metadata with full from/to
  stage and schema version details.
- Dead version checks removed from worker loop — they were structurally
  incapable of detecting mismatches and created false confidence.

## [0.2.2] - 2026-04-11

### Fixed
- Correct Go module path from `github.com/overlord/overlord` to
  `github.com/brianbuquoi/overlord` — `go install` was failing with
  a module path conflict
- All LLM adapters (Anthropic, OpenAI, Gemini, Ollama) now parse model
  response text as JSON before setting TaskResult.Output. Previously
  adapters passed the raw text string directly, causing every task to
  fail contract validation with "got string, want object". Adapters also
  strip markdown code fences before parsing. JSON parse errors are
  retryable so the retry policy fires on occasional malformed responses.
- Broker no longer overwrites task.Payload with the envelope prompt
  before calling agent.Execute. Agents were receiving their own system
  prompt as the user message with no input data, causing models to
  respond "I'm ready to review code. Please provide the JSON object".
  Task struct now carries a separate Prompt field for the envelope result.

### Changed
- tokenBucket now accepts an optional clock function (default time.Now)
  for deterministic testing, eliminating timing-dependent test flakiness

## [0.2.1] - 2026-04-10
### Added
- Conditional routing: route tasks to different stages based on field
  values in agent output, supporting ==, !=, >, >=, <, <=, and contains
  operators
- Pipeline dashboard: single-page web UI served by Overlord showing
  live pipeline topology, per-stage queue depths, task event feed, and
  agent health status
- Retry budgets: pipeline-level and agent-level caps on total retry
  attempts within a sliding time window
- Dead letter queue management: inspect, replay, and discard
  dead-lettered tasks via CLI and API
- Plugin system: load custom LLM provider adapters as Go shared
  libraries without forking Overlord
- Cycle detection in pipeline config validation — circular routing
  references are now rejected at startup
- Runtime max_stage_transitions safeguard (default: 50) as
  defence-in-depth against routing loops

### Security
- SEC4-001: Security headers middleware — X-Content-Type-Options,
  X-Frame-Options, and Referrer-Policy now set on all responses
- SEC4-002: Content Security Policy on dashboard — restricts scripts
  to self and cdnjs.cloudflare.com only
- Full SEC4 audit completed — 8 additional findings tracked in
  KNOWN_GAPS.md

## [0.1.0] - 2026-04-09

### Added

- YAML-driven pipeline configuration with hot-reload on SIGHUP
- Broker engine with typed stage routing, retry policies (exponential, linear, fixed), and goroutine pool management
- LLM provider adapters: Anthropic, OpenAI, Google Gemini, Ollama, GitHub Copilot (stub)
- Schema registry with first-class versioning in YAML — major version mismatches are hard errors
- JSONSchema-based I/O contract validation on every stage input and output
- Prompt injection sanitizer using the envelope pattern for safe inter-agent data passing
- Store backends: in-memory (dev/test), Redis (sorted set indexes), Postgres (FOR UPDATE SKIP LOCKED)
- Multi-instance deployment support via Postgres with atomic task dequeuing
- HTTP REST API for task submission, status, cancellation, and pipeline inspection
- WebSocket endpoint for real-time task event streaming
- CLI commands: run, submit, status, cancel, validate, health, pipelines (list/describe), migrate
- Schema migration framework with dry-run support and batch processing
- OpenTelemetry tracing with OTLP and stdout exporters
- Prometheus metrics endpoint
- Per-IP rate limiting on the HTTP API
- Shell completion for bash, zsh, fish, and PowerShell
- Code review example pipeline with sample input and migration

### Fixed

- Broker.Reload data race — added sync.RWMutex around pipeline/stage/agent maps
- Hot-reload now starts workers for newly added stages and drains removed stages
- Redis ListTasks replaced O(N) SCAN with sorted set index for O(log N + page) queries
- Postgres UpdateTask wrapped in SELECT ... FOR UPDATE transaction for atomicity
- WebSocket hub shutdown race guarded with sync.Once
- Flaky timing-sensitive tests replaced with deterministic synchronization
- staticcheck ST1005 in copilot stub error strings

### Security

Pre-release security audit findings (two rounds) — all Critical and High findings resolved:

- **SEC-007**: Pipeline/stage/agent IDs now validated against safe character set (`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)
- **SEC-008**: ListTasks limit parameter capped at 1000 with strict validation
- **SEC-009**: TLS enforcement for Anthropic, OpenAI, and Gemini adapters; Ollama allows HTTP only for localhost
- **SEC-011**: Upgraded from Go 1.25.0 to 1.25.9, resolving 17 stdlib vulnerabilities
- **SEC2-001**: OTLP exporter defaults to TLS; plaintext requires explicit opt-in via `otlp_insecure`
- **SEC2-002**: /metrics endpoint exempt from rate limiter to prevent Prometheus scrape blocking
- **SEC2-003**: cancel command TOCTOU race documented (open — requires Store interface change)
- **SEC2-004**: trace_id and span_id added to reserved metadata keys
- **SEC2-005**: Migration against live broker documented — requires quiesced pipeline (open)
- **SEC2-006**: docker-compose sslmode=disable annotated with network safety guidance
- **SEC2-NEW-001**: Config parser no longer leaks file content in YAML error messages; symlinks rejected
- **SEC2-NEW-002**: Metrics endpoint on shared port documented in deployment hardening guidance (informational)
