# Orcastrator — Known Gaps & Deferred Issues

These items were identified during integration testing and security audits.
Each entry includes the severity, location, and recommended fix approach.

---

## Open Issues

### SEC-010: Envelope delimiters are predictable
**Location:** `internal/sanitize/envelope.go`
**Severity:** Medium
**Description:** Envelope delimiters (`[SYSTEM CONTEXT ...]`, `[END SYSTEM CONTEXT]`)
are static, human-readable strings. While the sanitizer detects these in agent output,
a per-task random nonce in the delimiter would provide stronger defense-in-depth.
**Recommendation:** Use a cryptographically random nonce in delimiters per-task.

### SEC-012: Redis UpdateTask is not atomic (read-modify-write race)
**Location:** `internal/store/redis/redis.go` — `UpdateTask()`
**Severity:** Medium
**Description:** GET → modify → SET without locking. Concurrent updates may cause
lost writes. Unlike the Postgres store which uses `SELECT FOR UPDATE`, Redis has
no equivalent row locking.
**Recommendation:** Use a Lua script or WATCH/MULTI for atomic read-modify-write.

### SEC-013: Unbounded WebSocket client count
**Location:** `internal/api/websocket.go` — `wsHub`
**Severity:** Low
**Description:** No limit on total connected WebSocket clients. A large number of
connections could consume significant memory via per-client send buffers.
**Recommendation:** Add a maximum client count to the hub.

### SEC-014: Token bucket cleanup goroutine leaks
**Location:** `internal/api/ratelimit.go` — `cleanupLoop()`
**Severity:** Low
**Description:** The cleanup goroutine runs forever with no stop mechanism. Harmless
in production but leaks in tests.
**Recommendation:** Accept a context parameter and stop on cancellation.

### SEC2-003: cancel command TOCTOU race
**Location:** `cmd/orcastrator/main.go` — `cancelTask()`
**Severity:** Medium
**Description:** `cancelTask()` performs a read-then-write. Between GetTask and
UpdateTask, the broker can complete the task, and cancel will overwrite the DONE state.
**Recommendation:** Add `CancelTask` to the Store interface with atomic
`UPDATE WHERE state NOT IN ('DONE', 'FAILED')`.

### SEC2-005: Migration lacks concurrency protection against live broker
**Location:** `cmd/orcastrator/main.go` — `runMigration()`
**Severity:** Medium
**Description:** `migrate run` against a live pipeline with in-flight tasks can cause a
task to be processed with a pre-migration payload while migration writes the post-migration
payload. No data corruption (Postgres FOR UPDATE prevents that), but the task may be
processed with the wrong schema version.
**Recommendation:** Document that `migrate run` should target quiesced pipelines, or add
`--require-state DONE,FAILED` filter to only migrate terminal tasks.

### SEC4-003: WebSocket connections lack ping/pong keepalive
**Location:** `internal/api/websocket.go` — `readPump()`
**Severity:** Medium
**Description:** No `SetReadDeadline`, `PingHandler`, or `PongHandler` configured on
WebSocket connections. Zombie connections (client disconnects ungracefully) persist
indefinitely, consuming a goroutine and send buffer per connection. Combined with
SEC-013 (unbounded client count), this creates a resource exhaustion path.
**Recommendation:** Add ping/pong with 60s deadline. Close connections that miss
2 consecutive pong responses.

### SEC4-006: No config-level size limit on system_prompt
**Location:** `internal/config/types.go` — `Agent.SystemPrompt`
**Severity:** Medium
**Description:** System prompts can be arbitrarily large in YAML config. A very
large prompt (100MB+) would be loaded, concatenated with envelope-wrapped output,
and sent to the LLM API without any guardrail.
**Recommendation:** Enforce max system_prompt length (e.g., 512KB) during config
validation.

### SEC4-007: Plugin file paths not validated against directory traversal
**Location:** `internal/plugin/loader.go` — `resolvePaths()`
**Severity:** Medium
**Description:** Plugin `files:` list entries are passed to `os.Stat()` and
`plugin.Open()` without validating against `../` sequences. Requires config file
access (trusted), but defense-in-depth gap.
**Recommendation:** Reject paths containing `..` or resolve to absolute and verify
within an allowed directory.

### SEC4-008: Replay dead-letter TOCTOU race
**Location:** `internal/api/handlers.go` — `handleReplayDeadLetter()`
**Severity:** Medium
**Description:** GetTask → state check → Submit is not atomic. A concurrent discard
between check and submit could result in replaying a task that was just discarded.
**Recommendation:** Use atomic compare-and-swap or lock the task during replay.

### SEC4-010: IPv6 brute force tracking per /128 (not /64)
**Location:** `internal/auth/auth.go` — `RecordFailure()`, `internal/api/middleware.go` — `clientIP()`
**Severity:** Medium
**Description:** BruteForceTracker tracks IPv6 addresses as full /128 strings. An
attacker with a /64 block has 2^64 distinct IPs. At 100k IP cap, the tracker
overflows quickly, after which new IPs fail open.
**Recommendation:** Normalize IPv6 to /64 prefix before tracking.

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
**Location:** `cmd/orcastrator/main.go`
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

---

## Tracking

| # | Title | Severity | Status |
|---|-------|----------|--------|
| SEC-010 | Predictable envelope delimiters | Medium | Open |
| SEC-012 | Redis UpdateTask not atomic | Medium | Open |
| SEC-013 | Unbounded WebSocket client count | Low | Open |
| SEC-014 | Token bucket cleanup goroutine leak | Low | Open |
| SEC-015 | No DisallowUnknownFields | Informational | Accepted |
| SEC-016 | Path param validation adequate | Informational | Confirmed |
| SEC2-003 | cancel command TOCTOU race | Medium | Open |
| SEC2-005 | Migration lacks live broker guard | Medium | Open |
| SEC2-NEW-002 | Metrics endpoint on shared port | Informational | Informational |
| SEC3-001 | RecordSuccess resets brute force window | Medium | Resolved |
| SEC4-003 | WebSocket lacks ping/pong keepalive | Medium | Open |
| SEC4-004 | No max length on path parameters | Low | Accepted |
| SEC4-005 | No length limit on query filter params | Low | Accepted |
| SEC4-006 | No config-level system_prompt size limit | Medium | Open |
| SEC4-007 | Plugin paths not traversal-checked | Medium | Open |
| SEC4-008 | Replay dead-letter TOCTOU race | Medium | Open |
| SEC4-009 | UpdateTask allows arbitrary state transitions | Low | Accepted |
| SEC4-010 | IPv6 brute force tracking per /128 | Medium | Open |
| SEC4-011 | CI build missing -trimpath | Low | Accepted |
| SEC4-012 | Agent API keys not zeroed | Low | Accepted |
| SEC4-013 | math/rand/v2 for retry jitter | Informational | Accepted |
| SEC4-014 | Single-tenant by design | Informational | Accepted |
| SEC4-015 | DB connection error may leak creds | Low | Accepted |

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
