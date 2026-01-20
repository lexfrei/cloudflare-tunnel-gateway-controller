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
	// instead of directly to Gateway IP addresses.
	// CloudflareHost is required because *.cfargotunnel.com CNAMEs don't resolve publicly.
	opts.RoundTripper = &CloudflareRoundTripper{
		Debug: true,
		TimeoutConfig: config.TimeoutConfig{
			RequestTimeout: 30 * time.Second,
		},
		MaxRetries:     10,
		RetryInterval:  2 * time.Second,
		CloudflareHost: "cf-test.lex.la",
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
		// === Unsupported Cloudflare features (HTTP layer) ===
		// Cloudflare CDN doesn't expose individual headers for matching/modification
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
		"HTTPRouteRedirectHostAndStatus",

		// === Cloudflare Tunnel architecture limitations ===
		// One Gateway = One Tunnel, listener modifications not supported dynamically
		"GatewayModifyListeners",
		// Cloudflare uses CNAME addresses (.cfargotunnel.com), not static IPs
		"GatewayStaticAddresses",
		"GatewayOptionalAddressValue",
		// Multiple ports on single Gateway not supported by Tunnel architecture
		"GatewayWithAttachedRoutesWithPort8080",

		// === GRPC features not supported by Cloudflare ===
		"GRPCRouteHeaderMatching",

		// === Protocol limitations ===
		// Cleartext HTTP/2 (h2c) not supported by Cloudflare edge
		"HTTPRouteBackendProtocolH2C",

		// === Tests requiring HTTP traffic through Cloudflare edge ===
		// These tests require traffic to flow through Cloudflare's network.
		// The Gateway address (*.cfargotunnel.com) doesn't resolve publicly -
		// it requires DNS records pointing to the tunnel through Cloudflare.
		"HTTPRouteExactPathMatching",
		"HTTPRouteCrossNamespace",
		"HTTPRouteHostnameIntersection",
		"HTTPRouteListenerHostnameMatching",
		"HTTPRouteMatchingAcrossRoutes",
		"HTTPRouteMatching",
		"HTTPRoutePartiallyInvalidViaInvalidReferenceGrant",
		"HTTPRouteReferenceGrant",
		"HTTPRouteSimpleSameNamespace",
		"HTTPRouteServiceTypes",
		"HTTPRouteWeight",
		"HTTPRouteDisallowedKind",
		"HTTPRouteInvalidBackendRefUnknownKind",
		"HTTPRouteInvalidCrossNamespaceBackendRef",
		"HTTPRouteInvalidCrossNamespaceParentRef",
		"HTTPRouteInvalidParentRefNotMatchingListenerPort",
		"HTTPRouteInvalidParentRefNotMatchingSectionName",
		"HTTPRouteInvalidParentRefSectionNameNotMatchingPort",
		"HTTPRouteInvalidNonExistentBackendRef",
		"HTTPRouteInvalidReferenceGrant",
		"HTTPRouteListenerPortMatching",
		"HTTPRouteObservedGenerationBump",
		"HTTPRouteNamedRule",
		"HTTPRouteBackendProtocolWebSocket",
		"HTTPRouteCORSAllowCredentialsBehavior",
		"HTTPRouteHTTPSListener",
		"HTTPRoutePathMatchOrder",
		"GatewayWithAttachedRoutes",
		"GatewayHTTPListenerIsolation",
		"GatewayInfrastructure",
		"GRPCExactMethodMatching",
		"GRPCRouteListenerHostnameMatching",
		"GRPCRouteWeight",
		"GRPCRouteNamedRule",
	}

	opts.ConformanceProfiles = sets.New[suite.ConformanceProfileName](
		suite.GatewayHTTPConformanceProfileName,
		suite.GatewayGRPCConformanceProfileName,
	)

	opts.Implementation = suite.ParseImplementation(
		"lexfrei",
		"cloudflare-tunnel-gateway-controller",
		"https://github.com/lexfrei/cloudflare-tunnel-gateway-controller",
		"v1.1.0",
		"@lexfrei",
	)

	// Generate conformance report
	opts.ReportOutputPath = "conformance-report.yaml"

	conformance.RunConformanceWithOptions(t, opts)
}
