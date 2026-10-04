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

const (
	testGRPCService = "mypackage.MyService"
	testGRPCMethod  = "GetUser"
)

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
	assert.Empty(t, logs, "weighted backends are served by the proxy and must not warn")
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

func TestGRPCBuild_ProxyOnlyFeaturesAreNotLogged(t *testing.T) {
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

	// The document carries hostnames only, so nothing a route can declare is
	// a reduction of it worth logging.
	assert.Empty(t, buf.String())
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
