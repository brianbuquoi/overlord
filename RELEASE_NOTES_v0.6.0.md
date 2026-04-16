# Overlord v0.6.0

Audit bundle release. Closes three Codex audit findings against v0.5.0 and
ships one breaking default-change to harden production readiness for the
scaffolder-driven onboarding path.

## Highlights

### BREAKING — `overlord run` defaults to loopback

`overlord run` now binds `127.0.0.1:<port>` by default. v0.5.0 bound
`:<port>` (all interfaces), which combined with the commented-out `auth:`
block in scaffolded projects to expose an unauthenticated API on every
interface the host/firewall allowed.

Two layers of protection land together:

1. **Loopback default.** Nothing off-host can reach the API until you
   explicitly select a non-loopback address via `--bind` or `OVERLORD_BIND`.
2. **Refusal on dangerous combos.** `overlord run` exits with an error
   when the resolved bind is non-loopback and `auth.enabled=false`, unless
   you pass `--allow-public-noauth` as an explicit opt-in. The existing
   `slog.Warn` guardrail remains for the opt-in case so deliberate
   overrides still produce a log record.

**Migration for external reachability:**

```bash
# v0.5.0 behavior (no longer the default)
overlord run --config overlord.yaml --bind 0.0.0.0

# ... now requires one of:
#   (a) auth.enabled=true in the config (recommended)
#   (b) --allow-public-noauth (explicit opt-in with warning)
```

See [`docs/deployment.md#bind-address`](./docs/deployment.md#bind-address).

### New flags and env var

| Flag / env | Purpose |
|------------|---------|
| `--bind host[:port]` | Listen address. Default `127.0.0.1`. Accepts bare host, `host:port`, or bracketed/bare IPv6. |
| `--allow-public-noauth` | Opt-in override for non-loopback bind with auth disabled. |
| `OVERLORD_BIND` | Environment default for `--bind` (flag still wins). |

### Config validation catches stale `fixtures:` blocks

`overlord validate` (and every other path that calls `config.Load`) now
rejects any agent whose `provider` is not `mock` while `fixtures:` is set.
The previous behavior silently ignored `fixtures:` on non-mock providers,
which could make a partially migrated config either serve mock data in
production or make live-provider calls with stale fixture files sitting in
the YAML. The rejection names the agent ID, offending provider, and
fixture stage keys so the fix is obvious.

### `overlord init --force` cleanup and rollback

Two bugs in the scaffolder's `--force` merge path are fixed:

- **Tempdir leak.** Every successful `--force` run used to leave a
  `.overlord-init-<hex>/` directory in the parent (the copy path doesn't
  drain the tempdir, and `os.Remove` silently failed on non-empty dirs).
  Now `os.RemoveAll` handles it; any residual cleanup failure surfaces via
  the new `Result.CleanupWarnings` field instead of being swallowed.
- **No rollback on mid-merge failure.** If a file copy failed after
  backups were renamed, earlier backups and any already-copied files used
  to remain in place with no restoration path. The copy phase is now
  wrapped with a best-effort rollback: renamed backups are restored and
  partial copies are removed. Rollback errors surface via the new
  `WriteError.RollbackErrors` field. Rollback is data-safe: the backup
  file is `Lstat`ed before the live file is touched, so a missing backup
  cannot destroy the live file.

## Upgrade notes

- **If you were relying on all-interfaces bind:** add `--bind 0.0.0.0`
  (or the specific host you need) and enable `auth:` in your config.
  Alternatively, pass `--allow-public-noauth` as an explicit acknowledgement
  that you are serving unauthenticated traffic to a non-loopback address.
- **If you have `fixtures:` in a config with a non-mock provider:**
  `overlord validate` will fail with a clear error. Remove the `fixtures:`
  block (for real-provider agents) or switch the provider back to `mock`.
- **If you depend on the shape of `Result` or `WriteError`** (unlikely —
  these are internal to the scaffolder): the existing fields are unchanged;
  `Result.CleanupWarnings []string` and `WriteError.RollbackErrors []error`
  are new additive fields.

## Full changelog

See [`CHANGELOG.md`](./CHANGELOG.md) for the full list.
