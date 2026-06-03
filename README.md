# Cloudflare Tunnel Gateway Controller

[![Go Version](https://img.shields.io/github/go-mod/go-version/lexfrei/cloudflare-tunnel-gateway-controller)](https://go.dev/)
[![License](https://img.shields.io/github/license/lexfrei/cloudflare-tunnel-gateway-controller)](LICENSE)
[![Release](https://img.shields.io/github/v/release/lexfrei/cloudflare-tunnel-gateway-controller)](https://github.com/lexfrei/cloudflare-tunnel-gateway-controller/releases)
[![CI](https://github.com/lexfrei/cloudflare-tunnel-gateway-controller/actions/workflows/pr.yaml/badge.svg)](https://github.com/lexfrei/cloudflare-tunnel-gateway-controller/actions/workflows/pr.yaml)
[![Docs](https://img.shields.io/badge/docs-cf.k8s.lex.la-blue)](https://cf.k8s.lex.la)

Kubernetes controller implementing Gateway API for Cloudflare Tunnel.

Enables routing traffic through Cloudflare Tunnel using standard Gateway API resources (Gateway, HTTPRoute).

## Features

- Standard Gateway API implementation (GatewayClass, Gateway, HTTPRoute, GRPCRoute, ListenerSet)
- Cross-namespace backend references with ReferenceGrant support
- Hot reload of tunnel configuration (no cloudflared restart required)
- In-process L7 proxy embeds cloudflared transport (single data plane, no separate cloudflared deployment)
- Leader election for high availability deployments
- Multi-arch container images (amd64, arm64)
- Signed container images with cosign
- In-process L7 reverse proxy for full Gateway API HTTPRoute support
- Header, query parameter, and HTTP method matching
- Request/response header modification
- URL rewriting and request redirects
- Request mirroring
- Weighted traffic splitting between backends
- Regex path matching

> **Warning:** The controller assumes **exclusive ownership** of the tunnel configuration. It will remove any ingress rules not managed by HTTPRoute resources. Do not use a tunnel that has manually configured routes or is shared with other systems.

## L7 Proxy

The controller runs an in-process L7 reverse proxy inside the cloudflared process via the `OverrideProxy` hook (using a [fork of cloudflared](https://github.com/lexfrei/cloudflared)). This enables full Gateway API HTTPRoute support beyond Cloudflare Tunnel's native capabilities: header matching, query parameter matching, HTTP method matching, request/response header modification, URL rewriting, request redirects, request mirroring, weighted traffic splitting, and regex path matching.

All tunnel traffic is intercepted by the in-process proxy, which applies Gateway API routing rules before forwarding requests to backends. Cloudflare Tunnel API configuration is used only for DNS/edge routing — actual request routing is handled entirely by the proxy.

See [L7 Proxy Architecture](https://cf.k8s.lex.la/development/architecture/) for full details.

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
|-------|------------|--------|
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

For manual installation without Helm, see [Manual Installation](https://cf.k8s.lex.la/operations/manual-installation/).

## Usage

Create standard [Gateway API](https://gateway-api.sigs.k8s.io/) HTTPRoute or GRPCRoute resources referencing the `cloudflare-tunnel` Gateway. The controller automatically syncs routes to Cloudflare Tunnel configuration with hot reload (no cloudflared restart required). Both HTTPRoute and GRPCRoute route through the in-process L7 proxy at runtime.

### Supported Gateway Fields

| Field | Supported | Notes |
| --- | --- | --- |
| `spec.gatewayClassName` | ✅ | Must match controller's GatewayClass |
| `spec.listeners` | ✅ | Fully processed for route binding and status |
| `spec.listeners[].name` | ✅ | Used for route binding, status reporting, attached route counting |
| `spec.listeners[].protocol` | ✅ | Used for route kind filtering (HTTP/HTTPS → HTTPRoute; GRPCRoute filtering still applies but binding is broken in v3 — see Limitations) |
| `spec.listeners[].port` | ✅ | Used for route binding when route specifies a port |
| `spec.listeners[].hostname` | ✅ | Routes must have intersecting hostnames |
| `spec.listeners[].tls` | ✅ | CertificateRefs validated with ReferenceGrant support |
| `spec.listeners[].allowedRoutes` | ✅ | Namespace (Same/All/Selector) and kind filtering |
| `spec.addresses` | ❌ | Ignored; tunnel CNAME set in status |
| `spec.infrastructure` | ❌ | Not implemented |

> **Note:** Cloudflare Tunnel terminates TLS at its edge. TLS certificate references on listeners are validated (including cross-namespace ReferenceGrant checks), but the actual TLS termination is handled by Cloudflare, not by the controller.

### Supported Route Fields

The controller supports a subset of Gateway API fields that map to Cloudflare Tunnel ingress rules:

All matching and filter behavior is performed by the in-process L7 proxy that the chart deploys alongside the controller.

**HTTPRoute:**

| Field | Supported | Notes |
|-------|-----------|-------|
| `spec.hostnames` | ✅ | Wildcard `*` supported |
| `spec.rules[].matches[].path` | ✅ | PathPrefix, Exact, RegularExpression |
| `spec.rules[].matches[].headers` | ✅ | Exact and RegularExpression |
| `spec.rules[].matches[].queryParams` | ✅ | Exact and RegularExpression |
| `spec.rules[].matches[].method` | ✅ | All HTTP methods |
| `spec.rules[].filters` | ✅ | Header modifier, redirect, URL rewrite, mirror |
| `spec.rules[].backendRefs` | ✅ | Service name, namespace, port |
| `spec.rules[].backendRefs[].namespace` | ✅ | Cross-namespace refs require ReferenceGrant |
| `spec.rules[].backendRefs[].weight` | ✅ | True weighted traffic splitting across backends |

**GRPCRoute:** ✅ Supported — served by the in-process L7 proxy. gRPC service/method matches map onto `/{service}/{method}` path rules. The upstream hop is cleartext h2c by default; attaching a `BackendTLSPolicy` upgrades the hop to TLS with HTTP/2 negotiated via ALPN, and the Gateway's `clientCertificateRef` is presented on the handshake for mTLS. See [GRPCRoute docs](https://cf.k8s.lex.la/gateway-api/grpcroute/).

> **Load Balancing:** All traffic flows through the in-process L7 proxy, so full weighted traffic splitting between multiple backends is supported end-to-end. See [Limitations](https://cf.k8s.lex.la/gateway-api/limitations/#traffic-splitting-and-load-balancing) for the edge-side caveats that still apply.

### Known Limitations

The in-process L7 proxy handles routing for every tunnel request. Edge-side caveats (Cloudflare hostname registration, edge HTTPS termination, etc.) are documented in the [Limitations](https://cf.k8s.lex.la/gateway-api/limitations/) page. In particular, a GRPCRoute also needs Cloudflare zone gRPC proxying enabled (dashboard → Network → gRPC); if disabled, the edge returns `403` zone-wide for `application/grpc` and gRPC fails before reaching the proxy ([details](https://cf.k8s.lex.la/gateway-api/limitations/#grpc-requires-cloudflare-zone-grpc-proxying)).

`BackendTLSPolicy` (proxy → backend TLS) is supported at minimum-viable scope:

- `SubjectAltNames` of both `Hostname` (RFC 6125 wildcards via `VerifyHostname`) and `URI` types (e.g. SPIFFE IDs, exact string match against the leaf cert's URIs) are supported, with OR-matching across the list
- `WellKnownCACertificates: System` is not honoured — only explicit `CACertificateRefs`
- Multi-policy conflicts resolve by oldest-creationTimestamp (then alphabetical). Losing policies are stamped `Accepted=False, Reason=Conflicted, Message="conflicts with BackendTLSPolicy <ns/name>"` per GEP-713; distinct `SectionName` scopes do not conflict
- HTTPS-listener Re-encrypt is not supported (Cloudflare terminates frontend TLS at the edge)
- `RequestMirror` filter routes mirrored traffic through the same per-cert TLS-aware transport pool the main leg uses; when a `BackendTLSPolicy` targets a mirror destination the mirrored copy completes the same TLS handshake the main leg would, including mTLS via the Gateway's `clientCertificateRef`
- Backend mTLS is supported via `Gateway.spec.tls.backend.clientCertificateRef` (Gateway API `GatewayBackendClientCertificate` feature, Standard channel). The referenced `kubernetes.io/tls` Secret's keypair is presented during the backend TLS handshake **only** when a `BackendTLSPolicy` also targets the Service — a client cert over plaintext is meaningless. Cross-namespace refs require ReferenceGrant.
- Backend WebSocket is supported via `Service.spec.ports[].appProtocol: kubernetes.io/ws` (and `kubernetes.io/wss` when paired with a `BackendTLSPolicy`). The proxy handles the upgrade on a custom path (`proxyWebSocketUpgrade`) rather than `httputil.ReverseProxy`, because the stdlib path hijacks the conn before writing the 101 status — which fails over cloudflared's HTTP/2 response writer. Route-level `ResponseHeaderModifier` filters apply to the 101 (and to the non-101 fallback when the backend refuses the upgrade), matching how filters apply to plain HTTP responses. If `kubernetes.io/wss` is declared without a `BackendTLSPolicy` the proxy logs a WARN at conversion time and falls through to plaintext — the backend will then reject the upgrade. The proxy does not enforce the precondition; operators must attach the policy themselves.
- `spec.rules[].timeouts.request` and `timeouts.backendRequest` are enforced as header-only deadlines on the backend transport (`http.Transport.ResponseHeaderTimeout`), not as full-request context deadlines. The deadline bounds only the wait for backend response headers; once headers arrive the body streams freely. This keeps Server-Sent Events, chunked transfer, large file downloads, and gRPC server-streaming responses alive past the timeout boundary — a context-based deadline would have truncated them at the timeout mark. Backends that miss the header deadline get a 504 to the client. WebSocket routes are naturally exempt (the upgrade path bypasses the cached transport). The `BackendProtocol: H2C` path uses `golang.org/x/net/http2.Transport`, which has no `ResponseHeaderTimeout` knob; the proxy synthesises one by wrapping the h2c transport with a `headerTimeoutRoundTripper` so h2c backends honour the same streaming-friendly contract.

Besides a core `Service`, a `backendRef` may target a `ServiceImport` (`multicluster.x-k8s.io`, resolved via `clusterset.local` DNS) or an `ExternalBackend` (`cf.k8s.lex.la`, an out-of-cluster HTTP(S) URL). A kind outside that set is reported `ResolvedRefs=False, InvalidKind`. Cross-namespace `ServiceImport`/`ExternalBackend` refs need a `ReferenceGrant` keyed on the matching group/kind. See [Limitations](https://cf.k8s.lex.la/gateway-api/limitations/#non-service-backend-kinds) for details.

An unavailable backend in a weighted rule returns an HTTP status for its traffic fraction rather than dialing a dead address (a generic 502): an invalid `backendRef` (nonexistent Service, missing `ServiceImport`/`ExternalBackend`, unauthorized cross-namespace ref, or unsupported kind) returns `500`, and an authorized Service with no ready endpoints returns `503`. The backend stays in the weighted pool so the other backends keep serving their share, and the `503` clears automatically once an endpoint becomes ready. See [Limitations](https://cf.k8s.lex.la/gateway-api/limitations/#unavailable-backends-return-a-status-not-a-dial-error) for details.

The L7 proxy implements the Gateway API `HTTPRouteCORS` filter (preflight + simple requests, wildcard origin / methods / headers, credentials-aware echoing), including the `HTTPRouteCORSAllowCredentialsBehavior` conformance subtest: `Access-Control-Allow-Credentials` is emitted only for a matched origin, and a credentialed response echoes the concrete request origin rather than `*`.

See [Backend mTLS](https://cf.k8s.lex.la/gateway-api/limitations/#backend-mtls-backendtlspolicy) for details.

The proxy can emit a structured per-request access log (method, host, path, query, status, bytes_written, duration_ms, user_agent) via `proxy.accessLog.enabled: true` in Helm values. Off by default; sampling-rate knob keeps log volume manageable on busy gateways. See [Access Logging](https://cf.k8s.lex.la/operations/access-logging/) for the full contract.

See [Gateway API documentation](https://cf.k8s.lex.la/gateway-api/) for full details and examples.

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

Full documentation is available at **[cf.k8s.lex.la](https://cf.k8s.lex.la)**.

| Section | Description |
|---------|-------------|
| [Getting Started](https://cf.k8s.lex.la/getting-started/) | Prerequisites, installation, and quick start |
| [Configuration](https://cf.k8s.lex.la/configuration/) | Controller configuration and Helm values |
| [Gateway API](https://cf.k8s.lex.la/gateway-api/) | Supported resources, examples, and limitations |
| [Guides](https://cf.k8s.lex.la/guides/) | L7 proxy setup, external-dns, cross-namespace, monitoring |
| [Operations](https://cf.k8s.lex.la/operations/) | Troubleshooting, metrics, manual installation |
| [Development](https://cf.k8s.lex.la/development/) | Development setup, architecture, contributing |
| [Reference](https://cf.k8s.lex.la/reference/) | CRD reference, Helm chart, security policy |

## Roadmap

Planned features and improvements:

| Issue | Description | Status |
|-------|-------------|--------|
| -- | L7 reverse proxy for full Gateway API HTTPRoute support | Done |
| [#45](https://github.com/lexfrei/cloudflare-tunnel-gateway-controller/issues/45) | Weighted backend traffic splitting | Done |
| [#44](https://github.com/lexfrei/cloudflare-tunnel-gateway-controller/issues/44) | Warning logs for partially ignored route configuration | Planned |
| [#40](https://github.com/lexfrei/cloudflare-tunnel-gateway-controller/issues/40) | TCPRoute and TLSRoute support (GRPCRoute removed from the runtime in v3 — see [migration guide](https://cf.k8s.lex.la/upgrading/v2-to-v3/)) | In Progress |
| [#33](https://github.com/lexfrei/cloudflare-tunnel-gateway-controller/issues/33) | Auto-generate artifacthub.io/changes from git history | Planned |
| [#25](https://github.com/lexfrei/cloudflare-tunnel-gateway-controller/issues/25) | Increase unit test coverage for core packages | Ongoing |

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
