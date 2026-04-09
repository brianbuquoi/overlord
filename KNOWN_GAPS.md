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

### SEC3-001: RecordSuccess resets brute force window indefinitely
**Location:** `internal/auth/auth.go` — `RecordSuccess()` method
**Severity:** Medium
**Description:** `RecordSuccess` calls `delete(t.failures, ip)`, fully resetting
the failure counter to 0. An attacker who possesses one valid API key can reset
their own brute force window indefinitely by authenticating successfully every
N-1 attempts (where N is the failure threshold). This allows unlimited password
guessing against other keys from the same IP as long as the attacker has one
valid credential.
**Recommendation:** Replace the full delete with a decrement or partial reset.
Alternatively, track success and failure counts separately so that success
reduces but does not eliminate the failure history.

---

## Informational (no action required)

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
| SEC3-001 | RecordSuccess resets brute force window | Medium | Open |

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

### Third Security Audit (SEC3-)

| # | Title | Severity | Resolution |
|---|-------|----------|------------|
| SEC3-002 | Non-uniform 401 responses | Medium | All auth failures return identical `UNAUTHORIZED` body |
| SEC3-003 | 403 reveals scope information | Low | Forbidden responses return generic `FORBIDDEN` body |
