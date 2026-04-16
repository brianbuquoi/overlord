# Overlord v0.6.1

Post-release polish on top of v0.6.0. No breaking changes, no user-facing
behavior changes to `run`, `init`, or `validate`. Two small additions and
two tidy-ups.

## What's new

### `overlord version` subcommand

Both forms now work:

```bash
$ overlord version
overlord 0.6.1

$ overlord --version
overlord version 0.6.1
```

The version string moves from a literal on the root cobra command to a
single `overlordVersion` constant in `cmd/overlord/main.go`, so future
releases update one place.

### Scaffold warnings surface at the CLI

`overlord init`'s `Result.CleanupWarnings` and `WriteError.RollbackErrors`
(introduced in v0.6.0 as part of the `--force` cleanup/rollback fix) are
now printed to stderr. The scaffolder always populated them; the CLI
swallowed them. A leaked tempdir or partial rollback is no longer
invisible to operators.

Example error-path output when a mid-merge copy fails and some rollback
steps cannot complete:

```
rollback reported 1 partial failure(s):
  restore backup hello/overlord.yaml.overlord-init-bak.20260416221000 -> hello/overlord.yaml: cross-device link
Error: ...
```

Example success-path output when the scaffold commits cleanly but the
tempdir cannot be fully removed:

```
Backed up 1 colliding file(s):
  overlord.yaml -> overlord.yaml.overlord-init-bak.20260416221005
post-commit cleanup warnings (1):
  remove tempdir /tmp/.overlord-init-abc123: directory not empty
Scaffolded hello into /tmp/hello
```

### Housekeeping

- `.context/` is now in `.gitignore`.
- The audit-bundle plan frontmatter (`docs/plans/2026-04-16-002-...`) is
  marked `status: completed` with links to PR #2 and the v0.6.0 release.

## Upgrade notes

Drop-in replacement for v0.6.0. No config changes, no flag changes, no
behavior changes other than the added CLI output on the scaffold warning
paths.

## Full changelog

See [`CHANGELOG.md`](./CHANGELOG.md) for the full list.
