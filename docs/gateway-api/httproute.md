# HTTPRoute

HTTPRoute is the primary resource for configuring HTTP routing through
Cloudflare Tunnel.

## Basic Example

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

## Path-Based Routing

Route different paths to different services:

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

### Path Match Types

| Type | Example | Cloudflare Rule | Description |
|------|---------|-----------------|-------------|
| `PathPrefix` | `/api` | `/api*` | Matches paths starting with value |
| `Exact` | `/health` | `/health` | Matches exact path only |

## Multiple Hostnames

Route multiple hostnames to the same service:

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

!!! warning "Wildcard SSL"

    Multi-level subdomains like `*.staging.example.com` require
    [Advanced Certificate Manager](https://developers.cloudflare.com/ssl/edge-certificates/advanced-certificate-manager/)
    for SSL certificates.

## Backend Selection with Weights

When multiple backends are specified, the backend with the highest weight
is selected:

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

!!! note "Not Traffic Splitting"

    This is NOT traffic splitting. The controller always sends 100% of
    traffic to the selected backend. For traffic splitting, deploy a
    dedicated load balancer.

## Cross-Namespace Routing

Route to services in different namespaces using ReferenceGrant:

```yaml
---
# ReferenceGrant in target namespace
apiVersion: gateway.networking.k8s.io/v1beta1
kind: ReferenceGrant
metadata:
  name: allow-app-to-backend
  namespace: backend-namespace
spec:
  from:
    - group: gateway.networking.k8s.io
      kind: HTTPRoute
      namespace: app-namespace
  to:
    - group: ""
      kind: Service
---
# HTTPRoute in source namespace
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: cross-ns-route
  namespace: app-namespace
spec:
  parentRefs:
    - name: cloudflare-tunnel
      namespace: cloudflare-tunnel-system
  hostnames:
    - myapp.example.com
  rules:
    - backendRefs:
        - name: backend-service
          namespace: backend-namespace
          port: 8080
```

See [ReferenceGrant](referencegrant.md) for more details.

## External-DNS Integration

Add annotations for automatic DNS record creation:

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
  hostnames:
    - auto-dns.example.com
  rules:
    - backendRefs:
        - name: my-service
          port: 80
```

See [External-DNS Integration](../guides/external-dns.md) for setup.

## Listener Section Matching

Target specific Gateway listeners using `sectionName`:

```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: specific-listener
spec:
  parentRefs:
    - name: cloudflare-tunnel
      namespace: cloudflare-tunnel-system
      sectionName: http  # Match listener name
  hostnames:
    - app.example.com
  rules:
    - backendRefs:
        - name: my-service
          port: 80
```

## Checking Route Status

Verify that the route is accepted:

```bash
kubectl get httproute my-app --output jsonpath='{.status.parents[*].conditions}'
```

Expected output includes `"type":"Accepted","status":"True"`.

### Common Status Conditions

| Condition | Meaning |
|-----------|---------|
| `Accepted: True` | Route is active and synced to Cloudflare |
| `Accepted: False` | Route was rejected (check reason) |
| `ResolvedRefs: True` | All backend references resolved |
| `ResolvedRefs: False` | Backend reference failed (missing service or ReferenceGrant) |

## Troubleshooting

### Route Not Accepted

Check controller logs:

```bash
kubectl logs --selector app.kubernetes.io/name=cloudflare-tunnel-gateway-controller \
  --namespace cloudflare-tunnel-system
```

Common causes:

- Gateway not found (wrong name or namespace in parentRefs)
- Cloudflare API error (invalid credentials)
- Service not found

### Cross-Namespace Reference Denied

Ensure ReferenceGrant exists in the target namespace and allows your
route's namespace.

```bash
kubectl get referencegrant --namespace backend-namespace
```
