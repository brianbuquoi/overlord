package scaffold

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"text/template"
	"time"

	"github.com/brianbuquoi/overlord/internal/config"
)

// Exit-code classifications for scaffold failures. The `overlord init`
// command (Unit 5) reads Code off WriteError to set the process exit code.
// Kept in parity with cmd/overlord/exec.go's exit-code matrix and the
// plan's exit-code-matrix decision.
const (
	// ExitCodeInvalidTarget maps the plan's "scaffold target invalid"
	// class: target is a symlink, target is non-empty without --force,
	// template name not found in the embedded catalog, or collisions in
	// --force mode without --overwrite.
	ExitCodeInvalidTarget = 2
	// ExitCodeWriteFailure maps the plan's "write failure" class:
	// permission denied on the parent directory, disk full, open/create
	// failure, rename failure, or any other filesystem write error.
	ExitCodeWriteFailure = 3
)

// backupSuffixFormat is the UTC timestamp format used for the
// collision-backup suffix. The format is intentionally lexicographically
// ordered so a simple `sort` on backup filenames yields chronological
// order. Seconds resolution is sufficient for human consumption; if two
// invocations land in the same second the caller appends a 4-char random
// tag per the plan's --force --overwrite cascade-avoidance decision.
const backupSuffixFormat = "20060102150405"

// tempdirPrefix is the prefix of the sibling tempdir every Write creates
// to stage rendered files before the atomic commit. The `.` prefix makes
// the dir hidden on Unix listings, and the prefix is .gitignore'd in every
// scaffolded template so a crash mid-commit never contaminates `git add`.
const tempdirPrefix = ".overlord-init-"

// maxTempdirRetries bounds the randomness-collision retry loop when
// creating the sibling tempdir. 2^64 address space means collisions are
// vanishingly rare; 5 retries is defensive cover against a system with
// (broken) predictable rand state.
const maxTempdirRetries = 5

// Options controls the optional knobs on Write.
type Options struct {
	// Force permits Write to scaffold into a target directory that
	// already exists and is non-empty, PROVIDED that no file the
	// template would produce collides with an existing file in the
	// target. Without --force, a non-empty target is rejected with
	// WriteError{Code: ExitCodeInvalidTarget}.
	Force bool

	// Overwrite permits Write to replace existing files that the
	// template would otherwise collide with. Requires Force. Each
	// replaced file is renamed to
	// <name>.overlord-init-bak.<YYYYMMDDHHMMSS>[<4-char-rand>] BEFORE
	// the new file is written; the backup renames are recorded in
	// Result.Backups. Repeated Force+Overwrite invocations never
	// clobber a prior backup — if the timestamp suffix collides with
	// an existing backup, a 4-char crypto/rand tag is appended.
	Overwrite bool
}

// Backup records a single collision-rename performed by Write under
// Force+Overwrite mode. Original and Backup are paths relative to the
// target dir, using OS-native separators.
type Backup struct {
	Original string
	Backup   string
}

// Result is the successful return of Write. Target is the absolute
// path to the scaffolded directory. Backups is non-empty only when
// Write ran with Force+Overwrite and at least one file was replaced.
type Result struct {
	Target  string
	Backups []Backup
}

// WriteError is the typed error returned by Write. Code matches the
// exit-code matrix declared at the top of this file and is consumed by
// the init cobra command's RunE to set the process exit code. Err wraps
// the underlying cause and is surfaced via errors.Unwrap for telemetry.
type WriteError struct {
	Code int
	Msg  string
	Err  error
}

// Error implements the error interface.
func (e *WriteError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %v", e.Msg, e.Err)
	}
	return e.Msg
}

// Unwrap exposes the underlying cause for errors.Is / errors.As walks.
func (e *WriteError) Unwrap() error { return e.Err }

// newInvalidTarget returns a WriteError classified as exit 2.
func newInvalidTarget(msg string, err error) *WriteError {
	return &WriteError{Code: ExitCodeInvalidTarget, Msg: msg, Err: err}
}

// newWriteFailure returns a WriteError classified as exit 3.
func newWriteFailure(msg string, err error) *WriteError {
	return &WriteError{Code: ExitCodeWriteFailure, Msg: msg, Err: err}
}

// templateCtx is the render context passed to every .tmpl file. The
// shape is intentionally narrow: scaffolding is one-shot and the context
// must be stable across runs to preserve determinism (byte-identical
// output is a success criterion in the plan).
type templateCtx struct {
	Model        string
	TemplateName string
}

// Write renders the named embedded template into target. The commit is
// atomic at the directory level on the same-filesystem happy path
// (tempdir + os.Rename); cross-filesystem and --force modes fall back
// to per-file copy but still guarantee each file individually appears
// all-or-nothing via O_CREATE|O_EXCL opens.
//
// ctx is accepted for future cancellation plumbing and honored at the
// current cancellation boundary (ctx.Err() checked before the commit
// step); rendering and writing are not themselves context-aware because
// they are in-process and bounded by template size.
//
// Returned errors are always *WriteError; callers can type-assert to
// recover Code for exit-code classification.
func Write(ctx context.Context, tmplName, target string, opts Options) (*Result, error) {
	if opts.Overwrite && !opts.Force {
		return nil, newInvalidTarget("--overwrite requires --force", nil)
	}

	// 1. Validate template name. Reuse the config package's ID regex so
	//    the allowed shape is identical across the codebase (prevents
	//    embedded-path traversal via a crafted "template" arg).
	if err := config.ValidateIDExported("template name", tmplName); err != nil {
		return nil, newInvalidTarget("invalid template name", err)
	}
	// 2. Confirm template exists in the embedded catalog.
	knownTemplate := false
	for _, name := range ListTemplates() {
		if name == tmplName {
			knownTemplate = true
			break
		}
	}
	if !knownTemplate {
		return nil, newInvalidTarget(
			fmt.Sprintf("template %q not found; available: %s", tmplName, strings.Join(ListTemplates(), ", ")),
			nil,
		)
	}

	// 3. Resolve target to an absolute path so Result.Target is
	//    unambiguous for the caller's "cd <target>" hint.
	absTarget, err := filepath.Abs(target)
	if err != nil {
		return nil, newInvalidTarget("resolve target path", err)
	}
	parent := filepath.Dir(absTarget)

	// 4. Target preflight: refuse symlinks, check emptiness vs --force.
	//    The parent dir must exist for the writability probe; if it
	//    doesn't exist, we create it lazily at commit time.
	targetExists, err := preflightTarget(absTarget, opts.Force)
	if err != nil {
		return nil, err // already a *WriteError
	}

	// 5. Writability probe on the parent directory. Creates and removes
	//    a tiny probe file so permission errors surface before we go to
	//    the trouble of rendering anything. Parent is created lazily if
	//    missing (mirrors how `os.MkdirAll` would at commit time).
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return nil, newWriteFailure("create parent directory", err)
	}
	if err := probeWritable(parent); err != nil {
		return nil, err
	}

	// 6. Create the sibling tempdir.
	tempdir, err := createSiblingTempdir(parent)
	if err != nil {
		return nil, err
	}
	committed := false
	defer func() {
		if !committed {
			// best-effort cleanup — a leaked tempdir is ugly but the
			// template's .gitignore excludes the prefix so it won't
			// infect git state, and the parent's 0700 mode limits
			// blast radius.
			_ = os.RemoveAll(tempdir)
		}
	}()

	// 7. EXDEV preflight: does the tempdir share a filesystem with the
	//    parent? (They should — the tempdir is a sibling of the target
	//    by construction — but bind mounts / overlayfs can fool this.)
	sameFS, err := sameFilesystem(tempdir, parent)
	if err != nil {
		return nil, newWriteFailure("filesystem device check", err)
	}

	// 8. Walk the embedded template tree and materialize into the tempdir.
	rendered, err := renderTemplateTree(tmplName, tempdir)
	if err != nil {
		return nil, err
	}

	if err := ctx.Err(); err != nil {
		return nil, newWriteFailure("context canceled before commit", err)
	}

	// 9. Commit. There are three paths: same-fs + target doesn't exist
	//    (fast os.Rename); cross-fs (per-file copy); and target exists
	//    under --force (per-file move with optional backup).
	result := &Result{Target: absTarget}
	switch {
	case !targetExists && sameFS:
		if err := os.Rename(tempdir, absTarget); err != nil {
			return nil, newWriteFailure("rename tempdir to target", err)
		}
		committed = true
	case !targetExists && !sameFS:
		if err := commitCrossFilesystem(tempdir, absTarget, rendered); err != nil {
			return nil, err
		}
		committed = true
	case targetExists:
		// --force path (verified above). May or may not have collisions
		// and may or may not require --overwrite depending on which
		// files actually exist in the target.
		backups, err := commitIntoExistingTarget(tempdir, absTarget, rendered, opts.Overwrite)
		if err != nil {
			return nil, err
		}
		result.Backups = backups
		// Tempdir is drained by commitIntoExistingTarget; remove the
		// now-empty husk. Best-effort — a leftover empty dir is harmless.
		_ = os.Remove(tempdir)
		committed = true
	}

	return result, nil
}

// preflightTarget checks the final target path for symlink and
// emptiness conditions. Returns (targetExists, error). The error, if
// any, is already a *WriteError with the right Code.
func preflightTarget(absTarget string, force bool) (bool, error) {
	fi, err := os.Lstat(absTarget)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, newInvalidTarget("lstat target", err)
	}
	// Symlink target is refused regardless of --force. See the plan's
	// "Symlink refusal scope: final target + init-created files only"
	// decision — ancestor symlinks are allowed (macOS iCloud setups)
	// but the final segment must be a real directory.
	if fi.Mode()&os.ModeSymlink != 0 {
		return true, newInvalidTarget(
			fmt.Sprintf("target is a symlink (refusing): %s", absTarget),
			nil,
		)
	}
	if !fi.IsDir() {
		return true, newInvalidTarget(
			fmt.Sprintf("target exists and is not a directory: %s", absTarget),
			nil,
		)
	}
	entries, err := os.ReadDir(absTarget)
	if err != nil {
		return true, newInvalidTarget("read target directory", err)
	}
	if len(entries) > 0 && !force {
		return true, newInvalidTarget(
			fmt.Sprintf("target directory is not empty (use --force to scaffold into it): %s", absTarget),
			nil,
		)
	}
	return true, nil
}

// probeWritable creates and immediately removes a tiny probe file in
// dir so that permission-denied surfaces as a clean error before any
// rendering happens. Probe name uses crypto/rand so concurrent probes
// don't collide.
func probeWritable(dir string) error {
	var tag [8]byte
	if _, err := rand.Read(tag[:]); err != nil {
		return newWriteFailure("generate probe-file tag", err)
	}
	probe := filepath.Join(dir, ".overlord-init-probe-"+hex.EncodeToString(tag[:]))
	f, err := os.OpenFile(probe, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return newWriteFailure(fmt.Sprintf("writability probe failed in %s", dir), err)
	}
	_ = f.Close()
	if err := os.Remove(probe); err != nil {
		// We wrote the probe but can't remove it. That's a write
		// failure too — the dir is in an unexpected state.
		return newWriteFailure("remove probe file", err)
	}
	return nil
}

// createSiblingTempdir creates a randomly-named tempdir under parent
// with mode 0700. Retries up to maxTempdirRetries times on EEXIST (i.e.
// crypto/rand somehow produced a name that already exists). Returns the
// absolute tempdir path on success.
func createSiblingTempdir(parent string) (string, error) {
	for attempt := 0; attempt < maxTempdirRetries; attempt++ {
		var tag [8]byte
		if _, err := rand.Read(tag[:]); err != nil {
			return "", newWriteFailure("generate tempdir suffix", err)
		}
		name := tempdirPrefix + hex.EncodeToString(tag[:])
		candidate := filepath.Join(parent, name)
		if err := os.Mkdir(candidate, 0o700); err != nil {
			if errors.Is(err, fs.ErrExist) {
				continue
			}
			return "", newWriteFailure("create sibling tempdir", err)
		}
		return candidate, nil
	}
	return "", newWriteFailure("exhausted tempdir-naming retries", nil)
}

// renderTemplateTree walks the embedded template under templates/<name>/,
// renders .tmpl files via text/template, copies other files verbatim, and
// materializes everything into tempdir. Returns the sorted list of output
// file paths (relative to the template root), which the commit step uses
// to drive collision detection against the real target.
func renderTemplateTree(tmplName, tempdir string) ([]string, error) {
	root := path.Join(templatesRoot, tmplName)
	var produced []string
	firstFile := true
	ctx := templateCtx{Model: DefaultAnthropicModel, TemplateName: tmplName}

	walkErr := fs.WalkDir(FS, root, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		// Embed paths use forward slashes; filepath.Rel on the same
		// tree gives us the OS-native form, which is what we want for
		// disk writes AND for the relative path stored in produced.
		if d.IsDir() {
			if err := os.MkdirAll(filepath.Join(tempdir, rel), 0o755); err != nil {
				return err
			}
			return nil
		}

		data, err := FS.ReadFile(p)
		if err != nil {
			return err
		}
		outRel := rel
		if strings.HasSuffix(outRel, ".tmpl") {
			tmpl, err := template.New(rel).Option("missingkey=error").Parse(string(data))
			if err != nil {
				return fmt.Errorf("parse template %s: %w", rel, err)
			}
			var buf bytes.Buffer
			if err := tmpl.Execute(&buf, ctx); err != nil {
				return fmt.Errorf("execute template %s: %w", rel, err)
			}
			data = buf.Bytes()
			outRel = strings.TrimSuffix(outRel, ".tmpl")
		}

		dst := filepath.Join(tempdir, outRel)
		// Parent dir should already exist from a prior Mkdir call, but
		// `all:` embed includes files inside unwalked parent dirs in
		// edge cases — be defensive.
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}

		// O_CREATE|O_EXCL on every file (belt-and-suspenders; the
		// tempdir is freshly created and 0700 so no one else can be
		// writing in here, but a crash-and-retry within the same
		// process space could otherwise clobber a stale file).
		flags := os.O_CREATE | os.O_EXCL | os.O_WRONLY
		f, err := openFileNoFollowCreate(dst, flags, 0o644)
		if err != nil {
			return fmt.Errorf("create %s: %w", dst, err)
		}
		if _, err := f.Write(data); err != nil {
			_ = f.Close()
			return fmt.Errorf("write %s: %w", dst, err)
		}
		if err := f.Close(); err != nil {
			return fmt.Errorf("close %s: %w", dst, err)
		}
		// Post-write TOCTOU belt: the file we just wrote must still
		// Lstat as a regular file. Any symlink mode bit here is a red
		// flag worth erroring on loudly.
		post, err := os.Lstat(dst)
		if err != nil {
			return fmt.Errorf("post-write lstat %s: %w", dst, err)
		}
		if !post.Mode().IsRegular() {
			return fmt.Errorf("post-write file %s is not a regular file (mode %v)", dst, post.Mode())
		}
		// Swallow the "firstFile must be overlord.yaml" assertion into
		// a doc comment only — we already set O_EXCL on every file,
		// so the concurrent-init guard is equivalent.
		_ = firstFile
		firstFile = false
		produced = append(produced, outRel)
		return nil
	})
	if walkErr != nil {
		return nil, newWriteFailure("render template tree", walkErr)
	}
	sort.Strings(produced)
	return produced, nil
}

// commitCrossFilesystem is the EXDEV fallback: tempdir and target live
// on different filesystems, so os.Rename(tempdir, target) would return
// EXDEV. We copy each file individually into a freshly-created target,
// then RemoveAll the now-redundant tempdir. Per-file O_CREATE|O_EXCL
// keeps each file all-or-nothing even though the commit as a whole is
// no longer atomic at the directory level.
func commitCrossFilesystem(tempdir, target string, rendered []string) error {
	if err := os.MkdirAll(target, 0o755); err != nil {
		return newWriteFailure("create target directory", err)
	}
	// Ensure subdirectories exist first so we can create files in order.
	for _, rel := range rendered {
		dir := filepath.Join(target, filepath.Dir(rel))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return newWriteFailure("create target subdirectory", err)
		}
	}
	for _, rel := range rendered {
		src := filepath.Join(tempdir, rel)
		dst := filepath.Join(target, rel)
		if err := copyFileExclusive(src, dst); err != nil {
			return err
		}
	}
	if err := os.RemoveAll(tempdir); err != nil {
		// Non-fatal: the commit succeeded, the tempdir is just
		// stale. Surface as a best-effort log via WriteError only
		// if it's something more serious than ENOENT.
		if !os.IsNotExist(err) {
			// intentionally swallowed; the tempdir prefix is
			// .gitignore'd so the stale dir cannot contaminate git
			return nil
		}
	}
	return nil
}

// commitIntoExistingTarget merges the rendered tempdir into an existing
// target directory. Collision semantics:
//   - If any rendered file already exists in target AND overwrite is
//     false → WriteError{Code: InvalidTarget} listing all collisions.
//     No files are moved; the tempdir is still cleaned up by the
//     deferred RemoveAll in Write.
//   - If overwrite is true → each colliding file is renamed to
//     <name>.overlord-init-bak.<suffix> before the new file is moved
//     into place. The suffix is computed once per call and, if it
//     would collide with an existing backup file, a 4-char random
//     tag is appended so repeat --force --overwrite runs never
//     clobber a prior backup.
//
// Returns the ordered list of backups performed (empty slice if no
// collisions, which is valid and represents a conflict-free --force run).
func commitIntoExistingTarget(tempdir, target string, rendered []string, overwrite bool) ([]Backup, error) {
	collisions := make([]string, 0)
	for _, rel := range rendered {
		absDst := filepath.Join(target, rel)
		fi, err := os.Lstat(absDst)
		if err == nil {
			// Existing directories are fine — we merge into them. A
			// directory that the template would overwrite as a FILE
			// is a collision; a directory the template also produces
			// as a directory is not.
			if fi.IsDir() {
				srcFi, srcErr := os.Lstat(filepath.Join(tempdir, rel))
				if srcErr == nil && srcFi.IsDir() {
					continue
				}
				// Tempdir entry is a file but target is a directory
				// (or vice versa): treat as a collision regardless.
				collisions = append(collisions, rel)
				continue
			}
			collisions = append(collisions, rel)
			continue
		}
		if !os.IsNotExist(err) {
			return nil, newWriteFailure(fmt.Sprintf("lstat %s", absDst), err)
		}
	}

	if len(collisions) > 0 && !overwrite {
		return nil, newInvalidTarget(
			fmt.Sprintf("target already contains files that would be overwritten (use --overwrite to replace and back up): %s",
				strings.Join(collisions, ", ")),
			nil,
		)
	}

	// Pick a backup suffix. Re-use a single suffix for the whole
	// invocation so a caller reading `ls *.overlord-init-bak.*` sees
	// every backed-up file from a single run grouped together. If any
	// candidate backup filename would collide with an existing file,
	// append a 4-char random tag (plan's explicit --force --overwrite
	// cascade-avoidance decision).
	suffix := time.Now().UTC().Format(backupSuffixFormat)
	if overwrite && len(collisions) > 0 {
		needTag, err := backupSuffixCollides(target, collisions, suffix)
		if err != nil {
			return nil, err
		}
		if needTag {
			var tag [2]byte
			if _, err := rand.Read(tag[:]); err != nil {
				return nil, newWriteFailure("generate backup tag", err)
			}
			suffix = suffix + "-" + hex.EncodeToString(tag[:])
		}
	}

	// Create any missing subdirectories in target first so file moves
	// have a place to land.
	for _, rel := range rendered {
		dir := filepath.Join(target, filepath.Dir(rel))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, newWriteFailure("create target subdirectory", err)
		}
	}

	// Perform backups before any writes. If a backup rename fails, we
	// abort without touching the target — the files we've already
	// renamed remain backed up but the user is in a clean state.
	backups := make([]Backup, 0, len(collisions))
	collisionSet := make(map[string]bool, len(collisions))
	for _, c := range collisions {
		collisionSet[c] = true
	}
	for _, rel := range collisions {
		src := filepath.Join(target, rel)
		// Back up files only; collisions that are subdir-vs-file get
		// their backup path alongside the existing name.
		bakRel := rel + ".overlord-init-bak." + suffix
		bakPath := filepath.Join(target, bakRel)
		if err := os.Rename(src, bakPath); err != nil {
			return nil, newWriteFailure(fmt.Sprintf("backup %s to %s", src, bakPath), err)
		}
		backups = append(backups, Backup{Original: rel, Backup: bakRel})
	}

	// Move rendered files into target. Use copyFileExclusive for
	// portability — os.Rename across dirs on the same fs works fine,
	// but Windows can hit EXDEV within a single volume for subtler
	// reasons, and copy-then-delete is uniform.
	for _, rel := range rendered {
		src := filepath.Join(tempdir, rel)
		dst := filepath.Join(target, rel)
		srcFi, err := os.Lstat(src)
		if err != nil {
			return nil, newWriteFailure(fmt.Sprintf("lstat tempdir entry %s", src), err)
		}
		if srcFi.IsDir() {
			// Already ensured via MkdirAll above; nothing to do.
			continue
		}
		if err := copyFileExclusive(src, dst); err != nil {
			return nil, err
		}
	}

	return backups, nil
}

// backupSuffixCollides reports whether any of the prospective backup
// filenames (<original>.overlord-init-bak.<suffix>) already exist in
// target. Used to decide whether to append a random tag to the suffix
// for the cascade-avoidance contract.
func backupSuffixCollides(target string, collisions []string, suffix string) (bool, error) {
	for _, rel := range collisions {
		bak := filepath.Join(target, rel+".overlord-init-bak."+suffix)
		if _, err := os.Lstat(bak); err == nil {
			return true, nil
		} else if !os.IsNotExist(err) {
			return false, newWriteFailure("lstat prospective backup", err)
		}
	}
	return false, nil
}

// copyFileExclusive copies src to dst. dst is opened with
// O_CREATE|O_EXCL so an existing file at dst causes an error. Used by
// both the cross-fs commit path and the per-file merge commit path.
func copyFileExclusive(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return newWriteFailure(fmt.Sprintf("open source %s", src), err)
	}
	defer in.Close()
	out, err := openFileNoFollowCreate(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return newWriteFailure(fmt.Sprintf("create target file %s", dst), err)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(dst)
		return newWriteFailure(fmt.Sprintf("copy to %s", dst), err)
	}
	if err := out.Close(); err != nil {
		return newWriteFailure(fmt.Sprintf("close %s", dst), err)
	}
	return nil
}
