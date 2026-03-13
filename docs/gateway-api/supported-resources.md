# Supported Resources

This document details the feature support matrix for each Gateway API resource
type in the Cloudflare Tunnel Gateway Controller.

## Resource Overview

| Resource | API Version | Status |
| --- | --- | --- |
| GatewayClass | `gateway.networking.k8s.io/v1` | Supported |
| Gateway | `gateway.networking.k8s.io/v1` | Supported |
| HTTPRoute | `gateway.networking.k8s.io/v1` | Supported |
| GRPCRoute | `gateway.networking.k8s.io/v1` | Supported |
| ReferenceGrant | `gateway.networking.k8s.io/v1beta1` | Supported |
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

The Gateway resource is fully processed. Listeners are used for route binding,
status reporting, and validation. TLS termination is handled by Cloudflare edge,
but TLS certificate references are validated (including ReferenceGrant checks).

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
| `spec.addresses` | No | Ignored; tunnel CNAME set automatically in status |
| `spec.infrastructure` | No | Not implemented |

!!! info "TLS Termination"

    Cloudflare Tunnel terminates TLS at Cloudflare's edge network.
    TLS certificate references on listeners are validated (existence,
    ReferenceGrant for cross-namespace refs), but the actual TLS termination
    is handled by Cloudflare, not by the controller. The listener `port` and
    `protocol` fields are used for Gateway API route binding semantics, not
    for configuring network listeners.

## HTTPRoute

The L7 proxy enables full Gateway API HTTPRoute support. Without the proxy,
only path-based routing through the Cloudflare Tunnel API is available.

| Field | Supported | Notes |
| --- | --- | --- |
| `spec.parentRefs` | Yes | References to Gateway |
| `spec.parentRefs[].name` | Yes | Gateway name |
| `spec.parentRefs[].namespace` | Yes | Gateway namespace |
| `spec.parentRefs[].sectionName` | Yes | Listener name (optional) |
| `spec.parentRefs[].port` | Yes | Listener port (optional) |
| `spec.hostnames` | Yes | Wildcard `*` supported |
| `spec.rules` | Yes | Routing rules |
| `spec.rules[].matches` | Yes | Full matching with L7 proxy |
| `spec.rules[].matches[].path` | Yes | PathPrefix, Exact; RegularExpression requires L7 proxy |
| `spec.rules[].matches[].headers` | Yes | Requires L7 proxy |
| `spec.rules[].matches[].queryParams` | Yes | Requires L7 proxy |
| `spec.rules[].matches[].method` | Yes | Requires L7 proxy |
| `spec.rules[].filters` | Yes | Requires L7 proxy; see [Filters](#filters) |
| `spec.rules[].backendRefs` | Yes | Service backends only |
| `spec.rules[].backendRefs[].name` | Yes | Service name |
| `spec.rules[].backendRefs[].namespace` | Yes | Cross-namespace refs require ReferenceGrant |
| `spec.rules[].backendRefs[].port` | Yes | Service port |
| `spec.rules[].backendRefs[].weight` | Yes | Requires L7 proxy for traffic splitting; without proxy, highest weight wins |
| `spec.rules[].backendRefs[].filters` | No | Not implemented |
| `spec.rules[].timeouts` | Yes | Requires L7 proxy |

### Filters

The following HTTPRoute filters are supported with the L7 proxy enabled:

| Filter Type | Supported | Notes |
| --- | --- | --- |
| `RequestHeaderModifier` | Yes | Add, set, or remove request headers |
| `ResponseHeaderModifier` | Yes | Add, set, or remove response headers |
| `RequestRedirect` | Yes | Redirect with scheme, hostname, port, path, status code |
| `URLRewrite` | Yes | Rewrite hostname and/or path |
| `RequestMirror` | Yes | Mirror traffic to a secondary backend |
| `ExtensionRef` | No | Not implemented |

### Supported Service Types

| Service Type | Supported | Notes |
| --- | --- | --- |
| `ClusterIP` | Yes | Routes via cluster-local DNS |
| `NodePort` | Yes | Routes via cluster-local DNS |
| `LoadBalancer` | Yes | Routes via cluster-local DNS |
| `ExternalName` | Yes | Routes directly to external hostname |

## GRPCRoute

| Field | Supported | Notes |
| --- | --- | --- |
| `spec.parentRefs` | Yes | References to Gateway |
| `spec.parentRefs[].name` | Yes | Gateway name |
| `spec.parentRefs[].namespace` | Yes | Gateway namespace |
| `spec.parentRefs[].sectionName` | Yes | Listener name (optional) |
| `spec.hostnames` | Yes | Wildcard `*` supported |
| `spec.rules` | Yes | Routing rules |
| `spec.rules[].matches` | Yes | Service/method matching |
| `spec.rules[].matches[].method.service` | Yes | gRPC service name |
| `spec.rules[].matches[].method.method` | Yes | gRPC method name |
| `spec.rules[].matches[].method.type` | Yes | Exact or RegularExpression |
| `spec.rules[].matches[].headers` | Yes | Requires L7 proxy |
| `spec.rules[].filters` | Yes | Requires L7 proxy |
| `spec.rules[].backendRefs` | Yes | Service backends only |
| `spec.rules[].backendRefs[].name` | Yes | Service name |
| `spec.rules[].backendRefs[].namespace` | Yes | Cross-namespace refs require ReferenceGrant |
| `spec.rules[].backendRefs[].port` | Yes | Service port |
| `spec.rules[].backendRefs[].weight` | Yes | Requires L7 proxy for traffic splitting; without proxy, highest weight wins |
| `spec.rules[].backendRefs[].filters` | No | Not implemented |

## ReferenceGrant

ReferenceGrant enables cross-namespace backend references in HTTPRoute and
GRPCRoute resources.

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

| Type | Without L7 Proxy | With L7 Proxy |
| --- | --- | --- |
| `PathPrefix` | `/api` → `/api*` | Full prefix matching |
| `Exact` | `/health` → `/health` (no true exact match) | True exact matching |
| `RegularExpression` | Not supported | Full regex matching |

!!! warning "Cloudflare Tunnel API Limitations"

    Without the L7 proxy, path matching is handled by Cloudflare Tunnel's
    native ingress rules, which have known limitations: no true exact match
    (Exact paths may match sub-paths), and no regex support.
    See [Limitations](limitations.md) for details.

## gRPC Method Matching

gRPC methods are mapped to HTTP/2 paths using the standard format
`/package.Service/Method`.

| Match Type | Example | Cloudflare Rule |
| --- | --- | --- |
| Service only | `service: mypackage.MyService` | `/mypackage.MyService/*` |
| Service + Method | `service: mypackage.MyService, method: GetUser` | `/mypackage.MyService/GetUser` |
| No match | (empty) | Matches all gRPC traffic |

## Weight and Traffic Splitting

| Mode | Behavior |
| --- | --- |
| Without L7 proxy | Backend with highest `weight` is selected; 100% traffic to single backend |
| With L7 proxy | True weighted traffic splitting across multiple backends |

- **Default weight**: `1` (per Gateway API specification)
- **Zero weight**: Backends with `weight: 0` are disabled
- **Equal weights** (without proxy): First backend in list is selected

!!! warning "Without L7 Proxy"

    Without the L7 proxy, weight selection is NOT traffic splitting. The
    controller always sends 100% of traffic to the highest-weight backend.

!!! danger "No Fallback on Rejection"

    If the highest-weight backend is rejected (e.g., due to missing
    ReferenceGrant for cross-namespace reference), the controller does
    **not** fall back to the next backend. The entire rule is skipped,
    and the route status will show `ResolvedRefs=False`. This is per
    Gateway API specification — weights indicate preference, not failover order.

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
