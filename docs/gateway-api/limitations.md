# Limitations

This document describes the known limitations of the Cloudflare Tunnel Gateway Controller and provides workarounds where applicable.

## Cloudflare Tunnel API Constraints

!!! note "L7 Proxy removes most limitations"
    When using the [L7 proxy](../guides/l7-proxy.md), all features below
    are fully supported. The proxy handles routing in-process, bypassing
    Cloudflare Tunnel ingress API limitations.

The following features require the L7 proxy and are **not** available when running with only the Cloudflare Tunnel API:

| Feature | Without L7 proxy | With L7 proxy |
| --- | --- | --- |
| Exact path matching | No | Yes |
| Header matching | No | Yes |
| Query parameter matching | No | Yes |
| Method matching | No | Yes |
| Request header modification | No | Yes |
| Response header modification | No | Yes |
| Request redirect | No | Yes |
| URL rewrite | No | Yes |
| Request mirroring | No | Yes |
| Traffic splitting (weighted) | No | Yes |
| Regex path matching | No | Yes |

## Cloudflare Tunnel Path Matching Limitations

Cloudflare Tunnel has specific path matching behavior that differs from Gateway API expectations:

### No True Exact Path Match

Cloudflare Tunnel does **not** support true exact path matching. A path rule without a wildcard (e.g., `/api`) will still match subpaths like `/api/v1`.

**Example:** If you configure:

- `/api` (Exact) → backend-a
- `/api` (PathPrefix) → backend-b

Both `/api` and `/api/v1` will route to backend-a because Cloudflare treats all paths as prefixes internally.

**Workaround:** Use different base paths for different backends:

```yaml
# Instead of same path with different match types
- path: /api-exact    # Exact match
- path: /api-prefix   # Prefix match
```

### Paths with Common Prefixes

Paths sharing a common prefix may exhibit unexpected routing behavior. For example, `/multi-v1`, `/multi-v2`, and `/multi-v3` might all route to the first matching backend.

**Workaround:** Use distinct path prefixes:

```yaml
# Instead of:
- path: /multi-v1
- path: /multi-v2
- path: /multi-v3

# Use:
- path: /alpha
- path: /beta
- path: /gamma
```

### Path Priority

The controller sorts paths to ensure consistent behavior:

1. Longer paths match before shorter paths (`/api/v2` before `/api`)
2. Paths of equal length are sorted alphabetically for determinism
3. Wildcard hostname `*` always comes last

This ensures predictable routing despite Cloudflare's limitations.

## GRPCRoute is not supported in v3

The L7 proxy is the only data plane in v3 (the vendored cloudflared fork's `OverrideProxy` hook is always wired to it), and the proxy converter does not yet implement gRPC-specific route matching. As a consequence, gRPC traffic that flows through the tunnel reaches the proxy without a matching routing rule and returns `404 no matching route`.

The controller continues to accept GRPCRoute resources and pushes a Cloudflare-side ingress config for them via `internal/ingress/grpc_builder.go`, but those edge-side rules are **not consulted at runtime in v3** — they exist only so the Cloudflare dashboard shows the expected hostname → service mapping.

**v2 → v3 impact.** Users on v2 with `proxy.enabled: false` (the v2 default) had working GRPCRoute via cloudflared's native ingress. v3 removes that path. If you have any GRPCRoute resources today, migrate them to HTTPRoute before upgrading, or stay on the v2.x chart line until the proxy converter learns gRPC.

GRPCRoute support inside the proxy converter is on the v3.x roadmap; the regression is intentional and documented rather than promised on a specific timeline.

## Controller Limitations

| Limitation | Description |
|------------|-------------|
| Single backend | Only highest-weight `backendRef` is used per rule |
| Full sync | Any change triggers full config sync |
| No cross-cluster | Only in-cluster services supported |
| Service only | Only `Service` kind backends (ClusterIP, NodePort, LoadBalancer, ExternalName) |

## Traffic Splitting and Load Balancing

**Design Decision:** This controller intentionally does not implement traffic splitting or weighted load balancing between multiple backends.

### Why No Traffic Splitting?

Cloudflare Tunnel ingress rules accept only a single service URL per rule. To support Gateway API's `backendRefs` with weights, the controller would need to:

1. Create and manage intermediate Kubernetes Services
2. Watch and synchronize Endpoints from all referenced services
3. Handle cross-namespace references and RBAC
4. Manage lifecycle of controller-created resources

This approach introduces significant complexity, potential for orphaned resources, and creates an opaque traffic path that is difficult for users to debug.

### Our Approach

Provide the tunnel a single, stable entrypoint. The controller selects the backend with the highest weight and sends 100% of traffic to it.

### Workarounds

**Between pods of the same Deployment:**

Use a standard Kubernetes Service (built-in round-robin load balancing):

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

**Weighted traffic splitting or canary:**

Deploy a dedicated load balancer (Traefik, Envoy, Nginx, HAProxy) and point the HTTPRoute to it:

```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: my-app
spec:
  parentRefs:
    - name: cloudflare-tunnel
      namespace: cloudflare-tunnel-system
  hostnames:
    - app.example.com
  rules:
    - backendRefs:
        - name: traefik  # Traefik handles weighted routing internally
          port: 80
```

This keeps the controller simple and predictable, and gives you full control over load balancing behavior.

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

- [Advanced Certificate Manager](https://developers.cloudflare.com/ssl/edge-certificates/advanced-certificate-manager/) ($10/month)
- Business or Enterprise plan

## Gateway Listener Configuration

Gateway listeners follow Gateway API specification. Some fields are ignored because Cloudflare Tunnel manages them at the edge:

| Field | Status | Notes |
|-------|--------|-------|
| `port` | Ignored | Cloudflare uses 443/80 |
| `protocol` | Ignored | Cloudflare handles protocols |
| `hostname` | Supported | Routes must have intersecting hostnames |
| `tls` | Ignored | Cloudflare manages TLS |
| `allowedRoutes` | Supported | Namespace (Same/All/Selector) and kind filtering |

This is because Cloudflare Tunnel terminates TLS at Cloudflare's edge, not in the cluster. However, `hostname` and `allowedRoutes` are validated per Gateway API specification.

## Backend Protocol (`Service.spec.ports[].appProtocol`)

The L7 proxy reads the backend Service port's `appProtocol` to pick the upstream transport. The supported Kubernetes-defined values:

| `appProtocol` | Supported | Notes |
| --- | --- | --- |
| _(unset)_ | Yes | Default — proxy speaks HTTP/1.1 to the backend |
| `kubernetes.io/h2c` | Yes | Proxy speaks HTTP/2 cleartext (prior knowledge) |
| `kubernetes.io/ws` | Yes | The proxy detects `Connection: Upgrade` + `Upgrade: websocket` headers and routes the request through a dedicated WebSocket upgrade path that dials the backend, forwards the upgrade, writes the 101 to the response writer, and bidirectionally copies bytes after hijack. Plain HTTP requests to the same backend continue to use the default HTTP/1.1 transport |
| `kubernetes.io/wss` | Yes | Requires a `BackendTLSPolicy` targeting the Service (same precondition as `appProtocol: https`); without one the proxy logs a WARN and falls back to plaintext, which the backend will refuse |
| any other value | No | Logged with a warning at conversion time; proxy falls back to default HTTP/1.1 |

The upstream conformance test `HTTPRouteBackendProtocolWebSocket` is not testable through Cloudflare Tunnel: it dials the Gateway address directly via `golang.org/x/net/websocket.Dial` with no RoundTripper hook, and our Gateway address is `<tunnel-id>.cfargotunnel.com` whose AAAA records point at Cloudflare's ULA (`fd10::/8`), which is unreachable from any test runner. Same structural limitation as the gRPC conformance tests documented above. `test/e2e/e2e_backend_protocol_websocket_test.go` is the substitute proof — it runs an end-to-end WebSocket round trip against the real tunnel hostname.

### Interaction with `appProtocol: kubernetes.io/ws`

When a backend Service carries both `appProtocol: kubernetes.io/ws` AND a `BackendTLSPolicy` targeting the same Service, the TLS policy wins: `resolveBackendTLS` rewrites the URL to `https://`, the proxy completes a TLS handshake, and the WebSocket upgrade runs over TLS regardless of the cleartext appProtocol hint. A WARN surfaces the suppressed `ws` hint so operators can either drop the BackendTLSPolicy (if they actually wanted cleartext WebSocket) or flip the hint to `kubernetes.io/wss` (if they wanted the TLS-protected variant all along).

### `ResponseHeaderModifier` MUST preserve WebSocket handshake headers

The L7 proxy applies route-level + per-backend `ResponseHeaderModifier` filters to every backend response, including the 101 Switching Protocols response that carries the WebSocket handshake. Per Gateway API spec the filter pipeline runs unconditionally; the proxy makes no exception for upgrade responses. The operator-facing consequence: a `Remove` list that strips `Sec-WebSocket-Accept`, `Upgrade`, or `Connection` on a route whose backend is WS-marked silently breaks every upgrade on that route. The 101 reaches the client missing a header the RFC 6455 handshake requires, and the client just disconnects.

The converter scans rule-level and per-backend `ResponseHeaderModifier` filters at HTTPRoute apply time. If a `Remove` list on a WS-marked route intersects `{Sec-WebSocket-Accept, Upgrade, Connection}`, the controller logs a WARN naming the offending header(s) and the filter scope (`rule` or `backend`). The filter still applies as configured — the warning is a diagnostic, not a hard rejection, because the misconfiguration is operator-fixable and bypassing the filter would silently violate spec.

Same guidance applies symmetrically to `Set` overriding these headers with a non-handshake-compatible value, though that is rarer and not currently checked.

### Interaction with `spec.rules[].timeouts`

`timeouts.request` and `timeouts.backendRequest` are enforced as **header-only deadlines** on the backend transport (`http.Transport.ResponseHeaderTimeout`), not as full-request context deadlines. The deadline bounds only the wait for backend response headers; once headers arrive, the body streams freely. Both Gateway API knobs collapse onto the same transport-level header timeout because this proxy has no retry logic — a single backend attempt is the whole request. When both knobs are set the stricter (`min(Request, Backend)`) value wins.

This is a deliberate spec interpretation. The spec is underspecified on whether `timeouts.request` should kill an in-flight streaming response (Server-Sent Events, chunked transfer, large file downloads, gRPC server-streaming). A context-based deadline cancels the body read mid-stream and truncates the response at the timeout boundary, which is hostile to any streaming workload. The header-only deadline avoids that while still catching slow-to-respond backends in the dial-and-headers phase — exactly where timeouts are operationally useful.

Backends that take longer than the deadline to send response headers get a 504 Gateway Timeout to the client (the transport's `ResponseHeaderTimeout` error is mapped to 504 in `errorHandler`, parallel to the existing 504 for `context.DeadlineExceeded`).

Symmetric consequence on request uploads: per the stdlib godoc, `ResponseHeaderTimeout` starts measuring only _after_ the request body is fully written. A streaming or very large request upload (chunked PUT, multipart, gRPC client-streaming) is therefore NOT bounded by `timeouts.request` either. Operators who expected `timeouts.request` to act as an upload budget should know that the deadline starts only when the upload completes and the wait for response headers begins. The transport's connect timeout (dial / TLS handshake) still bounds the establishment phase.

The same shift removes the slow-loris-upload protection the old context-based deadline incidentally provided: a malicious client that drip-feeds request body bytes will keep the proxy → backend conn open as long as it sends at least one byte before the underlying TCP read timeout. The Cloudflare edge in front of the tunnel has its own upload deadlines, so the operational risk in production is bounded by edge policy rather than by `timeouts.request`. Per-rule upload-phase deadlines are out of scope for this knob and would need a separate mechanism (e.g. a wrapping `io.Reader` with its own inter-byte deadline) if ever required.

WebSocket routes are naturally exempt: WS upgrades flow through the dedicated `proxyWebSocketUpgrade` path (because cloudflared's HTTP/2 response writer cannot be hijacked the way stdlib `httputil.ReverseProxy` expects), and that path bypasses the cached transport entirely. Once the upgrade completes, two `io.Copy` goroutines pipe bytes bidirectionally between the hijacked client conn and the backend conn; they run until either side closes its conn.

The `BackendProtocol: H2C` path uses `golang.org/x/net/http2.Transport`, which does not expose a `ResponseHeaderTimeout` knob. The proxy synthesises one by wrapping the h2c transport with a `headerTimeoutRoundTripper` that cancels the request context if response headers do not arrive within the per-rule deadline, then releases the cancellation on body Close so streaming bodies (SSE / chunked / gRPC server-streaming) survive past the deadline. The h2c dialer's own connect timeout still catches a fully dead backend.

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
| `WellKnownCACertificates: System` | No | Only explicit `CACertificateRefs` are honoured |
| Multiple `targetRefs` per policy | Yes | All targeted Services share the same TLS config |
| `SectionName` per-port targeting | Yes | When a `TargetRef` carries `sectionName`, only the matching named Service port receives TLS; siblings on the same Service stay plaintext |
| Conflict resolution across multiple policies on the same target | Partial | Oldest-creationTimestamp wins, alphabetical name on tie. Losers are NOT yet stamped `Accepted=False, Reason=Conflicted` — they share the winner's `Accepted=True`. The upstream `BackendTLSPolicyConflictResolution` conformance subtest is skipped pending that work |
| Cross-namespace CA refs | No | Same-namespace only |
| `GatewayBackendClientCertificate` (mutual TLS) | Yes | The Gateway's `spec.tls.backend.clientCertificateRef` (Standard channel) loads a `kubernetes.io/tls` Secret and the proxy presents the keypair during backend TLS handshakes. Cross-namespace refs require ReferenceGrant. The client cert is attached **only** when the target Service has a `BackendTLSPolicy` — sending a cert over plaintext is meaningless. When an HTTPRoute attaches to multiple Gateways, the first parentRef managed by this controller that has a resolvable client cert wins; foreign-controller parents and parents without a cert are skipped, not blocking. The conformance test does not exercise this multi-parent edge case |
| HTTPS-listener Re-encrypt (frontend TLS termination + backend TLS) | No | Cloudflare terminates TLS at the edge, so HTTPS listeners aren't supported (see the HTTPRoute HTTPS Listener limitation). The upstream `BackendTLSPolicy` parent test is skipped for the same reason |

Policy status (`Accepted` / `ResolvedRefs`) is maintained per-Gateway-ancestor. Edits to the CA `ConfigMap` (creation, content patch, or deletion) re-trigger status reconciliation. The `Status.Ancestors` slice is capped at the spec's limit of 16 entries; entries are sorted deterministically by `{namespace, name}` so the truncated set stays stable across reconciles. `LastTransitionTime` is maintained via `meta.SetStatusCondition`, so it only flips when `Status`, `Reason`, or `Message` actually changes.

### Fail-closed enforcement

When a `BackendTLSPolicy` targets a Service but cannot be enforced — the CA `ConfigMap` is missing or unreadable, `ca.crt` is empty, or the bundle is malformed PEM — the proxy receives a poisoned TLS config (empty CA pool) and the next request to that Service fails with HTTP 502. It is NOT silently downgraded to plaintext. The operator's stated intent ("this hop MUST be authenticated TLS") is preserved. Monitor the policy's `Accepted` condition to detect the failure mode (`Reason=NoValidCACertificate`).

### Interaction with `appProtocol: kubernetes.io/h2c`

When a backend Service carries both `appProtocol: kubernetes.io/h2c` AND a `BackendTLSPolicy` targets it, the policy wins: the proxy dials TLS (HTTPS, with HTTP/2 negotiated via ALPN). h2c is by definition cleartext HTTP/2 and cannot coexist with backend TLS; the h2c marker is silently ignored on that hop. If you genuinely want HTTP/2 over TLS, omit the `appProtocol` hint and let ALPN negotiate it during the handshake.

### Gateway client cert rotation has a propagation lag

The controller's existing Secret watch only matches the GatewayClassConfig's `cloudflareCredentialsSecretRef`. A rotation of the `Secret` referenced by `spec.tls.backend.clientCertificateRef` (or by a listener's `certificateRefs`) does NOT enqueue the Gateway on its own — the proxy continues to dial backends with the previous keypair until some unrelated event (HTTPRoute create/update, BackendTLSPolicy change, periodic resync, controller restart) drives the next reconcile. On that reconcile the new keypair is loaded, the converter stamps it onto every affected `BackendTLSConfig`, and the per-cert transport-pool hash evicts the stale transport on the next config push.

In active clusters the propagation window is short, but operators that rotate certs more frequently than other Gateway-namespace events occur will observe stale-cert traffic until the next reconcile fires. Active hot-reload on a Secret-only change is a tracked follow-up — extending `ConfigMapper.MapSecretToRequests` to also match Gateway-level `clientCertificateRef` Secrets is the planned fix.

### RequestMirror filter does not honour BackendTLSPolicy

The Gateway API `RequestMirror` filter sends a fire-and-forget copy of the request to a secondary backend through a side-channel HTTP client that does NOT share the proxy's TLS-aware transport pool. If a `BackendTLSPolicy` targets the mirror destination Service, the mirrored copy is sent in plaintext — the primary leg still respects the policy, but the mirror leg silently bypasses it.

The converter surfaces a WARN log when a mirror destination has a matching policy so operators don't ship a silent bypass. A TLS-aware mirror dial path is a tracked follow-up; until then, if the mirror destination MUST receive TLS, route mirror traffic through a separate Service without a policy or use weighted `backendRefs` instead of the mirror filter.

### Interaction with `appProtocol: https` / `HTTPS`

`appProtocol: https` (or `HTTPS`) is treated as a hint that the backend expects TLS — but the proxy cannot dial TLS on its own without a CA to verify against. The behaviour:

- With a matching `BackendTLSPolicy`: the policy provides the CA, the proxy dials TLS, and the request goes through normally. The `appProtocol` hint is redundant but accepted silently.
- Without a matching `BackendTLSPolicy`: the proxy logs a WARN and falls back to plaintext HTTP/1.1. The misconfiguration is visible in logs so operators don't ship a broken TLS expectation. Attach a `BackendTLSPolicy` to upgrade the hop to authenticated TLS.

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

Any change to an HTTPRoute or GRPCRoute triggers a full configuration sync to Cloudflare Tunnel. This means:

1. Controller lists all HTTPRoutes and GRPCRoutes
2. Filters by GatewayClass
3. Rebuilds entire ingress configuration
4. Pushes to Cloudflare API

### Implications

- More API calls than incremental updates
- Brief delay when many routes are present
- All routes are re-evaluated on any change

### Mitigation

For large deployments:

- Batch route changes when possible
- Monitor Cloudflare API rate limits
- Consider separating high-churn routes to different tunnels

## Route Conflict Resolution

Routes are processed in order:

1. Exact path matches first
2. Longer prefix paths before shorter
3. Alphabetically by hostname

If routes conflict, the first match wins. The controller does not merge rules from different routes.

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

## No Multi-Cluster Support

The controller only routes to Services within the same Kubernetes cluster. Cross-cluster routing is not supported.

### Workaround

For multi-cluster scenarios:

1. Deploy the controller in each cluster with separate tunnels
2. Use Cloudflare Load Balancing to distribute traffic between tunnels
3. Consider a service mesh with cross-cluster capabilities

## Metrics and Observability

The controller provides Prometheus metrics for monitoring, but:

- No distributed tracing integration
- Per-request access logs are off by default. Enable via `proxy.accessLog.enabled: true` in Helm values; see [Access Logging](../operations/access-logging.md) for the contract, sampling semantics, and the WS-upgrade carve-out. The Cloudflare dashboard remains the only edge-side view (TLS termination, geographic distribution, etc.).
- Limited visibility into Cloudflare API operations

See [Metrics & Alerting](../operations/metrics.md) for available metrics.
