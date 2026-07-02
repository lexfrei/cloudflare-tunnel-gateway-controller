# Limitations

This document describes the known limitations of the Cloudflare Tunnel Gateway Controller and provides workarounds where applicable.

## Controller Limitations

| Limitation | Description |
|------------|-------------|
| Full sync | Any change triggers full config sync |
| Backend kinds | `Service` (ClusterIP, NodePort, LoadBalancer, ExternalName, and headless `clusterIP: None` — expanded to one backend per ready endpoint), `ServiceImport` (multicluster.x-k8s.io, resolved via `clusterset.local` DNS), and `ExternalBackend` (`cf.k8s.lex.la`, an out-of-cluster HTTP(S) URL). Any other kind → `ResolvedRefs=False, InvalidKind`. |

## Unsupported config is surfaced on resource status

When the controller will not serve a piece of route config exactly as written, it surfaces the decision on the resource's status conditions — not only in a log line — so `kubectl describe httproute` / `grpcroute` shows the problem and the fix. Several shapes are used, chosen per the Gateway API spec:

- **`Accepted=False` / `UnsupportedValue`** — the whole route cannot be served. This fires when every rule of the route is unservable, e.g. each rule carries an unsupported filter type.
- **`PartiallyInvalid=True`** (alongside `Accepted=True`) — some rules or backends are dropped while the route still serves the rest. The message starts with the spec-mandated `Dropped Rule` prefix and names the affected rule indices. This covers a single unsupported filter among several rules, an unsupported per-backend filter (only that backend's traffic fraction fails closed), and an unparseable rule `timeouts` value (the rule serves without it).
- **`ResolvedRefs=False`** — a backend or object reference cannot be resolved or declares an unsupported app protocol. This covers a missing `Service`, a missing `ServiceImport` (or one that does not export the requested port), a missing `ExternalBackend`, a backend kind outside the supported set (`Reason=InvalidKind`), an unauthorized cross-namespace `backendRef` (`Reason=RefNotPermitted`), plus `appProtocol: https`/`wss` without a `BackendTLSPolicy` (`Reason=UnsupportedProtocol`), an unrecognised `appProtocol`, and a dropped `RequestMirror` backendRef.
- **Dedicated `cf.k8s.lex.la/` condition** (alongside `Accepted=True`) — a problem that leaves the route valid and bound but unserved, shadowed, or un-isolated, where the Gateway API defines no condition. `cf.k8s.lex.la/RouteShadowed=True` marks a route whose `(hostname, match)` pair is exactly claimed by a higher-precedence route; `cf.k8s.lex.la/ProxyConfigPushed=False` marks a SUSTAINED proxy config-push failure (the spec is valid and the tunnel document was written, but the in-cluster proxy never received the config, so requests 502 — it clears on the first successful push); `cf.k8s.lex.la/TunnelShared=True` marks a per-Gateway data plane sharing one Cloudflare Tunnel with another namespace's Gateway (supported, but not isolation). Each mirrors a Warning Event. The same domain-prefixed scheme is used for a listener advisory: `cf.k8s.lex.la/PermissiveHostname=True` flags a Gateway listener OR a ListenerSet entry combining `allowedRoutes.namespaces.from: All` with no hostname pin (the hostname-capture combination); the listener stays `Accepted` and the condition clears once it is pinned or scoped.
- **Kubernetes Event** (`reason=ConfigOverridden`) — config applied successfully but a redundant or conflicting hint was overridden, where no standard condition fits. A Normal event covers a cleartext `appProtocol` (`h2c`, `ws`) superseded by a `BackendTLSPolicy` ("TLS wins"); a Warning event covers a `ResponseHeaderModifier` that strips a WebSocket handshake header (honored as written, but it breaks the upgrade).

Separately, Gateway and ListenerSet listeners whose protocol this controller cannot serve (`TCP`, `TLS`, `UDP`) are marked `Accepted=False, Reason=UnsupportedProtocol` on the listener status.

Unsupported filters fail closed: per the Gateway API spec an `ExtensionRef`, `ExternalAuth`, or unknown HTTPRoute filter type — and a GRPCRoute `RequestMirror` or `ExtensionRef` filter — must not be silently skipped. Requests matching the affected rule (or backend) receive HTTP 500 rather than being served without the dropped filter. A rule-level filter takes the whole rule down; a per-backend filter (HTTPRoute or GRPCRoute `backendRef.filters`) fails only that backend's traffic fraction while the rule keeps serving its other backends. The GRPCRoute core `RequestHeaderModifier` and the extended `ResponseHeaderModifier` filters are served — gRPC metadata is carried as HTTP/2 headers, so they route through the same header-modifier pipeline as HTTPRoute.

A TLS `appProtocol` (`https`, `kubernetes.io/wss`) without a `BackendTLSPolicy` fails the backend closed (HTTP 502) rather than dialing plaintext to a TLS backend. An unrecognised `appProtocol` is report-only: the proxy keeps serving over HTTP/1.1 and records the diagnostic so the ignored hint is visible.

## Non-Service backend kinds

Because the v3 data plane is a generic in-process L7 proxy that ultimately dials a URL, a `backendRef` may target more than a core `Service`:

- **`ServiceImport` (`multicluster.x-k8s.io`)** — a multicluster Service imported by the Multi-Cluster Services (MCS) API. The controller reads `spec.ports` to validate the requested port and resolves the backend to `<name>.<namespace>.svc.clusterset.local:<port>`, letting the cluster's MCS DNS plane route to the imported endpoints. An absent `ServiceImport`, or one that does not export the requested port, is surfaced as `ResolvedRefs=False, BackendNotFound`. The cluster must run an MCS implementation that serves `clusterset.local`; the zero-ready-endpoint 503 probe (applied to core Services) is **not** applied to `ServiceImport`, since MCS `EndpointSlice` labelling is implementation-specific. Unlike `ExternalBackend`, a `ServiceImport` is **not** watched (its CRD is not guaranteed installed, and a hard watch would crash the controller on clusters without MCS): editing a `ServiceImport` — e.g. adding the port a route requested — does not auto-retrigger a reconcile, so the referencing route clears `BackendNotFound` on its next reconcile (a route or `Service` change, or a controller restart) rather than immediately.
- **`ExternalBackend` (`cf.k8s.lex.la`)** — a namespaced CRD declaring an out-of-cluster HTTP(S) origin (`spec.scheme` / `spec.host` / `spec.port` / optional `spec.path`). The proxy dials that URL directly from the pod. The `backendRef` port is ignored in favour of `spec.port`. A missing `ExternalBackend` is surfaced as `ResolvedRefs=False, BackendNotFound` and returns HTTP 500 for its traffic fraction. There is no SSRF allowlist: an `ExternalBackend` is operator-authored and the trust boundary is namespace write-access plus `ReferenceGrant`, identical to a `Service` of type `ExternalName` — supporting which is itself a deliberate deviation from the Gateway API recommendation that implementations SHOULD NOT support `ExternalName` Services (CVE-2021-25740), accepted on that same trust-boundary basis. gRPC over an `ExternalBackend` uses h2c when `scheme: http` and HTTP/2 over TLS (ALPN) when `scheme: https`.

A cross-namespace `ServiceImport` or `ExternalBackend` `backendRef` requires a `ReferenceGrant` whose `to` entry names the matching `group`/`kind` (`multicluster.x-k8s.io`/`ServiceImport` or `cf.k8s.lex.la`/`ExternalBackend`) — a Service-only grant does not authorize them.

## Traffic Splitting and Load Balancing

The in-process L7 proxy performs weighted traffic splitting across the `backendRefs` of a rule, for both HTTPRoute and GRPCRoute. Each backend's `weight` is honoured by a weighted-random selection at request time: traffic is distributed in proportion to the weights, and a backend with `weight: 0` receives no traffic (a rule whose backends all have weight 0 serves nothing). Weighted splitting works directly in the proxy and does not require an external load balancer.

A headless `Service` (`spec.clusterIP: None`) has no VIP for kube-proxy to balance, so the controller resolves its `EndpointSlices` and expands the `backendRef` into one backend per ready endpoint, dialing each pod at its `targetPort`. The endpoints share the `backendRef`'s weight equally, so a headless Service is load-balanced per-endpoint by the proxy's own weighted-random selection while preserving the `backendRef`'s overall traffic proportion against any siblings. A normal Service with a ClusterIP is left to route through its VIP and kube-proxy.

For plain round-robin between pods of one Deployment, a standard Kubernetes `Service` is still the simplest option — point the route at the Service and let kube-proxy balance:

```yaml
apiVersion: v1
kind: Service
metadata:
  name: my-app
spec:
  selector:
    app: my-app
  ports:
    - port: 80
      targetPort: 8080
```

This keeps the controller simple and predictable, and gives you full control over load balancing behavior.

### Unavailable backends return a status, not a dial error

When a rule's `backendRefs` include a backend that cannot serve traffic, the proxy returns an HTTP status for _that backend's traffic fraction_ rather than dialing a dead address (which would surface as a generic 502). The backend stays in the weighted pool, so the fraction is preserved and the other backends keep serving their share. This matches the Gateway API spec, which applies the per-backend status to the proportion of requests that would otherwise have been routed to the failing backend.

Two cases are handled:

- **Invalid `backendRef` → 500.** The `backendRef` is invalid for any reason the spec recognises: it names a Service that does not exist, a cross-namespace Service with no permitting `ReferenceGrant`, a non-`Service` kind, or an out-of-range port. As long as the ref carries traffic (`weight` greater than 0) it stays in the weighted pool and requests routed to it receive `500` for its fraction. A `weight: 0` invalid ref carries no traffic, so it is dropped rather than marked (no fraction is lost).
- **Service with no ready endpoints → 503.** The Service exists (and is authorized) but currently has zero ready endpoints (for example, all pods are `NotReady` during a rollout or a scale-to-zero). Requests routed to this backend receive `503`. An `ExternalName` Service is never treated this way — it has no `EndpointSlices` yet is legitimately reachable. As pods become Ready/NotReady the controller re-evaluates the marking (it watches `EndpointSlices`), so the `503` clears automatically once an endpoint is ready.

If a backend is both nonexistent and endpoint-less the `500` (invalid-ref) status wins. A single-backend rule whose only backend is unavailable returns the corresponding status for all of its traffic.

## Multi-Tenant Isolation Boundary

By default all Gateways of the class share one proxy process and one Cloudflare Tunnel; route-level scoping (`allowedRoutes`, hostname intersection, the optional hostname-ownership policy) is admission/control-plane isolation only. The Gateway API defines no route-to-route hostname ownership — same-hostname routes merge by precedence, and a shadowed route is reported via the `cf.k8s.lex.la/RouteShadowed` condition rather than rejected. That condition is exact-match only: it does not flag overlapping-but-non-identical wildcards (`*.example.com` versus `*.app.example.com`), so cross-tenant wildcard overlap stays invisible to it — use the hostname-ownership policy to bound a tenant's wildcards rather than relying on collision detection. Per-ListenerSet TLS certificate refs are validated for status, never served (TLS terminates at the Cloudflare edge). For hard isolation a Gateway can opt into a dedicated proxy + tunnel. See the [Multi-Tenancy](../guides/multi-tenancy.md) and [Per-Gateway Isolation](../guides/per-gateway-isolation.md) guides.

## SSL Certificate Limitations

Cloudflare's free [Universal SSL](https://developers.cloudflare.com/ssl/edge-certificates/universal-ssl/limitations/) certificates only cover root and first-level subdomains:

| Hostname | Covered | Notes |
|----------|---------|-------|
| `example.com` | Yes | Root domain |
| `*.example.com` | Yes | First-level wildcard |
| `app.example.com` | Yes | First-level subdomain |
| `app.dev.example.com` | No | Multi-level subdomain |
| `*.dev.example.com` | No | Multi-level wildcard |

### Workaround

For multi-level subdomains, you need:

- [Advanced Certificate Manager](https://developers.cloudflare.com/ssl/edge-certificates/advanced-certificate-manager/) (paid add-on — see Cloudflare pricing)
- Business or Enterprise plan

## Gateway Listener Configuration

Gateway listeners follow Gateway API specification. Some fields are ignored because Cloudflare Tunnel manages them at the edge:

| Field | Status | Notes |
|-------|--------|-------|
| `port` | Ignored | Cloudflare uses 443/80 |
| `protocol` | Validated | Only `HTTP` and `HTTPS` listeners are served (they carry HTTPRoute / GRPCRoute). A `TCP`, `TLS`, or `UDP` listener has no data plane here and is marked `Accepted=False, Reason=UnsupportedProtocol` (and not Programmed) on its listener status |
| `hostname` | Supported | Routes must have intersecting hostnames |
| `tls` | Ignored | Cloudflare manages TLS |
| `allowedRoutes` | Supported | Namespace (Same/All/Selector) and kind filtering |

This is because Cloudflare Tunnel terminates TLS at Cloudflare's edge, not in the cluster. However, `hostname` and `allowedRoutes` are validated per Gateway API specification. The same `Accepted=False, Reason=UnsupportedProtocol` listener verdict applies to ListenerSet entries.

### `spec.addresses` is not honoured

The controller does not read `spec.addresses` on a Gateway. The only reachable address for a Cloudflare Tunnel is the tunnel CNAME, which the controller assigns automatically and reports in `status.addresses`; a user cannot request a specific address. This is the same constraint as the Gateway API `GatewayStaticAddresses` feature, which this implementation does not claim. A value placed in `spec.addresses` is neither honoured nor flagged as invalid.

## Backend Protocol (`Service.spec.ports[].appProtocol`)

The L7 proxy reads the backend Service port's `appProtocol` to pick the upstream transport. The supported Kubernetes-defined values:

| `appProtocol` | Supported | Notes |
| --- | --- | --- |
| _(unset)_ | Yes | Default — proxy speaks HTTP/1.1 to the backend |
| `kubernetes.io/h2c` | Yes | Proxy speaks HTTP/2 cleartext (prior knowledge) |
| `kubernetes.io/ws` | Yes | The proxy detects `Connection: Upgrade` + `Upgrade: websocket` headers and routes the request through a dedicated WebSocket upgrade path that dials the backend, forwards the upgrade, writes the 101 to the response writer, and bidirectionally copies bytes after hijack. Plain HTTP requests to the same backend continue to use the default HTTP/1.1 transport |
| `kubernetes.io/wss` | Yes | Requires a `BackendTLSPolicy` targeting the Service (same precondition as `appProtocol: https`); without one the proxy fails the backend closed (HTTP 502) and sets `ResolvedRefs=False, Reason=UnsupportedProtocol` on the route — dialing plaintext to a TLS backend would silently fail |
| any other value | No | Report-only: the proxy serves over HTTP/1.1 (a safe default the backend may speak) and sets `ResolvedRefs=False, Reason=UnsupportedProtocol` on the route so the ignored hint is visible |

The upstream conformance test `HTTPRouteBackendProtocolWebSocket` is not testable through Cloudflare Tunnel: it dials the Gateway address directly via `golang.org/x/net/websocket.Dial`, and our Gateway address is `<tunnel-id>.cfargotunnel.com` whose AAAA records point at Cloudflare's ULA (`fd10::/8`), which is unreachable from any test runner. Unlike the HTTP suite (custom `RoundTripper`) and the gRPC suite (custom gRPC client) — both of which let us redirect the dial to the Cloudflare edge — the WebSocket test exposes no injection point for a custom dialer, so it cannot be redirected and stays skipped. This gap is filed upstream as [kubernetes-sigs/gateway-api#4925](https://github.com/kubernetes-sigs/gateway-api/issues/4925). `test/e2e/e2e_backend_protocol_websocket_test.go` is the substitute proof — it runs an end-to-end WebSocket round trip against the real tunnel hostname.

### gRPC backends and `appProtocol`

The table above describes HTTPRoute backends. A GRPCRoute backend is HTTP/2 by definition, so it is dialed h2c (cleartext HTTP/2) by default and the only `appProtocol` decision that applies is TLS-vs-cleartext. That decision is honoured identically to the HTTP path: a Service port declaring a TLS `appProtocol` (`https`, `HTTPS`, or `kubernetes.io/wss`) with no `BackendTLSPolicy` targeting the Service fails the gRPC backend closed (HTTP 502) and sets `ResolvedRefs=False, Reason=UnsupportedProtocol` on the GRPCRoute — dialing cleartext h2c to a backend that asked for TLS would silently defeat the operator's intent. A `BackendTLSPolicy` puts TLS on the wire (ALPN negotiates HTTP/2); every other `appProtocol` value (unset, `kubernetes.io/h2c`, or an unrecognised string) keeps the h2c default — the correct gRPC transport regardless.

### Interaction with `appProtocol: kubernetes.io/ws`

When a backend Service carries both `appProtocol: kubernetes.io/ws` AND a `BackendTLSPolicy` targeting the same Service, the TLS policy wins: `resolveBackendTLS` rewrites the URL to `https://`, the proxy completes a TLS handshake, and the WebSocket upgrade runs over TLS regardless of the cleartext appProtocol hint. The config is applied successfully, so this is surfaced as a Normal Kubernetes Event (reason `ConfigOverridden`) on the route — not a condition — so operators can either drop the BackendTLSPolicy (if they actually wanted cleartext WebSocket) or flip the hint to `kubernetes.io/wss` (if they wanted the TLS-protected variant all along). The same Normal event fires for `appProtocol: kubernetes.io/h2c` suppressed by a BackendTLSPolicy.

### `ResponseHeaderModifier` MUST preserve WebSocket handshake headers

The L7 proxy applies route-level + per-backend `ResponseHeaderModifier` filters to every backend response, including the 101 Switching Protocols response that carries the WebSocket handshake. Per Gateway API spec the filter pipeline runs unconditionally; the proxy makes no exception for upgrade responses. The operator-facing consequence: a `Remove` list that strips `Sec-WebSocket-Accept`, `Upgrade`, or `Connection` on a route whose backend is WS-marked silently breaks every upgrade on that route. The 101 reaches the client missing a header the RFC 6455 handshake requires, and the client just disconnects.

The converter scans rule-level and per-backend `ResponseHeaderModifier` filters at HTTPRoute apply time. If a `Remove` list on a WS-marked route intersects `{Sec-WebSocket-Accept, Upgrade, Connection}`, the controller emits a Warning Kubernetes Event (reason `ConfigOverridden`) on the route naming the offending header(s) and the filter scope (`rule` or `backend`). The filter still applies as configured — the event is a diagnostic, not a hard rejection, because the misconfiguration is operator-fixable and bypassing the filter would silently violate spec.

Same guidance applies symmetrically to `Set` overriding these headers with a non-handshake-compatible value, though that is rarer and not currently checked.

### Interaction with `spec.rules[].timeouts`

`timeouts.request` and `timeouts.backendRequest` are enforced as **header-only deadlines** on the backend transport (`http.Transport.ResponseHeaderTimeout`), not as full-request context deadlines. The deadline bounds only the wait for backend response headers; once headers arrive, the body streams freely. Both Gateway API knobs collapse onto the same transport-level header timeout because this proxy has no retry logic — a single backend attempt is the whole request. When both knobs are set the stricter (`min(Request, Backend)`) value wins.

This is a deliberate spec interpretation. The spec is underspecified on whether `timeouts.request` should kill an in-flight streaming response (Server-Sent Events, chunked transfer, large file downloads, gRPC server-streaming). A context-based deadline cancels the body read mid-stream and truncates the response at the timeout boundary, which is hostile to any streaming workload. The header-only deadline avoids that while still catching slow-to-respond backends in the dial-and-headers phase — exactly where timeouts are operationally useful.

Backends that take longer than the deadline to send response headers get a 504 Gateway Timeout to the client (the transport's `ResponseHeaderTimeout` error is mapped to 504 in `errorHandler`, parallel to the existing 504 for `context.DeadlineExceeded`).

Symmetric consequence on request uploads: per the stdlib godoc, `ResponseHeaderTimeout` starts measuring only _after_ the request body is fully written. A streaming or very large request upload (chunked PUT, multipart, gRPC client-streaming) is therefore NOT bounded by `timeouts.request` either. Operators who expected `timeouts.request` to act as an upload budget should know that the deadline starts only when the upload completes and the wait for response headers begins. The transport's connect timeout (dial / TLS handshake) still bounds the establishment phase.

The same shift removes the slow-loris-upload protection the old context-based deadline incidentally provided: a malicious client that drip-feeds request body bytes will keep the proxy → backend conn open as long as it sends at least one byte before the underlying TCP read timeout. The Cloudflare edge in front of the tunnel has its own upload deadlines, so the operational risk in production is bounded by edge policy rather than by `timeouts.request`. Per-rule upload-phase deadlines are out of scope for this knob and would need a separate mechanism (e.g. a wrapping `io.Reader` with its own inter-byte deadline) if ever required.

WebSocket routes are naturally exempt: WS upgrades flow through the dedicated `proxyWebSocketUpgrade` path (because cloudflared's HTTP/2 response writer cannot be hijacked the way stdlib `httputil.ReverseProxy` expects), and that path bypasses the cached transport entirely. Once the upgrade completes, two `io.Copy` goroutines pipe bytes bidirectionally between the hijacked client conn and the backend conn; they run until either side closes its conn.

The `BackendProtocol: H2C` path uses `golang.org/x/net/http2.Transport`, which does not expose a `ResponseHeaderTimeout` knob. The proxy synthesises one by wrapping the h2c transport with a `headerTimeoutRoundTripper` that cancels the request context if response headers do not arrive within the per-rule deadline, then releases the cancellation on body Close so streaming bodies (SSE / chunked / gRPC server-streaming) survive past the deadline. The h2c dialer's own connect timeout still catches a fully dead backend.

When a rule's `timeouts` value cannot be parsed (not a valid GEP-2257 duration), the rule is still served without the timeout and the route is marked `PartiallyInvalid=True` with a `Dropped Rule` message naming the rule, so the dropped timeout is visible on `kubectl describe httproute` rather than only in a log line.

## Backend mTLS (BackendTLSPolicy)

The L7 proxy supports Gateway API `BackendTLSPolicy` for proxy → backend TLS, with the following minimum-viable scope:

| Behaviour | Status | Notes |
| --- | --- | --- |
| CA via same-namespace `ConfigMap` ref with `ca.crt` | Yes | Core support level. The CA bundle is parsed as PEM and rejected with `Accepted=False, Reason=NoValidCACertificate` if it contains no `CERTIFICATE` blocks |
| Multiple `CACertificateRefs` per policy | Yes | All bundles are concatenated into the trust pool |
| `Hostname` as TLS SNI + authentication (when no SANs) | Yes | Required field; matches stdlib RFC 6125 hostname verification |
| `SubjectAltNames` of type `Hostname` (OR-matching) | Yes | Cert must match at least one entry. Wildcards in the cert SAN list are honoured via `x509.Certificate.VerifyHostname` |
| `SubjectAltNames` of type `URI` (e.g. SPIFFE) | Yes | Matched by exact string equality against the leaf cert's `URIs` field. OR-matched alongside Hostname SANs in the same policy — either path passing accepts the handshake (matches `BackendTLSPolicySANValidation` conformance) |
| SNI / authentication split when SANs are set | Yes | When ANY `SubjectAltNames` entry is present (Hostname OR URI), `Hostname` is used only for SNI and NOT for authentication, per Gateway API spec. Authentication runs against the SAN list |
| `WellKnownCACertificates: System` | No | Only explicit `CACertificateRefs` are honoured. A policy relying solely on `WellKnownCACertificates` is rejected with `Accepted=False, Reason=Invalid` and a message naming the unsupported value and directing the operator to configure explicit `caCertificateRefs` instead |
| Multiple `targetRefs` per policy | Yes | All targeted Services share the same TLS config |
| `SectionName` per-port targeting | Yes | When a `TargetRef` carries `sectionName`, only the matching named Service port receives TLS; siblings on the same Service stay plaintext |
| Conflict resolution across multiple policies on the same target | Yes | Oldest-creationTimestamp wins, alphabetical name on tie. Losers are stamped `Accepted=False, Reason=Conflicted, Message="conflicts with BackendTLSPolicy <ns/name>"` per GEP-713; the upstream `BackendTLSPolicyConflictResolution` conformance subtest passes. Distinct `SectionName` scopes do not conflict (a policy targeting all Service ports and another scoped to a specific named port are different scopes). When a losing policy also has an invalid CA, `Reason=InvalidCACertificateRef` / `NoValidCACertificate` dominates over `Conflicted` — the actionable CA error surfaces first |
| Cross-namespace CA refs | No | Same-namespace only |
| `GatewayBackendClientCertificate` (mutual TLS) | Yes | The Gateway's `spec.tls.backend.clientCertificateRef` (Standard channel) loads a `kubernetes.io/tls` Secret and the proxy presents the keypair during backend TLS handshakes. Cross-namespace refs require ReferenceGrant. The client cert is attached **only** when the target Service has a `BackendTLSPolicy` — sending a cert over plaintext is meaningless. When an HTTPRoute attaches to multiple Gateways, the first parentRef managed by this controller that has a resolvable client cert wins; foreign-controller parents and parents without a cert are skipped, not blocking. The conformance test does not exercise this multi-parent edge case |
| HTTPS-listener Re-encrypt (frontend TLS termination + backend TLS) | No | Cloudflare terminates TLS at the edge, so frontend `protocol: HTTPS` listeners have no in-cluster TLS-termination data plane and re-encrypt is structurally unsupported (see [Gateway Listener Configuration](#gateway-listener-configuration)). The upstream `BackendTLSPolicy` parent test is skipped for the same reason |

Policy status (`Accepted` / `ResolvedRefs`) is maintained per-Gateway-ancestor. Edits to the CA `ConfigMap` (creation, content patch, or deletion) re-trigger status reconciliation. The `Status.Ancestors` slice is capped at the spec's limit of 16 entries; entries are sorted deterministically by `{namespace, name}` so the truncated set stays stable across reconciles. `LastTransitionTime` is maintained via `meta.SetStatusCondition`, so it only flips when `Status`, `Reason`, or `Message` actually changes.

### Fail-closed enforcement

When a `BackendTLSPolicy` targets a Service but cannot be enforced — the CA `ConfigMap` is missing or unreadable, `ca.crt` is empty, or the bundle is malformed PEM — the proxy receives a poisoned TLS config (empty CA pool) and the next request to that Service fails with HTTP 502. It is NOT silently downgraded to plaintext. The operator's stated intent ("this hop MUST be authenticated TLS") is preserved. Monitor the policy's `Accepted` condition to detect the failure mode (`Reason=NoValidCACertificate`).

### Interaction with `appProtocol: kubernetes.io/h2c`

When a backend Service carries both `appProtocol: kubernetes.io/h2c` AND a `BackendTLSPolicy` targets it, the policy wins: the proxy dials TLS (HTTPS, with HTTP/2 negotiated via ALPN). h2c is by definition cleartext HTTP/2 and cannot coexist with backend TLS; the h2c marker is silently ignored on that hop. If you genuinely want HTTP/2 over TLS, omit the `appProtocol` hint and let ALPN negotiate it during the handshake.

### gRPC needs the `http2` tunnel transport (auto upgrades for you)

cloudflared does not forward HTTP trailers over QUIC (its default transport): the QUIC response adapter's `AddTrailer` is a no-op. gRPC carries the mandatory `grpc-status` in a trailer, so over a QUIC tunnel that trailer is dropped at the edge and every gRPC call fails with `server closed the stream without sending trailers`. This is a cloudflared/Cloudflare limitation, not a controller bug.

With the default `proxy.tunnel.protocol: auto` (or unset) you do not need to do anything for the common case: the proxy waits for the controller's first config push at startup and, when a GRPCRoute is present, dials `http2` instead of letting cloudflared negotiate QUIC. A steady-state deploy that already has GRPCRoutes therefore serves gRPC on the default transport. If a GRPCRoute is added _after_ the proxy has already dialed a non-`http2` transport, a live re-dial is not safe, so the proxy logs a restart-needed error; restart the proxy (it re-dials on `http2`) or pin `proxy.tunnel.protocol: http2`. An explicit `proxy.tunnel.protocol: quic` is never upgraded — it cannot serve gRPC. The controller both logs an error and sets the GRPCRoute's `Accepted` condition to `False` with `Reason=UnsupportedProtocol`, so `kubectl describe grpcroute` shows the incompatibility and the remediation (switch to `http2` or `auto`).

Startup-latency tradeoff of `auto`: because the `auto` choice must learn whether a GRPCRoute is present before dialing, an `auto`/unset proxy waits for the controller's first config push — bounded by a cap (~30s) — before establishing the tunnel. On a cluster that has routes, the first push arrives within seconds, so the delay is negligible. But a route-less cluster (no HTTPRoutes or GRPCRoutes yet) has nothing to push, so every `auto` proxy pod waits the full cap on each start before the tunnel comes up. There is no traffic to serve in that state, so it is harmless in practice — but if you see an `auto` proxy slow to establish its tunnel on a route-less cluster, that wait is the cause; pin `proxy.tunnel.protocol: http2` or `quic` to dial immediately (an explicit transport skips the wait).

### gRPC requires Cloudflare zone gRPC proxying

Separate from the tunnel transport above, the Cloudflare _zone_ must have gRPC proxying enabled (dashboard → Network → gRPC). When it is disabled, the Cloudflare edge returns `403` with `content-type: text/html` zone-wide for any request whose `content-type` is `application/grpc` — the request never reaches the in-cluster proxy.

Because the 403 happens at the edge, upstream of the tunnel, the controller never sees it: the GRPCRoute reports `Accepted=True` / `ResolvedRefs=True` while every gRPC call fails. The client-side symptom is opaque:

```text
rpc error: code = PermissionDenied desc = unexpected HTTP status code received from server: 403 (Forbidden); transport: received unexpected content-type "text/html"
```

The toggle is dashboard-only: there is no zone-settings API for it (`PATCH /settings/grpc` returns `400` / code `1006` "Unrecognized zone setting name" even with a full-scope token) and the Terraform provider has no resource, so the controller cannot read the setting to validate it. As a breadcrumb, the controller emits a Normal Kubernetes Event (`reason: GRPCEdgeProxyingRequired`) on every accepted GRPCRoute naming this prerequisite, so `kubectl describe grpcroute <name>` points at the fix without edge packet inspection.

**Fix:** enable Network → gRPC for the zone in the Cloudflare dashboard.

### Gateway client cert rotation is hot-reloaded

A rotation of the `Secret` referenced by `Gateway.spec.tls.backend.clientCertificateRef` enqueues the affected routes directly — `ConfigMapper.MapSecretToRequests` matches credentials and Gateway-level client-cert Secrets, including cross-namespace refs guarded by a matching `ReferenceGrant` (`from: Gateway`, `to: Secret`). On the resulting reconcile the new keypair is loaded by `loadGatewayClientCertPEM`, the converter stamps it onto every affected `BackendTLSConfig`, and the per-cert transport-pool hash on the proxy evicts the stale transport. The next request to that backend handshakes with the rotated keypair.

Frontend listener `certificateRefs` are not in scope — Cloudflare terminates TLS at the edge, so frontend `protocol: HTTPS` listeners have no in-cluster TLS-termination data plane and are structurally unsupported (see [Gateway Listener Configuration](#gateway-listener-configuration)).

### RequestMirror filter honours BackendTLSPolicy

The Gateway API `RequestMirror` filter sends a fire-and-forget copy of the request to a secondary backend. When a `BackendTLSPolicy` targets the mirror destination Service, the converter stamps the resulting `BackendTLSConfig` on the filter's `MirrorConfig` and the proxy borrows a per-cert `RoundTripper` from the same transport pool the main leg uses. The mirror dial then completes a TLS handshake exactly the way the main leg would, so a TLS-only mirror backend receives the mirrored copy correctly and the operator's TLS expectation is preserved on both legs.

Cross-namespace mirror destinations follow the same `ReferenceGrant` rule as cross-namespace `backendRefs` — without a permitting grant the mirror is dropped entirely. A dropped mirror (unsupported kind, out-of-range port, or unauthorized cross-namespace ref) is surfaced as `ResolvedRefs=False` on the route with the mirror-specific reason (`InvalidKind` / `UnsupportedValue` / `RefNotPermitted`); the main request is unaffected, so the route stays `Accepted`.

A mirror destination must be a `Service` or a `ServiceImport` — both resolve to an in-cluster DNS name the converter can build directly. An `ExternalBackend` is **not** supported as a mirror destination (only as a primary backend): its URL lives in the CRD spec, which the converter cannot read, and the sentinel-rewrite step does not walk mirror filters. An `ExternalBackend` mirror ref is therefore dropped with `Reason=InvalidKind`.

### Interaction with `appProtocol: https` / `HTTPS`

`appProtocol: https` (or `HTTPS`) is treated as a hint that the backend expects TLS — but the proxy cannot dial TLS on its own without a CA to verify against. The behaviour:

- With a matching `BackendTLSPolicy`: the policy provides the CA, the proxy dials TLS, and the request goes through normally. The `appProtocol` hint is redundant but accepted silently.
- Without a matching `BackendTLSPolicy`: the backend fails closed — requests routed to it receive HTTP 502 instead of being dialed in plaintext — and the route's `ResolvedRefs` condition is set to `False, Reason=UnsupportedProtocol` with a message naming the fix. `kubectl describe httproute` shows the dropped backend; attach a `BackendTLSPolicy` to upgrade the hop to authenticated TLS.

## HTTP CORS filter (`HTTPRouteCORS`)

The L7 proxy honours the Gateway API `HTTPCORSFilter` for both CORS preflight (OPTIONS + `Access-Control-Request-Method`) and simple cross-origin requests.

| Behaviour | Status | Notes |
| --- | --- | --- |
| Exact-match origins (`https://www.foo.com`) | Yes | Compared by full scheme + host + port string |
| Wildcard host (`https://*.bar.com`) | Yes | Greedy left-match against any number of DNS labels; the base domain (`bar.com`) does NOT match — only proper subdomains do |
| Universal wildcard (`"*"` alone) | Yes | Matches every origin |
| Port matching | Yes | When the pattern carries a port, the origin must include the same port (and vice versa). Defaults (80/443) are NOT auto-applied — operators who care about port matching spell it out |
| `allowMethods: ["*"]` | Yes | When the request is uncredentialed and carries `Access-Control-Request-Method`, the requested method is echoed back. When credentialed AND no requested method is in the header, the proxy omits the `Access-Control-Allow-Methods` response header entirely rather than emitting `*` (per spec, `*` with credentials is forbidden) |
| `allowHeaders: ["*"]` | Yes | Same wildcard-vs-credentials logic as `allowMethods` |
| `allowCredentials: true` | Yes | The proxy never emits `*` for `Access-Control-Allow-Origin`, `-Methods`, or `-Headers` while credentials are enabled — it echoes the request's Origin / requested method / requested headers, or omits the header entirely when there is nothing to echo |
| `exposeHeaders` | Yes | Joined with a comma+space separator and stamped on both preflight and simple responses. With `allowCredentials: true` and `exposeHeaders: ["*"]` the header is omitted entirely (spec forbids `*` with credentials) |
| `maxAge` | Yes | Emitted as `Access-Control-Max-Age`. Defaults to 5 seconds when the policy carries 0 (matches the CRD default; applied at emit time so the controller doesn't need to mirror CRD-default logic) |

Preflight handling: a matched OPTIONS preflight short-circuits with HTTP 204 and the negotiated CORS headers — the backend is never hit. A preflight from a non-matched Origin still returns 204 but with no CORS headers, so the browser fails the cross-origin request on the client side. Simple cross-origin requests pass through to the backend and receive `Access-Control-Allow-Origin` (and credentials/expose headers when applicable) stamped on the way back; same-origin requests (no `Origin` header) are untouched.

## Route Types Not Supported

| Route Type | Status | Reason |
|------------|--------|--------|
| TCPRoute | Not supported | Cloudflare Tunnel is HTTP-focused |
| TLSRoute | Not supported | TLS is terminated at edge |
| UDPRoute | Not supported | No UDP support in tunnels |

### Workaround for TCP/UDP

For non-HTTP traffic:

1. Use [Cloudflare Spectrum](https://www.cloudflare.com/products/cloudflare-spectrum/) (separate product)
2. Use a different ingress solution (LoadBalancer, NodePort)

## Full Sync Behavior

Any change to an HTTPRoute or GRPCRoute triggers a full desired-state rebuild from the controller's informer cache:

1. Controller lists all HTTPRoutes and GRPCRoutes (cached, no API traffic)
2. Filters by GatewayClass
3. Rebuilds the entire ingress configuration and diffs it against the deployed one
4. Writes hostname/edge routing to the Cloudflare API **only when the resulting document differs** (one GET always; the PUT is skipped on steady-state syncs)
5. Pushes the rebuilt L7 routing config to the in-process proxy replicas **only when the config content or the replica set changed** (content-hash deduplication; a new replica always receives the current config)

### Why there is no true incremental sync

The Cloudflare Tunnel configurations endpoint is a whole-document update — there is no rule-level PATCH — so a "delta" write to Cloudflare is not expressible: any change requires writing the full ingress document. The controller therefore optimises the only thing it can: it never writes when nothing changed. The full rebuild itself is cheap — benchmarked at roughly 2 ms for a 500-route fleet (`BenchmarkRebuildAndDiff_500Routes`) — so reconcile latency is dominated by the (skippable) network calls, not by the rebuild.

### Implications

- A real route change costs one GET + one PUT to Cloudflare and one config push per proxy replica, regardless of how many routes changed.
- Steady-state reconciles (status updates, endpoint events, periodic resyncs) cost one Cloudflare GET and zero PUTs, and no proxy pushes.
- All routes are re-evaluated on any change; full sync remains the startup and recovery path (drift introduced out-of-band is corrected on the next change-triggering sync).

### Mitigation

For very large deployments:

- Batch route changes when possible
- Monitor Cloudflare API rate limits
- Consider separating high-churn routes to different tunnels

## Route Conflict Resolution

A request first selects a hostname bucket — exact hostname over wildcard over the default (no-hostname) bucket — and then the matching rules within that bucket are ordered by match specificity, highest first:

1. Path match type: exact, then regex, then prefix
2. Longer path value before shorter
3. Then method, header-count, and query-count specificity

The most specific match wins, and the controller does not merge rules from different routes. Hostname only selects the bucket; it is **not** a precedence tiebreaker among matching rules. When two equally-specific rules from different Routes still tie, the cross-Route tiebreak is applied per the Gateway API spec (`httproute_types.go:192-197`): the oldest Route by creationTimestamp wins, then the Route first alphabetically by `{namespace}/{name}`. The within-Route fallback is the first matching rule in list order.

### Example Conflict

```yaml
# Route A
rules:
  - matches:
      - path:
          type: PathPrefix
          value: /api
    backendRefs:
      - name: api-v1

# Route B
rules:
  - matches:
      - path:
          type: PathPrefix
          value: /api/v2
    backendRefs:
      - name: api-v2
```

Route B's `/api/v2` matches first (longer path), then Route A's `/api` matches remaining traffic.

### Case-variant header match names

HTTP header match names are compared case-insensitively at request time (Go's `http.Header.Values` canonicalises the key), but the CRD enforces name uniqueness case-sensitively via its list-map key. Two header matches in one rule that differ only in case (for example `Foo` and `foo`) are therefore both admitted and both required to match, where the spec treats them as equivalent and says only the first should be considered. The effect is a single over-strict (never-matching) rule, not mis-routing; impact is negligible. Query-parameter match names are exact (case-sensitive) string matches per the spec, so `Foo` and `foo` are legitimately distinct parameters and are not affected.

## Route rule name uniqueness (`spec.rules[].name`)

Gateway API states that `HTTPRouteRule.name` / `GRPCRouteRule.name` MUST be unique within a Route when set. Upstream enforces this only through a CEL admission rule that ships in the **experimental**-channel CRDs; the **Standard**-channel CRDs this controller targets omit it, so by default the apiserver admits a Route whose rules share a `name`. Rule names are used only for status diagnostics, not for routing, so a duplicate does not affect traffic — but the normative MUST is unenforced.

The controller does not add an equivalent runtime check: Gateway API defines no status condition for this violation, the `HTTPRoute`/`GRPCRoute` CRDs are owned upstream (the project cannot add the CEL to them), and flagging a working Route as `Accepted=False` would misreport reality.

To enforce uniqueness at admission, enable the bundled opt-in `ValidatingAdmissionPolicy` (`ruleNameUniquenessPolicy.enabled=true` in the Helm chart). It replicates the upstream experimental CEL natively in the apiserver — no webhook server, no CRD ownership — and rejects a Route with duplicate rule names.

### Why it is off by default

The policy is cluster-scoped: it applies to every `HTTPRoute`/`GRPCRoute` in the cluster (a Route carries no `controllerName` at admission, so it cannot be limited to this controller's routes) and can block updates to routes that already have duplicate names. It also requires a cluster with `admissionregistration.k8s.io/v1` ValidatingAdmissionPolicy (Kubernetes 1.30+). Enable it deliberately when those trade-offs are acceptable.

## No Native Multi-Cluster Discovery

The controller performs no direct peer-cluster discovery or mesh-style cross-cluster routing on its own. It does not connect clusters together or watch endpoints in remote clusters. Cross-cluster backends ARE reachable, however, through the Multi-Cluster Services (MCS) API: a `backendRef` targeting a `ServiceImport` (`multicluster.x-k8s.io`) resolves to the `clusterset.local` DNS plane, so the cluster's own MCS implementation routes to the imported remote endpoints (see [Non-Service backend kinds](#non-service-backend-kinds)).

### Workaround

For multi-cluster scenarios:

1. Deploy the controller in each cluster with separate tunnels
2. Use Cloudflare Load Balancing to distribute traffic between tunnels
3. Consider a service mesh with cross-cluster capabilities

## GatewayClass behaviour

### GatewayClass changes propagate to existing Gateways

The Gateway API spec (`gatewayclass_types.go:43`) recommends snapshotting GatewayClass configuration when a Gateway is created and NOT propagating later GatewayClass changes; an implementation that does propagate MUST document it. This controller propagates: it watches GatewayClass and re-reconciles every managed Gateway whenever the GatewayClass spec changes — notably `parametersRef`, which points at the `GatewayClassConfig` holding the Cloudflare credentials and tunnel ID. Credential and tunnel-config updates therefore take effect on already-running Gateways without recreating them.

### `SupportedVersion` condition is verified against the installed CRD bundle

The GatewayClass `SupportedVersion` condition is an Experimental-channel surface (`gatewayclass_types.go:244`, marked `// <gateway:experimental>`); this controller pins and runs the Standard channel (Gateway API v1.6.0), where `SupportedVersion` is not a required condition. The controller populates it anyway as a best-effort operator signal: on each GatewayClass reconcile it reads the `gateway.networking.k8s.io/bundle-version` annotation on the installed `gatewayclasses` CRD and compares its `major.minor` to the Gateway API version the controller is built against (`consts.BundleVersion`). A matching `major.minor` sets `SupportedVersion=True` (patch releases are treated as compatible); an older or newer minor, a missing annotation, or an unreadable CRD sets `SupportedVersion=False` with reason `UnsupportedVersion` and a message naming the mismatch, so a CRD/controller version skew surfaces on status rather than as silent field-drift at runtime.

### The gateway-exists finalizer is managed

The Gateway API spec recommends adding the `gateway-exists-finalizer.gateway.networking.k8s.io` finalizer to a GatewayClass while at least one Gateway uses it. The controller honours this: the finalizer is added when the first Gateway referencing the class appears and removed when the last one goes away, so deleting an in-use GatewayClass blocks until its Gateways are gone.

## Policy discoverability conditions stay on the policy

GEP-713 recommends that implementations surface a policy's effect by writing a condition onto the **affected** objects (the Gateway, or the targeted Service) for discoverability. This controller deviates: BackendTLSPolicy acceptance, conflict, and resolution verdicts are written to the policy's own `status.ancestors` (namespaced per ancestor Gateway and controller, per GEP-713's ancestor-status mechanism), but no condition is stamped onto the affected Gateway or Service objects. Rationale: Services are user-owned objects whose `status` this controller deliberately never writes, and the ancestor entries on the policy already name every affected Gateway — `kubectl describe backendtlspolicy` shows the full effect surface. Use the policy's status, not the Service's, to discover what applies to a backend.

## Redirect port defaulting never needs the listener fallback

The spec says that when a `RequestRedirect` filter sets a scheme with no well-known port and no explicit `port`, the redirect SHOULD fall back to the Gateway listener's port. With the Standard-channel CRD the `scheme` enum is `http`/`https` only — both have well-known ports — so the listener-port fallback branch is unreachable and is not implemented. An explicit `port` in the filter is always honoured; an omitted `port` emits no port in `Location` (the scheme's well-known port is implied).

## Metrics and Observability

The controller provides Prometheus metrics for monitoring, but:

- Per-request access logs are off by default. Enable via `proxy.accessLog.enabled: true` in Helm values; see [Access Logging](../operations/access-logging.md) for the contract, sampling semantics, and the WS-upgrade carve-out. The Cloudflare dashboard remains the only edge-side view (TLS termination, geographic distribution, etc.).
- Edge-side request visibility (traffic volume, geographic distribution, WAF/firewall events) is available only through the Cloudflare dashboard, not from in-cluster metrics.

Distributed tracing is supported (off by default) via OpenTelemetry. Controller-side Cloudflare API calls and the in-cluster request path both emit spans when enabled. See [Distributed Tracing](../operations/tracing.md) for the end-to-end span model, propagation, and the one known gap (WebSocket backend handshakes are not yet span-propagated).

See [Metrics & Alerting](../operations/metrics.md) for available metrics.
