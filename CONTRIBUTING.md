# Contributing to Orcastrator

## Getting started

```bash
git clone https://github.com/orcastrator/orcastrator.git
cd orcastrator
make test-unit
```

## Running tests

```bash
make test-unit          # fast, no external services
make test-integration   # requires Docker (spins up Redis + Postgres)
make test-all           # both
make check              # tests + go vet + staticcheck
```

## Code style

All code must pass:

- `gofmt` (zero reformatted files)
- `go vet ./...`
- `staticcheck ./...`

## Pull request requirements

- Tests required for all new features and bug fixes
- Update `CLAUDE.md` if the change affects architecture or interfaces
- Add a `CHANGELOG.md` entry under `[Unreleased]`
- PR checklist (filled in automatically via template):
  - [ ] Tests added/updated
  - [ ] `go vet` passes
  - [ ] `CLAUDE.md` updated (if architecture changed)
  - [ ] Changelog entry added

## Adding a new LLM provider adapter

1. Create `internal/agent/<provider>/<provider>.go`
2. Implement the `agent.Agent` interface (see `internal/agent/anthropic/` as a template)
3. Every adapter must implement `HealthCheck`
4. Register the provider in `internal/agent/registry/registry.go`
5. Add unit tests with a mock HTTP server (no real API calls in unit tests)
6. Add TLS enforcement: cloud providers must reject non-HTTPS base URLs
   (localhost is exempt for testing)
7. Update `CLAUDE.md` architecture section and the README provider table

### Adding a fan-out pipeline

Fan-out stages require an `aggregate_schema` that validates the combined
output from all agents. The `output_schema` validates each individual agent's
output, while `aggregate_schema` validates the merged result passed to the
next stage.

## Writing a Plugin (Custom Provider Adapter)

Plugins let you add custom LLM providers without modifying Orcastrator.
Each plugin is a Go shared library (.so) that exports a single symbol.

### 1. Implement `AgentPlugin` and `Agent`

Import the public plugin API:

```go
import "github.com/orcastrator/orcastrator/pkg/plugin"
```

Create a type that implements `plugin.AgentPlugin`:

```go
var Plugin plugin.AgentPlugin = &myPlugin{}

type myPlugin struct{}

func (p *myPlugin) ProviderName() string { return "myprovider" }

func (p *myPlugin) NewAgent(cfg plugin.PluginAgentConfig) (plugin.Agent, error) {
    // Use cfg.Auth, cfg.Model, cfg.Extra, etc. to configure your agent.
    return &myAgent{id: cfg.ID}, nil
}
```

Your agent must implement `plugin.Agent` (ID, Provider, Execute, HealthCheck).

### 2. Build the .so file

```bash
go build -buildmode=plugin -o myprovider.so ./path/to/plugin/
```

### 3. Configure in YAML

```yaml
plugins:
  dir: ./plugins        # scan directory for .so files
  # or list specific files:
  files:
    - ./plugins/myprovider.so

agents:
  - id: my-agent
    provider: myprovider   # matches ProviderName()
    model: my-model
    auth:
      api_key_env: MY_API_KEY
    extra:
      custom_option: value  # passed via PluginAgentConfig.Extra
```

### 4. Platform limitations

- Go's `plugin` package only works on **Linux and macOS** with **CGO enabled**.
- Plugins must be compiled with the **same Go version** and **same module
  dependencies** as the Orcastrator binary.
- Tests that load actual .so files must be tagged `//go:build plugin_test`.

See `examples/plugins/echo/` for a complete example. Build it with
`make build-echo-plugin`.

## Reporting bugs

Use [GitHub Issues](../../issues) with the bug report template.

## Reporting security issues

See [SECURITY.md](SECURITY.md). Do not open public issues for vulnerabilities.

## Branch model

`main` is the stable branch. All work goes through pull requests against `main`.
