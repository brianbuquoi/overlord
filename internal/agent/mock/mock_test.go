package mock

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/brianbuquoi/overlord/internal/agent"
	"github.com/brianbuquoi/overlord/internal/broker"
	"github.com/brianbuquoi/overlord/internal/config"
	"github.com/brianbuquoi/overlord/internal/contract"
)

// Compile-time check that Adapter satisfies agent.Agent. If the interface
// shifts, this line fails the build.
var _ agent.Agent = (*Adapter)(nil)

// --- Test helpers ---

// newRegistry writes a schema file and returns a compiled contract.Registry
// whose basePath is the given dir. The schema is trivial: object with
// required field "x" of any type. Fixtures matching that shape validate.
func newRegistry(t *testing.T, dir, schemaName, schemaVersion string) *contract.Registry {
	t.Helper()
	return newRegistryWithSchema(t, dir, schemaName, schemaVersion,
		`{"type":"object","required":["x"]}`)
}

func newRegistryWithSchema(t *testing.T, dir, schemaName, schemaVersion, schema string) *contract.Registry {
	t.Helper()
	path := filepath.Join(dir, "schemas", schemaName+"_"+schemaVersion+".json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(schema), 0o644); err != nil {
		t.Fatal(err)
	}
	reg, err := contract.NewRegistry([]config.SchemaEntry{{
		Name:    schemaName,
		Version: schemaVersion,
		Path:    filepath.Join("schemas", schemaName+"_"+schemaVersion+".json"),
	}}, dir)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	return reg
}

// writeFixture writes a fixture file under dir at rel and returns its
// relative path for inclusion in Config.Fixtures.
func writeFixture(t *testing.T, dir, rel, body string) string {
	t.Helper()
	full := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return rel
}

// stage builds a config.Stage referencing the given output schema.
func stage(id, schemaName, schemaVersion string) config.Stage {
	return config.Stage{
		ID:    id,
		Agent: "mock-agent",
		OutputSchema: config.StageSchemaRef{
			Name:    schemaName,
			Version: schemaVersion,
		},
	}
}

// --- Happy path ---

func TestNew_HappyPath(t *testing.T) {
	dir := t.TempDir()
	reg := newRegistry(t, dir, "out", "v1")
	fixPath := writeFixture(t, dir, "fixtures/greet.json", `{"x":"hi"}`)

	a, err := New(Config{
		ID:       "mock-agent",
		Fixtures: map[string]string{"greet": fixPath},
		BasePath: dir,
	}, slog.Default(), reg, []config.Stage{stage("greet", "out", "v1")})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if a.ID() != "mock-agent" {
		t.Errorf("ID: got %q, want mock-agent", a.ID())
	}
	if a.Provider() != "mock" {
		t.Errorf("Provider: got %q, want mock", a.Provider())
	}

	res, err := a.Execute(context.Background(), &broker.Task{
		ID:      "task-1",
		StageID: "greet",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if string(res.Payload) != `{"x":"hi"}` {
		t.Errorf("Payload: got %q, want %q", string(res.Payload), `{"x":"hi"}`)
	}
	if res.TaskID != "task-1" {
		t.Errorf("TaskID: got %q, want task-1", res.TaskID)
	}

	if err := a.HealthCheck(context.Background()); err != nil {
		t.Errorf("HealthCheck: %v", err)
	}
}

func TestNew_MultiStageFixtures(t *testing.T) {
	dir := t.TempDir()
	reg := newRegistry(t, dir, "out", "v1")
	writeFixture(t, dir, "fixtures/a.json", `{"x":"A"}`)
	writeFixture(t, dir, "fixtures/b.json", `{"x":"B"}`)

	a, err := New(Config{
		ID: "mock-agent",
		Fixtures: map[string]string{
			"stage-a": "fixtures/a.json",
			"stage-b": "fixtures/b.json",
		},
		BasePath: dir,
	}, slog.Default(), reg, []config.Stage{
		stage("stage-a", "out", "v1"),
		stage("stage-b", "out", "v1"),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	for _, tc := range []struct {
		stageID string
		want    string
	}{
		{"stage-a", `{"x":"A"}`},
		{"stage-b", `{"x":"B"}`},
	} {
		res, err := a.Execute(context.Background(), &broker.Task{ID: "t", StageID: tc.stageID})
		if err != nil {
			t.Fatalf("Execute(%s): %v", tc.stageID, err)
		}
		if string(res.Payload) != tc.want {
			t.Errorf("Execute(%s): got %q, want %q", tc.stageID, string(res.Payload), tc.want)
		}
	}
}

func TestExecute_ReturnsCopy(t *testing.T) {
	dir := t.TempDir()
	reg := newRegistry(t, dir, "out", "v1")
	writeFixture(t, dir, "fix.json", `{"x":"hi"}`)

	a, err := New(Config{
		ID:       "mock-agent",
		Fixtures: map[string]string{"s": "fix.json"},
		BasePath: dir,
	}, slog.Default(), reg, []config.Stage{stage("s", "out", "v1")})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	r1, _ := a.Execute(context.Background(), &broker.Task{ID: "1", StageID: "s"})
	r2, _ := a.Execute(context.Background(), &broker.Task{ID: "2", StageID: "s"})

	// Mutate r1 — r2 must still read the clean fixture.
	for i := range r1.Payload {
		r1.Payload[i] = 'Z'
	}
	if string(r2.Payload) != `{"x":"hi"}` {
		t.Errorf("Execute returned a shared buffer; second call saw mutation: %q", string(r2.Payload))
	}
}

// --- Path containment rejections ---

func TestNew_RejectsAbsoluteFixturePath(t *testing.T) {
	dir := t.TempDir()
	reg := newRegistry(t, dir, "out", "v1")
	abs := filepath.Join(dir, "fix.json")
	if err := os.WriteFile(abs, []byte(`{"x":"hi"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := New(Config{
		ID:       "mock-agent",
		Fixtures: map[string]string{"s": abs},
		BasePath: dir,
	}, slog.Default(), reg, []config.Stage{stage("s", "out", "v1")})
	if err == nil {
		t.Fatal("expected error for absolute fixture path")
	}
	if !strings.Contains(err.Error(), "absolute") {
		t.Errorf("error should mention 'absolute': %v", err)
	}
}

func TestNew_RejectsDotDotPath(t *testing.T) {
	dir := t.TempDir()
	reg := newRegistry(t, dir, "out", "v1")

	// Create a file "above" dir. The resolved path would escape base.
	parent := filepath.Dir(dir)
	outside := filepath.Join(parent, "outside.json")
	if err := os.WriteFile(outside, []byte(`{"x":"outside"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(outside)

	for _, rel := range []string{
		"../outside.json",
		"subdir/../../outside.json",
		"..",
	} {
		t.Run(rel, func(t *testing.T) {
			_, err := New(Config{
				ID:       "mock-agent",
				Fixtures: map[string]string{"s": rel},
				BasePath: dir,
			}, slog.Default(), reg, []config.Stage{stage("s", "out", "v1")})
			if err == nil {
				t.Fatalf("expected error for %q", rel)
			}
			if !strings.Contains(err.Error(), "..") && !strings.Contains(err.Error(), "outside") {
				t.Errorf("error should mention '..' or 'outside' containment, got: %v", err)
			}
		})
	}
}

func TestNew_RejectsEmptyFixturePath(t *testing.T) {
	dir := t.TempDir()
	reg := newRegistry(t, dir, "out", "v1")

	_, err := New(Config{
		ID:       "mock-agent",
		Fixtures: map[string]string{"s": ""},
		BasePath: dir,
	}, slog.Default(), reg, []config.Stage{stage("s", "out", "v1")})
	if err == nil || !strings.Contains(err.Error(), "empty") {
		t.Errorf("expected empty-path error, got: %v", err)
	}
}

// --- Size limit ---

func TestNew_RejectsOversizedFixture(t *testing.T) {
	dir := t.TempDir()
	// Schema here just demands any object — we're testing size, not shape.
	reg := newRegistryWithSchema(t, dir, "out", "v1", `{"type":"object"}`)

	// Build a fixture just past the cap. Use a single big string value so
	// json structure stays shallow.
	big := make([]byte, MaxFixtureBytes+1024)
	for i := range big {
		big[i] = 'a'
	}
	body := `{"x":"` + string(big) + `"}`
	writeFixture(t, dir, "huge.json", body)

	_, err := New(Config{
		ID:       "mock-agent",
		Fixtures: map[string]string{"s": "huge.json"},
		BasePath: dir,
	}, slog.Default(), reg, []config.Stage{stage("s", "out", "v1")})
	if err == nil {
		t.Fatal("expected size-limit error")
	}
	if !strings.Contains(err.Error(), "byte cap") && !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("error should mention size/byte cap, got: %v", err)
	}
}

// --- Missing / broken files ---

func TestNew_MissingFixtureFile(t *testing.T) {
	dir := t.TempDir()
	reg := newRegistry(t, dir, "out", "v1")

	_, err := New(Config{
		ID:       "mock-agent",
		Fixtures: map[string]string{"s": "does-not-exist.json"},
		BasePath: dir,
	}, slog.Default(), reg, []config.Stage{stage("s", "out", "v1")})
	if err == nil {
		t.Fatal("expected missing-file error")
	}
	if !strings.Contains(err.Error(), "does-not-exist.json") {
		t.Errorf("error should name the missing path, got: %v", err)
	}
}

func TestNew_FixtureFailsSchemaValidation(t *testing.T) {
	dir := t.TempDir()
	reg := newRegistryWithSchema(t, dir, "out", "v1",
		`{"type":"object","required":["x"],"properties":{"x":{"type":"string"}}}`)

	// Missing required x.
	writeFixture(t, dir, "bad.json", `{"y":42}`)

	_, err := New(Config{
		ID:       "mock-agent",
		Fixtures: map[string]string{"s": "bad.json"},
		BasePath: dir,
	}, slog.Default(), reg, []config.Stage{stage("s", "out", "v1")})
	if err == nil {
		t.Fatal("expected schema-validation error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "bad.json") {
		t.Errorf("error should name the offending fixture path, got: %v", err)
	}
	if !strings.Contains(msg, "out") {
		t.Errorf("error should name the schema (out), got: %v", err)
	}
}

// --- Stage binding / fixture map consistency ---

func TestNew_MissingFixtureForBoundStage(t *testing.T) {
	dir := t.TempDir()
	reg := newRegistry(t, dir, "out", "v1")

	_, err := New(Config{
		ID:       "mock-agent",
		Fixtures: map[string]string{},
		BasePath: dir,
	}, slog.Default(), reg, []config.Stage{stage("greet", "out", "v1")})
	if err == nil {
		t.Fatal("expected error naming undeclared fixture")
	}
	if !strings.Contains(err.Error(), "greet") {
		t.Errorf("error should name the stage, got: %v", err)
	}
}

func TestNew_UnusedFixtureKey(t *testing.T) {
	dir := t.TempDir()
	reg := newRegistry(t, dir, "out", "v1")
	writeFixture(t, dir, "a.json", `{"x":"A"}`)
	writeFixture(t, dir, "b.json", `{"x":"B"}`)

	_, err := New(Config{
		ID: "mock-agent",
		Fixtures: map[string]string{
			"stage-a": "a.json",
			"typo":    "b.json", // not referenced by any stage
		},
		BasePath: dir,
	}, slog.Default(), reg, []config.Stage{stage("stage-a", "out", "v1")})
	if err == nil {
		t.Fatal("expected unused-fixture error")
	}
	if !strings.Contains(err.Error(), "typo") {
		t.Errorf("error should name the unused key, got: %v", err)
	}
}

func TestNew_ZeroStages_OK(t *testing.T) {
	// Agent defined in config with no bindings yet (e.g. user is editing
	// the pipeline). Construction should succeed with an empty fixture
	// cache so the agent is still constructable by the factory.
	dir := t.TempDir()
	a, err := New(Config{
		ID:       "mock-agent",
		Fixtures: nil,
		BasePath: dir,
	}, slog.Default(), nil, nil)
	if err != nil {
		t.Fatalf("New with zero stages: %v", err)
	}
	if len(a.fixtures) != 0 {
		t.Errorf("expected empty fixture map, got %d entries", len(a.fixtures))
	}
}

func TestNew_StagesButNilRegistry(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, dir, "fix.json", `{"x":"hi"}`)

	_, err := New(Config{
		ID:       "mock-agent",
		Fixtures: map[string]string{"s": "fix.json"},
		BasePath: dir,
	}, slog.Default(), nil, []config.Stage{stage("s", "out", "v1")})
	if err == nil {
		t.Fatal("expected error when stages are non-empty but registry is nil")
	}
	if !strings.Contains(err.Error(), "registry is required") {
		t.Errorf("error should explain registry requirement, got: %v", err)
	}
}

func TestNew_EmptyBasePath(t *testing.T) {
	_, err := New(Config{
		ID:       "mock-agent",
		Fixtures: map[string]string{"s": "fix.json"},
		BasePath: "",
	}, slog.Default(), nil, []config.Stage{stage("s", "out", "v1")})
	if err == nil || !strings.Contains(err.Error(), "config base path is empty") {
		t.Errorf("expected empty-base error, got: %v", err)
	}
}

// --- Symlink rejection ---

func TestNew_RejectsSymlinkFixture(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX symlink semantics differ on Windows (covered separately)")
	}
	dir := t.TempDir()
	reg := newRegistry(t, dir, "out", "v1")

	// Write the real file outside dir so the symlink test isn't a dotdot
	// test in disguise. Then symlink from inside dir to it.
	realTarget := filepath.Join(dir, "real.json")
	if err := os.WriteFile(realTarget, []byte(`{"x":"real"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(realTarget, link); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}

	_, err := New(Config{
		ID:       "mock-agent",
		Fixtures: map[string]string{"s": "link.json"},
		BasePath: dir,
	}, slog.Default(), reg, []config.Stage{stage("s", "out", "v1")})
	if err == nil {
		t.Fatal("expected symlink refusal")
	}
	// O_NOFOLLOW yields ELOOP on Linux; we just require that some error
	// came back that references either the link path or mentions symlink.
	msg := err.Error()
	if !strings.Contains(msg, "link.json") && !strings.Contains(strings.ToLower(msg), "symlink") {
		t.Errorf("error should reference the symlinked path or mention symlink, got: %v", err)
	}
}

// --- Execute edge cases ---

func TestExecute_UnknownStageID_NonRetryable(t *testing.T) {
	dir := t.TempDir()
	reg := newRegistry(t, dir, "out", "v1")
	writeFixture(t, dir, "fix.json", `{"x":"hi"}`)

	a, err := New(Config{
		ID:       "mock-agent",
		Fixtures: map[string]string{"known": "fix.json"},
		BasePath: dir,
	}, slog.Default(), reg, []config.Stage{stage("known", "out", "v1")})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = a.Execute(context.Background(), &broker.Task{ID: "t", StageID: "unknown"})
	if err == nil {
		t.Fatal("expected error for unknown stage")
	}
	var ae *agent.AgentError
	if !errors.As(err, &ae) {
		t.Fatalf("expected *agent.AgentError, got %T: %v", err, err)
	}
	if ae.Retryable {
		t.Error("missing-fixture error must be non-retryable")
	}
	if ae.AgentID != "mock-agent" {
		t.Errorf("AgentID: got %q", ae.AgentID)
	}
	if ae.Prov != "mock" {
		t.Errorf("Prov: got %q", ae.Prov)
	}
	if !strings.Contains(ae.Err.Error(), "unknown") {
		t.Errorf("error should mention the missing stage id, got: %v", ae.Err)
	}
}

func TestExecute_NilTask(t *testing.T) {
	dir := t.TempDir()
	a, err := New(Config{
		ID:       "mock-agent",
		Fixtures: nil,
		BasePath: dir,
	}, slog.Default(), nil, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = a.Execute(context.Background(), nil)
	if err == nil {
		t.Fatal("expected error for nil task")
	}
	var ae *agent.AgentError
	if !errors.As(err, &ae) || ae.Retryable {
		t.Errorf("expected non-retryable *agent.AgentError, got %v", err)
	}
}

// --- Round-trip: bytes are forwarded verbatim ---

func TestExecute_BytesVerbatim(t *testing.T) {
	dir := t.TempDir()
	reg := newRegistryWithSchema(t, dir, "out", "v1", `{"type":"object"}`)

	// Exotic (but valid-JSON) body with whitespace and unicode. Schema
	// validation will canonicalise via json.Unmarshal, but the bytes
	// returned by Execute must be the originals.
	body := "{\n  \"x\": \"caf\u00e9\"\n}"
	writeFixture(t, dir, "f.json", body)

	a, err := New(Config{
		ID:       "mock-agent",
		Fixtures: map[string]string{"s": "f.json"},
		BasePath: dir,
	}, slog.Default(), reg, []config.Stage{stage("s", "out", "v1")})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	res, _ := a.Execute(context.Background(), &broker.Task{ID: "t", StageID: "s"})
	if string(res.Payload) != body {
		t.Errorf("payload not verbatim\nwant:\n%s\ngot:\n%s", body, string(res.Payload))
	}

	// And it's still valid JSON that round-trips.
	var v any
	if err := json.Unmarshal(res.Payload, &v); err != nil {
		t.Errorf("payload not valid JSON: %v", err)
	}
}

// --- Resolver unit coverage ---

func TestResolveFixturePath_Containment(t *testing.T) {
	base := t.TempDir()
	cases := []struct {
		name    string
		rel     string
		wantErr bool
	}{
		{"plain", "fix.json", false},
		{"nested", "a/b/fix.json", false},
		{"with-dot", "./fix.json", false},
		{"redundant-slashes", "a//b/fix.json", false},
		{"absolute", "/etc/passwd", true},
		{"dotdot-leading", "../x.json", true},
		{"dotdot-middle", "a/../../x.json", true},
		{"empty", "", true},
		{"only-spaces", "   ", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := resolveFixturePath(base, tc.rel)
			if tc.wantErr && err == nil {
				t.Errorf("expected error for %q", tc.rel)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("unexpected error for %q: %v", tc.rel, err)
			}
		})
	}
}
