package proxy_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

	cfg := proxy.ConvertGRPCRoutes(context.Background(), routes, "cluster.local", nil)

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

	cfg := proxy.ConvertGRPCRoutes(context.Background(), routes, "cluster.local", nil)

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

	cfg := proxy.ConvertGRPCRoutes(context.Background(), routes, "cluster.local", nil)

	require.Len(t, cfg.Rules, 1)
	require.NotNil(t, cfg.Rules[0].Matches[0].Path)
	assert.Equal(t, proxy.PathMatchRegularExpression, cfg.Rules[0].Matches[0].Path.Type)
	assert.Equal(t, "^/grpc.examples.echo.Echo/Unary.*$", cfg.Rules[0].Matches[0].Path.Value,
		"generated gRPC regex must be fully anchored — the proxy regex matcher is substring-based")
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

	cfg := proxy.ConvertGRPCRoutes(context.Background(), routes, "cluster.local", nil)

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
		{name: "empty service", service: nil, method: &method, want: "^/[^/]+/Bar.*$"},
		{name: "empty method", service: &svc, method: nil, want: "^/svc.Foo/[^/]+$"},
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

			cfg := proxy.ConvertGRPCRoutes(context.Background(), routes, "cluster.local", nil)

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

	cfg := proxy.ConvertGRPCRoutes(context.Background(), routes, "cluster.local", nil)

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

	cfg := proxy.ConvertGRPCRoutes(context.Background(), routes, "cluster.local", nil)

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

			cfg := proxy.ConvertGRPCRoutes(context.Background(), routes, "cluster.local", nil)

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

	cfg := proxy.ConvertGRPCRoutes(context.Background(), routes, "cluster.local", denyAll)

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

	cfg := proxy.ConvertGRPCRoutes(context.Background(), routes, "cluster.local", nil)

	require.Len(t, cfg.Rules, 1)
	require.Len(t, cfg.Rules[0].Matches[0].Headers, 1)
	assert.Equal(t, "x-tenant", cfg.Rules[0].Matches[0].Headers[0].Name)
	assert.Equal(t, "blue", cfg.Rules[0].Matches[0].Headers[0].Value)
	assert.Equal(t, proxy.HeaderMatchExact, cfg.Rules[0].Matches[0].Headers[0].Type)
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

	cfg := proxy.ConvertGRPCRoutes(context.Background(), routes, "cluster.local", nil)

	require.Len(t, cfg.Rules, 1)
	assert.Empty(t, cfg.Rules[0].Matches, "no method match → no match constraints (match all)")
	require.Len(t, cfg.Rules[0].Backends, 1)
	assert.Equal(t, proxy.BackendProtocolH2C, cfg.Rules[0].Backends[0].Protocol)
}
