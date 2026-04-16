// Package scaffold embeds first-party project templates and exposes a
// default-model constant consumed by `overlord init` when rendering the
// commented real-provider block in each template's overlord.yaml.
package scaffold

// DefaultAnthropicModel is the model ID substituted into template
// `{{ .Model }}` placeholders when scaffolding a project. It is the
// recommended Anthropic model for the real-provider block shipped in the
// commented section of every generated overlord.yaml.
//
// The constant is inherited from existing codebase literals (see
// config/examples/basic.yaml, README.md, CLAUDE.md) which consistently
// reference claude-sonnet-4-20250514 as the default Sonnet build. The
// plan's Unit 6 is responsible for an integration-tagged smoke test that
// asserts this ID resolves to a 2xx against the live Anthropic catalog
// before a release ships; until that runs, the value is not independently
// verified here and MUST be revisited during Unit 6 integration testing.
const DefaultAnthropicModel = "claude-sonnet-4-20250514"
