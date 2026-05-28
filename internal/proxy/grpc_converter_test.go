package proxy_test

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
)

func grpcExact() *gatewayv1.GRPCMethodMatchType {
	t := gatewayv1.GRPCMethodMatchExact

	return &t
}

func grpcRegex() *gatewayv1.GRPCMethodMatchType {
	t := gatewayv1.GRPCMethodMatchRegularExpression

	return &t
}

func grpcBackendRef(name string, port, weight int) gatewayv1.GRPCBackendRef {
	p := gatewayv1.PortNumber(port)
	w := int32(weight)

	return gatewayv1.GRPCBackendRef{
		BackendRef: gatewayv1.BackendRef{
			BackendObjectReference: gatewayv1.BackendObjectReference{
				Name: gatewayv1.ObjectName(name),
				Port: &p,
			},
			Weight: &w,
		},
	}
}

// TestConvertGRPCRoutes_ExactServiceMethod maps an Exact service+method match
// to an exact-path proxy rule using the HTTP/2 form /{service}/{method}, with
// the backend forced to h2c (gRPC is HTTP/2).
func TestConvertGRPCRoutes_ExactServiceMethod(t *testing.T) {
	t.Parallel()

	svc := "grpc.examples.echo.Echo"
	method := "UnaryEcho"
	routes := []*gatewayv1.GRPCRoute{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "echo", Namespace: "default"},
			Spec: gatewayv1.GRPCRouteSpec{
				Hostnames: []gatewayv1.Hostname{"grpc.example.com"},
				Rules: []gatewayv1.GRPCRouteRule{
					{
						Matches: []gatewayv1.GRPCRouteMatch{
							{Method: &gatewayv1.GRPCMethodMatch{Type: grpcExact(), Service: &svc, Method: &method}},
						},
						BackendRefs: []gatewayv1.GRPCBackendRef{grpcBackendRef("echo-svc", 9000, 1)},
					},
				},
			},
		},
	}

	cfg := proxy.ConvertGRPCRoutes(context.Background(), routes, "cluster.local", nil, nil, nil)

	require.Len(t, cfg.Rules, 1)
	rule := cfg.Rules[0]
	assert.Contains(t, rule.Hostnames, "grpc.example.com")
	require.Len(t, rule.Matches, 1)
	require.NotNil(t, rule.Matches[0].Path)
	assert.Equal(t, proxy.PathMatchExact, rule.Matches[0].Path.Type)
	assert.Equal(t, "/grpc.examples.echo.Echo/UnaryEcho", rule.Matches[0].Path.Value)
	require.Len(t, rule.Backends, 1)
	assert.Equal(t, proxy.BackendProtocolH2C, rule.Backends[0].Protocol, "gRPC backend must be h2c")
}

// TestConvertGRPCRoutes_BackendPort443ForcesCleartextH2C pins the port-443
// normalization: buildServiceURL emits https:// for port 443, but gRPC h2c is
// cleartext, so the converter rewrites the scheme to http:// regardless of
// port. Without the rewrite a gRPC backend on 443 would get an https URL the
// h2c transport cannot dial.
//
// This is the backward-compat path: no BackendTLSPolicy attached, no Gateway
// clientCertificateRef → the port-443 backend stays h2c. When a policy IS
// attached, the gRPC backend is upgraded to TLS instead — pinned by
// TestConvertGRPCRoutes_PolicyOnBackendUpgradesToTLS.
func TestConvertGRPCRoutes_BackendPort443NoPolicyStaysCleartextH2C(t *testing.T) {
	t.Parallel()

	svc := "grpc.examples.echo.Echo"
	method := "UnaryEcho"
	routes := []*gatewayv1.GRPCRoute{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "echo", Namespace: "default"},
			Spec: gatewayv1.GRPCRouteSpec{
				Rules: []gatewayv1.GRPCRouteRule{
					{
						Matches: []gatewayv1.GRPCRouteMatch{
							{Method: &gatewayv1.GRPCMethodMatch{Type: grpcExact(), Service: &svc, Method: &method}},
						},
						BackendRefs: []gatewayv1.GRPCBackendRef{grpcBackendRef("echo-svc", 443, 1)},
					},
				},
			},
		},
	}

	cfg := proxy.ConvertGRPCRoutes(context.Background(), routes, "cluster.local", nil, nil, nil)

	require.Len(t, cfg.Rules, 1)
	require.Len(t, cfg.Rules[0].Backends, 1)
	backend := cfg.Rules[0].Backends[0]
	assert.True(t, strings.HasPrefix(backend.URL, "http://"),
		"port-443 gRPC backend must be rewritten to cleartext http:// for h2c, got %q", backend.URL)
	assert.NotContains(t, backend.URL, "https://", "no https scheme may survive for an h2c backend")
	assert.Equal(t, proxy.BackendProtocolH2C, backend.Protocol)
}

// TestConvertGRPCRoutes_PolicyOnBackendUpgradesToTLS pins the headline
// behavior of #344: when a BackendTLSPolicy targets the gRPC backend's
// Service, the converter stamps BackendTLSConfig onto the backend,
// flips the scheme to https://, and switches Protocol to
// BackendProtocolHTTPS. The proxy's transport layer then negotiates
// HTTP/2 via ALPN — gRPC over TLS — instead of dialing cleartext h2c
// and getting refused by a TLS-only backend.
func TestConvertGRPCRoutes_PolicyOnBackendUpgradesToTLS(t *testing.T) {
	t.Parallel()

	svc := "grpc.examples.echo.Echo"
	method := "UnaryEcho"
	routes := []*gatewayv1.GRPCRoute{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "echo", Namespace: "default"},
			Spec: gatewayv1.GRPCRouteSpec{
				Rules: []gatewayv1.GRPCRouteRule{
					{
						Matches: []gatewayv1.GRPCRouteMatch{
							{Method: &gatewayv1.GRPCMethodMatch{Type: grpcExact(), Service: &svc, Method: &method}},
						},
						BackendRefs: []gatewayv1.GRPCBackendRef{grpcBackendRef("echo-svc", 8443, 1)},
					},
				},
			},
		},
	}

	tlsResolver := func(_ context.Context, namespace, serviceName string, port int32) *proxy.BackendTLSConfig {
		if namespace == "default" && serviceName == "echo-svc" && port == 8443 {
			return &proxy.BackendTLSConfig{
				CABundlePEM: "-----BEGIN CERTIFICATE-----\nFAKE\n-----END CERTIFICATE-----\n",
				ServerName:  "echo-svc.default.svc.cluster.local",
			}
		}

		return nil
	}

	cfg := proxy.ConvertGRPCRoutes(context.Background(), routes, "cluster.local", nil, tlsResolver, nil)

	require.Len(t, cfg.Rules, 1)
	require.Len(t, cfg.Rules[0].Backends, 1)
	backend := cfg.Rules[0].Backends[0]
	require.NotNil(t, backend.TLS, "BackendTLSPolicy match MUST stamp TLS config on gRPC backend")
	assert.Equal(t, "echo-svc.default.svc.cluster.local", backend.TLS.ServerName)
	assert.True(t, strings.HasPrefix(backend.URL, "https://"),
		"gRPC backend with BackendTLSPolicy MUST flip to https scheme, got %q", backend.URL)
	// Protocol is empty (BackendProtocolHTTP) — when backendTLS is set,
	// newTLSTransport's http.Transport with ALPN auto-negotiates HTTP/2.
	// h2c is cleartext and cannot coexist with TLS (see handler.go:550-552
	// and the HTTPRoute resolveH2C "TLS wins" branch).
	assert.Equal(t, proxy.BackendProtocolHTTP, backend.Protocol,
		"gRPC backend with BackendTLSPolicy MUST drop the h2c marker — ALPN negotiates HTTP/2 over TLS")
}

// TestConvertGRPCRoutes_GatewayClientCertStampedOnGRPCBackend pins that
// the parent Gateway's clientCertificateRef is presented during the gRPC
// backend TLS handshake. Without this, an operator who attaches a
// BackendTLSPolicy expecting mTLS would see one-way TLS and the server
// would reject the handshake — same shape as the HTTPRoute mTLS path,
// which the upstream conformance already covers.
func TestConvertGRPCRoutes_GatewayClientCertStampedOnGRPCBackend(t *testing.T) {
	t.Parallel()

	svc := "grpc.examples.echo.Echo"
	method := "UnaryEcho"
	routes := []*gatewayv1.GRPCRoute{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "echo", Namespace: "default"},
			Spec: gatewayv1.GRPCRouteSpec{
				CommonRouteSpec: gatewayv1.CommonRouteSpec{
					ParentRefs: []gatewayv1.ParentReference{{Name: gatewayv1.ObjectName("gw")}},
				},
				Rules: []gatewayv1.GRPCRouteRule{
					{
						Matches: []gatewayv1.GRPCRouteMatch{
							{Method: &gatewayv1.GRPCMethodMatch{Type: grpcExact(), Service: &svc, Method: &method}},
						},
						BackendRefs: []gatewayv1.GRPCBackendRef{grpcBackendRef("echo-svc", 8443, 1)},
					},
				},
			},
		},
	}

	tlsResolver := func(_ context.Context, _, _ string, _ int32) *proxy.BackendTLSConfig {
		return &proxy.BackendTLSConfig{
			CABundlePEM: "-----BEGIN CERTIFICATE-----\nFAKE\n-----END CERTIFICATE-----\n",
			ServerName:  "echo-svc.default.svc.cluster.local",
		}
	}

	clientCertPEM := []byte("-----BEGIN CERTIFICATE-----\nCLIENT-CERT\n-----END CERTIFICATE-----\n")
	clientKeyPEM := []byte("-----BEGIN PRIVATE KEY-----\nCLIENT-KEY\n-----END PRIVATE KEY-----\n")

	certResolver := func(_ context.Context, gw types.NamespacedName) *proxy.ClientCertConfig {
		if gw.Namespace == "default" && gw.Name == "gw" {
			return &proxy.ClientCertConfig{CertPEM: clientCertPEM, KeyPEM: clientKeyPEM}
		}

		return nil
	}

	cfg := proxy.ConvertGRPCRoutes(context.Background(), routes, "cluster.local", nil, tlsResolver, certResolver)

	require.Len(t, cfg.Rules, 1)
	require.Len(t, cfg.Rules[0].Backends, 1)
	backend := cfg.Rules[0].Backends[0]
	require.NotNil(t, backend.TLS)
	assert.Equal(t, clientCertPEM, backend.TLS.ClientCertPEM,
		"parent Gateway's clientCertificateRef MUST be stamped on the gRPC backend's TLS config")
	assert.Equal(t, clientKeyPEM, backend.TLS.ClientKeyPEM)
}

// TestConvertGRPCRoutes_MixedTLSAndCleartextBackends pins that two
// backends in the same rule are resolved independently — one with a
// matching BackendTLSPolicy goes TLS+ALPN, the other without stays
// h2c. Mixed rules are unusual but legal per the spec, and a future
// refactor that hoists the TLS branch above the per-backend loop would
// silently break the cleartext sibling.
func TestConvertGRPCRoutes_MixedTLSAndCleartextBackends(t *testing.T) {
	t.Parallel()

	svc := "grpc.examples.echo.Echo"
	method := "UnaryEcho"
	routes := []*gatewayv1.GRPCRoute{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "echo", Namespace: "default"},
			Spec: gatewayv1.GRPCRouteSpec{
				Rules: []gatewayv1.GRPCRouteRule{
					{
						Matches: []gatewayv1.GRPCRouteMatch{
							{Method: &gatewayv1.GRPCMethodMatch{Type: grpcExact(), Service: &svc, Method: &method}},
						},
						BackendRefs: []gatewayv1.GRPCBackendRef{
							grpcBackendRef("echo-tls", 8443, 1),
							grpcBackendRef("echo-plain", 9000, 1),
						},
					},
				},
			},
		},
	}

	tlsResolver := func(_ context.Context, _, serviceName string, _ int32) *proxy.BackendTLSConfig {
		if serviceName == "echo-tls" {
			return &proxy.BackendTLSConfig{CABundlePEM: "FAKE", ServerName: "echo-tls"}
		}

		return nil
	}

	cfg := proxy.ConvertGRPCRoutes(context.Background(), routes, "cluster.local", nil, tlsResolver, nil)

	require.Len(t, cfg.Rules, 1)
	require.Len(t, cfg.Rules[0].Backends, 2)

	tlsBackend := cfg.Rules[0].Backends[0]
	assert.Equal(t, "echo-tls", tlsBackend.TLS.ServerName, "first backend with policy MUST be TLS")
	assert.True(t, strings.HasPrefix(tlsBackend.URL, "https://"))
	assert.Equal(t, proxy.BackendProtocolHTTP, tlsBackend.Protocol)

	plainBackend := cfg.Rules[0].Backends[1]
	assert.Nil(t, plainBackend.TLS, "second backend without policy MUST stay cleartext")
	assert.True(t, strings.HasPrefix(plainBackend.URL, "http://"))
	assert.Equal(t, proxy.BackendProtocolH2C, plainBackend.Protocol)
}

// TestConvertGRPCRoutes_NoBackendTLSAlwaysCleartextH2C pins the backward
// compatibility property: when no BackendTLSPolicy targets the backend
// AND no Gateway clientCertificateRef applies, the gRPC backend stays
// cleartext h2c — same shape as before the TLS-upgrade work. The
// renamed port-443 test pins the same property for the port-443 edge
// case; this one pins it for a non-standard port.
func TestConvertGRPCRoutes_NoBackendTLSAlwaysCleartextH2C(t *testing.T) {
	t.Parallel()

	svc := "grpc.examples.echo.Echo"
	method := "UnaryEcho"
	routes := []*gatewayv1.GRPCRoute{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "echo", Namespace: "default"},
			Spec: gatewayv1.GRPCRouteSpec{
				Rules: []gatewayv1.GRPCRouteRule{
					{
						Matches: []gatewayv1.GRPCRouteMatch{
							{Method: &gatewayv1.GRPCMethodMatch{Type: grpcExact(), Service: &svc, Method: &method}},
						},
						BackendRefs: []gatewayv1.GRPCBackendRef{grpcBackendRef("echo-svc", 9000, 1)},
					},
				},
			},
		},
	}

	cfg := proxy.ConvertGRPCRoutes(context.Background(), routes, "cluster.local", nil, nil, nil)

	require.Len(t, cfg.Rules, 1)
	require.Len(t, cfg.Rules[0].Backends, 1)
	backend := cfg.Rules[0].Backends[0]
	assert.Nil(t, backend.TLS, "gRPC backend must carry no TLS config — BackendTLSPolicy is not applied")
	assert.Equal(t, proxy.BackendProtocolH2C, backend.Protocol)
	assert.True(t, strings.HasPrefix(backend.URL, "http://"), "gRPC backend stays cleartext, got %q", backend.URL)
}

// TestConvertGRPCRoutes_MultipleWeightedBackends pins weighted traffic
// splitting: a rule with two weighted backends emits both into the proxy
// rule's Backends with their weights preserved, so the router's weighted
// selection distributes traffic proportionally (same as HTTPRoute). The docs
// describe this behavior, so it must stay covered.
func TestConvertGRPCRoutes_MultipleWeightedBackends(t *testing.T) {
	t.Parallel()

	svc := "grpc.examples.echo.Echo"
	method := "UnaryEcho"
	routes := []*gatewayv1.GRPCRoute{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "echo", Namespace: "default"},
			Spec: gatewayv1.GRPCRouteSpec{
				Rules: []gatewayv1.GRPCRouteRule{
					{
						Matches: []gatewayv1.GRPCRouteMatch{
							{Method: &gatewayv1.GRPCMethodMatch{Type: grpcExact(), Service: &svc, Method: &method}},
						},
						BackendRefs: []gatewayv1.GRPCBackendRef{
							grpcBackendRef("primary-svc", 9000, 80),
							grpcBackendRef("fallback-svc", 9000, 20),
						},
					},
				},
			},
		},
	}

	cfg := proxy.ConvertGRPCRoutes(context.Background(), routes, "cluster.local", nil, nil, nil)

	require.Len(t, cfg.Rules, 1)
	require.Len(t, cfg.Rules[0].Backends, 2, "both weighted backends must survive for proportional splitting")

	weightByURL := map[string]int32{}
	for _, b := range cfg.Rules[0].Backends {
		weightByURL[b.URL] = b.Weight
	}

	assert.Equal(t, int32(80), weightByURL["http://primary-svc.default.svc.cluster.local:9000"])
	assert.Equal(t, int32(20), weightByURL["http://fallback-svc.default.svc.cluster.local:9000"])
}

// TestConvertGRPCRoutes_ServiceOnly maps a service-only match to a path-prefix
// rule /{service}/ so every method of the service routes.
func TestConvertGRPCRoutes_ServiceOnly(t *testing.T) {
	t.Parallel()

	svc := "grpc.examples.echo.Echo"
	routes := []*gatewayv1.GRPCRoute{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "echo", Namespace: "default"},
			Spec: gatewayv1.GRPCRouteSpec{
				Rules: []gatewayv1.GRPCRouteRule{
					{
						Matches: []gatewayv1.GRPCRouteMatch{
							{Method: &gatewayv1.GRPCMethodMatch{Type: grpcExact(), Service: &svc}},
						},
						BackendRefs: []gatewayv1.GRPCBackendRef{grpcBackendRef("echo-svc", 9000, 1)},
					},
				},
			},
		},
	}

	cfg := proxy.ConvertGRPCRoutes(context.Background(), routes, "cluster.local", nil, nil, nil)

	require.Len(t, cfg.Rules, 1)
	require.NotNil(t, cfg.Rules[0].Matches[0].Path)
	assert.Equal(t, proxy.PathMatchPathPrefix, cfg.Rules[0].Matches[0].Path.Type)
	assert.Equal(t, "/grpc.examples.echo.Echo/", cfg.Rules[0].Matches[0].Path.Value)
}

// TestConvertGRPCRoutes_RegexMethod maps a RegularExpression method match to a
// regex path rule.
func TestConvertGRPCRoutes_RegexMethod(t *testing.T) {
	t.Parallel()

	svc := "grpc.examples.echo.Echo"
	method := "Unary.*"
	routes := []*gatewayv1.GRPCRoute{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "echo", Namespace: "default"},
			Spec: gatewayv1.GRPCRouteSpec{
				Rules: []gatewayv1.GRPCRouteRule{
					{
						Matches: []gatewayv1.GRPCRouteMatch{
							{Method: &gatewayv1.GRPCMethodMatch{Type: grpcRegex(), Service: &svc, Method: &method}},
						},
						BackendRefs: []gatewayv1.GRPCBackendRef{grpcBackendRef("echo-svc", 9000, 1)},
					},
				},
			},
		},
	}

	cfg := proxy.ConvertGRPCRoutes(context.Background(), routes, "cluster.local", nil, nil, nil)

	require.Len(t, cfg.Rules, 1)
	require.NotNil(t, cfg.Rules[0].Matches[0].Path)
	assert.Equal(t, proxy.PathMatchRegularExpression, cfg.Rules[0].Matches[0].Path.Type)
	assert.Equal(t, "^/(?:grpc.examples.echo.Echo)/(?:Unary.*)$", cfg.Rules[0].Matches[0].Path.Value,
		"generated gRPC regex must be fully anchored and segment-scoped — the proxy regex matcher is substring-based")
}

// TestConvertGRPCRoutes_RegexAlternationAnchored proves a user RegularExpression
// with top-level alternation stays scoped to its segment. Inserted raw into
// "^/svc/Foo|Bar$" the alternation binds loosest and yields
// "(^/svc/Foo)|(Bar$)" — matching any path ending in "Bar". Each user pattern
// must be wrapped in a non-capturing group so the anchors apply to the whole
// method name.
func TestConvertGRPCRoutes_RegexAlternationAnchored(t *testing.T) {
	t.Parallel()

	svc := "svc"
	method := "Foo|Bar"
	routes := []*gatewayv1.GRPCRoute{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "alt", Namespace: "default"},
			Spec: gatewayv1.GRPCRouteSpec{
				Rules: []gatewayv1.GRPCRouteRule{
					{
						Matches: []gatewayv1.GRPCRouteMatch{
							{Method: &gatewayv1.GRPCMethodMatch{Type: grpcRegex(), Service: &svc, Method: &method}},
						},
						BackendRefs: []gatewayv1.GRPCBackendRef{grpcBackendRef("alt-svc", 9000, 1)},
					},
				},
			},
		},
	}

	cfg := proxy.ConvertGRPCRoutes(context.Background(), routes, "cluster.local", nil, nil, nil)

	require.Len(t, cfg.Rules, 1)
	require.NotNil(t, cfg.Rules[0].Matches[0].Path)

	value := cfg.Rules[0].Matches[0].Path.Value
	assert.Equal(t, "^/(?:svc)/(?:Foo|Bar)$", value,
		"user alternation must be wrapped so it cannot escape the segment anchors")

	re := regexp.MustCompile(value)
	assert.True(t, re.MatchString("/svc/Foo"), "should match service svc method Foo")
	assert.True(t, re.MatchString("/svc/Bar"), "should match service svc method Bar")
	assert.False(t, re.MatchString("/other/Bar"), "alternation must not leak to any-path-ending-in-Bar")
	assert.False(t, re.MatchString("/svc/FooBar"), "anchors must reject a longer method")
}

// TestConvertGRPCRoutes_MethodOnlyExact maps an Exact method-only match (no
// service) to an anchored regex over any single service segment, with the
// literal method regexp-quoted.
func TestConvertGRPCRoutes_MethodOnlyExact(t *testing.T) {
	t.Parallel()

	method := "Get.Thing" // dot must be quoted so it matches literally
	routes := []*gatewayv1.GRPCRoute{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: "default"},
			Spec: gatewayv1.GRPCRouteSpec{
				Rules: []gatewayv1.GRPCRouteRule{
					{
						Matches: []gatewayv1.GRPCRouteMatch{
							{Method: &gatewayv1.GRPCMethodMatch{Type: grpcExact(), Method: &method}},
						},
						BackendRefs: []gatewayv1.GRPCBackendRef{grpcBackendRef("m-svc", 9000, 1)},
					},
				},
			},
		},
	}

	cfg := proxy.ConvertGRPCRoutes(context.Background(), routes, "cluster.local", nil, nil, nil)

	require.Len(t, cfg.Rules, 1)
	require.NotNil(t, cfg.Rules[0].Matches[0].Path)
	assert.Equal(t, proxy.PathMatchRegularExpression, cfg.Rules[0].Matches[0].Path.Type)
	assert.Equal(t, `^/[^/]+/Get\.Thing$`, cfg.Rules[0].Matches[0].Path.Value)
}

// TestConvertGRPCRoutes_RegexEmptyServiceAndMethod exercises the empty-service
// and empty-method substitution branches of a RegularExpression match.
func TestConvertGRPCRoutes_RegexEmptyServiceAndMethod(t *testing.T) {
	t.Parallel()

	svc := "svc.Foo"
	method := "Bar.*"

	tests := []struct {
		name    string
		service *string
		method  *string
		want    string
	}{
		{name: "empty service", service: nil, method: &method, want: "^/(?:[^/]+)/(?:Bar.*)$"},
		{name: "empty method", service: &svc, method: nil, want: "^/(?:svc.Foo)/(?:[^/]+)$"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			routes := []*gatewayv1.GRPCRoute{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "default"},
					Spec: gatewayv1.GRPCRouteSpec{
						Rules: []gatewayv1.GRPCRouteRule{
							{
								Matches: []gatewayv1.GRPCRouteMatch{
									{Method: &gatewayv1.GRPCMethodMatch{Type: grpcRegex(), Service: tt.service, Method: tt.method}},
								},
								BackendRefs: []gatewayv1.GRPCBackendRef{grpcBackendRef("r-svc", 9000, 1)},
							},
						},
					},
				},
			}

			cfg := proxy.ConvertGRPCRoutes(context.Background(), routes, "cluster.local", nil, nil, nil)

			require.Len(t, cfg.Rules, 1)
			require.NotNil(t, cfg.Rules[0].Matches[0].Path)
			assert.Equal(t, tt.want, cfg.Rules[0].Matches[0].Path.Value)
		})
	}
}

// TestConvertGRPCRoutes_HeaderRegexType maps a RegularExpression gRPC header
// match to the proxy's regex header matcher.
func TestConvertGRPCRoutes_HeaderRegexType(t *testing.T) {
	t.Parallel()

	svc := "svc.Foo"
	regexType := gatewayv1.GRPCHeaderMatchRegularExpression
	routes := []*gatewayv1.GRPCRoute{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "h", Namespace: "default"},
			Spec: gatewayv1.GRPCRouteSpec{
				Rules: []gatewayv1.GRPCRouteRule{
					{
						Matches: []gatewayv1.GRPCRouteMatch{
							{
								Method:  &gatewayv1.GRPCMethodMatch{Type: grpcExact(), Service: &svc},
								Headers: []gatewayv1.GRPCHeaderMatch{{Type: &regexType, Name: "x-tenant", Value: "blue-.*"}},
							},
						},
						BackendRefs: []gatewayv1.GRPCBackendRef{grpcBackendRef("foo-svc", 9000, 1)},
					},
				},
			},
		},
	}

	cfg := proxy.ConvertGRPCRoutes(context.Background(), routes, "cluster.local", nil, nil, nil)

	require.Len(t, cfg.Rules, 1)
	require.Len(t, cfg.Rules[0].Matches[0].Headers, 1)
	assert.Equal(t, proxy.HeaderMatchRegularExpression, cfg.Rules[0].Matches[0].Headers[0].Type)
}

// TestConvertGRPCRoutes_FiltersDropped: gRPC filters are not supported, so a
// rule carrying one produces no proxy filters (logged + skipped).
func TestConvertGRPCRoutes_FiltersDropped(t *testing.T) {
	t.Parallel()

	svc := "svc.Foo"
	routes := []*gatewayv1.GRPCRoute{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "f", Namespace: "default"},
			Spec: gatewayv1.GRPCRouteSpec{
				Rules: []gatewayv1.GRPCRouteRule{
					{
						Matches: []gatewayv1.GRPCRouteMatch{
							{Method: &gatewayv1.GRPCMethodMatch{Type: grpcExact(), Service: &svc}},
						},
						Filters: []gatewayv1.GRPCRouteFilter{
							{Type: gatewayv1.GRPCRouteFilterRequestHeaderModifier},
						},
						BackendRefs: []gatewayv1.GRPCBackendRef{grpcBackendRef("foo-svc", 9000, 1)},
					},
				},
			},
		},
	}

	cfg := proxy.ConvertGRPCRoutes(context.Background(), routes, "cluster.local", nil, nil, nil)

	require.Len(t, cfg.Rules, 1)
	assert.Empty(t, cfg.Rules[0].Filters, "gRPC filters are not supported and must be dropped")
}

// TestConvertGRPCRoutes_BackendSkips exercises the backend drop paths: a
// dropped backend leaves the rule with no backends (→ proxy returns 500).
func TestConvertGRPCRoutes_BackendSkips(t *testing.T) {
	t.Parallel()

	svc := "svc.Foo"
	badPort := gatewayv1.PortNumber(70000) // > 65535
	negWeight := int32(-1)
	configMapKind := gatewayv1.Kind("ConfigMap")
	port := gatewayv1.PortNumber(9000)

	tests := []struct {
		name    string
		backend gatewayv1.GRPCBackendRef
	}{
		{
			name: "non-Service kind",
			backend: gatewayv1.GRPCBackendRef{BackendRef: gatewayv1.BackendRef{
				BackendObjectReference: gatewayv1.BackendObjectReference{Kind: &configMapKind, Name: "cm", Port: &port},
			}},
		},
		{
			name: "invalid port",
			backend: gatewayv1.GRPCBackendRef{BackendRef: gatewayv1.BackendRef{
				BackendObjectReference: gatewayv1.BackendObjectReference{Name: "svc", Port: &badPort},
			}},
		},
		{
			name: "negative weight",
			backend: gatewayv1.GRPCBackendRef{BackendRef: gatewayv1.BackendRef{
				BackendObjectReference: gatewayv1.BackendObjectReference{Name: "svc", Port: &port},
				Weight:                 &negWeight,
			}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			routes := []*gatewayv1.GRPCRoute{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "default"},
					Spec: gatewayv1.GRPCRouteSpec{
						Rules: []gatewayv1.GRPCRouteRule{
							{
								Matches: []gatewayv1.GRPCRouteMatch{
									{Method: &gatewayv1.GRPCMethodMatch{Type: grpcExact(), Service: &svc}},
								},
								BackendRefs: []gatewayv1.GRPCBackendRef{tt.backend},
							},
						},
					},
				},
			}

			cfg := proxy.ConvertGRPCRoutes(context.Background(), routes, "cluster.local", nil, nil, nil)

			require.Len(t, cfg.Rules, 1)
			assert.Empty(t, cfg.Rules[0].Backends, "invalid backend must be dropped → rule has no backends")
		})
	}
}

// TestConvertGRPCRoutes_CrossNamespaceDenied drops a cross-namespace backend
// when the ReferenceGrant validator denies it.
func TestConvertGRPCRoutes_CrossNamespaceDenied(t *testing.T) {
	t.Parallel()

	svc := "svc.Foo"
	otherNS := gatewayv1.Namespace("other")
	port := gatewayv1.PortNumber(9000)
	denyAll := func(_ context.Context, _ string, _ gatewayv1.BackendObjectReference) bool { return false }

	routes := []*gatewayv1.GRPCRoute{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: "default"},
			Spec: gatewayv1.GRPCRouteSpec{
				Rules: []gatewayv1.GRPCRouteRule{
					{
						Matches: []gatewayv1.GRPCRouteMatch{
							{Method: &gatewayv1.GRPCMethodMatch{Type: grpcExact(), Service: &svc}},
						},
						BackendRefs: []gatewayv1.GRPCBackendRef{
							{BackendRef: gatewayv1.BackendRef{
								BackendObjectReference: gatewayv1.BackendObjectReference{
									Name: "remote-svc", Namespace: &otherNS, Port: &port,
								},
							}},
						},
					},
				},
			},
		},
	}

	cfg := proxy.ConvertGRPCRoutes(context.Background(), routes, "cluster.local", denyAll, nil, nil)

	require.Len(t, cfg.Rules, 1)
	assert.Empty(t, cfg.Rules[0].Backends, "cross-namespace backend denied by ReferenceGrant must be dropped")
}

// TestConvertGRPCRoutes_HeaderMatch carries gRPC header matches through to the
// proxy rule's header matchers.
func TestConvertGRPCRoutes_HeaderMatch(t *testing.T) {
	t.Parallel()

	svc := "svc.Foo"
	routes := []*gatewayv1.GRPCRoute{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "h", Namespace: "default"},
			Spec: gatewayv1.GRPCRouteSpec{
				Rules: []gatewayv1.GRPCRouteRule{
					{
						Matches: []gatewayv1.GRPCRouteMatch{
							{
								Method:  &gatewayv1.GRPCMethodMatch{Type: grpcExact(), Service: &svc},
								Headers: []gatewayv1.GRPCHeaderMatch{{Name: "x-tenant", Value: "blue"}},
							},
						},
						BackendRefs: []gatewayv1.GRPCBackendRef{grpcBackendRef("foo-svc", 9000, 1)},
					},
				},
			},
		},
	}

	cfg := proxy.ConvertGRPCRoutes(context.Background(), routes, "cluster.local", nil, nil, nil)

	require.Len(t, cfg.Rules, 1)
	require.Len(t, cfg.Rules[0].Matches[0].Headers, 1)
	assert.Equal(t, "x-tenant", cfg.Rules[0].Matches[0].Headers[0].Name)
	assert.Equal(t, "blue", cfg.Rules[0].Matches[0].Headers[0].Value)
	assert.Equal(t, proxy.HeaderMatchExact, cfg.Rules[0].Matches[0].Headers[0].Type)
}

// TestConvertGRPCRoutes_HeaderOnlyNoMethod pins the spec-allowed header-only
// match: a GRPCRouteMatch with no Method but a header produces a RouteMatch
// with a nil Path (matching every gRPC method) constrained only by the header.
// The nil-Path arm is load-bearing — dropping it would silently break
// header-only gRPC routing — so it must stay covered.
func TestConvertGRPCRoutes_HeaderOnlyNoMethod(t *testing.T) {
	t.Parallel()

	routes := []*gatewayv1.GRPCRoute{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "hdr", Namespace: "default"},
			Spec: gatewayv1.GRPCRouteSpec{
				Rules: []gatewayv1.GRPCRouteRule{
					{
						Matches: []gatewayv1.GRPCRouteMatch{
							{Headers: []gatewayv1.GRPCHeaderMatch{{Name: "x-tenant", Value: "blue"}}},
						},
						BackendRefs: []gatewayv1.GRPCBackendRef{grpcBackendRef("foo-svc", 9000, 1)},
					},
				},
			},
		},
	}

	cfg := proxy.ConvertGRPCRoutes(context.Background(), routes, "cluster.local", nil, nil, nil)

	require.Len(t, cfg.Rules, 1)
	require.Len(t, cfg.Rules[0].Matches, 1)
	assert.Nil(t, cfg.Rules[0].Matches[0].Path, "header-only match has no path → matches every gRPC method")
	require.Len(t, cfg.Rules[0].Matches[0].Headers, 1)
	assert.Equal(t, "x-tenant", cfg.Rules[0].Matches[0].Headers[0].Name)
	assert.Equal(t, "blue", cfg.Rules[0].Matches[0].Headers[0].Value)
}

// TestConvertGRPCRoutes_NoMatchesMatchesAll: a rule with no matches routes all
// gRPC traffic (no path constraint), backend still h2c.
func TestConvertGRPCRoutes_NoMatchesMatchesAll(t *testing.T) {
	t.Parallel()

	routes := []*gatewayv1.GRPCRoute{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "catchall", Namespace: "default"},
			Spec: gatewayv1.GRPCRouteSpec{
				Hostnames: []gatewayv1.Hostname{"grpc.example.com"},
				Rules: []gatewayv1.GRPCRouteRule{
					{BackendRefs: []gatewayv1.GRPCBackendRef{grpcBackendRef("echo-svc", 9000, 1)}},
				},
			},
		},
	}

	cfg := proxy.ConvertGRPCRoutes(context.Background(), routes, "cluster.local", nil, nil, nil)

	require.Len(t, cfg.Rules, 1)
	assert.Empty(t, cfg.Rules[0].Matches, "no method match → no match constraints (match all)")
	require.Len(t, cfg.Rules[0].Backends, 1)
	assert.Equal(t, proxy.BackendProtocolH2C, cfg.Rules[0].Backends[0].Protocol)
}

// TestConvertGRPCRoutes_EmptyMatchMixedWithSpecificIsDropped pins the
// deliberate handling of a nonsensical rule that mixes a match-all (empty)
// match with a specific one in the same matches[] array: the empty match is
// dropped, so the rule keeps only the specific constraint rather than being
// promoted to a catch-all that would shadow other routes.
func TestConvertGRPCRoutes_EmptyMatchMixedWithSpecificIsDropped(t *testing.T) {
	t.Parallel()

	svc := "svc.Foo"
	method := "Bar"
	routes := []*gatewayv1.GRPCRoute{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "mixed", Namespace: "default"},
			Spec: gatewayv1.GRPCRouteSpec{
				Rules: []gatewayv1.GRPCRouteRule{
					{
						Matches: []gatewayv1.GRPCRouteMatch{
							{}, // match-all: nil method, no headers
							{Method: &gatewayv1.GRPCMethodMatch{Type: grpcExact(), Service: &svc, Method: &method}},
						},
						BackendRefs: []gatewayv1.GRPCBackendRef{grpcBackendRef("foo-svc", 9000, 1)},
					},
				},
			},
		},
	}

	cfg := proxy.ConvertGRPCRoutes(context.Background(), routes, "cluster.local", nil, nil, nil)

	require.Len(t, cfg.Rules, 1)
	require.Len(t, cfg.Rules[0].Matches, 1, "empty match dropped; only the specific match survives")
	require.NotNil(t, cfg.Rules[0].Matches[0].Path)
	assert.Equal(t, "/svc.Foo/Bar", cfg.Rules[0].Matches[0].Path.Value)
}
