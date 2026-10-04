// Package ingress provides conversion from Gateway API HTTPRoute resources
// to Cloudflare Tunnel ingress configuration.
//
// # Overview
//
// The Builder type converts a list of HTTPRoute resources into Cloudflare
// tunnel ingress rules, one per distinct hostname. The in-process proxy
// receives every tunnel request and does all path and match handling, so no
// rule carries a path. It handles:
//
//   - Hostname extraction from HTTPRoute.spec.hostnames
//   - Backend service resolution to cluster-internal URLs
//   - One rule per hostname, naming the smallest backend URL serving it
//
// # Diff-based Synchronization
//
// The package provides diff-based synchronization functions to minimize
// changes when updating tunnel configuration:
//
//   - DiffRules: Computes rules to add and remove
//   - ApplyDiff: Applies the diff to current rules
//   - EnsureCatchAll: Ensures catch-all rule exists at the end
//
// This approach only adds new rules and removes orphaned rules,
// rather than replacing the entire configuration.
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
