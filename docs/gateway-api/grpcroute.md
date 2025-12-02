# GRPCRoute

GRPCRoute enables routing gRPC traffic through Cloudflare Tunnel with
service and method-level matching.

## Basic Example

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

## Service Routing

Route by gRPC service name:

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
    # Route UserService to user backend
    - matches:
        - method:
            service: mypackage.UserService
      backendRefs:
        - name: user-service
          port: 50051
    # Route OrderService to order backend
    - matches:
        - method:
            service: mypackage.OrderService
      backendRefs:
        - name: order-service
          port: 50051
```

## Method Routing

Route specific gRPC methods to different backends:

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
    # Exact method match - read operations
    - matches:
        - method:
            service: mypackage.UserService
            method: GetUser
      backendRefs:
        - name: user-read-service
          port: 50051
    # All other methods on the service - write operations
    - matches:
        - method:
            service: mypackage.UserService
      backendRefs:
        - name: user-write-service
          port: 50051
```

## Method Matching

gRPC methods are mapped to HTTP/2 paths using the standard format
`/package.Service/Method`.

| Match Type | Example | Cloudflare Rule |
|------------|---------|-----------------|
| Service only | `service: mypackage.MyService` | `/mypackage.MyService/*` |
| Service + Method | `service: mypackage.MyService, method: GetUser` | `/mypackage.MyService/GetUser` |
| No match | (empty) | Matches all gRPC traffic |

### Match Type Field

The `type` field specifies how to match:

| Type | Description |
|------|-------------|
| `Exact` | Exact string match (default) |
| `RegularExpression` | Regular expression match |

```yaml
matches:
  - method:
      service: mypackage.UserService
      method: Get.*
      type: RegularExpression
```

## Cross-Namespace Routing

GRPCRoute also supports cross-namespace backend references with ReferenceGrant:

```yaml
---
# ReferenceGrant in target namespace
apiVersion: gateway.networking.k8s.io/v1beta1
kind: ReferenceGrant
metadata:
  name: allow-grpc-routes
  namespace: grpc-services
spec:
  from:
    - group: gateway.networking.k8s.io
      kind: GRPCRoute
      namespace: app-namespace
  to:
    - group: ""
      kind: Service
---
# GRPCRoute in source namespace
apiVersion: gateway.networking.k8s.io/v1
kind: GRPCRoute
metadata:
  name: cross-ns-grpc-route
  namespace: app-namespace
spec:
  parentRefs:
    - name: cloudflare-tunnel
      namespace: cloudflare-tunnel-system
  hostnames:
    - grpc.example.com
  rules:
    - matches:
        - method:
            service: mypackage.UserService
      backendRefs:
        - name: user-grpc-service
          namespace: grpc-services
          port: 50051
```

## Backend Selection with Weights

When multiple backends are specified, the backend with the highest weight
is selected:

```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: GRPCRoute
metadata:
  name: weighted-grpc
spec:
  parentRefs:
    - name: cloudflare-tunnel
      namespace: cloudflare-tunnel-system
  hostnames:
    - grpc.example.com
  rules:
    - matches:
        - method:
            service: mypackage.UserService
      backendRefs:
        - name: primary-grpc
          port: 50051
          weight: 100  # Selected
        - name: fallback-grpc
          port: 50051
          weight: 0    # Disabled
```

## Multiple Hostnames

Route multiple gRPC endpoints:

```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: GRPCRoute
metadata:
  name: multi-host-grpc
spec:
  parentRefs:
    - name: cloudflare-tunnel
      namespace: cloudflare-tunnel-system
  hostnames:
    - grpc.example.com
    - api.example.com
    - "*.grpc.example.com"
  rules:
    - backendRefs:
        - name: grpc-server
          port: 50051
```

## Checking Route Status

Verify that the route is accepted:

```bash
kubectl get grpcroute my-grpc-route --output jsonpath='{.status.parents[*].conditions}'
```

### Status Conditions

| Condition | Meaning |
|-----------|---------|
| `Accepted: True` | Route is active and synced to Cloudflare |
| `Accepted: False` | Route was rejected (check reason) |
| `ResolvedRefs: True` | All backend references resolved |
| `ResolvedRefs: False` | Backend reference failed |

## Limitations

### Not Supported

| Feature | Reason |
|---------|--------|
| Header matching | Cloudflare Tunnel limitation |
| Filters | Not implemented |
| Backend filters | Not implemented |

### Traffic Splitting

The controller does NOT implement traffic splitting. When multiple backends
have weights, the highest weight backend receives 100% of traffic.

For actual traffic splitting, deploy a gRPC-aware load balancer (e.g., Envoy)
and point the GRPCRoute to it.

## Troubleshooting

### Route Not Accepted

Check controller logs:

```bash
kubectl logs --selector app.kubernetes.io/name=cloudflare-tunnel-gateway-controller \
  --namespace cloudflare-tunnel-system
```

Common causes:

- Gateway not found
- Cloudflare API error
- Invalid method specification

### gRPC Connection Issues

1. Verify cloudflared is running and connected:

```bash
kubectl logs --selector app=cloudflared \
  --namespace cloudflare-tunnel-system
```

2. Check that the backend service supports gRPC (HTTP/2):

```bash
kubectl describe service grpc-server
```

3. Verify the backend pod is listening on the correct port:

```bash
kubectl port-forward svc/grpc-server 50051:50051
grpcurl --plaintext localhost:50051 list
```
