# Cloudflare Tunnel Gateway Controller

[![Go Version](https://img.shields.io/github/go-mod/go-version/lexfrei/cloudflare-tunnel-gateway-controller)](https://go.dev/)
[![License](https://img.shields.io/github/license/lexfrei/cloudflare-tunnel-gateway-controller)](LICENSE)
[![Release](https://img.shields.io/github/v/release/lexfrei/cloudflare-tunnel-gateway-controller)](https://github.com/lexfrei/cloudflare-tunnel-gateway-controller/releases)
[![CI](https://github.com/lexfrei/cloudflare-tunnel-gateway-controller/actions/workflows/pr.yaml/badge.svg)](https://github.com/lexfrei/cloudflare-tunnel-gateway-controller/actions/workflows/pr.yaml)

> **Note:** The Helm chart is published to a separate OCI path (`cloudflare-tunnel-gateway-controller-chart`) to avoid conflicts with container image tags. Available tags: `VERSION`, `MAJOR.MINOR`, `MAJOR`, `latest`.

Kubernetes controller implementing Gateway API for Cloudflare Tunnel.

Enables routing traffic through Cloudflare Tunnel using standard Gateway API resources (Gateway, HTTPRoute).

## Features

- Standard Gateway API implementation (GatewayClass, Gateway, HTTPRoute)
- Hot reload of tunnel configuration (no cloudflared restart required)
- Optional cloudflared lifecycle management via Helm SDK
- Leader election for high availability deployments
- Multi-arch container images (amd64, arm64)
- Signed container images with cosign

> **Warning:** The controller assumes **exclusive ownership** of the tunnel configuration. It will remove any ingress rules not managed by HTTPRoute resources. Do not use a tunnel that has manually configured routes or is shared with other systems.

## Quick Start

```bash
# 1. Install Gateway API CRDs
kubectl apply -f https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.4.0/standard-install.yaml

# 2. Install the controller
helm install cloudflare-tunnel-gateway-controller \
  oci://ghcr.io/lexfrei/cloudflare-tunnel-gateway-controller-chart \
  --namespace cloudflare-tunnel-system \
  --create-namespace \
  --set config.tunnelID=YOUR_TUNNEL_ID \
  --set config.apiToken=YOUR_API_TOKEN

# 3. Create HTTPRoute to expose your service
kubectl apply -f - <<EOF
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

See [Installation](#installation) for detailed setup instructions.

## Prerequisites

- Kubernetes cluster with Gateway API CRDs installed
- Cloudflare account with a pre-created Cloudflare Tunnel
- Cloudflare API token with tunnel permissions

### Create Cloudflare Tunnel

Before deploying the controller, you must create a Cloudflare Tunnel:

1. Go to [Cloudflare Zero Trust Dashboard](https://one.dash.cloudflare.com/)
2. Navigate to **Networks** > **Tunnels**
3. Click **Create a tunnel**
4. Choose **Cloudflared** connector type
5. Name your tunnel and save the **Tunnel ID** and **Tunnel Token**

The controller manages tunnel ingress configuration via API. You can either:

- Let the controller deploy cloudflared automatically (default behavior)
- Deploy cloudflared yourself using the tunnel token (`cloudflared.enabled: false` in Helm values)

### Cloudflare API Token Permissions

Create an API token at [Cloudflare API Tokens](https://dash.cloudflare.com/profile/api-tokens) with the following permissions:

| Scope | Permission | Access |
|-------|------------|--------|
| Account | Cloudflare Tunnel | Edit |

Account ID is auto-detected from the API token when not explicitly provided (works if the token has access to a single account).

## Installation

Helm is the only supported installation method. It handles CRD installation, RBAC setup, and provides a simple upgrade path.

```bash
helm install cloudflare-tunnel-gateway-controller \
  oci://ghcr.io/lexfrei/cloudflare-tunnel-gateway-controller-chart \
  --namespace cloudflare-tunnel-system \
  --create-namespace \
  --values values.yaml
```

See [charts/cloudflare-tunnel-gateway-controller/README.md](charts/cloudflare-tunnel-gateway-controller/README.md) for all configuration options.

For manual installation without Helm, see [Manual Installation](docs/MANUAL_INSTALLATION.md).

> **Note:** This controller uses [cloudflare-tunnel](https://github.com/lexfrei/charts/tree/main/charts/cloudflare-tunnel) Helm chart under the hood to deploy cloudflared. If you don't need Gateway API integration, you can use that chart directly.

## Usage

Create standard [Gateway API](https://gateway-api.sigs.k8s.io/) HTTPRoute resources referencing the `cloudflare-tunnel` Gateway. The controller automatically syncs routes to Cloudflare Tunnel configuration with hot reload (no cloudflared restart required).

See [Gateway API documentation](docs/GATEWAY_API.md) for supported features and examples.

## External-DNS Integration

The controller sets `status.addresses` on the Gateway with the tunnel CNAME (`TUNNEL_ID.cfargotunnel.com`). If you have [external-dns](https://github.com/kubernetes-sigs/external-dns) configured with Gateway API source, it will automatically create DNS records for your HTTPRoute hostnames.

All external-dns annotations (TTL, provider-specific settings, etc.) should be placed on HTTPRoute resources, not on Gateway. See the [external-dns Gateway API documentation](https://kubernetes-sigs.github.io/external-dns/latest/docs/sources/gateway-api/) for details.

## Documentation

| Document | Description |
|----------|-------------|
| [Architecture](docs/ARCHITECTURE.md) | System architecture and design decisions |
| [AWG Quick Start](docs/AWG_QUICKSTART.md) | AmneziaWG sidecar setup guide |
| [Configuration](docs/CONFIGURATION.md) | CLI flags and environment variables |
| [Gateway API](docs/GATEWAY_API.md) | Supported Gateway API features and limitations |
| [Metrics](docs/METRICS.md) | Prometheus metrics, alerting rules, Grafana dashboard |
| [Development](docs/DEVELOPMENT.md) | Development setup and contributing guide |
| [Manual Installation](docs/MANUAL_INSTALLATION.md) | Installation without Helm (not recommended) |
| [Troubleshooting](docs/TROUBLESHOOTING.md) | Common issues and solutions |
| [Helm Chart](charts/cloudflare-tunnel-gateway-controller/README.md) | Helm chart configuration reference |

## Contributing

Contributions are welcome! Please read [CONTRIBUTING.md](CONTRIBUTING.md) for guidelines.

## Security

For security issues, please see [SECURITY.md](SECURITY.md).

## License

BSD 3-Clause License - see [LICENSE](LICENSE) for details.
