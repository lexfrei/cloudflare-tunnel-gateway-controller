# Reference

This section contains reference documentation for the Cloudflare Tunnel Gateway
Controller.

## Sections

<div class="grid cards" markdown>

-   :material-kubernetes:{ .lg .middle } **Helm Chart**

    ---

    Complete Helm chart configuration reference.

    [:octicons-arrow-right-24: Helm Chart](helm-chart.md)

-   :material-code-braces:{ .lg .middle } **CRD Reference**

    ---

    GatewayClassConfig Custom Resource Definition API reference.

    [:octicons-arrow-right-24: CRD Reference](crd-reference.md)

-   :material-shield:{ .lg .middle } **Security**

    ---

    Security policy and vulnerability reporting.

    [:octicons-arrow-right-24: Security](security.md)

</div>

## Quick Links

| Resource | Description |
|----------|-------------|
| [GitHub Releases](https://github.com/lexfrei/cloudflare-tunnel-gateway-controller/releases) | Release notes and changelogs |
| [Container Registry](https://ghcr.io/lexfrei/cloudflare-tunnel-gateway-controller) | Multi-arch container images |
| [Helm Chart Registry](https://ghcr.io/lexfrei/cloudflare-tunnel-gateway-controller/chart) | OCI Helm chart |
| [Gateway API Docs](https://gateway-api.sigs.k8s.io/) | Official Gateway API documentation |
| [Cloudflare Tunnel Docs](https://developers.cloudflare.com/cloudflare-one/connections/connect-networks/) | Cloudflare Tunnel documentation |

## API Versions

| API | Version | Status |
|-----|---------|--------|
| GatewayClassConfig | `cf.k8s.lex.la/v1alpha1` | Alpha |
| Gateway API | `gateway.networking.k8s.io/v1` | GA |
| Cloudflare API | v4 | Stable |
