# Subprocess Plugin Security Model

Overlord ships with two different plugin mechanisms:

1. **`.so` shared-library plugins** (`internal/plugin/loader.go`). These run
   in-process and are fully trusted. Use only for first-party extensions
   built from source you control.
2. **Subprocess plugins** (`provider: "plugin"`). These run as isolated OS
   processes and communicate with Overlord over newline-delimited JSON-RPC
   2.0 on stdin/stdout. This document covers the security model of the
   subprocess mechanism.

## Process Isolation

Each subprocess plugin runs as a separate OS process spawned by Overlord with
`exec.Command`. Crashing, panicking, or running out of memory in a plugin
cannot corrupt Overlord's heap, goroutines, or connection pools. A crashed
plugin is automatically restarted (subject to `max_restarts`) and the next
task is retried on the fresh process.

## Environment Isolation

Plugins do **not** inherit Overlord's environment by default. The manifest
declares an explicit allow list:

```yaml
env:
  - PATH
  - HOME
```

Anything not listed — `ANTHROPIC_API_KEY`, `DATABASE_URL`, `REDIS_URL`, and
every other secret Overlord was started with — is stripped before the
subprocess launches. A plugin that wants access to a specific credential
must name its env var in the manifest, which is an explicit, reviewable act.

## Syscall Restriction (Linux only)

Overlord calls `applySeccompProfile(pid)` after starting a plugin on Linux.
The in-tree implementation is a documented placeholder: installing a
seccomp-BPF filter on an already-running child PID from the parent requires
ptrace and is brittle in practice. Instead, operators who need syscall-level
restriction should launch Overlord plugins through a hardening wrapper:

- `bwrap` (bubblewrap) — reduces the subprocess to a minimal filesystem
  namespace and can install seccomp filters.
- `systemd-run --property=...` — apply a
  `SystemCallFilter=`/`NoNewPrivileges=yes` sandbox to the plugin binary.
- `landlock-exec` — per-process filesystem restriction.

On non-Linux platforms (macOS, Windows, FreeBSD) this function is a no-op
and the plugin runs with standard OS process isolation only.

## What Plugins Cannot Do

- Access Overlord's internal state (tasks, store, config) — the wire
  protocol exposes only the envelope fields the plugin is told about.
- Read environment variables not listed in the manifest.
- Share memory with Overlord or with other plugins.
- Bypass the broker retry policy — plugin-reported errors still flow
  through the standard `AgentError.Retryable` flag, respecting the manifest
  `on_failure` setting.
- Cause an unbounded restart storm: `max_restarts > 0` caps the restart
  count; once exceeded, the agent is marked unhealthy and every call
  returns a non-retryable error.

## What Plugins Can Do

- Read files inside their working directory (manifest directory by default)
  subject to OS file permissions.
- Make outbound network calls (unless the operator wraps them with a
  sandbox that forbids network — see the Linux hardening section above).
- Use CPU, memory, and file descriptors from the Overlord host process's
  quota. Operators should set cgroup limits at the launcher level.

## Prompt Injection Mitigation

Plugin subprocesses receive sanitized input just like any other agent
adapter: the broker wraps prior-stage output in the
`[SYSTEM CONTEXT]`/`[END SYSTEM CONTEXT]` envelope before it arrives in the
task payload. Plugins should still treat `system_prompt` as an instruction
surface and `payload` as untrusted data.

## Trust Model

1. **The manifest is trusted config.** Operators review the manifest for
   binary path, env allow list, and restart budget before deploying.
2. **The binary is untrusted code.** Everything in this document is
   designed under the assumption that the plugin binary is adversarial.
3. **Stdin/stdout is a trust boundary.** Malformed JSON, mismatched IDs,
   and oversized frames are logged and discarded; the plugin cannot inject
   responses for a request ID it was not assigned.

## Restart and Failure Behavior

| Event                               | Result                                            |
|-------------------------------------|---------------------------------------------------|
| Plugin exits between RPCs           | Next call triggers a single restart-and-retry.    |
| RPC times out (`rpc_timeout`)       | Retryable `AgentError`; broker may retry.         |
| JSON-RPC `invalid_params` (-32602)  | Non-retryable `AgentError`; task fails.           |
| JSON-RPC `internal_error` (-32603)  | Retryable iff manifest `on_failure: retryable`.   |
| `MaxRestarts` exceeded              | Agent marked unhealthy; all calls non-retryable.  |
| Overlord shutdown                   | SIGTERM, wait `shutdown_timeout`, then SIGKILL.   |

## Deployment Recommendations

- Run the plugin binary as a less-privileged user than Overlord itself.
- Launch Overlord from a systemd unit that applies `SystemCallFilter` and
  `ProtectSystem=strict` to the entire service tree — this transitively
  hardens all spawned plugins.
- Pin plugin binaries by path and checksum in configuration-management.
- Audit the `env:` allow list in each manifest before merging.
