# Access Logging

The in-process L7 proxy can emit a structured access log for every request it routes. The feature is **opt-in** and **off by default** — enabling it adds one structured log line per request, which on a busy gateway materially raises log volume.

## Why

Without access logging, diagnosing incidents that flow through Cloudflare Tunnel is a black box from inside the cluster: the proxy emits warnings about config churn and connection errors, but there is no per-request record of who saw what status. Cluster log aggregators end up with cloudflared's edge-side logs only, which lack the post-routing context (matched hostname, observed status, response size).

When enabled, the proxy emits one JSON line per request via the same stdout `slog` sink the controller already uses, so existing cluster logging pipelines pick it up without additional wiring.

## Enabling

### Helm

```yaml
proxy:
  accessLog:
    enabled: true
    samplingRate: 1.0
```

`samplingRate` accepts a float in `[0, 1]`:

- `1.0` logs every request (the default once enabled).
- `0.5` logs about 50% of non-5xx requests.
- `0.0` logs only 5xx responses — the always-log-errors carve-out (see below).

Out-of-range values are clamped: a typo like `samplingRate: 50` (intending percent) degrades to "always log" rather than "silently log nothing", so the symptom is fixable from logs.

### Environment variables

If you wire the proxy binary without the Helm chart:

```bash
PROXY_ACCESS_LOG_ENABLED=true \
PROXY_ACCESS_LOG_SAMPLING_RATE=0.1 \
  proxy
```

Truthy forms for `PROXY_ACCESS_LOG_ENABLED`: `1`, `true`, `TRUE` (case-insensitive, leading/trailing whitespace ignored). Any other value disables the feature.

## Log line shape

Emitted as INFO-level JSON via stdlib `slog`:

```json
{
  "time": "2026-05-26T00:00:00Z",
  "level": "INFO",
  "msg": "access",
  "method": "GET",
  "host": "app.example.com",
  "path": "/api/v1/users",
  "query": "filter=active",
  "status": 200,
  "bytes_written": 1234,
  "duration_ms": 42,
  "user_agent": "curl/8.6.0"
}
```

Fields:

| Field | Type | Source |
| --- | --- | --- |
| `method` | string | HTTP request method |
| `host` | string | request `Host` header as seen by the proxy |
| `path` | string | URL path (query string split out separately) |
| `query` | string | URL query string without the leading `?` (`""` if absent) |
| `status` | integer | HTTP status code the proxy sent to the client |
| `bytes_written` | integer | response body bytes sent to the client (does not count headers) |
| `duration_ms` | integer | wall-clock from ServeHTTP entry to the deferred log emission |
| `user_agent` | string | `User-Agent` request header (`""` if absent) |

## Always-log-errors carve-out

Status `>= 500` is logged regardless of `samplingRate`. A 5xx is by definition a server-side failure the operator needs to see, and dropping it to keep sample rate low would hide the most important diagnostic signal. If you set `samplingRate: 0` to keep volume minimal, you still get every 504 / 502 / 503 that the proxy emits.

## Privacy considerations

The `query` field is logged verbatim. URLs in some applications carry tokens (`?token=...`), session IDs (`?sid=...`), signed-URL credentials, or PII in query parameters. Enabling access logging makes these strings searchable in the cluster log aggregator.

Mitigations:

- **Source-level (recommended for token-bearing APIs)**: set `proxy.accessLog.stripQuery: true` (or `PROXY_ACCESS_LOG_STRIP_QUERY=true`). The proxy zeroes the `query` field on every line; `path` stays intact so operators still see which endpoint was hit, just not which parameters carried sensitive values. Defence-in-depth — the field never reaches the log sink in the first place, so no downstream scrubbing rule can be misconfigured into leaking it.
- **Route-level**: design APIs so secrets ride in `Authorization` / `Cookie` headers, not query string. Headers other than `User-Agent` are NOT logged by this feature, so a token in `Authorization: Bearer …` is safe from this sink.
- **Sink-level**: configure the cluster log pipeline (Fluent Bit / Vector / Promtail) to scrub `query` matching known token shapes before forwarding to long-term storage. Use when `stripQuery: true` is too coarse (e.g. you want to keep `?action=delete` triage signal but redact `?token=...`).
- **Sampling**: set `samplingRate` < 1 to reduce the surface area; the always-log-5xx carve-out still surfaces every server-side failure.

## What is NOT logged

- **WebSocket upgrades (`101 Switching Protocols`).** `pipeWebSocket` writes the 101 status BEFORE hijacking the conn, so the wrapper records `status=101`; the access-log emission is then suppressed for that status. Without the skip, every WS upgrade would produce a log line whose `duration_ms` equals the entire WS session lifetime (because the deferred emission runs after the bidirectional copy goroutines exit) and `bytes_written=0` (post-Hijack bytes bypass the wrapper). Both signals would mislead triage. The WS upgrade path emits its own diagnostics.
- **Headers** beyond `User-Agent`. Per-route filters (`HTTPRoute.spec.rules[].filters[].requestHeaderModifier`) are not summarised. If you need that level of detail, add a `responseHeaderModifier` to inject the desired headers into the response, which the access log will then record on the client-visible side.
- **Request bodies.** Streaming and large uploads make body logging both expensive and a privacy hazard.
- **Backend URL** the proxy forwarded to. The router decision context isn't visible to the deferred log emission today; a future enhancement can plumb a `route_id` once the router exposes a stable identifier.

## Cost when disabled

Zero. The handler skips both the response-writer wrapping and the deferred log emission when `accessLog == nil`. `WithAccessLog(nil, _)` is a no-op so the option list stays additive without forcing callers to gate their own construction.

## Sampling design notes

- Sampling is per-request (each request rolls its own random number), not time-window-based.
- The random source is `math/rand/v2.Float64` in production; tests inject deterministic generators to pin sampling behaviour.
- Logs are emitted via a deferred function, so an early-return path (no matching route → 404, redirect filter → 30x) still produces a log line with the right status code recorded.
