# Cloudflare Tunnel Gateway Controller

[![Go Version](https://img.shields.io/github/go-mod/go-version/lexfrei/cloudflare-tunnel-gateway-controller)](https://go.dev/)
[![License](https://img.shields.io/github/license/lexfrei/cloudflare-tunnel-gateway-controller)](LICENSE)
[![Release](https://img.shields.io/github/v/release/lexfrei/cloudflare-tunnel-gateway-controller)](https://github.com/lexfrei/cloudflare-tunnel-gateway-controller/releases)
[![CI](https://github.com/lexfrei/cloudflare-tunnel-gateway-controller/actions/workflows/pr.yaml/badge.svg)](https://github.com/lexfrei/cloudflare-tunnel-gateway-controller/actions/workflows/pr.yaml)

> **Note:** The Helm chart is published at `cloudflare-tunnel-gateway-controller/chart` subpath because Helm CLI doesn't support OCI Image Index with `artifactType` selection. Once [helm/helm#31582](https://github.com/helm/helm/issues/31582) is resolved, the chart will be available at `oci://ghcr.io/lexfrei/cloudflare-tunnel-gateway-controller`. Available tags: `VERSION`, `MAJOR.MINOR`, `MAJOR`, `latest`.

Kubernetes controller implementing Gateway API for Cloudflare Tunnel.

Enables routing traffic through Cloudflare Tunnel using standard Gateway API resources (Gateway, HTTPRoute, GRPCRoute).

## Features

- Standard Gateway API implementation (GatewayClass, Gateway, HTTPRoute, GRPCRoute)
- Cross-namespace backend references with ReferenceGrant support
- Hot reload of tunnel configuration (no cloudflared restart required)
- Optional cloudflared lifecycle management via Helm SDK
- Leader election for high availability deployments
- Multi-arch container images (amd64, arm64)
- Signed container images with cosign

> **Warning:** The controller assumes **exclusive ownership** of the tunnel configuration. It will remove any ingress rules not managed by HTTPRoute/GRPCRoute resources. Do not use a tunnel that has manually configured routes or is shared with other systems.

## Quick Start

```bash
# 1. Install Gateway API CRDs
kubectl apply -f https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.4.0/standard-install.yaml

# 2. Install the controller
helm install cloudflare-tunnel-gateway-controller \
  oci://ghcr.io/lexfrei/cloudflare-tunnel-gateway-controller/chart \
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
  oci://ghcr.io/lexfrei/cloudflare-tunnel-gateway-controller/chart \
  --namespace cloudflare-tunnel-system \
  --create-namespace \
  --values values.yaml
```

See [charts/cloudflare-tunnel-gateway-controller/README.md](charts/cloudflare-tunnel-gateway-controller/README.md) for all configuration options.

For manual installation without Helm, see [Manual Installation](docs/MANUAL_INSTALLATION.md).

> **Note:** This controller uses [cloudflare-tunnel](https://github.com/lexfrei/charts/tree/main/charts/cloudflare-tunnel) Helm chart under the hood to deploy cloudflared. If you don't need Gateway API integration, you can use that chart directly.

## Usage

Create standard [Gateway API](https://gateway-api.sigs.k8s.io/) HTTPRoute or GRPCRoute resources referencing the `cloudflare-tunnel` Gateway. The controller automatically syncs routes to Cloudflare Tunnel configuration with hot reload (no cloudflared restart required).

### Supported Gateway Fields

The controller processes Gateway resources but with important limitations due to Cloudflare Tunnel architecture:

| Field | Supported | Notes |
|-------|-----------|-------|
| `spec.gatewayClassName` | ✅ | Must match controller's GatewayClass |
| `spec.listeners` | ⚠️ | Accepted but not used for routing |
| `spec.listeners[].name` | ✅ | Used for status reporting |
| `spec.listeners[].protocol` | ❌ | Ignored; Cloudflare handles TLS |
| `spec.listeners[].port` | ❌ | Ignored; Cloudflare uses 443/80 |
| `spec.listeners[].hostname` | ❌ | Ignored; use HTTPRoute hostnames ([#43](https://github.com/lexfrei/cloudflare-tunnel-gateway-controller/issues/43)) |
| `spec.listeners[].tls` | ❌ | Ignored; Cloudflare manages TLS |
| `spec.listeners[].allowedRoutes` | ❌ | All HTTPRoute/GRPCRoute allowed ([#43](https://github.com/lexfrei/cloudflare-tunnel-gateway-controller/issues/43)) |
| `spec.addresses` | ❌ | Ignored; tunnel CNAME set in status |
| `spec.infrastructure` | ❌ | Not implemented |

> **Note:** Cloudflare Tunnel terminates TLS at Cloudflare edge. The Gateway `listeners` configuration (ports, protocols, TLS settings) is accepted for compatibility but has no effect on routing. All routing is determined by HTTPRoute/GRPCRoute hostnames and paths.

### Supported Route Fields

The controller supports a subset of Gateway API fields that map to Cloudflare Tunnel ingress rules:

**HTTPRoute:**

| Field | Supported | Notes |
|-------|-----------|-------|
| `spec.hostnames` | ✅ | Wildcard `*` supported |
| `spec.rules[].matches[].path` | ✅ | PathPrefix and Exact types |
| `spec.rules[].backendRefs` | ✅ | Service name, namespace, port |
| `spec.rules[].backendRefs[].namespace` | ✅ | Cross-namespace refs require ReferenceGrant |
| `spec.rules[].matches[].headers` | ❌ | Cloudflare limitation |
| `spec.rules[].matches[].queryParams` | ❌ | Cloudflare limitation |
| `spec.rules[].matches[].method` | ❌ | Cloudflare limitation |
| `spec.rules[].filters` | ❌ | Cloudflare limitation |
| `spec.rules[].backendRefs[].weight` | ⚠️ | Highest weight backend used ([#45](https://github.com/lexfrei/cloudflare-tunnel-gateway-controller/issues/45)) |

**GRPCRoute:**

| Field | Supported | Notes |
|-------|-----------|-------|
| `spec.hostnames` | ✅ | Wildcard `*` supported |
| `spec.rules[].matches[].method.service` | ✅ | Maps to `/Service/*` path |
| `spec.rules[].matches[].method.method` | ✅ | Maps to `/Service/Method` path |
| `spec.rules[].backendRefs` | ✅ | Service name, namespace, port |
| `spec.rules[].backendRefs[].namespace` | ✅ | Cross-namespace refs require ReferenceGrant |
| `spec.rules[].matches[].headers` | ❌ | Cloudflare limitation |
| `spec.rules[].filters` | ❌ | Cloudflare limitation |
| `spec.rules[].backendRefs[].weight` | ⚠️ | Highest weight backend used ([#45](https://github.com/lexfrei/cloudflare-tunnel-gateway-controller/issues/45)) |

> **Load Balancing:** This controller does not implement traffic splitting between multiple backends. Cloudflare Tunnel accepts only a single service URL per ingress rule. If you need weighted routing or canary deployments, deploy a dedicated load balancer (Traefik, Envoy, Nginx) and point your HTTPRoute to it. See [Traffic Splitting and Load Balancing](docs/GATEWAY_API.md#traffic-splitting-and-load-balancing) for details.

See [Gateway API documentation](docs/GATEWAY_API.md) for full details and examples.

## External-DNS Integration

The controller sets `status.addresses` on the Gateway with the tunnel CNAME (`TUNNEL_ID.cfargotunnel.com`). If you have [external-dns](https://github.com/kubernetes-sigs/external-dns) configured with Gateway API source, it will automatically create DNS records for your HTTPRoute hostnames.

All external-dns annotations (TTL, provider-specific settings, etc.) should be placed on HTTPRoute resources, not on Gateway. See the [external-dns Gateway API documentation](https://kubernetes-sigs.github.io/external-dns/latest/docs/sources/gateway-api/) for details.

## FAQ

### Why do I get SSL certificate errors for multi-level subdomains?

Cloudflare's free [Universal SSL](https://developers.cloudflare.com/ssl/edge-certificates/universal-ssl/limitations/) certificates only cover root and first-level subdomains:

- ✅ `example.com`, `*.example.com`
- ❌ `app.dev.example.com`

For multi-level subdomains, you need [Advanced Certificate Manager](https://developers.cloudflare.com/ssl/edge-certificates/advanced-certificate-manager/) ($10/month) or a Business/Enterprise plan.

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

## Roadmap

Planned features and improvements:

| Issue | Description | Status |
|-------|-------------|--------|
| [#43](https://github.com/lexfrei/cloudflare-tunnel-gateway-controller/issues/43) | Gateway API listener hostname and allowedRoutes validation | Planned |
| [#45](https://github.com/lexfrei/cloudflare-tunnel-gateway-controller/issues/45) | Select backend with highest weight instead of first | Planned |
| [#44](https://github.com/lexfrei/cloudflare-tunnel-gateway-controller/issues/44) | Warning logs for partially ignored route configuration | Planned |
| [#40](https://github.com/lexfrei/cloudflare-tunnel-gateway-controller/issues/40) | TCPRoute and TLSRoute support (GRPCRoute done in v0.8.0) | In Progress |
| [#33](https://github.com/lexfrei/cloudflare-tunnel-gateway-controller/issues/33) | Auto-generate artifacthub.io/changes from git history | Planned |
| [#25](https://github.com/lexfrei/cloudflare-tunnel-gateway-controller/issues/25) | Increase unit test coverage for core packages | Ongoing |

## Contributing

Contributions are welcome! Please read [CONTRIBUTING.md](CONTRIBUTING.md) for guidelines.

## Security

For security issues, please see [SECURITY.md](SECURITY.md).

## License

BSD 3-Clause License - see [LICENSE](LICENSE) for details.
