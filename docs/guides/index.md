# Guides

This section contains step-by-step guides for common integration scenarios
and advanced configurations.

## Available Guides

<div class="grid cards" markdown>

-   :material-shield-lock:{ .lg .middle } **AmneziaWG Sidecar**

    ---

    Set up traffic obfuscation with AmneziaWG sidecar container.

    [:octicons-arrow-right-24: AmneziaWG Sidecar](awg-sidecar.md)

-   :material-dns:{ .lg .middle } **External-DNS Integration**

    ---

    Automatic DNS record management with external-dns.

    [:octicons-arrow-right-24: External-DNS](external-dns.md)

-   :material-swap-horizontal:{ .lg .middle } **Cross-Namespace Routing**

    ---

    Route traffic to services in different namespaces using ReferenceGrant.

    [:octicons-arrow-right-24: Cross-Namespace](cross-namespace.md)

-   :material-chart-line:{ .lg .middle } **Monitoring**

    ---

    Set up Prometheus metrics, Grafana dashboards, and alerting.

    [:octicons-arrow-right-24: Monitoring](monitoring.md)

</div>

## Prerequisites

Before following these guides, ensure you have:

1. Completed the [Getting Started](../getting-started/index.md) guide
2. A working Cloudflare Tunnel Gateway Controller installation
3. Familiarity with Kubernetes and Gateway API concepts

## Choosing a Guide

| Use Case | Recommended Guide |
|----------|-------------------|
| Traffic obfuscation in restricted networks | [AmneziaWG Sidecar](awg-sidecar.md) |
| Automatic DNS record creation | [External-DNS](external-dns.md) |
| Multi-namespace service mesh | [Cross-Namespace Routing](cross-namespace.md) |
| Production observability | [Monitoring](monitoring.md) |
