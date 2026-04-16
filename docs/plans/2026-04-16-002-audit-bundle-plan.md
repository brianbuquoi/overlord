---
title: "fix: audit bundle — loopback-default bind, fixtures hard-reject, --force cleanup/rollback"
type: fix
status: completed
date: 2026-04-16
completed: 2026-04-16
origin: codex audit (v0.5.0) — findings #1 (bind default), #3 (fixtures fail-open), #2 (--force cleanup/rollback)
pr: https://github.com/brianbuquoi/overlord/pull/2
release: https://github.com/brianbuquoi/overlord/releases/tag/v0.6.0
---

# fix: audit bundle — loopback-default bind, fixtures hard-reject, `--force` cleanup/rollback

> **For agentic workers:** REQUIRED SUB-SKILL: Use `superpowers:subagent-driven-development` (recommended) or `superpowers:executing-plans` to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close three Codex audit findings in one bundled PR: make `overlord run` loopback-only by default and refuse non-loopback + auth-off startup unless explicitly opted in; hard-reject `fixtures:` on non-mock providers during `overlord validate`; fix the `--force` merge path to actually clean up its tempdir on success and roll back backups on mid-merge failure.

**Architecture:**
- Finding #1: new `--bind` flag on `overlord run` defaulting to `127.0.0.1`; retain `--port` as a compat shortcut that becomes `127.0.0.1:$port`. When the resolved bind is non-loopback AND `auth.enabled=false`, `runCmd` returns an error from `RunE` (not a suppressible log line) unless `--allow-public-noauth` is passed. The existing `checkAuthGuardrail` warn-only path is retained *only* for the `--allow-public-noauth` case.
- Finding #3: add `validateAgents` in `internal/config/config.go` invoked from `validate()`. Non-`mock` providers with a non-empty `fixtures:` map are rejected with a clear error naming the agent ID and provider. `overlord validate` now catches this class of footgun at config-load time.
- Finding #2: rewrite the `--force` commit tail to (a) use `os.RemoveAll(tempdir)` on success (not `os.Remove`), and (b) wrap the copy-into-target phase in a rollback: on copy failure, undo any backups (rename `.overlord-init-bak.*` back to original names) and delete any files already copied into the target. Rollback is best-effort but reported.

**Tech Stack:** Go 1.22+, cobra, slog, ginkgo-free testing (standard `testing` package), `os`/`path/filepath`/`net`.

---

## Problem Frame

Codex audited v0.5.0 after the `overlord init` scaffolder landed. Three findings materially affect the production-readiness claim in the release notes:

1. **Bind default is all-interfaces.** `cmd/overlord/main.go:380` builds `bindAddr := ":" + port`, so `overlord run` listens on every interface the firewall allows. The auth guardrail (`checkAuthGuardrail`) warns but does not refuse, and `LOG_LEVEL=error` silences the warning entirely (`newLogger()` at `:157` honors it). Combined with the scaffolded `auth:` block that ships commented-out by design (per `CLAUDE.md`), a scaffolded project run with default flags exposes an unauthenticated API on every interface. The loopback-safe cases exercised in `auth_guardrail_test.go` are not reachable from the real command path.
2. **`--force` leaks its tempdir on success and has no rollback on mid-merge failure.** `internal/scaffold/writer.go:254–262` calls `commitIntoExistingTarget(...)` (which *copies* files, not renames), then does `_ = os.Remove(tempdir)`. `os.Remove` on a non-empty directory fails silently, so every successful `--force` run leaves `.overlord-init-<hex>/` under the parent dir. If a later `copyFileExclusive` fails after backups were already renamed, earlier backups and copied files remain in place with no restoration path.
3. **`fixtures:` accepts silently on every provider.** `config.Agent.Fixtures` (`internal/config/types.go:196`) is marshaled for all providers but only consumed when `Provider=="mock"` (`internal/agent/registry/registry.go:116`). A partially migrated config can keep serving mock fixtures (stage binding still points at `*-mock`) OR make live provider calls while stale fixtures sit in the config. `overlord validate` never constructs agents, so it cannot catch this today.

## Requirements Trace

**Bind hardening (finding #1)**
- R1.1 `overlord run` accepts `--bind host[:port]` and `--port port`. `--bind` takes precedence when both are set. Default bind host is `127.0.0.1`. Default port (existing behavior) is `$OVERLORD_PORT` or `8080`. A new env var `OVERLORD_BIND` overrides `--bind`'s default when set (flag still wins over env).
- R1.2 Listen address resolution: if only `--port` is given, the listener binds to `127.0.0.1:<port>` (**breaking change from v0.5.0 which bound `:<port>` / all-interfaces**). If `--bind` is `host` (no port), the listener uses `<host>:<port>`. If `--bind` is `host:port`, the listener uses it verbatim and `--port` is ignored.
- R1.3 When the resolved bind is non-loopback AND `cfg.Auth.Enabled=false`, `runCmd` returns a `*fmt.Errorf` error from `RunE` (non-zero exit, unmissable). The error names the bind address, the missing auth, and the override flag.
- R1.4 New flag `--allow-public-noauth` (bool, default false) suppresses the refusal. When set, the existing `checkAuthGuardrail` slog.Warn is still emitted so operators who opt in see a log record. This satisfies the "local-dev users may intentionally bind to LAN" case that the original warn-only design targeted.
- R1.5 `auth.enabled=true` with non-loopback bind continues to start silently (no warning, no refusal).
- R1.6 Loopback bind with `auth.enabled=false` continues to start silently (no warning, no refusal) — this is the dev-default path.
- R1.7 Startup log line at `:346` additionally includes `"bind"` and `"allow_public_noauth"` for observability.

**Fixtures hard-reject (finding #3)**
- R3.1 `overlord validate` rejects any agent config where `len(Fixtures) > 0 && Provider != "mock"`. Error names the agent ID, the offending provider, and the fixture keys for debuggability.
- R3.2 The rejection is unconditional — no feature flag, no allow-list. A scaffolded config mid-migration that still has `fixtures:` under a flipped provider fails validation loudly.
- R3.3 `overlord run` and every other `loadConfig`-using entrypoint inherits the rejection because `config.Load` already routes through `validate()`.

**`--force` tempdir cleanup + rollback (finding #2)**
- R2.1 On a successful `--force` merge, no `.overlord-init-<hex>/` directory remains under the parent after `Write` returns. Enforced by extending `TestWrite_NoStrayTempdir` to cover both fresh and `--force` paths.
- R2.2 If `copyFileExclusive` fails mid-merge, the writer restores every completed backup (rename `<original>.overlord-init-bak.<suffix>` back to `<original>`) and deletes every file already copied into the target. Rollback is best-effort: rollback errors are attached to the returned `*WriteError` via a new `RollbackErrors []error` field but do not mask the primary copy failure.
- R2.3 Rollback is NOT invoked on pre-copy failures (collision-detection errors, MkdirAll errors, backup-suffix lookup errors). Those paths already leave the target untouched.
- R2.4 `result.Backups` is populated with the final (post-rollback) state: if rollback succeeded, `Backups` is empty; if rollback partially failed, `Backups` contains only the backups that could not be restored.

## Scope Boundaries

- **Out of scope:** reworking the auth model itself (scope stays: enable/disable + keys); introducing multi-address listen (still a single bind per process); scaffolder changes beyond writer internals; changing `--overwrite` semantics; rewriting `commitIntoExistingTarget` into an atomic two-phase commit (the rollback is best-effort, not transactional).
- **Out of scope:** Warning behavior for plugin providers with `fixtures:` set — plugins opt into their own config schema; `fixtures:` is a first-party field and only applies to the first-party `mock` adapter. Third-party plugin providers must ignore it per existing convention.
- **Out of scope:** Deprecating `--port` — it remains a supported shortcut. The `OVERLORD_PORT` env var stays honored. Removing either is a separate breaking change.

## Files Touched

- **Modify** `cmd/overlord/main.go` — runCmd flag additions (`--bind`, `--allow-public-noauth`), bind resolution, refusal logic; update the startup log line.
- **Modify** `cmd/overlord/auth_guardrail_test.go` — retain existing warn/no-warn matrix; add tests for refusal and for `--allow-public-noauth` bypass.
- **Create** `cmd/overlord/bind_test.go` — unit tests for a new `resolveBindAddr(bindFlag, portFlag string) (string, error)` helper.
- **Modify** `internal/config/config.go` — add `validateAgents` calling it from `validate()`.
- **Create** `internal/config/agent_validation_test.go` — tests for fixtures/non-mock rejection and valid-path coverage.
- **Modify** `internal/scaffold/writer.go` — fix `commitIntoExistingTarget` tempdir cleanup; add `mergeRollback` helper; widen `WriteError` with `RollbackErrors`.
- **Modify** `internal/scaffold/errors.go` (if `WriteError` lives elsewhere, confirm via Grep during Unit 3) — add `RollbackErrors` field and accessor.
- **Modify** `internal/scaffold/writer_test.go` — extend `TestWrite_NoStrayTempdir` to cover `--force`; add `TestWrite_ForceRollback_*` tests.
- **Modify** `docs/deployment.md` — rewrite the Authentication section's scaffolded-projects note to describe the refusal, `--bind`, and `--allow-public-noauth`.
- **Modify** `docs/init.md` — update any `overlord run` invocations that implicitly relied on all-interfaces default.
- **Modify** `CLAUDE.md` — update the "Scaffolded projects and the runtime auth guardrail" section to describe the hard refusal and the override flag.
- **Modify** `CHANGELOG.md` or release-notes doc — call out the breaking bind default and the new flags.

## Context & Research

### Key existing helpers (leave alone; extend, don't replace)

- `isLoopbackHost(host string) bool` and `bindHost(addr string) string` — `cmd/overlord/main.go:496` and `:516`. Correct and well-tested; reuse for the refusal decision.
- `checkAuthGuardrail(logger, cfg, bindAddr)` — `cmd/overlord/main.go:530`. Keep as-is for the `--allow-public-noauth` opt-in case (emit slog.Warn when bypass flag was passed). Do NOT call it in the refusal path — the refusal returns an error instead.
- `envOrDefault(key, fallback string) string` — `cmd/overlord/main.go:558`. Use for `OVERLORD_BIND` env resolution.
- `config.validate(cfg *Config)` — `internal/config/config.go:78`. Already the single entry point. Add a `validateAgents(cfg.Agents)` call after `validateAuth(cfg.Auth)`.
- `commitIntoExistingTarget(tempdir, target, rendered, overwrite)` — `internal/scaffold/writer.go:512`. Keep the collision-detection + backup phase; rewrap the copy phase with rollback.

### WriteError type location

`WriteError` with `.Code` is referenced at `internal/scaffold/writer.go:142`. Confirm the struct is in `internal/scaffold/errors.go` (or adjacent) via Grep before Unit 3; extend that file, not writer.go.

### Prior-art tests

- `cmd/overlord/auth_guardrail_test.go:47` — `TestCheckAuthGuardrail_NoWarning` covers loopback/auth-on cases. Keep these.
- `cmd/overlord/auth_guardrail_test.go:79` — `TestCheckAuthGuardrail_Warning` covers non-loopback warn cases. After the change, the warn path only fires when `--allow-public-noauth` is set; repurpose this test or move the non-loopback/auth-off matrix into a new refusal test.
- `internal/scaffold/writer_test.go:181` — `TestWrite_NoStrayTempdir` only covers the fresh-target path. Extend it.

---

## Implementation Units

Each unit is self-contained and committable. Recommended order: 3 → 2 → 1, because #3 is the smallest and provides a concrete `overlord validate` improvement that makes #1's test matrix more meaningful; #2 is pure scaffolder-internal; #1 is the largest and touches the most surface. But the plan is written unit-by-unit; implement in any order.

### Unit 1: Bind hardening — loopback default + non-loopback refusal

**Files:**
- Modify: `cmd/overlord/main.go` (runCmd factory, lines ~280–486; startup log ~346; run path ~380)
- Create: `cmd/overlord/bind_test.go`
- Modify: `cmd/overlord/auth_guardrail_test.go`

#### Task 1.1: Write the failing test for `resolveBindAddr`

The helper takes two flag values (`bindFlag`, `portFlag`) and an env snapshot and returns the final listen address (or an error). Loopback-default is its whole reason to exist.

- [ ] **Step 1: Create `cmd/overlord/bind_test.go` with the target matrix**

```go
package main

import (
	"testing"
)

func TestResolveBindAddr(t *testing.T) {
	cases := []struct {
		name       string
		bindFlag   string
		portFlag   string
		envBind    string
		wantAddr   string
		wantErr    bool
	}{
		{"defaults (no flags, no env)", "", "8080", "", "127.0.0.1:8080", false},
		{"port only", "", "9000", "", "127.0.0.1:9000", false},
		{"bind host only, default port", "0.0.0.0", "8080", "", "0.0.0.0:8080", false},
		{"bind host:port overrides port flag", "10.0.0.5:7777", "8080", "", "10.0.0.5:7777", false},
		{"bind host-only + non-default port", "127.0.0.1", "9090", "", "127.0.0.1:9090", false},
		{"env OVERLORD_BIND supplies host", "", "8080", "10.0.0.5", "10.0.0.5:8080", false},
		{"flag beats env", "127.0.0.1", "8080", "10.0.0.5", "127.0.0.1:8080", false},
		{"IPv6 literal", "[::1]:8080", "9090", "", "[::1]:8080", false},
		{"empty port rejected", "127.0.0.1", "", "", "", true},
		{"invalid bind host rejected", "not a host", "8080", "", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveBindAddr(tc.bindFlag, tc.portFlag, tc.envBind)
			if (err != nil) != tc.wantErr {
				t.Fatalf("resolveBindAddr(%q,%q,%q) err=%v wantErr=%v", tc.bindFlag, tc.portFlag, tc.envBind, err, tc.wantErr)
			}
			if got != tc.wantAddr {
				t.Errorf("resolveBindAddr(%q,%q,%q) = %q, want %q", tc.bindFlag, tc.portFlag, tc.envBind, got, tc.wantAddr)
			}
		})
	}
}
```

- [ ] **Step 2: Run the test to confirm it fails (function undefined)**

```bash
cd /home/minime/projects/claude/overlord
go test ./cmd/overlord -run TestResolveBindAddr -v
```

Expected: FAIL with `undefined: resolveBindAddr`.

- [ ] **Step 3: Implement `resolveBindAddr` in `cmd/overlord/main.go`**

Place directly below `bindHost` (around line 524):

```go
// resolveBindAddr computes the listen address from the --bind flag, the
// --port flag, and the OVERLORD_BIND env var snapshot. Default host is
// loopback (127.0.0.1). A --bind value of "host:port" wins over --port;
// a bare "host" is combined with the port flag. Empty port is rejected.
func resolveBindAddr(bindFlag, portFlag, envBind string) (string, error) {
	if portFlag == "" {
		return "", fmt.Errorf("port must be non-empty")
	}
	host := bindFlag
	if host == "" {
		host = envBind
	}
	if host == "" {
		host = "127.0.0.1"
	}
	// If host already contains a port, accept it verbatim.
	if h, p, err := net.SplitHostPort(host); err == nil && p != "" {
		if h == "" {
			return "", fmt.Errorf("bind host cannot be empty when port is specified")
		}
		return net.JoinHostPort(h, p), nil
	}
	// Validate bare host: either a parseable IP or a plausible hostname.
	if ip := net.ParseIP(host); ip == nil {
		if !isPlausibleHostname(host) {
			return "", fmt.Errorf("invalid bind host: %q", host)
		}
	}
	return net.JoinHostPort(host, portFlag), nil
}

// isPlausibleHostname accepts labels a–z, 0–9, '-' (not leading/trailing),
// separated by dots. Looser than RFC 1123 on length — we're rejecting
// whitespace and obvious junk, not doing DNS validation.
func isPlausibleHostname(h string) bool {
	if h == "" || len(h) > 253 {
		return false
	}
	for _, label := range strings.Split(h, ".") {
		if label == "" || len(label) > 63 {
			return false
		}
		for i, r := range label {
			switch {
			case r >= 'a' && r <= 'z':
			case r >= 'A' && r <= 'Z':
			case r >= '0' && r <= '9':
			case r == '-' && i != 0 && i != len(label)-1:
			default:
				return false
			}
		}
	}
	return true
}
```

- [ ] **Step 4: Re-run the test — expect PASS**

```bash
go test ./cmd/overlord -run TestResolveBindAddr -v
```

- [ ] **Step 5: Commit**

```bash
git add cmd/overlord/main.go cmd/overlord/bind_test.go
git commit -m "feat(cli): add resolveBindAddr helper with loopback default"
```

#### Task 1.2: Write the failing test for non-loopback + auth-off refusal

- [ ] **Step 1: Add the refusal test to `cmd/overlord/auth_guardrail_test.go`**

Append these tests (do not delete existing ones):

```go
// TestRefusePublicNoauth asserts shouldRefusePublicNoauth returns true
// exactly when the bind address is non-loopback AND auth is disabled
// AND --allow-public-noauth was not set.
func TestRefusePublicNoauth(t *testing.T) {
	cases := []struct {
		name        string
		bindAddr    string
		authEnabled bool
		allow       bool
		wantRefuse  bool
	}{
		{"loopback + auth off", "127.0.0.1:8080", false, false, false},
		{"loopback + auth on", "127.0.0.1:8080", true, false, false},
		{"public + auth on", "0.0.0.0:8080", true, false, false},
		{"public + auth off (danger)", "0.0.0.0:8080", false, false, true},
		{"public + auth off + allow", "0.0.0.0:8080", false, true, false},
		{"LAN + auth off (danger)", "10.0.0.5:8080", false, false, true},
		{"LAN + auth off + allow", "10.0.0.5:8080", false, true, false},
		{"implicit all-interfaces + auth off (danger)", ":8080", false, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{Auth: config.APIAuthConfig{Enabled: tc.authEnabled}}
			got := shouldRefusePublicNoauth(cfg, tc.bindAddr, tc.allow)
			if got != tc.wantRefuse {
				t.Errorf("shouldRefusePublicNoauth(bind=%q, authOn=%v, allow=%v) = %v, want %v",
					tc.bindAddr, tc.authEnabled, tc.allow, got, tc.wantRefuse)
			}
		})
	}
}
```

- [ ] **Step 2: Run the test to confirm failure**

```bash
go test ./cmd/overlord -run TestRefusePublicNoauth -v
```

Expected: FAIL with `undefined: shouldRefusePublicNoauth`.

- [ ] **Step 3: Implement `shouldRefusePublicNoauth` in `cmd/overlord/main.go`**

Place directly below `checkAuthGuardrail` (around line 547):

```go
// shouldRefusePublicNoauth returns true when the bind address is
// non-loopback AND auth is disabled AND the operator did not opt in
// via --allow-public-noauth. Returning true causes runCmd to fail
// startup with a clear error — this is the refusal half of the
// guardrail. Warning-only behavior remains for the opt-in case.
func shouldRefusePublicNoauth(cfg *config.Config, bindAddr string, allow bool) bool {
	if cfg == nil {
		return false
	}
	if cfg.Auth.Enabled {
		return false
	}
	if allow {
		return false
	}
	host := bindHost(bindAddr)
	if isLoopbackHost(host) {
		return false
	}
	return true
}
```

- [ ] **Step 4: Run the test — expect PASS**

```bash
go test ./cmd/overlord -run TestRefusePublicNoauth -v
```

- [ ] **Step 5: Commit**

```bash
git add cmd/overlord/main.go cmd/overlord/auth_guardrail_test.go
git commit -m "feat(cli): add shouldRefusePublicNoauth classifier"
```

#### Task 1.3: Wire the flags and refusal into `runCmd`

- [ ] **Step 1: Update the `runCmd` flag block**

Replace the existing flag block at `cmd/overlord/main.go:482-484`:

```go
	cmd.Flags().StringVar(&configPath, "config", "", "path to pipeline YAML config file")
	cmd.Flags().StringVar(&port, "port", envOrDefault("OVERLORD_PORT", "8080"), "HTTP server port")
	cmd.MarkFlagRequired("config")
	return cmd
}
```

With:

```go
	cmd.Flags().StringVar(&configPath, "config", "", "path to pipeline YAML config file")
	cmd.Flags().StringVar(&port, "port", envOrDefault("OVERLORD_PORT", "8080"), "HTTP server port (combined with --bind host when --bind has no port)")
	cmd.Flags().StringVar(&bindFlag, "bind", envOrDefault("OVERLORD_BIND", ""), "HTTP bind address (host or host:port); defaults to 127.0.0.1")
	cmd.Flags().BoolVar(&allowPublicNoauth, "allow-public-noauth", false, "explicitly allow non-loopback bind with auth disabled (requires operator opt-in)")
	cmd.MarkFlagRequired("config")
	return cmd
}
```

And add the two new vars at the top of `runCmd` (around line 283):

```go
	var configPath string
	var port string
	var bindFlag string
	var allowPublicNoauth bool
```

- [ ] **Step 2: Replace the `bindAddr := ":" + port` line with bind resolution + refusal**

At `cmd/overlord/main.go:380`, replace:

```go
	srv := api.NewServerWithContext(ctx, b, logger, m, metricsPath, authKeys)
	bindAddr := ":" + port
	// Warn once at startup if auth is off on a non-loopback bind.
	// Warn-only: local-dev users may intentionally bind to LAN.
	// Not re-invoked on SIGHUP hot-reload (startup-only).
	checkAuthGuardrail(logger, cfg, bindAddr)
	ln, err := net.Listen("tcp", bindAddr)
```

With:

```go
	srv := api.NewServerWithContext(ctx, b, logger, m, metricsPath, authKeys)
	bindAddr, err := resolveBindAddr(bindFlag, port, os.Getenv("OVERLORD_BIND"))
	if err != nil {
		cancel()
		return fmt.Errorf("bind: %w", err)
	}
	if shouldRefusePublicNoauth(cfg, bindAddr, allowPublicNoauth) {
		cancel()
		return fmt.Errorf(
			"refusing to start: bind=%s is non-loopback and auth.enabled=false — enable auth or pass --allow-public-noauth (see %s)",
			bindAddr, authGuardrailDocURL,
		)
	}
	// Warning path retained for --allow-public-noauth opt-in so operators
	// who knowingly override still see a log record. Not re-invoked on
	// SIGHUP hot-reload (startup-only).
	if allowPublicNoauth {
		checkAuthGuardrail(logger, cfg, bindAddr)
	}
	ln, err := net.Listen("tcp", bindAddr)
```

- [ ] **Step 3: Extend the startup log line**

At `cmd/overlord/main.go:346`, update:

```go
	logger.Info("overlord starting",
		"config", configPath,
		"pipelines", len(cfg.Pipelines),
		"agents", len(cfg.Agents),
		"schemas", len(cfg.SchemaRegistry),
		"store", pipelineStoreType(cfg),
		"port", port,
		"tracing_enabled", cfg.Observability.Tracing.Enabled,
	)
```

Replace `"port", port,` with:

```go
		"port", port,
		"bind", bindFlag,
		"allow_public_noauth", allowPublicNoauth,
```

- [ ] **Step 4: Run the full `cmd/overlord` test package — ensure nothing regressed**

```bash
go test ./cmd/overlord/... -race -v
```

Expected: all pass (existing `TestCheckAuthGuardrail_*` tests still pass; new `TestRefusePublicNoauth` and `TestResolveBindAddr` pass).

- [ ] **Step 5: Commit**

```bash
git add cmd/overlord/main.go
git commit -m "feat(cli): loopback-default bind, refuse non-loopback+noauth

Adds --bind and --allow-public-noauth flags. The default listen address
is now 127.0.0.1:<port>, replacing the v0.5.0 all-interfaces default.
overlord run refuses to start when bind is non-loopback and auth is
disabled unless --allow-public-noauth is explicitly set.

Closes Codex audit finding #1."
```

#### Task 1.4: Update existing tests for the warn/refuse split

- [ ] **Step 1: Retarget `TestCheckAuthGuardrail_Warning` to cover only the opt-in case**

The existing matrix in `cmd/overlord/auth_guardrail_test.go:79-122` asserts `checkAuthGuardrail` emits a warning for non-loopback + auth-off. That semantic is now gated on `--allow-public-noauth=true`. Keep the test as-is — `checkAuthGuardrail` itself is unchanged; only its caller (runCmd) now gates it. Document this by appending a comment above the test:

```go
// TestCheckAuthGuardrail_Warning asserts the warn-emit behavior. Note
// runCmd now only invokes this function when --allow-public-noauth is
// set; the warn path is the opt-in case, not the default. See
// TestRefusePublicNoauth for the default-refuse matrix.
func TestCheckAuthGuardrail_Warning(t *testing.T) {
```

- [ ] **Step 2: Run the full package tests again**

```bash
go test ./cmd/overlord/... -race -v
```

Expected: all pass.

- [ ] **Step 3: Commit**

```bash
git add cmd/overlord/auth_guardrail_test.go
git commit -m "test(cli): document warn-opt-in vs refuse-default split"
```

---

### Unit 2: `fixtures:` hard-reject on non-mock providers

**Files:**
- Modify: `internal/config/config.go`
- Create: `internal/config/agent_validation_test.go`

#### Task 2.1: Write the failing test for fixtures-on-non-mock rejection

- [ ] **Step 1: Create `internal/config/agent_validation_test.go`**

```go
package config

import (
	"strings"
	"testing"
)

func TestValidateAgents_FixturesOnMockAllowed(t *testing.T) {
	cfg := &Config{
		Agents: []Agent{
			{
				ID:       "reviewer-mock",
				Provider: "mock",
				Fixtures: map[string]string{"review": "fixtures/review.json"},
			},
		},
	}
	if err := validateAgents(cfg.Agents); err != nil {
		t.Fatalf("expected no error for mock+fixtures; got %v", err)
	}
}

func TestValidateAgents_FixturesOnNonMockRejected(t *testing.T) {
	cases := []string{"anthropic", "openai", "openai-responses", "google", "ollama", "copilot", "plugin", "some-plugin"}
	for _, provider := range cases {
		t.Run(provider, func(t *testing.T) {
			cfg := &Config{
				Agents: []Agent{
					{
						ID:       "reviewer",
						Provider: provider,
						Fixtures: map[string]string{"review": "fixtures/review.json"},
					},
				},
			}
			err := validateAgents(cfg.Agents)
			if err == nil {
				t.Fatalf("expected error for provider=%q with fixtures; got nil", provider)
			}
			if !strings.Contains(err.Error(), "reviewer") {
				t.Errorf("expected error to name agent id; got %q", err.Error())
			}
			if !strings.Contains(err.Error(), provider) {
				t.Errorf("expected error to name provider; got %q", err.Error())
			}
			if !strings.Contains(err.Error(), "fixtures") {
				t.Errorf("expected error to mention fixtures; got %q", err.Error())
			}
		})
	}
}

func TestValidateAgents_NoFixturesNoError(t *testing.T) {
	// Non-mock provider with no fixtures is the normal case.
	cfg := &Config{
		Agents: []Agent{
			{ID: "reviewer", Provider: "anthropic"},
		},
	}
	if err := validateAgents(cfg.Agents); err != nil {
		t.Fatalf("expected no error; got %v", err)
	}
}

func TestValidateAgents_EmptyFixturesMapAllowed(t *testing.T) {
	// An empty (but non-nil) fixtures map should be treated as "no fixtures".
	cfg := &Config{
		Agents: []Agent{
			{ID: "reviewer", Provider: "anthropic", Fixtures: map[string]string{}},
		},
	}
	if err := validateAgents(cfg.Agents); err != nil {
		t.Fatalf("expected no error for empty fixtures map; got %v", err)
	}
}
```

- [ ] **Step 2: Run the test — expect failure on undefined symbol**

```bash
cd /home/minime/projects/claude/overlord
go test ./internal/config -run TestValidateAgents -v
```

Expected: FAIL with `undefined: validateAgents`.

- [ ] **Step 3: Implement `validateAgents` and wire it into `validate`**

Add to `internal/config/config.go` directly below `validateAuth` (around line 355):

```go
// validateAgents enforces cross-field invariants on agent configs that
// the per-field tag parsing cannot express. Currently:
//
//   - fixtures: is only meaningful for provider: mock. Any other
//     provider with a non-empty fixtures map is rejected at config-load
//     time so a half-finished migration cannot silently ship — the
//     alternative (log at debug and ignore) hid fail-open behavior in
//     both directions (Codex audit finding #3).
func validateAgents(agents []Agent) error {
	for _, a := range agents {
		if len(a.Fixtures) == 0 {
			continue
		}
		if a.Provider == "mock" {
			continue
		}
		keys := make([]string, 0, len(a.Fixtures))
		for k := range a.Fixtures {
			keys = append(keys, k)
		}
		return fmt.Errorf(
			"agent %q: fixtures: is only supported on provider: mock (got provider: %q); remove the fixtures map or switch the provider back to mock (stage keys: %v)",
			a.ID, a.Provider, keys,
		)
	}
	return nil
}
```

And update `validate(cfg *Config)` at line 78 — add after the existing `validateAuth` call (around line 89):

```go
	if err := validateAuth(cfg.Auth); err != nil {
		return err
	}
	if err := validateAgents(cfg.Agents); err != nil {
		return err
	}
```

- [ ] **Step 4: Run the tests — expect PASS**

```bash
go test ./internal/config -run TestValidateAgents -v
```

- [ ] **Step 5: Run the full config package to catch regressions**

```bash
go test ./internal/config -race -v
```

Expected: all pass.

- [ ] **Step 6: Commit**

```bash
git add internal/config/config.go internal/config/agent_validation_test.go
git commit -m "feat(config): reject fixtures on non-mock providers

Closes Codex audit finding #3. A partially migrated config that leaves
fixtures: under a flipped provider now fails overlord validate rather
than silently ignoring the fixtures (or silently serving mock data
when the stage still binds to a mock agent ID)."
```

#### Task 2.2: Smoke-test the full CLI validate path

- [ ] **Step 1: Build the CLI and create a deliberately-bad config**

```bash
cd /home/minime/projects/claude/overlord
go build -o /tmp/overlord-smoke ./cmd/overlord
mkdir -p /tmp/overlord-smoke-cfg
cat > /tmp/overlord-smoke-cfg/overlord.yaml <<'EOF'
version: "1"
schema_registry: []
pipelines: []
agents:
  - id: reviewer
    provider: anthropic
    fixtures:
      review: fixtures/review.json
EOF
```

- [ ] **Step 2: Run `overlord validate` and confirm rejection**

```bash
/tmp/overlord-smoke validate --config /tmp/overlord-smoke-cfg/overlord.yaml
echo "exit=$?"
```

Expected: non-zero exit; stderr contains `agent "reviewer": fixtures: is only supported on provider: mock (got provider: "anthropic")`.

- [ ] **Step 3: Flip to provider: mock and confirm it validates**

```bash
sed -i 's/provider: anthropic/provider: mock/' /tmp/overlord-smoke-cfg/overlord.yaml
/tmp/overlord-smoke validate --config /tmp/overlord-smoke-cfg/overlord.yaml
echo "exit=$?"
```

Expected: exit 0 (or the existing unrelated error about missing schemas/stages — the fixtures check is satisfied; other validators may still complain but NOT about fixtures).

- [ ] **Step 4: Clean up**

```bash
rm -rf /tmp/overlord-smoke-cfg /tmp/overlord-smoke
```

No commit needed — smoke test only.

---

### Unit 3: `--force` tempdir cleanup + merge rollback

**Files:**
- Modify: `internal/scaffold/writer.go`
- Modify: `internal/scaffold/errors.go` (confirm location via Grep first)
- Modify: `internal/scaffold/writer_test.go`

#### Task 3.1: Confirm `WriteError` location

- [ ] **Step 1: Find the struct definition**

```bash
cd /home/minime/projects/claude/overlord
```

Then Grep:

```
pattern: "type WriteError"
path: internal/scaffold
output_mode: content
-n: true
```

- [ ] **Step 2: Note the file path for Task 3.3**

Record it below this line in your working notes:

```
WriteError defined at: <fill in — likely internal/scaffold/errors.go>
```

No commit.

#### Task 3.2: Write failing test for tempdir cleanup on `--force`

- [ ] **Step 1: Extend `TestWrite_NoStrayTempdir` in `internal/scaffold/writer_test.go`**

Replace the existing test body (around line 181) with a table-driven version that covers both paths:

```go
// TestWrite_NoStrayTempdir asserts no .overlord-init-<hex> directory
// remains in the parent after a successful commit — tempdir must be
// renamed-in-place (fresh target) or fully cleaned up (--force merge).
func TestWrite_NoStrayTempdir(t *testing.T) {
	cases := []struct {
		name    string
		prefill func(t *testing.T, target string)
		opts    Options
	}{
		{
			name:    "fresh target",
			prefill: func(*testing.T, string) {},
			opts:    Options{},
		},
		{
			name: "--force merge into pre-existing empty dir",
			prefill: func(t *testing.T, target string) {
				if err := os.MkdirAll(target, 0o755); err != nil {
					t.Fatalf("prefill mkdir: %v", err)
				}
				// Pre-populate with a sentinel file so the dir is not empty.
				if err := os.WriteFile(filepath.Join(target, "unrelated.txt"), []byte("x"), 0o644); err != nil {
					t.Fatalf("prefill write: %v", err)
				}
			},
			opts: Options{Force: true},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			parent := t.TempDir()
			target := filepath.Join(parent, "hello")
			tc.prefill(t, target)

			if _, err := Write(context.Background(), "hello", target, tc.opts); err != nil {
				t.Fatalf("Write: %v", err)
			}
			entries, err := os.ReadDir(parent)
			if err != nil {
				t.Fatalf("read parent: %v", err)
			}
			for _, e := range entries {
				if strings.HasPrefix(e.Name(), tempdirPrefix) {
					t.Errorf("stray tempdir in parent: %s", e.Name())
				}
			}
		})
	}
}
```

- [ ] **Step 2: Run the test — expect the `--force` case to fail**

```bash
go test ./internal/scaffold -run TestWrite_NoStrayTempdir -v
```

Expected: `fresh target` passes, `--force merge into pre-existing empty dir` FAILS with a stray `.overlord-init-<hex>` entry.

- [ ] **Step 3: Fix `commitIntoExistingTarget` call site in `Write`**

In `internal/scaffold/writer.go` at line 261, replace:

```go
		// Tempdir is drained by commitIntoExistingTarget; remove the
		// now-empty husk. Best-effort — a leftover empty dir is harmless.
		_ = os.Remove(tempdir)
		committed = true
```

With:

```go
		// commitIntoExistingTarget copies files (not renames) so the
		// tempdir still contains the full rendered tree after a
		// successful merge. Use RemoveAll; Remove silently failed on
		// non-empty dirs and leaked the tempdir (Codex audit finding #2).
		if err := os.RemoveAll(tempdir); err != nil {
			// Committed successfully — a leftover tempdir is ugly but
			// not a correctness failure. Log via the returned result
			// rather than aborting the commit.
			result.CleanupWarnings = append(result.CleanupWarnings, fmt.Sprintf("remove tempdir %s: %v", tempdir, err))
		}
		committed = true
```

If `Result` does not yet have `CleanupWarnings`, add it. Open `internal/scaffold/writer.go` and find the `Result` struct (Grep: `type Result`). Add:

```go
type Result struct {
	Target          string
	Backups         []Backup
	CleanupWarnings []string // post-commit best-effort failures (e.g. tempdir removal)
}
```

If the existing `Result` has other fields, preserve them — just append `CleanupWarnings`.

- [ ] **Step 4: Re-run the test — expect PASS**

```bash
go test ./internal/scaffold -run TestWrite_NoStrayTempdir -v
```

- [ ] **Step 5: Commit**

```bash
git add internal/scaffold/writer.go internal/scaffold/writer_test.go
git commit -m "fix(scaffold): clean up tempdir after --force merge

os.Remove silently fails on non-empty dirs, so every successful --force
run leaked .overlord-init-<hex>/ into the parent. Switch to RemoveAll
and attach any cleanup failure to Result.CleanupWarnings rather than
masking the successful commit.

Closes Codex audit finding #2 (first half)."
```

#### Task 3.3: Write failing test for rollback on mid-merge copy failure

Rollback is harder to exercise because `copyFileExclusive` rarely fails once backups have succeeded. Inject a failure by pre-creating a directory at one of the destination file paths — `openFileNoFollowCreate` will fail with `EISDIR`.

- [ ] **Step 1: Add the rollback test to `internal/scaffold/writer_test.go`**

Append:

```go
// TestWrite_ForceRollback_CopyFails asserts that when a copy fails
// after backups have been performed, backups are restored to their
// original names and any files already copied are removed. The target
// directory ends up byte-identical to its pre-Write state (modulo any
// directories the writer created along the way).
func TestWrite_ForceRollback_CopyFails(t *testing.T) {
	parent := t.TempDir()
	target := filepath.Join(parent, "hello")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatalf("mkdir target: %v", err)
	}

	// Pre-populate target with a file whose name collides with a
	// template output (overlord.yaml ships in every template).
	originalContent := []byte("ORIGINAL\n")
	yamlPath := filepath.Join(target, "overlord.yaml")
	if err := os.WriteFile(yamlPath, originalContent, 0o644); err != nil {
		t.Fatalf("prefill yaml: %v", err)
	}

	// Inject a failure: pre-create a DIRECTORY at the path where the
	// writer will try to copy a file. Pick a template file that is
	// not the first one copied. "schemas/" is a subdir in every
	// template; "schemas/hello_input.json" is a file the writer will
	// try to create.
	schemasDir := filepath.Join(target, "schemas")
	if err := os.MkdirAll(schemasDir, 0o755); err != nil {
		t.Fatalf("mkdir schemas: %v", err)
	}
	poisonPath := filepath.Join(schemasDir, "hello_input.json")
	if err := os.Mkdir(poisonPath, 0o755); err != nil {
		t.Fatalf("mkdir poison: %v", err)
	}

	_, err := Write(context.Background(), "hello", target, Options{Force: true, Overwrite: true})
	if err == nil {
		t.Fatalf("expected copy failure; got nil")
	}

	// Rollback check: the original overlord.yaml content must be
	// restored (backup renamed back).
	got, readErr := os.ReadFile(yamlPath)
	if readErr != nil {
		t.Fatalf("read target yaml after rollback: %v", readErr)
	}
	if !bytes.Equal(got, originalContent) {
		t.Errorf("rollback did not restore original overlord.yaml\nwant: %q\n got: %q", originalContent, got)
	}

	// Rollback check: no leftover .overlord-init-bak.* files.
	bakMatches, globErr := filepath.Glob(filepath.Join(target, "*.overlord-init-bak.*"))
	if globErr != nil {
		t.Fatalf("glob backups: %v", globErr)
	}
	if len(bakMatches) != 0 {
		t.Errorf("rollback left backup files behind: %v", bakMatches)
	}
}
```

Make sure `"bytes"` is imported.

- [ ] **Step 2: Run the test — expect FAIL (no rollback yet)**

```bash
go test ./internal/scaffold -run TestWrite_ForceRollback_CopyFails -v
```

Expected: FAIL — either the test's restoration check reports the original content was not restored, OR backup files are left behind.

- [ ] **Step 3: Refactor `commitIntoExistingTarget` to wrap the copy phase with rollback**

In `internal/scaffold/writer.go`, extract the copy phase into a helper that tracks progress and rolls back on failure. Replace the `// Move rendered files into target.` block at `writer.go:593-612` with:

```go
	// Move rendered files into target, with rollback on any copy
	// failure: restore the backups we just created and delete any
	// files we already copied. Rollback is best-effort and reported
	// via RollbackErrors on the returned WriteError.
	copied, copyErr := copyRenderedFiles(tempdir, target, rendered)
	if copyErr != nil {
		rbErrs := rollbackMerge(target, backups, copied)
		we := copyErr // already *WriteError via newWriteFailure
		if wrapped, ok := we.(*WriteError); ok {
			wrapped.RollbackErrors = rbErrs
		}
		return nil, we
	}

	return backups, nil
}

// copyRenderedFiles copies every non-directory entry in rendered from
// tempdir into target. Returns the ordered list of destination files
// it successfully created so the caller can roll back on error.
func copyRenderedFiles(tempdir, target string, rendered []string) ([]string, error) {
	copied := make([]string, 0, len(rendered))
	for _, rel := range rendered {
		src := filepath.Join(tempdir, rel)
		dst := filepath.Join(target, rel)
		srcFi, err := os.Lstat(src)
		if err != nil {
			return copied, newWriteFailure(fmt.Sprintf("lstat tempdir entry %s", src), err)
		}
		if srcFi.IsDir() {
			continue
		}
		if err := copyFileExclusive(src, dst); err != nil {
			return copied, err
		}
		copied = append(copied, dst)
	}
	return copied, nil
}

// rollbackMerge reverses the backups-then-copy sequence on failure.
// Files already copied into target are removed; every backup is
// renamed back to its original name. All errors are accumulated and
// returned; rollback continues past individual failures so partial
// success is possible.
func rollbackMerge(target string, backups []Backup, copied []string) []error {
	var errs []error
	// 1. Remove the copies we made. If a copy is missing we already
	//    failed to create it — not a rollback failure.
	for _, dst := range copied {
		if err := os.Remove(dst); err != nil && !os.IsNotExist(err) {
			errs = append(errs, fmt.Errorf("remove copied file %s: %w", dst, err))
		}
	}
	// 2. Rename each backup back to the original name. The copy phase
	//    may have overwritten the original (O_EXCL means it won't, but
	//    guard anyway): if the original path still exists, remove it
	//    first.
	for _, b := range backups {
		origPath := filepath.Join(target, b.Original)
		bakPath := filepath.Join(target, b.Backup)
		if _, err := os.Lstat(origPath); err == nil {
			if err := os.Remove(origPath); err != nil {
				errs = append(errs, fmt.Errorf("clear path for backup restore %s: %w", origPath, err))
				continue
			}
		}
		if err := os.Rename(bakPath, origPath); err != nil {
			errs = append(errs, fmt.Errorf("restore backup %s -> %s: %w", bakPath, origPath, err))
		}
	}
	return errs
}
```

- [ ] **Step 4: Add `RollbackErrors` to `WriteError`**

Open the file confirmed in Task 3.1 (likely `internal/scaffold/errors.go`). Add to the `WriteError` struct:

```go
type WriteError struct {
	Code           int
	Msg            string
	Err            error
	RollbackErrors []error // populated when commit failed and rollback was attempted
}
```

If the struct has different field names, preserve them — just append `RollbackErrors []error`.

- [ ] **Step 5: Run the rollback test — expect PASS**

```bash
go test ./internal/scaffold -run TestWrite_ForceRollback_CopyFails -v
```

- [ ] **Step 6: Run the full scaffold package to catch regressions**

```bash
go test ./internal/scaffold -race -v
```

Expected: all pass.

- [ ] **Step 7: Commit**

```bash
git add internal/scaffold/writer.go internal/scaffold/errors.go internal/scaffold/writer_test.go
git commit -m "fix(scaffold): roll back --force merge when copy fails mid-run

A mid-merge copyFileExclusive failure used to leave backups renamed
and a partial set of new files in place, with no automatic
restoration. Now the writer tracks every file it copies and, on
failure, restores each backup to its original name and removes any
files already copied. Rollback is best-effort and reported via
WriteError.RollbackErrors so the caller can surface partial failures.

Closes Codex audit finding #2 (second half)."
```

---

### Unit 4: Docs + release notes

**Files:**
- Modify: `docs/deployment.md`
- Modify: `docs/init.md`
- Modify: `CLAUDE.md`
- Modify: `CHANGELOG.md` (or release-notes file)

#### Task 4.1: Update `docs/deployment.md` Authentication section

- [ ] **Step 1: Rewrite the scaffolded-projects callout at `docs/deployment.md:313-322`**

Replace the existing blockquote with:

```markdown
> **Scaffolded projects:** `overlord init` generates projects with the
> memory store and a commented `auth:` block so the first-run demo works
> without credentials. The `overlord run` default bind is `127.0.0.1`
> (loopback-only), so a scaffolded project is not exposed outside the
> host. If you pass `--bind` to select a non-loopback address while
> `auth.enabled=false`, `overlord run` **refuses to start** unless you
> also pass `--allow-public-noauth`. This makes the "commented auth +
> public bind" footgun unreachable by default. See
> [docs/init.md](init.md) for the full graduation path (swap memory →
> Redis/Postgres, uncomment the `auth:` block, add real keys, then drop
> the override flag).
```

- [ ] **Step 2: Append a short "Bind address" subsection after the callout and before the "Enabling auth" subsection**

Insert:

```markdown
### Bind address

Use `--bind` to control which address `overlord run` listens on.
Accepts either a bare host (`--bind 0.0.0.0`) combined with `--port`,
or a full `host:port` string (`--bind 10.0.0.5:8080`). Defaults to
`127.0.0.1`. The `OVERLORD_BIND` environment variable sets the flag
default.

```bash
# Loopback-only (default) — safest; external clients cannot reach the API.
overlord run --config overlord.yaml

# Listen on all interfaces. Requires auth.enabled=true OR --allow-public-noauth.
overlord run --config overlord.yaml --bind 0.0.0.0

# Opt-in to the dangerous combo (bind public + auth off). Logs a warning.
overlord run --config overlord.yaml --bind 0.0.0.0 --allow-public-noauth
```

When `--bind` resolves to a non-loopback address and `auth.enabled=false`,
startup fails with:

> refusing to start: bind=<addr> is non-loopback and auth.enabled=false —
> enable auth or pass --allow-public-noauth
```

- [ ] **Step 3: Commit**

```bash
git add docs/deployment.md
git commit -m "docs(deployment): document bind default and public-noauth refusal"
```

#### Task 4.2: Update `docs/init.md` graduation path

- [ ] **Step 1: Read the current graduation section to find the exact insertion point**

```bash
```

Grep:

```
pattern: "graduation"
path: docs/init.md
output_mode: content
-n: true
context: 5
```

- [ ] **Step 2: Add a bullet to the graduation checklist reminding operators about `--bind`**

In the graduation-path checklist, add (preserving surrounding bullets):

```markdown
- When you start binding to a non-loopback address (e.g. moving from
  `overlord run` on laptop to a server), enable `auth:` first. `overlord
  run` refuses to start with a non-loopback bind + `auth.enabled=false`
  unless you pass `--allow-public-noauth` as an explicit opt-in. See
  [docs/deployment.md#bind-address](deployment.md#bind-address).
```

- [ ] **Step 3: Commit**

```bash
git add docs/init.md
git commit -m "docs(init): add bind-address step to graduation path"
```

#### Task 4.3: Update `CLAUDE.md` guardrail section

- [ ] **Step 1: Rewrite the "Scaffolded projects and the runtime auth guardrail" paragraph**

In `CLAUDE.md`, replace the existing paragraph with:

```markdown
### Scaffolded projects and the runtime auth guardrail

`overlord init` scaffolds projects with memory store + commented `auth:`
block so the first-run demo is zero-friction. Two layers make the
commented-auth pattern safe by default:

1. **Loopback-default bind.** `overlord run` defaults to `127.0.0.1`,
   so a freshly scaffolded project is never exposed outside the host
   regardless of firewall state. Use `--bind host[:port]` to select a
   different address.
2. **Refuse non-loopback + no-auth.** When the resolved bind is
   non-loopback AND `auth.enabled=false`, `overlord run` refuses to
   start unless `--allow-public-noauth` is explicitly passed. The
   existing `slog.Warn` guardrail (`checkAuthGuardrail`) is retained
   for the opt-in case so operators who deliberately override still see
   a log record.

Together these remove the "commented auth + public bind" footgun at
runtime. See `docs/deployment.md#authentication` and
`docs/init.md` for the graduation path.
```

And update the "What NOT to do" entry:

```markdown
- Do not silently weaken the runtime auth refusal in `overlord run` —
  the hard error for non-loopback + auth-off bind is the only thing
  catching the scaffolded "commented auth + public bind" footgun at
  runtime. The slog.Warn guardrail only runs on the `--allow-public-noauth`
  opt-in path.
```

- [ ] **Step 2: Commit**

```bash
git add CLAUDE.md
git commit -m "docs(claude): update guardrail description for refusal + bind flag"
```

#### Task 4.4: Update the changelog

- [ ] **Step 1: Locate the changelog**

```bash
ls /home/minime/projects/claude/overlord/CHANGELOG.md 2>/dev/null || ls /home/minime/projects/claude/overlord/docs/CHANGELOG.md 2>/dev/null || find /home/minime/projects/claude/overlord -name 'CHANGELOG*' -not -path '*/node_modules/*' -not -path '*/.git/*'
```

- [ ] **Step 2: Prepend a new entry for v0.5.1 (or the next version)**

```markdown
## [Unreleased] — audit bundle

### Security & breaking

- **Breaking:** `overlord run` now defaults to `--bind 127.0.0.1` (loopback
  only). Previous behavior was all-interfaces (`:$port`). Deployments that
  relied on external reachability must add `--bind 0.0.0.0` (or a specific
  host) and enable `auth:`.
- **Refuse non-loopback + auth-off startup.** `overlord run` exits with
  an error when the resolved bind is non-loopback and `auth.enabled=false`,
  unless `--allow-public-noauth` is passed. Closes Codex audit finding #1.
- **Reject `fixtures:` on non-mock providers.** `overlord validate` (and
  `overlord run` which routes through the same loader) now fails with a
  clear error if an agent has `fixtures:` set while `provider:` is not
  `mock`. Closes Codex audit finding #3.

### Fixed

- `overlord init --force` no longer leaves a `.overlord-init-<hex>/` tempdir
  under the parent directory after a successful merge. Closes Codex audit
  finding #2 (first half).
- `overlord init --force` now rolls back backups and removes partially-copied
  files when a copy fails mid-merge. Rollback errors are surfaced via
  `WriteError.RollbackErrors`. Closes Codex audit finding #2 (second half).

### Added

- `overlord run --bind host[:port]` flag (default `127.0.0.1`). Accepts bare
  host + existing `--port`, or a full `host:port` string.
- `overlord run --allow-public-noauth` flag. Required when binding to a
  non-loopback address while `auth.enabled=false`.
- `OVERLORD_BIND` environment variable (overrides `--bind` default).
```

If no changelog exists, create one at repo root with the v0.5.0 history you already have in release notes as the prior entry.

- [ ] **Step 3: Commit**

```bash
git add CHANGELOG.md
git commit -m "docs(changelog): add audit-bundle entry"
```

---

### Unit 5: Pre-PR validation

#### Task 5.1: Full test + vet + staticcheck

- [ ] **Step 1: Run the project's `make check` target**

```bash
cd /home/minime/projects/claude/overlord
make check
```

Expected: all tests pass, `go vet` clean, `staticcheck` clean.

- [ ] **Step 2: Run the integration suite if Docker is available**

```bash
make test-integration
```

Expected: pass (or skip with a clear reason if Docker is unavailable — record the reason in the PR description).

- [ ] **Step 3: Smoke-test the end-to-end scenarios**

```bash
go build -o /tmp/overlord-smoke ./cmd/overlord

# 1. Default run on a scaffolded project — expect successful loopback startup.
cd /tmp && /tmp/overlord-smoke init hello --non-interactive
cd /tmp/hello
/tmp/overlord-smoke run --config overlord.yaml &
RUN_PID=$!
sleep 2
curl -sS http://127.0.0.1:8080/v1/health || echo "loopback health check failed"
kill $RUN_PID 2>/dev/null; wait $RUN_PID 2>/dev/null

# 2. --bind 0.0.0.0 with commented auth — expect refusal.
/tmp/overlord-smoke run --config overlord.yaml --bind 0.0.0.0 --port 8081
echo "exit=$?"   # non-zero

# 3. --bind 0.0.0.0 with --allow-public-noauth — expect warning, then running.
/tmp/overlord-smoke run --config overlord.yaml --bind 0.0.0.0 --port 8082 --allow-public-noauth &
RUN_PID=$!
sleep 2
kill $RUN_PID 2>/dev/null; wait $RUN_PID 2>/dev/null

# 4. Cleanup.
cd /
rm -rf /tmp/hello /tmp/overlord-smoke
```

Expected: scenario 1 returns a health response; scenario 2 exits non-zero with the refusal message; scenario 3 logs the warning and runs normally until killed.

- [ ] **Step 4: No commit — smoke test only**

#### Task 5.2: Self-review checklist

- [ ] Re-read the diff end-to-end.
- [ ] Confirm no commit introduces `os.Exit` inside a cobra `RunE`.
- [ ] Confirm no hardcoded API keys or model names slipped in.
- [ ] Confirm every new file has tests (writer.go / main.go additions are covered in Tasks 1.x, 2.x, 3.x).
- [ ] Confirm the bind-default and fixtures-reject behaviors are documented in `docs/deployment.md`, `docs/init.md`, `CLAUDE.md`, and `CHANGELOG.md`.
- [ ] Confirm `KNOWN_GAPS.md` does not need an update (this fix closes external audit findings, not tracked internal items).

---

## Self-Review Notes

1. **Spec coverage check.** Each Codex finding maps to one Unit: #1 → Unit 1, #3 → Unit 2, #2 → Unit 3. Unit 4 and Unit 5 are docs and validation. No finding is left uncovered.

2. **Placeholder scan.** Every code step shows the actual code or test it produces. The single intentional lookup (Task 3.1 WriteError location) is bounded and has a concrete Grep to run.

3. **Type consistency check.** `resolveBindAddr(bindFlag, portFlag, envBind string)`, `shouldRefusePublicNoauth(cfg, bindAddr, allow)`, `validateAgents(agents []Agent)`, `copyRenderedFiles(tempdir, target, rendered) ([]string, error)`, `rollbackMerge(target, backups, copied) []error`, `WriteError.RollbackErrors []error`, `Result.CleanupWarnings []string` — names used consistently across task bodies and test code. Field name `authGuardrailDocURL` matches its existing definition at `cmd/overlord/main.go:491`.

4. **Breaking-change call-outs.** The bind-default change is breaking; it is called out in the changelog entry and in `CLAUDE.md`. Deployments relying on external reachability see a refusal (not a silent bind change), so the failure mode is loud.

5. **Rollback scope.** The rollback is best-effort by design. The plan does not try to make `commitIntoExistingTarget` atomic — doing so would require staging backups in a separate tempdir and a two-phase commit, which is a larger rewrite than the audit finding requires.

## Execution Notes

Each Unit commits independently. Recommended execution order: Unit 3 → Unit 2 → Unit 1 → Unit 4 → Unit 5. The unit-ordering is independent of task numbering inside a unit.

Total commits expected: ~12 (2 per unit 1-3, 4 for unit 4, 0 for unit 5 validation).
