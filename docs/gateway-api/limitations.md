# Limitations

This document describes the known limitations of the Cloudflare Tunnel Gateway
Controller and provides workarounds where applicable.

## Cloudflare Tunnel Constraints

The following Gateway API features are not supported due to Cloudflare Tunnel
architecture:

| Feature | Reason |
|---------|--------|
| Header matching | Tunnel ingress rules don't support header conditions |
| Query parameter matching | Tunnel ingress rules don't support query conditions |
| Method matching | Tunnel ingress rules don't support HTTP method conditions |
| Request header modification | Tunnel doesn't modify requests |
| Response header modification | Tunnel doesn't modify responses |
| Request redirect | Tunnel doesn't support redirects |
| URL rewrite | Tunnel doesn't support rewrites |
| Request mirroring | Tunnel doesn't support mirroring |
| Traffic splitting | Tunnel sends to single backend |

## Controller Limitations

| Limitation | Description |
|------------|-------------|
| Single backend | Only highest-weight `backendRef` is used per rule |
| Full sync | Any change triggers full config sync |
| No cross-cluster | Only in-cluster services supported |
| Service only | Only `Service` kind backends supported |

## Traffic Splitting and Load Balancing

**Design Decision:** This controller intentionally does not implement traffic
splitting or weighted load balancing between multiple backends.

### Why No Traffic Splitting?

Cloudflare Tunnel ingress rules accept only a single service URL per rule.
To support Gateway API's `backendRefs` with weights, the controller would
need to:

1. Create and manage intermediate Kubernetes Services
2. Watch and synchronize Endpoints from all referenced services
3. Handle cross-namespace references and RBAC
4. Manage lifecycle of controller-created resources

This approach introduces significant complexity, potential for orphaned
resources, and creates an opaque traffic path that is difficult for users
to debug.

### Our Approach

Provide the tunnel a single, stable entrypoint. The controller selects the
backend with the highest weight and sends 100% of traffic to it.

### Workarounds

**Between pods of the same Deployment:**

Use a standard Kubernetes Service (built-in round-robin load balancing):

```yaml
apiVersion: v1
kind: Service
metadata:
  name: my-app
spec:
  selector:
    app: my-app
  ports:
    - port: 80
      targetPort: 8080
```

**Weighted traffic splitting or canary:**

Deploy a dedicated load balancer (Traefik, Envoy, Nginx, HAProxy) and point
the HTTPRoute to it:

```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: my-app
spec:
  parentRefs:
    - name: cloudflare-tunnel
      namespace: cloudflare-tunnel-system
  hostnames:
    - app.example.com
  rules:
    - backendRefs:
        - name: traefik  # Traefik handles weighted routing internally
          port: 80
```

This keeps the controller simple and predictable, and gives you full control
over load balancing behavior.

## SSL Certificate Limitations

Cloudflare's free [Universal SSL](https://developers.cloudflare.com/ssl/edge-certificates/universal-ssl/limitations/)
certificates only cover root and first-level subdomains:

| Hostname | Covered | Notes |
|----------|---------|-------|
| `example.com` | Yes | Root domain |
| `*.example.com` | Yes | First-level wildcard |
| `app.example.com` | Yes | First-level subdomain |
| `app.dev.example.com` | No | Multi-level subdomain |
| `*.dev.example.com` | No | Multi-level wildcard |

### Workaround

For multi-level subdomains, you need:

- [Advanced Certificate Manager](https://developers.cloudflare.com/ssl/edge-certificates/advanced-certificate-manager/)
  ($10/month)
- Business or Enterprise plan

## Gateway Listener Configuration

Gateway listeners are accepted for compatibility but most fields are ignored:

| Field | Status | Notes |
|-------|--------|-------|
| `port` | Ignored | Cloudflare uses 443/80 |
| `protocol` | Ignored | Cloudflare handles protocols |
| `hostname` | Ignored | Use HTTPRoute/GRPCRoute hostnames |
| `tls` | Ignored | Cloudflare manages TLS |
| `allowedRoutes` | Ignored | All routes accepted |

This is because Cloudflare Tunnel terminates TLS at Cloudflare's edge, not
in the cluster.

## Route Types Not Supported

| Route Type | Status | Reason |
|------------|--------|--------|
| TCPRoute | Not supported | Cloudflare Tunnel is HTTP-focused |
| TLSRoute | Not supported | TLS is terminated at edge |
| UDPRoute | Not supported | No UDP support in tunnels |

### Workaround for TCP/UDP

For non-HTTP traffic:

1. Use [Cloudflare Spectrum](https://www.cloudflare.com/products/cloudflare-spectrum/)
   (separate product)
2. Use a different ingress solution (LoadBalancer, NodePort)

## Full Sync Behavior

Any change to an HTTPRoute or GRPCRoute triggers a full configuration sync
to Cloudflare Tunnel. This means:

1. Controller lists all HTTPRoutes and GRPCRoutes
2. Filters by GatewayClass
3. Rebuilds entire ingress configuration
4. Pushes to Cloudflare API

### Implications

- More API calls than incremental updates
- Brief delay when many routes are present
- All routes are re-evaluated on any change

### Mitigation

For large deployments:

- Batch route changes when possible
- Monitor Cloudflare API rate limits
- Consider separating high-churn routes to different tunnels

## Route Conflict Resolution

Routes are processed in order:

1. Exact path matches first
2. Longer prefix paths before shorter
3. Alphabetically by hostname

If routes conflict, the first match wins. The controller does not merge
rules from different routes.

### Example Conflict

```yaml
# Route A
rules:
  - matches:
      - path:
          type: PathPrefix
          value: /api
    backendRefs:
      - name: api-v1

# Route B
rules:
  - matches:
      - path:
          type: PathPrefix
          value: /api/v2
    backendRefs:
      - name: api-v2
```

Route B's `/api/v2` matches first (longer path), then Route A's `/api`
matches remaining traffic.

## No Multi-Cluster Support

The controller only routes to Services within the same Kubernetes cluster.
Cross-cluster routing is not supported.

### Workaround

For multi-cluster scenarios:

1. Deploy the controller in each cluster with separate tunnels
2. Use Cloudflare Load Balancing to distribute traffic between tunnels
3. Consider a service mesh with cross-cluster capabilities

## Metrics and Observability

The controller provides Prometheus metrics for monitoring, but:

- No distributed tracing integration
- No access logs (use Cloudflare dashboard)
- Limited visibility into Cloudflare API operations

See [Metrics & Alerting](../operations/metrics.md) for available metrics.
