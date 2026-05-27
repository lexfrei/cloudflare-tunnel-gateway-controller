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
| `spec.rules[].backendRefs` | Yes | Service backends only |
| `spec.rules[].backendRefs[].name` | Yes | Service name |
| `spec.rules[].backendRefs[].namespace` | Yes | Cross-namespace refs require ReferenceGrant |
| `spec.rules[].backendRefs[].port` | Yes | Service port |
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

## GRPCRoute

GRPCRoute is served by the in-process L7 proxy. gRPC requests are HTTP/2 POSTs to `/{service}/{method}`, so the converter maps each method match onto the proxy's path matcher and forces the upstream hop to h2c (cleartext HTTP/2).

| Field | Supported | Notes |
| --- | --- | --- |
| `spec.parentRefs` | Yes | Gateway or ListenerSet, same as HTTPRoute |
| `spec.hostnames` | Yes | Wildcard `*` supported |
| `spec.rules[].matches[].method` | Yes | `Exact` (service+method, service-only, method-only) and `RegularExpression` |
| `spec.rules[].matches[].headers` | Yes | Exact and RegularExpression matchers (shared with HTTP) |
| `spec.rules[].backendRefs` | Yes | Service backends; the proxy speaks h2c upstream |
| `spec.rules[].filters` | No | gRPC filters are not yet applied by the proxy; logged and skipped |
| BackendTLSPolicy / Gateway `clientCertificateRef` | No | Not applied to gRPC backends in this revision; the upstream hop is always cleartext h2c and any TLS policy is silently ignored |

gRPC method matching maps to paths as follows:

| Method match | Proxy path rule |
| --- | --- |
| `Exact` service + method | exact `/{service}/{method}` |
| `Exact` service only | prefix `/{service}/` (all methods) |
| `Exact` method only | regex `/[^/]+/{method}` (any service) |
| `RegularExpression` | regex `/{service}/{method}` (as written) |

!!! note "Conformance dial limitation"

    The upstream Gateway API gRPC conformance tests dial the Gateway address (`*.cfargotunnel.com`) directly. That hostname resolves to Cloudflare's ULA range, which is not externally routable, so those tests cannot reach this controller and remain skipped. gRPC routing is validated end-to-end by the in-house e2e suite against a real tunnel instead.

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

gRPC methods are mapped to HTTP/2 paths using the standard format `/package.Service/Method`.

| Match Type | Example | Cloudflare Rule |
| --- | --- | --- |
| Service only | `service: mypackage.MyService` | `/mypackage.MyService/*` |
| Service + Method | `service: mypackage.MyService, method: GetUser` | `/mypackage.MyService/GetUser` |
| No match | (empty) | Matches all gRPC traffic |

## Weight and Traffic Splitting

True weighted traffic splitting across multiple backends is performed by the in-process L7 proxy.

- **Default weight**: `1` (per Gateway API specification)
- **Zero weight**: Backends with `weight: 0` are disabled

!!! danger "No Fallback on Rejection"

    If the highest-weight backend is rejected (e.g., due to missing ReferenceGrant for cross-namespace reference), the controller does **not** fall back to the next backend. The entire rule is skipped, and the route status will show `ResolvedRefs=False`. This is per Gateway API specification — weights indicate preference, not failover order.

## Status Conditions

### Gateway Conditions

| Type | Status | Reason | Description |
| --- | --- | --- | --- |
| `Accepted` | `True` | `Accepted` | Gateway accepted by controller |
| `Programmed` | `True` | `Programmed` | Gateway configured in Cloudflare |

### Gateway Listener Conditions

| Type | Status | Reason | Description |
| --- | --- | --- | --- |
| `Accepted` | `True` | `Accepted` | Listener accepted |
| `Programmed` | `True` | `Programmed` | Listener programmed |
| `Programmed` | `False` | `Invalid` | Listener has unresolved references |
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
| `ResolvedRefs` | `True` | `ResolvedRefs` | Backend references resolved |
| `ResolvedRefs` | `False` | `RefNotPermitted` | Cross-namespace reference denied |
| `ResolvedRefs` | `False` | `BackendNotFound` | Backend Service not found |
