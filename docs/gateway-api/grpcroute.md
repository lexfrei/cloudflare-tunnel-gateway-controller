# GRPCRoute

GRPCRoute enables routing gRPC traffic through Cloudflare Tunnel with service and method-level matching. It is served by the in-process L7 proxy: gRPC requests are HTTP/2 POSTs to `/{service}/{method}`, which the proxy matches with its path matcher; the upstream hop to the backend is forced to h2c (cleartext HTTP/2).

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

gRPC methods are mapped to HTTP/2 paths using the standard format `/package.Service/Method`.

| Match Type | Example | Proxy path rule |
|------------|---------|-----------------|
| Service only | `service: mypackage.MyService` | prefix `/mypackage.MyService/` |
| Service + Method | `service: mypackage.MyService, method: GetUser` | exact `/mypackage.MyService/GetUser` |
| Method only | `method: GetUser` | regex `/[^/]+/GetUser` (any service) |
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

When multiple backends are specified, traffic is split across them in proportion to their weights (a backend with weight 0 receives none):

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
          weight: 80   # ~80% of traffic
        - name: fallback-grpc
          port: 50051
          weight: 20   # ~20% of traffic
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

### Supported and not supported

| Feature | Status |
|---------|--------|
| Service / method matching (Exact, RegularExpression) | Supported |
| Header matching (Exact, RegularExpression) | Supported |
| Filters | Not implemented — logged and skipped |
| Backend filters | Not implemented |
| BackendTLSPolicy / Gateway `clientCertificateRef` | Not applied — see Backend TLS below |

### Backend TLS

The upstream hop to a gRPC backend is always cleartext h2c (HTTP/2 without TLS). Unlike HTTPRoute backends, a `BackendTLSPolicy` targeting a gRPC backend Service and the Gateway's `spec.tls.backend.clientCertificateRef` are **not** applied to gRPC traffic in this revision — they are silently ignored, and no WARN is logged. If your gRPC backend requires TLS, terminate it in front of the Service (for example with a sidecar or an in-cluster gRPC-aware proxy) and point the GRPCRoute at the cleartext side.

### Traffic Splitting

Weighted traffic splitting is supported: when a rule lists multiple backends with weights, the in-process L7 proxy distributes requests across them in proportion to their weights at request time (the same weighted selection used for HTTPRoute). A backend with weight `0` receives no traffic, and a rule whose backends all have weight `0` sends no traffic.

Note that gRPC connections are long-lived (HTTP/2 multiplexes many calls over one connection), so the split applies per request the proxy routes, not per TCP connection — a single client holding one connection still has its individual calls distributed by weight.

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

1. Verify the proxy pods (which embed the cloudflared tunnel in-process) are running and connected:

```bash
kubectl logs --selector app.kubernetes.io/component=proxy \
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
