//go:build !windows

package scaffold

import (
	"fmt"
	"os"
	"syscall"
)

// openFileNoFollowCreate opens (or creates) a regular file with O_NOFOLLOW
// set so that a symlink at the final path segment is rejected at open time.
// The flags argument MUST include O_CREATE|O_EXCL or other create/write
// intent — callers pick the exact set. Mode 0644 is the scaffolder default.
//
// Mirrors the policy `internal/config/config.go` applies to the top-level
// config file and the `internal/agent/mock/fs_posix.go` pattern for fixture
// loading, but adapted for the write side. If the final component is a
// symlink, the kernel returns ELOOP which we surface verbatim — the writer
// wraps it in WriteError with the offending path attached.
func openFileNoFollowCreate(path string, flags int, mode os.FileMode) (*os.File, error) {
	return os.OpenFile(path, flags|syscall.O_NOFOLLOW, mode)
}

// sameFilesystem reports whether two paths live on the same filesystem
// device (st_dev), which is the precondition for a safe `os.Rename` at the
// directory level on POSIX. Bind mounts, tmpfs subdirs, and overlayfs
// upper-layers that straddle disks all show up here as different devices.
//
// Both paths MUST exist — the caller is expected to pass a freshly-created
// tempdir and an extant parent directory.
func sameFilesystem(a, b string) (bool, error) {
	var sa, sb syscall.Stat_t
	if err := syscall.Stat(a, &sa); err != nil {
		return false, fmt.Errorf("stat %s: %w", a, err)
	}
	if err := syscall.Stat(b, &sb); err != nil {
		return false, fmt.Errorf("stat %s: %w", b, err)
	}
	return sa.Dev == sb.Dev, nil
}
