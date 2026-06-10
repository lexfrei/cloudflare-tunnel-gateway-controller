# Development

This section covers development setup, architecture, and contribution guidelines for the Cloudflare Tunnel Gateway Controller.

## Overview

The controller is built with:

- **Go** - Primary programming language
- **controller-runtime** - Kubernetes controller framework
- **Cloudflare Go SDK** - For tunnel configuration API
- **Vendored cloudflared fork** - Tunnel transport, exposed via the `OverrideProxy` hook so the in-process L7 proxy receives traffic directly

## Sections

<div class="grid cards" markdown>

-   :material-laptop:{ .lg .middle } **Setup**

    ---

    Development environment setup and build commands.

    [:octicons-arrow-right-24: Setup](setup.md)

-   :material-sitemap:{ .lg .middle } **Architecture**

    ---

    System architecture, components, and data flow.

    [:octicons-arrow-right-24: Architecture](architecture.md)

-   :material-source-pull:{ .lg .middle } **Contributing**

    ---

    Contribution guidelines and code review process.

    [:octicons-arrow-right-24: Contributing](contributing.md)

-   :material-test-tube:{ .lg .middle } **Testing**

    ---

    Testing standards, patterns, and commands.

    [:octicons-arrow-right-24: Testing](testing.md)

</div>

## Quick Start

```bash
# Clone repository
git clone https://github.com/lexfrei/cloudflare-tunnel-gateway-controller.git
cd cloudflare-tunnel-gateway-controller

# Build binary
go build -o bin/controller ./cmd/controller

# Run tests
go test -v -race ./...

# Run linter
golangci-lint run --timeout=5m --build-tags e2e,conformance,envtest
```

## Project Structure

```text
api/v1alpha1/            # GatewayClassConfig and ExternalBackend CRD types
cmd/controller/          # Controller entrypoint and CLI
cmd/proxy/               # L7 proxy binary entrypoint
internal/
  config/                # GatewayClassConfig resolver
  controller/            # Kubernetes controllers (GatewayClass, GatewayClassConfig, Gateway, ListenerSet, HTTPRoute, GRPCRoute, BackendTLSPolicy, ProxySyncer)
  dns/                   # Cluster domain detection
  ingress/               # HTTPRoute → Cloudflare ingress rule conversion
  proxy/                 # L7 reverse proxy (router, matcher, filter, config API)
  tunnel/                # cloudflared tunnel bootstrap and GatewayOriginProxy adapter
charts/                  # Helm chart (controller + proxy)
deploy/                  # Raw Kubernetes manifests
```

## Code Quality

All changes must pass:

- `go test -race ./...` - Unit tests with race detection
- `golangci-lint run --timeout=5m --build-tags e2e,conformance,envtest` - Linting (all errors must be fixed)
- `markdownlint-cli2 '**/*.md'` - Markdown linting
