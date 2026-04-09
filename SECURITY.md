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
