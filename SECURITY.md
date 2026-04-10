# Security Policy

## Reporting a Vulnerability

**Do not open a public GitHub issue for security vulnerabilities.**

Use [GitHub's private vulnerability reporting](../../security/advisories/new)
to submit a report. If that is unavailable, email security@orcastrator.dev.

### What to include

- Description of the vulnerability
- Steps to reproduce
- Affected versions
- Potential impact

### Response timeline

| Severity | Acknowledgement | Fix target |
|----------|-----------------|------------|
| Critical | 48 hours        | 7 days     |
| High     | 48 hours        | 30 days    |
| Medium   | 48 hours        | 90 days    |

## Scope

**In scope:** the broker, all LLM provider adapters, the HTTP/WebSocket API,
the CLI (`cmd/orcastrator`), the prompt injection sanitizer, configuration
loading and validation, and the store backends.

**Out of scope:** the LLM providers themselves (Anthropic, OpenAI, Google,
Ollama APIs), issues requiring physical access to the host, and
denial-of-service attacks against third-party services.

## Plugin Trust Model

Orcastrator supports loading Go shared library (.so) plugins as custom
LLM provider adapters. Plugins run in the same process as Orcastrator
with **full process privileges** — they share memory, file descriptors,
environment variables, and network access.

**Important:**

- **Do not load plugins from untrusted sources.** A malicious plugin has
  the same access as the Orcastrator binary itself.
- **Verify plugin checksums before deployment.** Use `sha256sum` or
  equivalent to confirm plugin integrity matches a known-good build.
- **Orcastrator's security guarantees do not extend to plugins.**
  The built-in prompt injection sanitizer, schema validation, and
  authentication middleware protect built-in providers. Plugins bypass
  these guarantees if they handle input/output outside the standard
  pipeline flow.
- Treat the plugins directory with the same file system permissions and
  access controls as the Orcastrator binary.
