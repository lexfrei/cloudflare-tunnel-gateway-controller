# Proxy Architecture

This document describes the internal architecture of the L7 proxy data plane.

## Overview

The proxy sits between cloudflared tunnel transport and backend Kubernetes services. It implements full Gateway API HTTPRoute routing locally, removing the limitations of Cloudflare's tunnel ingress API.

```mermaid
flowchart LR
    subgraph Proxy Binary
        CFD[cloudflared<br/>supervisor]
        OP[OriginProxy]
        HANDLER[Handler]
        ROUTER[Router]
        FILTER[Filters]
        BACKEND[Backend Pool]
    end

    CFD -->|ProxyHTTP| OP
    OP -->|ServeHTTP| HANDLER
    HANDLER --> ROUTER
    ROUTER -->|match| FILTER
    FILTER -->|proxy| BACKEND
```

## Packages

```text
internal/
├── proxy/
│   ├── config.go      # ProxyConfig, RouteRule, RouteMatch types
│   ├── matcher.go     # Path/header/query/method matchers
│   ├── router.go      # Routing table with atomic config swap
│   ├── filter.go      # Request/response filters (headers, redirect, rewrite, mirror)
│   ├── handler.go     # http.Handler: match → filter → proxy → response filter
│   ├── api.go         # Config API (PUT/GET /config, /healthz, /readyz)
│   ├── converter.go   # Gateway API HTTPRoute → proxy config conversion
│   └── pusher.go      # HTTP client for pushing config to proxy replicas
│
├── tunnel/
│   ├── origin.go      # GatewayOriginProxy (connection.OriginProxy)
│   ├── bootstrap.go   # Tunnel startup, token parsing, supervisor config
│   └── retry.go       # StartTunnelWithRetry: bootstrap-dial backoff loop
│
└── cmd/proxy/
    └── main.go        # Binary entry point (tunnel mode / standalone mode)
```

## Request Flow

```mermaid
sequenceDiagram
    participant Edge as Cloudflare Edge
    participant CFD as cloudflared
    participant OP as OriginProxy
    participant H as Handler
    participant R as Router
    participant F as Filters
    participant B as Backend

    Edge->>CFD: QUIC/HTTP2 tunnel
    CFD->>OP: ProxyHTTP(writer, request)
    OP->>H: ServeHTTP(w, r)
    H->>R: Route(request)
    R-->>H: matched rule + backend index
    H->>F: Apply request filters
    alt Redirect filter
        F-->>H: redirect response
        H-->>OP: write redirect
    else Normal flow
        F-->>H: modified request
        H->>B: Select weighted backend
        B-->>H: reverse proxy response
        H->>F: Apply response filters
        F-->>H: modified response
        H-->>OP: write response
    end
    OP-->>CFD: done
    CFD-->>Edge: response
```

## Routing Table

The router uses `atomic.Pointer[routingTable]` for lock-free reads during config updates:

- **Exact hosts**: `map[string][]*compiledRule` for O(1) hostname lookup
- **Wildcard hosts**: `[]wildcardEntry` for `*.example.com` patterns
- **Default rules**: Fallback rules without hostname

### Precedence (Gateway API spec)

1. Longest hostname (exact before wildcard)
2. Path type: Exact > Regex > Prefix
3. Longest path value
4. Method present
5. Most header matches
6. Most query parameter matches

## Config Push

The controller pushes routing config via HTTP:

```text
Controller  ──PUT /config──▶  Proxy Config API
                                    │
                              compile routing table
                                    │
                              atomic.Pointer.Store()
                                    │
                              lock-free reads ◀── request goroutines
```

Config versioning prevents stale updates. Each push includes a monotonically increasing version number; the proxy rejects versions older than current.

## Filters

| Filter | Phase | Behavior |
| --- | --- | --- |
| RequestHeaderModifier | Request | Add/set/remove request headers |
| ResponseHeaderModifier | Response | Add/set/remove response headers |
| RequestRedirect | Request | Return redirect response (short-circuit) |
| URLRewrite | Request | Modify URL path and/or host |
| RequestMirror | Request | Clone request to mirror backend (async) |
| CORS | Response | Short-circuit preflight (204 + CORS headers) and attach CORS headers to simple cross-origin responses |

## Backend Selection

Weighted random selection using cumulative weight sums:

1. Precompute cumulative weights: `[30, 30+70] = [30, 100]`
2. Generate random number in `[0, totalWeight)`
3. Linear scan in cumulative weight array
4. Each backend has its own `*http.Transport` with connection pooling

## Tunnel Integration

`GatewayOriginProxy` implements `connection.OriginProxy`:

- `ProxyHTTP`: Delegates to `proxy.Handler.ServeHTTP`
- `ProxyTCP`: Returns error (TCPRoute is future work)

`StartTunnel` (`internal/tunnel/bootstrap.go`) builds the full cloudflared supervisor config:

- Parse tunnel token (base64 JSON)
- Build edge TLS configs (Cloudflare root CAs + system pool)
- Create protocol selector (auto: QUIC preferred)
- **In-process mode** (default): Set `OverrideProxy` on supervisor config to route all requests directly to `proxy.Handler`, bypassing ingress rules entirely
- Start `supervisor.StartTunnelDaemon`

`cmd/proxy/main.go`'s `runTunnelMode` calls `StartTunnelWithRetry` (`internal/tunnel/retry.go`), not `StartTunnel` directly. It wraps `StartTunnel` in a full-jitter exponential-backoff loop scoped to the bootstrap window — before the tunnel has ever registered a connection with the edge — so a transient dial failure (cluster DNS not yet reachable, the edge briefly unreachable) retries instead of exiting the process. A deterministic, config-derived failure (a token that fails to parse, an unsupported protocol setting) is marked non-retryable and still returns immediately. Because the retry loop can call `StartTunnel` more than once per process, everything an attempt spawns is scoped to a per-attempt child context (`attemptCtx`), so a repeated call cannot leak background goroutines. The cloudflared `connection.Observer` is built per attempt too (`newAttemptObserver`) and tied to that same context: when the attempt ends, `Observer.Close` — added by the fork — stops its dispatch goroutine and releases the sinks registered against it, notably the `tunnelstate.ConnTracker` the vendored supervisor registers on every attempt. Reusing one tracker across attempts is not an alternative: the supervisor reads its connection history to choose a fallback protocol, so one attempt's outcome would steer the next one's. Double-registering cloudflared's Prometheus collectors is prevented separately, by the retry-safe registerer in `retry.go`. See the doc comments on `attemptCtx`, `startWaitConnected`, and `newAttemptObserver` in `bootstrap.go` for the full reasoning.

When `TUNNEL_TOKEN` is unset the binary runs in **standalone mode** instead: `runStandaloneMode` (`cmd/proxy/main.go`) starts a plain HTTP server on `PROXY_ADDR` (default `:8080`) that serves `proxy.Handler` directly, plus the config API on `PROXY_CONFIG_ADDR`. `StartTunnel` is never called, so no cloudflared tunnel is started — this mode is for local development and testing.

## Tunnel-Mode Response Writer Semantics

When the proxy runs in in-process mode (production default), the `http.ResponseWriter` `proxy.Handler.ServeHTTP` receives is `cloudflared.connection.http2RespWriter`, NOT a stdlib HTTP/1.1 writer. The two have materially different contracts; missing the gap shipped a production-only WebSocket regression that two rounds of pre-merge code review failed to catch.

| Behaviour | `httptest.NewServer` (HTTP/1.1) | `cloudflared.connection.http2RespWriter` |
| --- | --- | --- |
| `Hijack` before `WriteHeader` | Succeeds — returns the raw TCP conn | **Fails** with `status not yet written before attempting to hijack connection` |
| `WriteHeader(101)` on the wire | `HTTP/1.1 101 Switching Protocols` literal | Translated to status 200 (HTTP/2 has no 1xx); the Cloudflare edge unpacks the 200 back to 101 for HTTP/1.1 clients on the wire (verified empirically by the WebSocket round-trip — the edge translation itself lives in closed-source Cloudflare code) |
| Headers wire format | RFC 7230 ASCII | Serialised into a single `cf-cloudflared-response-headers` blob the edge unpacks (`vendor/github.com/cloudflare/cloudflared/connection/header.go` `ResponseUserHeaders`) |

Practical consequences:

- `httputil.ReverseProxy.handleUpgradeResponse` calls `Hijack` BEFORE `WriteHeader`. Over HTTP/2 that fails; `ReverseProxy`'s default error handler then writes 502 and the client sees a 502 (or a Cloudflare edge-rewritten 403). This is the bug that motivated the custom `proxyWebSocketUpgrade` path in `handler_websocket.go`.
- Writing a status, hijacking, and bidirectionally piping bytes is the correct shape for WebSocket and any future upgrade flow over the tunnel; do NOT route them through `httputil.ReverseProxy`.

### Panic containment

`GatewayOriginProxy.ProxyHTTP` recovers handler panics on both branches, because nothing else does: cloudflared dispatches each QUIC stream on a bare goroutine, and `protocol: auto` dials QUIC first, so an escaping panic took the whole data plane down rather than one request. `x/net/http2` recovers only the handler goroutine, never the ones it spawns, which is why the two WebSocket copy goroutines carry their own guard.

What the recover does next depends on what already reached the client, and the answer differs from a stdlib server's:

- Nothing written yet: the staged headers are cleared and a 500 goes out. Clearing matters because a `Content-Length` staged before the panic would promise a body that never arrives.
- Status already written, or the handler took the connection: nothing can be answered, so the error returns and cloudflared resets the stream. Writing a second status here is worse than useless — the HTTP/2 writer serialises its header map once, so the client keeps the first.

The upgrade branch is the exception and decides nothing. It hands cloudflared the raw writer, so it cannot tell what was sent without wrapping it, and it always returns the error instead. Cloudflared then applies the same rule from its own side: 502 while the status is unsent, reset once it is out.

`http.ErrAbortHandler` is exempt from logging. `httputil.ReverseProxy` raises it on any failed response copy, which a client closing mid-download does routinely, and the standard library suppresses its stack for that reason.

### Required test fixture for tunnel-mode paths

Any proxy code that reads, writes, or hijacks the response MUST be covered by a test that runs through `fakeCloudflaredRespWriter` (`internal/proxy/handler_tunnelfake_test.go`), in addition to any existing `httptest.NewServer`-based coverage. The fake enforces the HTTP/2 contract above and reproduces production failures deterministically:

- `Hijack` rejects unless `statusWritten == true`.
- `WriteHeader(101)` is recorded as 200.
- `WriteHeader` after `Hijack` is a silent no-op (mirroring cloudflared's warn-and-return).
- Second `Hijack` returns `http.ErrHijacked`.

Use the fake from the start of design — not as a last-mile add-on during local CI gates. If a test passes against `httptest.NewServer` and you have no fake-fixture coverage of the same code path, treat the green test as inconclusive for production behaviour.

### Re-vendoring discipline

When bumping the `lexfrei/cloudflared` fork (see CLAUDE.md `Cloudflared Fork`), re-verify each contract row in the table above against the new vendored sources. The fake's behaviour is pinned to the snapshot of cloudflared at fake-authoring time; an upstream rename or semantic change is silent until detected by hand. Tests that string-match the cloudflared error message ALSO won't catch drift because they assert against the fake's own copy of the string.

Fix-up points to re-verify on every cloudflared rebase — at least one per contract row in the table above, in the same order:

- **Hijack precondition** — `fakeCloudflaredRespWriter.Hijack` and the `errFakeStatusNotWritten` constant in `internal/proxy/handler_tunnelfake_test.go`. The fake's error message is pinned to the snapshot of cloudflared at fake-authoring time; if upstream renames the message, update both the constant and any assertion that string-matches against it.
- **101 → 200 translation** — `fakeCloudflaredRespWriter.WriteHeader` mirrors `cloudflared.connection.http2RespWriter.WriteRespHeaders`. Both must keep collapsing `http.StatusSwitchingProtocols` to `http.StatusOK`; if cloudflared changes the translation rule (e.g. adds a different sentinel for Extended CONNECT WebSocket), update the fake to match.
- **WriteHeader after Hijack** — the silent-no-op branch in the fake's `WriteHeader` (cloudflared logs a warning and returns; the fake drops the warning). Re-verify the upstream still no-ops; if it starts panicking or writing a second status, mirror the new behaviour.
- **Second `Hijack` returns `ErrHijacked`** — the fake's `hijacked` flag short-circuits with the stdlib `http.ErrHijacked` sentinel. Re-verify cloudflared still returns the same sentinel (and not a custom error) for the second-call case.

Treat the table as the authoritative contract and the list above as a mechanical checklist; if upstream adds a new contract row, the table, the fake, and this checklist all need a matching update.
