# Overlord — Known Gaps & Deferred Issues

These items were identified during integration testing and security audits.
Each entry includes the severity, location, and recommended fix approach.

---

## Open Issues

### SEC-015: (Resolved) Chain-adapter envelope bypass via `{{steps.<id>.output}}` substitution
**Location:** `internal/chain/adapter.go`, `internal/sanitize/envelope.go`
**Severity:** High (resolved)
**Description:** The chain step adapter rendered
`{{prev}}` / `{{steps.<id>.output}}` / `{{input}}` placeholders into
`task.Prompt` *after* the broker's sanitizer envelope had already wrapped
the prior-stage payload. Raw prior-step output was re-injected into the
`Your task:` section outside the envelope, letting a malicious upstream
output instruct the downstream model directly. Workflow mode inherited the
same bypass via `{{prev}}` auto-rewrite.
**Resolution:** Chain adapter now pre-sanitizes every substituted value
and wraps it inline via the new `sanitize.WrapInline`, so in-prompt
substitutions receive the same `[SYSTEM CONTEXT ...]` protection the
broker's outer envelope applies. Raw step output is still stored on task
metadata for operator inspection. Adversarial regression tests live in
`internal/chain/adapter_test.go`.

### SEC-016: (Resolved) Broker swallowed store persistence errors + Postgres requeue duplicate-key
**Location:** `internal/broker/broker.go`, `internal/store/postgres/postgres.go`
**Severity:** High (resolved)
**Description:** Broker state-transition helpers (`retryTask`,
`routeSuccess`, `failTask`, `transition`, `mergeMetadata`) ignored
store-write errors with `_ = b.store.*`, then emitted terminal
events, incremented success metrics, and mutated in-memory task
state regardless. A task could be reported `DONE` or re-routed while
the durable record lagged or was never updated at all.

Compounding this: the Postgres `EnqueueTask` was a plain `INSERT`,
but the broker called `EnqueueTask` for stage-to-stage routing and
retry of *existing* task IDs — deterministically producing
duplicate-key violations that the above error-swallowing then hid.

**Resolution:** Split the store contract: `EnqueueTask` keeps
insert-only semantics for net-new tasks; new `RequeueTask(ctx,
taskID, stageID, update)` atomically applies the field update and
places the task back onto the destination stage's queue (Memory:
single-mutex update + append; Redis: one Lua script; Postgres:
single `UPDATE`). Broker state-transition helpers now return on
store errors, log at error level, and increment the new
`overlord_broker_store_errors_total{operation, pipeline_id,
stage_id}` counter rather than silently continuing. Terminal
events and success metrics only fire after persistence succeeds.

### SEC-010: Envelope delimiters are predictable
**Location:** `internal/sanitize/envelope.go`
**Severity:** Medium
**Description:** Envelope delimiters (`[SYSTEM CONTEXT ...]`, `[END SYSTEM CONTEXT]`)
are static, human-readable strings. While the sanitizer detects these in agent output,
a per-task random nonce in the delimiter would provide stronger defense-in-depth.
**Recommendation:** Use a cryptographically random nonce in delimiters per-task.

## Sanitizer Coverage Gaps

### SAN-001: Sanitizer active detection covers 6 of 8 known vector classes
**Severity:** Low
**Location:** `internal/sanitize/sanitizer.go`, `internal/sanitize/output.go`
**Detail:** Sanitizer covers 6 of 8 known prompt injection vector classes
with active detection:
1. Instruction override (`instruction_override`)
2. Delimiter injection, including jailbreak bracket/tag preambles like
   `[SYSTEM]`, `[new instructions]`, `<instructions>`, `SYSTEM:` line-leads
   (`delimiter_injection`)
3. Role-play / persona hijacking, including "pretend to be", "roleplay as",
   "your true purpose is" (`role_hijack`)
4. (Folded into class 2 — jailbreak preambles now matched by the delimiter
   detector)
5. Data exfiltration probes (`exfiltration_probe`)
6. Encoding and obfuscation: base64-wrapped injection payloads
   (`encoded_payload`), Unicode homoglyph substitution
   (`homoglyph_substitution`), and zero-width/format-control characters
   (`zero_width`)

Classes 7 (multi-turn context manipulation) and 8 (indirect injection via
retrieved content in external tools) rely on model alignment — active
detection for these requires semantic analysis beyond pattern matching
and is a field-wide open problem.

**Status:** Covered to the extent reasonable with pattern matching.
Remaining classes are tracked as informational (SAN-003).

### SAN-004: Output validation layer may produce false positives
**Severity:** Informational
**Location:** `internal/sanitize/output.go`
**Detail:** `ValidateOutput` detects instruction-like patterns in model
responses (`output_system_preamble`, `output_instruction_echo`,
`output_redirect_attempt`) as a defense-in-depth check that complements
the input sanitizer. It cannot guarantee detection of all successful
injections, and the false positive rate on legitimate outputs is
non-zero for some output schemas — a summarizer asked to quote a
document that contains the literal string "disregard the previous" will
flag. Warnings are attached to task metadata under
`sanitizer_output_warnings`; the broker continues processing on warning,
mirroring the input-sanitizer policy. `TestValidateOutput_FalsePositiveRate`
guards against the detectors becoming too aggressive against common
structured-output shapes.
**Status:** Informational — accepted tradeoff for defense-in-depth.

### SAN-002: Base64 detector does not produce sanitizer warnings
**Severity:** Low
**Location:** `internal/sanitize/sanitizer.go` lines 153-170
**Detail:** The base64 detector (lines 153-170) finds and decodes base64
strings and checks them for injection keywords. However, in live testing
with a base64-encoded injection payload (vector 4), no sanitizer_warnings
appeared in task metadata. Either the decoded content did not match the
injection keyword patterns, or the warning generation code path was not
reached. The injection did not succeed, but the lack of a warning means
operators cannot verify from logs that the base64 detector fired.
**Recommendation:** Add an explicit SanitizeWarning when the base64
decoder decodes a string and the decoded content contains any injection
keyword, regardless of whether the full pattern matches. This provides
an audit trail without changing the redaction behavior.
**Status:** Open

### SAN-003: Sanitizer coverage is model-version dependent
**Severity:** Informational
**Location:** `internal/sanitize/` (architectural concern)
**Detail:** For 6 of 8 tested injection vector classes, the pipeline's
security relies on the LLM provider's model alignment rather than
Overlord's sanitizer. This means that upgrading to a new model version
(e.g. from claude-sonnet-4-20250514 to a future version) could change the
security posture without any code changes. A model with weaker instruction-
following discipline or different alignment training could be vulnerable to
vectors 3, 5, 6, 7, or 8 even though all currently pass.
**Recommendation:** When upgrading model versions in agent configs, re-run
the injection stress test (05-injection-stress-test.yaml) against all 8
vectors before deploying to production. Add this to the deployment checklist
in docs/deployment.md.
**Status:** Informational — no code change required. Document in deployment
checklist.

### SEC-013: Unbounded WebSocket client count — RESOLVED
**Location:** `internal/api/websocket.go` — `wsHub.register`
**Severity:** Low
**Status:** Resolved
**Description:** No limit on total connected WebSocket clients. A large number of
connections could consume significant memory via per-client send buffers.
**Resolution:** `wsHub.register` enforces a `maxWSClients = 512` cap; arrivals past
the cap are rejected at registration with a 503-style close frame so legitimate
clients see a clear signal rather than a hung socket.

### SEC-014: Token bucket cleanup goroutine leaks
**Location:** `internal/api/ratelimit.go` — `cleanupLoop()`
**Severity:** Low
**Description:** The cleanup goroutine runs forever with no stop mechanism. Harmless
in production but leaks in tests.
**Recommendation:** Accept a context parameter and stop on cancellation.

### SEC2-003: cancel command TOCTOU race
**Location:** `cmd/overlord/main.go` — `cancelTask()`
**Severity:** Medium
**Description:** `cancelTask()` performs a read-then-write. Between GetTask and
UpdateTask, the broker can complete the task, and cancel will overwrite the DONE state.
**Recommendation:** Add `CancelTask` to the Store interface with atomic
`UPDATE WHERE state NOT IN ('DONE', 'FAILED')`.

### SEC2-005: Migration lacks concurrency protection against live broker
**Location:** `cmd/overlord/main.go` — `runMigration()`
**Severity:** Medium
**Description:** `migrate run` against a live pipeline with in-flight tasks can cause a
task to be processed with a pre-migration payload while migration writes the post-migration
payload. No data corruption (Postgres FOR UPDATE prevents that), but the task may be
processed with the wrong schema version.
**Recommendation:** Document that `migrate run` should target quiesced pipelines, or add
`--require-state DONE,FAILED` filter to only migrate terminal tasks.

### SEC4-003: WebSocket connections lack ping/pong keepalive — RESOLVED
**Location:** `internal/api/websocket.go` — `wsClient.readPump`, `wsClient.writePump`
**Severity:** Medium
**Status:** Resolved
**Description:** No `SetReadDeadline`, `PingHandler`, or `PongHandler` configured on
WebSocket connections. Zombie connections (client disconnects ungracefully) persist
indefinitely, consuming a goroutine and send buffer per connection. Combined with
SEC-013 (unbounded client count), this creates a resource exhaustion path.
**Resolution:** Each connection now runs with a `wsPongWait = 60s` read deadline
refreshed on every pong; the write pump emits pings every `wsPingPeriod` (90% of
`wsPongWait`) and every write is bounded by `wsWriteWait = 10s`. Dead peers are
closed within `wsPongWait` of their last pong.

### SEC4-006: No config-level size limit on system_prompt
**Location:** `internal/config/types.go` — `Agent.SystemPrompt`
**Severity:** Medium
**Description:** System prompts can be arbitrarily large in YAML config. A very
large prompt (100MB+) would be loaded, concatenated with envelope-wrapped output,
and sent to the LLM API without any guardrail.
**Recommendation:** Enforce max system_prompt length (e.g., 512KB) during config
validation.

### SEC4-007: Plugin file paths not validated against directory traversal — RESOLVED
**Location:** `internal/plugin/loader.go` — `resolvePaths()`
**Severity:** Medium
**Status:** Resolved
**Description:** Plugin `files:` list entries are passed to `os.Stat()` and
`plugin.Open()` without validating against `../` sequences. Requires config file
access (trusted), but defense-in-depth gap.
**Resolution:** A new subprocess-based plugin system (`provider: "plugin"`) has
been introduced alongside the existing `.so` loader. Subprocess plugins run in
isolated OS processes communicating via JSON-RPC 2.0 over stdin/stdout with
manifest-validated binary paths (name may not contain path separators),
explicit environment allow-listing, and Linux seccomp-BPF documentation for
defense-in-depth. See `docs/plugin-security.md`. The `.so` loader remains for
trusted operators; untrusted integrations should prefer the subprocess
provider.

### KG-004: Redis state index is not pipeline-scoped for dead-letter bulk ops — RESOLVED
**Location:** `internal/store/redis/redis.go` — `listTasksFromStatePipelineIndex`
**Severity:** Medium
**Status:** Resolved
**Description:** `ListTasks` with both State and PipelineID filters previously
read the entire per-state ZSET and filtered/paginated in Go, an O(total_failed)
scan per page fetched on accumulating dead-letter backlogs.
**Resolution:** Added a two-dimensional state×pipeline index at
`{prefix}tasks:state:{STATE}:pipeline:{PIPELINE_ID}`. Maintained on
EnqueueTask and in all three Lua state-transition scripts (updateTaskScript,
claimForReplayScript, rollbackReplayClaimScript). `ListTasks` with a
state+pipeline filter now reads the scoped 2D index directly.

### KG-005: Redis two-dimensional state×pipeline index is not backfilled
**Location:** `internal/store/redis/redis.go`
**Severity:** Low
**Description:** The two-dimensional state×pipeline index introduced for KG-004
is populated on write going forward. Tasks created before this index was
introduced will not appear in pipeline-scoped state queries until their state
next transitions. A backfill script iterating existing task keys and populating
the index is needed for live deployments upgrading from a prior version.
**Recommendation:** Provide a one-shot CLI backfill command that SCANs
`{prefix}task:*`, decodes each, and ZADDs to the corresponding
`tasks:state:{STATE}:pipeline:{PIPELINE_ID}` key.
**Status:** Open.

### SEC4-008d: Dead-letter discard was TOCTOU / non-atomic — RESOLVED
**Location:** `internal/api/handlers.go` — `handleDiscardDeadLetter`; `cmd/overlord/main.go` — `deadLetterDiscardCmd`; `internal/deadletter/service.go` — `DiscardAll`; `internal/store/{memory,redis,postgres}` — `DiscardDeadLetter`
**Severity:** Medium
**Status:** Resolved
**Description:** SEC4-008/a/b/c closed the replay side of the dead-letter
race (replay now claims atomically), but discard still used a
read-check-write shape (`GetTask` / `ListTasks` → state check → unconditional
`UpdateTask`). A concurrent replay that moved the task into `REPLAY_PENDING`
could be silently overwritten by a late discard, leaving the task's audit
state disagreeing with the replay outcome.
**Resolution:** Added `Store.DiscardDeadLetter(ctx, taskID) error` across
memory/redis/postgres as an atomic CAS (`FAILED && routed_to_dead_letter=true
→ DISCARDED`). Sentinel errors `ErrTaskNotDiscardable` and
`ErrTaskAlreadyDiscarded` let the HTTP API preserve the
`NOT_DEAD_LETTERED` / `ALREADY_DISCARDED` response codes while remaining
race-safe. API handler, CLI discard command, and bulk `DiscardAll` all
route through the new primitive. Regression coverage is in the store
conformance suite (`TestMemoryStoreConformance/DiscardDeadLetter_*`,
including `_LosesToReplayClaim` which is the direct race reproducer).

### SEC4-016: Starter workflow scaffold omitted .gitignore and .env.example — RESOLVED
**Location:** `internal/scaffold/templates/starter/`
**Severity:** Low
**Status:** Resolved
**Description:** The default `overlord init` starter is the beginner
path and explicitly documents switching to real providers via environment
variables, but it was the only first-party scaffold excluded from the
`.gitignore` / `.env.example` safety contract. A distracted user had no
scaffolded place to stage provider secrets and no default gitignore rule
for `.env` or `*.overlord-init-bak` backup files.
**Resolution:** Starter template now ships the same credential-hygiene
artefacts the strict templates carry; the scaffold safety-file tests in
`internal/scaffold/templates_test.go` (gitignore contract + env-example
placeholder check) iterate over `ListTemplates()` rather than only the
strict subset, so a regression that drops either file from starter fails
the suite.

### SEC4-010: IPv6 brute force tracking per /128 (not /64) — RESOLVED
**Location:** `internal/auth/auth.go` — `normalizeIP`
**Severity:** Medium
**Status:** Resolved
**Description:** BruteForceTracker tracks IPv6 addresses as full /128 strings. An
attacker with a /64 block has 2^64 distinct IPs. At 100k IP cap, the tracker
overflows quickly, after which new IPs fail open.
**Resolution:** `normalizeIP` masks IPv6 addresses to /64 before they are used as
map keys in the failures table, so every address inside a /64 aggregates into one
counter and an attacker holding a /64 prefix cannot bypass the threshold by rotating
through its addresses.

### SEC3-001: RecordSuccess resets brute force window indefinitely — RESOLVED
**Location:** `internal/auth/auth.go` — `RecordSuccess()` method
**Severity:** Medium
**Status:** Resolved in v0.2.0
**Description:** `RecordSuccess` previously called `delete(t.failures, ip)`, fully
resetting the failure counter to 0.
**Resolution:** `RecordSuccess` is now a no-op. Failures accumulate in a sliding
window regardless of intervening successes and expire naturally. The middleware
no longer calls `RecordSuccess` on successful auth. 429 responses now include
`Retry-After` and `X-RateLimit-Reset` headers so legitimate users know when
to retry.

---

## Informational (no action required)

### SEC4-004: No maximum length on path parameters
**Location:** `internal/api/handlers.go` — `pathParam()`
**Description:** Path parameters (pipelineID, taskID) have no length limit. Go's
HTTP server enforces URL length limits (~8KB) which caps this in practice.
**Status:** Accepted — implicit limit from HTTP layer is sufficient.

### SEC4-005: No length limit on query filter parameters
**Location:** `internal/api/handlers.go` — `handleListTasks()`
**Description:** `pipeline_id` and `stage_id` query parameters have no length
validation. Used for equality filtering only, not interpolated.
**Status:** Accepted — bounded by HTTP URL length limits.

### SEC4-009: UpdateTask allows arbitrary state transitions
**Location:** `internal/store/memory/memory.go`, `internal/store/postgres/postgres.go`
**Description:** Store implementations accept any state value without validating
it's a legal transition. Currently safe because only the broker (not API) calls
UpdateTask with hardcoded valid transitions.
**Status:** Accepted — defensive gap, not exploitable with current API surface.

### SEC4-011: CI build does not use -trimpath flag
**Location:** `.github/workflows/ci.yml`
**Description:** Builds embed the CI machine's filesystem paths. Prevents
reproducible builds and leaks minor path information.
**Status:** Accepted — low risk, improve opportunistically.

### SEC4-012: Agent API keys not zeroed after initialization
**Location:** `internal/agent/registry/registry.go`, agent adapter constructors
**Description:** LLM provider API keys persist in memory as plaintext in agent
structs. Unlike auth keys (which are zeroed), these must remain available for
each API call, so zeroing is not feasible.
**Status:** Accepted — inherent to the design; keys must be sent on each request.

### SEC4-013: math/rand/v2 used for retry jitter
**Location:** `internal/broker/broker.go`
**Description:** Non-cryptographic randomness used for ±10% backoff jitter.
Appropriate — jitter timing is not security-sensitive.
**Status:** Accepted — correct use of non-cryptographic randomness.

### SEC4-014: Single-tenant by design
**Location:** Architecture-wide
**Description:** Any authenticated write-scoped key can submit tasks to any
pipeline. No per-pipeline ACLs or ownership model. Deliberate single-operator
design.
**Status:** Accepted architectural decision — document in deployment guide if
multi-tenant requirements arise.

### SEC4-015: Database connection error may leak credentials
**Location:** `cmd/overlord/main.go`
**Description:** Connection failures use `%w` wrapping of driver errors. If pgx
or go-redis include the DSN in error messages, credentials could appear in logs.
**Status:** Accepted — driver-dependent; monitor if DSN leakage is observed.

### SEC-015: No JSON DisallowUnknownFields on task submission
**Location:** `internal/api/handlers.go` — `handleSubmitTask()`
**Description:** Extra fields in request body are silently ignored. Acceptable for
forward compatibility.
**Status:** Accepted — intentional for forward compatibility.

### SEC-016: Path parameter validation is adequate
**Location:** `internal/api/handlers.go` — `pathParam()`
**Description:** `pathParam()` rejects paths with `/`, preventing traversal. Pipeline
IDs, task IDs are validated before use. No action needed.
**Status:** Confirmed adequate.

### SEC2-NEW-002: Metrics endpoint on shared port
**Severity:** Informational
**Detail:** `/metrics` is served on the same port as the REST API
(default 8080). There is no separate metrics port configuration.
In production, restrict access to `/metrics` at the load balancer or
firewall level to prevent external exposure of pipeline topology,
agent names, and throughput data.
**Recommendation:** When deploying behind a reverse proxy (nginx,
caddy, etc.), block external access to `/metrics` while allowing
internal scraping from your metrics infrastructure. A future
enhancement could add a separate `--metrics-port` flag.
**Status:** Informational — no code change required.

### KG-001: Lua cjson round-trip shifts numeric encoding in payloads
**Location:** `internal/store/redis/redis.go` — `updateTaskScript`
**Severity:** Low
**Description:** The atomic UpdateTask Lua script decodes the task JSON with
`cjson.decode`, merges the update, and re-encodes with `cjson.encode`. This
round-trip is not byte-identical: key order, whitespace, and the
integer/float distinction are not preserved for numeric values inside
`json.RawMessage` payload fields. Current Task schema stores payloads as
`json.RawMessage` but comparison is always by semantic JSON equality, so this
is acceptable today.
**Recommendation:** Revisit if byte-identity of the payload becomes a
requirement (e.g. for content-addressable hashing or downstream signature
verification).
**Status:** Open — acceptable tradeoff for atomicity.

### KG-002: Per-state index is flat (not two-dimensional)
**Location:** `internal/store/redis/redis.go` — per-state secondary indexes
**Severity:** Low
**Description:** The new per-state secondary index introduces one ZSET per
state, aggregated across all pipelines. A `ListTasks` call that filters by
both State and PipelineID reads the full state ZSET and filters in Go. If a
single state accumulates a large cross-pipeline backlog (e.g. FAILED tasks
across many pipelines), this becomes O(N) in the backlog size rather than
O(result-set).
**Recommendation:** If a large cross-pipeline backlog materializes in
practice, add a two-dimensional state×pipeline index (`state:{state}:pipe:{id}`
ZSETs).
**Status:** Open — defer until an actual workload triggers it.

### KG-003: ListTasks total-count over-reports with certain filters
**Location:** `internal/store/redis/redis.go` — `ListTasks`
**Severity:** Low
**Description:** When `ListTasks` is called without a State filter, the total
count reflects `ZCARD` of the base index prior to filtering. `RoutedToDeadLetter`
and `IncludeDiscarded` are applied in Go after the range fetch, so the
reported total may exceed the number of tasks actually matching those filters.
This matches pre-existing Redis semantics (the old implementation had the same
behavior) and is consistent across stores.
**Recommendation:** If accurate totals are required for the dashboard pagination
UI, add per-flag indexes or switch the dashboard to cursor-based pagination
without a total count.
**Status:** Open — matches prior semantics; low-priority.

---

## Tracking

| # | Title | Severity | Status |
|---|-------|----------|--------|
| SEC-010 | Predictable envelope delimiters | Medium | Open |
| SEC-015 | Chain-adapter envelope bypass via placeholder substitution | High | Resolved |
| SEC-016 | Broker swallowed store errors + Postgres requeue duplicate-key | High | Resolved |
| SEC-012 | Redis UpdateTask not atomic | Medium | Resolved |
| SEC-013 | Unbounded WebSocket client count | Low | Resolved |
| SEC-014 | Token bucket cleanup goroutine leak | Low | Open |
| SEC-015 | No DisallowUnknownFields | Informational | Accepted |
| SEC-016 | Path param validation adequate | Informational | Confirmed |
| SEC2-003 | cancel command TOCTOU race | Medium | Open |
| SEC2-005 | Migration lacks live broker guard | Medium | Open |
| SEC2-NEW-002 | Metrics endpoint on shared port | Informational | Informational |
| SEC3-001 | RecordSuccess resets brute force window | Medium | Resolved |
| SEC4-003 | WebSocket lacks ping/pong keepalive | Medium | Resolved |
| SEC4-004 | No max length on path parameters | Low | Accepted |
| SEC4-005 | No length limit on query filter params | Low | Accepted |
| SEC4-006 | No config-level system_prompt size limit | Medium | Open |
| SEC4-007 | Plugin paths not traversal-checked | Medium | Resolved |
| SEC4-008 | Replay dead-letter phantom PENDING | High | Resolved |
| SEC4-008b | Concurrent replay semantics: N-winner read-only claim | Medium | Resolved |
| SEC4-008c | Replay claim consumed before Submit succeeds strands task (FAILED+DL=false) | High | Resolved |
| SEC4-008d | Dead-letter discard was TOCTOU / non-atomic vs concurrent replay | Medium | Resolved |
| SEC4-009 | UpdateTask allows arbitrary state transitions | Low | Accepted |
| SEC4-010 | IPv6 brute force tracking per /128 | Medium | Resolved |
| SEC4-011 | CI build missing -trimpath | Low | Accepted |
| SEC4-012 | Agent API keys not zeroed | Low | Accepted |
| SEC4-013 | math/rand/v2 for retry jitter | Informational | Accepted |
| SEC4-014 | Single-tenant by design | Informational | Accepted |
| SEC4-015 | DB connection error may leak creds | Low | Accepted |
| SAN-001 | Sanitizer active detection covers 6 of 8 vector classes | Low | Covered |
| SAN-004 | Output validation layer may produce false positives | Informational | Accepted |
| SAN-002 | Base64 detector produces no sanitizer warnings | Low | Open |
| SAN-003 | Sanitizer coverage is model-version dependent | Informational | Open |
| KG-001 | Lua cjson round-trip shifts numeric encoding | Low | Open |
| KG-002 | Per-state index is flat (not 2D) | Low | Open |
| KG-003 | ListTasks total-count over-reports with certain filters | Low | Open |
| KG-004 | Redis state index not pipeline-scoped for bulk ops | Medium | Resolved |
| KG-005 | Redis 2D state×pipeline index not backfilled | Low | Open |
| SEC4-016 | Starter workflow scaffold omitted .gitignore / .env.example | Low | Resolved |

---

## Resolved Post-v0.2.0

| # | Title | Severity | Resolution |
|---|-------|----------|------------|
| — | Plugin manifest/binary validation deferred to first Execute/HealthCheck — missing/non-executable binary surfaces as task failure (exit 1) instead of config/startup error (exit 3) | Medium | `Manifest.ValidateBinary` checks existence, type, and executable bit; `LoadAndCreate` now loads the manifest eagerly and validates the resolved binary path at construction time. Plugin agent construction failures in `overlord exec` surface as `execExitConfig` (exit 3) because `buildAgents` runs before task submission and returns a config error. |
| — | Plugin subprocess Stop() not wired into run, exec, or reload shutdown paths — plugin subprocesses become zombies on shutdown despite docs claiming SIGTERM→SIGKILL | Medium | New `agent.Stopper` interface (`Stop() error`); plugin `*Agent` satisfies it and the `lazyAgent` wrapper forwards to it. `registry.Stoppers(agents)` returns all agents that implement Stopper. `overlord run` shutdown, `overlord exec` shutdown, and the SIGHUP hot-reload path all call `Stop()` on each Stopper after the broker drains (reload stops the *old* registry before swapping). `docs/plugin-security.md` shutdown section updated to describe the wired sequence. |
| — | Plugin RPC mutex held across full I/O path not documented for operators planning capacity | Low | Comment added at the mutex declaration in `internal/plugin/agent.go` documenting serial execution and directing operators at the capacity section; new "Throughput and Capacity Planning" section in `docs/plugin-security.md` explains how to scale via multiple agent instances sharing a manifest. |
| — | Prompt/payload/output preview debug logging in adapters | High | Debug log lines removed from anthropic, google, openai, ollama adapters |
| — | Raw internal errors returned in HTTP handler responses | High | `writeInternalError` helper introduced; all leak sites replaced; stable public messages and request IDs |
| SEC-012 | Redis UpdateTask not atomic | Medium | Atomic cjson Lua script merges updates server-side in one round-trip |
| — | Redis terminal tasks removed from sorted-set index | Medium | Terminal tasks retained with TTL expiry instead of eviction |
| — | Redis ListTasks full-scan on state filter | Medium | Per-state secondary index replaces full-scan filtering |
| SEC4-008 | Single-task replay mutates original dead-lettered task to PENDING without re-enqueueing, leaving a phantom pending task | High | `ClaimForReplay` is now an atomic validator-and-claimer across Redis/Memory/Postgres; on successful replay submission the original is marked `REPLAYED` (terminal audit state) and a new task carries the retry. If submit fails, `RollbackReplayClaim` restores the original to FAILED+dead-lettered. |
| SEC4-008b | Concurrent replay requests for the same dead-lettered task each submitted duplicate tasks (N-winner read-only claim) | Medium | `ClaimForReplay` now atomically transitions the task into `REPLAY_PENDING` across Redis/Memory/Postgres; exactly one caller wins, N-1 concurrent callers receive `ErrTaskNotReplayable` (409). On submit success the original becomes `REPLAYED` (terminal) preserving the audit trail; on submit failure it rolls back to FAILED+dead-lettered. replay-all now goes through `ClaimForReplay` too. |
| SEC4-008c | Replay claim consumed before Submit succeeds strands task (FAILED+DL=false, invisible to replay) | High | `ClaimForReplay` now transitions the task to `REPLAY_PENDING` (distinct state) instead of flipping only the flag. Handler calls `RollbackReplayClaim` if Submit fails, restoring `FAILED+DL=true`. On Submit success, the original is marked `REPLAYED` as a terminal audit state. New sentinel `ErrTaskNotReplayPending` for the rollback failure path. |
| — | OpenAI Responses adapter failed hard on non-JSON model output | Medium | Adapter now parses-then-falls-back: non-JSON text is wrapped as `{"text": "<raw>"}` so downstream schema validation always sees an object root |
| — | ws-token endpoint missing issuance/consumption logging, concurrent upgrade test, and documentation | Low | Structured logs added on issue/consume/reject (without leaking token values); `TestWSToken_ConcurrentUpgrade` asserts single-winner semantics; `docs/api.md` documents the flow; misleading "periodic background sweep" comment corrected |
| — | Brute-force tracker comments overstated near-capacity behavior; eviction only fired at hard cap | Medium | Tracker now evicts in bulk at 90% of `maxIPCap` down to 80%, amortising the hot-path cost. Comments updated to match. SEC4-010 (IPv6 /64 coalescing) already resolved — near-capacity behaviour is the remaining piece |
| — | replay-all / discard-all silently ignored per-task failures | Low | Per-task failures now logged at Warn with `task_id`, `pipeline_id`, and `error`; response body now includes a `failed` count alongside `count` |
| — | Postgres store drift: `ClaimForReplay` ignored `RoutedToDeadLetter`; `UpdateTask` and `ListTasks` ignored `RoutedToDeadLetter`, `CrossStageTransitions`, and dead-letter/discarded filters | High | Postgres schema extended (migration 003) with `routed_to_dead_letter` and `cross_stage_transitions` columns; Postgres `ClaimForReplay`/`UpdateTask`/`ListTasks` now match Redis and Memory semantics |
| — | CLI replay commands bypass atomic `ClaimForReplay`, reintroducing duplicate-replay bug through a separate code path | High | CLI `dead-letter replay` and `replay-all` now go through the same `ClaimForReplay` → `Submit` → `RollbackReplayClaim`-on-failure → mark `REPLAYED` sequence as the HTTP handlers. Concurrent CLI invocations and mixed CLI/API usage now produce exactly one winner; failed submits roll back to FAILED+DL=true or surface actionable REPLAY_PENDING warnings. |
| — | `ClaimForReplay`/`RollbackReplayClaim` contract documented in `store.Store` but broker mirror interface carried stale doc comments | Medium | `internal/broker/broker.go` mirror interface comments now copy `store.Store` verbatim, keeping the two interfaces in lockstep |
| — | Store conformance suite missing `ClaimForReplay` and `RollbackReplayClaim` coverage — backend drift could re-enter silently | Medium | `internal/store/store_conformance_test.go` gains happy-path, not-found, wrong-state, already-claimed, 20-way concurrent single-winner, rollback happy-path, rollback not-found, rollback wrong-state, and claim-after-rollback tests — all backends run them |
| — | `docs/api.md` ws-token length inaccurate (doc said 32-char, implementation emits 64-char hex) | Low | Doc corrected to "64-character hex-encoded string (32 random bytes)"; clarified single-use + 30s TTL semantics alongside |
| — | ws-token issuance and rejection logs omit `client_ip`, making incident triage slow | Low | Both logs now carry `client_ip` via the existing `clientIP(r)` helper; token values still never logged; consumption (DEBUG) intentionally unchanged |
| — | `failed` field uses `omitempty` on bulk dead-letter responses, collapsing zero-failures with field-absent wire shape | Low | `failed` no longer has `omitempty` on `replayAllResponse`/`discardAllResponse`; `"failed": 0` is always present so callers can distinguish "no failures" from an older server |
| — | `internal/store/mock` has no import-path signal that it is test-only | Low | Package moved to `internal/testutil/storemock` with explicit `Do not import this package in production code` package doc; sole importer (`internal/api/dead_letter_test.go`) updated |
| — | Hot-reload stops old plugin subprocesses immediately after b.Reload(), racing with in-flight plugin RPCs and causing spurious task failures for unchanged agents | High | Plugin agents now implement `Drainer` interface with atomic in-flight RPC tracking. Hot-reload marks old agents as draining (no new RPCs), waits for in-flight RPCs to complete (up to configurable drain grace period, default 10s), then calls `Stop()`. New `waitForDrain` polls until all in-flight counters reach zero or context times out. Unchanged agents are stopped and restarted during reload (known limitation — future versions will reuse subprocesses). |
| — | replay-all `failed` count may exceed distinct failing task IDs because rolled-back tasks reappear on subsequent pages and are retried | Low | `handleReplayAllDeadLetter` now tracks a `failedIDs` set (mirroring `discard-all`) and skips already-failed IDs on subsequent pages; `failed` is now `len(failedIDs)` — exact distinct-task count. Test `TestReplayAll_PerTaskFailure` tightened to assert `Failed == 2` and new `TestReplayAll_RollbackDoesNotInflateCount` added |
| — | CLI `replay-all` confirmation prompt understates total when dead-letter set exceeds 1000 tasks | Low | CLI now calls `ListTasks` with `Limit: 1` and reads `result.Total` for an accurate count before prompting; handles empty set (no prompt) and counts exceeding `maxBulkOperationTasks` (100000) with an explicit ceiling warning |
| — | CLI bulk dead-letter operations (replay-all, discard-all) have drifted from hardened API semantics — CLI replay-all does not track failedIDs, CLI discard-all is single-page | Medium | Shared `internal/deadletter` service introduced with `ReplayAll`, `DiscardAll`, and `Count` methods. API handlers and CLI both delegate to `deadletter.Service` — the `failedIDs` pattern, full pagination, ceiling enforcement, and structured logging live in one place. CLI `discard-all` now paginates fully (was single-page at 1000); CLI `replay-all` now tracks `failedIDs` (was missing, could double-count rolled-back tasks) |
| — | Recovery for stranded REPLAY_PENDING tasks is operationally under-specified — code comment references "admin API" that does not exist, no first-class recovery mechanism documented | Medium | New first-class recovery path: `POST /v1/tasks/{id}/recover` endpoint and `overlord dead-letter recover` CLI command both wrap `RollbackReplayClaim` to transition a stranded task back to FAILED+RoutedToDeadLetter=true. Double-failure Error log now points operators at the recovery path; `StateReplayPending` type comment now documents the endpoint; `docs/api.md` documents the endpoint alongside existing dead-letter endpoints |
| — | README omits `REPLAY_PENDING` and `REPLAYED` states from lifecycle documentation | Low | README lifecycle section now includes the replay state machine with `REPLAY_PENDING` and `REPLAYED`; dead-letter section documents single-replay, replay-all, discard-all, and recovery semantics accurately |
| — | Conformance suite missing `REPLAY_PENDING`→`REPLAYED` transition test and `RollbackReplayClaim` idempotency test | Low | Added `ReplayPendingToReplayed` (verifies transition, non-replayability of REPLAYED, dead-letter exclusion) and `RollbackReplayClaim_Idempotency` (second rollback returns `ErrTaskNotReplayPending` cleanly, state unchanged) to `internal/store/store_conformance_test.go`; all backends run them |
| — | Double-failure API test asserted logs without asserting task remains in `REPLAY_PENDING` | Low | `TestReplayDeadLetter_SubmitAndRollbackFail` and `TestReplayAll_SubmitAndRollbackFail` now also assert `State == REPLAY_PENDING` and `RoutedToDeadLetter == false` post-double-failure, documenting the stranded-task reality operators must recover via `POST /v1/tasks/{id}/recover` |
| — | Postgres `ClaimForReplay` two-round-trip TOCTOU — UPDATE returning 0 rows followed by a separate SELECT to distinguish NOT_FOUND vs NOT_REPLAYABLE creates a window where a deleted row produces the wrong error | Low | `ClaimForReplay` and `RollbackReplayClaim` collapsed into single CTE-backed statements with FULL OUTER JOIN; the UPDATE and existence check share one MVCC snapshot, so NOT_FOUND vs NOT_REPLAYABLE (and NOT_FOUND vs NOT_REPLAY_PENDING) disambiguation is atomic and completes in a single round trip |
| — | Standalone pipeline file schema paths resolved against the infra-config directory rather than the pipeline file's own directory | Medium | `LoadPipelineFile` now returns the canonical absolute pipeline-file path; `MergeInto` rebases relative `schema_registry` paths against `filepath.Dir(pipelineFilePath)` before merging into the infra config and stat-checks each so missing files surface a clear error referencing the pipeline file. Documented in `docs/exec.md` and covered by new tests in `internal/config/pipeline_file_test.go` (rebased, absolute-pass-through, and mismatched-relative cases). |
| — | Terminal state mismatch between `overlord exec` and `overlord submit --wait` (DISCARDED and REPLAYED not recognized by submit --wait) | Medium | Introduced `broker.TaskState.IsTerminal()` covering DONE/FAILED/DISCARDED/REPLAYED. `exec` and `submit --wait` both poll on `IsTerminal()`. `submit --wait` now exits 0 on DONE/REPLAYED, 1 on FAILED/dead-letter/DISCARDED (with an explanatory message and replay hint where applicable), and 2 on timeout — matching exec's exit-code contract. New `submitWaitError` carries the exit code out to `main.Run`. |
| — | `--payload @file` followed symlinks and accepted non-regular files, inconsistent with config and pipeline file loading | Low | New `readPayloadFile` helper in `cmd/overlord/exec.go` mirrors the config/pipeline `Lstat` → reject-symlink → require-regular-file → read flow, with human-readable error messages. Tests in `cmd/overlord/exec_test.go` cover symlink rejection, missing file, and the previous happy path. |
| — | `Agent.Stop()` sent `SIGINT` (via `os.Interrupt`) instead of `SIGTERM`, inconsistent with documented shutdown behavior in `docs/plugin-security.md` | Low | `internal/plugin/agent.go` `Stop()` now sends `syscall.SIGTERM`, matching the documented SIGTERM→SIGKILL sequence. |
| — | submit still uses raw `os.ReadFile` for `@file` payloads, bypassing symlink and non-regular-file hardening applied in exec | Medium | `readPayloadFile` moved to shared `cmd/overlord/payload.go`; submit now calls it for `@file` paths, matching exec's Lstat → reject-symlink → require-regular-file → read flow |
| — | submit `--wait` missing broker drain and Stoppers cleanup, reintroducing plugin orphaning on that command path | Medium | submit `--wait` now mirrors exec's cleanup: broker cancel → drain wait (10s cap) → `registry.Stoppers(b.Agents())` Stop. Runs via defer on all exit paths |
| — | `TestPluginAgent_Stop_SendsSIGTERM` does not distinguish SIGTERM from stdin EOF | Low | Echo plugin now writes `{"signal":"SIGTERM"}` sentinel on SIGTERM receipt (controlled by `ECHO_PLUGIN_SIGTERM_SENTINEL`). Test reads the agent's stdout channel after Stop and asserts the sentinel was emitted |
| — | Stale lifecycle and help text docs (README hot-reload section, plugin-security.md ordering, terminal state help strings) | Low | README hot-reload roadmap updated; status `--watch` and cancel help text list all four terminal states; submit `Long` now documents `--wait` exit codes |

---

## Resolved in v0.2.0

Items below were resolved and verified in the codebase. Kept for audit trail.

### Integration Testing Findings

| # | Title | Severity | Resolution |
|---|-------|----------|------------|
| 1 | Broker.Reload data races | Critical | Added `sync.RWMutex` to Broker struct |
| 2 | Race detector not validated | Critical | `go test -race ./...` passes clean |
| 3 | Hot-reload missing new stage workers | High | `Reload()` diffs and spawns/drains workers |
| 4 | Redis ListTasks O(N) scan | High | Replaced with sorted set index |
| 5 | Postgres UpdateTask not atomic | High | Wrapped in `SELECT ... FOR UPDATE` transaction |
| 6 | WebSocket hub shutdown race | Medium | Guarded with `sync.Once` |
| 7 | Flaky timing-sensitive tests | Medium | Replaced with deterministic synchronization |
| 8 | staticcheck ST1005 copilot stub | Low | Lowercase error string |
| 9 | Integration tests need live services | Low | Added docker-compose.test.yml and Makefile |

### First Security Audit (SEC-)

| # | Title | Severity | Resolution |
|---|-------|----------|------------|
| SEC-007 | No ID character validation | Medium | `validateID()` with safe character regex |
| SEC-008 | ListTasks limit unbounded | Medium | Capped at 1000 with strict validation |
| SEC-009 | No TLS enforcement for cloud providers | Medium | `requireTLS()` rejects non-HTTPS (localhost exempt) |
| SEC-011 | Go 1.25.0 stdlib vulnerabilities | Medium | Upgraded to Go 1.25.9 |
| SEC-017 | No API authentication | Informational | Resolved — auth added in v0.2.0 |

### Second Security Audit (SEC2-)

| # | Title | Severity | Resolution |
|---|-------|----------|------------|
| SEC2-001 | OTLP exporter insecure by default | High | Defaults to TLS; plaintext requires `otlp_insecure` |
| SEC2-002 | /metrics rate-limited (DoS scrapes) | High | `/metrics` exempt from rate limiter |
| SEC2-004 | trace_id/span_id not reserved | Medium | Added to `reservedMetadataKeys` |
| SEC2-006 | sslmode=disable in compose example | Medium | Annotated with network safety guidance |
| SEC2-NEW-001 | YAML parse error leaks file content | Low-Medium | Symlinks rejected; error messages sanitized |

### Fourth Security Audit (SEC4-)

| # | Title | Severity | Resolution |
|---|-------|----------|------------|
| SEC4-001 | Missing HTTP security headers | High | Added `securityHeaders` middleware (X-Content-Type-Options, X-Frame-Options, Referrer-Policy) |
| SEC4-002 | No CSP on dashboard | High | Added Content-Security-Policy header to dashboard responses |

### Third Security Audit (SEC3-)

| # | Title | Severity | Resolution |
|---|-------|----------|------------|
| SEC3-001 | RecordSuccess resets brute force window | Medium | `RecordSuccess` is now a no-op; failures expire via sliding window; Retry-After header on 429 |
| SEC3-002 | Non-uniform 401 responses | Medium | All auth failures return identical `UNAUTHORIZED` body |
| SEC3-003 | 403 reveals scope information | Low | Forbidden responses return generic `FORBIDDEN` body |
