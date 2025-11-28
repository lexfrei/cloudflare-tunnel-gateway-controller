# Cloudflare Tunnel Gateway Controller

[![Go Version](https://img.shields.io/github/go-mod/go-version/lexfrei/cloudflare-tunnel-gateway-controller)](https://go.dev/)
[![License](https://img.shields.io/github/license/lexfrei/cloudflare-tunnel-gateway-controller)](LICENSE)
[![Release](https://img.shields.io/github/v/release/lexfrei/cloudflare-tunnel-gateway-controller)](https://github.com/lexfrei/cloudflare-tunnel-gateway-controller/releases)
[![CI](https://github.com/lexfrei/cloudflare-tunnel-gateway-controller/actions/workflows/pr.yaml/badge.svg)](https://github.com/lexfrei/cloudflare-tunnel-gateway-controller/actions/workflows/pr.yaml)

> **Note:** The Helm chart is published to a separate OCI path (`cloudflare-tunnel-gateway-controller-chart`) because Helm CLI doesn't support OCI Image Index with `artifactType` selection. Once [helm/helm#31582](https://github.com/helm/helm/issues/31582) is resolved, the chart will be available at `oci://ghcr.io/lexfrei/cloudflare-tunnel-gateway-controller`.

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

# 2. Add Helm repository
helm registry login ghcr.io

# 3. Install the controller
helm install cloudflare-tunnel-gateway-controller \
  oci://ghcr.io/lexfrei/cloudflare-tunnel-gateway-controller-chart \
  --namespace cloudflare-tunnel-system \
  --create-namespace \
  --set config.tunnelID=YOUR_TUNNEL_ID \
  --set config.apiToken=YOUR_API_TOKEN

# 4. Create HTTPRoute to expose your service
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

- Let the controller deploy cloudflared automatically (`--manage-cloudflared=true`)
- Deploy cloudflared yourself using the tunnel token

### Cloudflare API Token Permissions

Create an API token at [Cloudflare API Tokens](https://dash.cloudflare.com/profile/api-tokens) with the following permissions:

| Scope | Permission | Access |
|-------|------------|--------|
| Account | Cloudflare Tunnel | Edit |
| Account | Cloudflare Tunnel | Read |
| Account | Account Settings | Read |

The **Account Settings: Read** permission is required for auto-detecting the Account ID when not explicitly provided.

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

## Usage

Create standard [Gateway API](https://gateway-api.sigs.k8s.io/) HTTPRoute resources referencing the `cloudflare-tunnel` Gateway. The controller automatically syncs routes to Cloudflare Tunnel configuration with hot reload (no cloudflared restart required).

See [Gateway API documentation](docs/GATEWAY_API.md) for supported features and examples.

## External-DNS Integration

The controller sets `status.addresses` on the Gateway with the tunnel CNAME (`TUNNEL_ID.cfargotunnel.com`). If you have [external-dns](https://github.com/kubernetes-sigs/external-dns) configured with Gateway API source, it will automatically create DNS records for your HTTPRoute hostnames.

For provider-specific annotations and configuration details, see the [external-dns Gateway API documentation](https://kubernetes-sigs.github.io/external-dns/latest/docs/sources/gateway-api/).

## Configuration

| Flag | Environment Variable | Default | Description |
|------|---------------------|---------|-------------|
| `--account-id` | `CF_ACCOUNT_ID` | (auto-detect) | Cloudflare account ID |
| `--tunnel-id` | `CF_TUNNEL_ID` | | Cloudflare tunnel ID |
| `--api-token` | `CF_API_TOKEN` | | Cloudflare API token |
| `--cluster-domain` | `CF_CLUSTER_DOMAIN` | (auto-detect) | Kubernetes cluster domain (fallback: `cluster.local`) |
| `--gateway-class-name` | `CF_GATEWAY_CLASS_NAME` | `cloudflare-tunnel` | GatewayClass name to watch |
| `--controller-name` | `CF_CONTROLLER_NAME` | `cf.k8s.lex.la/tunnel-controller` | Controller name |
| `--metrics-addr` | `CF_METRICS_ADDR` | `:8080` | Metrics endpoint address |
| `--health-addr` | `CF_HEALTH_ADDR` | `:8081` | Health probe endpoint address |
| `--log-level` | `CF_LOG_LEVEL` | `info` | Log level (debug, info, warn, error) |
| `--log-format` | `CF_LOG_FORMAT` | `json` | Log format (json, text) |
| `--manage-cloudflared` | `CF_MANAGE_CLOUDFLARED` | `false` | Deploy and manage cloudflared via Helm |
| `--tunnel-token` | `CF_TUNNEL_TOKEN` | | Tunnel token for remote-managed mode |
| `--cloudflared-namespace` | `CF_CLOUDFLARED_NAMESPACE` | `cloudflare-tunnel-system` | Namespace for cloudflared |
| `--cloudflared-protocol` | `CF_CLOUDFLARED_PROTOCOL` | | Transport protocol (auto, quic, http2) |
| `--leader-elect` | | `false` | Enable leader election for HA |
| `--leader-election-namespace` | | | Namespace for leader election lease |
| `--leader-election-name` | | `cloudflare-tunnel-gateway-controller-leader` | Leader election lease name |

## Documentation

| Document | Description |
|----------|-------------|
| [Architecture](docs/ARCHITECTURE.md) | System architecture and design decisions |
| [AWG Quick Start](docs/AWG_QUICKSTART.md) | AmneziaWG sidecar setup guide |
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
