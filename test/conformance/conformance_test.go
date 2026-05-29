//go:build conformance

package conformance

import (
	"os"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/gateway-api/conformance"
	confv1 "sigs.k8s.io/gateway-api/conformance/apis/v1"
	"sigs.k8s.io/gateway-api/conformance/tests"
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
		// GRPCRoute is served by the in-process proxy: the converter maps
		// gRPC service/method matches onto /{service}/{method} path rules and
		// forces h2c upstream. The upstream GRPCRoute conformance tests stay
		// in SkipTests below because their gRPC client dials
		// *.cfargotunnel.com directly (Cloudflare ULA, not externally
		// routable); in-house e2e against the real tunnel is the end-to-end
		// signal. The flag reflects actual proxy support.
		features.SupportGRPCRoute,

		// Extended Gateway: the reconciler writes Status.Addresses with the
		// tunnel CNAME unconditionally and never reads spec.addresses, so a
		// Gateway whose spec.addresses entry carries no value is still Accepted
		// and surfaces an address in status — the GatewayOptionalAddressValue
		// contract. (Distinct from SupportGatewayStaticAddresses, exempt below:
		// the tunnel address is not user-supplied.)
		features.SupportGatewayAddressEmpty,

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

		// ListenerSet is in the Gateway API Standard channel as of v1.5.1.
		// The controller watches ListenerSet resources, honours
		// spec.allowedListeners on the parent Gateway, applies the precedence
		// + conflict view (Gateway > ListenerSet by creation time > namespace/
		// name), validates TLS cert refs with a ListenerSet-scoped
		// ReferenceGrant, and accepts HTTPRoute/GRPCRoute parentRefs with
		// Kind=ListenerSet.
		features.SupportListenerSet,

		// Redirect status codes. An e2e probe through the real Cloudflare edge
		// (test/e2e: HTTPRouteRedirectSchemeProbe) confirmed the edge passes a
		// Location: http://... response through untouched for 303/307/308 — the
		// earlier "edge rewrites the redirect scheme to HTTPS" rationale was
		// never observed and does not hold. The proxy emits the correct scheme,
		// status, host, and path, so these run normally.
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
		features.SupportGatewayFrontendClientCertificateValidation,
		features.SupportGatewayFrontendClientCertificateValidationInsecureFallback,
		features.SupportGatewayHTTPSListenerDetectMisdirectedRequests,

		// Protocols not supported
		features.SupportTLSRoute,
		features.SupportTLSRouteModeTerminate,
		features.SupportTLSRouteModeMixed,
		features.SupportUDPRoute,
		features.SupportMesh,
	)

	// --- Skip tests ---
	// Tests that fail due to Cloudflare Tunnel semantics, not missing features.
	// Also skip tests for unsupported protocols/features that ExemptFeatures
	// does not reliably filter (conformance suite runs all profile tests).
	opts.SkipTests = conformanceSkipTests()

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

// conformanceSkipTests returns the conformance ShortNames skipped for Cloudflare
// Tunnel semantics (not missing features). Extracted so the gRPC drift guard
// below can assert coverage without provisioning a cluster.
func conformanceSkipTests() []string {
	return []string{
		// Single logical listener — hostname intersection tests don't apply.
		"HTTPRouteListenerHostnameMatching",
		"HTTPRouteHostnameIntersection",

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
		// HTTPRoute_ extended suite. ConflictResolution is newly enabled this
		// revision — the controller now emits Conflicted on losing policies
		// per GEP-713 — alongside the previously-enabled
		// InvalidCACertificateRef / InvalidKind / ObservedGenerationBump /
		// SANValidation sub-tests.
		"BackendTLSPolicy",

		// Gateway features not applicable to tunnel architecture.
		"GatewayStaticAddresses",
		"GatewayHTTPListenerIsolation",
		"GatewayInfrastructure",
		"GatewayFrontendClientCertificateValidation",
		"GatewayFrontendClientCertificateValidationInsecureFallback",
		"GatewayFrontendInvalidDefaultClientCertificateValidation",
		"GatewayInvalidFrontendClientCertificateValidation",
		"GatewayWithAttachedRoutesWithPort8080",

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
		// routing is exercised by test/e2e/e2e_grpc_test.go through the real
		// edge. ALL GRPCRoute tests must be listed here — the guard test
		// TestGRPCConformanceTestsAreSkipped fails loudly if a suite bump
		// adds one that is not skipped.
		"GRPCExactMethodMatching",
		"GRPCRouteHeaderMatching",
		"GRPCRouteWeight",
		"GRPCRouteNamedRule",
		"GRPCRouteListenerHostnameMatching",
	}
}

// TestGRPCConformanceTestsAreSkipped guards against suite drift: every
// conformance test that exercises SupportGRPCRoute (which this suite now
// declares) must be in the skip list, because the upstream gRPC client dials
// the Gateway address directly — *.cfargotunnel.com resolves to Cloudflare's
// ULA (fd10::/8), unreachable from any external runner, so an unskipped gRPC
// test hangs the mandatory pre-merge run. A future gateway-api bump that adds
// a new gRPC test trips this guard instead of silently hanging conformance.
func TestGRPCConformanceTestsAreSkipped(t *testing.T) {
	t.Parallel()

	skipped := sets.New(conformanceSkipTests()...)

	grpcChecked := 0

	for _, ct := range tests.ConformanceTests {
		if !slices.Contains(ct.Features, features.SupportGRPCRoute) {
			continue
		}

		grpcChecked++

		assert.Truef(t, skipped.Has(ct.ShortName),
			"GRPCRoute conformance test %q exercises SupportGRPCRoute but is not in conformanceSkipTests(); "+
				"it dials the unroutable *.cfargotunnel.com ULA and will hang. Add it to the skip list.",
			ct.ShortName)
	}

	// Guard against a vacuous pass: if the vendored suite ever stops tagging
	// gRPC tests with SupportGRPCRoute, the loop above would assert nothing.
	assert.Positive(t, grpcChecked, "expected at least one SupportGRPCRoute conformance test in the vendored suite")
}

// TestStaleSkipsStayLifted pins the conformance tests that were de-skipped
// once the v3 data plane was shown to satisfy them: exact path matching, the
// optional Gateway address value, the credentials-aware CORS subtest, and the
// 3xx redirect family (the inherited "Cloudflare edge rewrites the Location
// scheme" rationale was disproven by an empirical edge probe). Each of these
// is also documented as supported in README.md / docs/gateway-api, so this
// guard is the machine-checkable counterpart: if a future change silently
// re-adds one of these skips, this test fails and points at the doc drift.
func TestStaleSkipsStayLifted(t *testing.T) {
	t.Parallel()

	skipped := sets.New(conformanceSkipTests()...)

	lifted := []string{
		"HTTPRouteExactPathMatching",
		"GatewayOptionalAddressValue",
		"HTTPRouteCORSAllowCredentialsBehavior",
		"HTTPRoute303Redirect",
		"HTTPRoute307Redirect",
		"HTTPRoute308Redirect",
		"HTTPRouteRedirectHostAndStatus",
		"HTTPRouteRedirectPath",
		"HTTPRouteRedirectPort",
	}

	for _, name := range lifted {
		assert.Falsef(t, skipped.Has(name),
			"%q was de-skipped because the v3 proxy satisfies it (see README.md / docs/gateway-api); "+
				"if it must be skipped again, update the docs in the same change.", name)
	}
}
