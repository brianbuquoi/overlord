package scaffold

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/brianbuquoi/overlord/internal/config"
)

// These tests exercise the public Write() contract. We construct target
// dirs under t.TempDir() so each test is isolated; we assert on the
// rendered on-disk tree via filepath.Walk and on the returned *Result.

// assertTemplateTreePresent verifies every file we expect under the
// rendered template exists at target and that no `.tmpl` suffixed file
// survived. Mirrors the structural guard in templates_test.go.
func assertTemplateTreePresent(t *testing.T, name, target string) {
	t.Helper()
	mustExist := []string{
		"overlord.yaml", // rendered from overlord.yaml.tmpl
		"sample_payload.json",
		".env.example",
		".gitignore",
	}
	for _, rel := range mustExist {
		fi, err := os.Lstat(filepath.Join(target, rel))
		if err != nil {
			t.Errorf("template %s: missing %s: %v", name, rel, err)
			continue
		}
		if !fi.Mode().IsRegular() {
			t.Errorf("template %s: %s is not regular (mode %v)", name, rel, fi.Mode())
		}
	}
	mustDirs := []string{"schemas", "fixtures"}
	for _, d := range mustDirs {
		fi, err := os.Stat(filepath.Join(target, d))
		if err != nil {
			t.Errorf("template %s: missing dir %s: %v", name, d, err)
			continue
		}
		if !fi.IsDir() {
			t.Errorf("template %s: %s is not a directory", name, d)
		}
	}
	// No .tmpl survivors.
	_ = filepath.WalkDir(target, func(p string, d fs.DirEntry, _ error) error {
		if !d.IsDir() && strings.HasSuffix(p, ".tmpl") {
			t.Errorf("template %s: stray .tmpl file %s", name, p)
		}
		return nil
	})
}

// TestWrite_HappyPath_SameFilesystem covers the fast path: fresh target
// directory, same filesystem as the parent, default Options. Expect
// tempdir → os.Rename, rendered tree present, empty backups.
func TestWrite_HappyPath_SameFilesystem(t *testing.T) {
	for _, name := range strictTemplates() {
		t.Run(name, func(t *testing.T) {
			parent := t.TempDir()
			target := filepath.Join(parent, name)
			res, err := Write(context.Background(), name, target, Options{})
			if err != nil {
				t.Fatalf("Write: %v", err)
			}
			if res == nil {
				t.Fatal("Write returned nil Result")
			}
			if res.Target != target {
				// res.Target is filepath.Abs; if target is already
				// absolute (which t.TempDir is), they should match.
				t.Errorf("Result.Target = %q, want %q", res.Target, target)
			}
			if len(res.Backups) != 0 {
				t.Errorf("expected empty Backups on fresh target, got %d", len(res.Backups))
			}
			assertTemplateTreePresent(t, name, target)
		})
	}
}

// TestWrite_RenderedConfigLoads is the integration assertion that the
// scaffolded overlord.yaml parses cleanly via config.Load. This is the
// strongest end-to-end check the writer can do without touching Unit 5.
func TestWrite_RenderedConfigLoads(t *testing.T) {
	for _, name := range strictTemplates() {
		t.Run(name, func(t *testing.T) {
			parent := t.TempDir()
			target := filepath.Join(parent, name)
			if _, err := Write(context.Background(), name, target, Options{}); err != nil {
				t.Fatalf("Write: %v", err)
			}
			cfg, err := config.Load(filepath.Join(target, "overlord.yaml"))
			if err != nil {
				t.Fatalf("config.Load: %v", err)
			}
			if len(cfg.Pipelines) == 0 {
				t.Error("rendered config has zero pipelines")
			}
			if len(cfg.Agents) == 0 {
				t.Error("rendered config has zero agents")
			}
		})
	}
}

// TestWrite_ModelSubstitution asserts {{ .Model }} is resolved in the
// rendered overlord.yaml and no unresolved placeholders remain.
func TestWrite_ModelSubstitution(t *testing.T) {
	parent := t.TempDir()
	target := filepath.Join(parent, "hello")
	if _, err := Write(context.Background(), "hello", target, Options{}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(target, "overlord.yaml"))
	if err != nil {
		t.Fatalf("read overlord.yaml: %v", err)
	}
	body := string(data)
	if !strings.Contains(body, DefaultAnthropicModel) {
		t.Errorf("rendered overlord.yaml missing DefaultAnthropicModel %q", DefaultAnthropicModel)
	}
	if strings.Contains(body, "{{ .Model }}") || strings.Contains(body, "{{.Model}}") {
		t.Error("rendered overlord.yaml contains unresolved {{ .Model }} placeholder")
	}
}

// TestWrite_NonTmplFilesByteIdentical asserts that files without the
// .tmpl suffix are copied byte-for-byte from the embedded FS. This
// guards against accidental rendering of JSON schemas or fixtures.
func TestWrite_NonTmplFilesByteIdentical(t *testing.T) {
	parent := t.TempDir()
	target := filepath.Join(parent, "hello")
	if _, err := Write(context.Background(), "hello", target, Options{}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	// Compare every non-.tmpl file against FS.
	root := "templates/hello"
	if err := fs.WalkDir(FS, root, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		if strings.HasSuffix(rel, ".tmpl") {
			return nil
		}
		src, err := FS.ReadFile(p)
		if err != nil {
			return err
		}
		dst, err := os.ReadFile(filepath.Join(target, rel))
		if err != nil {
			return err
		}
		if string(src) != string(dst) {
			t.Errorf("non-.tmpl file %s differs between FS and disk", rel)
		}
		return nil
	}); err != nil {
		t.Fatalf("walk FS: %v", err)
	}
}

// TestWrite_NoStrayTempdir asserts no .overlord-init-<hex> directory
// remains in the parent after a successful commit — tempdir must be
// renamed-in-place (fresh target) or fully cleaned up (--force merge).
func TestWrite_NoStrayTempdir(t *testing.T) {
	cases := []struct {
		name    string
		prefill func(t *testing.T, target string)
		opts    Options
	}{
		{
			name:    "fresh target",
			prefill: func(*testing.T, string) {},
			opts:    Options{},
		},
		{
			name: "--force merge into pre-existing empty dir",
			prefill: func(t *testing.T, target string) {
				if err := os.MkdirAll(target, 0o755); err != nil {
					t.Fatalf("prefill mkdir: %v", err)
				}
				if err := os.WriteFile(filepath.Join(target, "unrelated.txt"), []byte("x"), 0o644); err != nil {
					t.Fatalf("prefill write: %v", err)
				}
			},
			opts: Options{Force: true},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			parent := t.TempDir()
			target := filepath.Join(parent, "hello")
			tc.prefill(t, target)

			if _, err := Write(context.Background(), "hello", target, tc.opts); err != nil {
				t.Fatalf("Write: %v", err)
			}
			entries, err := os.ReadDir(parent)
			if err != nil {
				t.Fatalf("read parent: %v", err)
			}
			for _, e := range entries {
				if strings.HasPrefix(e.Name(), tempdirPrefix) {
					t.Errorf("stray tempdir in parent: %s", e.Name())
				}
			}
		})
	}
}

// TestWrite_Symlink_Refused asserts that if the target path itself is a
// symlink, Write refuses regardless of --force or --overwrite. Code 2.
func TestWrite_Symlink_Refused(t *testing.T) {
	parent := t.TempDir()
	real := filepath.Join(parent, "real")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatalf("mkdir real: %v", err)
	}
	link := filepath.Join(parent, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	cases := []Options{
		{},
		{Force: true},
		{Force: true, Overwrite: true},
	}
	for _, opts := range cases {
		_, err := Write(context.Background(), "hello", link, opts)
		if err == nil {
			t.Fatalf("expected error for symlink target with opts %+v", opts)
		}
		var werr *WriteError
		if !errors.As(err, &werr) {
			t.Fatalf("want *WriteError, got %T: %v", err, err)
		}
		if werr.Code != ExitCodeInvalidTarget {
			t.Errorf("code = %d, want %d", werr.Code, ExitCodeInvalidTarget)
		}
	}
}

// TestWrite_AncestorSymlink_Allowed covers the macOS iCloud /
// ~/projects-symlinked-elsewhere case: an ancestor of the target is a
// symlink to a real directory. That must be permitted — only the final
// segment is inspected.
func TestWrite_AncestorSymlink_Allowed(t *testing.T) {
	parent := t.TempDir()
	realAncestor := filepath.Join(parent, "real-ancestor")
	if err := os.Mkdir(realAncestor, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	linkedAncestor := filepath.Join(parent, "linked-ancestor")
	if err := os.Symlink(realAncestor, linkedAncestor); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	target := filepath.Join(linkedAncestor, "proj")
	if _, err := Write(context.Background(), "hello", target, Options{}); err != nil {
		t.Fatalf("Write via symlinked ancestor: %v", err)
	}
	// The files should be visible via the symlinked path.
	if _, err := os.Stat(filepath.Join(target, "overlord.yaml")); err != nil {
		t.Errorf("overlord.yaml not visible via symlinked ancestor: %v", err)
	}
}

// TestWrite_NonEmpty_NoForce asserts a non-empty target without --force
// yields a code-2 error AND leaves no files written AND leaves no
// tempdir leaked in the parent.
func TestWrite_NonEmpty_NoForce(t *testing.T) {
	parent := t.TempDir()
	target := filepath.Join(parent, "proj")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatalf("mkdir target: %v", err)
	}
	existing := filepath.Join(target, "existing.txt")
	if err := os.WriteFile(existing, []byte("hi"), 0o644); err != nil {
		t.Fatalf("write existing: %v", err)
	}

	_, err := Write(context.Background(), "hello", target, Options{})
	if err == nil {
		t.Fatal("expected error for non-empty target without --force")
	}
	var werr *WriteError
	if !errors.As(err, &werr) {
		t.Fatalf("want *WriteError, got %T", err)
	}
	if werr.Code != ExitCodeInvalidTarget {
		t.Errorf("code = %d, want %d", werr.Code, ExitCodeInvalidTarget)
	}
	// Pre-existing file still there, no template files written.
	if _, err := os.Stat(existing); err != nil {
		t.Errorf("pre-existing file was removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(target, "overlord.yaml")); !os.IsNotExist(err) {
		t.Errorf("overlord.yaml leaked into target: %v", err)
	}
	// No tempdir leaked.
	entries, _ := os.ReadDir(parent)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), tempdirPrefix) {
			t.Errorf("tempdir leaked: %s", e.Name())
		}
	}
}

// TestWrite_Force_NonEmpty_NoCollisions: target exists, has unrelated
// files, nothing collides with the template set → Write succeeds, no
// backups, unrelated files untouched.
func TestWrite_Force_NonEmpty_NoCollisions(t *testing.T) {
	parent := t.TempDir()
	target := filepath.Join(parent, "proj")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	stray := filepath.Join(target, "README.md")
	if err := os.WriteFile(stray, []byte("# existing\n"), 0o644); err != nil {
		t.Fatalf("write stray: %v", err)
	}
	res, err := Write(context.Background(), "hello", target, Options{Force: true})
	if err != nil {
		t.Fatalf("Write --force: %v", err)
	}
	if len(res.Backups) != 0 {
		t.Errorf("expected empty backups, got %d", len(res.Backups))
	}
	data, err := os.ReadFile(stray)
	if err != nil {
		t.Errorf("stray README.md removed: %v", err)
	}
	if string(data) != "# existing\n" {
		t.Errorf("stray README.md content changed: %q", string(data))
	}
	assertTemplateTreePresent(t, "hello", target)
}

// TestWrite_Force_Collision_NoOverwrite: target has a colliding
// overlord.yaml; --force alone must fail with code 2 listing the
// collision and leave no files touched.
func TestWrite_Force_Collision_NoOverwrite(t *testing.T) {
	parent := t.TempDir()
	target := filepath.Join(parent, "proj")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	collider := filepath.Join(target, "overlord.yaml")
	prior := []byte("# user's own config\n")
	if err := os.WriteFile(collider, prior, 0o644); err != nil {
		t.Fatalf("write collider: %v", err)
	}
	_, err := Write(context.Background(), "hello", target, Options{Force: true})
	if err == nil {
		t.Fatal("expected collision error")
	}
	var werr *WriteError
	if !errors.As(err, &werr) {
		t.Fatalf("want *WriteError, got %T", err)
	}
	if werr.Code != ExitCodeInvalidTarget {
		t.Errorf("code = %d, want %d", werr.Code, ExitCodeInvalidTarget)
	}
	if !strings.Contains(werr.Error(), "overlord.yaml") {
		t.Errorf("error should name colliding file, got %q", werr.Error())
	}
	// Existing file untouched.
	got, err := os.ReadFile(collider)
	if err != nil {
		t.Fatalf("read collider: %v", err)
	}
	if string(got) != string(prior) {
		t.Errorf("existing overlord.yaml was clobbered")
	}
	// No new template files snuck in.
	if _, err := os.Stat(filepath.Join(target, "schemas", "input_v1.json")); !os.IsNotExist(err) {
		t.Errorf("schemas/input_v1.json leaked: %v", err)
	}
}

// TestWrite_Force_Overwrite_WithCollision: --force --overwrite replaces
// the existing overlord.yaml, renames the old one to
// overlord.yaml.overlord-init-bak.<suffix>, and records the rename.
func TestWrite_Force_Overwrite_WithCollision(t *testing.T) {
	parent := t.TempDir()
	target := filepath.Join(parent, "proj")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	collider := filepath.Join(target, "overlord.yaml")
	prior := []byte("# user's own config\n")
	if err := os.WriteFile(collider, prior, 0o644); err != nil {
		t.Fatalf("write collider: %v", err)
	}
	res, err := Write(context.Background(), "hello", target, Options{Force: true, Overwrite: true})
	if err != nil {
		t.Fatalf("Write --force --overwrite: %v", err)
	}
	if len(res.Backups) == 0 {
		t.Fatal("expected at least one backup, got 0")
	}
	var overYAMLBackup *Backup
	for i := range res.Backups {
		if res.Backups[i].Original == "overlord.yaml" {
			overYAMLBackup = &res.Backups[i]
			break
		}
	}
	if overYAMLBackup == nil {
		t.Fatalf("expected a backup for overlord.yaml in %+v", res.Backups)
	}
	if !strings.HasPrefix(overYAMLBackup.Backup, "overlord.yaml.overlord-init-bak.") {
		t.Errorf("unexpected backup name: %s", overYAMLBackup.Backup)
	}
	// Backup contents match prior.
	bakData, err := os.ReadFile(filepath.Join(target, overYAMLBackup.Backup))
	if err != nil {
		t.Fatalf("read backup: %v", err)
	}
	if string(bakData) != string(prior) {
		t.Errorf("backup contents do not match original prior config")
	}
	// New overlord.yaml is the rendered template, which must contain
	// DefaultAnthropicModel (the commented real-provider block).
	newData, err := os.ReadFile(collider)
	if err != nil {
		t.Fatalf("read new overlord.yaml: %v", err)
	}
	if !strings.Contains(string(newData), DefaultAnthropicModel) {
		t.Errorf("rendered overlord.yaml missing DefaultAnthropicModel")
	}
}

// TestWrite_Force_Overwrite_TwiceProducesDistinctBackup asserts that a
// second --force --overwrite run (against the same target) produces a
// new backup with a distinct filename — even if the wall-clock second
// is identical. The first backup file must survive untouched.
func TestWrite_Force_Overwrite_TwiceProducesDistinctBackup(t *testing.T) {
	parent := t.TempDir()
	target := filepath.Join(parent, "proj")

	// Initial scaffold.
	if _, err := Write(context.Background(), "hello", target, Options{}); err != nil {
		t.Fatalf("initial Write: %v", err)
	}

	// First --force --overwrite run.
	res1, err := Write(context.Background(), "hello", target, Options{Force: true, Overwrite: true})
	if err != nil {
		t.Fatalf("first --force --overwrite: %v", err)
	}
	if len(res1.Backups) == 0 {
		t.Fatal("expected first backup set to be non-empty")
	}

	// Capture backup filenames from the first run so we can assert they
	// survive the second run.
	firstRunBackupPaths := make(map[string][]byte)
	for _, b := range res1.Backups {
		full := filepath.Join(target, b.Backup)
		data, err := os.ReadFile(full)
		if err != nil {
			t.Fatalf("read first-run backup %s: %v", full, err)
		}
		firstRunBackupPaths[b.Backup] = data
	}

	// Second run — same target, same second if we're quick enough.
	res2, err := Write(context.Background(), "hello", target, Options{Force: true, Overwrite: true})
	if err != nil {
		t.Fatalf("second --force --overwrite: %v", err)
	}
	if len(res2.Backups) == 0 {
		t.Fatal("expected second backup set to be non-empty")
	}

	// The second run's backup names must not collide with the first
	// run's — if the suffix collided, the writer should have appended a
	// random 4-hex tag to disambiguate.
	for _, b2 := range res2.Backups {
		if _, clash := firstRunBackupPaths[b2.Backup]; clash {
			t.Errorf("second run produced a backup with the same name as the first: %s", b2.Backup)
		}
	}
	// Every first-run backup still exists with unchanged bytes.
	for name, want := range firstRunBackupPaths {
		got, err := os.ReadFile(filepath.Join(target, name))
		if err != nil {
			t.Errorf("first-run backup %s vanished: %v", name, err)
			continue
		}
		if string(got) != string(want) {
			t.Errorf("first-run backup %s was modified during second run", name)
		}
	}
}

// TestWrite_ExistingBackupFilesNotCollisions: files matching
// *.overlord-init-bak.* in the target are NOT treated as collisions
// (they aren't in the template set). --force alone should succeed.
func TestWrite_ExistingBackupFilesNotCollisions(t *testing.T) {
	parent := t.TempDir()
	target := filepath.Join(parent, "proj")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	bak := filepath.Join(target, "overlord.yaml.overlord-init-bak.20260101000000")
	if err := os.WriteFile(bak, []byte("old"), 0o644); err != nil {
		t.Fatalf("write bak: %v", err)
	}
	if _, err := Write(context.Background(), "hello", target, Options{Force: true}); err != nil {
		t.Fatalf("Write --force: %v", err)
	}
	// Backup file still there.
	if _, err := os.Stat(bak); err != nil {
		t.Errorf("backup file vanished: %v", err)
	}
	// Template files present.
	assertTemplateTreePresent(t, "hello", target)
}

// TestWrite_UnknownTemplate asserts code-2 on a template name not
// present in the embedded catalog.
func TestWrite_UnknownTemplate(t *testing.T) {
	parent := t.TempDir()
	target := filepath.Join(parent, "proj")
	_, err := Write(context.Background(), "does-not-exist", target, Options{})
	if err == nil {
		t.Fatal("expected error for unknown template")
	}
	var werr *WriteError
	if !errors.As(err, &werr) {
		t.Fatalf("want *WriteError, got %T", err)
	}
	if werr.Code != ExitCodeInvalidTarget {
		t.Errorf("code = %d, want %d", werr.Code, ExitCodeInvalidTarget)
	}
	if !strings.Contains(werr.Error(), "does-not-exist") {
		t.Errorf("error should mention the unknown template name, got %q", werr.Error())
	}
}

// TestWrite_InvalidTemplateName asserts the ID-validator classifies
// "../etc/passwd" and similar adversarial input as a target-invalid
// error (code 2). Would-be path traversal via the template arg is the
// main threat this guards against.
func TestWrite_InvalidTemplateName(t *testing.T) {
	cases := []string{
		"../etc/passwd",
		"hello/../other",
		"hello with spaces",
		"",
	}
	for _, bad := range cases {
		t.Run(bad, func(t *testing.T) {
			parent := t.TempDir()
			target := filepath.Join(parent, "proj")
			_, err := Write(context.Background(), bad, target, Options{})
			if err == nil {
				t.Fatalf("expected error for template name %q", bad)
			}
			var werr *WriteError
			if !errors.As(err, &werr) {
				t.Fatalf("want *WriteError, got %T", err)
			}
			if werr.Code != ExitCodeInvalidTarget {
				t.Errorf("code = %d, want %d", werr.Code, ExitCodeInvalidTarget)
			}
		})
	}
}

// TestWrite_PermissionDenied asserts writability probe catches a
// read-only parent dir before any tempdir is created.
func TestWrite_PermissionDenied(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root — permission bits are advisory")
	}
	parent := t.TempDir()
	ro := filepath.Join(parent, "readonly")
	if err := os.Mkdir(ro, 0o500); err != nil {
		t.Fatalf("mkdir readonly: %v", err)
	}
	defer func() {
		// Restore write so t.TempDir cleanup can remove it.
		_ = os.Chmod(ro, 0o700)
	}()
	target := filepath.Join(ro, "proj")
	_, err := Write(context.Background(), "hello", target, Options{})
	if err == nil {
		t.Fatal("expected permission error")
	}
	var werr *WriteError
	if !errors.As(err, &werr) {
		t.Fatalf("want *WriteError, got %T", err)
	}
	if werr.Code != ExitCodeWriteFailure {
		t.Errorf("code = %d, want %d", werr.Code, ExitCodeWriteFailure)
	}
	// No tempdir leaked in the read-only dir.
	entries, _ := os.ReadDir(ro)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), tempdirPrefix) {
			t.Errorf("tempdir leaked into read-only dir: %s", e.Name())
		}
	}
}

// TestWrite_Concurrent_OneWinner: two goroutines call Write() for the
// same fresh target. Exactly one must succeed and the other must surface
// a clean *WriteError. The survivor's files are intact.
func TestWrite_Concurrent_OneWinner(t *testing.T) {
	parent := t.TempDir()
	target := filepath.Join(parent, "proj")

	var wg sync.WaitGroup
	var successes atomic.Int32
	var errs [2]error
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, err := Write(context.Background(), "hello", target, Options{})
			if err == nil {
				successes.Add(1)
			} else {
				errs[idx] = err
			}
		}(i)
	}
	wg.Wait()
	if successes.Load() != 1 {
		t.Fatalf("expected exactly 1 successful concurrent Write, got %d", successes.Load())
	}
	// The loser's error must be a *WriteError (classified, not raw).
	var winnerErr error
	for _, e := range errs {
		if e != nil {
			winnerErr = e
			break
		}
	}
	var werr *WriteError
	if !errors.As(winnerErr, &werr) {
		t.Fatalf("loser's error is not *WriteError: %T %v", winnerErr, winnerErr)
	}
	// The survivor's target must hold the rendered tree.
	assertTemplateTreePresent(t, "hello", target)
	// No tempdir leaked.
	entries, _ := os.ReadDir(parent)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), tempdirPrefix) {
			t.Errorf("concurrent Writes left a stray tempdir: %s", e.Name())
		}
	}
}

// TestWrite_NoTempdirOnEarlyError asserts the probeWritable failure
// path does NOT leak a tempdir. This covers the case where
// preflightTarget or probeWritable returns early and the deferred
// cleanup never fires (because no tempdir was created yet).
func TestWrite_NoTempdirOnEarlyError(t *testing.T) {
	parent := t.TempDir()
	target := filepath.Join(parent, "proj")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Plant a file so the non-empty check fires before probeWritable.
	if err := os.WriteFile(filepath.Join(target, "existing.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write existing: %v", err)
	}
	_, _ = Write(context.Background(), "hello", target, Options{}) // expected to error
	entries, _ := os.ReadDir(parent)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), tempdirPrefix) {
			t.Errorf("stray tempdir after early error: %s", e.Name())
		}
	}
}

// TestWrite_OverwriteWithoutForce is a guard: --overwrite alone is
// nonsensical (can't replace what you haven't opted into touching).
// Writer must reject it with code 2 before doing any filesystem work.
func TestWrite_OverwriteWithoutForce(t *testing.T) {
	parent := t.TempDir()
	target := filepath.Join(parent, "proj")
	_, err := Write(context.Background(), "hello", target, Options{Overwrite: true})
	if err == nil {
		t.Fatal("expected error for --overwrite without --force")
	}
	var werr *WriteError
	if !errors.As(err, &werr) {
		t.Fatalf("want *WriteError, got %T", err)
	}
	if werr.Code != ExitCodeInvalidTarget {
		t.Errorf("code = %d, want %d", werr.Code, ExitCodeInvalidTarget)
	}
}

// TestWrite_ContextCanceled: a canceled context after tempdir creation
// but before commit surfaces as a WriteError and leaves nothing behind.
// We can't easily cancel mid-render without instrumentation, so we
// test the pre-commit boundary: ctx.Err() is checked after render.
func TestWrite_ContextCanceled(t *testing.T) {
	parent := t.TempDir()
	target := filepath.Join(parent, "proj")
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-canceled — renderTemplateTree still runs but the
	// post-render ctx.Err() check fires before commit.
	_, err := Write(ctx, "hello", target, Options{})
	if err == nil {
		t.Fatal("expected cancellation error")
	}
	var werr *WriteError
	if !errors.As(err, &werr) {
		t.Fatalf("want *WriteError, got %T", err)
	}
	if werr.Code != ExitCodeWriteFailure {
		t.Errorf("code = %d, want %d", werr.Code, ExitCodeWriteFailure)
	}
	// No tempdir leaked (deferred cleanup runs on error).
	entries, _ := os.ReadDir(parent)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), tempdirPrefix) {
			t.Errorf("tempdir leaked after context cancel: %s", e.Name())
		}
	}
	// No target created.
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Errorf("target created despite context cancel: %v", err)
	}
}

// TestWriteError_UnwrapAndIsAs verifies errors.As / errors.Unwrap
// plumbing is wired correctly so Unit 5 can propagate underlying
// causes to telemetry.
func TestWriteError_UnwrapAndIsAs(t *testing.T) {
	underlying := errors.New("root cause")
	wrapped := &WriteError{Code: 3, Msg: "top", Err: underlying}
	if !errors.Is(wrapped, underlying) {
		t.Error("errors.Is(wrapped, underlying) = false, want true")
	}
	var unwrapped *WriteError
	if !errors.As(wrapped, &unwrapped) {
		t.Error("errors.As to *WriteError failed")
	}
	if unwrapped.Code != 3 {
		t.Errorf("Code = %d, want 3", unwrapped.Code)
	}
	if !strings.Contains(wrapped.Error(), "top") || !strings.Contains(wrapped.Error(), "root cause") {
		t.Errorf("Error() = %q, expected to contain both message and underlying", wrapped.Error())
	}
}

// TestWrite_CrossFilesystem exercises the EXDEV-fallback commit path.
// We skip unless we can identify a viable cross-fs pair on this host.
// On Linux, /dev/shm (tmpfs) and /tmp (often ext4) typically qualify.
func TestWrite_CrossFilesystem(t *testing.T) {
	// Attempt to find a parent that lives on a different filesystem
	// device than t.TempDir(). /dev/shm is tmpfs on most Linux CI
	// hosts; skip cleanly if we can't find a second fs.
	tmp := t.TempDir()
	candidate := "/dev/shm"
	fi, err := os.Stat(candidate)
	if err != nil || !fi.IsDir() {
		t.Skipf("cross-fs test requires an accessible %s directory; skipping", candidate)
	}
	same, err := sameFilesystem(tmp, candidate)
	if err != nil {
		t.Skipf("sameFilesystem check failed: %v", err)
	}
	if same {
		t.Skip("t.TempDir() and /dev/shm share a device; cannot simulate EXDEV")
	}
	// Make sure /dev/shm is writable (it usually is).
	shmDir, err := os.MkdirTemp(candidate, "overlord-scaffold-test-")
	if err != nil {
		t.Skipf("/dev/shm not writable: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(shmDir) })

	target := filepath.Join(shmDir, "proj")
	res, err := Write(context.Background(), "hello", target, Options{})
	if err != nil {
		t.Fatalf("Write across filesystems: %v", err)
	}
	if res.Target != target {
		t.Errorf("Result.Target = %q, want %q", res.Target, target)
	}
	assertTemplateTreePresent(t, "hello", target)
	// No tempdir survives in /dev/shm either.
	entries, _ := os.ReadDir(shmDir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), tempdirPrefix) {
			t.Errorf("stray tempdir in /dev/shm: %s", e.Name())
		}
	}
}

// TestCommitCrossFilesystem_PartialFailureCleanup asserts that if
// copyFileExclusive fails mid-loop, commitCrossFilesystem removes the
// target directory it created so the caller does not see a
// half-scaffolded project. Safe because the cross-fs path only runs
// when the target did not pre-exist (the !targetExists && !sameFS
// switch case in Write); the function's cleanup callback may therefore
// os.RemoveAll(target) without risk of clobbering user content.
//
// We exercise the helper directly rather than staging a real cross-fs
// flow because reliably provoking an EXDEV + mid-copy failure needs
// two filesystems and a specific file the kernel refuses to write.
func TestCommitCrossFilesystem_PartialFailureCleanup(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root — permission bits are advisory")
	}
	parent := t.TempDir()
	tempdir := filepath.Join(parent, ".tmp")
	target := filepath.Join(parent, "target")
	if err := os.Mkdir(tempdir, 0o755); err != nil {
		t.Fatalf("mkdir tempdir: %v", err)
	}
	// Stage two rendered files in the tempdir. The first will copy
	// fine; the second will fail because its destination subdir is
	// made read-only below.
	if err := os.WriteFile(filepath.Join(tempdir, "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatalf("write a.txt: %v", err)
	}
	if err := os.Mkdir(filepath.Join(tempdir, "sub"), 0o755); err != nil {
		t.Fatalf("mkdir sub: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tempdir, "sub", "b.txt"), []byte("b"), 0o644); err != nil {
		t.Fatalf("write sub/b.txt: %v", err)
	}

	// Pre-create target/sub and chmod it read-only so copyFileExclusive
	// of sub/b.txt fails with permission denied. The partial state (a.txt
	// already copied into target) is exactly the mid-loop failure
	// scenario the cleanup is designed for.
	if err := os.MkdirAll(filepath.Join(target, "sub"), 0o755); err != nil {
		t.Fatalf("pre-create target/sub: %v", err)
	}
	if err := os.Chmod(filepath.Join(target, "sub"), 0o500); err != nil {
		t.Fatalf("chmod ro: %v", err)
	}
	t.Cleanup(func() {
		// Best-effort restore so TempDir teardown can remove it (in
		// case the function under test failed to clean up).
		_ = os.Chmod(filepath.Join(target, "sub"), 0o700)
	})

	err := commitCrossFilesystem(tempdir, target, []string{"a.txt", "sub/b.txt"})
	if err == nil {
		t.Fatal("expected commitCrossFilesystem to fail")
	}
	var werr *WriteError
	if !errors.As(err, &werr) {
		t.Fatalf("want *WriteError, got %T: %v", err, err)
	}
	if werr.Code != ExitCodeWriteFailure {
		t.Errorf("code = %d, want %d", werr.Code, ExitCodeWriteFailure)
	}
	// Target must have been removed by the cleanup callback. No
	// half-scaffolded project left behind.
	if _, statErr := os.Stat(target); !os.IsNotExist(statErr) {
		t.Errorf("expected target to be removed on partial failure, got stat err: %v", statErr)
	}
}

// TestWrite_RelativeTargetPath asserts a relative target path is
// resolved via filepath.Abs before Result.Target is set — callers who
// want to `cd` into the scaffolded dir should not have to re-resolve.
func TestWrite_RelativeTargetPath(t *testing.T) {
	parent := t.TempDir()
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if err := os.Chdir(parent); err != nil {
		t.Fatalf("Chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })

	res, err := Write(context.Background(), "hello", "proj", Options{})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if !filepath.IsAbs(res.Target) {
		t.Errorf("Result.Target = %q, expected absolute path", res.Target)
	}
	if !strings.HasSuffix(res.Target, "proj") {
		t.Errorf("Result.Target = %q, expected to end with 'proj'", res.Target)
	}
}

// TestWrite_ForceRollback_CopyFails asserts that when a copy fails
// after backups have been performed, backups are restored to their
// original names and any files already copied are removed.
//
// Poison mechanism: pre-create target/schemas/ with mode 0o500 (no
// write bit). The template also produces schemas/input_v1.json and
// schemas/output_v1.json. commitIntoExistingTarget calls MkdirAll on
// schemas/ (no-op, already exists) then iterates the sorted rendered
// list. Files before schemas/* (.env.example, .gitignore,
// fixtures/greet.json, sample_payload.json) copy fine; the first
// schemas/input_v1.json copy fails with EACCES, triggering rollback.
func TestWrite_ForceRollback_CopyFails(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root — permission bits are advisory")
	}
	parent := t.TempDir()
	target := filepath.Join(parent, "hello")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatalf("mkdir target: %v", err)
	}

	// Pre-populate target with a file whose name collides with a
	// template output so --overwrite triggers a backup.
	originalContent := []byte("ORIGINAL\n")
	yamlPath := filepath.Join(target, "overlord.yaml")
	if err := os.WriteFile(yamlPath, originalContent, 0o644); err != nil {
		t.Fatalf("prefill yaml: %v", err)
	}

	// Inject a failure: pre-create target/schemas/ with no write bit so
	// copyFileExclusive cannot create schemas/input_v1.json inside it.
	// Template produces schemas/input_v1.json (confirmed from embedded tree).
	schemasDir := filepath.Join(target, "schemas")
	if err := os.MkdirAll(schemasDir, 0o500); err != nil {
		t.Fatalf("mkdir schemas ro: %v", err)
	}
	t.Cleanup(func() {
		// Restore write so TempDir teardown can remove it even if
		// rollback did not (schemas/ itself is never removed by rollback).
		_ = os.Chmod(schemasDir, 0o700)
	})

	_, err := Write(context.Background(), "hello", target, Options{Force: true, Overwrite: true})
	if err == nil {
		t.Fatalf("expected copy failure; got nil")
	}

	// Restore write permission before reading — rollback may have left
	// schemas/ intact (it only removes files it copied, not dirs).
	_ = os.Chmod(schemasDir, 0o700)

	got, readErr := os.ReadFile(yamlPath)
	if readErr != nil {
		t.Fatalf("read target yaml after rollback: %v", readErr)
	}
	if !bytes.Equal(got, originalContent) {
		t.Errorf("rollback did not restore original overlord.yaml\nwant: %q\n got: %q", originalContent, got)
	}

	bakMatches, globErr := filepath.Glob(filepath.Join(target, "*.overlord-init-bak.*"))
	if globErr != nil {
		t.Fatalf("glob backups: %v", globErr)
	}
	if len(bakMatches) != 0 {
		t.Errorf("rollback left backup files behind: %v", bakMatches)
	}
}

// TestRollbackMerge_BackupMissing asserts that when a backup file is
// unexpectedly gone at rollback time, rollbackMerge does NOT remove
// the live file at origPath. Tests the data-safety invariant added
// after code-quality review of Unit 3.
func TestRollbackMerge_BackupMissing(t *testing.T) {
	target := t.TempDir()
	// Set up: a live file at origPath, no backup file.
	origContent := []byte("LIVE\n")
	if err := os.WriteFile(filepath.Join(target, "overlord.yaml"), origContent, 0o644); err != nil {
		t.Fatalf("write orig: %v", err)
	}
	// Call rollbackMerge with a backup entry whose bakPath does not exist.
	backups := []Backup{
		{Original: "overlord.yaml", Backup: "overlord.yaml.overlord-init-bak.MISSING"},
	}
	errs := rollbackMerge(target, backups, nil)
	// Expect: an error about the missing backup, AND the live file is untouched.
	if len(errs) != 1 {
		t.Fatalf("expected 1 rollback error; got %d: %v", len(errs), errs)
	}
	if !strings.Contains(errs[0].Error(), "backup missing") {
		t.Errorf("expected 'backup missing' in error; got %q", errs[0].Error())
	}
	got, readErr := os.ReadFile(filepath.Join(target, "overlord.yaml"))
	if readErr != nil {
		t.Fatalf("read live file after rollback: %v", readErr)
	}
	if !bytes.Equal(got, origContent) {
		t.Errorf("rollback destroyed live file when backup was missing\nwant: %q\n got: %q", origContent, got)
	}
}
