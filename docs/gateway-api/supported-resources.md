# Supported Resources

This document details the feature support matrix for each Gateway API resource
type in the Cloudflare Tunnel Gateway Controller.

## Resource Overview

| Resource | API Version | Status |
|----------|-------------|--------|
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
|-------|-----------|-------|
| `spec.controllerName` | Yes | Must match `--controller-name` flag |
| `spec.parametersRef` | Yes | Via GatewayClassConfig CRD |
| `spec.description` | Yes | Informational only |

## Gateway

The Gateway resource is accepted but most listener configuration is ignored
because Cloudflare Tunnel handles TLS termination and port management at
the edge.

| Field | Supported | Notes |
|-------|-----------|-------|
| `spec.gatewayClassName` | Yes | Required, must match configured class |
| `spec.listeners` | Partial | Accepted for compatibility, not used for routing |
| `spec.listeners[].name` | Yes | Used for status reporting and sectionName matching |
| `spec.listeners[].port` | No | Ignored; Cloudflare uses standard 443/80 |
| `spec.listeners[].protocol` | No | Ignored; Cloudflare handles protocol negotiation |
| `spec.listeners[].hostname` | No | Ignored; use HTTPRoute/GRPCRoute hostnames |
| `spec.listeners[].tls` | No | Ignored; Cloudflare manages TLS certificates |
| `spec.listeners[].allowedRoutes` | No | Ignored; all HTTPRoute/GRPCRoute accepted |
| `spec.addresses` | No | Ignored; tunnel CNAME set automatically in status |
| `spec.infrastructure` | No | Not implemented |

!!! info "TLS Termination"

    Cloudflare Tunnel terminates TLS at Cloudflare's edge network.
    The tunnel connector (cloudflared) establishes an outbound connection
    to Cloudflare. Gateway listener configuration for ports, protocols,
    and TLS settings has no effect on routing behavior.

## HTTPRoute

| Field | Supported | Notes |
|-------|-----------|-------|
| `spec.parentRefs` | Yes | References to Gateway |
| `spec.parentRefs[].name` | Yes | Gateway name |
| `spec.parentRefs[].namespace` | Yes | Gateway namespace |
| `spec.parentRefs[].sectionName` | Yes | Listener name (optional) |
| `spec.hostnames` | Yes | Wildcard `*` supported |
| `spec.rules` | Yes | Routing rules |
| `spec.rules[].matches` | Partial | Only path matching supported |
| `spec.rules[].matches[].path` | Yes | See [Path Matching](#path-matching) |
| `spec.rules[].matches[].headers` | No | Cloudflare limitation |
| `spec.rules[].matches[].queryParams` | No | Cloudflare limitation |
| `spec.rules[].matches[].method` | No | Cloudflare limitation |
| `spec.rules[].filters` | No | Not implemented |
| `spec.rules[].backendRefs` | Yes | Service backends only |
| `spec.rules[].backendRefs[].name` | Yes | Service name |
| `spec.rules[].backendRefs[].namespace` | Yes | Cross-namespace refs require ReferenceGrant |
| `spec.rules[].backendRefs[].port` | Yes | Service port |
| `spec.rules[].backendRefs[].weight` | Yes | Backend with highest weight selected |
| `spec.rules[].backendRefs[].filters` | No | Not implemented |
| `spec.rules[].timeouts` | No | Not implemented |

## GRPCRoute

| Field | Supported | Notes |
|-------|-----------|-------|
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
| `spec.rules[].matches[].headers` | No | Cloudflare limitation |
| `spec.rules[].filters` | No | Not implemented |
| `spec.rules[].backendRefs` | Yes | Service backends only |
| `spec.rules[].backendRefs[].name` | Yes | Service name |
| `spec.rules[].backendRefs[].namespace` | Yes | Cross-namespace refs require ReferenceGrant |
| `spec.rules[].backendRefs[].port` | Yes | Service port |
| `spec.rules[].backendRefs[].weight` | Yes | Backend with highest weight selected |
| `spec.rules[].backendRefs[].filters` | No | Not implemented |

## ReferenceGrant

ReferenceGrant enables cross-namespace backend references in HTTPRoute and
GRPCRoute resources.

| Field | Supported | Notes |
|-------|-----------|-------|
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

| Type | Supported | Example | Cloudflare Rule |
|------|-----------|---------|-----------------|
| `PathPrefix` | Yes | `/api` | `/api*` |
| `Exact` | Yes | `/health` | `/health` |
| `RegularExpression` | Partial | `^/v[0-9]+/` | Treated as prefix |

## gRPC Method Matching

gRPC methods are mapped to HTTP/2 paths using the standard format
`/package.Service/Method`.

| Match Type | Example | Cloudflare Rule |
|------------|---------|-----------------|
| Service only | `service: mypackage.MyService` | `/mypackage.MyService/*` |
| Service + Method | `service: mypackage.MyService, method: GetUser` | `/mypackage.MyService/GetUser` |
| No match | (empty) | Matches all gRPC traffic |

## Weight Selection Behavior

When multiple `backendRefs` are specified in a rule, the controller selects
the backend with the highest `weight` value:

- **Default weight**: `1` (per Gateway API specification)
- **Zero weight**: Backends with `weight: 0` are disabled
- **Equal weights**: First backend in list is selected

```yaml
backendRefs:
  - name: primary-service
    port: 80
    weight: 100  # Selected (highest weight)
  - name: secondary-service
    port: 80
    weight: 50
```

!!! warning "Not Traffic Splitting"

    This is NOT traffic splitting. The controller always sends 100% of
    traffic to the selected backend. Use weights to indicate preference,
    not traffic distribution.

!!! danger "No Fallback on Rejection"

    If the highest-weight backend is rejected (e.g., due to missing
    ReferenceGrant for cross-namespace reference), the controller does
    **not** fall back to the next backend. The entire rule is skipped,
    and the route status will show `ResolvedRefs=False`. This is per
    Gateway API specification — weights indicate preference, not failover order.

## Status Conditions

### Gateway Conditions

| Type | Status | Reason | Description |
|------|--------|--------|-------------|
| `Accepted` | `True` | `Accepted` | Gateway accepted by controller |
| `Programmed` | `True` | `Programmed` | Gateway configured in Cloudflare |

### Gateway Listener Conditions

| Type | Status | Reason | Description |
|------|--------|--------|-------------|
| `Accepted` | `True` | `Accepted` | Listener accepted |
| `Programmed` | `True` | `Programmed` | Listener programmed |
| `ResolvedRefs` | `True` | `ResolvedRefs` | References resolved |

### HTTPRoute/GRPCRoute Conditions

| Type | Status | Reason | Description |
|------|--------|--------|-------------|
| `Accepted` | `True` | `Accepted` | Route accepted and synced |
| `Accepted` | `False` | `NoMatchingParent` | Sync to Cloudflare failed |
| `ResolvedRefs` | `True` | `ResolvedRefs` | Backend references resolved |
| `ResolvedRefs` | `False` | `RefNotPermitted` | Cross-namespace reference denied |
