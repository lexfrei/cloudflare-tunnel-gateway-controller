# Gateway API Support

This document details the Gateway API implementation in the Cloudflare Tunnel Gateway Controller.

## Supported Resources

| Resource | API Version | Status |
|----------|-------------|--------|
| GatewayClass | `gateway.networking.k8s.io/v1` | ✅ Supported |
| Gateway | `gateway.networking.k8s.io/v1` | ✅ Supported |
| HTTPRoute | `gateway.networking.k8s.io/v1` | ✅ Supported |
| GRPCRoute | `gateway.networking.k8s.io/v1` | ✅ Supported |
| TCPRoute | `gateway.networking.k8s.io/v1alpha2` | ❌ Not supported |
| TLSRoute | `gateway.networking.k8s.io/v1alpha2` | ❌ Not supported |
| UDPRoute | `gateway.networking.k8s.io/v1alpha2` | ❌ Not supported |

## Feature Support Matrix

### GatewayClass

| Field | Supported | Notes |
|-------|-----------|-------|
| `spec.controllerName` | ✅ | Must match `--controller-name` flag |
| `spec.parametersRef` | ✅ | Via GatewayClassConfig CRD |
| `spec.description` | ✅ | Informational only |

### Gateway

The Gateway resource is accepted but most listener configuration is ignored because Cloudflare Tunnel handles TLS termination and port management at the edge.

| Field | Supported | Notes |
|-------|-----------|-------|
| `spec.gatewayClassName` | ✅ | Required, must match configured class |
| `spec.listeners` | ⚠️ | Accepted for compatibility, not used for routing |
| `spec.listeners[].name` | ✅ | Used for status reporting and sectionName matching |
| `spec.listeners[].port` | ❌ | Ignored; Cloudflare uses standard 443/80 |
| `spec.listeners[].protocol` | ❌ | Ignored; Cloudflare handles protocol negotiation |
| `spec.listeners[].hostname` | ❌ | Ignored; use HTTPRoute/GRPCRoute hostnames ([#43](https://github.com/lexfrei/cloudflare-tunnel-gateway-controller/issues/43)) |
| `spec.listeners[].tls` | ❌ | Ignored; Cloudflare manages TLS certificates |
| `spec.listeners[].allowedRoutes` | ❌ | Ignored; all HTTPRoute/GRPCRoute accepted ([#43](https://github.com/lexfrei/cloudflare-tunnel-gateway-controller/issues/43)) |
| `spec.addresses` | ❌ | Ignored; tunnel CNAME set automatically in status |
| `spec.infrastructure` | ❌ | Not implemented |

> **Important:** Cloudflare Tunnel terminates TLS at Cloudflare's edge network. The tunnel connector (cloudflared) establishes an outbound connection to Cloudflare. Gateway listener configuration for ports, protocols, and TLS settings has no effect on routing behavior.

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
| `spec.rules[].backendRefs[].weight` | ✅ | Backend with highest weight selected (default: 1); see [Weight Selection](#weight-selection-behavior) |
| `spec.rules[].backendRefs[].filters` | ❌ | Not implemented |
| `spec.rules[].timeouts` | ❌ | Not implemented |

### GRPCRoute

| Field | Supported | Notes |
|-------|-----------|-------|
| `spec.parentRefs` | ✅ | References to Gateway |
| `spec.parentRefs[].name` | ✅ | Gateway name |
| `spec.parentRefs[].namespace` | ✅ | Gateway namespace |
| `spec.parentRefs[].sectionName` | ✅ | Listener name (optional) |
| `spec.hostnames` | ✅ | Wildcard `*` supported |
| `spec.rules` | ✅ | Routing rules |
| `spec.rules[].matches` | ✅ | Service/method matching |
| `spec.rules[].matches[].method.service` | ✅ | gRPC service name |
| `spec.rules[].matches[].method.method` | ✅ | gRPC method name |
| `spec.rules[].matches[].method.type` | ✅ | Exact or RegularExpression |
| `spec.rules[].matches[].headers` | ❌ | Cloudflare limitation |
| `spec.rules[].filters` | ❌ | Not implemented |
| `spec.rules[].backendRefs` | ✅ | Service backends only |
| `spec.rules[].backendRefs[].name` | ✅ | Service name |
| `spec.rules[].backendRefs[].namespace` | ✅ | Service namespace |
| `spec.rules[].backendRefs[].port` | ✅ | Service port |
| `spec.rules[].backendRefs[].weight` | ✅ | Backend with highest weight selected (default: 1); see [Weight Selection](#weight-selection-behavior) |
| `spec.rules[].backendRefs[].filters` | ❌ | Not implemented |

### Weight Selection Behavior

When multiple `backendRefs` are specified in a rule, the controller selects the backend with the highest `weight` value. This behavior applies consistently across:

- **Within a single HTTPRoute rule** — highest weight backend is selected
- **Within a single GRPCRoute rule** — highest weight backend is selected
- **When HTTPRoute and GRPCRoute are merged** — all routes referencing the same Gateway are combined into a single Cloudflare Tunnel ingress configuration; each rule independently selects its highest-weight backend

**Default weight:** If `weight` is not specified, it defaults to `1` (per Gateway API specification).

**Zero weight:** Backends with `weight: 0` are disabled and will not receive traffic (per Gateway API specification). If all backends have zero weight, the rule is skipped entirely.

**Equal weights:** When multiple backends have the same highest weight, the first one in the list is selected for deterministic behavior.

```yaml
# Example: Backend with highest weight wins
backendRefs:
  - name: primary-service
    port: 80
    weight: 100  # ← Selected (highest weight)
  - name: secondary-service
    port: 80
    weight: 50
```

> **Note:** This is NOT traffic splitting. The controller always sends 100% of traffic to the selected backend. Use weights to indicate preference, not traffic distribution. For actual traffic splitting, deploy a dedicated load balancer (see [Traffic Splitting](#traffic-splitting-and-load-balancing)).

### gRPC Method Matching

gRPC methods are mapped to HTTP/2 paths using the standard format `/package.Service/Method`.

| Match Type | Example | Cloudflare Rule |
|------------|---------|-----------------|
| Service only | `service: mypackage.MyService` | `/mypackage.MyService/*` |
| Service + Method | `service: mypackage.MyService, method: GetUser` | `/mypackage.MyService/GetUser` |
| No match | (empty) | Matches all gRPC traffic |

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
| Single backend | Only highest-weight `backendRef` is used per rule (no traffic splitting) |
| Full sync | Any change triggers full config sync |
| No cross-cluster | Only in-cluster services supported |
| Service only | Only `Service` kind backends supported |

### Traffic Splitting and Load Balancing

**Design Decision:** This controller intentionally does not implement traffic splitting or weighted load balancing between multiple backends.

Cloudflare Tunnel ingress rules accept only a single service URL per rule. To support Gateway API's `backendRefs` with weights, the controller would need to:

1. Create and manage intermediate Kubernetes Services
2. Watch and synchronize Endpoints from all referenced services
3. Handle cross-namespace references and RBAC
4. Manage lifecycle of controller-created resources

This approach introduces significant complexity, potential for orphaned resources, and creates an opaque traffic path that is difficult for users to debug.

**Our approach:** Provide the tunnel a single, stable entrypoint. If you need load balancing:

- **Between pods of the same Deployment:** Use a standard Kubernetes Service (built-in round-robin)
- **Weighted traffic splitting or canary:** Deploy a dedicated load balancer (Traefik, Envoy, Nginx, HAProxy) and point the HTTPRoute to it

```yaml
# Example: Use Traefik for weighted routing
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: my-app
spec:
  parentRefs:
    - name: cloudflare-tunnel
  hostnames:
    - app.example.com
  rules:
    - backendRefs:
        - name: traefik  # Traefik handles weighted routing internally
          port: 80
```

This keeps the controller simple and predictable, and gives you full control over load balancing behavior.

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

### GRPCRoute Conditions

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

### Backend Selection with Weights

```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: weighted-backend
spec:
  parentRefs:
    - name: cloudflare-tunnel
      namespace: cloudflare-tunnel-system
  hostnames:
    - app.example.com
  rules:
    - backendRefs:
        # Backend with highest weight is selected
        - name: primary-service
          port: 80
          weight: 80
        - name: fallback-service
          port: 80
          weight: 20
        # primary-service is selected (weight 80 > 20)
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

### Basic gRPC Routing

```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: GRPCRoute
metadata:
  name: my-grpc-service
  namespace: default
spec:
  parentRefs:
    - name: cloudflare-tunnel
      namespace: cloudflare-tunnel-system
  hostnames:
    - grpc.example.com
  rules:
    - backendRefs:
        - name: grpc-server
          port: 50051
```

### gRPC Service Routing

```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: GRPCRoute
metadata:
  name: grpc-service-routes
spec:
  parentRefs:
    - name: cloudflare-tunnel
      namespace: cloudflare-tunnel-system
  hostnames:
    - api.example.com
  rules:
    # Route specific service to backend
    - matches:
        - method:
            service: mypackage.UserService
      backendRefs:
        - name: user-service
          port: 50051
    # Route another service
    - matches:
        - method:
            service: mypackage.OrderService
      backendRefs:
        - name: order-service
          port: 50051
```

### gRPC Method Routing

```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: GRPCRoute
metadata:
  name: grpc-method-routes
spec:
  parentRefs:
    - name: cloudflare-tunnel
      namespace: cloudflare-tunnel-system
  hostnames:
    - api.example.com
  rules:
    # Exact method match
    - matches:
        - method:
            service: mypackage.UserService
            method: GetUser
      backendRefs:
        - name: user-read-service
          port: 50051
    # All other methods on the service
    - matches:
        - method:
            service: mypackage.UserService
      backendRefs:
        - name: user-write-service
          port: 50051
```

## Troubleshooting

### Route Not Accepted

Check HTTPRoute or GRPCRoute status:

```bash
kubectl get httproute my-route -o jsonpath='{.status.parents[*].conditions}'
kubectl get grpcroute my-grpc-route -o jsonpath='{.status.parents[*].conditions}'
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
