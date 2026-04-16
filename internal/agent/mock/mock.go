// Package mock implements a first-class agent.Agent that returns
// pre-recorded fixture payloads keyed by stage ID. It is designed for
// scaffolded-project demo runs and for deterministic local development
// without any LLM credentials.
//
// The constructor does all the heavy lifting:
//
//   - Resolves each fixture path relative to the pipeline config directory.
//   - Rejects absolute paths, paths containing ".." after filepath.Clean,
//     and paths that resolve outside the config base directory.
//   - Opens each file via openFileNoFollow (O_NOFOLLOW on POSIX, pre-open
//     Lstat + post-open Stat compare on Windows) so a symlinked fixture
//     cannot leak arbitrary file contents through the adapter.
//   - Enforces a per-fixture 256 KiB size cap via io.LimitReader so an
//     oversized file cannot be buffered whole before rejection.
//   - Validates each fixture's bytes against the stage's output_schema from
//     the compiled contract.Registry, surfacing the offending path + schema
//     violation at agent-construction time.
//
// Because every code path that constructs an agent goes through
// registry.NewFromConfigWithPlugins, fixture validation happens uniformly
// whether the adapter is built by `overlord run`, `overlord health`, the
// demo runner invoked by `overlord init`, or a SIGHUP hot-reload — no
// separate public validation interface is required.
package mock

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/brianbuquoi/overlord/internal/agent"
	"github.com/brianbuquoi/overlord/internal/broker"
	"github.com/brianbuquoi/overlord/internal/config"
	"github.com/brianbuquoi/overlord/internal/contract"
	"github.com/brianbuquoi/overlord/internal/metrics"
)

// providerName is the YAML `provider:` string that selects this adapter.
const providerName = "mock"

// MaxFixtureBytes caps each fixture file at 256 KiB. The cap protects
// against accidental-check-in of oversized artefacts and gives the adapter
// a bounded construction cost regardless of config shape. Large enough for
// realistic summarize-style demo outputs; the sanitize envelope downstream
// provides an independent guard against runaway values.
const MaxFixtureBytes = 256 * 1024

// Config is the adapter configuration derived from the YAML agent block.
// The adapter does not consume Model / Auth / SystemPrompt / Temperature —
// those fields are declared on config.Agent for all providers but carry no
// meaning here.
type Config struct {
	// ID matches config.Agent.ID and is returned by the Agent.ID method.
	ID string
	// Fixtures maps stage_id → fixture path (relative to the pipeline
	// config directory unless absolute, which is rejected).
	Fixtures map[string]string
	// BasePath is the directory fixture paths resolve against. Typically
	// the directory containing the overlord.yaml file. Must be absolute.
	BasePath string
}

// Adapter is a mock agent.Agent implementation backed by in-memory fixtures.
// Fixtures are loaded and validated at construction time, then returned
// verbatim from Execute.
type Adapter struct {
	cfg      Config
	logger   *slog.Logger
	metrics  *metrics.Metrics
	fixtures map[string]json.RawMessage // stage_id → validated fixture bytes
}

// New constructs a mock adapter, loading and validating every referenced
// fixture inline. Any failure — unknown schema, path outside base, too
// large, schema violation — is returned before the adapter becomes usable.
//
// The stages slice must already be filtered to the subset that reference
// cfg.ID; the registry factory is responsible for that filtering so the
// adapter can treat every entry as relevant.
//
// registry is required when stages is non-empty; callers that construct
// a mock adapter for a pipeline with zero bindings (e.g. a health check
// where no stage points at it) may pass registry=nil and stages=nil.
func New(
	cfg Config,
	logger *slog.Logger,
	registry *contract.Registry,
	stages []config.Stage,
	m ...*metrics.Metrics,
) (*Adapter, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if cfg.Fixtures == nil {
		cfg.Fixtures = map[string]string{}
	}

	absBase, err := ensureAbsoluteBase(cfg.BasePath)
	if err != nil {
		return nil, fmt.Errorf("mock agent %q: %w", cfg.ID, err)
	}
	cfg.BasePath = absBase

	loaded := make(map[string]json.RawMessage, len(stages))
	// Track which stage IDs have been loaded so a second binding with the
	// same output schema doesn't redundantly re-read the same file.
	for _, st := range stages {
		if _, ok := loaded[st.ID]; ok {
			continue
		}
		rel, ok := cfg.Fixtures[st.ID]
		if !ok {
			return nil, fmt.Errorf(
				"mock agent %q: no fixture declared for stage %q (add agents[].fixtures.%s to the config)",
				cfg.ID, st.ID, st.ID,
			)
		}

		full, err := resolveFixturePath(cfg.BasePath, rel)
		if err != nil {
			return nil, fmt.Errorf(
				"mock agent %q: stage %q fixture %q: %w",
				cfg.ID, st.ID, rel, err,
			)
		}

		data, err := readFixture(full)
		if err != nil {
			return nil, fmt.Errorf(
				"mock agent %q: stage %q fixture %s: %w",
				cfg.ID, st.ID, full, err,
			)
		}

		if registry == nil {
			return nil, fmt.Errorf(
				"mock agent %q: contract registry is required to validate fixture for stage %q",
				cfg.ID, st.ID,
			)
		}

		validator := contract.NewValidator(registry)
		schemaVer := contract.SchemaVersion(st.OutputSchema.Version)
		if err := validator.ValidateOutput(
			st.OutputSchema.Name,
			schemaVer,
			schemaVer,
			data,
		); err != nil {
			return nil, fmt.Errorf(
				"mock agent %q: stage %q fixture %s failed schema validation (%s@%s): %w",
				cfg.ID, st.ID, full, st.OutputSchema.Name, st.OutputSchema.Version, err,
			)
		}

		loaded[st.ID] = append(json.RawMessage(nil), data...)
	}

	// Surface unused fixture entries as an error. A typo in a stage ID —
	// fixtures: { greetr: ... } vs stages[].id: greet — would otherwise
	// silently load nothing and surface only as a runtime miss.
	if extra := unusedFixtureKeys(cfg.Fixtures, stages); len(extra) > 0 {
		return nil, fmt.Errorf(
			"mock agent %q: fixtures declared for unknown stage IDs: %s (known stage bindings: %s)",
			cfg.ID, strings.Join(extra, ", "), stageIDs(stages),
		)
	}

	a := &Adapter{
		cfg:      cfg,
		logger:   logger,
		fixtures: loaded,
	}
	if len(m) > 0 && m[0] != nil {
		a.metrics = m[0]
	}
	logger.Debug("mock agent initialized",
		"agent_id", cfg.ID,
		"fixtures_loaded", len(loaded),
	)
	return a, nil
}

// ID returns the configured agent ID.
func (a *Adapter) ID() string { return a.cfg.ID }

// Provider returns the YAML provider string this adapter registers under.
func (a *Adapter) Provider() string { return providerName }

// Execute returns the fixture bytes associated with task.StageID verbatim.
// A missing fixture is a non-retryable error — the constructor validates
// all declared fixtures up front, so a runtime miss indicates that the
// agent was bound to a stage that was not in the stages slice passed to
// New (i.e. a code-path bug in the caller, not a transient failure).
func (a *Adapter) Execute(_ context.Context, task *broker.Task) (*broker.TaskResult, error) {
	if task == nil {
		return nil, &agent.AgentError{
			Err:       fmt.Errorf("mock agent %q: nil task", a.cfg.ID),
			AgentID:   a.cfg.ID,
			Prov:      providerName,
			Retryable: false,
		}
	}
	payload, ok := a.fixtures[task.StageID]
	if !ok {
		return nil, &agent.AgentError{
			Err: fmt.Errorf(
				"mock agent %q has no fixture loaded for stage %q; declared fixtures cover stages: %s",
				a.cfg.ID, task.StageID, a.loadedStageIDs(),
			),
			AgentID:   a.cfg.ID,
			Prov:      providerName,
			Retryable: false,
		}
	}
	// Return a copy so downstream consumers cannot mutate the adapter's
	// cached bytes — the broker treats the payload as an opaque byte slice
	// and has no formal contract against in-place edits.
	out := make(json.RawMessage, len(payload))
	copy(out, payload)
	return &broker.TaskResult{
		TaskID:  task.ID,
		Payload: out,
	}, nil
}

// HealthCheck returns nil. The constructor has already guaranteed that
// every declared fixture exists, fits under the size cap, and validates
// against the stage's output schema — there is nothing further for
// HealthCheck to verify at runtime.
func (a *Adapter) HealthCheck(_ context.Context) error { return nil }

// loadedStageIDs returns a stable-sorted list of stage IDs the adapter
// has fixtures for, used for error messages.
func (a *Adapter) loadedStageIDs() string {
	if len(a.fixtures) == 0 {
		return "<none>"
	}
	ids := make([]string, 0, len(a.fixtures))
	for id := range a.fixtures {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return strings.Join(ids, ", ")
}

// resolveFixturePath validates a fixture path and returns its absolute
// cleaned form rooted at base. Absolute paths, paths whose cleaned form
// escapes base (via ..), and blank paths are all rejected.
func resolveFixturePath(base, rel string) (string, error) {
	if strings.TrimSpace(rel) == "" {
		return "", fmt.Errorf("fixture path is empty")
	}
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("absolute fixture paths are rejected; use a path relative to the config directory")
	}
	cleaned := filepath.Clean(rel)
	// After Clean, ".." components can only appear at the start. If any
	// component is "..", the path escapes base after joining.
	for _, part := range strings.Split(cleaned, string(os.PathSeparator)) {
		if part == ".." {
			return "", fmt.Errorf("fixture path must not contain %q components: %s", "..", rel)
		}
	}
	full := filepath.Clean(filepath.Join(base, cleaned))
	// Belt-and-suspenders containment check: even after Join+Clean, the
	// result must still start with base + separator. Guards against odd
	// corner cases and aligns with R7 of the plan.
	if !strings.HasPrefix(full, base+string(os.PathSeparator)) && full != base {
		return "", fmt.Errorf("fixture path %q resolves outside config directory %q", rel, base)
	}
	return full, nil
}

// readFixture opens path with symlink refusal, enforces the size cap via
// an io.LimitReader (reading one byte past the cap to detect overflow
// without buffering the whole file), and returns the raw bytes.
func readFixture(path string) ([]byte, error) {
	f, err := openFileNoFollow(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	// Belt-and-suspenders: after opening, assert the handle is a regular
	// file. This catches fifos/sockets/directories on platforms where
	// O_NOFOLLOW alone does not short-circuit those.
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("fixture is not a regular file (mode=%s)", fi.Mode())
	}

	limited := io.LimitReader(f, MaxFixtureBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if len(data) > MaxFixtureBytes {
		return nil, fmt.Errorf(
			"fixture exceeds %d byte cap (size>%d)",
			MaxFixtureBytes, MaxFixtureBytes,
		)
	}
	return data, nil
}

// ensureAbsoluteBase requires a non-empty, absolute base path. The mock
// adapter is never usable without one — callers pass the config base from
// configBasePath in cmd/overlord/main.go.
func ensureAbsoluteBase(base string) (string, error) {
	if strings.TrimSpace(base) == "" {
		return "", fmt.Errorf("config base path is empty (internal wiring bug: caller must supply the config directory)")
	}
	if !filepath.IsAbs(base) {
		abs, err := filepath.Abs(base)
		if err != nil {
			return "", fmt.Errorf("resolve config base path: %w", err)
		}
		base = abs
	}
	return filepath.Clean(base), nil
}

// unusedFixtureKeys returns fixture map keys that don't correspond to any
// stage ID in stages, in sorted order.
func unusedFixtureKeys(fixtures map[string]string, stages []config.Stage) []string {
	if len(fixtures) == 0 {
		return nil
	}
	known := make(map[string]struct{}, len(stages))
	for _, s := range stages {
		known[s.ID] = struct{}{}
	}
	var extra []string
	for id := range fixtures {
		if _, ok := known[id]; !ok {
			extra = append(extra, id)
		}
	}
	sort.Strings(extra)
	return extra
}

// stageIDs returns a stable human-readable list of stage IDs for error
// messages.
func stageIDs(stages []config.Stage) string {
	if len(stages) == 0 {
		return "<none>"
	}
	ids := make([]string, 0, len(stages))
	for _, s := range stages {
		ids = append(ids, s.ID)
	}
	sort.Strings(ids)
	return strings.Join(ids, ", ")
}
