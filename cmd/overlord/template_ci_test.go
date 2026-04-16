package main

// Parametric CI coverage for every shipped template. Lives in cmd/overlord
// (not internal/scaffold) because the only demo runner — runDemo — is
// private to this package, and the CI assertion needs to drive the full
// scaffold → config.Load → validate → runDemo → determinism chain in one
// place. The template correctness checks that don't need runDemo still
// live in internal/scaffold/templates_test.go; this file is specifically
// the cross-layer gate.
//
// The plan (docs/plans/2026-04-16-001-feat-overlord-init-scaffolder-plan.md,
// Unit 6) prescribes this file at internal/scaffold/ci_test.go; we placed
// it here instead to avoid exporting runDemo (an internal cmd concern) or
// duplicating the broker-lifecycle wiring into a test-only helper. The
// deviation is called out in the commit message.

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/brianbuquoi/overlord/internal/config"
	"github.com/brianbuquoi/overlord/internal/scaffold"
)

// templateTimeBudget is the wall-clock ceiling for scaffold.Write +
// config.Load + validate + two runDemo invocations. The plan promises
// 60s post-install on a reasonable dev machine (R1/SLA); this budget
// gives 6x headroom on CI hardware. If the gate flakes, widen toward
// 20s — the plan's upper tolerance before the 60s promise is unverified.
const templateTimeBudget = 10 * time.Second

// realProviderOpenRe / realProviderCloseRe mirror the banner regexes in
// internal/scaffold/templates_test.go. Duplicated here (not exported)
// because the scaffold package deliberately keeps them private: the
// banner shape is a template-layer contract, not a writer-layer one.
var (
	realProviderOpenRe  = regexp.MustCompile(`(?m)^[ \t]*# === real provider:.*===$`)
	realProviderCloseRe = regexp.MustCompile(`(?m)^[ \t]*# === end real provider ===$`)
	authBannerRe        = regexp.MustCompile(`(?m)^[ \t]*# === auth \(ENABLE BEFORE EXPOSING BEYOND LOCALHOST\) ===$`)
)

// TestShippedTemplates is the parametric CI gate. For every template in
// scaffold.ListTemplates() it runs the end-to-end first-impression path
// and asserts every success criterion called out in the plan's Unit 6
// approach section. Subtests are named after the template so a failure
// localizes immediately (e.g. `go test -run TestShippedTemplates/hello`).
func TestShippedTemplates(t *testing.T) {
	// goleak runs at the outer test level so goroutines leaked from ANY
	// of the inner subtests fail the parent. runDemo's documented drain
	// contract is supposed to guarantee zero residual goroutines; this
	// is the enforcement.
	//
	// Known-clean leaks we ignore:
	//  * nothing yet; if the broker's metrics.RegistrerGatherer background
	//    goroutine shows up here, add it with goleak.IgnoreTopFunction.
	defer goleak.VerifyNone(t)

	for _, name := range scaffold.ListTemplates() {
		name := name
		t.Run(name, func(t *testing.T) {
			start := time.Now()
			assertTemplateFullFlow(t, name)
			elapsed := time.Since(start)
			if elapsed > templateTimeBudget {
				t.Errorf("template %s: full CI flow took %s (budget %s)", name, elapsed, templateTimeBudget)
			}
			t.Logf("template %s: full CI flow wall-clock %s (budget %s)", name, elapsed, templateTimeBudget)
		})
	}
}

// assertTemplateFullFlow runs every Unit 6 assertion against a single
// scaffolded template. Split out so TestShippedTemplates stays readable
// and so the live-integration test (live_integration_test.go under the
// `integration` build tag) can reuse the first half (scaffold + swap)
// without re-running the mock demo.
func assertTemplateFullFlow(t *testing.T, name string) {
	t.Helper()

	target := filepath.Join(t.TempDir(), name)

	// (1) scaffold.Write into a fresh tempdir with zero options.
	res, err := scaffold.Write(context.Background(), name, target, scaffold.Options{})
	if err != nil {
		t.Fatalf("scaffold.Write(%s): %v", name, err)
	}
	if res == nil || res.Target == "" {
		t.Fatalf("scaffold.Write(%s): empty result", name)
	}

	configPath := filepath.Join(target, "overlord.yaml")

	// (2) config.Load returns a valid config.
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("config.Load(%s): %v", name, err)
	}
	if len(cfg.Pipelines) == 0 {
		t.Fatalf("config.Load(%s): zero pipelines", name)
	}
	if len(cfg.Agents) == 0 {
		t.Fatalf("config.Load(%s): zero agents", name)
	}

	// (3) Run the same validation path `overlord validate` uses: compile
	//     the contract registry and resolve agent bindings via buildBroker.
	//     buildBroker is the same entry point runDemo uses, so this also
	//     proves the agent fixtures validate against the schema registry
	//     (the mock adapter's constructor re-validates them).
	basePath := configBasePath(configPath)
	if _, err := buildContractRegistry(cfg, basePath); err != nil {
		t.Fatalf("buildContractRegistry(%s): %v", name, err)
	}

	// (4) runDemo succeeds and is deterministic: two consecutive runs
	//     produce byte-identical pipeline result output on stdout.
	first, err := runDemo(context.Background(), target, io.Discard)
	if err != nil {
		t.Fatalf("runDemo(%s) first run: %v", name, err)
	}
	if len(first) == 0 {
		t.Fatalf("runDemo(%s) first run: empty result payload", name)
	}
	second, err := runDemo(context.Background(), target, io.Discard)
	if err != nil {
		t.Fatalf("runDemo(%s) second run: %v", name, err)
	}
	if !bytes.Equal(first, second) {
		t.Errorf("runDemo(%s) not deterministic:\nfirst:  %s\nsecond: %s", name, string(first), string(second))
	}

	// (5) Migration swap: uncomment the real-provider block AND rewrite
	//     every stage's agent reference from `<id>-mock` to `<id>`. The
	//     resulting YAML must still parse via config.Load. Runtime
	//     execution is NOT asserted — the live-integration test does
	//     that under the `integration` build tag.
	assertMigrationSwapParses(t, name, target, cfg)

	// (6) The commented `auth:` block is present (safety contract).
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read rendered config: %v", err)
	}
	body := string(raw)
	if !authBannerRe.MatchString(body) {
		t.Errorf("template %s: auth block banner missing from overlord.yaml", name)
	}
	if !strings.Contains(body, "auth:") {
		t.Errorf("template %s: overlord.yaml missing a commented `auth:` key", name)
	}

	// (7) .gitignore contains the required credential-hygiene entries.
	assertGitignoreContract(t, name, target)

	// (8) .env.example placeholder does NOT start with `sk-` (secret
	//     scanner hazard) and each _KEY= line has a non-empty value.
	assertEnvExampleSafe(t, name, target)

	// Sanity: the writer's documented byte-identical contract also
	// covers re-runs into a fresh directory. We don't re-assert that
	// here — internal/scaffold/writer_test.go owns that guarantee.
}

// assertMigrationSwapParses implements Unit 6 step (5). It reads the
// rendered config, strips the comment prefix from every line inside the
// real-provider banner block, rewrites each stage's `agent: <id>-mock`
// reference to `agent: <id>`, writes the mutated YAML back, and asserts
// config.Load still succeeds. The assertion is YAML/schema/registry
// validity — missing environment variables are not the failure mode
// under test here.
func assertMigrationSwapParses(t *testing.T, name, target string, origCfg *config.Config) {
	t.Helper()
	configPath := filepath.Join(target, "overlord.yaml")
	rawBytes, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	swapped := uncommentRealProviderBlock(t, string(rawBytes))

	// Rewrite `agent: <id>-mock` → `agent: <id>` for every stage in the
	// scaffolded config. We walk the ORIGINAL config so we know which
	// agent IDs to rewrite — the comment stripping doesn't change them.
	for _, p := range origCfg.Pipelines {
		for _, s := range p.Stages {
			real := strings.TrimSuffix(s.Agent, "-mock")
			if real == s.Agent {
				continue // stage isn't bound to a -mock agent
			}
			swapped = strings.ReplaceAll(
				swapped,
				"agent: "+s.Agent,
				"agent: "+real,
			)
		}
	}

	if err := os.WriteFile(configPath, []byte(swapped), 0o644); err != nil {
		t.Fatalf("write swapped config: %v", err)
	}
	newCfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("config.Load after migration swap (%s): %v\n--- yaml ---\n%s", name, err, swapped)
	}

	// Every stage must now reference a real-provider agent. If any still
	// carries a `-mock` suffix OR resolves to a mock provider, the swap
	// didn't take.
	providers := make(map[string]string, len(newCfg.Agents))
	for _, a := range newCfg.Agents {
		providers[a.ID] = a.Provider
	}
	for _, p := range newCfg.Pipelines {
		for _, s := range p.Stages {
			if strings.HasSuffix(s.Agent, "-mock") {
				t.Errorf("template %s: stage %q still references mock agent %q after swap", name, s.ID, s.Agent)
			}
			prov, ok := providers[s.Agent]
			if !ok {
				t.Errorf("template %s: stage %q references unknown agent %q after swap", name, s.ID, s.Agent)
				continue
			}
			if prov == "mock" {
				t.Errorf("template %s: stage %q targets agent %q but its provider is still mock", name, s.ID, s.Agent)
			}
		}
	}

	// Restore the original rendered YAML so subsequent assertions in
	// the caller operate against the as-scaffolded file rather than the
	// swapped version we just wrote.
	if err := os.WriteFile(configPath, rawBytes, 0o644); err != nil {
		t.Fatalf("restore config after swap: %v", err)
	}
}

// uncommentRealProviderBlock is a local reimplementation of the helper
// with the same name in internal/scaffold/templates_test.go. It is not
// exported from that package (it's a test helper), so cmd/overlord
// can't depend on it directly. The implementations must stay in lockstep;
// a shared exported helper would widen the scaffold package's public
// surface for a single internal use.
func uncommentRealProviderBlock(t *testing.T, body string) string {
	t.Helper()
	openLoc := realProviderOpenRe.FindStringIndex(body)
	closeLoc := realProviderCloseRe.FindStringIndex(body)
	if openLoc == nil || closeLoc == nil {
		t.Fatal("real-provider banners not found — template structure broken")
	}
	before := body[:openLoc[1]]
	between := body[openLoc[1]:closeLoc[0]]
	after := body[closeLoc[0]:]

	var out strings.Builder
	for _, line := range strings.Split(between, "\n") {
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
	uncommented := strings.TrimSuffix(out.String(), "\n")
	return before + uncommented + after
}

// assertGitignoreContract verifies the scaffolded .gitignore excludes
// every credential-hygiene entry the plan commits us to.
func assertGitignoreContract(t *testing.T, name, target string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(target, ".gitignore"))
	if err != nil {
		t.Fatalf("read .gitignore: %v", err)
	}
	body := string(data)
	required := []string{
		".env",
		".env.*",
		"!.env.example",
		"*.overlord-init-bak*",
		"fixtures/*.secret.json",
	}
	lines := strings.Split(body, "\n")
	lineSet := make(map[string]bool, len(lines))
	for _, l := range lines {
		lineSet[strings.TrimSpace(l)] = true
	}
	for _, want := range required {
		if !lineSet[want] {
			t.Errorf("template %s: .gitignore missing required entry %q\nfull body:\n%s",
				name, want, body)
		}
	}
}

// assertEnvExampleSafe verifies the .env.example shipped with the
// template does NOT include a placeholder that looks like a real
// Anthropic / OpenAI key (starts with `sk-`), and that every _KEY= line
// has a non-empty value. The sk- check guards against secret-scanner
// false negatives on an accidental commit of the unchanged file.
func assertEnvExampleSafe(t *testing.T, name, target string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(target, ".env.example"))
	if err != nil {
		t.Fatalf("read .env.example: %v", err)
	}
	for i, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		eq := strings.Index(trimmed, "=")
		if eq < 0 {
			continue
		}
		val := strings.TrimSpace(trimmed[eq+1:])
		val = strings.Trim(val, `"'`)
		if strings.HasPrefix(val, "sk-") {
			t.Errorf("template %s: .env.example line %d placeholder %q starts with sk- (secret-scanner hazard)",
				name, i+1, val)
		}
		// Only enforce non-empty values for lines that look like key
		// assignments (the _KEY= convention the plan commits us to).
		if strings.Contains(strings.ToUpper(trimmed[:eq]), "_KEY") && val == "" {
			t.Errorf("template %s: .env.example line %d has empty value for a _KEY placeholder",
				name, i+1)
		}
	}
}

// TestInitHelloTimeBudget asserts the full init path (scaffold + demo)
// for the `hello` template stays well under the post-install 60-second
// promise on CI hardware. 10 seconds is the documented budget; 6× safety
// margin below the user-facing SLA.
//
// Acknowledgement: the plan's Unit 6 verification block mentions that a
// deliberately-introduced goroutine leak in runDemo OR a deliberately
// broken fixture in a template should surface as a test failure. This
// test and TestShippedTemplates collectively provide that gate: the
// goleak.VerifyNone(t) in TestShippedTemplates catches the goroutine
// leak, and buildBroker/mock-adapter fixture validation (exercised by
// runDemo) catches the broken-fixture case.
func TestInitHelloTimeBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("time-budget guard skipped in -short mode")
	}
	dir := filepath.Join(t.TempDir(), "hello-budget")

	start := time.Now()
	if _, err := scaffold.Write(context.Background(), "hello", dir, scaffold.Options{}); err != nil {
		t.Fatalf("scaffold.Write: %v", err)
	}
	if _, err := runDemo(context.Background(), dir, io.Discard); err != nil {
		t.Fatalf("runDemo: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed > templateTimeBudget {
		t.Errorf("init hello time budget exceeded: %s > %s", elapsed, templateTimeBudget)
	}
	t.Logf("init hello wall-clock: %s (budget %s)", elapsed, templateTimeBudget)
}
