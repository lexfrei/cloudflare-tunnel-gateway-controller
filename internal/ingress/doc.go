// Package ingress provides conversion from Gateway API HTTPRoute resources
// to Cloudflare Tunnel ingress configuration.
//
// # Overview
//
// The Builder type converts a list of HTTPRoute resources into Cloudflare
// tunnel ingress rules. It handles:
//
//   - Hostname extraction from HTTPRoute.spec.hostnames
//   - Path matching (Exact and PathPrefix types)
//   - Backend service resolution to cluster-internal URLs
//   - Rule ordering by priority and path specificity
//
// # Path Matching
//
// The builder supports two path match types as defined by Gateway API:
//
//   - PathMatchExact: Matches the path exactly (priority 1)
//   - PathMatchPathPrefix: Matches paths with the given prefix (priority 0)
//
// Rules are sorted by hostname, then by priority (exact matches first),
// then by path length (longer paths first for prefix matches).
//
// # Service Resolution
//
// Backend references are resolved to fully-qualified cluster DNS names:
//
//	http://<service>.<namespace>.svc.<cluster-domain>:<port>
//
// Port 443 automatically uses HTTPS scheme.
//
// # Catch-All Rule
//
// A catch-all rule returning HTTP 404 is always appended as the last rule,
// as required by Cloudflare Tunnel configuration.
package ingress
