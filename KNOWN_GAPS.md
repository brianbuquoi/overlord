# Orcastrator — Known Gaps & Deferred Issues

These items were identified during integration testing and deferred from
the initial implementation. Each entry includes the severity, location,
and recommended fix approach.

---

## Critical (address before production)

### 1. Broker.Reload has data races
**Location:** `internal/broker/broker.go` — `Reload()` method  
**Severity:** Critical  
**Status:** Resolved  
**Resolution:** Added `sync.RWMutex` to the Broker struct. `Reload()` acquires
the write lock for the full map swap. Workers snapshot `pipelines`, `stages`,
`agentCfg`, `agents`, and `validator` under a read lock each iteration.
`Run()` reads config under read lock when spawning workers. `Submit()` reads
`pipelines` under read lock. Confirmed clean under `go test -race ./...`.

### 2. Race detector not yet validated
**Location:** CI pipeline  
**Severity:** Critical  
**Status:** Resolved  
**Resolution:** gcc/cgo is now available. `go test -race ./...` passes clean
across the entire codebase with zero data races detected.

---

## High (address before significant load)

### 3. Hot-reload does not start workers for new stages
**Location:** `internal/broker/broker.go`  
**Severity:** High  
**Status:** Resolved  
**Resolution:** `Reload()` now diffs the old and new stage sets. New stages get a
fresh worker pool derived from the `Run()` context. Removed stages have their
per-stage context cancelled, signalling workers to drain — in-flight tasks finish
before the worker exits. Worker goroutines from both `Run()` and `Reload()` are
tracked in a shared `sync.WaitGroup` so `Run()` waits for all of them on shutdown.
Confirmed clean under `go test -race ./internal/broker/...`.

### 4. Redis ListTasks uses SCAN (O(N))
**Location:** `internal/store/redis/redis.go` — `ListTasks()`  
**Severity:** High at scale — full keyspace scan blocks Redis  
**Status:** Resolved  
**Resolution:** Replaced the SCAN-based implementation with a sorted set index.
`EnqueueTask` now ZADDs the task ID to `{prefix}:index:{pipelineID}:{stageID}`
with the CreatedAt timestamp as score. `ListTasks` queries the sorted set with
ZRANGEBYSCORE + LIMIT for O(log N + page size) pagination and ZCARD for O(1)
total count. Task data is fetched via MGET. Dangling index entries (task key
expired while index entry remains) are silently skipped. `UpdateTask` removes
tasks from the index when they reach terminal states (DONE/FAILED). State
filtering is applied in Go after fetch — documented as a known limitation since
sorted sets don't index by state. Filter combinations: pipeline+stage queries a
single index key directly; pipeline-only unions across stage index keys; no
filter scans index key names (not task keys).

### 5. Postgres UpdateTask is not atomic
**Location:** `internal/store/postgres/postgres.go` — `UpdateTask()`  
**Severity:** High for multi-instance deployments  
**Status:** Resolved  
**Resolution:** Wrapped the read-modify-write in a transaction with
`SELECT ... FOR UPDATE` to acquire a row lock before applying updates.
If the SELECT returns no rows, the transaction rolls back and returns
`ErrTaskNotFound`. Added integration tests verifying concurrent update
atomicity (two goroutines with conflicting updates — exactly one wins,
no mixed state) and ErrTaskNotFound on unknown ID.

---

## Medium (address before GA)

### 6. WebSocket hub shutdown race
**Location:** `internal/api/` — `wsHub.run()`  
**Severity:** Medium — low probability, non-fatal  
**Status:** Resolved  
**Resolution:** Added `sync.Once` (`closeOnce`) to guard `close(h.done)` in
`wsHub.run()`. Added `stopped()` helper that checks the `done` channel via
non-blocking select. `register()` checks `stopped()` before adding clients.
`shutdown()` is safe for concurrent calls: `Unsubscribe()` is idempotent,
`<-h.done` returns immediately on a closed channel, and client cleanup uses
per-client `closeOnce`. Confirmed clean under `go test -race ./internal/api/...`
with targeted tests for immediate shutdown, concurrent unregister (10 goroutines),
concurrent shutdown (10 goroutines), and register-after-shutdown.

### 7. Flaky timing-sensitive tests
**Location:**
- `internal/store/memory/` — `TestFIFOGuaranteeUnderConcurrentEnqueue`
- `internal/config/` — `TestAdversarial_WatchReloadsOnFileChange`  
**Severity:** Medium — ~1/20 failure rate in CI  
**Status:** Resolved  
**Resolution:** Replaced sleep-based synchronization with deterministic approaches.
The FIFO test now serializes enqueue + order recording under a single mutex so the
recorded order exactly matches queue insertion order, eliminating the racy timestamp
check. The file watcher test now uses a done channel signaled by the onChange callback,
blocked on via `select` with a 2-second timeout instead of polling with sleeps.
Both tests pass 20/20 with `-count=1`.

---

## Low (quality of life)

### 8. staticcheck ST1005 in copilot stub
**Location:** `internal/agent/copilot/copilot.go`  
**Severity:** Low — style only  
**Status:** Resolved  
**Resolution:** Changed error string to lowercase without trailing period:
`"github copilot agentic API is not yet publicly available"`. ST1005 finding eliminated.

### 9. Redis/Postgres integration tests require live services
**Location:** `internal/store/redis/`, `internal/store/postgres/`  
**Severity:** Low — development convenience  
**Status:** Resolved  
**Resolution:** Added `docker-compose.test.yml` (Redis 7-alpine + Postgres 16-alpine
with tmpfs and health checks) and a `Makefile` with `test-unit`, `test-integration`,
`test-all`, and `check` targets. `make test-integration` brings up compose services,
runs integration tests with the correct env vars, and tears down on exit.

---

## Security Audit Findings (SEC- prefix)

### SEC-007: No validation of pipeline/stage/agent IDs against safe character set
**Location:** `internal/config/config.go` — `validate()` function  
**Severity:** Medium  
**Status:** Resolved  
**Resolution:** Added `validateID()` function with regex `^[a-zA-Z0-9][a-zA-Z0-9._-]*$`
that validates all pipeline names, stage IDs, and agent IDs at config load time.
IDs containing `:`, `/`, spaces, null bytes, or other unsafe characters are rejected
with a clear error message. This prevents Redis key collisions and broker `stageKey()`
confusion.

### SEC-008: ListTasks limit parameter has no upper bound
**Location:** `internal/api/handlers.go` — `handleListTasks()`  
**Severity:** Medium  
**Status:** Resolved  
**Resolution:** Added strict validation for `limit` and `offset` query parameters.
`limit` must be 1–1000 (capped at `maxListLimit = 1000`); `offset` must be >= 0.
Non-numeric values, negative values, and values exceeding the cap all return 400
with a structured JSON error including the `INVALID_LIMIT` or `INVALID_OFFSET` code.

### SEC-009: No TLS enforcement for non-Ollama providers
**Location:** `internal/agent/anthropic/anthropic.go`, `openai/openai.go`,
`google/gemini.go`, `ollama/ollama.go` — BaseURL/endpoint validation  
**Severity:** Medium  
**Status:** Resolved  
**Resolution:** Added `requireTLS()` function to Anthropic, OpenAI, and Gemini
adapters that rejects non-HTTPS base URLs at construction time. Localhost/127.0.0.1
are exempted to allow test servers. Ollama gets `validateEndpoint()` with a
different policy: HTTP is allowed only for localhost/127.0.0.1 (its default use
case); remote endpoints must use HTTPS. Error messages explain the TLS requirement.

### SEC-010: Envelope delimiters are predictable
**Location:** `internal/sanitize/envelope.go`  
**Severity:** Medium  
**Description:** Envelope delimiters (`[SYSTEM CONTEXT ...]`, `[END SYSTEM CONTEXT]`)
are static, human-readable strings. While the sanitizer detects these in agent output,
a per-task random nonce in the delimiter would provide stronger defense-in-depth.  
**Recommendation:** Use a cryptographically random nonce in delimiters per-task.

### SEC-011: Go 1.25.0 has 17 known standard library vulnerabilities
**Location:** `go.mod` — `go 1.25.0`  
**Severity:** Medium  
**Status:** Resolved  
**Resolution:** Upgraded to Go 1.25.9 in `go.mod`. `govulncheck -mode binary`
reports 0 vulnerabilities in the compiled binary. All 17 original stdlib
vulnerabilities (crypto/tls, net/url, net/http, etc.) are fixed.

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

### SEC-015: No JSON DisallowUnknownFields on task submission
**Location:** `internal/api/handlers.go` — `handleSubmitTask()`  
**Severity:** Informational  
**Description:** Extra fields in request body are silently ignored. Acceptable for
forward compatibility.  
**Status:** Accepted — intentional for forward compatibility.

### SEC-016: Path parameter validation is adequate
**Location:** `internal/api/handlers.go` — `pathParam()`  
**Severity:** Informational  
**Description:** `pathParam()` rejects paths with `/`, preventing traversal. Pipeline
IDs, task IDs are validated before use. No action needed.  
**Status:** Confirmed adequate.

### SEC-017: No authentication on any API endpoint
**Location:** `internal/api/server.go`  
**Severity:** Informational  
**Description:** All endpoints are publicly accessible. Anyone with network access
can submit tasks, read task data, and observe real-time events.  
**Recommendation:** Add authentication before production deployment.  
**Status:** Accepted gap — TODO for pre-production.

---

## Second Security Audit Findings (SEC2- prefix)

### SEC2-001: OTLP exporter uses WithInsecure unconditionally
**Location:** `internal/tracing/tracing.go`  
**Severity:** High  
**Status:** Resolved  
**Resolution:** OTLP exporter now defaults to TLS. Added `otlp_insecure` config field
to explicitly opt in to plaintext (for local collectors). Added `otlp_headers_env` field
that reads auth headers from an environment variable. Credentials never appear in YAML.

### SEC2-002: /metrics endpoint subject to rate limiter
**Location:** `internal/api/server.go`, `internal/api/middleware.go`  
**Severity:** High  
**Status:** Resolved  
**Resolution:** `/metrics` path is now exempt from the per-IP rate limiter, preventing
attacker traffic from blocking Prometheus scrapes. Added documentation that `/metrics`
should be protected by network-level ACLs in production.

### SEC2-003: cancel command TOCTOU race
**Location:** `cmd/orcastrator/main.go` — `cancelTask()`  
**Severity:** Medium  
**Description:** `cancelTask()` performs a read-then-write. Between GetTask and
UpdateTask, the broker can complete the task, and cancel will overwrite the DONE state.  
**Recommendation:** Add `CancelTask` to the Store interface with atomic
`UPDATE WHERE state NOT IN ('DONE', 'FAILED')`.

### SEC2-004: trace_id and span_id not in reservedMetadataKeys
**Location:** `internal/broker/broker.go`  
**Severity:** Medium  
**Status:** Resolved  
**Resolution:** Added `trace_id` and `span_id` to `reservedMetadataKeys` so agents
cannot overwrite trace correlation metadata.

### SEC2-005: Migration lacks concurrency protection against live broker
**Location:** `cmd/orcastrator/main.go` — `runMigration()`  
**Severity:** Medium  
**Description:** `migrate run` against a live pipeline with in-flight tasks can cause a
task to be processed with a pre-migration payload while migration writes the post-migration
payload. No data corruption (Postgres FOR UPDATE prevents that), but the task may be
processed with the wrong schema version.  
**Recommendation:** Document that `migrate run` should target quiesced pipelines, or add
`--require-state DONE,FAILED` filter to only migrate terminal tasks.

### SEC2-006: docker-compose uses sslmode=disable without warning
**Location:** `docs/deployment.md`  
**Severity:** Medium  
**Status:** Resolved  
**Resolution:** Added comments explaining that sslmode=disable is only safe on the Docker
bridge network. Production deployments across networks must use sslmode=require or
sslmode=verify-full.

---

## Deployment Hardening

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

## New Findings (SEC2 Verification)

### SEC2-NEW-001: YAML parse error leaks file content
**Severity:** Low-Medium
**Location:** `internal/config/config.go` — `Load()` function
**Detail:** When `orcastrator validate --config <symlink-to-sensitive-file>`
was run, the YAML parser included the file's raw content in the error
message. An attacker with local filesystem access could read arbitrary
files by symlinking them and observing the error output.
**Status:** Resolved
**Resolution:** `Load()` now rejects symlinks and non-regular files via
`os.Lstat` before reading. YAML parse errors are sanitized to strip raw
file content — only the filename and "invalid YAML structure" are returned
to callers. Full error details are logged at debug level for operators.

---

## Tracking

| # | Title | Severity | Status |
|---|-------|----------|--------|
| 1 | Broker.Reload data races | Critical | Resolved |
| 2 | Race detector not validated | Critical | Resolved |
| 3 | Hot-reload missing new stage workers | High | Resolved |
| 4 | Redis ListTasks O(N) scan | High | Resolved |
| 5 | Postgres UpdateTask not atomic | High | Resolved |
| 6 | WebSocket hub shutdown race | Medium | Resolved |
| 7 | Flaky timing-sensitive tests | Medium | Resolved |
| 8 | staticcheck ST1005 copilot stub | Low | Resolved |
| 9 | Integration tests need live services | Low | Resolved |
| SEC-007 | No ID character validation | Medium | Resolved |
| SEC-008 | ListTasks limit unbounded | Medium | Resolved |
| SEC-009 | No TLS enforcement for cloud providers | Medium | Resolved |
| SEC-010 | Predictable envelope delimiters | Medium | Open |
| SEC-011 | Go 1.25.0 stdlib vulnerabilities | Medium | Resolved |
| SEC-012 | Redis UpdateTask not atomic | Medium | Open |
| SEC-013 | Unbounded WebSocket client count | Low | Open |
| SEC-014 | Token bucket cleanup goroutine leak | Low | Open |
| SEC-015 | No DisallowUnknownFields | Informational | Accepted |
| SEC-016 | Path param validation adequate | Informational | Confirmed |
| SEC-017 | No API authentication | Informational | Accepted |
| SEC2-001 | OTLP exporter insecure by default | High | Resolved |
| SEC2-002 | /metrics rate-limited (DoS scrapes) | High | Resolved |
| SEC2-003 | cancel command TOCTOU race | Medium | Open |
| SEC2-004 | trace_id/span_id not reserved | Medium | Resolved |
| SEC2-005 | Migration lacks live broker guard | Medium | Open |
| SEC2-006 | sslmode=disable in compose example | Medium | Resolved |
| SEC2-NEW-001 | YAML parse error leaks file content | Low-Medium | Resolved |
| SEC2-NEW-002 | Metrics endpoint on shared port | Informational | Informational |
