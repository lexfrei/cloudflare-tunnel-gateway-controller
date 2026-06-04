//go:build conformance

package conformance

import (
	"os"
	"slices"
	"testing"
	"time"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/test/internal/tunnelhost"
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
	// Fail fast when the edge hostname is unset: without it every request would
	// route at an empty host and the suite would only fail deep inside its poll
	// timeouts. There is no default — the deployed tunnel's hostname must be
	// supplied (hack/conformance-setup.sh threads it from .env).
	_, err := tunnelhost.Resolve(envTunnelHostname)
	if err != nil {
		t.Fatal(err)
	}

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
		// SupportHTTPRouteRequestPercentageMirror can flake on sampling variance
		// over the real tunnel: the subtest asserts the observed mirror rate
		// lands in an 85-115% band over a 500-request sample, and an individual
		// attempt occasionally lands just outside (78%/116% observed) before
		// passing within the suite's retry budget. The tolerance and sample size
		// are hardcoded upstream, so this is documented as a known statistical
		// non-regression rather than tuned — see docs/development/testing.md
		// "Known conformance flakes" and kubernetes-sigs/gateway-api#4933.
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

	// --- Custom gRPC client ---
	// The default gRPC client dials the Gateway address directly; that
	// address is a tunnel CNAME (Cloudflare ULA, unroutable from a test
	// runner). TunnelGRPCClient routes every RPC through the edge instead,
	// mirroring TunnelRoundTripper.
	opts.GRPCClient = &TunnelGRPCClient{
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
		// HTTPRouteHostnameIntersection asserts that a request whose Host
		// matches NO listener hostname is rejected (404), even when an attached
		// route would otherwise serve it. The single tunnel ingress flattens
		// every listener into one routing table, so a route bound to one
		// listener still answers hosts that only the (absent) listener
		// boundary should have excluded — the negative cases (non.matching.com,
		// the wildcard.io apex, foo.specific.com) return 200 instead of 404.
		// This is the same listener-isolation gap as the exempt
		// GatewayHTTPListenerIsolation feature, not a hostname-matching bug:
		// the positive multi-label cases all pass (see
		// HTTPRouteListenerHostnameMatching, which is run).
		"HTTPRouteHostnameIntersection",

		// Cloudflare terminates TLS at edge — we don't control certs.
		"HTTPRouteHTTPSListener",
		"HTTPRouteHTTPSListenerDetectMisdirectedRequests",

		// Tunnel doesn't expose multiple ports.
		"HTTPRouteListenerPortMatching",

		// HTTPRouteServiceTypes routes to three backends: a normal ClusterIP
		// Service with manual EndpointSlices (passes), and two headless
		// Services (clusterIP: None) — one with a selector, one with manual
		// EndpointSlices (both 502). The proxy builds upstreams as the Service
		// FQDN at the Service port (converter.go buildServiceURL) and relies on
		// kube-proxy to translate the Service VIP port to the endpoint
		// targetPort. A headless Service has no VIP, so the FQDN resolves to the
		// endpoint IPs and the proxy dials them at the Service port (8080) while
		// the endpoints listen on the targetPort (3000) -> connection fails.
		// Headless backend support is tracked in #426; until then the whole test
		// is skipped (the suite cannot skip individual sub-cases). The
		// manual-endpointslices sub-case already passes, so the earlier "only
		// ClusterIP backends" reason was inaccurate — non-headless Services with
		// hand-managed EndpointSlices route fine.
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

		// GRPCRouteWeight: the distribution sampler news its own
		// grpc.DefaultClient (grpcroute-weight.go) instead of the injectable
		// suite.GRPCClient, so it dials the unroutable *.cfargotunnel.com
		// directly and cannot be redirected to the Cloudflare edge — the same
		// structural class as the WebSocket skip above. Filed upstream as
		// kubernetes-sigs/gateway-api#4926. The other GRPCRoute tests run via
		// TunnelGRPCClient.
		"GRPCRouteWeight",
	}
}

// TestGRPCConformanceTestsRunThroughTunnelClient guards GRPCRoute coverage.
// The north-south GRPCRoute tests fall into two buckets that this guard pins so
// the split can't drift silently:
//
//   - runViaTunnel: run through TunnelGRPCClient (opts.GRPCClient), which dials
//     the Cloudflare edge instead of the unroutable Gateway address, and MUST
//     NOT be skipped.
//   - skippedWithReason: cannot run through the injected client and stay in
//     conformanceSkipTests with a documented reason (GRPCRouteWeight bypasses
//     opts.GRPCClient via its own DefaultClient — kubernetes-sigs/gateway-api#4926).
//
// Every SupportGRPCRoute test must appear in exactly one bucket, so a
// gateway-api bump that adds a new gRPC test trips this guard and forces a
// conscious decision instead of silently running — or hanging — in the
// mandatory pre-merge run.
func TestGRPCConformanceTestsRunThroughTunnelClient(t *testing.T) {
	t.Parallel()

	skipped := sets.New(conformanceSkipTests()...)

	runViaTunnel := sets.New(
		"GRPCExactMethodMatching",
		"GRPCRouteHeaderMatching",
		"GRPCRouteNamedRule",
		"GRPCRouteListenerHostnameMatching",
	)
	skippedWithReason := sets.New(
		"GRPCRouteWeight",
	)

	grpcChecked := 0

	for _, ct := range tests.ConformanceTests {
		if !slices.Contains(ct.Features, features.SupportGRPCRoute) {
			continue
		}

		// Mesh gRPC tests (e.g. MeshGRPCRouteWeight) also carry
		// SupportGRPCRoute but stay in the general mesh skip — this controller
		// is north-south tunnel ingress, not a service mesh. Only the
		// north-south GRPCRoute tests run through TunnelGRPCClient.
		if slices.Contains(ct.Features, features.SupportMesh) {
			continue
		}

		grpcChecked++

		assert.Truef(t, runViaTunnel.Has(ct.ShortName) || skippedWithReason.Has(ct.ShortName),
			"GRPCRoute conformance test %q exercises SupportGRPCRoute but is in neither bucket; either confirm "+
				"TunnelGRPCClient routes it (add to runViaTunnel) or skip it with a documented reason "+
				"(add to skippedWithReason and conformanceSkipTests).",
			ct.ShortName)

		if runViaTunnel.Has(ct.ShortName) {
			assert.Falsef(t, skipped.Has(ct.ShortName),
				"GRPCRoute conformance test %q must run through TunnelGRPCClient (opts.GRPCClient), not be skipped.",
				ct.ShortName)
		}

		if skippedWithReason.Has(ct.ShortName) {
			assert.Truef(t, skipped.Has(ct.ShortName),
				"GRPCRoute conformance test %q is recorded as un-runnable but is not in conformanceSkipTests().",
				ct.ShortName)
		}
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
		// GRPCRoute tests now routed through TunnelGRPCClient (opts.GRPCClient).
		"GRPCExactMethodMatching",
		"GRPCRouteHeaderMatching",
		"GRPCRouteNamedRule",
		// Lifted once the proxy wildcard matcher accepted multi-label hosts (#371).
		"GRPCRouteListenerHostnameMatching",
		"HTTPRouteListenerHostnameMatching",
	}

	for _, name := range lifted {
		assert.Falsef(t, skipped.Has(name),
			"%q was de-skipped because the v3 proxy satisfies it (see README.md / docs/gateway-api); "+
				"if it must be skipped again, update the docs in the same change.", name)
	}
}
