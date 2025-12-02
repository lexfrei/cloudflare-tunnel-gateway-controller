# Cross-Namespace Routing

This guide covers routing traffic to services in different namespaces using
ReferenceGrant.

## Overview

By default, Gateway API only allows Routes to reference Services in the same
namespace. Cross-namespace references require explicit permission via
ReferenceGrant.

## Use Cases

- **Shared services**: Route to services in a `shared-services` namespace
- **Multi-team deployments**: Teams own namespaces but share ingress
- **Service mesh patterns**: Central gateway routing to distributed services

## Basic Setup

### 1. Create ReferenceGrant in Target Namespace

The ReferenceGrant must be created in the namespace where the Service exists:

```yaml
apiVersion: gateway.networking.k8s.io/v1beta1
kind: ReferenceGrant
metadata:
  name: allow-frontend-routes
  namespace: backend  # Where the Service is
spec:
  from:
    - group: gateway.networking.k8s.io
      kind: HTTPRoute
      namespace: frontend  # Where the Route is
  to:
    - group: ""  # Core API group
      kind: Service
```

### 2. Create HTTPRoute with Cross-Namespace Reference

```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: api-route
  namespace: frontend  # Source namespace
spec:
  parentRefs:
    - name: cloudflare-tunnel
      namespace: cloudflare-tunnel-system
  hostnames:
    - api.example.com
  rules:
    - backendRefs:
        - name: api-service
          namespace: backend  # Target namespace
          port: 8080
```

## Common Patterns

### Shared Services Namespace

Allow all namespaces to route to shared services:

```yaml
apiVersion: gateway.networking.k8s.io/v1beta1
kind: ReferenceGrant
metadata:
  name: allow-all-to-shared
  namespace: shared-services
spec:
  from:
    # Allow from app namespaces
    - group: gateway.networking.k8s.io
      kind: HTTPRoute
      namespace: app-team-a
    - group: gateway.networking.k8s.io
      kind: HTTPRoute
      namespace: app-team-b
    - group: gateway.networking.k8s.io
      kind: HTTPRoute
      namespace: app-team-c
  to:
    - group: ""
      kind: Service
```

### Specific Service Access

Restrict access to specific services:

```yaml
apiVersion: gateway.networking.k8s.io/v1beta1
kind: ReferenceGrant
metadata:
  name: allow-public-api-only
  namespace: backend
spec:
  from:
    - group: gateway.networking.k8s.io
      kind: HTTPRoute
      namespace: frontend
  to:
    - group: ""
      kind: Service
      name: public-api  # Only this service
```

### GRPCRoute Cross-Namespace

```yaml
---
apiVersion: gateway.networking.k8s.io/v1beta1
kind: ReferenceGrant
metadata:
  name: allow-grpc-routes
  namespace: grpc-services
spec:
  from:
    - group: gateway.networking.k8s.io
      kind: GRPCRoute
      namespace: grpc-clients
  to:
    - group: ""
      kind: Service
---
apiVersion: gateway.networking.k8s.io/v1
kind: GRPCRoute
metadata:
  name: user-service
  namespace: grpc-clients
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
        - name: user-grpc
          namespace: grpc-services
          port: 50051
```

## Multi-Service Routing

Route to multiple services in different namespaces:

```yaml
---
# ReferenceGrant for team-a services
apiVersion: gateway.networking.k8s.io/v1beta1
kind: ReferenceGrant
metadata:
  name: allow-gateway-routes
  namespace: team-a
spec:
  from:
    - group: gateway.networking.k8s.io
      kind: HTTPRoute
      namespace: ingress
  to:
    - group: ""
      kind: Service
---
# ReferenceGrant for team-b services
apiVersion: gateway.networking.k8s.io/v1beta1
kind: ReferenceGrant
metadata:
  name: allow-gateway-routes
  namespace: team-b
spec:
  from:
    - group: gateway.networking.k8s.io
      kind: HTTPRoute
      namespace: ingress
  to:
    - group: ""
      kind: Service
---
# Single HTTPRoute routing to multiple namespaces
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: multi-team-route
  namespace: ingress
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
            value: /team-a
      backendRefs:
        - name: team-a-service
          namespace: team-a
          port: 8080
    - matches:
        - path:
            type: PathPrefix
            value: /team-b
      backendRefs:
        - name: team-b-service
          namespace: team-b
          port: 8080
```

## Verification

### Check ReferenceGrant Status

```bash
kubectl get referencegrant --all-namespaces
```

### Check Route Status

```bash
kubectl get httproute api-route --namespace frontend \
  --output jsonpath='{.status.parents[*].conditions}'
```

Look for `ResolvedRefs: True`. If `False` with reason `RefNotPermitted`,
the ReferenceGrant is missing or misconfigured.

### Debug Missing Grants

```bash
# Check controller logs for reference errors
kubectl logs --selector app.kubernetes.io/name=cloudflare-tunnel-gateway-controller \
  --namespace cloudflare-tunnel-system | grep -i reference
```

## Security Considerations

!!! warning "Principle of Least Privilege"

    - Create ReferenceGrants with minimum necessary permissions
    - Use `to[].name` to restrict to specific Services when possible
    - Regularly audit ReferenceGrants in shared namespaces
    - Consider namespace isolation for sensitive services

### Audit Script

```bash
#!/bin/bash
# List all cross-namespace permissions
for ns in $(kubectl get ns -o jsonpath='{.items[*].metadata.name}'); do
  grants=$(kubectl get referencegrant -n "$ns" -o name 2>/dev/null)
  if [ -n "$grants" ]; then
    echo "Namespace: $ns"
    kubectl get referencegrant -n "$ns" -o yaml | grep -A5 'from:'
    echo "---"
  fi
done
```

## Troubleshooting

### RefNotPermitted Error

1. Verify ReferenceGrant is in the **target** namespace (where Service is)
2. Check `from[].namespace` matches the Route's namespace
3. Check `from[].kind` is correct (HTTPRoute or GRPCRoute)
4. Check `from[].group` is `gateway.networking.k8s.io`
5. Check `to[].group` is `""` (empty string for Services)

### Service Not Found

Even with ReferenceGrant, the target Service must exist:

```bash
kubectl get service api-service --namespace backend
```

### Network Policy Blocking Traffic

If using NetworkPolicy, ensure traffic is allowed between namespaces:

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: allow-from-cloudflared
  namespace: backend
spec:
  podSelector: {}
  ingress:
    - from:
        - namespaceSelector:
            matchLabels:
              kubernetes.io/metadata.name: cloudflare-tunnel-system
```
