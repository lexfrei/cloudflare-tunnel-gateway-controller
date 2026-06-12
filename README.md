# Cloudflare Tunnel Gateway Controller

[![Go Version](https://img.shields.io/github/go-mod/go-version/lexfrei/cloudflare-tunnel-gateway-controller)](https://go.dev/)
[![License](https://img.shields.io/github/license/lexfrei/cloudflare-tunnel-gateway-controller)](LICENSE)
[![Release](https://img.shields.io/github/v/release/lexfrei/cloudflare-tunnel-gateway-controller)](https://github.com/lexfrei/cloudflare-tunnel-gateway-controller/releases)
[![CI](https://github.com/lexfrei/cloudflare-tunnel-gateway-controller/actions/workflows/pr.yaml/badge.svg)](https://github.com/lexfrei/cloudflare-tunnel-gateway-controller/actions/workflows/pr.yaml)
[![Docs](https://img.shields.io/badge/docs-cf.k8s.lex.la-blue)](https://cf.k8s.lex.la)

Kubernetes controller implementing Gateway API for Cloudflare Tunnel.

Expose in-cluster services through a Cloudflare Tunnel using standard Gateway API resources (Gateway, HTTPRoute, GRPCRoute), with no public load balancer or inbound firewall rule.

## Features

- Standard Gateway API: GatewayClass, Gateway, HTTPRoute, GRPCRoute, ListenerSet
- In-process L7 reverse proxy embeds the cloudflared transport — a single data plane handles routing and tunnel egress
- Hot reload of routing configuration (no cloudflared restart on route changes)
- Path matching (prefix, exact, regex), header / query-parameter / HTTP-method matching
- Request and response header modification, URL rewrite, request redirect, request mirror
- Weighted traffic splitting across backends
- HTTPRoute CORS filter
- Cross-namespace backend references gated by ReferenceGrant
- Backend TLS (`BackendTLSPolicy`) and backend WebSocket via `appProtocol`
- Leader election for high-availability deployments
- Multi-arch images (amd64, arm64), signed with cosign

> **Warning:** The controller assumes **exclusive ownership** of the tunnel configuration. It removes any ingress rules not managed by HTTPRoute/GRPCRoute resources. Do not point it at a tunnel that has manually configured routes or is shared with other systems.

## L7 Proxy

The controller runs an in-process L7 reverse proxy inside the cloudflared process via the `OverrideProxy` hook (using a [fork of cloudflared](https://github.com/lexfrei/cloudflared)). All tunnel traffic is intercepted by the proxy, which applies Gateway API routing rules — hostname, path, header, query and method matching, filters, and weighted backend selection — before forwarding to backends. The Cloudflare Tunnel API configuration is used only for DNS / edge routing; request routing is handled entirely by the proxy, so HTTPRoute features work end-to-end regardless of Cloudflare Tunnel's native capabilities.

See [L7 Proxy Architecture](https://cf.k8s.lex.la/latest/development/architecture/) for full details.

## Quick Start

```bash
# 1. Install Gateway API CRDs
kubectl apply -f https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.5.1/standard-install.yaml

# 2. Create credentials Secrets
kubectl create namespace cloudflare-tunnel-system
kubectl create secret generic cloudflare-credentials \
  --namespace cloudflare-tunnel-system \
  --from-literal=api-token="YOUR_API_TOKEN"
kubectl create secret generic cloudflare-tunnel-token \
  --namespace cloudflare-tunnel-system \
  --from-literal=tunnel-token="YOUR_TUNNEL_TOKEN"

# 3. Install the controller
helm install cloudflare-tunnel-gateway-controller \
  oci://ghcr.io/lexfrei/charts/cloudflare-tunnel-gateway-controller \
  --namespace cloudflare-tunnel-system \
  --set gatewayClassConfig.create=true \
  --set gatewayClassConfig.tunnelID=YOUR_TUNNEL_ID \
  --set gatewayClassConfig.cloudflareCredentialsSecretRef.name=cloudflare-credentials \
  --set proxy.tunnelTokenSecretRef.name=cloudflare-tunnel-token

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

The controller manages tunnel ingress configuration via the Cloudflare API. Tunnel traffic is terminated by the in-process L7 proxy that ships with the chart; supply the tunnel token via `proxy.tunnelTokenSecretRef` in Helm values.

### Cloudflare API Token Permissions

Create an API token at [Cloudflare API Tokens](https://dash.cloudflare.com/profile/api-tokens) with the following permissions:

| Scope | Permission | Access |
| --- | --- | --- |
| Account | Cloudflare Tunnel | Edit |

Account ID is auto-detected from the API token when not explicitly provided (works if the token has access to a single account).

## Installation

Helm is the only supported installation method. It handles CRD installation, RBAC setup, and provides a simple upgrade path.

```bash
helm install cloudflare-tunnel-gateway-controller \
  oci://ghcr.io/lexfrei/charts/cloudflare-tunnel-gateway-controller \
  --namespace cloudflare-tunnel-system \
  --create-namespace \
  --values values.yaml
```

See [charts/cloudflare-tunnel-gateway-controller/README.md](charts/cloudflare-tunnel-gateway-controller/README.md) for all configuration options.

For manual installation without Helm, see [Manual Installation](https://cf.k8s.lex.la/latest/operations/manual-installation/).

## Usage

Create standard [Gateway API](https://gateway-api.sigs.k8s.io/) HTTPRoute or GRPCRoute resources referencing the `cloudflare-tunnel` Gateway. The controller syncs routes to the in-process L7 proxy and the Cloudflare Tunnel configuration with hot reload (no cloudflared restart). Both HTTPRoute and GRPCRoute are served by the proxy at runtime.

### Supported Gateway Fields

| Field | Supported | Notes |
| --- | --- | --- |
| `spec.gatewayClassName` | ✅ | Must match controller's GatewayClass |
| `spec.listeners` | ✅ | Fully processed for route binding and status |
| `spec.listeners[].name` | ✅ | Used for route binding, status reporting, attached route counting |
| `spec.listeners[].protocol` | ✅ | HTTP/HTTPS listeners bind HTTPRoute and GRPCRoute |
| `spec.listeners[].port` | ✅ | Used for route binding when route specifies a port |
| `spec.listeners[].hostname` | ✅ | Routes must have intersecting hostnames |
| `spec.listeners[].tls` | ✅ | CertificateRefs validated with ReferenceGrant support |
| `spec.listeners[].allowedRoutes` | ✅ | Namespace (Same/All/Selector) and kind filtering |
| `spec.addresses` | ❌ | Ignored; tunnel CNAME set in status |
| `spec.infrastructure` | ❌ | Not implemented |

> **Note:** Cloudflare Tunnel terminates TLS at its edge. TLS certificate references on listeners are validated (including cross-namespace ReferenceGrant checks), but the actual TLS termination is handled by Cloudflare, not by the controller.

### Supported Route Fields

All matching and filter behavior is performed by the in-process L7 proxy that the chart deploys alongside the controller.

**HTTPRoute:**

| Field | Supported | Notes |
| --- | --- | --- |
| `spec.hostnames` | ✅ | Wildcard `*` supported |
| `spec.rules[].matches[].path` | ✅ | PathPrefix, Exact, RegularExpression |
| `spec.rules[].matches[].headers` | ✅ | Exact and RegularExpression |
| `spec.rules[].matches[].queryParams` | ✅ | Exact and RegularExpression |
| `spec.rules[].matches[].method` | ✅ | All HTTP methods |
| `spec.rules[].filters` | ✅ | Header modifier, redirect, URL rewrite, mirror, CORS |
| `spec.rules[].backendRefs` | ✅ | Service name, namespace, port |
| `spec.rules[].backendRefs[].namespace` | ✅ | Cross-namespace refs require ReferenceGrant |
| `spec.rules[].backendRefs[].weight` | ✅ | True weighted traffic splitting across backends |

**GRPCRoute:** ✅ Served by the in-process L7 proxy. gRPC service/method matches map onto `/{service}/{method}` path rules. The upstream hop is cleartext h2c by default; attaching a `BackendTLSPolicy` upgrades the hop to TLS with HTTP/2 negotiated via ALPN, and the Gateway's `clientCertificateRef` is presented on the handshake for mTLS. See [GRPCRoute docs](https://cf.k8s.lex.la/latest/gateway-api/grpcroute/).

A `backendRef` may target a core `Service`, a `ServiceImport` (`multicluster.x-k8s.io`), or an `ExternalBackend` (`cf.k8s.lex.la`, an out-of-cluster HTTP(S) URL). Other kinds are reported `ResolvedRefs=False, InvalidKind`.

### Limitations

The L7 proxy handles routing for every tunnel request, so most Gateway API behavior works end-to-end. The caveats that remain are documented in full on the [Limitations](https://cf.k8s.lex.la/latest/gateway-api/limitations/) page:

- Edge-side constraints — Cloudflare hostname registration and edge HTTPS termination apply to all traffic.
- gRPC requires Cloudflare zone gRPC proxying enabled (dashboard → Network → gRPC); otherwise the edge returns `403` zone-wide for `application/grpc`.
- `BackendTLSPolicy` (proxy → backend TLS) is supported at minimum-viable scope: explicit `CACertificateRefs` only, `Hostname` and `URI` SANs, backend mTLS via the Gateway's `clientCertificateRef`.
- Backend WebSocket via `appProtocol: kubernetes.io/ws` (and `/wss` with a `BackendTLSPolicy`).
- `timeouts.request` / `timeouts.backendRequest` are enforced as header-only deadlines, so streaming responses (SSE, chunked, gRPC server-streaming) keep flowing past the deadline.
- Unavailable backends in a weighted rule return a status (`500`/`503`) for their share rather than dialing a dead address, so the other backends keep serving.
- `HTTPRouteRule.name` uniqueness is not enforced at admission; an opt-in `ValidatingAdmissionPolicy` (`ruleNameUniquenessPolicy` Helm value) enforces it on Kubernetes 1.30+.

The proxy can emit a structured per-request access log via `proxy.accessLog.enabled: true`. See [Access Logging](https://cf.k8s.lex.la/latest/operations/access-logging/).

See the [Gateway API documentation](https://cf.k8s.lex.la/latest/gateway-api/) for full details and examples.

## External-DNS Integration

The controller sets `status.addresses` on the Gateway with the tunnel CNAME (`TUNNEL_ID.cfargotunnel.com`). If you run [external-dns](https://github.com/kubernetes-sigs/external-dns) with the Gateway API source, it will automatically create DNS records for your HTTPRoute hostnames.

All external-dns annotations (TTL, provider-specific settings, etc.) should be placed on HTTPRoute resources, not on the Gateway. See the [external-dns Gateway API documentation](https://kubernetes-sigs.github.io/external-dns/latest/docs/sources/gateway-api/) for details.

## FAQ

### Why do I get SSL certificate errors for multi-level subdomains?

Cloudflare's free [Universal SSL](https://developers.cloudflare.com/ssl/edge-certificates/universal-ssl/limitations/) certificates only cover root and first-level subdomains:

- ✅ `example.com`, `*.example.com`
- ❌ `app.dev.example.com`

For multi-level subdomains, you need [Advanced Certificate Manager](https://developers.cloudflare.com/ssl/edge-certificates/advanced-certificate-manager/) or a Business/Enterprise plan.

## Documentation

Full documentation is available at **[cf.k8s.lex.la](https://cf.k8s.lex.la)**.

| Section | Description |
| --- | --- |
| [Getting Started](https://cf.k8s.lex.la/latest/getting-started/) | Prerequisites, installation, and quick start |
| [Configuration](https://cf.k8s.lex.la/latest/configuration/) | Controller configuration and Helm values |
| [Gateway API](https://cf.k8s.lex.la/latest/gateway-api/) | Supported resources, examples, and limitations |
| [Guides](https://cf.k8s.lex.la/latest/guides/) | L7 proxy setup, external-dns, cross-namespace, monitoring |
| [Operations](https://cf.k8s.lex.la/latest/operations/) | Troubleshooting, metrics, manual installation |
| [Development](https://cf.k8s.lex.la/latest/development/) | Development setup, architecture, contributing |
| [Reference](https://cf.k8s.lex.la/latest/reference/) | CRD reference, Helm chart, security policy |

## Contributing

Contributions are welcome! Please read [CONTRIBUTING.md](CONTRIBUTING.md) for guidelines.

Common development tasks are available via `make`:

```bash
make help        # list all targets
make check-deps  # verify required tools are installed
make test        # run tests
make lint        # run linter
```

## Security

For security issues, please see [SECURITY.md](SECURITY.md).

## License

BSD 3-Clause License - see [LICENSE](LICENSE) for details.
