package proxy_test

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
)

func TestConvertHTTPRoutes_Basic(t *testing.T) {
	t.Parallel()

	pathPrefix := gatewayv1.PathMatchPathPrefix
	pathExact := gatewayv1.PathMatchExact

	routes := []*gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						Matches: []gatewayv1.HTTPRouteMatch{
							{
								Path: &gatewayv1.HTTPPathMatch{
									Type:  &pathPrefix,
									Value: new("/"),
								},
							},
						},
						BackendRefs: []gatewayv1.HTTPBackendRef{
							backendRef("web-svc", 80, 1),
						},
					},
				},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						Matches: []gatewayv1.HTTPRouteMatch{
							{
								Path: &gatewayv1.HTTPPathMatch{
									Type:  &pathExact,
									Value: new("/api/health"),
								},
							},
						},
						BackendRefs: []gatewayv1.HTTPBackendRef{
							backendRef("api-svc", 8080, 1),
						},
					},
				},
			},
		},
	}

	cfg := proxy.ConvertHTTPRoutes(context.Background(), routes, "cluster.local", nil, nil, nil)

	require.Len(t, cfg.Rules, 2)
	assert.Contains(t, cfg.Rules[0].Hostnames, "example.com")
	assert.Contains(t, cfg.Rules[1].Hostnames, "example.com")
}

// TestConvertHTTPRoutes_NamedAndUnnamedRules_BothRoutable pins the routing
// behaviour exercised by the upstream conformance test HTTPRouteNamedRule.
// Resource names, paths, and backend service names mirror the upstream
// fixture httproute-named-rule.yaml verbatim. The route's hostnames are
// left unset (matching the fixture) so the converter must produce two
// independently matchable proxy rules purely from path matchers. The rule
// Name field is metadata only and must not interfere with path-based
// dispatch.
func TestConvertHTTPRoutes_NamedAndUnnamedRules_BothRoutable(t *testing.T) {
	t.Parallel()

	pathPrefix := gatewayv1.PathMatchPathPrefix
	ruleName := gatewayv1.SectionName("named-rule")

	routes := []*gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "http-named-rules", Namespace: "gateway-conformance-infra"},
			Spec: gatewayv1.HTTPRouteSpec{
				Rules: []gatewayv1.HTTPRouteRule{
					{
						Name: &ruleName,
						Matches: []gatewayv1.HTTPRouteMatch{
							{Path: &gatewayv1.HTTPPathMatch{Type: &pathPrefix, Value: new("/named")}},
						},
						BackendRefs: []gatewayv1.HTTPBackendRef{
							backendRef("infra-backend-v1", 8080, 1),
						},
					},
					{
						Matches: []gatewayv1.HTTPRouteMatch{
							{Path: &gatewayv1.HTTPPathMatch{Type: &pathPrefix, Value: new("/unnamed")}},
						},
						BackendRefs: []gatewayv1.HTTPBackendRef{
							backendRef("infra-backend-v2", 8080, 1),
						},
					},
				},
			},
		},
	}

	cfg := proxy.ConvertHTTPRoutes(context.Background(), routes, "cluster.local", nil, nil, nil)

	require.Len(t, cfg.Rules, 2)

	// Both rules must keep their distinct path matchers and resolved backends —
	// the Name field on one rule must not alter the output of the other.
	require.Len(t, cfg.Rules[0].Matches, 1)
	require.NotNil(t, cfg.Rules[0].Matches[0].Path)
	assert.Equal(t, "/named", cfg.Rules[0].Matches[0].Path.Value)
	require.Len(t, cfg.Rules[0].Backends, 1)
	assert.Contains(t, cfg.Rules[0].Backends[0].URL, "infra-backend-v1")

	require.Len(t, cfg.Rules[1].Matches, 1)
	require.NotNil(t, cfg.Rules[1].Matches[0].Path)
	assert.Equal(t, "/unnamed", cfg.Rules[1].Matches[0].Path.Value)
	require.Len(t, cfg.Rules[1].Backends, 1)
	assert.Contains(t, cfg.Rules[1].Backends[0].URL, "infra-backend-v2")
}

func TestConvertHTTPRoutes_BackendProtocolH2C(t *testing.T) {
	t.Parallel()

	pathPrefix := gatewayv1.PathMatchPathPrefix

	routes := []*gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "h2c", Namespace: "default"},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						Matches: []gatewayv1.HTTPRouteMatch{
							{Path: &gatewayv1.HTTPPathMatch{Type: &pathPrefix, Value: new("/")}},
						},
						BackendRefs: []gatewayv1.HTTPBackendRef{
							backendRef("grpc-svc", 8081, 1),
							backendRef("plain-svc", 80, 1),
						},
					},
				},
			},
		},
	}

	// Resolver mimics reading Service port appProtocol: grpc-svc:8081 is h2c,
	// everything else has no appProtocol.
	resolver := func(_ context.Context, namespace, name string, port int32) string {
		if namespace == "default" && name == "grpc-svc" && port == 8081 {
			return "kubernetes.io/h2c"
		}

		return ""
	}

	cfg := proxy.ConvertHTTPRoutes(context.Background(), routes, "cluster.local", nil, resolver, nil)

	require.Len(t, cfg.Rules, 1)
	require.Len(t, cfg.Rules[0].Backends, 2)
	assert.Equal(t, proxy.BackendProtocolH2C, cfg.Rules[0].Backends[0].Protocol, "grpc-svc:8081 should be h2c")
	assert.Equal(t, proxy.BackendProtocolHTTP, cfg.Rules[0].Backends[1].Protocol, "plain-svc:80 should default to http")
}

func TestConvertHTTPRoutes_UnknownAppProtocol_LogsAndDefaults(t *testing.T) {
	// Sequential test: swaps the default slog logger so we can capture the
	// warning. t.Parallel() would race with other tests using slog.Default().
	pathPrefix := gatewayv1.PathMatchPathPrefix

	routes := []*gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "wss", Namespace: "default"},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						Matches: []gatewayv1.HTTPRouteMatch{
							{Path: &gatewayv1.HTTPPathMatch{Type: &pathPrefix, Value: new("/")}},
						},
						BackendRefs: []gatewayv1.HTTPBackendRef{backendRef("ws-svc", 80, 1)},
					},
				},
			},
		},
	}

	resolver := func(_ context.Context, _, _ string, _ int32) string {
		return "kubernetes.io/wss"
	}

	var logs bytes.Buffer

	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))

	t.Cleanup(func() { slog.SetDefault(previous) })

	cfg := proxy.ConvertHTTPRoutes(context.Background(), routes, "cluster.local", nil, resolver, nil)

	require.Len(t, cfg.Rules, 1)
	require.Len(t, cfg.Rules[0].Backends, 1)
	assert.Equal(t, proxy.BackendProtocolHTTP, cfg.Rules[0].Backends[0].Protocol,
		"unknown appProtocol must fall back to default HTTP/1.1")
	assert.Contains(t, logs.String(), "unsupported backend appProtocol",
		"unknown appProtocol must surface a warning, not silently disappear")
	assert.Contains(t, logs.String(), "kubernetes.io/wss",
		"warning must name the offending appProtocol")
}

// TestConvertHTTPRoutes_AppProtocolPlaintextPassThrough verifies that
// `appProtocol: http`/`HTTP` flow through silently. They're transport-default
// hints aligning with the proxy's default plaintext HTTP/1.1 transport.
func TestConvertHTTPRoutes_AppProtocolPlaintextPassThrough(t *testing.T) {
	cases := []struct {
		name        string
		appProtocol string
	}{
		{name: "lowercase http", appProtocol: "http"},
		{name: "uppercase HTTP", appProtocol: "HTTP"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pathPrefix := gatewayv1.PathMatchPathPrefix
			routes := []*gatewayv1.HTTPRoute{httpAppProtocolTestRoute(pathPrefix)}

			resolver := func(_ context.Context, _, _ string, _ int32) string { return tc.appProtocol }
			logs, cleanup := captureWarnLogs()
			t.Cleanup(cleanup)

			cfg := proxy.ConvertHTTPRoutes(context.Background(), routes, "cluster.local", nil, resolver, nil)

			require.Len(t, cfg.Rules, 1)
			require.Len(t, cfg.Rules[0].Backends, 1)
			assert.Equal(t, proxy.BackendProtocolHTTP, cfg.Rules[0].Backends[0].Protocol)
			assert.NotContains(t, logs.String(), "unsupported backend appProtocol",
				"http appProtocol must NOT log 'unsupported'")
			assert.NotContains(t, logs.String(), "appProtocol https but no BackendTLSPolicy",
				"plain http appProtocol must NOT log the no-policy warning")
		})
	}
}

// TestConvertHTTPRoutes_AppProtocolHTTPSWithoutPolicy_Warns confirms that
// declaring `appProtocol: https` without a matching BackendTLSPolicy logs a
// WARN — the proxy would otherwise dial in plaintext, silently violating the
// operator's TLS intent.
func TestConvertHTTPRoutes_AppProtocolHTTPSWithoutPolicy_Warns(t *testing.T) {
	cases := []struct {
		name        string
		appProtocol string
	}{
		{name: "lowercase https", appProtocol: "https"},
		{name: "uppercase HTTPS", appProtocol: "HTTPS"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pathPrefix := gatewayv1.PathMatchPathPrefix
			routes := []*gatewayv1.HTTPRoute{httpAppProtocolTestRoute(pathPrefix)}

			resolver := func(_ context.Context, _, _ string, _ int32) string { return tc.appProtocol }
			logs, cleanup := captureWarnLogs()
			t.Cleanup(cleanup)

			// tlsResolver = nil → no BackendTLSPolicy applies → must warn.
			cfg := proxy.ConvertHTTPRoutes(context.Background(), routes, "cluster.local", nil, resolver, nil)

			require.Len(t, cfg.Rules, 1)
			require.Len(t, cfg.Rules[0].Backends, 1)
			assert.Equal(t, proxy.BackendProtocolHTTP, cfg.Rules[0].Backends[0].Protocol)
			assert.Nil(t, cfg.Rules[0].Backends[0].TLS, "no policy attached → no TLS config")
			assert.Contains(t, logs.String(), "appProtocol https but no BackendTLSPolicy",
				"appProtocol https without a policy MUST log a warning so the misconfiguration is visible")
		})
	}
}

// TestConvertHTTPRoutes_AppProtocolHTTPSWithPolicy_NoWarn confirms that
// declaring `appProtocol: https` together with a BackendTLSPolicy is the
// happy path: TLS is configured, no warning is logged.
func TestConvertHTTPRoutes_AppProtocolHTTPSWithPolicy_NoWarn(t *testing.T) {
	pathPrefix := gatewayv1.PathMatchPathPrefix
	routes := []*gatewayv1.HTTPRoute{httpAppProtocolTestRoute(pathPrefix)}

	protocolResolver := func(_ context.Context, _, _ string, _ int32) string { return "https" }
	tlsResolver := func(_ context.Context, _, _ string, _ int32) *proxy.BackendTLSConfig {
		return &proxy.BackendTLSConfig{
			CABundlePEM: "-----BEGIN CERTIFICATE-----\nQUFBQQ==\n-----END CERTIFICATE-----\n",
			ServerName:  "svc.default.svc.cluster.local",
		}
	}

	logs, cleanup := captureWarnLogs()
	t.Cleanup(cleanup)

	cfg := proxy.ConvertHTTPRoutes(context.Background(), routes, "cluster.local", nil, protocolResolver, tlsResolver)

	require.Len(t, cfg.Rules, 1)
	require.Len(t, cfg.Rules[0].Backends, 1)
	assert.NotNil(t, cfg.Rules[0].Backends[0].TLS, "policy attached → TLS config present")
	assert.NotContains(t, logs.String(), "appProtocol https but no BackendTLSPolicy",
		"appProtocol https WITH a BackendTLSPolicy must NOT warn — the operator did the right thing")
}

// TestConvertHTTPRoutes_Mirror_TargetHasBackendTLSPolicy_Warns pins the
// known gap: the RequestMirror filter dials through a side-channel HTTP
// client that does NOT share the proxy's TLS-aware transport pool, so a
// mirror destination protected by BackendTLSPolicy would receive plaintext
// instead of TLS. Until an actual TLS-aware mirror dial lands, the converter
// surfaces a WARN so operators don't ship a silent policy bypass.
func TestConvertHTTPRoutes_Mirror_TargetHasBackendTLSPolicy_Warns(t *testing.T) {
	pathPrefix := gatewayv1.PathMatchPathPrefix
	port := gatewayv1.PortNumber(443)

	routes := []*gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "mirror-route", Namespace: "default"},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"app.example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						Matches: []gatewayv1.HTTPRouteMatch{
							{Path: &gatewayv1.HTTPPathMatch{Type: &pathPrefix, Value: new("/")}},
						},
						Filters: []gatewayv1.HTTPRouteFilter{
							{
								Type: gatewayv1.HTTPRouteFilterRequestMirror,
								RequestMirror: &gatewayv1.HTTPRequestMirrorFilter{
									BackendRef: gatewayv1.BackendObjectReference{
										Name: "mirror-target",
										Port: &port,
									},
								},
							},
						},
						BackendRefs: []gatewayv1.HTTPBackendRef{backendRef("primary", 80, 1)},
					},
				},
			},
		},
	}

	// tlsResolver returns a TLS config for mirror-target → warn must fire.
	tlsResolver := func(_ context.Context, _, serviceName string, _ int32) *proxy.BackendTLSConfig {
		if serviceName == "mirror-target" {
			return &proxy.BackendTLSConfig{
				CABundlePEM: "-----BEGIN CERTIFICATE-----\nQUFBQQ==\n-----END CERTIFICATE-----\n",
				ServerName:  "mirror-target.default.svc.cluster.local",
			}
		}

		return nil
	}

	logs, cleanup := captureWarnLogs()
	t.Cleanup(cleanup)

	proxy.ConvertHTTPRoutes(context.Background(), routes, "cluster.local", nil, nil, tlsResolver)

	assert.Contains(t, logs.String(), "mirror filter dials plaintext",
		"a BackendTLSPolicy protecting a mirror target MUST surface a WARN — "+
			"the mirror leg currently bypasses TLS enforcement and operators must be told")
}

// TestConvertHTTPRoutes_BackendTLSPolicy_OverridesH2C verifies the docs claim
// that when both `appProtocol: kubernetes.io/h2c` and a BackendTLSPolicy
// target the same Service, TLS wins: the URL stays https://, the TLS config
// is attached, and a WARN surfaces the suppressed h2c hint so operators
// don't ship a confusing combo silently. Without this fix the h2c arm would
// rewrite https:// → http://, defeating the TLS policy and silently routing
// plaintext (a security regression).
func TestConvertHTTPRoutes_BackendTLSPolicy_OverridesH2C(t *testing.T) {
	pathPrefix := gatewayv1.PathMatchPathPrefix
	routes := []*gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: "default"},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"app.example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						Matches: []gatewayv1.HTTPRouteMatch{
							{Path: &gatewayv1.HTTPPathMatch{Type: &pathPrefix, Value: new("/")}},
						},
						BackendRefs: []gatewayv1.HTTPBackendRef{backendRef("svc", 443, 1)},
					},
				},
			},
		},
	}

	// Service signals h2c.
	protocolResolver := func(_ context.Context, _, _ string, _ int32) string { return "kubernetes.io/h2c" }
	// BackendTLSPolicy also targets the Service.
	tlsResolver := func(_ context.Context, _, _ string, _ int32) *proxy.BackendTLSConfig {
		return &proxy.BackendTLSConfig{
			CABundlePEM: "-----BEGIN CERTIFICATE-----\nQUFBQQ==\n-----END CERTIFICATE-----\n",
			ServerName:  "svc.default.svc.cluster.local",
		}
	}

	logs, cleanup := captureWarnLogs()
	t.Cleanup(cleanup)

	cfg := proxy.ConvertHTTPRoutes(context.Background(), routes, "cluster.local", nil, protocolResolver, tlsResolver)

	require.Len(t, cfg.Rules, 1)
	require.Len(t, cfg.Rules[0].Backends, 1)

	backend := cfg.Rules[0].Backends[0]
	assert.NotNil(t, backend.TLS, "BackendTLSPolicy MUST stay attached even when h2c is also signalled")
	assert.Contains(t, backend.URL, "https://",
		"TLS policy MUST win — URL must remain https:// so stdlib applies TLSClientConfig. "+
			"If the URL gets rewritten to http:// the proxy silently routes plaintext.")
	assert.Equal(t, proxy.BackendProtocolHTTP, backend.Protocol,
		"with TLS attached the protocol marker drops back to HTTP — HTTP/2 is negotiated via ALPN on the TLS handshake")
	assert.Contains(t, logs.String(), "h2c suppressed by BackendTLSPolicy",
		"the h2c-suppressed warning must surface so operators see the conflict")
}

func httpAppProtocolTestRoute(pathPrefix gatewayv1.PathMatchType) *gatewayv1.HTTPRoute {
	return &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: "default"},
		Spec: gatewayv1.HTTPRouteSpec{
			Hostnames: []gatewayv1.Hostname{"example.com"},
			Rules: []gatewayv1.HTTPRouteRule{
				{
					Matches: []gatewayv1.HTTPRouteMatch{
						{Path: &gatewayv1.HTTPPathMatch{Type: &pathPrefix, Value: new("/")}},
					},
					BackendRefs: []gatewayv1.HTTPBackendRef{backendRef("svc", 80, 1)},
				},
			},
		},
	}
}

func captureWarnLogs() (*bytes.Buffer, func()) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))

	return &logs, func() { slog.SetDefault(previous) }
}

func TestConvertHTTPRoutes_RuleMirror_CrossNamespaceRejectedWithoutGrant(t *testing.T) {
	t.Parallel()

	pathPrefix := gatewayv1.PathMatchPathPrefix
	mirrorNS := gatewayv1.Namespace("other-ns")

	routes := []*gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "mirror", Namespace: "default"},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						Matches: []gatewayv1.HTTPRouteMatch{
							{Path: &gatewayv1.HTTPPathMatch{Type: &pathPrefix, Value: new("/")}},
						},
						Filters: []gatewayv1.HTTPRouteFilter{
							{
								Type: gatewayv1.HTTPRouteFilterRequestMirror,
								RequestMirror: &gatewayv1.HTTPRequestMirrorFilter{
									BackendRef: gatewayv1.BackendObjectReference{
										Name:      "shadow",
										Namespace: &mirrorNS,
										Port:      new(gatewayv1.PortNumber(8080)),
									},
								},
							},
						},
						BackendRefs: []gatewayv1.HTTPBackendRef{backendRef("primary", 80, 1)},
					},
				},
			},
		},
	}

	// Validator that denies every cross-namespace reference — mimics absence
	// of a ReferenceGrant.
	rejectAll := func(_ context.Context, _ string, _ gatewayv1.BackendObjectReference) bool {
		return false
	}

	cfg := proxy.ConvertHTTPRoutes(context.Background(), routes, "cluster.local", rejectAll, nil, nil)

	require.Len(t, cfg.Rules, 1)
	assert.Empty(t, cfg.Rules[0].Filters,
		"rule-level mirror to a cross-namespace Service without ReferenceGrant must be dropped, not silently forwarded")
}

func TestConvertHTTPRoutes_PerBackendMirror_CrossNamespaceRejectedWithoutGrant(t *testing.T) {
	t.Parallel()

	pathPrefix := gatewayv1.PathMatchPathPrefix
	mirrorNS := gatewayv1.Namespace("other-ns")

	routes := []*gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "perbackendmirror", Namespace: "default"},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						Matches: []gatewayv1.HTTPRouteMatch{
							{Path: &gatewayv1.HTTPPathMatch{Type: &pathPrefix, Value: new("/")}},
						},
						BackendRefs: []gatewayv1.HTTPBackendRef{
							{
								BackendRef: gatewayv1.BackendRef{
									BackendObjectReference: gatewayv1.BackendObjectReference{
										Name: "primary",
										Port: new(gatewayv1.PortNumber(80)),
									},
									Weight: new(int32(1)),
								},
								Filters: []gatewayv1.HTTPRouteFilter{
									{
										Type: gatewayv1.HTTPRouteFilterRequestMirror,
										RequestMirror: &gatewayv1.HTTPRequestMirrorFilter{
											BackendRef: gatewayv1.BackendObjectReference{
												Name:      "shadow",
												Namespace: &mirrorNS,
												Port:      new(gatewayv1.PortNumber(8080)),
											},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}

	rejectAll := func(_ context.Context, _ string, _ gatewayv1.BackendObjectReference) bool {
		return false
	}

	cfg := proxy.ConvertHTTPRoutes(context.Background(), routes, "cluster.local", rejectAll, nil, nil)

	require.Len(t, cfg.Rules, 1)
	require.Len(t, cfg.Rules[0].Backends, 1)
	assert.Empty(t, cfg.Rules[0].Backends[0].Filters,
		"per-backend mirror to a cross-namespace Service without ReferenceGrant must be dropped")
}

func TestConvertHTTPRoutes_PerBackendFilters(t *testing.T) {
	t.Parallel()

	pathPrefix := gatewayv1.PathMatchPathPrefix
	port := gatewayv1.PortNumber(80)
	weight := int32(1)

	// Backend-scoped filters are part of the Gateway API HTTPRoute spec
	// (distinct from rule-level filters). The converter must carry them onto
	// BackendRef.Filters; otherwise per-backend tweaks silently no-op.
	routes := []*gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "per-backend", Namespace: "default"},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						Matches: []gatewayv1.HTTPRouteMatch{
							{Path: &gatewayv1.HTTPPathMatch{Type: &pathPrefix, Value: new("/")}},
						},
						BackendRefs: []gatewayv1.HTTPBackendRef{
							{
								BackendRef: gatewayv1.BackendRef{
									BackendObjectReference: gatewayv1.BackendObjectReference{
										Name: "v1",
										Port: &port,
									},
									Weight: &weight,
								},
								Filters: []gatewayv1.HTTPRouteFilter{
									{
										Type: gatewayv1.HTTPRouteFilterRequestHeaderModifier,
										RequestHeaderModifier: &gatewayv1.HTTPHeaderFilter{
											Set: []gatewayv1.HTTPHeader{
												{Name: "X-Backend-Version", Value: "v1"},
											},
										},
									},
								},
							},
							{
								BackendRef: gatewayv1.BackendRef{
									BackendObjectReference: gatewayv1.BackendObjectReference{
										Name: "v2",
										Port: &port,
									},
									Weight: &weight,
								},
							},
						},
					},
				},
			},
		},
	}

	cfg := proxy.ConvertHTTPRoutes(context.Background(), routes, "cluster.local", nil, nil, nil)

	require.Len(t, cfg.Rules, 1)
	require.Len(t, cfg.Rules[0].Backends, 2)
	require.Len(t, cfg.Rules[0].Backends[0].Filters, 1, "v1 backend must have its per-backend filter converted")
	assert.Equal(t, proxy.FilterRequestHeaderModifier, cfg.Rules[0].Backends[0].Filters[0].Type)
	require.NotNil(t, cfg.Rules[0].Backends[0].Filters[0].RequestHeaderModifier)
	require.Len(t, cfg.Rules[0].Backends[0].Filters[0].RequestHeaderModifier.Set, 1)
	assert.Equal(t, "X-Backend-Version", cfg.Rules[0].Backends[0].Filters[0].RequestHeaderModifier.Set[0].Name)
	assert.Equal(t, "v1", cfg.Rules[0].Backends[0].Filters[0].RequestHeaderModifier.Set[0].Value)
	assert.Empty(t, cfg.Rules[0].Backends[1].Filters, "v2 backend declares no per-backend filters")
}

// TestConvertHTTPRoutes_CORSFilter pins the converter's mapping from
// gatewayv1.HTTPCORSFilter to proxy.CORSConfig. Every field on the upstream
// type that this controller honours must round-trip into the proxy config.
func TestConvertHTTPRoutes_CORSFilter(t *testing.T) {
	t.Parallel()

	pathPrefix := gatewayv1.PathMatchPathPrefix
	credentials := true

	routes := []*gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "cors", Namespace: "default"},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						Matches: []gatewayv1.HTTPRouteMatch{
							{Path: &gatewayv1.HTTPPathMatch{Type: &pathPrefix, Value: new("/cors")}},
						},
						Filters: []gatewayv1.HTTPRouteFilter{
							{
								Type: gatewayv1.HTTPRouteFilterCORS,
								CORS: &gatewayv1.HTTPCORSFilter{
									AllowOrigins:     []gatewayv1.CORSOrigin{"https://www.foo.com", "https://*.bar.com"},
									AllowCredentials: &credentials,
									AllowMethods:     []gatewayv1.HTTPMethodWithWildcard{"GET", "OPTIONS"},
									AllowHeaders:     []gatewayv1.HTTPHeaderName{"x-header-1", "x-header-2"},
									ExposeHeaders:    []gatewayv1.HTTPHeaderName{"x-header-3"},
									MaxAge:           3600,
								},
							},
						},
						BackendRefs: []gatewayv1.HTTPBackendRef{backendRef("v1", 80, 1)},
					},
				},
			},
		},
	}

	cfg := proxy.ConvertHTTPRoutes(context.Background(), routes, "cluster.local", nil, nil, nil)

	require.Len(t, cfg.Rules, 1)
	require.Len(t, cfg.Rules[0].Filters, 1, "CORS filter must land on the route's Filters slice")
	assert.Equal(t, proxy.FilterCORS, cfg.Rules[0].Filters[0].Type)

	require.NotNil(t, cfg.Rules[0].Filters[0].CORS)
	cors := cfg.Rules[0].Filters[0].CORS

	assert.Equal(t, []string{"https://www.foo.com", "https://*.bar.com"}, cors.AllowOrigins)
	assert.True(t, cors.AllowCredentials)
	assert.Equal(t, []string{"GET", "OPTIONS"}, cors.AllowMethods)
	assert.Equal(t, []string{"x-header-1", "x-header-2"}, cors.AllowHeaders)
	assert.Equal(t, []string{"x-header-3"}, cors.ExposeHeaders)
	assert.Equal(t, int32(3600), cors.MaxAge)
}

// TestConvertHTTPRoutes_CORSFilter_NilCredentialsAndMaxAge confirms that
// optional fields (AllowCredentials pointer, MaxAge default) round-trip
// correctly: nil pointer → false, zero MaxAge stays zero (the proxy applies
// the spec default of 5 seconds at emit time, not at conversion time).
func TestConvertHTTPRoutes_CORSFilter_NilCredentialsAndMaxAge(t *testing.T) {
	t.Parallel()

	pathPrefix := gatewayv1.PathMatchPathPrefix

	routes := []*gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "cors-minimal", Namespace: "default"},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						Matches: []gatewayv1.HTTPRouteMatch{
							{Path: &gatewayv1.HTTPPathMatch{Type: &pathPrefix, Value: new("/")}},
						},
						Filters: []gatewayv1.HTTPRouteFilter{
							{
								Type: gatewayv1.HTTPRouteFilterCORS,
								CORS: &gatewayv1.HTTPCORSFilter{
									AllowOrigins: []gatewayv1.CORSOrigin{"*"},
									AllowMethods: []gatewayv1.HTTPMethodWithWildcard{"*"},
								},
							},
						},
						BackendRefs: []gatewayv1.HTTPBackendRef{backendRef("v1", 80, 1)},
					},
				},
			},
		},
	}

	cfg := proxy.ConvertHTTPRoutes(context.Background(), routes, "cluster.local", nil, nil, nil)
	require.Len(t, cfg.Rules, 1)
	require.Len(t, cfg.Rules[0].Filters, 1)

	cors := cfg.Rules[0].Filters[0].CORS
	require.NotNil(t, cors)
	assert.False(t, cors.AllowCredentials, "nil AllowCredentials pointer must round-trip to false")
	assert.Equal(t, int32(0), cors.MaxAge, "zero MaxAge must stay zero; spec default is applied at emit time")
}

// TestConvertHTTPRoutes_CORSFilter_NilConfig_Skips guards against a filter
// entry with Type=CORS but no .CORS payload — that's a malformed HTTPRoute
// that the CRD admission webhook normally blocks, but the converter must
// skip it gracefully rather than panic.
func TestConvertHTTPRoutes_CORSFilter_NilConfig_Skips(t *testing.T) {
	t.Parallel()

	pathPrefix := gatewayv1.PathMatchPathPrefix

	routes := []*gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "cors-broken", Namespace: "default"},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						Matches: []gatewayv1.HTTPRouteMatch{
							{Path: &gatewayv1.HTTPPathMatch{Type: &pathPrefix, Value: new("/")}},
						},
						Filters: []gatewayv1.HTTPRouteFilter{
							{Type: gatewayv1.HTTPRouteFilterCORS, CORS: nil},
						},
						BackendRefs: []gatewayv1.HTTPBackendRef{backendRef("v1", 80, 1)},
					},
				},
			},
		},
	}

	cfg := proxy.ConvertHTTPRoutes(context.Background(), routes, "cluster.local", nil, nil, nil)
	require.Len(t, cfg.Rules, 1)
	assert.Empty(t, cfg.Rules[0].Filters,
		"CORS filter with nil .CORS payload must be dropped (no panic, no half-config)")
}

func TestConvertHTTPRoutes_MultipleMirrors(t *testing.T) {
	t.Parallel()

	pathPrefix := gatewayv1.PathMatchPathPrefix
	port := gatewayv1.PortNumber(80)

	// Two RequestMirror filters in the same rule must yield two distinct
	// mirror RouteFilters (one per target). Gateway API 1.5 standard channel
	// removed the previous "at most one mirror filter per rule" restriction.
	routes := []*gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "multi-mirror", Namespace: "default"},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						Matches: []gatewayv1.HTTPRouteMatch{
							{Path: &gatewayv1.HTTPPathMatch{Type: &pathPrefix, Value: new("/")}},
						},
						Filters: []gatewayv1.HTTPRouteFilter{
							{
								Type: gatewayv1.HTTPRouteFilterRequestMirror,
								RequestMirror: &gatewayv1.HTTPRequestMirrorFilter{
									BackendRef: gatewayv1.BackendObjectReference{Name: "mirror-a", Port: &port},
								},
							},
							{
								Type: gatewayv1.HTTPRouteFilterRequestMirror,
								RequestMirror: &gatewayv1.HTTPRequestMirrorFilter{
									BackendRef: gatewayv1.BackendObjectReference{Name: "mirror-b", Port: &port},
								},
							},
						},
						BackendRefs: []gatewayv1.HTTPBackendRef{backendRef("primary", 80, 1)},
					},
				},
			},
		},
	}

	cfg := proxy.ConvertHTTPRoutes(context.Background(), routes, "cluster.local", nil, nil, nil)

	require.Len(t, cfg.Rules, 1)
	require.Len(t, cfg.Rules[0].Filters, 2, "both mirror filters must be carried into the rule")
	assert.Equal(t, proxy.FilterRequestMirror, cfg.Rules[0].Filters[0].Type)
	assert.Equal(t, proxy.FilterRequestMirror, cfg.Rules[0].Filters[1].Type)
	require.NotNil(t, cfg.Rules[0].Filters[0].RequestMirror)
	require.NotNil(t, cfg.Rules[0].Filters[1].RequestMirror)
	assert.Contains(t, cfg.Rules[0].Filters[0].RequestMirror.BackendURL, "mirror-a")
	assert.Contains(t, cfg.Rules[0].Filters[1].RequestMirror.BackendURL, "mirror-b")
}

func TestConvertHTTPRoutes_MirrorWithPercent(t *testing.T) {
	t.Parallel()

	pathPrefix := gatewayv1.PathMatchPathPrefix
	port := gatewayv1.PortNumber(80)
	percent := int32(20)

	routes := []*gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "percent-mirror", Namespace: "default"},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						Matches: []gatewayv1.HTTPRouteMatch{
							{Path: &gatewayv1.HTTPPathMatch{Type: &pathPrefix, Value: new("/")}},
						},
						Filters: []gatewayv1.HTTPRouteFilter{
							{
								Type: gatewayv1.HTTPRouteFilterRequestMirror,
								RequestMirror: &gatewayv1.HTTPRequestMirrorFilter{
									BackendRef: gatewayv1.BackendObjectReference{Name: "mirror", Port: &port},
									Percent:    &percent,
								},
							},
						},
						BackendRefs: []gatewayv1.HTTPBackendRef{backendRef("primary", 80, 1)},
					},
				},
			},
		},
	}

	cfg := proxy.ConvertHTTPRoutes(context.Background(), routes, "cluster.local", nil, nil, nil)

	require.Len(t, cfg.Rules, 1)
	require.Len(t, cfg.Rules[0].Filters, 1)
	require.NotNil(t, cfg.Rules[0].Filters[0].RequestMirror)
	require.NotNil(t, cfg.Rules[0].Filters[0].RequestMirror.Percent, "Percent field must propagate from HTTPRequestMirrorFilter")
	assert.Equal(t, int32(20), *cfg.Rules[0].Filters[0].RequestMirror.Percent)
}

func TestConvertHTTPRoutes_MirrorFractionLargeDenominatorNoOverflow(t *testing.T) {
	t.Parallel()

	pathPrefix := gatewayv1.PathMatchPathPrefix
	port := gatewayv1.PortNumber(80)

	// Numerator=30_000_000 and Denominator=100_000_000 encode 30% sampling at
	// high resolution. The CRD validation does not cap either value.
	// Numerator*100 = 3_000_000_000 overflows int32; int64 arithmetic must be
	// used to land on 30, not on a wrapped negative.
	routes := []*gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "fraction-big", Namespace: "default"},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						Matches: []gatewayv1.HTTPRouteMatch{
							{Path: &gatewayv1.HTTPPathMatch{Type: &pathPrefix, Value: new("/")}},
						},
						Filters: []gatewayv1.HTTPRouteFilter{
							{
								Type: gatewayv1.HTTPRouteFilterRequestMirror,
								RequestMirror: &gatewayv1.HTTPRequestMirrorFilter{
									BackendRef: gatewayv1.BackendObjectReference{Name: "mirror", Port: &port},
									Fraction:   &gatewayv1.Fraction{Numerator: 30_000_000, Denominator: new(int32(100_000_000))},
								},
							},
						},
						BackendRefs: []gatewayv1.HTTPBackendRef{backendRef("primary", 80, 1)},
					},
				},
			},
		},
	}

	cfg := proxy.ConvertHTTPRoutes(context.Background(), routes, "cluster.local", nil, nil, nil)

	require.Len(t, cfg.Rules, 1)
	require.NotNil(t, cfg.Rules[0].Filters[0].RequestMirror.Percent)
	assert.Equal(t, int32(30), *cfg.Rules[0].Filters[0].RequestMirror.Percent,
		"large-denominator Fraction must use int64 arithmetic; got %d (likely int32 overflow)",
		*cfg.Rules[0].Filters[0].RequestMirror.Percent)
}

func TestConvertHTTPRoutes_MirrorPercentDetachedFromSourcePointer(t *testing.T) {
	t.Parallel()

	pathPrefix := gatewayv1.PathMatchPathPrefix
	port := gatewayv1.PortNumber(80)
	percent := int32(20)

	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "alias", Namespace: "default"},
		Spec: gatewayv1.HTTPRouteSpec{
			Hostnames: []gatewayv1.Hostname{"example.com"},
			Rules: []gatewayv1.HTTPRouteRule{
				{
					Matches: []gatewayv1.HTTPRouteMatch{
						{Path: &gatewayv1.HTTPPathMatch{Type: &pathPrefix, Value: new("/")}},
					},
					Filters: []gatewayv1.HTTPRouteFilter{
						{
							Type: gatewayv1.HTTPRouteFilterRequestMirror,
							RequestMirror: &gatewayv1.HTTPRequestMirrorFilter{
								BackendRef: gatewayv1.BackendObjectReference{Name: "mirror", Port: &port},
								Percent:    &percent,
							},
						},
					},
					BackendRefs: []gatewayv1.HTTPBackendRef{backendRef("primary", 80, 1)},
				},
			},
		},
	}

	cfg := proxy.ConvertHTTPRoutes(context.Background(), []*gatewayv1.HTTPRoute{route}, "cluster.local", nil, nil, nil)

	// Mutate the source pointer after conversion. The proxy config must hold a
	// snapshot, not a live alias — otherwise the filter goroutine could read
	// changing values while serving requests.
	*route.Spec.Rules[0].Filters[0].RequestMirror.Percent = 99
	assert.Equal(t, int32(20), *cfg.Rules[0].Filters[0].RequestMirror.Percent,
		"proxy config must own its own copy of Percent; source mutation leaked in")
}

func TestConvertHTTPRoutes_MirrorFractionNonPositiveDenominator_Skipped(t *testing.T) {
	t.Parallel()

	pathPrefix := gatewayv1.PathMatchPathPrefix
	port := gatewayv1.PortNumber(80)

	// CRD validation requires Denominator>=1 but the proxy is permissive against
	// malformed input that bypasses admission (e.g. a pushed config). A zero or
	// negative denominator must not panic-divide; mirrorPercent returns nil and
	// the filter falls back to default 100% sampling.
	routes := []*gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "zero-denom", Namespace: "default"},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						Matches: []gatewayv1.HTTPRouteMatch{
							{Path: &gatewayv1.HTTPPathMatch{Type: &pathPrefix, Value: new("/")}},
						},
						Filters: []gatewayv1.HTTPRouteFilter{
							{
								Type: gatewayv1.HTTPRouteFilterRequestMirror,
								RequestMirror: &gatewayv1.HTTPRequestMirrorFilter{
									BackendRef: gatewayv1.BackendObjectReference{Name: "mirror", Port: &port},
									Fraction:   &gatewayv1.Fraction{Numerator: 1, Denominator: new(int32(0))},
								},
							},
						},
						BackendRefs: []gatewayv1.HTTPBackendRef{backendRef("primary", 80, 1)},
					},
				},
			},
		},
	}

	require.NotPanics(t, func() {
		cfg := proxy.ConvertHTTPRoutes(context.Background(), routes, "cluster.local", nil, nil, nil)
		require.Len(t, cfg.Rules, 1)
		require.Len(t, cfg.Rules[0].Filters, 1)
		require.NotNil(t, cfg.Rules[0].Filters[0].RequestMirror)
		assert.Nil(t, cfg.Rules[0].Filters[0].RequestMirror.Percent,
			"non-positive denominator must drop the sampling rate (fall back to default 100%%)")
	})
}

func TestShouldMirror_Contract(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		percent *int32
		want    bool // either deterministic, or "expected steady-state" for stochastic cases
		isProb  bool // when true, want is the steady-state and we sample
	}{
		{name: "nil percent mirrors every request", percent: nil, want: true},
		{name: "negative percent never mirrors (clamped)", percent: new(int32(-1)), want: false},
		{name: "zero percent never mirrors", percent: new(int32(0)), want: false},
		{name: "one hundred always mirrors", percent: new(int32(100)), want: true},
		{name: "above one hundred always mirrors (clamped)", percent: new(int32(101)), want: true},
		{name: "fifty mirrors stochastically", percent: new(int32(50)), want: true, isProb: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if !tt.isProb {
				// Deterministic case: a single call must produce the expected verdict.
				assert.Equal(t, tt.want, proxy.ShouldMirrorForTest(tt.percent))

				return
			}

			// Stochastic case: across many calls the function must return both
			// true and false at least once (it isn't stuck on one branch).
			sawTrue := false
			sawFalse := false

			for range 1000 {
				if proxy.ShouldMirrorForTest(tt.percent) {
					sawTrue = true
				} else {
					sawFalse = true
				}
				if sawTrue && sawFalse {
					break
				}
			}

			assert.True(t, sawTrue, "percent=50 must produce true at least once over 1000 calls")
			assert.True(t, sawFalse, "percent=50 must produce false at least once over 1000 calls")
		})
	}
}

func TestConvertHTTPRoutes_MirrorWithFractionResolvesToPercent(t *testing.T) {
	t.Parallel()

	pathPrefix := gatewayv1.PathMatchPathPrefix
	port := gatewayv1.PortNumber(80)

	routes := []*gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "fraction-mirror", Namespace: "default"},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						Matches: []gatewayv1.HTTPRouteMatch{
							{Path: &gatewayv1.HTTPPathMatch{Type: &pathPrefix, Value: new("/")}},
						},
						Filters: []gatewayv1.HTTPRouteFilter{
							{
								Type: gatewayv1.HTTPRouteFilterRequestMirror,
								RequestMirror: &gatewayv1.HTTPRequestMirrorFilter{
									BackendRef: gatewayv1.BackendObjectReference{Name: "mirror", Port: &port},
									// 25/50 = 50%
									Fraction: &gatewayv1.Fraction{Numerator: 25, Denominator: new(int32(50))},
								},
							},
						},
						BackendRefs: []gatewayv1.HTTPBackendRef{backendRef("primary", 80, 1)},
					},
				},
			},
		},
	}

	cfg := proxy.ConvertHTTPRoutes(context.Background(), routes, "cluster.local", nil, nil, nil)

	require.Len(t, cfg.Rules, 1)
	require.Len(t, cfg.Rules[0].Filters, 1)
	require.NotNil(t, cfg.Rules[0].Filters[0].RequestMirror)
	require.NotNil(t, cfg.Rules[0].Filters[0].RequestMirror.Percent,
		"Fraction must be normalized to Percent at conversion time")
	assert.Equal(t, int32(50), *cfg.Rules[0].Filters[0].RequestMirror.Percent,
		"25/50 must resolve to 50%%")
}

func TestConvertHTTPRoutes_BackendTLSPolicy_AttachesTLSConfig(t *testing.T) {
	t.Parallel()

	pathPrefix := gatewayv1.PathMatchPathPrefix

	routes := []*gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "tls", Namespace: "default"},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						Matches: []gatewayv1.HTTPRouteMatch{
							{Path: &gatewayv1.HTTPPathMatch{Type: &pathPrefix, Value: new("/")}},
						},
						BackendRefs: []gatewayv1.HTTPBackendRef{
							backendRef("tls-svc", 8443, 1),
							backendRef("plain-svc", 80, 1),
						},
					},
				},
			},
		},
	}

	caPEM := "-----BEGIN CERTIFICATE-----\nFAKE CA PEM FOR TEST\n-----END CERTIFICATE-----\n"
	tlsResolver := func(_ context.Context, namespace, name string, port int32) *proxy.BackendTLSConfig {
		if namespace == "default" && name == "tls-svc" && port == 8443 {
			return &proxy.BackendTLSConfig{
				CABundlePEM:     caPEM,
				ServerName:      "tls-svc.example.com",
				SubjectAltNames: []string{"alt.example.com"},
			}
		}

		return nil
	}

	cfg := proxy.ConvertHTTPRoutes(context.Background(), routes, "cluster.local", nil, nil, tlsResolver)

	require.Len(t, cfg.Rules, 1)
	require.Len(t, cfg.Rules[0].Backends, 2)

	require.NotNil(t, cfg.Rules[0].Backends[0].TLS, "BackendTLSPolicy targeting tls-svc must attach TLS config")
	assert.Equal(t, caPEM, cfg.Rules[0].Backends[0].TLS.CABundlePEM)
	assert.Equal(t, "tls-svc.example.com", cfg.Rules[0].Backends[0].TLS.ServerName)
	assert.Equal(t, []string{"alt.example.com"}, cfg.Rules[0].Backends[0].TLS.SubjectAltNames)
	assert.True(t, strings.HasPrefix(cfg.Rules[0].Backends[0].URL, "https://"),
		"backend with TLS policy must use https:// scheme regardless of port, got %q", cfg.Rules[0].Backends[0].URL)

	assert.Nil(t, cfg.Rules[0].Backends[1].TLS, "plain-svc has no BackendTLSPolicy")
	assert.True(t, strings.HasPrefix(cfg.Rules[0].Backends[1].URL, "http://"),
		"plain backend on port 80 keeps http:// scheme")
}

func TestConvertHTTPRoutes_BackendTLSPolicy_NoResolverLeavesTLSNil(t *testing.T) {
	t.Parallel()

	pathPrefix := gatewayv1.PathMatchPathPrefix

	routes := []*gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "default-tls", Namespace: "default"},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						Matches: []gatewayv1.HTTPRouteMatch{
							{Path: &gatewayv1.HTTPPathMatch{Type: &pathPrefix, Value: new("/")}},
						},
						BackendRefs: []gatewayv1.HTTPBackendRef{backendRef("svc", 80, 1)},
					},
				},
			},
		},
	}

	cfg := proxy.ConvertHTTPRoutes(context.Background(), routes, "cluster.local", nil, nil, nil)

	require.Len(t, cfg.Rules, 1)
	require.Len(t, cfg.Rules[0].Backends, 1)
	assert.Nil(t, cfg.Rules[0].Backends[0].TLS, "no resolver means no TLS attached")
}

func TestConvertHTTPRoutes_BackendProtocolH2C_OnPort443_UsesHTTPScheme(t *testing.T) {
	t.Parallel()

	pathPrefix := gatewayv1.PathMatchPathPrefix

	// Port 443 normally produces an https:// backend URL. h2c is cleartext —
	// dialing plaintext against an https-scheme URL silently misbehaves.
	// The converter must force http:// when the backend is marked h2c.
	routes := []*gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "h2c-443", Namespace: "default"},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						Matches: []gatewayv1.HTTPRouteMatch{
							{Path: &gatewayv1.HTTPPathMatch{Type: &pathPrefix, Value: new("/")}},
						},
						BackendRefs: []gatewayv1.HTTPBackendRef{backendRef("grpc-svc", 443, 1)},
					},
				},
			},
		},
	}

	resolver := func(_ context.Context, _, _ string, _ int32) string { return "kubernetes.io/h2c" }

	cfg := proxy.ConvertHTTPRoutes(context.Background(), routes, "cluster.local", nil, resolver, nil)

	require.Len(t, cfg.Rules, 1)
	require.Len(t, cfg.Rules[0].Backends, 1)
	assert.Equal(t, proxy.BackendProtocolH2C, cfg.Rules[0].Backends[0].Protocol)
	assert.True(t, strings.HasPrefix(cfg.Rules[0].Backends[0].URL, "http://"),
		"h2c backend on port 443 must use http:// scheme, got %q", cfg.Rules[0].Backends[0].URL)
}

func TestConvertHTTPRoutes_NoProtocolResolver_DefaultsHTTP(t *testing.T) {
	t.Parallel()

	pathPrefix := gatewayv1.PathMatchPathPrefix

	routes := []*gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "plain", Namespace: "default"},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						Matches: []gatewayv1.HTTPRouteMatch{
							{Path: &gatewayv1.HTTPPathMatch{Type: &pathPrefix, Value: new("/")}},
						},
						BackendRefs: []gatewayv1.HTTPBackendRef{backendRef("web-svc", 80, 1)},
					},
				},
			},
		},
	}

	cfg := proxy.ConvertHTTPRoutes(context.Background(), routes, "cluster.local", nil, nil, nil)

	require.Len(t, cfg.Rules, 1)
	require.Len(t, cfg.Rules[0].Backends, 1)
	assert.Equal(t, proxy.BackendProtocolHTTP, cfg.Rules[0].Backends[0].Protocol)
}

func TestConvertHTTPRoutes_MultipleHostnames(t *testing.T) {
	t.Parallel()

	pathPrefix := gatewayv1.PathMatchPathPrefix

	routes := []*gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "multi", Namespace: "default"},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"app.example.com", "app.example.org"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						Matches: []gatewayv1.HTTPRouteMatch{
							{Path: &gatewayv1.HTTPPathMatch{Type: &pathPrefix, Value: new("/")}},
						},
						BackendRefs: []gatewayv1.HTTPBackendRef{
							backendRef("app-svc", 80, 1),
						},
					},
				},
			},
		},
	}

	cfg := proxy.ConvertHTTPRoutes(context.Background(), routes, "cluster.local", nil, nil, nil)

	require.Len(t, cfg.Rules, 1)
	assert.Equal(t, []string{"app.example.com", "app.example.org"}, cfg.Rules[0].Hostnames)
}

func TestConvertHTTPRoutes_Filters(t *testing.T) {
	t.Parallel()

	pathPrefix := gatewayv1.PathMatchPathPrefix
	scheme := "https"
	statusCode := 301

	routes := []*gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "redirect", Namespace: "default"},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						Matches: []gatewayv1.HTTPRouteMatch{
							{Path: &gatewayv1.HTTPPathMatch{Type: &pathPrefix, Value: new("/")}},
						},
						Filters: []gatewayv1.HTTPRouteFilter{
							{
								Type: gatewayv1.HTTPRouteFilterRequestRedirect,
								RequestRedirect: &gatewayv1.HTTPRequestRedirectFilter{
									Scheme:     &scheme,
									StatusCode: &statusCode,
								},
							},
						},
						BackendRefs: []gatewayv1.HTTPBackendRef{
							backendRef("svc", 80, 1),
						},
					},
				},
			},
		},
	}

	cfg := proxy.ConvertHTTPRoutes(context.Background(), routes, "cluster.local", nil, nil, nil)

	require.Len(t, cfg.Rules, 1)
	require.Len(t, cfg.Rules[0].Filters, 1)
	assert.Equal(t, proxy.FilterRequestRedirect, cfg.Rules[0].Filters[0].Type)
	require.NotNil(t, cfg.Rules[0].Filters[0].RequestRedirect)
	assert.Equal(t, "https", *cfg.Rules[0].Filters[0].RequestRedirect.Scheme)
}

func TestConvertHTTPRoutes_HeaderMatch(t *testing.T) {
	t.Parallel()

	pathPrefix := gatewayv1.PathMatchPathPrefix
	headerExact := gatewayv1.HeaderMatchExact
	methodGet := gatewayv1.HTTPMethodGet

	routes := []*gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "header-match", Namespace: "default"},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						Matches: []gatewayv1.HTTPRouteMatch{
							{
								Path:   &gatewayv1.HTTPPathMatch{Type: &pathPrefix, Value: new("/api")},
								Method: &methodGet,
								Headers: []gatewayv1.HTTPHeaderMatch{
									{
										Type:  &headerExact,
										Name:  "X-Env",
										Value: "prod",
									},
								},
							},
						},
						BackendRefs: []gatewayv1.HTTPBackendRef{
							backendRef("api-svc", 80, 1),
						},
					},
				},
			},
		},
	}

	cfg := proxy.ConvertHTTPRoutes(context.Background(), routes, "cluster.local", nil, nil, nil)

	require.Len(t, cfg.Rules, 1)
	require.Len(t, cfg.Rules[0].Matches, 1)

	match := cfg.Rules[0].Matches[0]
	assert.Equal(t, "GET", match.Method)
	require.Len(t, match.Headers, 1)
	assert.Equal(t, "X-Env", match.Headers[0].Name)
	assert.Equal(t, "prod", match.Headers[0].Value)
}

func TestConvertHTTPRoutes_WeightedBackends(t *testing.T) {
	t.Parallel()

	pathPrefix := gatewayv1.PathMatchPathPrefix

	routes := []*gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "canary", Namespace: "default"},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						Matches: []gatewayv1.HTTPRouteMatch{
							{Path: &gatewayv1.HTTPPathMatch{Type: &pathPrefix, Value: new("/")}},
						},
						BackendRefs: []gatewayv1.HTTPBackendRef{
							backendRef("stable", 80, 80),
							backendRef("canary", 80, 20),
						},
					},
				},
			},
		},
	}

	cfg := proxy.ConvertHTTPRoutes(context.Background(), routes, "cluster.local", nil, nil, nil)

	require.Len(t, cfg.Rules, 1)
	require.Len(t, cfg.Rules[0].Backends, 2)
	assert.Equal(t, int32(80), cfg.Rules[0].Backends[0].Weight)
	assert.Equal(t, int32(20), cfg.Rules[0].Backends[1].Weight)
}

func TestConvertHTTPRoutes_NoHostnames(t *testing.T) {
	t.Parallel()

	pathPrefix := gatewayv1.PathMatchPathPrefix

	routes := []*gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "catch-all", Namespace: "default"},
			Spec: gatewayv1.HTTPRouteSpec{
				Rules: []gatewayv1.HTTPRouteRule{
					{
						Matches: []gatewayv1.HTTPRouteMatch{
							{Path: &gatewayv1.HTTPPathMatch{Type: &pathPrefix, Value: new("/")}},
						},
						BackendRefs: []gatewayv1.HTTPBackendRef{
							backendRef("default-svc", 80, 1),
						},
					},
				},
			},
		},
	}

	cfg := proxy.ConvertHTTPRoutes(context.Background(), routes, "cluster.local", nil, nil, nil)

	require.Len(t, cfg.Rules, 1)
	assert.Empty(t, cfg.Rules[0].Hostnames, "no hostnames means catch-all")
}

func TestConvertHTTPRoutes_Empty(t *testing.T) {
	t.Parallel()

	cfg := proxy.ConvertHTTPRoutes(context.Background(), nil, "cluster.local", nil, nil, nil)

	assert.Empty(t, cfg.Rules)
	assert.True(t, cfg.Version > 0, "version should be positive")
}

func TestConvertHTTPRoutes_NonServiceBackendSkipped(t *testing.T) {
	t.Parallel()

	// Route with a non-Service backend (e.g., kind=NonExistent) should produce
	// a rule with empty backends — per Gateway API spec, unresolvable backend
	// refs must return HTTP 500 (the proxy handler returns 500 for empty backends).
	nonExistentKind := gatewayv1.Kind("NonExistent")
	routes := []*gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "bad-backend", Namespace: "default"},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"app.example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						BackendRefs: []gatewayv1.HTTPBackendRef{
							{
								BackendRef: gatewayv1.BackendRef{
									BackendObjectReference: gatewayv1.BackendObjectReference{
										Kind: &nonExistentKind,
										Name: "some-backend",
										Port: new(gatewayv1.PortNumber(8080)),
									},
								},
							},
						},
					},
				},
			},
		},
	}

	cfg := proxy.ConvertHTTPRoutes(context.Background(), routes, "cluster.local", nil, nil, nil)

	require.Len(t, cfg.Rules, 1, "rule with unresolvable backend should still be present for 500 response")
	assert.Empty(t, cfg.Rules[0].Backends, "non-Service backend should not be in backends list")
}

func TestConvertQueryMatch(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		route    *gatewayv1.HTTPRoute
		expected proxy.QueryParamMatch
	}{
		{
			name: "exact match with explicit type",
			route: routeWithQueryMatch(gatewayv1.HTTPQueryParamMatch{
				Type:  new(gatewayv1.QueryParamMatchExact),
				Name:  "page",
				Value: "home",
			}),
			expected: proxy.QueryParamMatch{
				Type:  proxy.QueryParamMatchExact,
				Name:  "page",
				Value: "home",
			},
		},
		{
			name: "regex match",
			route: routeWithQueryMatch(gatewayv1.HTTPQueryParamMatch{
				Type:  new(gatewayv1.QueryParamMatchRegularExpression),
				Name:  "id",
				Value: "[0-9]+",
			}),
			expected: proxy.QueryParamMatch{
				Type:  proxy.QueryParamMatchRegularExpression,
				Name:  "id",
				Value: "[0-9]+",
			},
		},
		{
			name: "nil type defaults to exact",
			route: routeWithQueryMatch(gatewayv1.HTTPQueryParamMatch{
				Name:  "key",
				Value: "val",
			}),
			expected: proxy.QueryParamMatch{
				Type:  proxy.QueryParamMatchExact,
				Name:  "key",
				Value: "val",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := proxy.ConvertHTTPRoutes(context.Background(), []*gatewayv1.HTTPRoute{tt.route}, "cluster.local", nil, nil, nil)

			require.Len(t, cfg.Rules, 1)
			require.Len(t, cfg.Rules[0].Matches, 1)
			require.Len(t, cfg.Rules[0].Matches[0].QueryParams, 1)
			assert.Equal(t, tt.expected, cfg.Rules[0].Matches[0].QueryParams[0])
		})
	}
}

func TestConvertFilter_RequestHeaderModifier(t *testing.T) {
	t.Parallel()

	route := routeWithFilter(gatewayv1.HTTPRouteFilter{
		Type: gatewayv1.HTTPRouteFilterRequestHeaderModifier,
		RequestHeaderModifier: &gatewayv1.HTTPHeaderFilter{
			Set: []gatewayv1.HTTPHeader{
				{Name: "X-Custom", Value: "set-value"},
			},
			Add: []gatewayv1.HTTPHeader{
				{Name: "X-Added", Value: "add-value"},
			},
			Remove: []string{"X-Remove-Me"},
		},
	})

	cfg := proxy.ConvertHTTPRoutes(context.Background(), []*gatewayv1.HTTPRoute{route}, "cluster.local", nil, nil, nil)

	require.Len(t, cfg.Rules, 1)
	require.Len(t, cfg.Rules[0].Filters, 1)

	f := cfg.Rules[0].Filters[0]
	assert.Equal(t, proxy.FilterRequestHeaderModifier, f.Type)
	require.NotNil(t, f.RequestHeaderModifier)
	assert.Equal(t, []proxy.HeaderValue{{Name: "X-Custom", Value: "set-value"}}, f.RequestHeaderModifier.Set)
	assert.Equal(t, []proxy.HeaderValue{{Name: "X-Added", Value: "add-value"}}, f.RequestHeaderModifier.Add)
	assert.Equal(t, []string{"X-Remove-Me"}, f.RequestHeaderModifier.Remove)
}

func TestConvertFilter_RequestHeaderModifier_NilBody(t *testing.T) {
	t.Parallel()

	route := routeWithFilter(gatewayv1.HTTPRouteFilter{
		Type:                  gatewayv1.HTTPRouteFilterRequestHeaderModifier,
		RequestHeaderModifier: nil,
	})

	cfg := proxy.ConvertHTTPRoutes(context.Background(), []*gatewayv1.HTTPRoute{route}, "cluster.local", nil, nil, nil)

	require.Len(t, cfg.Rules, 1)
	assert.Empty(t, cfg.Rules[0].Filters)
}

func TestConvertFilter_ResponseHeaderModifier(t *testing.T) {
	t.Parallel()

	route := routeWithFilter(gatewayv1.HTTPRouteFilter{
		Type: gatewayv1.HTTPRouteFilterResponseHeaderModifier,
		ResponseHeaderModifier: &gatewayv1.HTTPHeaderFilter{
			Set: []gatewayv1.HTTPHeader{
				{Name: "Cache-Control", Value: "no-store"},
			},
		},
	})

	cfg := proxy.ConvertHTTPRoutes(context.Background(), []*gatewayv1.HTTPRoute{route}, "cluster.local", nil, nil, nil)

	require.Len(t, cfg.Rules, 1)
	require.Len(t, cfg.Rules[0].Filters, 1)

	f := cfg.Rules[0].Filters[0]
	assert.Equal(t, proxy.FilterResponseHeaderModifier, f.Type)
	require.NotNil(t, f.ResponseHeaderModifier)
	assert.Equal(t, []proxy.HeaderValue{{Name: "Cache-Control", Value: "no-store"}}, f.ResponseHeaderModifier.Set)
}

func TestConvertFilter_ResponseHeaderModifier_NilBody(t *testing.T) {
	t.Parallel()

	route := routeWithFilter(gatewayv1.HTTPRouteFilter{
		Type:                   gatewayv1.HTTPRouteFilterResponseHeaderModifier,
		ResponseHeaderModifier: nil,
	})

	cfg := proxy.ConvertHTTPRoutes(context.Background(), []*gatewayv1.HTTPRoute{route}, "cluster.local", nil, nil, nil)

	require.Len(t, cfg.Rules, 1)
	assert.Empty(t, cfg.Rules[0].Filters)
}

func TestConvertFilter_RequestRedirect_Full(t *testing.T) {
	t.Parallel()

	hostname := gatewayv1.PreciseHostname("new.example.com")
	port := gatewayv1.PortNumber(443)

	route := routeWithFilter(gatewayv1.HTTPRouteFilter{
		Type: gatewayv1.HTTPRouteFilterRequestRedirect,
		RequestRedirect: &gatewayv1.HTTPRequestRedirectFilter{
			Scheme:   new("https"),
			Hostname: &hostname,
			Port:     &port,
			Path: &gatewayv1.HTTPPathModifier{
				Type:            gatewayv1.FullPathHTTPPathModifier,
				ReplaceFullPath: new("/new-path"),
			},
			StatusCode: new(301),
		},
	})

	cfg := proxy.ConvertHTTPRoutes(context.Background(), []*gatewayv1.HTTPRoute{route}, "cluster.local", nil, nil, nil)

	require.Len(t, cfg.Rules, 1)
	require.Len(t, cfg.Rules[0].Filters, 1)

	f := cfg.Rules[0].Filters[0]
	assert.Equal(t, proxy.FilterRequestRedirect, f.Type)
	require.NotNil(t, f.RequestRedirect)
	assert.Equal(t, "https", *f.RequestRedirect.Scheme)
	assert.Equal(t, "new.example.com", *f.RequestRedirect.Hostname)
	assert.Equal(t, int32(443), *f.RequestRedirect.Port)
	require.NotNil(t, f.RequestRedirect.Path)
	assert.Equal(t, proxy.RedirectPathFullReplace, f.RequestRedirect.Path.Type)
	assert.Equal(t, "/new-path", f.RequestRedirect.Path.Value)
	assert.Equal(t, 301, *f.RequestRedirect.StatusCode)
}

func TestConvertFilter_RequestRedirect_NilBody(t *testing.T) {
	t.Parallel()

	route := routeWithFilter(gatewayv1.HTTPRouteFilter{
		Type:            gatewayv1.HTTPRouteFilterRequestRedirect,
		RequestRedirect: nil,
	})

	cfg := proxy.ConvertHTTPRoutes(context.Background(), []*gatewayv1.HTTPRoute{route}, "cluster.local", nil, nil, nil)

	require.Len(t, cfg.Rules, 1)
	assert.Empty(t, cfg.Rules[0].Filters)
}

func TestConvertFilter_URLRewrite(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		rewrite          *gatewayv1.HTTPURLRewriteFilter
		expectedHostname *string
		expectedPath     *proxy.URLRewritePath
	}{
		{
			name: "hostname only",
			rewrite: &gatewayv1.HTTPURLRewriteFilter{
				Hostname: (*gatewayv1.PreciseHostname)(new("rewritten.example.com")),
			},
			expectedHostname: new("rewritten.example.com"),
			expectedPath:     nil,
		},
		{
			name: "full path rewrite",
			rewrite: &gatewayv1.HTTPURLRewriteFilter{
				Path: &gatewayv1.HTTPPathModifier{
					Type:            gatewayv1.FullPathHTTPPathModifier,
					ReplaceFullPath: new("/replaced"),
				},
			},
			expectedHostname: nil,
			expectedPath: &proxy.URLRewritePath{
				Type:            proxy.URLRewriteFullPath,
				ReplaceFullPath: new("/replaced"),
			},
		},
		{
			name: "prefix match rewrite",
			rewrite: &gatewayv1.HTTPURLRewriteFilter{
				Path: &gatewayv1.HTTPPathModifier{
					Type:               gatewayv1.PrefixMatchHTTPPathModifier,
					ReplacePrefixMatch: new("/new-prefix"),
				},
			},
			expectedHostname: nil,
			expectedPath: &proxy.URLRewritePath{
				Type:               proxy.URLRewritePrefixMatch,
				ReplacePrefixMatch: new("/new-prefix"),
			},
		},
		{
			name: "hostname and path",
			rewrite: &gatewayv1.HTTPURLRewriteFilter{
				Hostname: (*gatewayv1.PreciseHostname)(new("host.example.com")),
				Path: &gatewayv1.HTTPPathModifier{
					Type:            gatewayv1.FullPathHTTPPathModifier,
					ReplaceFullPath: new("/full"),
				},
			},
			expectedHostname: new("host.example.com"),
			expectedPath: &proxy.URLRewritePath{
				Type:            proxy.URLRewriteFullPath,
				ReplaceFullPath: new("/full"),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			route := routeWithFilter(gatewayv1.HTTPRouteFilter{
				Type:       gatewayv1.HTTPRouteFilterURLRewrite,
				URLRewrite: tt.rewrite,
			})

			cfg := proxy.ConvertHTTPRoutes(context.Background(), []*gatewayv1.HTTPRoute{route}, "cluster.local", nil, nil, nil)

			require.Len(t, cfg.Rules, 1)
			require.Len(t, cfg.Rules[0].Filters, 1)

			f := cfg.Rules[0].Filters[0]
			assert.Equal(t, proxy.FilterURLRewrite, f.Type)
			require.NotNil(t, f.URLRewrite)

			if tt.expectedHostname != nil {
				require.NotNil(t, f.URLRewrite.Hostname)
				assert.Equal(t, *tt.expectedHostname, *f.URLRewrite.Hostname)
			} else {
				assert.Nil(t, f.URLRewrite.Hostname)
			}

			if tt.expectedPath != nil {
				require.NotNil(t, f.URLRewrite.Path)
				assert.Equal(t, tt.expectedPath.Type, f.URLRewrite.Path.Type)

				if tt.expectedPath.ReplaceFullPath != nil {
					require.NotNil(t, f.URLRewrite.Path.ReplaceFullPath)
					assert.Equal(t, *tt.expectedPath.ReplaceFullPath, *f.URLRewrite.Path.ReplaceFullPath)
				}

				if tt.expectedPath.ReplacePrefixMatch != nil {
					require.NotNil(t, f.URLRewrite.Path.ReplacePrefixMatch)
					assert.Equal(t, *tt.expectedPath.ReplacePrefixMatch, *f.URLRewrite.Path.ReplacePrefixMatch)
				}
			} else {
				assert.Nil(t, f.URLRewrite.Path)
			}
		})
	}
}

func TestConvertFilter_URLRewrite_NilBody(t *testing.T) {
	t.Parallel()

	route := routeWithFilter(gatewayv1.HTTPRouteFilter{
		Type:       gatewayv1.HTTPRouteFilterURLRewrite,
		URLRewrite: nil,
	})

	cfg := proxy.ConvertHTTPRoutes(context.Background(), []*gatewayv1.HTTPRoute{route}, "cluster.local", nil, nil, nil)

	require.Len(t, cfg.Rules, 1)
	assert.Empty(t, cfg.Rules[0].Filters)
}

func TestConvertFilter_RequestMirror(t *testing.T) {
	t.Parallel()

	route := routeWithFilter(gatewayv1.HTTPRouteFilter{
		Type: gatewayv1.HTTPRouteFilterRequestMirror,
		RequestMirror: &gatewayv1.HTTPRequestMirrorFilter{
			BackendRef: gatewayv1.BackendObjectReference{
				Name: "mirror-svc",
			},
		},
	})

	cfg := proxy.ConvertHTTPRoutes(context.Background(), []*gatewayv1.HTTPRoute{route}, "cluster.local", nil, nil, nil)

	require.Len(t, cfg.Rules, 1)
	require.Len(t, cfg.Rules[0].Filters, 1)

	f := cfg.Rules[0].Filters[0]
	assert.Equal(t, proxy.FilterRequestMirror, f.Type)
	require.NotNil(t, f.RequestMirror)
	assert.Equal(t, "http://mirror-svc.default.svc.cluster.local:80", f.RequestMirror.BackendURL)
}

func TestConvertFilter_RequestMirror_NilBody(t *testing.T) {
	t.Parallel()

	route := routeWithFilter(gatewayv1.HTTPRouteFilter{
		Type:          gatewayv1.HTTPRouteFilterRequestMirror,
		RequestMirror: nil,
	})

	cfg := proxy.ConvertHTTPRoutes(context.Background(), []*gatewayv1.HTTPRoute{route}, "cluster.local", nil, nil, nil)

	require.Len(t, cfg.Rules, 1)
	assert.Empty(t, cfg.Rules[0].Filters)
}

func TestConvertFilter_UnsupportedTypes(t *testing.T) {
	t.Parallel()

	unsupportedTypes := []gatewayv1.HTTPRouteFilterType{
		gatewayv1.HTTPRouteFilterExtensionRef,
	}

	for _, filterType := range unsupportedTypes {
		t.Run(string(filterType), func(t *testing.T) {
			t.Parallel()

			route := routeWithFilter(gatewayv1.HTTPRouteFilter{
				Type: filterType,
			})

			cfg := proxy.ConvertHTTPRoutes(context.Background(), []*gatewayv1.HTTPRoute{route}, "cluster.local", nil, nil, nil)

			require.Len(t, cfg.Rules, 1)
			assert.Empty(t, cfg.Rules[0].Filters)
		})
	}
}

func TestConvertHeaderModifier_AllOperations(t *testing.T) {
	t.Parallel()

	route := routeWithFilter(gatewayv1.HTTPRouteFilter{
		Type: gatewayv1.HTTPRouteFilterRequestHeaderModifier,
		RequestHeaderModifier: &gatewayv1.HTTPHeaderFilter{
			Set: []gatewayv1.HTTPHeader{
				{Name: "X-Set-One", Value: "one"},
				{Name: "X-Set-Two", Value: "two"},
			},
			Add: []gatewayv1.HTTPHeader{
				{Name: "X-Add-One", Value: "added"},
			},
			Remove: []string{"X-Del-One", "X-Del-Two"},
		},
	})

	cfg := proxy.ConvertHTTPRoutes(context.Background(), []*gatewayv1.HTTPRoute{route}, "cluster.local", nil, nil, nil)

	require.Len(t, cfg.Rules, 1)
	require.Len(t, cfg.Rules[0].Filters, 1)

	mod := cfg.Rules[0].Filters[0].RequestHeaderModifier
	require.NotNil(t, mod)
	assert.Len(t, mod.Set, 2)
	assert.Len(t, mod.Add, 1)
	assert.Len(t, mod.Remove, 2)
	assert.Equal(t, "X-Set-One", mod.Set[0].Name)
	assert.Equal(t, "X-Add-One", mod.Add[0].Name)
	assert.Equal(t, "X-Del-One", mod.Remove[0])
}

func TestConvertHeaderModifier_Empty(t *testing.T) {
	t.Parallel()

	route := routeWithFilter(gatewayv1.HTTPRouteFilter{
		Type:                  gatewayv1.HTTPRouteFilterRequestHeaderModifier,
		RequestHeaderModifier: &gatewayv1.HTTPHeaderFilter{},
	})

	cfg := proxy.ConvertHTTPRoutes(context.Background(), []*gatewayv1.HTTPRoute{route}, "cluster.local", nil, nil, nil)

	require.Len(t, cfg.Rules, 1)
	require.Len(t, cfg.Rules[0].Filters, 1)

	mod := cfg.Rules[0].Filters[0].RequestHeaderModifier
	require.NotNil(t, mod)
	assert.Empty(t, mod.Set)
	assert.Empty(t, mod.Add)
	assert.Empty(t, mod.Remove)
}

func TestConvertRedirectPath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		pathModifier *gatewayv1.HTTPPathModifier
		expectedType proxy.RedirectPathType
		expectedVal  string
	}{
		{
			name: "full path replacement",
			pathModifier: &gatewayv1.HTTPPathModifier{
				Type:            gatewayv1.FullPathHTTPPathModifier,
				ReplaceFullPath: new("/new-full-path"),
			},
			expectedType: proxy.RedirectPathFullReplace,
			expectedVal:  "/new-full-path",
		},
		{
			name: "prefix match replacement",
			pathModifier: &gatewayv1.HTTPPathModifier{
				Type:               gatewayv1.PrefixMatchHTTPPathModifier,
				ReplacePrefixMatch: new("/new-prefix"),
			},
			expectedType: proxy.RedirectPathPrefixReplace,
			expectedVal:  "/new-prefix",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			route := routeWithFilter(gatewayv1.HTTPRouteFilter{
				Type: gatewayv1.HTTPRouteFilterRequestRedirect,
				RequestRedirect: &gatewayv1.HTTPRequestRedirectFilter{
					Path: tt.pathModifier,
				},
			})

			cfg := proxy.ConvertHTTPRoutes(context.Background(), []*gatewayv1.HTTPRoute{route}, "cluster.local", nil, nil, nil)

			require.Len(t, cfg.Rules, 1)
			require.Len(t, cfg.Rules[0].Filters, 1)
			require.NotNil(t, cfg.Rules[0].Filters[0].RequestRedirect)
			require.NotNil(t, cfg.Rules[0].Filters[0].RequestRedirect.Path)
			assert.Equal(t, tt.expectedType, cfg.Rules[0].Filters[0].RequestRedirect.Path.Type)
			assert.Equal(t, tt.expectedVal, cfg.Rules[0].Filters[0].RequestRedirect.Path.Value)
		})
	}
}

func TestConvertRedirectPath_PrefixMatch(t *testing.T) {
	t.Parallel()

	route := routeWithFilter(gatewayv1.HTTPRouteFilter{
		Type: gatewayv1.HTTPRouteFilterRequestRedirect,
		RequestRedirect: &gatewayv1.HTTPRequestRedirectFilter{
			Path: &gatewayv1.HTTPPathModifier{
				Type:               gatewayv1.PrefixMatchHTTPPathModifier,
				ReplacePrefixMatch: new("/v2"),
			},
		},
	})

	cfg := proxy.ConvertHTTPRoutes(context.Background(), []*gatewayv1.HTTPRoute{route}, "cluster.local", nil, nil, nil)

	require.Len(t, cfg.Rules, 1)
	require.Len(t, cfg.Rules[0].Filters, 1)

	redirect := cfg.Rules[0].Filters[0].RequestRedirect
	require.NotNil(t, redirect)
	require.NotNil(t, redirect.Path)
	assert.Equal(t, proxy.RedirectPathPrefixReplace, redirect.Path.Type,
		"path modifier type must be preserved through conversion")
	assert.Equal(t, "/v2", redirect.Path.Value)
}

func TestConvertRedirectPath_NilFields(t *testing.T) {
	t.Parallel()

	route := routeWithFilter(gatewayv1.HTTPRouteFilter{
		Type:            gatewayv1.HTTPRouteFilterRequestRedirect,
		RequestRedirect: &gatewayv1.HTTPRequestRedirectFilter{
			// No fields set at all.
		},
	})

	cfg := proxy.ConvertHTTPRoutes(context.Background(), []*gatewayv1.HTTPRoute{route}, "cluster.local", nil, nil, nil)

	require.Len(t, cfg.Rules, 1)
	require.Len(t, cfg.Rules[0].Filters, 1)

	redirect := cfg.Rules[0].Filters[0].RequestRedirect
	require.NotNil(t, redirect)
	assert.Nil(t, redirect.Scheme)
	assert.Nil(t, redirect.Hostname)
	assert.Nil(t, redirect.Port)
	assert.Nil(t, redirect.Path)
	assert.Nil(t, redirect.StatusCode)
}

func TestConvertURLRewritePath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		pathModifier *gatewayv1.HTTPPathModifier
		expectedType proxy.URLRewritePathType
		expectedVal  string
	}{
		{
			name: "full path rewrite",
			pathModifier: &gatewayv1.HTTPPathModifier{
				Type:            gatewayv1.FullPathHTTPPathModifier,
				ReplaceFullPath: new("/full-rewrite"),
			},
			expectedType: proxy.URLRewriteFullPath,
			expectedVal:  "/full-rewrite",
		},
		{
			name: "prefix match rewrite",
			pathModifier: &gatewayv1.HTTPPathModifier{
				Type:               gatewayv1.PrefixMatchHTTPPathModifier,
				ReplacePrefixMatch: new("/prefix-rewrite"),
			},
			expectedType: proxy.URLRewritePrefixMatch,
			expectedVal:  "/prefix-rewrite",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			route := routeWithFilter(gatewayv1.HTTPRouteFilter{
				Type: gatewayv1.HTTPRouteFilterURLRewrite,
				URLRewrite: &gatewayv1.HTTPURLRewriteFilter{
					Path: tt.pathModifier,
				},
			})

			cfg := proxy.ConvertHTTPRoutes(context.Background(), []*gatewayv1.HTTPRoute{route}, "cluster.local", nil, nil, nil)

			require.Len(t, cfg.Rules, 1)
			require.Len(t, cfg.Rules[0].Filters, 1)

			rewritePath := cfg.Rules[0].Filters[0].URLRewrite.Path
			require.NotNil(t, rewritePath)
			assert.Equal(t, tt.expectedType, rewritePath.Type)

			if tt.expectedType == proxy.URLRewriteFullPath {
				require.NotNil(t, rewritePath.ReplaceFullPath)
				assert.Equal(t, tt.expectedVal, *rewritePath.ReplaceFullPath)
			} else {
				require.NotNil(t, rewritePath.ReplacePrefixMatch)
				assert.Equal(t, tt.expectedVal, *rewritePath.ReplacePrefixMatch)
			}
		})
	}
}

func TestConvertTimeouts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		timeouts        *gatewayv1.HTTPRouteTimeouts
		expectNil       bool
		expectedRequest time.Duration
		expectedBackend time.Duration
	}{
		{
			name: "both timeouts set",
			timeouts: &gatewayv1.HTTPRouteTimeouts{
				Request:        durationPtr("10s"),
				BackendRequest: durationPtr("5s"),
			},
			expectedRequest: 10 * time.Second,
			expectedBackend: 5 * time.Second,
		},
		{
			name: "request timeout only",
			timeouts: &gatewayv1.HTTPRouteTimeouts{
				Request: durationPtr("30s"),
			},
			expectedRequest: 30 * time.Second,
			expectedBackend: 0,
		},
		{
			name: "backend timeout only",
			timeouts: &gatewayv1.HTTPRouteTimeouts{
				BackendRequest: durationPtr("15s"),
			},
			expectedRequest: 0,
			expectedBackend: 15 * time.Second,
		},
		{
			name: "invalid request timeout drops all timeouts",
			timeouts: &gatewayv1.HTTPRouteTimeouts{
				Request:        durationPtr("not-a-duration"),
				BackendRequest: durationPtr("5s"),
			},
			expectNil: true,
		},
		{
			name: "invalid backend timeout drops all timeouts",
			timeouts: &gatewayv1.HTTPRouteTimeouts{
				Request:        durationPtr("10s"),
				BackendRequest: durationPtr("garbage"),
			},
			expectNil: true,
		},
		{
			name: "millisecond precision",
			timeouts: &gatewayv1.HTTPRouteTimeouts{
				Request: durationPtr("500ms"),
			},
			expectedRequest: 500 * time.Millisecond,
			expectedBackend: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			route := routeWithTimeouts(tt.timeouts)

			cfg := proxy.ConvertHTTPRoutes(context.Background(), []*gatewayv1.HTTPRoute{route}, "cluster.local", nil, nil, nil)

			require.Len(t, cfg.Rules, 1)

			if tt.expectNil {
				assert.Nil(t, cfg.Rules[0].Timeouts)
			} else {
				require.NotNil(t, cfg.Rules[0].Timeouts)
				assert.Equal(t, tt.expectedRequest, cfg.Rules[0].Timeouts.Request)
				assert.Equal(t, tt.expectedBackend, cfg.Rules[0].Timeouts.Backend)
			}
		})
	}
}

func TestConvertTimeouts_Nil(t *testing.T) {
	t.Parallel()

	pathPrefix := gatewayv1.PathMatchPathPrefix

	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "no-timeouts", Namespace: "default"},
		Spec: gatewayv1.HTTPRouteSpec{
			Rules: []gatewayv1.HTTPRouteRule{
				{
					Matches: []gatewayv1.HTTPRouteMatch{
						{Path: &gatewayv1.HTTPPathMatch{Type: &pathPrefix, Value: new("/")}},
					},
					BackendRefs: []gatewayv1.HTTPBackendRef{
						backendRef("svc", 80, 1),
					},
				},
			},
		},
	}

	cfg := proxy.ConvertHTTPRoutes(context.Background(), []*gatewayv1.HTTPRoute{route}, "cluster.local", nil, nil, nil)

	require.Len(t, cfg.Rules, 1)
	assert.Nil(t, cfg.Rules[0].Timeouts)
}

func TestConvertHeaderMatch_EdgeCases(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		header   gatewayv1.HTTPHeaderMatch
		expected proxy.HeaderMatch
	}{
		{
			name: "nil type defaults to exact",
			header: gatewayv1.HTTPHeaderMatch{
				Name:  "X-Test",
				Value: "value",
			},
			expected: proxy.HeaderMatch{
				Type:  proxy.HeaderMatchExact,
				Name:  "X-Test",
				Value: "value",
			},
		},
		{
			name: "regex type",
			header: gatewayv1.HTTPHeaderMatch{
				Type:  new(gatewayv1.HeaderMatchRegularExpression),
				Name:  "X-Pattern",
				Value: "v[0-9]+",
			},
			expected: proxy.HeaderMatch{
				Type:  proxy.HeaderMatchRegularExpression,
				Name:  "X-Pattern",
				Value: "v[0-9]+",
			},
		},
		{
			name: "explicit exact type",
			header: gatewayv1.HTTPHeaderMatch{
				Type:  new(gatewayv1.HeaderMatchExact),
				Name:  "Content-Type",
				Value: "application/json",
			},
			expected: proxy.HeaderMatch{
				Type:  proxy.HeaderMatchExact,
				Name:  "Content-Type",
				Value: "application/json",
			},
		},
		{
			name: "empty value",
			header: gatewayv1.HTTPHeaderMatch{
				Name:  "X-Empty",
				Value: "",
			},
			expected: proxy.HeaderMatch{
				Type:  proxy.HeaderMatchExact,
				Name:  "X-Empty",
				Value: "",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			route := routeWithHeaderMatch(tt.header)

			cfg := proxy.ConvertHTTPRoutes(context.Background(), []*gatewayv1.HTTPRoute{route}, "cluster.local", nil, nil, nil)

			require.Len(t, cfg.Rules, 1)
			require.Len(t, cfg.Rules[0].Matches, 1)
			require.Len(t, cfg.Rules[0].Matches[0].Headers, 1)
			assert.Equal(t, tt.expected, cfg.Rules[0].Matches[0].Headers[0])
		})
	}
}

func TestConvertBackendRef_InvalidPort(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		port          int
		expectSkipped bool
	}{
		{name: "port zero", port: 0, expectSkipped: true},
		{name: "negative port", port: -1, expectSkipped: true},
		{name: "port exceeds max", port: 65536, expectSkipped: true},
		{name: "valid port min", port: 1, expectSkipped: false},
		{name: "valid port max", port: 65535, expectSkipped: false},
		{name: "valid port common", port: 8080, expectSkipped: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			pathPrefix := gatewayv1.PathMatchPathPrefix

			route := &gatewayv1.HTTPRoute{
				ObjectMeta: metav1.ObjectMeta{Name: "port-test", Namespace: "default"},
				Spec: gatewayv1.HTTPRouteSpec{
					Hostnames: []gatewayv1.Hostname{"example.com"},
					Rules: []gatewayv1.HTTPRouteRule{
						{
							Matches: []gatewayv1.HTTPRouteMatch{
								{Path: &gatewayv1.HTTPPathMatch{Type: &pathPrefix, Value: new("/")}},
							},
							BackendRefs: []gatewayv1.HTTPBackendRef{
								backendRef("svc", tt.port, 1),
							},
						},
					},
				},
			}

			cfg := proxy.ConvertHTTPRoutes(context.Background(), []*gatewayv1.HTTPRoute{route}, "cluster.local", nil, nil, nil)

			if tt.expectSkipped {
				require.Len(t, cfg.Rules, 1, "rule with invalid backend should still be present for 500 response")
				assert.Empty(t, cfg.Rules[0].Backends, "invalid backend should not be in backends list")
			} else {
				require.Len(t, cfg.Rules, 1, "rule with valid backend should be kept")
				require.Len(t, cfg.Rules[0].Backends, 1, "backend with valid port should be kept")
			}
		})
	}
}

func TestConvertMirrorFilter_InvalidPort(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		port          int
		expectSkipped bool
	}{
		{name: "port zero", port: 0, expectSkipped: true},
		{name: "negative port", port: -1, expectSkipped: true},
		{name: "port exceeds max", port: 65536, expectSkipped: true},
		{name: "valid port", port: 8080, expectSkipped: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			portNum := gatewayv1.PortNumber(tt.port)

			route := routeWithFilter(gatewayv1.HTTPRouteFilter{
				Type: gatewayv1.HTTPRouteFilterRequestMirror,
				RequestMirror: &gatewayv1.HTTPRequestMirrorFilter{
					BackendRef: gatewayv1.BackendObjectReference{
						Name: "mirror-svc",
						Port: &portNum,
					},
				},
			})

			cfg := proxy.ConvertHTTPRoutes(context.Background(), []*gatewayv1.HTTPRoute{route}, "cluster.local", nil, nil, nil)

			require.Len(t, cfg.Rules, 1)

			if tt.expectSkipped {
				assert.Empty(t, cfg.Rules[0].Filters, "mirror filter with invalid port should be skipped")
			} else {
				require.Len(t, cfg.Rules[0].Filters, 1, "mirror filter with valid port should be kept")
				assert.Equal(t, proxy.FilterRequestMirror, cfg.Rules[0].Filters[0].Type)
			}
		})
	}
}

func TestConvertBackendRef_NonServiceKind(t *testing.T) {
	t.Parallel()

	pathPrefix := gatewayv1.PathMatchPathPrefix
	bucketKind := gatewayv1.Kind("Bucket")
	bucketGroup := gatewayv1.Group("objectbucket.io")
	portNum := gatewayv1.PortNumber(80)
	weight := int32(1)

	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "bucket-backend", Namespace: "default"},
		Spec: gatewayv1.HTTPRouteSpec{
			Hostnames: []gatewayv1.Hostname{"example.com"},
			Rules: []gatewayv1.HTTPRouteRule{
				{
					Matches: []gatewayv1.HTTPRouteMatch{
						{Path: &gatewayv1.HTTPPathMatch{Type: &pathPrefix, Value: new("/")}},
					},
					BackendRefs: []gatewayv1.HTTPBackendRef{
						{
							BackendRef: gatewayv1.BackendRef{
								BackendObjectReference: gatewayv1.BackendObjectReference{
									Group: &bucketGroup,
									Kind:  &bucketKind,
									Name:  "my-bucket",
									Port:  &portNum,
								},
								Weight: &weight,
							},
						},
					},
				},
			},
		},
	}

	cfg := proxy.ConvertHTTPRoutes(context.Background(), []*gatewayv1.HTTPRoute{route}, "cluster.local", nil, nil, nil)

	require.Len(t, cfg.Rules, 1, "rule with unresolvable backend should still be present for 500 response")
	assert.Empty(t, cfg.Rules[0].Backends, "non-Service backend should not be in backends list")
}

func TestConvertMirrorFilter_NonServiceKind(t *testing.T) {
	t.Parallel()

	bucketKind := gatewayv1.Kind("Bucket")
	bucketGroup := gatewayv1.Group("objectbucket.io")

	route := routeWithFilter(gatewayv1.HTTPRouteFilter{
		Type: gatewayv1.HTTPRouteFilterRequestMirror,
		RequestMirror: &gatewayv1.HTTPRequestMirrorFilter{
			BackendRef: gatewayv1.BackendObjectReference{
				Group: &bucketGroup,
				Kind:  &bucketKind,
				Name:  "my-bucket",
			},
		},
	})

	cfg := proxy.ConvertHTTPRoutes(context.Background(), []*gatewayv1.HTTPRoute{route}, "cluster.local", nil, nil, nil)

	require.Len(t, cfg.Rules, 1)
	assert.Empty(t, cfg.Rules[0].Filters, "mirror filter with non-Service kind should be skipped")
}

func TestConvertBackendRef_CrossNamespaceRejected(t *testing.T) {
	t.Parallel()

	pathPrefix := gatewayv1.PathMatchPathPrefix
	otherNS := gatewayv1.Namespace("other-namespace")
	portNum := gatewayv1.PortNumber(80)
	weight := int32(1)

	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "cross-ns", Namespace: "default"},
		Spec: gatewayv1.HTTPRouteSpec{
			Hostnames: []gatewayv1.Hostname{"example.com"},
			Rules: []gatewayv1.HTTPRouteRule{
				{
					Matches: []gatewayv1.HTTPRouteMatch{
						{Path: &gatewayv1.HTTPPathMatch{Type: &pathPrefix, Value: new("/")}},
					},
					BackendRefs: []gatewayv1.HTTPBackendRef{
						{
							BackendRef: gatewayv1.BackendRef{
								BackendObjectReference: gatewayv1.BackendObjectReference{
									Name:      "secret-svc",
									Namespace: &otherNS,
									Port:      &portNum,
								},
								Weight: &weight,
							},
						},
					},
				},
			},
		},
	}

	// Validator that rejects all cross-namespace refs.
	rejectAll := func(_ context.Context, _ string, _ gatewayv1.BackendObjectReference) bool {
		return false
	}

	cfg := proxy.ConvertHTTPRoutes(context.Background(), []*gatewayv1.HTTPRoute{route}, "cluster.local", rejectAll, nil, nil)

	require.Len(t, cfg.Rules, 1, "rule with rejected cross-namespace backend should still be present for 500 response")
	assert.Empty(t, cfg.Rules[0].Backends, "rejected cross-namespace backend should not be in backends list")
}

func TestConvertBackendRef_CrossNamespaceAllowed(t *testing.T) {
	t.Parallel()

	pathPrefix := gatewayv1.PathMatchPathPrefix
	otherNS := gatewayv1.Namespace("other-namespace")
	portNum := gatewayv1.PortNumber(80)
	weight := int32(1)

	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "cross-ns", Namespace: "default"},
		Spec: gatewayv1.HTTPRouteSpec{
			Hostnames: []gatewayv1.Hostname{"example.com"},
			Rules: []gatewayv1.HTTPRouteRule{
				{
					Matches: []gatewayv1.HTTPRouteMatch{
						{Path: &gatewayv1.HTTPPathMatch{Type: &pathPrefix, Value: new("/")}},
					},
					BackendRefs: []gatewayv1.HTTPBackendRef{
						{
							BackendRef: gatewayv1.BackendRef{
								BackendObjectReference: gatewayv1.BackendObjectReference{
									Name:      "allowed-svc",
									Namespace: &otherNS,
									Port:      &portNum,
								},
								Weight: &weight,
							},
						},
					},
				},
			},
		},
	}

	// Validator that allows all cross-namespace refs.
	allowAll := func(_ context.Context, _ string, _ gatewayv1.BackendObjectReference) bool {
		return true
	}

	cfg := proxy.ConvertHTTPRoutes(context.Background(), []*gatewayv1.HTTPRoute{route}, "cluster.local", allowAll, nil, nil)

	require.Len(t, cfg.Rules, 1)
	require.Len(t, cfg.Rules[0].Backends, 1, "cross-namespace backend should be allowed by validator")
	assert.Contains(t, cfg.Rules[0].Backends[0].URL, "other-namespace")
}

func TestConvertBackendRef_SameNamespaceAlwaysAllowed(t *testing.T) {
	t.Parallel()

	pathPrefix := gatewayv1.PathMatchPathPrefix

	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "same-ns", Namespace: "default"},
		Spec: gatewayv1.HTTPRouteSpec{
			Hostnames: []gatewayv1.Hostname{"example.com"},
			Rules: []gatewayv1.HTTPRouteRule{
				{
					Matches: []gatewayv1.HTTPRouteMatch{
						{Path: &gatewayv1.HTTPPathMatch{Type: &pathPrefix, Value: new("/")}},
					},
					BackendRefs: []gatewayv1.HTTPBackendRef{
						backendRef("svc", 80, 1),
					},
				},
			},
		},
	}

	// Validator that rejects everything — should NOT be called for same-namespace refs.
	rejectAll := func(_ context.Context, _ string, _ gatewayv1.BackendObjectReference) bool {
		t.Fatal("validator should not be called for same-namespace refs")

		return false
	}

	cfg := proxy.ConvertHTTPRoutes(context.Background(), []*gatewayv1.HTTPRoute{route}, "cluster.local", rejectAll, nil, nil)

	require.Len(t, cfg.Rules, 1)
	require.Len(t, cfg.Rules[0].Backends, 1, "same-namespace backend should always be allowed")
}

func TestBuildServiceURL_SchemeByPort(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		port        int
		expectedURL string
	}{
		{
			name:        "port 443 uses https scheme",
			port:        443,
			expectedURL: "https://svc.default.svc.cluster.local:443",
		},
		{
			name:        "port 80 uses http scheme",
			port:        80,
			expectedURL: "http://svc.default.svc.cluster.local:80",
		},
		{
			name:        "port 8080 uses http scheme",
			port:        8080,
			expectedURL: "http://svc.default.svc.cluster.local:8080",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			route := &gatewayv1.HTTPRoute{
				ObjectMeta: metav1.ObjectMeta{Name: "scheme-test", Namespace: "default"},
				Spec: gatewayv1.HTTPRouteSpec{
					Hostnames: []gatewayv1.Hostname{"example.com"},
					Rules: []gatewayv1.HTTPRouteRule{
						{
							BackendRefs: []gatewayv1.HTTPBackendRef{
								backendRef("svc", tt.port, 1),
							},
						},
					},
				},
			}

			cfg := proxy.ConvertHTTPRoutes(context.Background(), []*gatewayv1.HTTPRoute{route}, "cluster.local", nil, nil, nil)

			require.Len(t, cfg.Rules, 1)
			require.Len(t, cfg.Rules[0].Backends, 1)
			assert.Equal(t, tt.expectedURL, cfg.Rules[0].Backends[0].URL)
		})
	}
}

func TestConvertBackendRef_NegativeWeight(t *testing.T) {
	t.Parallel()

	pathPrefix := gatewayv1.PathMatchPathPrefix

	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "neg-weight", Namespace: "default"},
		Spec: gatewayv1.HTTPRouteSpec{
			Hostnames: []gatewayv1.Hostname{"example.com"},
			Rules: []gatewayv1.HTTPRouteRule{
				{
					Matches: []gatewayv1.HTTPRouteMatch{
						{Path: &gatewayv1.HTTPPathMatch{Type: &pathPrefix, Value: new("/")}},
					},
					BackendRefs: []gatewayv1.HTTPBackendRef{
						backendRef("svc", 80, -1),
					},
				},
			},
		},
	}

	cfg := proxy.ConvertHTTPRoutes(context.Background(), []*gatewayv1.HTTPRoute{route}, "cluster.local", nil, nil, nil)

	require.Len(t, cfg.Rules, 1, "rule with unresolvable backend should still be present for 500 response")
	assert.Empty(t, cfg.Rules[0].Backends, "negative-weight backend should not be in backends list")
}

// Helper functions.

func routeWithQueryMatch(query gatewayv1.HTTPQueryParamMatch) *gatewayv1.HTTPRoute {
	pathPrefix := gatewayv1.PathMatchPathPrefix

	return &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "query-route", Namespace: "default"},
		Spec: gatewayv1.HTTPRouteSpec{
			Rules: []gatewayv1.HTTPRouteRule{
				{
					Matches: []gatewayv1.HTTPRouteMatch{
						{
							Path:        &gatewayv1.HTTPPathMatch{Type: &pathPrefix, Value: new("/")},
							QueryParams: []gatewayv1.HTTPQueryParamMatch{query},
						},
					},
					BackendRefs: []gatewayv1.HTTPBackendRef{
						backendRef("svc", 80, 1),
					},
				},
			},
		},
	}
}

func routeWithFilter(filter gatewayv1.HTTPRouteFilter) *gatewayv1.HTTPRoute {
	pathPrefix := gatewayv1.PathMatchPathPrefix

	return &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "filter-route", Namespace: "default"},
		Spec: gatewayv1.HTTPRouteSpec{
			Rules: []gatewayv1.HTTPRouteRule{
				{
					Matches: []gatewayv1.HTTPRouteMatch{
						{Path: &gatewayv1.HTTPPathMatch{Type: &pathPrefix, Value: new("/")}},
					},
					Filters: []gatewayv1.HTTPRouteFilter{filter},
					BackendRefs: []gatewayv1.HTTPBackendRef{
						backendRef("svc", 80, 1),
					},
				},
			},
		},
	}
}

func routeWithTimeouts(timeouts *gatewayv1.HTTPRouteTimeouts) *gatewayv1.HTTPRoute {
	pathPrefix := gatewayv1.PathMatchPathPrefix

	return &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "timeout-route", Namespace: "default"},
		Spec: gatewayv1.HTTPRouteSpec{
			Rules: []gatewayv1.HTTPRouteRule{
				{
					Matches: []gatewayv1.HTTPRouteMatch{
						{Path: &gatewayv1.HTTPPathMatch{Type: &pathPrefix, Value: new("/")}},
					},
					BackendRefs: []gatewayv1.HTTPBackendRef{
						backendRef("svc", 80, 1),
					},
					Timeouts: timeouts,
				},
			},
		},
	}
}

func routeWithHeaderMatch(header gatewayv1.HTTPHeaderMatch) *gatewayv1.HTTPRoute {
	pathPrefix := gatewayv1.PathMatchPathPrefix

	return &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "header-route", Namespace: "default"},
		Spec: gatewayv1.HTTPRouteSpec{
			Rules: []gatewayv1.HTTPRouteRule{
				{
					Matches: []gatewayv1.HTTPRouteMatch{
						{
							Path:    &gatewayv1.HTTPPathMatch{Type: &pathPrefix, Value: new("/")},
							Headers: []gatewayv1.HTTPHeaderMatch{header},
						},
					},
					BackendRefs: []gatewayv1.HTTPBackendRef{
						backendRef("svc", 80, 1),
					},
				},
			},
		},
	}
}

func durationPtr(val string) *gatewayv1.Duration {
	d := gatewayv1.Duration(val)

	return &d
}

func backendRef(name string, port, weight int) gatewayv1.HTTPBackendRef {
	portNum := gatewayv1.PortNumber(port)
	weightInt := int32(weight)

	return gatewayv1.HTTPBackendRef{
		BackendRef: gatewayv1.BackendRef{
			BackendObjectReference: gatewayv1.BackendObjectReference{
				Name: gatewayv1.ObjectName(name),
				Port: &portNum,
			},
			Weight: &weightInt,
		},
	}
}
