# Overlord HTTP API

This document covers HTTP endpoints that are not otherwise documented in the
main README or deployment guide. The WebSocket authentication flow lives
here.

## `POST /v1/ws-token`

Exchanges a valid API key for a short-lived single-use WebSocket session
token. Use this token in the `?token=` query parameter when establishing a
WebSocket connection to `/v1/stream`. The token expires after 30 seconds and
is consumed on first use — subsequent upgrades using the same token are
rejected.

Full API keys MUST NOT be passed in the query string: API keys travelling in
URLs leak into proxy logs, browser history, and TLS debugging tooling. The
ws-token handshake lets browsers open authenticated sockets without
exposing a long-lived credential in the URL.

### Request

```
POST /v1/ws-token
Authorization: Bearer <api-key>
```

Any scope is sufficient (read, write, or admin) — the minted token itself
carries only read scope.

### Response 200

```json
{
  "token": "<32-char hex string>",
  "expires_in": 30
}
```

### Error responses

- `401 Unauthorized` — missing or invalid API key
- `429 Too Many Requests` — brute-force protection or global rate limit
- `503 Service Unavailable` — ws-token subsystem is not configured

### Logging

- Issuance is logged at `INFO` with `request_id` and `expires_in`. The
  token value itself is never logged.
- Successful consumption (on WebSocket upgrade) is logged at `DEBUG`.
- Rejected consumption attempts are logged at `WARN` with a `reason` tag
  such as "expired or already consumed or not found".

### Example

```
$ curl -sS -X POST https://overlord.example.com/v1/ws-token \
      -H "Authorization: Bearer $OVERLORD_API_KEY"
{"token":"a1b2c3…","expires_in":30}

$ wscat "wss://overlord.example.com/v1/stream?token=a1b2c3…"
```
