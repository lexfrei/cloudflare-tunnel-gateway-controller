# Getting Started

This section covers everything you need to get the Cloudflare Tunnel Gateway
Controller running in your Kubernetes cluster.

## Overview

The controller enables routing traffic through Cloudflare Tunnel using standard
Gateway API resources. Before installing, you need:

1. A Kubernetes cluster with Gateway API CRDs installed
2. A Cloudflare account with a pre-created Cloudflare Tunnel
3. A Cloudflare API token with tunnel permissions

## Sections

<div class="grid cards" markdown>

-   :material-check-circle:{ .lg .middle } **Prerequisites**

    ---

    Required components, Cloudflare Tunnel creation, and API token setup.

    [:octicons-arrow-right-24: Prerequisites](prerequisites.md)

-   :material-download:{ .lg .middle } **Installation**

    ---

    Install the controller using Helm chart with all configuration options.

    [:octicons-arrow-right-24: Installation](installation.md)

-   :material-rocket-launch:{ .lg .middle } **Quick Start**

    ---

    Create your first HTTPRoute and expose a service through Cloudflare Tunnel.

    [:octicons-arrow-right-24: Quick Start](quickstart.md)

</div>

## Next Steps

After completing the getting started guide:

- Learn about [Configuration](../configuration/index.md) options
- Explore [Gateway API](../gateway-api/index.md) features and examples
- Set up [Monitoring](../guides/monitoring.md) for production deployments
