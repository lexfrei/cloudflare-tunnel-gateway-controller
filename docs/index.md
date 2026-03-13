# Cloudflare Tunnel Gateway Controller

Kubernetes controller implementing Gateway API for Cloudflare Tunnel.

Enables routing traffic through Cloudflare Tunnel using standard Gateway API
resources (Gateway, HTTPRoute, GRPCRoute).

## Features

- Standard Gateway API implementation (GatewayClass, Gateway, HTTPRoute, GRPCRoute)
- Hot reload of tunnel configuration (no cloudflared restart required)
- Optional cloudflared lifecycle management via Helm SDK
- Leader election for high availability deployments
- Multi-arch container images (amd64, arm64)
- Signed container images with cosign

### L7 Proxy

An in-process L7 reverse proxy embedded inside cloudflared (via the
`OverrideProxy` hook) provides full Gateway API HTTPRoute feature support:

- **Header-based routing** -- match requests by HTTP header values
- **Query parameter matching** -- route based on URL query parameters
- **HTTP method matching** -- differentiate GET, POST, PUT, and other methods
- **Regex path matching** -- match paths using regular expressions
- **Request/response header modification** -- add, set, or remove headers via filters
- **Request redirects** -- configure HTTP redirects declaratively
- **URL rewriting** -- rewrite hostname and/or path before forwarding
- **Request mirroring** -- mirror traffic to a secondary backend
- **Weighted traffic splitting** -- distribute traffic across backends by percentage
- **Per-route timeouts** -- configure request timeouts per routing rule

See the [L7 Proxy Guide](guides/l7-proxy.md) for setup and examples.

!!! warning

    The controller assumes **exclusive ownership** of the tunnel configuration.
    It will remove any ingress rules not managed by HTTPRoute/GRPCRoute resources.
    Do not use a tunnel that has manually configured routes or is shared with
    other systems.

## Quick Start

```bash
# 1. Install Gateway API CRDs
kubectl apply --filename https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.5.0/standard-install.yaml

# 2. Install the controller
helm install cloudflare-tunnel-gateway-controller \
  oci://ghcr.io/lexfrei/charts/cloudflare-tunnel-gateway-controller \
  --namespace cloudflare-tunnel-system \
  --create-namespace \
  --set config.tunnelID=YOUR_TUNNEL_ID \
  --set config.apiToken=YOUR_API_TOKEN

# 3. Create HTTPRoute to expose your service
kubectl apply --filename - <<EOF
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
        - name: my-service
          port: 80
EOF
```

See [Getting Started](getting-started/index.md) for detailed setup instructions.

## Documentation Sections

| Section | Description |
|---------|-------------|
| [Getting Started](getting-started/index.md) | Prerequisites, installation, and quick start guide |
| [Configuration](configuration/index.md) | Controller options, Helm values, GatewayClassConfig |
| [Gateway API](gateway-api/index.md) | Supported resources, examples, and limitations |
| [Guides](guides/index.md) | Integration guides for AWG, external-dns, monitoring |
| [Operations](operations/index.md) | Troubleshooting, metrics, and manual installation |
| [Development](development/index.md) | Architecture, contributing, and testing |
| [Reference](reference/index.md) | Helm chart, CRD reference, security policy |

## Project Links

- [GitHub Repository](https://github.com/lexfrei/cloudflare-tunnel-gateway-controller)
- [Issues](https://github.com/lexfrei/cloudflare-tunnel-gateway-controller/issues)
- [Releases](https://github.com/lexfrei/cloudflare-tunnel-gateway-controller/releases)

## License

BSD 3-Clause License - see [LICENSE](https://github.com/lexfrei/cloudflare-tunnel-gateway-controller/blob/master/LICENSE)
for details.
