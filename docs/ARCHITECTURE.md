# Architecture

This document describes the internal architecture of the Cloudflare Tunnel Gateway Controller.

## High-Level Overview

The controller implements the Kubernetes Gateway API to configure Cloudflare Tunnel ingress rules. It watches Gateway and HTTPRoute resources and translates them into Cloudflare Tunnel configuration via the Cloudflare API.

```mermaid
flowchart TB
    subgraph Kubernetes["Kubernetes Cluster"]
        GW[Gateway]
        HR[HTTPRoute]
        SVC[Services]
        CTRL[Controller]
        CFD[cloudflared]
    end

    subgraph Cloudflare["Cloudflare Edge"]
        API[Cloudflare API]
        EDGE[Edge Network]
    end

    GW -->|watch| CTRL
    HR -->|watch| CTRL
    SVC -->|resolve| CTRL
    CTRL -->|configure| API
    API -->|push config| CFD
    CFD -->|tunnel| EDGE
    EDGE -->|traffic| CFD
    CFD -->|route| SVC
```

## Package Structure

```text
cmd/controller/
├── main.go              # Entry point, version injection
└── cmd/
    └── root.go          # CLI flags, Cobra command

internal/
├── controller/
│   ├── manager.go       # Controller manager setup, Run()
│   ├── gateway_controller.go    # Gateway reconciler
│   └── httproute_controller.go  # HTTPRoute reconciler
├── ingress/
│   └── builder.go       # HTTPRoute → Cloudflare rules conversion
└── helm/
    ├── manager.go       # Helm SDK operations
    ├── cloudflared.go   # cloudflared chart values builder
    └── constants.go     # Chart reference, timeouts
```

## Components

### GatewayReconciler

Watches Gateway resources and performs the following:

1. **Filtering**: Only processes Gateways with matching `gatewayClassName`
2. **Finalizers**: Adds finalizer for cleanup when Helm management is enabled
3. **Helm Management**: Deploys/upgrades cloudflared via Helm chart
4. **Status Update**: Sets Gateway address to `<tunnel-id>.cfargotunnel.com`

```mermaid
sequenceDiagram
    participant K8s as Kubernetes API
    participant GR as GatewayReconciler
    participant Helm as Helm Manager
    participant CF as Cloudflare

    K8s->>GR: Gateway created/updated
    GR->>GR: Check GatewayClass match
    alt Helm Management Enabled
        GR->>GR: Add finalizer
        GR->>Helm: Install/Upgrade cloudflared
        Helm->>K8s: Deploy cloudflared pods
    end
    GR->>K8s: Update Gateway status
    Note over K8s: status.addresses = [tunnel-id.cfargotunnel.com]
```

### HTTPRouteReconciler

Watches HTTPRoute resources and synchronizes them to Cloudflare:

1. **Filtering**: Only processes routes referencing managed Gateways
2. **Full Sync**: On any change, rebuilds entire tunnel configuration
3. **API Update**: Pushes configuration to Cloudflare API
4. **Status Update**: Sets route acceptance conditions

```mermaid
sequenceDiagram
    participant K8s as Kubernetes API
    participant HR as HTTPRouteReconciler
    participant Builder as Ingress Builder
    participant CF as Cloudflare API

    K8s->>HR: HTTPRoute changed
    HR->>K8s: List all HTTPRoutes
    HR->>HR: Filter by GatewayClass
    HR->>Builder: Build ingress rules
    Builder->>Builder: Sort by priority
    Builder-->>HR: Cloudflare ingress config
    HR->>CF: Update tunnel configuration
    CF-->>HR: Success
    HR->>K8s: Update HTTPRoute status
```

### Ingress Builder

Converts HTTPRoute specs to Cloudflare Tunnel ingress rules:

| HTTPRoute Field | Cloudflare Rule Field |
|-----------------|----------------------|
| `spec.hostnames[]` | `hostname` |
| `rules[].matches[].path` | `path` (with wildcard for prefix) |
| `rules[].backendRefs[]` | `service` (cluster DNS URL) |

**Rule Ordering**:

1. Alphabetically by hostname
2. Exact matches before prefix matches
3. Longer paths before shorter paths

### Helm Manager

Manages cloudflared deployment lifecycle:

- **Chart Source**: `oci://ghcr.io/lexfrei/charts/cloudflare-tunnel`
- **Auto-upgrade**: Detects and upgrades to latest stable version
- **Values Builder**: Configures tunnel token, protocol, AWG sidecar

## Data Flow

### Configuration Flow

```mermaid
flowchart LR
    subgraph Input
        FLAGS[CLI Flags]
        ENV[Environment Vars]
    end

    subgraph Controller
        VIPER[Viper Config]
        CONFIG[controller.Config]
        MGR[Manager]
    end

    FLAGS --> VIPER
    ENV --> VIPER
    VIPER --> CONFIG
    CONFIG --> MGR
```

### Reconciliation Flow

```mermaid
flowchart TB
    START([Watch Event]) --> CHECK{GatewayClass<br/>matches?}
    CHECK -->|No| SKIP[Skip]
    CHECK -->|Yes| DELETED{Resource<br/>deleted?}

    DELETED -->|Yes| CLEANUP[Cleanup]
    DELETED -->|No| RECONCILE[Reconcile]

    RECONCILE --> SYNC[Sync to Cloudflare]
    SYNC --> STATUS[Update Status]

    CLEANUP --> FINALIZER[Remove Finalizer]

    STATUS --> END([Complete])
    FINALIZER --> END
    SKIP --> END
```

## Error Handling

The controller follows these error handling patterns:

1. **Retryable Errors**: Return `ctrl.Result{Requeue: true}` for transient failures
2. **Permanent Errors**: Log error and update resource status condition
3. **API Errors**: Wrapped with context using `cockroachdb/errors`
4. **Not Found**: Silently ignore (resource was deleted)

## Leader Election

When running multiple replicas for high availability:

- Only one replica is the active leader
- Leader acquires lease in `coordination.k8s.io/leases`
- Other replicas wait in standby mode
- Automatic failover on leader failure

```mermaid
flowchart LR
    subgraph Replicas
        R1[Replica 1<br/>Leader]
        R2[Replica 2<br/>Standby]
        R3[Replica 3<br/>Standby]
    end

    LEASE[(Lease)]

    R1 -->|holds| LEASE
    R2 -.->|watches| LEASE
    R3 -.->|watches| LEASE
```

## Security Considerations

- **API Token**: Stored in Kubernetes Secret, mounted as environment variable
- **RBAC**: Minimal permissions following least-privilege principle
- **Network**: Controller only needs egress to Cloudflare API
- **Container**: Runs as non-root user (UID 65534) with read-only filesystem
