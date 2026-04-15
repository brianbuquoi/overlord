//go:build !linux

package plugin

import "log/slog"

// applySeccompProfile is a no-op on non-Linux platforms. Overlord subprocess
// plugins still get standard OS process isolation, but syscall-level
// restrictions are a Linux-only concept.
func applySeccompProfile(pid int) error {
	slog.Info("syscall restriction (seccomp) is not available on this platform; plugin runs with full process isolation only",
		"pid", pid)
	return nil
}
