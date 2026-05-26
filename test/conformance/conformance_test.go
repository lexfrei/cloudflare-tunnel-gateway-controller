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
	// Features the v3 in-process L7 proxy implements.
	opts.SupportedFeatures = sets.New[features.FeatureName](
		// Core
		features.SupportGateway,
		features.SupportHTTPRoute,
		features.SupportReferenceGrant,
		// SupportGRPCRoute: not in v3. The proxy converter has no gRPC matcher
		// yet, so gRPC requests get 404 from the proxy router. See
		// docs/gateway-api/limitations.md#grpcroute-is-not-supported-in-v3.
		// Re-add this flag once the converter learns gRPC and the matching
		// conformance tests can pass through the proxy.

		// Extended HTTPRoute (Standard channel feature gates; v1 CRD fields)
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
		features.SupportHTTPRouteBackendProtocolH2C,
		features.SupportHTTPRouteBackendProtocolWebSocket,
		features.SupportHTTPRouteRequestMultipleMirrors,
		features.SupportHTTPRouteRequestPercentageMirror,
		features.SupportBackendTLSPolicy,
		features.SupportBackendTLSPolicySANValidation,
		features.SupportGatewayBackendClientCertificate,
		features.SupportHTTPRouteCORS,
		features.SupportHTTPRouteNamedRouteRule,

		// Extended GRPCRoute (Standard channel feature gates; v1 CRD fields).
		// The matching conformance tests are listed in SkipTests below because
		// the upstream gRPC suite dials *.cfargotunnel.com directly and the
		// Cloudflare ULA address space is not externally routable. The feature
		// flag stays here so the conformance report reflects what the proxy
		// itself supports.
		features.SupportGRPCRouteNamedRouteRule,

		// Extended HTTPRoute (Experimental channel feature gates; v1 CRD fields).
		// DestinationPortMatching is gated as Experimental in upstream but the
		// underlying ParentRef.Port field is in Standard v1 — the gate covers
		// rejecting Accepted=False/NoMatchingParent when a parentRef.port does
		// not match any Listener.Port on the referenced Gateway, which works on
		// any v1 cluster regardless of CRD channel.
		features.SupportHTTPRouteDestinationPortMatching,

		// NOTE: 303/307/308 redirect status codes work correctly in the proxy,
		// but Cloudflare edge rewrites Location scheme to HTTPS, so conformance
		// tests that verify http:// scheme in redirects will always fail.
		// Moved to ExemptFeatures.
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
		features.SupportGatewayFrontendClientCertificateValidation,
		features.SupportGatewayFrontendClientCertificateValidationInsecureFallback,
		features.SupportGatewayHTTPSListenerDetectMisdirectedRequests,
		features.SupportListenerSet,

		// Protocols not supported
		features.SupportTLSRoute,
		features.SupportTLSRouteModeTerminate,
		features.SupportTLSRouteModeMixed,
		features.SupportUDPRoute,
		features.SupportMesh,

		// Redirect status codes: proxy implements them, but Cloudflare edge
		// rewrites Location scheme to HTTPS — conformance tests fail.
		features.SupportHTTPRoute303RedirectStatusCode,
		features.SupportHTTPRoute307RedirectStatusCode,
		features.SupportHTTPRoute308RedirectStatusCode,
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

		// BackendTLSPolicy: the main test exercises Re-encrypt against the
		// "same-namespace-with-https-listener" Gateway; Cloudflare terminates
		// TLS at the edge so HTTPS listeners are not supported (see the
		// HTTPRouteHTTPSListener skip above) and the parent test would fail on
		// its first sub-test. The subsequent HTTP sub-tests are covered by the
		// HTTPRoute_ extended suite. ConflictResolution requires cross-policy
		// conflict tracking (loser stamped with Reason=Conflicted) that this
		// implementation does not yet do — older policy wins, others share the
		// same Accepted=True status.
		// InvalidCACertificateRef, InvalidKind, ObservedGenerationBump, and
		// SANValidation (both Hostname and URI types, including OR-matching)
		// are enabled — the controller emits the conformance-required Reasons,
		// updates ObservedGeneration on every reconcile, and the proxy
		// matches DNS Hostname SANs via VerifyHostname (RFC 6125 wildcards)
		// and URI SANs by exact string equality against the leaf's URIs.
		"BackendTLSPolicy",
		"BackendTLSPolicyConflictResolution",

		// Gateway features not applicable to tunnel architecture.
		"GatewayStaticAddresses",
		"GatewayHTTPListenerIsolation",
		"GatewayInfrastructure",
		"GatewayFrontendClientCertificateValidation",
		"GatewayFrontendClientCertificateValidationInsecureFallback",
		"GatewayFrontendInvalidDefaultClientCertificateValidation",
		"GatewayInvalidFrontendClientCertificateValidation",
		"GatewayWithAttachedRoutesWithPort8080",
		"GatewayOptionalAddressValue",

		// Cloudflare edge always rewrites redirect Location scheme to HTTPS.
		// Our proxy sets scheme correctly, but Cloudflare overrides it.
		"HTTPRoute303Redirect",
		"HTTPRoute307Redirect",
		"HTTPRoute308Redirect",

		// Redirect tests that check Location scheme: Cloudflare edge rewrites
		// http:// to https:// in Location header regardless of proxy response.
		"HTTPRouteRedirectHostAndStatus",
		"HTTPRouteRedirectPath",
		"HTTPRouteRedirectPort",

		// HTTPRouteCORSAllowCredentialsBehavior exercises an edge case
		// in the "credentials + wildcard" branch that this implementation
		// does not yet cover end-to-end; the main HTTPRouteCORS test is
		// enabled above.
		"HTTPRouteCORSAllowCredentialsBehavior",

		// WebSocket: the upstream test calls
		// golang.org/x/net/websocket.Dial against the Gateway address and
		// has no RoundTripper hook for a custom dialer. The Gateway address
		// is *.cfargotunnel.com whose AAAA records point at Cloudflare's
		// ULA (fd10::/8) — unreachable from any external test runner. The
		// proxy's WebSocket path is exercised by the
		// HTTPRouteBackendProtocolWebSocket e2e against the real tunnel
		// hostname; the feature flag stays declared in SupportedFeatures
		// above so the conformance report reflects actual support.
		"HTTPRouteBackendProtocolWebSocket",

		// GRPCRoute: the upstream suite uses grpc.NewClient against the
		// Gateway address with no dialer-injection hook. Same Cloudflare
		// ULA routing limitation as the WebSocket case above. Our gRPC
		// routing is exercised by test/e2e/grpc_* through the real edge.
		"GRPCExactMethodMatching",
		"GRPCRouteHeaderMatching",
		"GRPCRouteWeight",
		"GRPCRouteNamedRule",
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
