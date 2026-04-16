# Overlord v0.5.0

One-command onboarding. `overlord init` scaffolds a runnable project from an
embedded template and runs a zero-API-key demo through a new first-party
`mock` provider — from `go install` to visible pipeline output in about a
minute, no credentials required.

## Highlights

### Get to "it works" in one command

```bash
go install github.com/brianbuquoi/overlord/cmd/overlord@latest
overlord init summarize
# ✓ Scaffolded summarize into ./summarize
# ✓ Submitted task ... to pipeline summarize
# {"summary": "...", "word_count": 27, "valid": true}
```

Two templates ship in v1:

- `hello` — single-stage minimal pipeline
- `summarize` — 2-stage linear chain (`summarize → validate`)

Every scaffolded project contains a complete `overlord.yaml`, schemas,
fixtures, a sample payload, an `.env.example`, and a `.gitignore` that
excludes common credential files and `--force --overwrite` backups. The
demo runs in-process against the new mock provider, so first-run success
does not depend on API keys or network access.

### First-party `mock` provider

Fixture-keyed deterministic adapter at `internal/agent/mock/`. Returns
fixture contents verbatim from `Execute`. Fixtures are validated against
the stage's registered `output_schema` at agent-construction time —
path containment check (rejects absolute paths, `..` traversal, and any
path resolving outside the config base), 256 KiB size cap, schema
validation. Every code path that constructs an agent (broker boot,
`overlord health`, SIGHUP hot-reload, integration tests) gets the
validation automatically. Usable beyond the scaffolder for CI, demos,
and regression tests.

### `overlord init` flag surface

| Flag | Purpose |
|------|---------|
| `--force` | scaffold into a non-empty target directory |
| `--overwrite` | back up and replace colliding files (requires `--force`) |
| `--no-run` | skip the in-process demo |
| `--non-interactive` | treat demo failure as a hard error (exit 4 instead of 0) |

Exit codes are distinct: `0` success, `2` invalid target or unknown
template, `3` write failure, `4` demo failure under `--non-interactive`,
`130` SIGINT during the demo. The full matrix lives in
[`docs/init.md`](./docs/init.md).

### Migrating from mock to a real provider

Every scaffolded `overlord.yaml` includes a banner-delimited
commented-out real-provider block with distinct agent IDs. To graduate:

1. Uncomment the `# === real provider ===` block
2. Change the stage's agent reference from `<id>-mock` to `<id>`
3. Set the provider's API-key environment variable

No other edits. The commented block ships with the current recommended
Anthropic model and is exercised by a build-tagged integration test
against the live API on every release candidate.

### Runtime auth guardrail

`overlord run` now emits a loud `slog.Warn` at startup when
`auth.enabled=false` AND the HTTP bind address is not loopback
(`127.0.0.0/8`, `::1`, `localhost`). Does not refuse to start — local
development regularly binds to LAN addresses — but the log line is
unmistakable in aggregators. This is the runtime-level mitigation for
the commented `auth:` block scaffolded templates ship with, so the
convenience default can't silently graduate to a production deploy
without auth enabled.

### Atomic scaffold writes

Scaffolder writes into a sibling tempdir (`.overlord-init-<rand>`,
mode 0700, 64-bit `crypto/rand` entropy) and commits via `os.Rename`
on the same-filesystem happy path. Cross-filesystem targets (bind
mounts, tmpfs, overlayfs) fall back to per-file copy after a
`syscall.Stat_t.Dev` preflight. POSIX builds use `O_NOFOLLOW` on every
file creation; Windows uses open + `fstat` + inode/volume-serial
compare against a pre-open `Lstat` to close the TOCTOU window without
a platform-specific flag. Collision backups are timestamped
(`*.overlord-init-bak.YYYYMMDDHHMMSS[-<rand>]`) so repeated
`--force --overwrite` runs never clobber a prior backup.

## Upgrade Notes from v0.4.0

- **No breaking changes to the public plugin API.** The narrow
  `registry.NewFromConfig` wrapper is preserved for non-mock callers.
- **Internal factory signature widened.** In-tree callers of
  `registry.NewFromConfigWithPlugins` must now pass
  `*contract.Registry`, `basePath string`, and
  `[]config.Stage` (filtered to bindings that reference the agent
  being constructed). Non-mock providers ignore the new parameters.
  All in-repo callers are updated.
- **`config.Agent.Fixtures`** is a new optional `map[string]string`
  field (`yaml:"fixtures,omitempty"`). Existing YAML configs without
  this field continue to parse and run unchanged.

## Known Limitations

See [KNOWN_GAPS.md](./KNOWN_GAPS.md). Items specific to this release:

- No `--list-templates` or `--json` output flag on `overlord init`.
  Programmatic callers currently parse the missing-template error
  text or import `scaffold.ListTemplates()` in-process.
- Fan-out and conditional-routing templates are not in the v1 scaffold
  catalog — those topologies require richer fixture keying
  (`{stage_id, agent_id}` pairs, multi-response fixtures) that warrants
  its own design.
- Auto-discovery of `overlord.yaml` via walk-up from cwd (so
  subsequent commands don't need `--config`) is a separate tracked
  feature, not in this release.

## Installing

```bash
go install github.com/brianbuquoi/overlord/cmd/overlord@latest
```

See [README.md](./README.md) for the quickstart.
See [docs/init.md](./docs/init.md) for the full `overlord init` reference.
See [docs/deployment.md](./docs/deployment.md) for production graduation
(Postgres/Redis stores, enabled auth).
