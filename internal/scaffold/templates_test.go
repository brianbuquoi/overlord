package scaffold

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"text/template"

	"github.com/brianbuquoi/overlord/internal/config"
	"github.com/brianbuquoi/overlord/internal/contract"
)

// requiredFiles enumerates the paths (relative to a template root) that
// every first-party template MUST contain after rendering. The test
// asserts each one exists on disk after materializing a template; the
// list is the single source of truth for the embed-all-regression guard.
var requiredFiles = []string{
	"overlord.yaml.tmpl",
	"sample_payload.json",
	".env.example",
	".gitignore",
}

// requiredDirs enumerates directories each template must ship.
var requiredDirs = []string{
	"schemas",
	"fixtures",
}

// requiredGitignoreLines enumerates the exact lines every template's
// .gitignore must contain. We check for membership, not a strict
// equality on the whole file, so the templates can carry comments.
var requiredGitignoreLines = []string{
	".env",
	".env.*",
	"!.env.example",
	"*.overlord-init-bak*",
	"fixtures/*.secret.json",
}

// templateContext mirrors the scaffolder's planned context shape. Unit 3
// owns the real writer; keep this definition in sync when that lands.
type templateContext struct {
	Model        string
	TemplateName string
}

// materializeTemplate copies a template from the embed FS into dir,
// rendering .tmpl files through text/template against ctx and stripping
// the .tmpl suffix from output paths. Non-.tmpl files are copied
// verbatim. This is a test-only minimal stand-in for Unit 3's writer.
func materializeTemplate(t *testing.T, name, dir string, ctx templateContext) {
	t.Helper()
	root := filepath.Join(templatesRoot, name)
	if err := fs.WalkDir(FS, root, func(p string, d fs.DirEntry, walkErr error) error {
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
		// embed paths use forward slashes on all platforms. filepath.Rel
		// returns OS-native separators on disk, which is what we want for
		// the output path; for the embed Open call we stick with slashes.
		dst := filepath.Join(dir, rel)
		if d.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}

		data, err := FS.ReadFile(p)
		if err != nil {
			return err
		}
		if strings.HasSuffix(dst, ".tmpl") {
			tmpl, err := template.New(rel).Option("missingkey=error").Parse(string(data))
			if err != nil {
				return err
			}
			var buf bytes.Buffer
			if err := tmpl.Execute(&buf, ctx); err != nil {
				return err
			}
			data = buf.Bytes()
			dst = strings.TrimSuffix(dst, ".tmpl")
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		return os.WriteFile(dst, data, 0o644)
	}); err != nil {
		t.Fatalf("materialize %q: %v", name, err)
	}
}

// readTemplateYAML returns the raw (un-rendered) contents of the template's
// overlord.yaml.tmpl for structural assertions that shouldn't care about
// model substitution.
func readTemplateYAML(t *testing.T, name string) string {
	t.Helper()
	data, err := FS.ReadFile(filepath.Join(templatesRoot, name, "overlord.yaml.tmpl"))
	if err != nil {
		t.Fatalf("read %s/overlord.yaml.tmpl: %v", name, err)
	}
	return string(data)
}

// renderedTemplate renders overlord.yaml.tmpl through text/template against
// the default context and returns the rendered bytes as a string.
func renderedTemplate(t *testing.T, name string) string {
	t.Helper()
	raw := readTemplateYAML(t, name)
	tmpl, err := template.New(name).Option("missingkey=error").Parse(raw)
	if err != nil {
		t.Fatalf("parse %s template: %v", name, err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, templateContext{
		Model:        DefaultAnthropicModel,
		TemplateName: name,
	}); err != nil {
		t.Fatalf("execute %s template: %v", name, err)
	}
	return buf.String()
}

// TestListTemplates_Sorted verifies the public ListTemplates accessor
// returns exactly the two first-party templates in sorted order. Adding
// a third template is a conscious change — this test forces an update.
func TestListTemplates_Sorted(t *testing.T) {
	got := ListTemplates()
	want := []string{"hello", "summarize"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ListTemplates = %v, want %v", got, want)
	}
}

// TestEveryTemplate_Structure exercises the per-template structural
// contract: every required file present, every required dir present, no
// stray dot-prefix files dropped by a missing `all:` prefix.
func TestEveryTemplate_Structure(t *testing.T) {
	for _, name := range ListTemplates() {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			materializeTemplate(t, name, dir, templateContext{
				Model:        DefaultAnthropicModel,
				TemplateName: name,
			})

			for _, rel := range requiredFiles {
				// overlord.yaml.tmpl is stripped to overlord.yaml at
				// render time — assert the rendered name.
				checkName := strings.TrimSuffix(rel, ".tmpl")
				p := filepath.Join(dir, checkName)
				fi, err := os.Stat(p)
				if err != nil {
					t.Errorf("missing required file %s: %v", checkName, err)
					continue
				}
				if fi.Size() == 0 {
					t.Errorf("required file %s is empty", checkName)
				}
			}
			for _, d := range requiredDirs {
				p := filepath.Join(dir, d)
				fi, err := os.Stat(p)
				if err != nil {
					t.Errorf("missing required dir %s: %v", d, err)
					continue
				}
				if !fi.IsDir() {
					t.Errorf("expected %s to be a directory", d)
				}
				// Dir must be non-empty — a template with zero schemas or
				// zero fixtures is meaningless.
				entries, err := os.ReadDir(p)
				if err != nil {
					t.Errorf("read %s: %v", d, err)
					continue
				}
				if len(entries) == 0 {
					t.Errorf("required dir %s is empty", d)
				}
			}

			// Belt-and-suspenders: no .tmpl files should remain after render.
			_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, _ error) error {
				if !d.IsDir() && strings.HasSuffix(p, ".tmpl") {
					t.Errorf("found stray .tmpl file after render: %s", p)
				}
				return nil
			})
		})
	}
}

// TestEveryTemplate_GitignoreContent enforces the exact required entries
// in every template's .gitignore — these protect credentials and
// backup-file leakage and are part of the Unit 2 deliverable contract.
func TestEveryTemplate_GitignoreContent(t *testing.T) {
	for _, name := range ListTemplates() {
		t.Run(name, func(t *testing.T) {
			data, err := FS.ReadFile(filepath.Join(templatesRoot, name, ".gitignore"))
			if err != nil {
				t.Fatalf("read .gitignore: %v", err)
			}
			body := string(data)
			for _, line := range requiredGitignoreLines {
				// Check line appears as a full line (not substring) so
				// partial matches don't silently pass.
				found := false
				for _, got := range strings.Split(body, "\n") {
					if strings.TrimSpace(got) == line {
						found = true
						break
					}
				}
				if !found {
					t.Errorf(".gitignore missing required line %q\nfull body:\n%s", line, body)
				}
			}
		})
	}
}

// TestEveryTemplate_EnvExamplePlaceholder guards against accidentally
// using an sk-prefixed placeholder, which secret scanners would otherwise
// greenlight. The placeholder must be obviously-fake to trip scanners if
// committed unchanged.
func TestEveryTemplate_EnvExamplePlaceholder(t *testing.T) {
	for _, name := range ListTemplates() {
		t.Run(name, func(t *testing.T) {
			data, err := FS.ReadFile(filepath.Join(templatesRoot, name, ".env.example"))
			if err != nil {
				t.Fatalf("read .env.example: %v", err)
			}
			for i, line := range strings.Split(string(data), "\n") {
				trimmed := strings.TrimSpace(line)
				if trimmed == "" || strings.HasPrefix(trimmed, "#") {
					continue
				}
				// Extract the value after the first "=".
				eq := strings.Index(trimmed, "=")
				if eq < 0 {
					continue
				}
				val := strings.TrimSpace(trimmed[eq+1:])
				val = strings.Trim(val, `"'`)
				if strings.HasPrefix(val, "sk-") {
					t.Errorf("line %d: placeholder %q starts with sk- (secret-scanner hazard)", i+1, val)
				}
				if val == "" {
					t.Errorf("line %d: env value is empty; placeholder should be REPLACE_ME_... style", i+1)
				}
			}
		})
	}
}

// realProviderBannerRe matches the opening banner used to delimit the
// commented real-provider block in every template. The closing banner is
// matched separately; both must appear exactly once per template.
var (
	realProviderOpenRe  = regexp.MustCompile(`(?m)^[ \t]*# === real provider:.*===$`)
	realProviderCloseRe = regexp.MustCompile(`(?m)^[ \t]*# === end real provider ===$`)
	authOpenRe          = regexp.MustCompile(`(?m)^[ \t]*# === auth \(ENABLE BEFORE EXPOSING BEYOND LOCALHOST\) ===$`)
	authCloseRe         = regexp.MustCompile(`(?m)^[ \t]*# === end auth ===$`)
)

// TestEveryTemplate_RealProviderBannerDelimited asserts the real-provider
// block banner markers appear exactly once and bracket a body where every
// non-blank line is currently comment-prefixed.
func TestEveryTemplate_RealProviderBannerDelimited(t *testing.T) {
	for _, name := range ListTemplates() {
		t.Run(name, func(t *testing.T) {
			body := renderedTemplate(t, name)

			openMatches := realProviderOpenRe.FindAllStringIndex(body, -1)
			closeMatches := realProviderCloseRe.FindAllStringIndex(body, -1)
			if len(openMatches) != 1 {
				t.Fatalf("expected exactly 1 real-provider opening banner, got %d", len(openMatches))
			}
			if len(closeMatches) != 1 {
				t.Fatalf("expected exactly 1 real-provider closing banner, got %d", len(closeMatches))
			}
			if closeMatches[0][0] <= openMatches[0][1] {
				t.Fatal("closing banner appears before opening banner")
			}

			// The body BETWEEN the banners must be entirely comment-prefixed.
			between := body[openMatches[0][1]:closeMatches[0][0]]
			for i, line := range strings.Split(between, "\n") {
				trimmed := strings.TrimSpace(line)
				if trimmed == "" {
					continue
				}
				if !strings.HasPrefix(trimmed, "#") {
					t.Errorf("real-provider body line %d is not comment-prefixed: %q", i+1, line)
				}
			}

			// It should contain `provider: anthropic` in commented form.
			if !strings.Contains(between, "provider: anthropic") {
				t.Errorf("real-provider block does not mention provider: anthropic\nbetween:\n%s", between)
			}
		})
	}
}

// TestEveryTemplate_AuthCommented asserts the commented auth: block
// delimited by the auth banner is present in every template.
func TestEveryTemplate_AuthCommented(t *testing.T) {
	for _, name := range ListTemplates() {
		t.Run(name, func(t *testing.T) {
			body := renderedTemplate(t, name)
			if !authOpenRe.MatchString(body) {
				t.Error("auth opening banner not found")
			}
			if !authCloseRe.MatchString(body) {
				t.Error("auth closing banner not found")
			}

			openLoc := authOpenRe.FindStringIndex(body)
			closeLoc := authCloseRe.FindStringIndex(body)
			if openLoc == nil || closeLoc == nil {
				return
			}
			if closeLoc[0] <= openLoc[1] {
				t.Fatal("auth closing banner before opening banner")
			}
			between := body[openLoc[1]:closeLoc[0]]
			// The body must contain `auth:` under the banner as a commented
			// line — and every non-blank line must be comment-prefixed.
			sawAuthKey := false
			for _, line := range strings.Split(between, "\n") {
				trimmed := strings.TrimSpace(line)
				if trimmed == "" {
					continue
				}
				if !strings.HasPrefix(trimmed, "#") {
					t.Errorf("auth block line is not comment-prefixed: %q", line)
				}
				if strings.Contains(trimmed, "auth:") {
					sawAuthKey = true
				}
			}
			if !sawAuthKey {
				t.Errorf("auth block does not contain a commented `auth:` key\nbetween:\n%s", between)
			}
		})
	}
}

// TestEveryTemplate_RenderedConfigLoads materializes every template into
// a tempdir and runs config.Load on the rendered overlord.yaml. This is
// the strongest integration check Unit 2 can run without Unit 3's writer:
// the YAML parses, schema paths resolve, agent/stage bindings validate.
func TestEveryTemplate_RenderedConfigLoads(t *testing.T) {
	for _, name := range ListTemplates() {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			materializeTemplate(t, name, dir, templateContext{
				Model:        DefaultAnthropicModel,
				TemplateName: name,
			})
			cfgPath := filepath.Join(dir, "overlord.yaml")
			cfg, err := config.Load(cfgPath)
			if err != nil {
				t.Fatalf("config.Load: %v", err)
			}
			if len(cfg.Pipelines) == 0 {
				t.Fatal("rendered config has zero pipelines")
			}
			if len(cfg.Agents) == 0 {
				t.Fatal("rendered config has zero agents")
			}
			// Every stage in every pipeline must bind to an agent whose ID
			// ends in "-mock" — the scaffolded default runs via the mock.
			for _, p := range cfg.Pipelines {
				for _, s := range p.Stages {
					if !strings.HasSuffix(s.Agent, "-mock") {
						t.Errorf("pipeline %q stage %q binds to %q; expected a *-mock agent", p.Name, s.ID, s.Agent)
					}
				}
			}
		})
	}
}

// TestEveryTemplate_ModelSubstitution asserts the {{ .Model }} placeholder
// resolves to DefaultAnthropicModel after render. This is the guard
// against the constant drifting out of the scaffolder.
func TestEveryTemplate_ModelSubstitution(t *testing.T) {
	for _, name := range ListTemplates() {
		t.Run(name, func(t *testing.T) {
			raw := readTemplateYAML(t, name)
			if !strings.Contains(raw, "{{ .Model }}") {
				t.Fatalf("template does not reference {{ .Model }} (got rendered placeholder or missing block)")
			}
			rendered := renderedTemplate(t, name)
			if !strings.Contains(rendered, DefaultAnthropicModel) {
				t.Errorf("rendered template missing DefaultAnthropicModel %q\nrendered:\n%s", DefaultAnthropicModel, rendered)
			}
			if strings.Contains(rendered, "{{ .Model }}") {
				t.Error("rendered template still contains unresolved {{ .Model }}")
			}
		})
	}
}

// uncommentRealProviderBlock strips the leading "# " (or "#" when the
// line is a bare "#") from each line BETWEEN the real-provider banners,
// returning the transformed body. The banner lines themselves are left
// as-is; the caller is responsible for removing or ignoring them if YAML
// parsing requires it (we keep them as YAML comments, which is valid).
func uncommentRealProviderBlock(t *testing.T, body string) string {
	t.Helper()
	openLoc := realProviderOpenRe.FindStringIndex(body)
	closeLoc := realProviderCloseRe.FindStringIndex(body)
	if openLoc == nil || closeLoc == nil {
		t.Fatal("banners not found — template structure broken")
	}
	before := body[:openLoc[1]]
	between := body[openLoc[1]:closeLoc[0]]
	after := body[closeLoc[0]:]

	var out strings.Builder
	for _, line := range strings.Split(between, "\n") {
		// Preserve leading whitespace so YAML indentation is unchanged;
		// strip the first "# " (or a bare "#") that follows the indent.
		idx := 0
		for idx < len(line) && (line[idx] == ' ' || line[idx] == '\t') {
			idx++
		}
		rest := line[idx:]
		switch {
		case strings.HasPrefix(rest, "# "):
			out.WriteString(line[:idx])
			out.WriteString(strings.TrimPrefix(rest, "# "))
		case rest == "#":
			out.WriteString(line[:idx])
		default:
			out.WriteString(line)
		}
		out.WriteString("\n")
	}
	// strings.Split of "a\nb\n" yields ["a","b",""] — the trailing "" adds
	// an extra \n above. Trim it so we don't double up on the join.
	uncommented := strings.TrimSuffix(out.String(), "\n")
	return before + uncommented + after
}

// TestEveryTemplate_MigrationSwap is the regression test for R9: after
// uncommenting the real-provider block AND swapping every stage's agent
// reference from "<id>-mock" to "<id>", the resulting YAML must still
// parse cleanly with config.Load. This is the contract the migration
// doc promises users.
func TestEveryTemplate_MigrationSwap(t *testing.T) {
	for _, name := range ListTemplates() {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			materializeTemplate(t, name, dir, templateContext{
				Model:        DefaultAnthropicModel,
				TemplateName: name,
			})

			cfgPath := filepath.Join(dir, "overlord.yaml")
			raw, err := os.ReadFile(cfgPath)
			if err != nil {
				t.Fatalf("read rendered config: %v", err)
			}
			swapped := uncommentRealProviderBlock(t, string(raw))

			// Load the scaffolded config once to know which mock-agent IDs
			// to rewrite. Then rewrite `agent: <id>-mock` → `agent: <id>`.
			origCfg, err := config.Load(cfgPath)
			if err != nil {
				t.Fatalf("config.Load baseline: %v", err)
			}
			for _, p := range origCfg.Pipelines {
				for _, s := range p.Stages {
					real := strings.TrimSuffix(s.Agent, "-mock")
					// Be precise: only replace the full `agent: <id>-mock`
					// line so we don't accidentally touch a comment or a
					// fixtures key that happens to share the name.
					swapped = strings.ReplaceAll(
						swapped,
						"agent: "+s.Agent,
						"agent: "+real,
					)
				}
			}

			// Write back and re-Load.
			if err := os.WriteFile(cfgPath, []byte(swapped), 0o644); err != nil {
				t.Fatalf("write swapped: %v", err)
			}
			newCfg, err := config.Load(cfgPath)
			if err != nil {
				t.Fatalf("config.Load after migration swap: %v\n--- yaml ---\n%s", err, swapped)
			}

			// Every pipeline stage must now bind to a real-provider agent
			// (no -mock suffix) AND that agent must exist in the final
			// config with a non-mock provider.
			realAgents := make(map[string]string, len(newCfg.Agents))
			for _, a := range newCfg.Agents {
				realAgents[a.ID] = a.Provider
			}
			for _, p := range newCfg.Pipelines {
				for _, s := range p.Stages {
					if strings.HasSuffix(s.Agent, "-mock") {
						t.Errorf("pipeline %q stage %q still references mock agent %q after swap", p.Name, s.ID, s.Agent)
					}
					prov, ok := realAgents[s.Agent]
					if !ok {
						t.Errorf("stage %q references unknown agent %q after swap", s.ID, s.Agent)
						continue
					}
					if prov == "mock" {
						t.Errorf("stage %q targets agent %q but its provider is still mock", s.ID, s.Agent)
					}
				}
			}
		})
	}
}

// TestEveryTemplate_FixturesValidateAgainstSchema walks each template's
// agents[].fixtures declarations and asserts each fixture passes
// validate.ValidateOutput against the stage's output_schema. This is the
// same assertion the mock adapter's constructor makes; catching it here
// means fixture breakage surfaces at `go test` time without needing to
// build a real adapter + broker.
func TestEveryTemplate_FixturesValidateAgainstSchema(t *testing.T) {
	for _, name := range ListTemplates() {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			materializeTemplate(t, name, dir, templateContext{
				Model:        DefaultAnthropicModel,
				TemplateName: name,
			})
			cfgPath := filepath.Join(dir, "overlord.yaml")
			cfg, err := config.Load(cfgPath)
			if err != nil {
				t.Fatalf("config.Load: %v", err)
			}
			reg, err := contract.NewRegistry(cfg.SchemaRegistry, dir)
			if err != nil {
				t.Fatalf("contract.NewRegistry: %v", err)
			}
			validator := contract.NewValidator(reg)

			// Build a stage index keyed on stage ID so we can look up the
			// output_schema for each fixture entry.
			type outSchema struct {
				Name    string
				Version contract.SchemaVersion
			}
			stageOutputs := map[string]outSchema{}
			for _, p := range cfg.Pipelines {
				for _, s := range p.Stages {
					stageOutputs[s.ID] = outSchema{
						Name:    s.OutputSchema.Name,
						Version: contract.SchemaVersion(s.OutputSchema.Version),
					}
				}
			}

			for _, a := range cfg.Agents {
				for stageID, rel := range a.Fixtures {
					out, ok := stageOutputs[stageID]
					if !ok {
						t.Errorf("agent %q declares fixture for stage %q but no such stage exists", a.ID, stageID)
						continue
					}
					path := filepath.Join(dir, rel)
					data, err := os.ReadFile(path)
					if err != nil {
						t.Errorf("read fixture %s: %v", path, err)
						continue
					}
					if err := validator.ValidateOutput(out.Name, out.Version, out.Version, data); err != nil {
						t.Errorf("fixture %s (stage %q, schema %s@%s) failed validation: %v",
							path, stageID, out.Name, out.Version, err)
					}
				}
			}
		})
	}
}

// TestEveryTemplate_SamplePayloadValidatesAgainstFirstStageInput asserts
// sample_payload.json is valid JSON AND matches the first stage's
// input_schema. This is the invariant the auto-run demo relies on.
func TestEveryTemplate_SamplePayloadValidatesAgainstFirstStageInput(t *testing.T) {
	for _, name := range ListTemplates() {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			materializeTemplate(t, name, dir, templateContext{
				Model:        DefaultAnthropicModel,
				TemplateName: name,
			})
			cfgPath := filepath.Join(dir, "overlord.yaml")
			cfg, err := config.Load(cfgPath)
			if err != nil {
				t.Fatalf("config.Load: %v", err)
			}
			reg, err := contract.NewRegistry(cfg.SchemaRegistry, dir)
			if err != nil {
				t.Fatalf("contract.NewRegistry: %v", err)
			}
			validator := contract.NewValidator(reg)

			if len(cfg.Pipelines) == 0 || len(cfg.Pipelines[0].Stages) == 0 {
				t.Fatal("no pipelines/stages in rendered config")
			}
			first := cfg.Pipelines[0].Stages[0]

			payloadPath := filepath.Join(dir, "sample_payload.json")
			raw, err := os.ReadFile(payloadPath)
			if err != nil {
				t.Fatalf("read sample_payload.json: %v", err)
			}
			var check any
			if err := json.Unmarshal(raw, &check); err != nil {
				t.Fatalf("sample_payload.json is not valid JSON: %v", err)
			}

			ver := contract.SchemaVersion(first.InputSchema.Version)
			if err := validator.ValidateInput(first.InputSchema.Name, ver, ver, raw); err != nil {
				t.Errorf("sample_payload.json failed input-schema validation for stage %q: %v",
					first.ID, err)
			}
		})
	}
}
