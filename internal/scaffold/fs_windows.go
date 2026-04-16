//go:build windows

package scaffold

import (
	"fmt"
	"os"
	"path/filepath"
)

// openFileNoFollowCreate is the Windows equivalent of the POSIX
// O_NOFOLLOW create. Windows has no direct O_NOFOLLOW equivalent exposed
// by the standard library, so on the write side we rely on the tempdir
// being freshly created with mode 0700 (no other actor can plant a symlink
// there before we open our O_CREATE|O_EXCL children) AND on a post-create
// Lstat that refuses any regular file whose Mode reports ModeSymlink.
//
// For the collision-backup path (renames into the final target), the
// caller is responsible for an Lstat-check on the existing file before
// issuing the rename, and the target is opened with O_CREATE|O_EXCL so
// the destination of any new file cannot pre-exist.
func openFileNoFollowCreate(path string, flags int, mode os.FileMode) (*os.File, error) {
	// Pre-check: refuse if the path already exists as a symlink. Combined
	// with O_CREATE|O_EXCL below, this collapses the TOCTOU window to the
	// kernel's create-atomicity guarantee.
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("refuses to open symlink: %s", path)
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	f, err := os.OpenFile(path, flags, mode)
	if err != nil {
		return nil, err
	}
	// Post-create Lstat belt: reject if the newly-created file somehow
	// reports as a symlink (should be impossible for O_CREATE|O_EXCL, but
	// cheaper to assert than to debug).
	fi, statErr := os.Lstat(path)
	if statErr != nil {
		_ = f.Close()
		return nil, statErr
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		_ = f.Close()
		return nil, fmt.Errorf("refuses to open symlink (post-create): %s", path)
	}
	return f, nil
}

// sameFilesystem reports whether two paths live on the same filesystem
// volume. On Windows, a volume is identified by its root path (e.g.
// `C:\` vs `D:\`); `os.Rename` succeeds across directories within the
// same volume and fails across volumes. We compare the first component
// of each absolute path (the drive letter + `\` or the UNC share) as a
// pragmatic proxy — true `GetVolumeInformationW` would add cgo surface
// for no practical win in this context.
//
// Both paths MUST exist — the caller passes a freshly-created tempdir
// and an extant parent directory.
func sameFilesystem(a, b string) (bool, error) {
	av, err := windowsVolumeRoot(a)
	if err != nil {
		return false, err
	}
	bv, err := windowsVolumeRoot(b)
	if err != nil {
		return false, err
	}
	return av == bv, nil
}

// windowsVolumeRoot returns the volume root for a path, resolving the
// path to absolute first. Returns e.g. `C:\` for `C:\Users\me\proj` and
// `\\server\share` for a UNC path.
func windowsVolumeRoot(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", fmt.Errorf("abs %s: %w", p, err)
	}
	// UNC: \\server\share\...
	if len(abs) >= 2 && abs[0] == '\\' && abs[1] == '\\' {
		// Find the end of the share component: \\server\share
		end := 2
		// skip server
		for end < len(abs) && abs[end] != '\\' {
			end++
		}
		if end < len(abs) {
			end++ // skip separator
		}
		for end < len(abs) && abs[end] != '\\' {
			end++
		}
		return abs[:end], nil
	}
	// Drive-letter: C:\...
	if len(abs) >= 3 && abs[1] == ':' {
		return abs[:3], nil
	}
	return abs, nil
}
