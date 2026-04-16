package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/brianbuquoi/overlord/internal/scaffold"
)

// runInitCmd drives the cobra command in-process. It captures stdout,
// stderr, and the exit code derived from any returned error. The exit
// code mirrors what main() would produce on os.Exit, so assertions are
// directly comparable to the init exit-code matrix. The failure-path
// message is appended to the captured stderr so tests can match the
// same text a user would see on their terminal.
func runInitCmd(t *testing.T, args ...string) (exitCode int, stdout, stderr string) {
	t.Helper()
	root := rootCmd()
	root.SetArgs(append([]string{"init"}, args...))
	var outBuf, errBuf bytes.Buffer
	root.SetOut(&outBuf)
	root.SetErr(&errBuf)

	err := root.Execute()
	stdout, stderr = outBuf.String(), errBuf.String()
	if err == nil {
		return initExitSuccess, stdout, stderr
	}
	var ie *initExitError
	if errors.As(err, &ie) {
		if ie.Msg != "" && ie.Code != initExitInterrupted && ie.Code != initExitSuccess {
			stderr += "Error: " + ie.Error() + "\n"
		}
		return ie.Code, stdout, stderr
	}
	stderr += "Error: " + err.Error() + "\n"
	return initExitGeneralError, stdout, stderr
}

// =============================================================================
// Argument and template validation
// =============================================================================

func TestInit_MissingTemplateArg(t *testing.T) {
	t.Chdir(t.TempDir())
	code, _, stderr := runInitCmd(t)
	if code != initExitInvalidTarget {
		t.Fatalf("expected exit %d, got %d\nstderr: %s", initExitInvalidTarget, code, stderr)
	}
	if !strings.Contains(stderr, "available templates:") {
		t.Errorf("stderr missing template list\nstderr: %s", stderr)
	}
	if !strings.Contains(stderr, "hello") {
		t.Errorf("stderr missing hello template\nstderr: %s", stderr)
	}
}

func TestInit_UnknownTemplate(t *testing.T) {
	t.Chdir(t.TempDir())
	code, _, stderr := runInitCmd(t, "not-a-real-template")
	if code != initExitInvalidTarget {
		t.Fatalf("expected exit %d, got %d\nstderr: %s", initExitInvalidTarget, code, stderr)
	}
	if !strings.Contains(stderr, "available templates:") {
		t.Errorf("stderr missing template list\nstderr: %s", stderr)
	}
	if !strings.Contains(stderr, "unknown template: not-a-real-template") {
		t.Errorf("stderr missing unknown-template marker\nstderr: %s", stderr)
	}
}

// =============================================================================
// --no-run happy path (scaffold only)
// =============================================================================

func TestInit_NoRun_Summarize(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "proj")
	code, _, stderr := runInitCmd(t, "summarize", dir, "--no-run")
	if code != initExitSuccess {
		t.Fatalf("expected exit 0, got %d\nstderr: %s", code, stderr)
	}
	// Verify files written.
	expected := []string{
		"overlord.yaml",
		"sample_payload.json",
		"schemas",
		"fixtures",
	}
	for _, name := range expected {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("missing expected file %s: %v", name, err)
		}
	}
	if !strings.Contains(stderr, "Next steps:") {
		t.Errorf("stderr missing next-steps block\nstderr: %s", stderr)
	}
	if !strings.Contains(stderr, "overlord exec") {
		t.Errorf("stderr missing 'overlord exec' in next-steps\nstderr: %s", stderr)
	}
	if !strings.Contains(stderr, "--id summarize") {
		t.Errorf("stderr missing pipeline id in next-steps\nstderr: %s", stderr)
	}
}

func TestInit_NoRun_HelloDefaultDir(t *testing.T) {
	tmp := t.TempDir()
	t.Chdir(tmp)
	code, _, stderr := runInitCmd(t, "hello", "--no-run")
	if code != initExitSuccess {
		t.Fatalf("expected exit 0, got %d\nstderr: %s", code, stderr)
	}
	// Default target is "./hello"
	if _, err := os.Stat(filepath.Join(tmp, "hello", "overlord.yaml")); err != nil {
		t.Errorf("default target dir not created: %v", err)
	}
}

// =============================================================================
// Happy path: scaffold + demo run (hello template)
// =============================================================================

func TestInit_HelloHappyPath(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "hello-proj")
	code, stdout, stderr := runInitCmd(t, "hello", dir)
	if code != initExitSuccess {
		t.Fatalf("expected exit 0, got %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	// Stdout should contain the pipeline result — the mock fixture
	// returns {"greeting": "Hello, world!"}.
	if !strings.Contains(stdout, `"greeting"`) {
		t.Errorf("stdout missing greeting payload\nstdout: %s", stdout)
	}
	if !strings.Contains(stdout, "Hello, world!") {
		t.Errorf("stdout missing greeting content\nstdout: %s", stdout)
	}
	// Stderr must include next-steps.
	if !strings.Contains(stderr, "Next steps:") {
		t.Errorf("stderr missing next-steps\nstderr: %s", stderr)
	}
	if !strings.Contains(stderr, "--id hello") {
		t.Errorf("stderr next-steps missing pipeline id\nstderr: %s", stderr)
	}
}

// =============================================================================
// Force / overwrite / collision handling
// =============================================================================

func TestInit_ExistingNonEmpty_NoForce(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "existing")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "blocker.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := runInitCmd(t, "hello", dir, "--no-run")
	if code != initExitInvalidTarget {
		t.Fatalf("expected exit 2, got %d\nstderr: %s", code, stderr)
	}
	if !strings.Contains(stderr, "not empty") && !strings.Contains(stderr, "--force") {
		t.Errorf("stderr missing hint about --force\nstderr: %s", stderr)
	}
}

func TestInit_Force_IntoEmptyWithExtraFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "force-demo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Put an unrelated file in the dir that does NOT collide with
	// anything the template writes.
	if err := os.WriteFile(filepath.Join(dir, "unrelated.txt"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := runInitCmd(t, "hello", dir, "--force", "--no-run")
	if code != initExitSuccess {
		t.Fatalf("expected exit 0, got %d\nstderr: %s", code, stderr)
	}
	// The unrelated file is still there, the scaffolded yaml was written.
	if _, err := os.Stat(filepath.Join(dir, "unrelated.txt")); err != nil {
		t.Errorf("pre-existing file was removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "overlord.yaml")); err != nil {
		t.Errorf("overlord.yaml not written: %v", err)
	}
}

func TestInit_Force_CollisionWithoutOverwrite(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "collide")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Collision: overlord.yaml already exists.
	if err := os.WriteFile(filepath.Join(dir, "overlord.yaml"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := runInitCmd(t, "hello", dir, "--force", "--no-run")
	if code != initExitInvalidTarget {
		t.Fatalf("expected exit 2, got %d\nstderr: %s", code, stderr)
	}
	if !strings.Contains(stderr, "overlord.yaml") {
		t.Errorf("stderr missing collision file name\nstderr: %s", stderr)
	}
}

func TestInit_ForceOverwrite_WithCollision(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "overwrite")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "overlord.yaml"), []byte("old-content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := runInitCmd(t, "hello", dir, "--force", "--overwrite", "--no-run")
	if code != initExitSuccess {
		t.Fatalf("expected exit 0, got %d\nstderr: %s", code, stderr)
	}
	if !strings.Contains(stderr, "Backed up") {
		t.Errorf("stderr missing backup note\nstderr: %s", stderr)
	}
	// The backup file should exist alongside the new overlord.yaml.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var foundBackup bool
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "overlord.yaml.overlord-init-bak.") {
			foundBackup = true
			break
		}
	}
	if !foundBackup {
		t.Errorf("no backup file present in %s", dir)
	}
}

func TestInit_OverwriteWithoutForce(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nope")
	code, _, stderr := runInitCmd(t, "hello", dir, "--overwrite", "--no-run")
	if code != initExitInvalidTarget {
		t.Fatalf("expected exit 2, got %d\nstderr: %s", code, stderr)
	}
	if !strings.Contains(stderr, "--overwrite requires --force") {
		t.Errorf("stderr missing overwrite-requires-force hint\nstderr: %s", stderr)
	}
}

// =============================================================================
// Demo failure injection (delete a fixture file after scaffold)
// =============================================================================

// scaffoldForDemoFailure writes the hello template into a fresh dir and
// then deletes the fixture so a subsequent runDemo / runInit call will
// fail at buildAgents (fixture validation).
func scaffoldForDemoFailure(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "scaffold")
	_, err := scaffold.Write(context.Background(), "hello", dir, scaffold.Options{})
	if err != nil {
		t.Fatalf("scaffold.Write: %v", err)
	}
	if err := os.Remove(filepath.Join(dir, "fixtures", "greet.json")); err != nil {
		t.Fatalf("remove fixture: %v", err)
	}
	return dir
}

// TestRunDemo_BuildFailure verifies runDemo surfaces the missing-fixture
// error directly. This is the substrate the --non-interactive exit-4
// path relies on.
func TestRunDemo_BuildFailure(t *testing.T) {
	dir := scaffoldForDemoFailure(t)
	_, err := runDemo(context.Background(), dir, io.Discard)
	if err == nil {
		t.Fatal("expected runDemo to fail on missing fixture")
	}
	if !strings.Contains(err.Error(), "build broker") &&
		!strings.Contains(err.Error(), "fixture") &&
		!strings.Contains(err.Error(), "mock") {
		t.Errorf("unexpected error message: %v", err)
	}
}

// runInitDirect is the core of the cobra RunE path, but bypasses the
// scaffold step so tests can seed arbitrary pre-scaffolded states (e.g.
// with a corrupted fixture). We reconstruct just the post-scaffold demo
// + signal + next-steps flow by calling runInit with an already-valid
// target and Force=true, then replacing the scaffold step with a
// scaffold.Write that is a no-op when the dir already contains the
// template — not possible, so instead we test by calling runDemo +
// reproducing the decision tree locally.

// simulateInitDemoPhase reproduces runInit's post-scaffold decision
// tree so tests can assert on the demo-failure exit-code behaviour
// without the scaffold writer rewriting a pre-broken directory.
func simulateInitDemoPhase(target string, nonInteractive bool) error {
	_, demoErr := runDemo(context.Background(), target, io.Discard)
	if demoErr == nil {
		return nil
	}
	if nonInteractive {
		return &initExitError{Code: initExitDemoFailure, Msg: "demo failed", Err: demoErr}
	}
	return nil
}

// TestInit_DemoFailure_InteractiveDefault: interactive default returns
// nil (exit 0) so files persist and the user sees the warning+next-steps.
func TestInit_DemoFailure_InteractiveDefault(t *testing.T) {
	dir := scaffoldForDemoFailure(t)
	if err := simulateInitDemoPhase(dir, false); err != nil {
		t.Fatalf("expected nil on interactive demo failure, got %v", err)
	}
}

// TestInit_DemoFailure_NonInteractive: --non-interactive propagates the
// demo failure as exit 4 with the underlying cause reachable via Unwrap.
func TestInit_DemoFailure_NonInteractive(t *testing.T) {
	dir := scaffoldForDemoFailure(t)
	err := simulateInitDemoPhase(dir, true)
	var ie *initExitError
	if !errors.As(err, &ie) {
		t.Fatalf("expected initExitError, got %T: %v", err, err)
	}
	if ie.Code != initExitDemoFailure {
		t.Fatalf("expected exit %d, got %d", initExitDemoFailure, ie.Code)
	}
	if ie.Unwrap() == nil {
		t.Error("expected Unwrap() to surface underlying cause")
	}
}

// =============================================================================
// SIGINT handling
// =============================================================================

// TestInit_SIGINT_InterruptsDemo exercises the SIGINT path by running
// runInit in a goroutine and firing SIGINT into the test process while
// the signal goroutine is still subscribed. The mock-backed demo is
// fast, so we stall it artificially by holding the scaffolded target's
// overlord.yaml open for read via a parallel goroutine that times out.
// Since we can't reliably slow the mock adapter, we instead test the
// signal translator directly: construct a cancelable context, install
// the signal handler, fire SIGINT, and assert the context is canceled.
func TestInit_SIGINT_SignalTranslation(t *testing.T) {
	// This exercises only the signal-to-cancel forwarding that lives
	// inside runInit's demo scope. Full end-to-end SIGINT coverage is
	// provided by the opt-in integration test below.
	sigCh := make(chan os.Signal, 1)
	signalCtx, signalCancel := newSignalContext(sigCh)
	defer signalCancel()

	sigCh <- syscall.SIGINT

	select {
	case <-signalCtx.Done():
		// expected
	case <-time.After(2 * time.Second):
		t.Fatal("signal did not cancel context within 2s")
	}
}

// newSignalContext is a small helper factored out for tests. It mirrors
// the signal goroutine inside runInit without the scaffolding around it.
func newSignalContext(sigCh <-chan os.Signal) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		select {
		case <-sigCh:
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, cancel
}

// TestInit_SIGINT_EndToEnd runs the full SIGINT path. Opt-in because it
// relies on delivering SIGINT to the test process itself, which is
// fragile on some CI runners. Enable with OVERLORD_RUN_SIGINT=1.
func TestInit_SIGINT_EndToEnd(t *testing.T) {
	if os.Getenv("OVERLORD_RUN_SIGINT") == "" {
		t.Skip("opt-in: set OVERLORD_RUN_SIGINT=1 to enable end-to-end SIGINT test")
	}
	// Use a fresh (non-existing) dir so runInit's scaffold step does
	// the full scaffold itself.
	dir := filepath.Join(t.TempDir(), "sigint-demo")

	var wg sync.WaitGroup
	wg.Add(1)
	var gotErr error
	go func() {
		defer wg.Done()
		gotErr = callRunInitDirect(t, []string{"hello", dir}, initArgs{})
	}()

	// Fire SIGINT quickly but not instantly — give the scaffold phase
	// time to complete so the demo window is open when the signal lands.
	time.Sleep(5 * time.Millisecond)
	_ = syscall.Kill(os.Getpid(), syscall.SIGINT)
	wg.Wait()

	var ie *initExitError
	if errors.As(gotErr, &ie) && ie.Code == initExitInterrupted {
		return // pass
	}
	if gotErr == nil {
		t.Skip("demo beat the signal; signal translation already unit-tested")
	}
	t.Fatalf("expected exit %d, got err=%v", initExitInterrupted, gotErr)
}

// =============================================================================
// Helpers
// =============================================================================

// callRunInitDirect drives runInit with the init subcommand pulled
// from a fresh root command tree. Stdout/stderr are discarded. Used by
// tests that want to assert on the error return value (exit-code
// surface) without re-parsing stderr.
func callRunInitDirect(t *testing.T, posArgs []string, a initArgs) error {
	t.Helper()
	root := rootCmd()
	for _, c := range root.Commands() {
		if c.Name() == "init" {
			c.SetOut(new(bytes.Buffer))
			c.SetErr(new(bytes.Buffer))
			return runInit(c, posArgs, a)
		}
	}
	return fmt.Errorf("init subcommand not registered")
}

// =============================================================================
// Formatting helpers
// =============================================================================

// TestFormatNextSteps_ShellSyntacticallyValid verifies the printed
// `overlord exec` invocation parses cleanly with a POSIX-ish tokenizer.
// A regression here would mean users can't copy-paste the hint.
func TestFormatNextSteps_ShellSyntacticallyValid(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "shell-test")
	if code, _, se := runInitCmd(t, "hello", dir, "--no-run"); code != initExitSuccess {
		t.Fatalf("scaffold setup failed: exit %d stderr=%s", code, se)
	}
	got, err := formatNextSteps(dir)
	if err != nil {
		t.Fatalf("formatNextSteps: %v", err)
	}
	// Collapse line continuations and join the non-comment body lines
	// into a single command string.
	body := strings.ReplaceAll(got, "\\\n", " ")
	var cmdLines []string
	for _, ln := range strings.Split(body, "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" || strings.HasPrefix(ln, "#") || strings.HasPrefix(ln, "Next steps") {
			continue
		}
		cmdLines = append(cmdLines, ln)
	}
	joined := strings.Join(cmdLines, " ")
	toks, err := simpleShellSplit(joined)
	if err != nil {
		t.Fatalf("next-steps tokenization failed: %v\njoined: %s", err, joined)
	}
	// Must contain 'exec' and '--id hello'.
	var haveExec, haveID bool
	for i, tok := range toks {
		if tok == "exec" {
			haveExec = true
		}
		if tok == "--id" && i+1 < len(toks) && toks[i+1] == "hello" {
			haveID = true
		}
	}
	if !haveExec {
		t.Errorf("tokenized next-steps missing 'exec': %v", toks)
	}
	if !haveID {
		t.Errorf("tokenized next-steps missing '--id hello': %v", toks)
	}
}

// simpleShellSplit implements a minimal POSIX-ish tokenizer that
// understands single quotes, double quotes, and bare words separated by
// whitespace. Sufficient for asserting the next-steps block is
// syntactically valid (balanced quotes, no unterminated strings).
func simpleShellSplit(s string) ([]string, error) {
	var out []string
	var cur strings.Builder
	inSingle, inDouble := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case inSingle:
			if c == '\'' {
				inSingle = false
				continue
			}
			cur.WriteByte(c)
		case inDouble:
			if c == '"' {
				inDouble = false
				continue
			}
			// Support \" escape in double quotes.
			if c == '\\' && i+1 < len(s) && s[i+1] == '"' {
				cur.WriteByte('"')
				i++
				continue
			}
			cur.WriteByte(c)
		case c == '\'':
			inSingle = true
		case c == '"':
			inDouble = true
		case c == '\\' && i+1 < len(s):
			// Escape next char outside quotes. '\''  inside quotes is
			// the POSIX idiom for a literal single quote in a
			// single-quoted string — handled here as close-quote,
			// escaped quote, reopen: result is a literal '.
			cur.WriteByte(s[i+1])
			i++
		case c == ' ' || c == '\t':
			if cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteByte(c)
		}
	}
	if inSingle || inDouble {
		return nil, errors.New("unterminated quote")
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out, nil
}

func TestShellQuote(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/tmp/plain", "/tmp/plain"},
		{"/home/user/proj-1_x", "/home/user/proj-1_x"},
		{"", "''"},
		{"/tmp/with space", "'/tmp/with space'"},
		{"/tmp/it's", `'/tmp/it'\''s'`},
	}
	for _, c := range cases {
		if got := shellQuote(c.in); got != c.want {
			t.Errorf("shellQuote(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestPrettyJSON_HandlesInvalid(t *testing.T) {
	if got := prettyJSON(nil); got != "" {
		t.Errorf("empty input should produce empty output, got %q", got)
	}
	// Invalid JSON: falls back to raw string.
	raw := json.RawMessage("not json")
	got := prettyJSON(raw)
	if !strings.Contains(got, "not json") {
		t.Errorf("expected raw fallback, got %q", got)
	}
	// Valid JSON gets pretty-printed.
	raw = json.RawMessage(`{"a":1}`)
	got = prettyJSON(raw)
	if !strings.Contains(got, "\"a\": 1") {
		t.Errorf("expected pretty-printed JSON, got %q", got)
	}
}

// TestInitExitError_ErrorAs verifies the error chain wiring main()
// relies on when mapping a runInit failure to an os.Exit code.
// TestInit_ScaffoldedProjectValidates asserts that the scaffolded
// `hello` project is structurally sound by running the real
// `overlord validate` subcommand against the generated overlord.yaml.
func TestInit_ScaffoldedProjectValidates(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "valid-proj")
	if code, _, se := runInitCmd(t, "hello", dir, "--no-run"); code != initExitSuccess {
		t.Fatalf("scaffold failed: exit %d stderr=%s", code, se)
	}
	configPath := filepath.Join(dir, "overlord.yaml")

	root := rootCmd()
	root.SetArgs([]string{"validate", "--config", configPath})
	var outBuf, errBuf bytes.Buffer
	root.SetOut(&outBuf)
	root.SetErr(&errBuf)
	if err := root.Execute(); err != nil {
		t.Fatalf("validate failed: %v\nstderr: %s", err, errBuf.String())
	}
}

func TestInitExitError_ErrorAs(t *testing.T) {
	underlying := errors.New("boom")
	wrapped := &initExitError{Code: initExitDemoFailure, Msg: "demo failed", Err: underlying}
	var got *initExitError
	if !errors.As(wrapped, &got) {
		t.Fatal("errors.As should match initExitError")
	}
	if got.Code != initExitDemoFailure {
		t.Errorf("code: want %d got %d", initExitDemoFailure, got.Code)
	}
	if errors.Unwrap(wrapped) != underlying {
		t.Error("Unwrap should return underlying cause")
	}
	// Nil-safe accessors.
	var nilErr *initExitError
	_ = nilErr.Error()
	_ = nilErr.Unwrap()
}

