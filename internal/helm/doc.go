// Package helm provides Helm SDK integration for managing cloudflared deployment.
//
// # Overview
//
// When the controller is started with --manage-cloudflared flag, this package
// handles the lifecycle of cloudflared deployment using the Helm SDK:
//
//   - Automatic chart discovery from OCI registry
//   - Chart version management and upgrades
//   - Release installation, upgrade, and uninstallation
//   - Values configuration for cloudflared and optional AWG sidecar
//
// # Chart Source
//
// The cloudflared chart is pulled from the OCI registry:
//
//	oci://ghcr.io/lexfrei/charts/cloudflare-tunnel
//
// The Manager automatically discovers the latest stable version (non-prerelease)
// and caches the chart to avoid repeated downloads.
//
// # AWG Sidecar
//
// Optional AmneziaWG (AWG) sidecar can be configured for routing cloudflared
// traffic through a VPN tunnel. This requires:
//
//   - AWG configuration secret in the cluster
//   - Unique interface name to avoid conflicts between instances
//
// # Thread Safety
//
// The Manager uses internal locking for chart cache access and is safe
// for concurrent use from multiple goroutines.
package helm
