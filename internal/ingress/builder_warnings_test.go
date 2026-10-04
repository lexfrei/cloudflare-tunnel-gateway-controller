package ingress_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/ingress"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/logging"
)

// TestBuild_WeightedBackendRefsNoWeightWarning pins the fix for the
// misleading "backendRef weight ignored, traffic splitting not supported"
// log: weight is fully honored end-to-end by the in-process L7 proxy
// (weighted-random selection across all backendRefs), so the Cloudflare-side
// ingress builder — whose document only feeds the Cloudflare dashboard and
// never serves traffic itself — must not claim weight is ignored or that traffic
// splitting is unsupported.
func TestBuild_WeightedBackendRefsNoWeightWarning(t *testing.T) {
	t.Parallel()

	logger, buf := logging.TestLogger(t)
	builder := ingress.NewBuilder("cluster.local", nil, nil, nil, logger)
	weight50 := int32(50)
	weight30 := int32(30)

	routes := []gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "weighted-route",
				Namespace: "production",
			},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"app.example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						BackendRefs: []gatewayv1.HTTPBackendRef{
							newHTTPBackendRefWithWeight("service1", &weight50, int32Ptr(8080)),
							newHTTPBackendRefWithWeight("service2", &weight30, int32Ptr(8080)),
						},
					},
				},
			},
		},
	}

	_ = builder.Build(context.Background(), routes)

	logs := buf.String()
	assert.Empty(t, logs, "weighted backends are served by the proxy and must not warn")
}

// TestBuild_SingleWeightedBackendRefNoWarnings reproduces the exact report
// in issue #510: a single backendRef with an explicit non-default weight
// (e.g. weight: 100, as Knative's net-gateway-api sets on every generated
// HTTPRoute) involves no traffic splitting at all — there is only one
// backend. The route must produce zero "route configuration partially
// applied" warnings.
func TestBuild_SingleWeightedBackendRefNoWarnings(t *testing.T) {
	t.Parallel()

	logger, buf := logging.TestLogger(t)
	builder := ingress.NewBuilder("cluster.local", nil, nil, nil, logger)
	weight100 := int32(100)

	routes := []gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "single-weighted-route",
				Namespace: "default",
			},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"app.example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						BackendRefs: []gatewayv1.HTTPBackendRef{
							newHTTPBackendRefWithWeight("service1", &weight100, int32Ptr(8080)),
						},
					},
				},
			},
		},
	}

	_ = builder.Build(context.Background(), routes)

	logs := buf.String()
	assert.Empty(t, logs, "a single weighted backendRef involves no splitting and must not warn")
}

func TestBuild_ProxyOnlyFeaturesAreNotLogged(t *testing.T) {
	t.Parallel()

	logger, buf := logging.TestLogger(t)
	builder := ingress.NewBuilder("cluster.local", nil, nil, nil, logger)
	method := gatewayv1.HTTPMethodGet
	headerType := gatewayv1.HeaderMatchExact
	weight50 := int32(50)

	routes := []gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "complex-route",
				Namespace: "default",
			},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"app.example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						Matches: []gatewayv1.HTTPRouteMatch{
							{
								Method: &method,
								Headers: []gatewayv1.HTTPHeaderMatch{
									{
										Type:  &headerType,
										Name:  "X-Test",
										Value: "test",
									},
								},
							},
						},
						BackendRefs: []gatewayv1.HTTPBackendRef{
							newHTTPBackendRefWithWeight("service1", &weight50, int32Ptr(8080)),
							newHTTPBackendRefWithWeight("service2", &weight50, int32Ptr(8080)),
						},
					},
				},
			},
		},
	}

	_ = builder.Build(context.Background(), routes)

	// The document carries hostnames only, so nothing a route can declare is
	// a reduction of it worth logging.
	assert.Empty(t, buf.String())
}

func TestBuild_NoWarningsForValidConfig(t *testing.T) {
	t.Parallel()

	logger, buf := logging.TestLogger(t)
	builder := ingress.NewBuilder("cluster.local", nil, nil, nil, logger)
	pathType := gatewayv1.PathMatchPathPrefix
	pathValue := "/api"

	routes := []gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "valid-route",
				Namespace: "default",
			},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"app.example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						Matches: []gatewayv1.HTTPRouteMatch{
							{
								Path: &gatewayv1.HTTPPathMatch{
									Type:  &pathType,
									Value: &pathValue,
								},
							},
						},
						BackendRefs: []gatewayv1.HTTPBackendRef{
							newHTTPBackendRefWithWeight("service1", nil, int32Ptr(8080)),
						},
					},
				},
			},
		},
	}

	_ = builder.Build(context.Background(), routes)

	logs := buf.String()
	// Should have no warnings for a properly configured route
	assert.Empty(t, logs, "expected no warnings for valid configuration")
}

// newHTTPBackendRefWithWeight creates an HTTPBackendRef with optional weight.
func newHTTPBackendRefWithWeight(name string, weight *int32, port *int32) gatewayv1.HTTPBackendRef {
	ref := gatewayv1.HTTPBackendRef{
		BackendRef: gatewayv1.BackendRef{
			BackendObjectReference: gatewayv1.BackendObjectReference{
				Name: gatewayv1.ObjectName(name),
			},
			Weight: weight,
		},
	}
	if port != nil {
		ref.Port = port
	}

	return ref
}
