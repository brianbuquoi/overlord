//go:build !windows

package mock

import (
	"os"
	"syscall"
)

// openFileNoFollow opens a file for reading with O_NOFOLLOW set so that a
// symlink at the final path segment is rejected at open time. Mirrors the
// policy `internal/config/config.go` applies to the top-level config file,
// adapted for fixture loading.
//
// If the final component is a symlink, the kernel returns ELOOP, which we
// surface as a plain os.PathError — callers wrap it with fixture context.
func openFileNoFollow(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
}
