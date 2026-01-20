//go:build conformance

package conformance

import (
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/gateway-api/conformance"
	"sigs.k8s.io/gateway-api/conformance/utils/config"
	"sigs.k8s.io/gateway-api/conformance/utils/suite"
	"sigs.k8s.io/gateway-api/pkg/features"
)

func TestGatewayAPIConformance(t *testing.T) {
	opts := conformance.DefaultOptions(t)

	// Use existing GatewayClass from cluster
	opts.GatewayClassName = "cloudflare-tunnel"

	// Enable debug logging for troubleshooting
	opts.Debug = true

	// Configure CloudflareRoundTripper to route requests through Cloudflare CDN
	// instead of directly to Gateway IP addresses
	opts.RoundTripper = &CloudflareRoundTripper{
		Debug: true,
		TimeoutConfig: config.TimeoutConfig{
			RequestTimeout: 30 * time.Second,
		},
		MaxRetries:    10,
		RetryInterval: 2 * time.Second,
	}

	opts.SupportedFeatures = sets.New[features.FeatureName](
		features.SupportGateway,
		features.SupportHTTPRoute,
		features.SupportGRPCRoute,
		features.SupportReferenceGrant,
	)

	opts.ExemptFeatures = sets.New[features.FeatureName](
		// Cloudflare Tunnel limitations - these features are not supported
		features.SupportHTTPRouteQueryParamMatching,
		features.SupportHTTPRouteMethodMatching,
		features.SupportHTTPRouteResponseHeaderModification,
		features.SupportHTTPRoutePortRedirect,
		features.SupportHTTPRouteSchemeRedirect,
		features.SupportHTTPRoutePathRedirect,
		features.SupportHTTPRouteHostRewrite,
		features.SupportHTTPRoutePathRewrite,
		features.SupportHTTPRouteRequestMirror,
		features.SupportHTTPRouteRequestMultipleMirrors,
		features.SupportHTTPRouteBackendRequestHeaderModification,
	)

	opts.SkipTests = []string{
		// Skip tests for unsupported Cloudflare Tunnel features
		"HTTPRouteHeaderMatching",
		"HTTPRouteQueryParamMatching",
		"HTTPRouteMethodMatching",
		"HTTPRouteRequestHeaderModifier",
		"HTTPRouteResponseHeaderModifier",
		"HTTPRouteRequestRedirect",
		"HTTPRouteRequestMirror",
		"HTTPRouteURLRewrite",
		"HTTPRouteBackendRequestHeaderModifier",
		"HTTPRouteRequestMultipleMirrors",
		"HTTPRouteRedirectPath",
		"HTTPRouteRedirectPort",
		"HTTPRouteRedirectScheme",
		"HTTPRouteRewritePath",
		"HTTPRouteRewriteHost",
	}

	opts.ConformanceProfiles = sets.New[suite.ConformanceProfileName](
		suite.GatewayHTTPConformanceProfileName,
		suite.GatewayGRPCConformanceProfileName,
	)

	opts.Implementation = suite.ParseImplementation(
		"lexfrei",
		"cloudflare-tunnel-gateway-controller",
		"https://github.com/lexfrei/cloudflare-tunnel-gateway-controller",
		"",
		"@lexfrei",
	)

	conformance.RunConformanceWithOptions(t, opts)
}
