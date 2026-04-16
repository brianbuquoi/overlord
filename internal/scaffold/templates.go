package scaffold

import (
	"embed"
	"io/fs"
	"sort"
)

// FS embeds every file under the templates/ tree. The `all:` prefix is
// load-bearing — without it, Go's embed rules silently omit files whose
// names begin with `.` (e.g. .env.example and .gitignore), and those are
// exactly the credential-safety artefacts every scaffolded project needs.
//
//go:embed all:templates
var FS embed.FS

// templatesRoot is the path prefix under FS that holds the embedded
// template tree. Kept as a private constant so the writer (Unit 3) and
// tests share a single source of truth.
const templatesRoot = "templates"

// ListTemplates returns the sorted list of template names available in the
// embedded FS. A template is any direct child directory of templates/ —
// files at the root of FS are not returned.
//
// Callers MUST treat the returned slice as immutable; ListTemplates allocates
// a fresh slice on every call but does not defensively copy the strings.
func ListTemplates() []string {
	entries, err := fs.ReadDir(FS, templatesRoot)
	if err != nil {
		// An embed.FS backed by `//go:embed all:templates` cannot fail to
		// read its root at runtime — the tree is baked into the binary.
		// Return nil rather than panic so callers can still surface a
		// deterministic "no templates available" error.
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
