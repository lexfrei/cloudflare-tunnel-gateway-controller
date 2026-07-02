package ingress_test

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/ingress"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/logging"
)

const (
	testGRPCService = "mypackage.MyService"
	testGRPCMethod  = "GetUser"
)

func TestGRPCBuild_WarnMultipleBackendRefs(t *testing.T) {
	t.Parallel()

	logger, buf := logging.TestLogger(t)
	builder := ingress.NewGRPCBuilder("cluster.local", nil, nil, nil, logger)
	routes := []gatewayv1.GRPCRoute{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-grpc-route",
				Namespace: "default",
			},
			Spec: gatewayv1.GRPCRouteSpec{
				Hostnames: []gatewayv1.Hostname{"grpc.example.com"},
				Rules: []gatewayv1.GRPCRouteRule{
					{
						BackendRefs: []gatewayv1.GRPCBackendRef{
							newGRPCBackendRefWithWeight("grpc-service1", nil, int32Ptr(9090)),
							newGRPCBackendRefWithWeight("grpc-service2", nil, int32Ptr(9090)),
							newGRPCBackendRefWithWeight("grpc-service3", nil, int32Ptr(9090)),
						},
					},
				},
			},
		},
	}

	_ = builder.Build(context.Background(), routes)

	logs := buf.String()
	assert.Contains(t, logs, "route configuration partially applied")
	assert.Contains(t, logs, `"route":"default/test-grpc-route"`)
	assert.Contains(t, logs, "multiple backendRefs specified")
	assert.Contains(t, logs, `"total_backends":3`)
	assert.Contains(t, logs, `"ignored_backends":2`)
}

// TestGRPCBuild_WeightedBackendRefsNoWeightWarning is the GRPCRoute twin of
// TestBuild_WeightedBackendRefsNoWeightWarning: weight is fully honored by
// the in-process L7 proxy for gRPC too (the same weighted-random selection),
// so the Cloudflare-side ingress builder must not claim weight is ignored.
func TestGRPCBuild_WeightedBackendRefsNoWeightWarning(t *testing.T) {
	t.Parallel()

	logger, buf := logging.TestLogger(t)
	builder := ingress.NewGRPCBuilder("cluster.local", nil, nil, nil, logger)
	weight70 := int32(70)
	weight30 := int32(30)

	routes := []gatewayv1.GRPCRoute{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "weighted-grpc-route",
				Namespace: "production",
			},
			Spec: gatewayv1.GRPCRouteSpec{
				Hostnames: []gatewayv1.Hostname{"grpc.example.com"},
				Rules: []gatewayv1.GRPCRouteRule{
					{
						BackendRefs: []gatewayv1.GRPCBackendRef{
							newGRPCBackendRefWithWeight("grpc-service1", &weight70, int32Ptr(9090)),
							newGRPCBackendRefWithWeight("grpc-service2", &weight30, int32Ptr(9090)),
						},
					},
				},
			},
		},
	}

	_ = builder.Build(context.Background(), routes)

	logs := buf.String()
	assert.NotContains(t, logs, "backendRef weight ignored")
	assert.NotContains(t, logs, "traffic splitting not supported")
	// The multiple-backends reduction to one Cloudflare-side ingress URL is a
	// separate, still-genuine fact and keeps its own log line.
	assert.Contains(t, logs, "multiple backendRefs specified")
}

// TestGRPCBuild_SingleWeightedBackendRefNoWarnings pins the no-splitting
// case from issue #510 for GRPCRoute: one backendRef with an explicit
// weight involves no traffic splitting and must produce no warnings.
func TestGRPCBuild_SingleWeightedBackendRefNoWarnings(t *testing.T) {
	t.Parallel()

	logger, buf := logging.TestLogger(t)
	builder := ingress.NewGRPCBuilder("cluster.local", nil, nil, nil, logger)
	weight100 := int32(100)
	service := testGRPCService

	routes := []gatewayv1.GRPCRoute{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "single-weighted-grpc-route",
				Namespace: "default",
			},
			Spec: gatewayv1.GRPCRouteSpec{
				Hostnames: []gatewayv1.Hostname{"grpc.example.com"},
				Rules: []gatewayv1.GRPCRouteRule{
					{
						Matches: []gatewayv1.GRPCRouteMatch{
							{
								Method: &gatewayv1.GRPCMethodMatch{
									Service: &service,
								},
							},
						},
						BackendRefs: []gatewayv1.GRPCBackendRef{
							newGRPCBackendRefWithWeight("grpc-service1", &weight100, int32Ptr(9090)),
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

func TestGRPCBuild_WarnHeaderMatching(t *testing.T) {
	t.Parallel()

	logger, buf := logging.TestLogger(t)
	builder := ingress.NewGRPCBuilder("cluster.local", nil, nil, nil, logger)
	headerType := gatewayv1.GRPCHeaderMatchExact
	service := testGRPCService

	routes := []gatewayv1.GRPCRoute{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "header-grpc-route",
				Namespace: "default",
			},
			Spec: gatewayv1.GRPCRouteSpec{
				Hostnames: []gatewayv1.Hostname{"grpc.example.com"},
				Rules: []gatewayv1.GRPCRouteRule{
					{
						Matches: []gatewayv1.GRPCRouteMatch{
							{
								Method: &gatewayv1.GRPCMethodMatch{
									Service: &service,
								},
								Headers: []gatewayv1.GRPCHeaderMatch{
									{
										Type:  &headerType,
										Name:  "X-Custom-Header",
										Value: "custom-value",
									},
									{
										Type:  &headerType,
										Name:  "Authorization",
										Value: "Bearer token",
									},
								},
							},
						},
						BackendRefs: []gatewayv1.GRPCBackendRef{
							newGRPCBackendRefWithWeight("grpc-service1", nil, int32Ptr(9090)),
						},
					},
				},
			},
		},
	}

	_ = builder.Build(context.Background(), routes)

	logs := buf.String()
	assert.Contains(t, logs, "route configuration partially applied")
	assert.Contains(t, logs, `"route":"default/header-grpc-route"`)
	assert.Contains(t, logs, "header matching not supported")
	assert.Contains(t, logs, `"ignored_headers":2`)
}

func TestGRPCBuild_WarnFilters(t *testing.T) {
	t.Parallel()

	logger, buf := logging.TestLogger(t)
	builder := ingress.NewGRPCBuilder("cluster.local", nil, nil, nil, logger)
	filterType := gatewayv1.GRPCRouteFilterRequestHeaderModifier

	routes := []gatewayv1.GRPCRoute{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "filter-grpc-route",
				Namespace: "default",
			},
			Spec: gatewayv1.GRPCRouteSpec{
				Hostnames: []gatewayv1.Hostname{"grpc.example.com"},
				Rules: []gatewayv1.GRPCRouteRule{
					{
						Filters: []gatewayv1.GRPCRouteFilter{
							{
								Type: filterType,
								RequestHeaderModifier: &gatewayv1.HTTPHeaderFilter{
									Set: []gatewayv1.HTTPHeader{
										{
											Name:  "X-Custom-Header",
											Value: "value",
										},
									},
								},
							},
							{
								Type: gatewayv1.GRPCRouteFilterRequestMirror,
								RequestMirror: &gatewayv1.HTTPRequestMirrorFilter{
									BackendRef: gatewayv1.BackendObjectReference{
										Name: "mirror-service",
									},
								},
							},
						},
						BackendRefs: []gatewayv1.GRPCBackendRef{
							newGRPCBackendRefWithWeight("grpc-service1", nil, int32Ptr(9090)),
						},
					},
				},
			},
		},
	}

	_ = builder.Build(context.Background(), routes)

	logs := buf.String()
	assert.Contains(t, logs, "route configuration partially applied")
	assert.Contains(t, logs, `"route":"default/filter-grpc-route"`)
	assert.Contains(t, logs, "filters not supported")
	assert.Contains(t, logs, `"ignored_filters":2`)
}

func TestGRPCBuild_MultipleWarnings(t *testing.T) {
	t.Parallel()

	logger, buf := logging.TestLogger(t)
	builder := ingress.NewGRPCBuilder("cluster.local", nil, nil, nil, logger)
	headerType := gatewayv1.GRPCHeaderMatchExact
	weight50 := int32(50)
	service := testGRPCService

	routes := []gatewayv1.GRPCRoute{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "complex-grpc-route",
				Namespace: "default",
			},
			Spec: gatewayv1.GRPCRouteSpec{
				Hostnames: []gatewayv1.Hostname{"grpc.example.com"},
				Rules: []gatewayv1.GRPCRouteRule{
					{
						Matches: []gatewayv1.GRPCRouteMatch{
							{
								Method: &gatewayv1.GRPCMethodMatch{
									Service: &service,
								},
								Headers: []gatewayv1.GRPCHeaderMatch{
									{
										Type:  &headerType,
										Name:  "X-Test",
										Value: "test",
									},
								},
							},
						},
						BackendRefs: []gatewayv1.GRPCBackendRef{
							newGRPCBackendRefWithWeight("grpc-service1", &weight50, int32Ptr(9090)),
							newGRPCBackendRefWithWeight("grpc-service2", &weight50, int32Ptr(9090)),
						},
					},
				},
			},
		},
	}

	_ = builder.Build(context.Background(), routes)

	logs := buf.String()
	// Should have warnings for: multiple backends, headers.
	// Weight itself is fully honored by the L7 proxy and never warned about.
	assert.Contains(t, logs, "multiple backendRefs specified")
	assert.NotContains(t, logs, "backendRef weight ignored")
	assert.Contains(t, logs, "header matching not supported")
	// All warnings should reference the same route
	assert.GreaterOrEqual(t, strings.Count(logs, `"route":"default/complex-grpc-route"`), 2)
}

func TestGRPCBuild_NoWarningsForValidConfig(t *testing.T) {
	t.Parallel()

	logger, buf := logging.TestLogger(t)
	builder := ingress.NewGRPCBuilder("cluster.local", nil, nil, nil, logger)
	service := testGRPCService
	method := testGRPCMethod

	routes := []gatewayv1.GRPCRoute{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "valid-grpc-route",
				Namespace: "default",
			},
			Spec: gatewayv1.GRPCRouteSpec{
				Hostnames: []gatewayv1.Hostname{"grpc.example.com"},
				Rules: []gatewayv1.GRPCRouteRule{
					{
						Matches: []gatewayv1.GRPCRouteMatch{
							{
								Method: &gatewayv1.GRPCMethodMatch{
									Service: &service,
									Method:  &method,
								},
							},
						},
						BackendRefs: []gatewayv1.GRPCBackendRef{
							newGRPCBackendRefWithWeight("grpc-service1", nil, int32Ptr(9090)),
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

// newGRPCBackendRefWithWeight creates a GRPCBackendRef with optional weight.
func newGRPCBackendRefWithWeight(name string, weight *int32, port *int32) gatewayv1.GRPCBackendRef {
	ref := gatewayv1.GRPCBackendRef{
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
