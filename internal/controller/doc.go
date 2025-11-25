// Package controller implements Kubernetes controllers for Gateway API resources.
//
// The package provides two main controllers:
//
//   - GatewayReconciler: Watches Gateway resources and manages cloudflared deployment
//     via Helm when --manage-cloudflared is enabled. Updates Gateway status with
//     the tunnel CNAME address for external-dns integration.
//
//   - HTTPRouteReconciler: Watches HTTPRoute resources and synchronizes them to
//     Cloudflare Tunnel ingress configuration via the Cloudflare API. Performs
//     full synchronization on startup and on any route change.
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
//	│ (optional Helm mgmt)    │  │  │ cloudflared     │
//	└─────────────────────────┘  │  │ (hot reload)    │
//	                             │  └─────────────────┘
//
// # Configuration
//
// Controllers are configured via the Config struct which accepts settings
// from CLI flags or environment variables (CF_* prefix).
//
// # Leader Election
//
// When running multiple replicas for high availability, enable leader election
// via --leader-elect flag to ensure only one controller actively reconciles
// resources at a time.
package controller
