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

!!! warning

    The controller assumes **exclusive ownership** of the tunnel configuration.
    It will remove any ingress rules not managed by HTTPRoute/GRPCRoute resources.
    Do not use a tunnel that has manually configured routes or is shared with
    other systems.

## Quick Start

```bash
# 1. Install Gateway API CRDs
kubectl apply --filename https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.4.0/standard-install.yaml

# 2. Install the controller
helm install cloudflare-tunnel-gateway-controller \
  oci://ghcr.io/lexfrei/cloudflare-tunnel-gateway-controller/chart \
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
