//go:build windows

package mock

import (
	"fmt"
	"os"
)

// openFileNoFollow is the Windows equivalent of the POSIX O_NOFOLLOW open.
// Windows has no direct O_NOFOLLOW equivalent exposed by the standard
// library, so we: (1) Lstat the path to detect a symlink before opening,
// (2) open the file normally, (3) Stat the opened handle and compare to the
// pre-open Lstat to close the TOCTOU window. Any symlink (pre-open) or any
// swap (TOCTOU) yields an error.
func openFileNoFollow(path string) (*os.File, error) {
	pre, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if pre.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("refuses to open symlink: %s", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	post, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !os.SameFile(pre, post) {
		_ = f.Close()
		return nil, fmt.Errorf("refuses to open path whose target changed between lstat and open: %s", path)
	}
	return f, nil
}
