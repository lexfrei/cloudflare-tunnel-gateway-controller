//go:build conformance

package conformance

import (
	"os"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/gateway-api/conformance"
	confv1 "sigs.k8s.io/gateway-api/conformance/apis/v1"
	"sigs.k8s.io/gateway-api/conformance/utils/config"
	"sigs.k8s.io/gateway-api/conformance/utils/suite"
	"sigs.k8s.io/gateway-api/pkg/features"
)

func TestGatewayAPIConformance(t *testing.T) {
	opts := conformance.DefaultOptions(t)

	opts.GatewayClassName = envOrDefault("CONFORMANCE_GATEWAY_CLASS", "cloudflare-tunnel")
	opts.Debug = true

	// --- Conformance profiles ---
	opts.ConformanceProfiles = sets.New(
		suite.GatewayHTTPConformanceProfileName,
		suite.GatewayGRPCConformanceProfileName,
	)

	// --- Supported features ---
	// These are features our v2 proxy actually implements.
	opts.SupportedFeatures = sets.New[features.FeatureName](
		// Core
		features.SupportGateway,
		features.SupportHTTPRoute,
		features.SupportReferenceGrant,
		features.SupportGRPCRoute,

		// Extended HTTPRoute (all via v2 proxy)
		features.SupportHTTPRouteQueryParamMatching,
		features.SupportHTTPRouteMethodMatching,
		features.SupportHTTPRouteResponseHeaderModification,
		features.SupportHTTPRouteBackendRequestHeaderModification,
		features.SupportHTTPRoutePortRedirect,
		features.SupportHTTPRouteSchemeRedirect,
		features.SupportHTTPRoutePathRedirect,
		features.SupportHTTPRouteHostRewrite,
		features.SupportHTTPRoutePathRewrite,
		features.SupportHTTPRouteRequestMirror,
		features.SupportHTTPRouteRequestTimeout,
		features.SupportHTTPRouteBackendTimeout,
		features.SupportHTTPRouteParentRefPort,
		features.SupportHTTPRoute303RedirectStatusCode,
		features.SupportHTTPRoute307RedirectStatusCode,
		features.SupportHTTPRoute308RedirectStatusCode,
	)

	// --- Exempt features ---
	// Features that don't apply to tunnel architecture — skip silently.
	opts.ExemptFeatures = sets.New[features.FeatureName](
		// Gateway: tunnel has no static IPs, no multi-port, no infra propagation
		features.SupportGatewayStaticAddresses,
		features.SupportGatewayHTTPListenerIsolation,
		features.SupportGatewayInfrastructurePropagation,
		features.SupportGatewayPort8080,
		features.SupportGatewayAddressEmpty,
		features.SupportGatewayBackendClientCertificate,
		features.SupportGatewayFrontendClientCertificateValidation,
		features.SupportGatewayFrontendClientCertificateValidationInsecureFallback,
		features.SupportGatewayHTTPSListenerDetectMisdirectedRequests,
		features.SupportListenerSet,

		// Protocols not supported
		features.SupportTLSRoute,
		features.SupportTLSRouteModeTerminate,
		features.SupportTLSRouteModeMixed,
		features.SupportBackendTLSPolicy,
		features.SupportBackendTLSPolicySANValidation,
		features.SupportUDPRoute,
		features.SupportMesh,

		// HTTPRoute features not implemented
		features.SupportHTTPRouteRequestMultipleMirrors,
		features.SupportHTTPRouteRequestPercentageMirror,
		features.SupportHTTPRouteBackendProtocolH2C,
		features.SupportHTTPRouteBackendProtocolWebSocket,
		features.SupportHTTPRouteCORS,
		features.SupportHTTPRouteNamedRouteRule,
		features.SupportHTTPRouteDestinationPortMatching,

		// GRPCRoute extended
		features.SupportGRPCRouteNamedRouteRule,
	)

	// --- Skip tests ---
	// Tests that fail due to Cloudflare Tunnel semantics, not missing features.
	// Also skip tests for unsupported protocols/features that ExemptFeatures
	// does not reliably filter (conformance suite runs all profile tests).
	opts.SkipTests = []string{
		// Cloudflare Tunnel uses prefix semantics for path matching internally.
		"HTTPRouteExactPathMatching",

		// Single logical listener — hostname intersection tests don't apply.
		"HTTPRouteListenerHostnameMatching",
		"HTTPRouteHostnameIntersection",
		"GRPCRouteListenerHostnameMatching",

		// Cloudflare terminates TLS at edge — we don't control certs.
		"HTTPRouteHTTPSListener",
		"HTTPRouteHTTPSListenerDetectMisdirectedRequests",

		// Tunnel doesn't expose multiple ports.
		"HTTPRouteListenerPortMatching",

		// We only support ClusterIP backends.
		"HTTPRouteServiceTypes",

		// Mesh: not supported — tunnel architecture, no service mesh.
		"MeshBasic",
		"MeshConsumerRoute",
		"MeshFrontend",
		"MeshFrontendHostname",
		"MeshPorts",
		"MeshTrafficSplit",
		"MeshGRPCRouteWeight",
		"MeshHTTPRoute303Redirect",
		"MeshHTTPRoute307Redirect",
		"MeshHTTPRoute308Redirect",
		"MeshHTTPRouteBackendRequestHeaderModifier",
		"MeshHTTPRouteMatching",
		"MeshHTTPRouteNamedRule",
		"MeshHTTPRouteQueryParamMatching",
		"MeshHTTPRouteRedirectHostAndStatus",
		"MeshHTTPRouteRedirectPath",
		"MeshHTTPRouteRedirectPort",
		"MeshHTTPRouteRequestHeaderModifier",
		"MeshHTTPRouteRewritePath",
		"MeshHTTPRouteSchemeRedirect",
		"MeshHTTPRouteSimpleSameNamespace",
		"MeshHTTPRouteWeight",

		// BackendTLSPolicy: not supported — tunnel terminates TLS at edge.
		"BackendTLSPolicy",
		"BackendTLSPolicyConflictResolution",
		"BackendTLSPolicyInvalidCACertificateRef",
		"BackendTLSPolicyInvalidKind",
		"BackendTLSPolicyObservedGenerationBump",
		"BackendTLSPolicySANValidation",

		// Gateway features not applicable to tunnel architecture.
		"GatewayStaticAddresses",
		"GatewayHTTPListenerIsolation",
		"GatewayInfrastructure",
		"GatewayBackendClientCertificateFeature",
		"GatewayFrontendClientCertificateValidation",
		"GatewayFrontendClientCertificateValidationInsecureFallback",
		"GatewayFrontendInvalidDefaultClientCertificateValidation",
		"GatewayInvalidFrontendClientCertificateValidation",
		"GatewayWithAttachedRoutesWithPort8080",
		"GatewayOptionalAddressValue",

		// HTTPRoute features not implemented.
		"HTTPRouteBackendProtocolH2C",
		"HTTPRouteBackendProtocolWebSocket",
		"HTTPRouteCORS",
		"HTTPRouteCORSAllowCredentialsBehavior",
	}

	// --- Timeouts ---
	// Increase timeouts for tunnel latency (Cloudflare edge round-trip).
	timeouts := config.DefaultTimeoutConfig()
	timeouts.RequestTimeout = 30 * time.Second
	timeouts.MaxTimeToConsistency = 90 * time.Second
	timeouts.HTTPRouteMustHaveCondition = 120 * time.Second
	timeouts.GatewayMustHaveAddress = 300 * time.Second
	timeouts.GatewayMustHaveCondition = 300 * time.Second
	timeouts.LatestObservedGenerationSet = 120 * time.Second
	timeouts.RouteMustHaveParents = 120 * time.Second
	opts.TimeoutConfig = timeouts

	// --- Custom RoundTripper ---
	// Routes requests through Cloudflare edge via HTTPS instead of plain HTTP.
	opts.RoundTripper = &TunnelRoundTripper{
		Debug:         true,
		TimeoutConfig: timeouts,
	}

	// --- Implementation metadata ---
	opts.Implementation = confv1.Implementation{
		Organization: "lexfrei",
		Project:      "cloudflare-tunnel-gateway-controller",
		URL:          "https://github.com/lexfrei/cloudflare-tunnel-gateway-controller",
		Version:      envOrDefault("CONTROLLER_VERSION", "dev"),
		Contact:      []string{"@lexfrei"},
	}

	// --- Report output ---
	if path := os.Getenv("CONFORMANCE_REPORT_OUTPUT"); path != "" {
		opts.ReportOutputPath = path
	}

	conformance.RunConformanceWithOptions(t, opts)
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}

	return fallback
}
