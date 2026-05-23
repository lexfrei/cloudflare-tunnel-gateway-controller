# Limitations

This document describes the known limitations of the Cloudflare Tunnel Gateway
Controller and provides workarounds where applicable.

## Cloudflare Tunnel API Constraints

!!! note "L7 Proxy removes most limitations"
    When using the [L7 proxy](../guides/l7-proxy.md), all features below
    are fully supported. The proxy handles routing in-process, bypassing
    Cloudflare Tunnel ingress API limitations.

The following features require the L7 proxy and are **not** available when
running with only the Cloudflare Tunnel API:

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

Cloudflare Tunnel has specific path matching behavior that differs from Gateway
API expectations:

### No True Exact Path Match

Cloudflare Tunnel does **not** support true exact path matching. A path rule
without a wildcard (e.g., `/api`) will still match subpaths like `/api/v1`.

**Example:** If you configure:

- `/api` (Exact) → backend-a
- `/api` (PathPrefix) → backend-b

Both `/api` and `/api/v1` will route to backend-a because Cloudflare treats
all paths as prefixes internally.

**Workaround:** Use different base paths for different backends:

```yaml
# Instead of same path with different match types
- path: /api-exact    # Exact match
- path: /api-prefix   # Prefix match
```

### Paths with Common Prefixes

Paths sharing a common prefix may exhibit unexpected routing behavior. For
example, `/multi-v1`, `/multi-v2`, and `/multi-v3` might all route to the
first matching backend.

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

## Controller Limitations

| Limitation | Description |
|------------|-------------|
| Single backend | Only highest-weight `backendRef` is used per rule |
| Full sync | Any change triggers full config sync |
| No cross-cluster | Only in-cluster services supported |
| Service only | Only `Service` kind backends (ClusterIP, NodePort, LoadBalancer, ExternalName) |

## Traffic Splitting and Load Balancing

**Design Decision:** This controller intentionally does not implement traffic
splitting or weighted load balancing between multiple backends.

### Why No Traffic Splitting?

Cloudflare Tunnel ingress rules accept only a single service URL per rule.
To support Gateway API's `backendRefs` with weights, the controller would
need to:

1. Create and manage intermediate Kubernetes Services
2. Watch and synchronize Endpoints from all referenced services
3. Handle cross-namespace references and RBAC
4. Manage lifecycle of controller-created resources

This approach introduces significant complexity, potential for orphaned
resources, and creates an opaque traffic path that is difficult for users
to debug.

### Our Approach

Provide the tunnel a single, stable entrypoint. The controller selects the
backend with the highest weight and sends 100% of traffic to it.

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

Deploy a dedicated load balancer (Traefik, Envoy, Nginx, HAProxy) and point
the HTTPRoute to it:

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

This keeps the controller simple and predictable, and gives you full control
over load balancing behavior.

## SSL Certificate Limitations

Cloudflare's free [Universal SSL](https://developers.cloudflare.com/ssl/edge-certificates/universal-ssl/limitations/)
certificates only cover root and first-level subdomains:

| Hostname | Covered | Notes |
|----------|---------|-------|
| `example.com` | Yes | Root domain |
| `*.example.com` | Yes | First-level wildcard |
| `app.example.com` | Yes | First-level subdomain |
| `app.dev.example.com` | No | Multi-level subdomain |
| `*.dev.example.com` | No | Multi-level wildcard |

### Workaround

For multi-level subdomains, you need:

- [Advanced Certificate Manager](https://developers.cloudflare.com/ssl/edge-certificates/advanced-certificate-manager/)
  ($10/month)
- Business or Enterprise plan

## Gateway Listener Configuration

Gateway listeners follow Gateway API specification. Some fields are ignored
because Cloudflare Tunnel manages them at the edge:

| Field | Status | Notes |
|-------|--------|-------|
| `port` | Ignored | Cloudflare uses 443/80 |
| `protocol` | Ignored | Cloudflare handles protocols |
| `hostname` | Supported | Routes must have intersecting hostnames |
| `tls` | Ignored | Cloudflare manages TLS |
| `allowedRoutes` | Supported | Namespace (Same/All/Selector) and kind filtering |

This is because Cloudflare Tunnel terminates TLS at Cloudflare's edge, not
in the cluster. However, `hostname` and `allowedRoutes` are validated per
Gateway API specification.

## Backend Protocol (`Service.spec.ports[].appProtocol`)

The L7 proxy reads the backend Service port's `appProtocol` to pick the upstream
transport. Only one Kubernetes-defined value is implemented today:

| `appProtocol` | Supported | Notes |
| --- | --- | --- |
| _(unset)_ | Yes | Default — proxy speaks HTTP/1.1 to the backend |
| `kubernetes.io/h2c` | Yes | Proxy speaks HTTP/2 cleartext (prior knowledge) |
| `kubernetes.io/ws` | No | Backend WebSocket not implemented; treated as default HTTP — no conformance coverage |
| `kubernetes.io/wss` | No | Backend WebSocket-over-TLS not implemented; treated as default HTTP |
| any other value | No | Logged with a warning at conversion time; proxy falls back to default HTTP/1.1 |

The conformance test `HTTPRouteBackendProtocolWebSocket` is not testable on
this controller via the upstream Gateway API conformance suite because the
test's `websocket.Dial` connects directly to the Gateway address
(`*.cfargotunnel.com`) — same structural limitation as the gRPC conformance
tests documented above. WebSocket backends may be supported separately via
e2e validation in a future change.

## Backend mTLS (BackendTLSPolicy)

The L7 proxy supports Gateway API `BackendTLSPolicy` for proxy → backend TLS,
with the following minimum-viable scope:

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
| `GatewayBackendClientCertificate` (mutual TLS) | No | Requires the experimental Gateway API CRD channel; homelab ships the standard channel |
| HTTPS-listener Re-encrypt (frontend TLS termination + backend TLS) | No | Cloudflare terminates TLS at the edge, so HTTPS listeners aren't supported (see the HTTPRoute HTTPS Listener limitation). The upstream `BackendTLSPolicy` parent test is skipped for the same reason |

Policy status (`Accepted` / `ResolvedRefs`) is maintained per-Gateway-ancestor.
Edits to the CA `ConfigMap` (creation, content patch, or deletion) re-trigger
status reconciliation. The `Status.Ancestors` slice is capped at the spec's
limit of 16 entries; entries are sorted deterministically by
`{namespace, name}` so the truncated set stays stable across reconciles.
`LastTransitionTime` is maintained via `meta.SetStatusCondition`, so it
only flips when `Status`, `Reason`, or `Message` actually changes.

### Fail-closed enforcement

When a `BackendTLSPolicy` targets a Service but cannot be enforced — the CA
`ConfigMap` is missing or unreadable, `ca.crt` is empty, or the bundle is
malformed PEM — the proxy receives a poisoned TLS config (empty CA pool) and
the next request to that Service fails with HTTP 502. It is NOT silently
downgraded to plaintext. The operator's stated intent ("this hop MUST be
authenticated TLS") is preserved. Monitor the policy's `Accepted` condition
to detect the failure mode (`Reason=NoValidCACertificate`).

### Interaction with `appProtocol: kubernetes.io/h2c`

When a backend Service carries both `appProtocol: kubernetes.io/h2c` AND a
`BackendTLSPolicy` targets it, the policy wins: the proxy dials TLS (HTTPS,
with HTTP/2 negotiated via ALPN). h2c is by definition cleartext HTTP/2 and
cannot coexist with backend TLS; the h2c marker is silently ignored on that
hop. If you genuinely want HTTP/2 over TLS, omit the `appProtocol` hint and
let ALPN negotiate it during the handshake.

### RequestMirror filter does not honour BackendTLSPolicy

The Gateway API `RequestMirror` filter sends a fire-and-forget copy of the
request to a secondary backend through a side-channel HTTP client that does
NOT share the proxy's TLS-aware transport pool. If a `BackendTLSPolicy`
targets the mirror destination Service, the mirrored copy is sent in
plaintext — the primary leg still respects the policy, but the mirror leg
silently bypasses it.

The converter surfaces a WARN log when a mirror destination has a matching
policy so operators don't ship a silent bypass. A TLS-aware mirror dial path
is a tracked follow-up; until then, if the mirror destination MUST receive
TLS, route mirror traffic through a separate Service without a policy or use
weighted `backendRefs` instead of the mirror filter.

### Interaction with `appProtocol: https` / `HTTPS`

`appProtocol: https` (or `HTTPS`) is treated as a hint that the backend
expects TLS — but the proxy cannot dial TLS on its own without a CA to
verify against. The behaviour:

- With a matching `BackendTLSPolicy`: the policy provides the CA, the proxy
  dials TLS, and the request goes through normally. The `appProtocol` hint
  is redundant but accepted silently.
- Without a matching `BackendTLSPolicy`: the proxy logs a WARN and falls
  back to plaintext HTTP/1.1. The misconfiguration is visible in logs so
  operators don't ship a broken TLS expectation. Attach a `BackendTLSPolicy`
  to upgrade the hop to authenticated TLS.

## Route Types Not Supported

| Route Type | Status | Reason |
|------------|--------|--------|
| TCPRoute | Not supported | Cloudflare Tunnel is HTTP-focused |
| TLSRoute | Not supported | TLS is terminated at edge |
| UDPRoute | Not supported | No UDP support in tunnels |

### Workaround for TCP/UDP

For non-HTTP traffic:

1. Use [Cloudflare Spectrum](https://www.cloudflare.com/products/cloudflare-spectrum/)
   (separate product)
2. Use a different ingress solution (LoadBalancer, NodePort)

## Full Sync Behavior

Any change to an HTTPRoute or GRPCRoute triggers a full configuration sync
to Cloudflare Tunnel. This means:

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

If routes conflict, the first match wins. The controller does not merge
rules from different routes.

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

Route B's `/api/v2` matches first (longer path), then Route A's `/api`
matches remaining traffic.

## No Multi-Cluster Support

The controller only routes to Services within the same Kubernetes cluster.
Cross-cluster routing is not supported.

### Workaround

For multi-cluster scenarios:

1. Deploy the controller in each cluster with separate tunnels
2. Use Cloudflare Load Balancing to distribute traffic between tunnels
3. Consider a service mesh with cross-cluster capabilities

## Metrics and Observability

The controller provides Prometheus metrics for monitoring, but:

- No distributed tracing integration
- No access logs (use Cloudflare dashboard)
- Limited visibility into Cloudflare API operations

See [Metrics & Alerting](../operations/metrics.md) for available metrics.
