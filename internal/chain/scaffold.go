package chain

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// templatesFS embeds every file under templates/ so `overlord chain
// init` can scaffold a project without shelling out or fetching from
// the network. The `all:` prefix ensures files whose names begin with
// a dot (if any are added later) are included; today every file in
// the tree is dot-prefix-free, but the prefix is cheap insurance.
//
//go:embed all:templates
var templatesFS embed.FS

// templatesRoot is the prefix under templatesFS where scaffold
// templates live.
const templatesRoot = "templates"

// ListTemplates returns the sorted list of embedded chain templates.
// Every direct child directory of templates/ is a template.
func ListTemplates() []string {
	entries, err := fs.ReadDir(templatesFS, templatesRoot)
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names
}

// ScaffoldOptions configures the chain-init writer.
type ScaffoldOptions struct {
	// Force allows writing into a non-empty target directory as long
	// as no file the template would produce collides with an existing
	// file. Required when scaffolding into the current directory.
	Force bool
}

// ScaffoldResult summarizes a successful scaffold.
type ScaffoldResult struct {
	Target string
	Files  []string
}

// Scaffold writes the named embedded chain template into target. The
// writer refuses to scaffold over existing files unless Force is set
// and the target is empty, mirroring the pipeline-mode scaffold's
// safety model at a simpler level — chain templates are small (a few
// files each) so this package does not implement the full
// backup/overwrite ceremony the `internal/scaffold` package does.
func Scaffold(name, target string, opts ScaffoldOptions) (*ScaffoldResult, error) {
	if !isKnownTemplate(name) {
		return nil, fmt.Errorf("unknown chain template %q (available: %s)", name, strings.Join(ListTemplates(), ", "))
	}

	absTarget, err := filepath.Abs(target)
	if err != nil {
		return nil, fmt.Errorf("resolve target: %w", err)
	}
	if err := preflightTarget(absTarget, opts.Force); err != nil {
		return nil, err
	}

	root := filepath.Join(templatesRoot, name)
	var produced []string
	walkErr := fs.WalkDir(templatesFS, root, func(p string, d fs.DirEntry, walkErr error) error {
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
		dst := filepath.Join(absTarget, rel)
		if d.IsDir() {
			if err := os.MkdirAll(dst, 0o755); err != nil {
				return fmt.Errorf("mkdir %s: %w", dst, err)
			}
			return nil
		}
		data, err := templatesFS.ReadFile(p)
		if err != nil {
			return fmt.Errorf("read embedded %s: %w", p, err)
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", filepath.Dir(dst), err)
		}
		if _, err := os.Lstat(dst); err == nil {
			return fmt.Errorf("refusing to overwrite existing file %s", dst)
		}
		if err := os.WriteFile(dst, data, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", dst, err)
		}
		produced = append(produced, rel)
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}

	sort.Strings(produced)
	return &ScaffoldResult{Target: absTarget, Files: produced}, nil
}

func isKnownTemplate(name string) bool {
	for _, t := range ListTemplates() {
		if t == name {
			return true
		}
	}
	return false
}

func preflightTarget(absTarget string, force bool) error {
	fi, err := os.Lstat(absTarget)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("lstat target: %w", err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("target is a symlink (refusing): %s", absTarget)
	}
	if !fi.IsDir() {
		return fmt.Errorf("target exists and is not a directory: %s", absTarget)
	}
	entries, err := os.ReadDir(absTarget)
	if err != nil {
		return fmt.Errorf("read target directory: %w", err)
	}
	if len(entries) > 0 && !force {
		return fmt.Errorf("target directory is not empty (use --force): %s", absTarget)
	}
	return nil
}
