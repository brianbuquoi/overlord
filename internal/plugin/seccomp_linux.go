//go:build linux

package plugin

import "log/slog"

// applySeccompProfile is a placeholder for a future seccomp-BPF filter that
// would restrict the plugin subprocess to a minimal syscall allow list
// (read/write/close/exit, etc.). A real implementation requires either:
//
//  1. A small launcher binary that installs the seccomp filter on itself
//     before exec'ing the plugin, OR
//  2. Wrapping the plugin in bubblewrap/firejail/landlock at deploy time.
//
// Installing a seccomp-BPF filter on an already-running child PID from the
// parent requires ptrace and is brittle in practice, so we intentionally keep
// this a documented no-op in-process. The stub returns nil so the overall
// plugin pipeline degrades cleanly on kernels without seccomp support.
//
// See docs/plugin-security.md for the production hardening story (bwrap /
// landlock / systemd unit file templates).
func applySeccompProfile(pid int) error {
	slog.Info("seccomp-BPF syscall restriction is a documented placeholder; production deployments should launch plugins via a bwrap/landlock/systemd-hardened wrapper",
		"pid", pid)
	return nil
}
