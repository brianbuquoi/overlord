# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [0.2.0] - 2026-04-09

### Added
- Fan-out stage support: execute multiple agents in parallel with
  gather and race modes, configurable require policies (all/any/majority)
- API key authentication with Bearer tokens, named keys, and three
  scope levels (read/write/admin)
- Brute force protection with per-IP failure tracking and sliding
  window cleanup
- Aggregate schema validation for fan-out stage outputs
- `orcastrator_fanout_agent_results_total` and
  `orcastrator_fanout_require_policy_failures_total` metrics
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

## [0.2.2] - 2026-04-11

### Fixed
- Correct Go module path from `github.com/orcastrator/orcastrator` to
  `github.com/brianbuquoi/orcastrator` — `go install` was failing with
  a module path conflict
- All LLM adapters now parse model response text as JSON before setting
  TaskResult.Output. Previously adapters passed the raw text string
  directly, causing every task to fail contract validation with
  "got string, want object". Adapters also strip markdown code fences
  before parsing.
- Rate limiter tests hardened with clock injection — replaced
  timing-dependent burst exhaustion tests with frozen-clock approach,
  eliminating ~1/20 flake rate on loaded CI runners

### Changed
- tokenBucket now accepts an optional clock function (default time.Now)
  for deterministic testing

## [0.2.1] - 2026-04-10
### Added
- Conditional routing: route tasks to different stages based on field
  values in agent output, supporting ==, !=, >, >=, <, <=, and contains
  operators
- Pipeline dashboard: single-page web UI served by Orcastrator showing
  live pipeline topology, per-stage queue depths, task event feed, and
  agent health status
- Retry budgets: pipeline-level and agent-level caps on total retry
  attempts within a sliding time window
- Dead letter queue management: inspect, replay, and discard
  dead-lettered tasks via CLI and API
- Plugin system: load custom LLM provider adapters as Go shared
  libraries without forking Orcastrator
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
