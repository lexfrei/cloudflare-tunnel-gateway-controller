// Package controller implements Kubernetes controllers for Gateway API resources.
//
// The package provides four main reconcilers:
//
//   - GatewayReconciler: Watches Gateway resources, updates Gateway status with
//     the tunnel CNAME address for external-dns integration. Status-only since
//     v3 (no Helm-SDK lifecycle, no per-Gateway finalizer beyond the legacy
//     v2 strip path).
//   - HTTPRouteReconciler: Watches HTTPRoute resources and synchronizes them
//     to Cloudflare Tunnel ingress configuration via the Cloudflare API.
//     Performs full synchronization on startup and on any route change.
//     Also pushes routing config to the L7 proxy data plane via ProxySyncer.
//   - GRPCRouteReconciler: Same Cloudflare-side sync as HTTPRouteReconciler;
//     does not push to the proxy (the proxy converter does not yet support
//     gRPC-specific routing semantics).
//   - GatewayClassConfigReconciler: Watches GatewayClassConfig CRDs and
//     reports validation status (Secret references resolve, required fields
//     present).
//
// # Architecture
//
// The controllers follow the standard controller-runtime reconciliation pattern:
//
//	┌─────────────┐    watch     ┌─────────────────────────┐
//	│ HTTPRoute   │─────────────>│ HTTPRouteReconciler     │
//	│ resources   │              │                         │
//	└─────────────┘              └───────────┬─────────────┘
//	                                         │
//	┌─────────────┐    watch                 │ Cloudflare API
//	│ Gateway     │─────────────>│           │
//	│ resources   │              │           ▼
//	└─────────────┘              │  ┌─────────────────┐
//	       │                     │  │ Tunnel Config   │
//	       │                     │  └────────┬────────┘
//	       ▼                     │           │
//	┌─────────────────────────┐  │           ▼
//	│ GatewayReconciler       │  │  ┌─────────────────┐
//	│ (status-only)           │  │  │ L7 proxy        │
//	└─────────────────────────┘  │  │ (in-process)    │
//	                             │  └─────────────────┘
//
// # Configuration
//
// Controllers are configured via the Config struct which accepts settings
// from CLI flags or environment variables (CF_* prefix). --proxy-endpoints
// is required (the v3 controller fails bootstrap without it).
//
// # Leader Election
//
// When running multiple replicas for high availability, enable leader election
// via --leader-elect flag to ensure only one controller actively reconciles
// resources at a time.
package controller
