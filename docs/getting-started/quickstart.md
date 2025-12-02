# Quick Start

This guide walks you through creating your first HTTPRoute to expose a
Kubernetes service through Cloudflare Tunnel.

## Prerequisites

Ensure you have completed:

- [Prerequisites](prerequisites.md) - Cloudflare Tunnel and API token created
- [Installation](installation.md) - Controller installed and running

## Deploy a Sample Application

First, deploy a simple application to expose:

```bash
kubectl create deployment nginx --image=nginx:latest
kubectl expose deployment nginx --port=80
```

## Create an HTTPRoute

Create an HTTPRoute to expose the nginx service through Cloudflare Tunnel:

```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: nginx
  namespace: default
spec:
  parentRefs:
    - name: cloudflare-tunnel
      namespace: cloudflare-tunnel-system
  hostnames:
    - nginx.example.com  # Replace with your domain
  rules:
    - backendRefs:
        - name: nginx
          port: 80
```

Apply the route:

```bash
kubectl apply --filename httproute.yaml
```

## Verify the Route

Check that the HTTPRoute is accepted:

```bash
kubectl get httproute nginx --output jsonpath='{.status.parents[*].conditions}'
```

Expected output includes `"type":"Accepted","status":"True"`.

## Configure DNS

The controller sets the Gateway address to `TUNNEL_ID.cfargotunnel.com`.
Create a CNAME record pointing your hostname to this address:

| Type | Name | Target |
|------|------|--------|
| CNAME | nginx | `YOUR_TUNNEL_ID.cfargotunnel.com` |

!!! tip "External-DNS"

    If you have [external-dns](https://external-dns.io/) configured with
    Gateway API source, DNS records are created automatically.
    See [External-DNS Integration](../guides/external-dns.md) for setup.

## Access Your Application

Once DNS propagates, access your application at `https://nginx.example.com`.

Cloudflare automatically provides:

- TLS certificate (via Universal SSL)
- DDoS protection
- Web Application Firewall (WAF)
- Caching (configurable)

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
    - matches:
        - path:
            type: PathPrefix
            value: /v1
      backendRefs:
        - name: api-v1
          port: 8080
    - matches:
        - path:
            type: PathPrefix
            value: /v2
      backendRefs:
        - name: api-v2
          port: 8080
```

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

## Troubleshooting

### Route Not Accepted

Check controller logs:

```bash
kubectl logs --selector app.kubernetes.io/name=cloudflare-tunnel-gateway-controller \
  --namespace cloudflare-tunnel-system
```

Common issues:

- Gateway not found (wrong namespace or name in parentRefs)
- Cloudflare API error (invalid credentials or permissions)
- Service not found (wrong service name or namespace)

### SSL Certificate Errors

Cloudflare's free Universal SSL covers:

- `example.com`
- `*.example.com`

For multi-level subdomains like `app.dev.example.com`, you need
[Advanced Certificate Manager](https://developers.cloudflare.com/ssl/edge-certificates/advanced-certificate-manager/).

## Next Steps

- Learn about [Configuration](../configuration/index.md) options
- Explore [Gateway API](../gateway-api/index.md) features
- Set up [Monitoring](../guides/monitoring.md) for production
