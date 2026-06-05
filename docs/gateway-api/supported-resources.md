# Supported Resources

This document details the feature support matrix for each Gateway API resource type in the Cloudflare Tunnel Gateway Controller.

## Resource Overview

| Resource | API Version | Status |
| --- | --- | --- |
| GatewayClass | `gateway.networking.k8s.io/v1` | Supported |
| Gateway | `gateway.networking.k8s.io/v1` | Supported |
| HTTPRoute | `gateway.networking.k8s.io/v1` | Supported |
| GRPCRoute | `gateway.networking.k8s.io/v1` | Supported |
| ReferenceGrant | `gateway.networking.k8s.io/v1beta1` | Supported |
| ListenerSet | `gateway.networking.k8s.io/v1` | Supported — see [ListenerSet](listenerset.md) |
| TCPRoute | `gateway.networking.k8s.io/v1alpha2` | Not supported |
| TLSRoute | `gateway.networking.k8s.io/v1alpha2` | Not supported |
| UDPRoute | `gateway.networking.k8s.io/v1alpha2` | Not supported |

## GatewayClass

| Field | Supported | Notes |
| --- | --- | --- |
| `spec.controllerName` | Yes | Must match `--controller-name` flag |
| `spec.parametersRef` | Yes | Via GatewayClassConfig CRD |
| `spec.description` | Yes | Informational only |

## Gateway

The Gateway resource is fully processed. Listeners are used for route binding, status reporting, and validation. TLS termination is handled by Cloudflare edge, but TLS certificate references are validated (including ReferenceGrant checks).

| Field | Supported | Notes |
| --- | --- | --- |
| `spec.gatewayClassName` | Yes | Required; the referenced GatewayClass must have a matching `spec.controllerName` |
| `spec.listeners` | Yes | Fully processed for route binding and status |
| `spec.listeners[].name` | Yes | Used for route binding, status reporting, attached route counting |
| `spec.listeners[].port` | Yes | Used for route binding when route specifies a port |
| `spec.listeners[].protocol` | Yes | Used for route kind filtering (HTTP/HTTPS allow HTTPRoute/GRPCRoute) |
| `spec.listeners[].hostname` | Yes | Routes must have intersecting hostnames |
| `spec.listeners[].tls` | Yes | CertificateRefs validated with ReferenceGrant support |
| `spec.listeners[].allowedRoutes` | Yes | Namespace (Same/All/Selector) and kind filtering |
| `spec.tls.backend.clientCertificateRef` | Yes | `kubernetes.io/tls` Secret only; same-namespace or via ReferenceGrant; presented during backend TLS handshake **only** when the target Service has a BackendTLSPolicy (no client cert is sent over plaintext) |
| `spec.addresses` | No | Ignored; tunnel CNAME set automatically in status |
| `spec.infrastructure` | No | Not implemented |

!!! info "TLS Termination"

    Cloudflare Tunnel terminates TLS at Cloudflare's edge network. TLS certificate references on listeners are validated (existence, ReferenceGrant for cross-namespace refs), but the actual TLS termination is handled by Cloudflare, not by the controller. The listener `port` and `protocol` fields are used for Gateway API route binding semantics, not for configuring network listeners.

## HTTPRoute

All HTTPRoute matching and filter behavior is performed by the in-process L7 proxy that the chart deploys alongside the controller.

| Field | Supported | Notes |
| --- | --- | --- |
| `spec.parentRefs` | Yes | References to Gateway |
| `spec.parentRefs[].name` | Yes | Gateway name |
| `spec.parentRefs[].namespace` | Yes | Gateway namespace |
| `spec.parentRefs[].sectionName` | Yes | Listener name (optional) |
| `spec.parentRefs[].port` | Yes | Listener port (optional) |
| `spec.hostnames` | Yes | Wildcard `*` supported |
| `spec.rules` | Yes | Routing rules |
| `spec.rules[].name` | Yes | Metadata only; preserved on the spec but not consulted during matching |
| `spec.rules[].matches` | Yes | Full matching |
| `spec.rules[].matches[].path` | Yes | PathPrefix, Exact, RegularExpression |
| `spec.rules[].matches[].headers` | Yes | Exact and RegularExpression matchers |
| `spec.rules[].matches[].queryParams` | Yes | Exact and RegularExpression matchers |
| `spec.rules[].matches[].method` | Yes | All HTTP methods |
| `spec.rules[].filters` | Yes | See [Filters](#filters) |
| `spec.rules[].backendRefs` | Yes | `Service`, `ServiceImport`, or `ExternalBackend` — see [Supported Backend Kinds](#supported-backend-kinds) |
| `spec.rules[].backendRefs[].name` | Yes | Name of the Service / ServiceImport / ExternalBackend |
| `spec.rules[].backendRefs[].namespace` | Yes | Cross-namespace refs require ReferenceGrant (keyed on the backend group/kind) |
| `spec.rules[].backendRefs[].port` | Yes | Service / ServiceImport port; ignored for ExternalBackend (its `spec.port` wins) |
| `spec.rules[].backendRefs[].weight` | Yes | True weighted traffic splitting across backends |
| `spec.rules[].backendRefs[].filters` | Yes | Per-backend filters applied after rule-level filters |
| `spec.rules[].timeouts` | Yes | Per-rule request and backend timeouts |

### Backend Protocol (`appProtocol`)

When an HTTPRoute references a Service whose target port sets `appProtocol`, the L7 proxy selects the upstream transport accordingly:

| `Service.spec.ports[].appProtocol` | Supported | Notes |
| --- | --- | --- |
| `kubernetes.io/h2c` | Yes | Proxy speaks HTTP/2 cleartext (prior knowledge) to the backend |
| _(unset)_ | Yes | Proxy speaks HTTP/1.1 to the backend (default) |
| `kubernetes.io/ws` | Yes | WebSocket over cleartext: the proxy detects upgrade headers and routes through a dedicated upgrade path that dials the backend, forwards the handshake, writes the 101 status, then bidirectionally pipes bytes after hijack |
| `kubernetes.io/wss` | Yes | WebSocket over TLS: requires a matching `BackendTLSPolicy` (same precondition as `appProtocol: https`); see [Backend Protocol notes](limitations.md#backend-protocol-servicespecportsappprotocol) |
| any other value | No | Logged with a warning; proxy falls back to default HTTP/1.1 |

A GRPCRoute backend is HTTP/2 by definition, so it is dialed h2c by default and only the TLS-vs-cleartext decision applies: a TLS `appProtocol` (`https` / `HTTPS` / `kubernetes.io/wss`) with no `BackendTLSPolicy` fails the backend closed (HTTP 502, `ResolvedRefs=False, Reason=UnsupportedProtocol`), identical to the HTTPRoute path. See [gRPC backends and appProtocol](limitations.md#grpc-backends-and-appprotocol).

### Filters

The following HTTPRoute filters are supported:

| Filter Type | Supported | Notes |
| --- | --- | --- |
| `RequestHeaderModifier` | Yes | Add, set, or remove request headers |
| `ResponseHeaderModifier` | Yes | Add, set, or remove response headers |
| `RequestRedirect` | Yes | Redirect with scheme, hostname, port, path, status code |
| `URLRewrite` | Yes | Rewrite hostname and/or path |
| `RequestMirror` | Yes | Mirror traffic to one or more secondary backends. Per Gateway API only one of `percent` or `fraction` may be set on a filter; `percent` takes precedence if both appear. |
| `ExtensionRef` | No | Not implemented |

### Supported Service Types

| Service Type | Supported | Notes |
| --- | --- | --- |
| `ClusterIP` | Yes | Routes via cluster-local DNS |
| `NodePort` | Yes | Routes via cluster-local DNS |
| `LoadBalancer` | Yes | Routes via cluster-local DNS |
| `ExternalName` | Yes | Routes directly to external hostname |

### Supported Backend Kinds

A `backendRef` may target one of the following kinds (applies to both HTTPRoute and GRPCRoute):

| Group | Kind | Supported | Notes |
| --- | --- | --- | --- |
| _(core)_ | `Service` | Yes | Default when group/kind are unset; all Service types above |
| `multicluster.x-k8s.io` | `ServiceImport` | Yes | Multi-Cluster Services API; resolved via `clusterset.local` DNS, port validated against `spec.ports` |
| `cf.k8s.lex.la` | `ExternalBackend` | Yes | Out-of-cluster HTTP(S) URL; see [ExternalBackend](external-backend.md) |
| _(any other)_ | _(any other)_ | No | `ResolvedRefs=False, InvalidKind` |

Cross-namespace `ServiceImport`/`ExternalBackend` references require a `ReferenceGrant` whose `to` entry names the matching group/kind, the same way a cross-namespace `Service` ref does.

## GRPCRoute

GRPCRoute is served by the in-process L7 proxy. gRPC requests are HTTP/2 POSTs to `/{service}/{method}`, so the converter maps each method match onto the proxy's path matcher. The upstream hop is cleartext h2c by default; attaching a `BackendTLSPolicy` upgrades the hop to TLS with HTTP/2 negotiated via ALPN, and the Gateway's `clientCertificateRef` is presented on the handshake for mTLS.

| Field | Supported | Notes |
| --- | --- | --- |
| `spec.parentRefs` | Yes | Gateway or ListenerSet, same as HTTPRoute |
| `spec.hostnames` | Yes | Wildcard `*` supported |
| `spec.rules[].matches[].method` | Yes | `Exact` (service+method, service-only, method-only) and `RegularExpression` |
| `spec.rules[].matches[].headers` | Yes | Exact and RegularExpression matchers (shared with HTTP) |
| `spec.rules[].backendRefs` | Yes | Service backends; cleartext h2c by default, TLS + ALPN HTTP/2 when a BackendTLSPolicy targets the Service |
| `spec.rules[].filters` | Partial | Core `RequestHeaderModifier` and extended `ResponseHeaderModifier` are served through the shared header-modifier pipeline (rule- and backend-scoped). `RequestMirror` and `ExtensionRef` are not served yet and fail closed (matching requests receive HTTP 500). |
| BackendTLSPolicy / Gateway `clientCertificateRef` | Yes | When a `BackendTLSPolicy` targets the backend Service, the proxy dials TLS with HTTP/2 negotiated via ALPN. The Gateway's `clientCertificateRef` is presented for mTLS only when a policy is also attached (Gateway API spec — a client cert over plaintext is meaningless). See [GRPCRoute docs](grpcroute.md#backend-tls). |

gRPC method matching maps to paths as follows:

| Method match | Proxy path rule |
| --- | --- |
| `Exact` service + method | exact `/{service}/{method}` |
| `Exact` service only | prefix `/{service}/` (all methods) |
| `Exact` method only | regex `/[^/]+/{method}` (any service) |
| `RegularExpression` | regex `/{service}/{method}` (as written) |

!!! note "Conformance runs through an edge-routing gRPC client"

    The upstream Gateway API gRPC conformance tests dial the Gateway address (`*.cfargotunnel.com`) directly, and that hostname resolves to Cloudflare's ULA range, which is not externally routable. The suite allows injecting a custom gRPC client, so the conformance run supplies one that dials the Cloudflare edge instead — the same approach the HTTP suite already uses via a custom `RoundTripper`. `GRPCExactMethodMatching`, `GRPCRouteHeaderMatching`, `GRPCRouteNamedRule`, and `GRPCRouteListenerHostnameMatching` run this way (the run pins the `http2` tunnel transport, since gRPC trailers do not survive QUIC). One test stays skipped: `GRPCRouteWeight`, whose distribution sampler news its own client and bypasses the injectable one ([kubernetes-sigs/gateway-api#4926](https://github.com/kubernetes-sigs/gateway-api/issues/4926)). gRPC routing is additionally validated end-to-end by the in-house e2e suite against a real tunnel.

## ReferenceGrant

ReferenceGrant enables cross-namespace backend references in HTTPRoute and GRPCRoute resources.

| Field | Supported | Notes |
| --- | --- | --- |
| `spec.from` | Yes | Source routes (HTTPRoute, GRPCRoute) |
| `spec.from[].group` | Yes | Must be `gateway.networking.k8s.io` |
| `spec.from[].kind` | Yes | Must be `HTTPRoute` or `GRPCRoute` |
| `spec.from[].namespace` | Yes | Namespace where routes are located |
| `spec.to` | Yes | Target resources (Services) |
| `spec.to[].group` | Yes | Must be `""` (core) for Services |
| `spec.to[].kind` | Yes | Must be `Service` |
| `spec.to[].name` | Yes | Optional; if omitted, all Services allowed |

See [ReferenceGrant](referencegrant.md) for detailed examples.

## Path Matching

All matching is performed by the in-process L7 proxy:

| Type | Behavior |
| --- | --- |
| `PathPrefix` | Full prefix matching |
| `Exact` | True exact matching |
| `RegularExpression` | Full regex matching (RE2) |

## gRPC Method Matching

gRPC methods are mapped to HTTP/2 paths using the standard format `/package.Service/Method`. All matching happens inside the in-process L7 proxy (see the [GRPCRoute](#grpcroute) table above for the same mapping in the route context).

| Match Type | Example | Proxy path rule |
| --- | --- | --- |
| Service only | `service: mypackage.MyService` | prefix `/mypackage.MyService/` |
| Service + Method | `service: mypackage.MyService, method: GetUser` | exact `/mypackage.MyService/GetUser` |
| Method only | `method: GetUser` | regex `/[^/]+/GetUser` (any service) |
| No match | (empty) | Matches all gRPC traffic |

## Weight and Traffic Splitting

True weighted traffic splitting across multiple backends is performed by the in-process L7 proxy.

- **Default weight**: `1` (per Gateway API specification)
- **Zero weight**: Backends with `weight: 0` are disabled

!!! warning "An invalid backend ref fails its own traffic fraction"

    If a backend ref in a rule fails validation — cross-namespace denial (`RefNotPermitted`), a missing Service (`BackendNotFound`), or an unsupported kind/port — it stays in the weighted pool and the proxy returns HTTP 500 for its traffic fraction only; the other backends in the same rule keep serving their proportional share. The route status shows `ResolvedRefs=False`. Weighting is not failover: an invalid ref fails closed for its fraction rather than silently shifting that traffic to the healthy backends. Only when *every* backend in a rule is unavailable (or all carry `weight: 0`) does the rule return 500 for all of its traffic. A `weight: 0` invalid ref carries no traffic, so it is dropped rather than marked. See [Unavailable backends return a status, not a dial error](limitations.md#unavailable-backends-return-a-status-not-a-dial-error).

## Status Conditions

### Gateway Conditions

| Type | Status | Reason | Description |
| --- | --- | --- | --- |
| `Accepted` | `True` | `Accepted` | Gateway accepted by controller |
| `Accepted` | `False` | `ListenersNotValid` | One or more of the Gateway's own listeners conflict (carry `Conflicted=True`) |
| `Programmed` | `True` | `Programmed` | Gateway configured in Cloudflare |

### Gateway Listener Conditions

| Type | Status | Reason | Description |
| --- | --- | --- | --- |
| `Accepted` | `True` | `Accepted` | Listener accepted |
| `Accepted` | `False` | `HostnameConflict` / `ProtocolConflict` | Listener conflicts with a higher-precedence listener on the same port |
| `Programmed` | `True` | `Programmed` | Listener programmed |
| `Programmed` | `False` | `Invalid` | Listener has unresolved references |
| `Programmed` | `False` | `HostnameConflict` / `ProtocolConflict` | Listener conflicts with a higher-precedence listener |
| `Conflicted` | `True` | `HostnameConflict` / `ProtocolConflict` | Listener clashes with another listener on hostname (same port + hostname) or protocol (different protocol on the same port) |
| `ResolvedRefs` | `True` | `ResolvedRefs` | References resolved |
| `ResolvedRefs` | `False` | `InvalidCertificateRef` | TLS certificate reference invalid |
| `ResolvedRefs` | `False` | `RefNotPermitted` | Cross-namespace TLS ref denied by ReferenceGrant |
| `ResolvedRefs` | `False` | `InvalidRouteKinds` | Invalid route kind in allowedRoutes |

### HTTPRoute/GRPCRoute Conditions

| Type | Status | Reason | Description |
| --- | --- | --- | --- |
| `Accepted` | `True` | `Accepted` | Route accepted and synced |
| `Accepted` | `False` | `NoMatchingParent` | No matching listener found |
| `Accepted` | `False` | `NoMatchingListenerHostname` | Route hostnames don't intersect with listener |
| `Accepted` | `False` | `NotAllowedByListeners` | Route namespace or kind not allowed by listener |
| `Accepted` | `False` | `Conflicted` | Route lost a cross-route-type conflict (HTTPRoute vs GRPCRoute on a shared Gateway with intersecting hostnames); the oldest Route by `creationTimestamp` is accepted |
| `ResolvedRefs` | `True` | `ResolvedRefs` | Backend references resolved |
| `ResolvedRefs` | `False` | `RefNotPermitted` | Cross-namespace reference denied |
| `ResolvedRefs` | `False` | `BackendNotFound` | Backend Service not found |
