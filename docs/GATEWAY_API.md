# Gateway API Support

This document details the Gateway API implementation in the Cloudflare Tunnel Gateway Controller.

## Supported Resources

| Resource | API Version | Status |
|----------|-------------|--------|
| GatewayClass | `gateway.networking.k8s.io/v1` | ✅ Supported |
| Gateway | `gateway.networking.k8s.io/v1` | ✅ Supported |
| HTTPRoute | `gateway.networking.k8s.io/v1` | ✅ Supported |
| GRPCRoute | `gateway.networking.k8s.io/v1` | ❌ Not supported |
| TCPRoute | `gateway.networking.k8s.io/v1alpha2` | ❌ Not supported |
| TLSRoute | `gateway.networking.k8s.io/v1alpha2` | ❌ Not supported |
| UDPRoute | `gateway.networking.k8s.io/v1alpha2` | ❌ Not supported |

## Feature Support Matrix

### GatewayClass

| Field | Supported | Notes |
|-------|-----------|-------|
| `spec.controllerName` | ✅ | Must match `--controller-name` flag |
| `spec.parametersRef` | ❌ | Not implemented |
| `spec.description` | ✅ | Informational only |

### Gateway

| Field | Supported | Notes |
|-------|-----------|-------|
| `spec.gatewayClassName` | ✅ | Required, must match configured class |
| `spec.listeners` | ✅ | Informational, used for status |
| `spec.listeners[].name` | ✅ | Used in HTTPRoute parentRef.sectionName |
| `spec.listeners[].port` | ✅ | Informational only (Cloudflare handles ports) |
| `spec.listeners[].protocol` | ✅ | HTTP and HTTPS supported |
| `spec.listeners[].hostname` | ❌ | Use HTTPRoute hostnames instead |
| `spec.listeners[].tls` | ❌ | Cloudflare manages TLS termination |
| `spec.listeners[].allowedRoutes` | ✅ | Namespace filtering supported |
| `spec.addresses` | ❌ | Cloudflare assigns addresses |

### HTTPRoute

| Field | Supported | Notes |
|-------|-----------|-------|
| `spec.parentRefs` | ✅ | References to Gateway |
| `spec.parentRefs[].name` | ✅ | Gateway name |
| `spec.parentRefs[].namespace` | ✅ | Gateway namespace |
| `spec.parentRefs[].sectionName` | ✅ | Listener name (optional) |
| `spec.hostnames` | ✅ | Wildcard `*` supported |
| `spec.rules` | ✅ | Routing rules |
| `spec.rules[].matches` | ⚠️ | Only path matching supported |
| `spec.rules[].matches[].path` | ✅ | See path matching below |
| `spec.rules[].matches[].headers` | ❌ | Cloudflare limitation |
| `spec.rules[].matches[].queryParams` | ❌ | Cloudflare limitation |
| `spec.rules[].matches[].method` | ❌ | Cloudflare limitation |
| `spec.rules[].filters` | ❌ | Not implemented |
| `spec.rules[].backendRefs` | ✅ | Service backends only |
| `spec.rules[].backendRefs[].name` | ✅ | Service name |
| `spec.rules[].backendRefs[].namespace` | ✅ | Service namespace |
| `spec.rules[].backendRefs[].port` | ✅ | Service port |
| `spec.rules[].backendRefs[].weight` | ❌ | First backend used |
| `spec.rules[].backendRefs[].filters` | ❌ | Not implemented |
| `spec.rules[].timeouts` | ❌ | Not implemented |

### Path Matching

| Type | Supported | Example | Cloudflare Rule |
|------|-----------|---------|-----------------|
| `PathPrefix` | ✅ | `/api` | `/api*` |
| `Exact` | ✅ | `/health` | `/health` |
| `RegularExpression` | ⚠️ | `^/v[0-9]+/` | Treated as prefix |

## Limitations

### Cloudflare Tunnel Constraints

The following Gateway API features are not supported due to Cloudflare Tunnel architecture:

| Feature | Reason |
|---------|--------|
| Header matching | Tunnel ingress rules don't support header conditions |
| Query parameter matching | Tunnel ingress rules don't support query conditions |
| Method matching | Tunnel ingress rules don't support method conditions |
| Request header modification | Tunnel doesn't modify requests |
| Response header modification | Tunnel doesn't modify responses |
| Request redirect | Tunnel doesn't support redirects |
| URL rewrite | Tunnel doesn't support rewrites |
| Request mirroring | Tunnel doesn't support mirroring |
| Traffic splitting | Tunnel sends to single backend |

### Controller Limitations

| Limitation | Description |
|------------|-------------|
| Single backend | Only first `backendRef` is used per rule |
| Full sync | Any change triggers full config sync |
| No cross-cluster | Only in-cluster services supported |
| Service only | Only `Service` kind backends supported |

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

### HTTPRoute Conditions

| Type | Status | Reason | Description |
|------|--------|--------|-------------|
| `Accepted` | `True` | `Accepted` | Route accepted and synced |
| `Accepted` | `False` | `NoMatchingParent` | Sync to Cloudflare failed |
| `ResolvedRefs` | `True` | `ResolvedRefs` | Backend references resolved |

## Examples

### Basic HTTP Routing

```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: my-app
  namespace: default
spec:
  parentRefs:
    - name: cloudflare-tunnel
      namespace: cloudflare-tunnel-system
  hostnames:
    - app.example.com
  rules:
    - matches:
        - path:
            type: PathPrefix
            value: /
      backendRefs:
        - name: my-service
          port: 80
```

### Path-Based Routing

```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: api-routes
spec:
  parentRefs:
    - name: cloudflare-tunnel
      namespace: cloudflare-tunnel-system
  hostnames:
    - api.example.com
  rules:
    # Exact match takes priority
    - matches:
        - path:
            type: Exact
            value: /health
      backendRefs:
        - name: health-service
          port: 8080
    # Prefix match for API v1
    - matches:
        - path:
            type: PathPrefix
            value: /v1
      backendRefs:
        - name: api-v1
          port: 8080
    # Prefix match for API v2
    - matches:
        - path:
            type: PathPrefix
            value: /v2
      backendRefs:
        - name: api-v2
          port: 8080
```

### Multiple Hostnames

```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: multi-host
spec:
  parentRefs:
    - name: cloudflare-tunnel
      namespace: cloudflare-tunnel-system
  hostnames:
    - app.example.com
    - www.example.com
    - "*.staging.example.com"
  rules:
    - backendRefs:
        - name: web-app
          port: 80
```

### Cross-Namespace Route

```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: cross-ns-route
  namespace: app-namespace
spec:
  parentRefs:
    - name: cloudflare-tunnel
      namespace: cloudflare-tunnel-system
      sectionName: http  # Match listener name
  hostnames:
    - myapp.example.com
  rules:
    - backendRefs:
        - name: backend-service
          namespace: backend-namespace  # Cross-namespace reference
          port: 8080
```

### External-DNS Integration

```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: with-dns
  annotations:
    external-dns.alpha.kubernetes.io/cloudflare-proxied: "true"
spec:
  parentRefs:
    - name: cloudflare-tunnel
      namespace: cloudflare-tunnel-system
      sectionName: http
  hostnames:
    - auto-dns.example.com
  rules:
    - backendRefs:
        - name: my-service
          port: 80
```

## Troubleshooting

### Route Not Accepted

Check HTTPRoute status:

```bash
kubectl get httproute my-route -o jsonpath='{.status.parents[*].conditions}'
```

Common issues:

- Gateway not found or wrong namespace
- GatewayClass mismatch
- Cloudflare API error (check controller logs)

### Traffic Not Reaching Backend

1. Verify HTTPRoute status shows `Accepted: True`
2. Check Cloudflare dashboard for tunnel configuration
3. Verify backend service exists and has endpoints
4. Check cloudflared logs for connection issues

### Multiple Routes Conflict

Routes are processed in order:

1. Exact path matches first
2. Longer prefix paths before shorter
3. Alphabetically by hostname

If routes conflict, the first match wins.
