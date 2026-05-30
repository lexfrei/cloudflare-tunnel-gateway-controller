package ingress_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/ingress"
)

// mbClusterIPService builds a minimal ClusterIP Service so the builder's
// existence check passes for the valid backend.
func mbClusterIPService(name, namespace string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: corev1.ServiceSpec{
			Type:      corev1.ServiceTypeClusterIP,
			ClusterIP: "10.0.0.1",
		},
	}
}

// weightedHTTPBackendRef builds an HTTPBackendRef with an explicit port and
// weight (the shared newHTTPBackendRef helper does not set a weight).
func weightedHTTPBackendRef(name string, port, weight int32) gatewayv1.HTTPBackendRef {
	ref := newHTTPBackendRef(name, nil, int32Ptr(port))
	ref.Weight = int32Ptr(weight)

	return ref
}

// weightedGRPCBackendRef builds a GRPCBackendRef with an explicit port and weight.
func weightedGRPCBackendRef(name string, port, weight int32) gatewayv1.GRPCBackendRef {
	ref := newGRPCBackendRef(name, nil, int32Ptr(port))
	ref.Weight = int32Ptr(weight)

	return ref
}

// TestBuild_MultiBackend_InvalidLowWeightReported pins the HTTP multi-backend
// behaviour: a rule with a valid high-weight backend and a nonexistent
// low-weight backend must report a FailedRef for the low-weight backend (with
// its port), so the proxy can mark it 500 for its traffic fraction. Before the
// fix only the highest-weight backend is validated, so FailedRefs is empty and
// this fails.
func TestBuild_MultiBackend_InvalidLowWeightReported(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	cli := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mbClusterIPService("valid-svc", "default")).
		Build()

	route := gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "default"},
		Spec: gatewayv1.HTTPRouteSpec{
			Rules: []gatewayv1.HTTPRouteRule{
				{
					BackendRefs: []gatewayv1.HTTPBackendRef{
						weightedHTTPBackendRef("valid-svc", 80, 80),
						weightedHTTPBackendRef("missing-svc", 8080, 20),
					},
				},
			},
		},
	}

	builder := ingress.NewBuilder("cluster.local", nil, cli, nil, nil)
	result := builder.Build(context.Background(), []gatewayv1.HTTPRoute{route})

	require.Len(t, result.FailedRefs, 1, "the invalid low-weight backend must be reported")
	assert.Equal(t, "missing-svc", result.FailedRefs[0].BackendName)
	assert.Equal(t, "default", result.FailedRefs[0].RouteNamespace)
	assert.Equal(t, "r", result.FailedRefs[0].RouteName)
	assert.Equal(t, int32(8080), result.FailedRefs[0].Port,
		"the failed ref must carry the backend port so the proxy can map it to the right backend")
}

// TestBuild_MultiBackend_GRPC_InvalidLowWeightReported is the GRPCRoute
// counterpart.
func TestBuild_MultiBackend_GRPC_InvalidLowWeightReported(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	cli := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mbClusterIPService("valid-grpc", "default")).
		Build()

	route := gatewayv1.GRPCRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "g", Namespace: "default"},
		Spec: gatewayv1.GRPCRouteSpec{
			Rules: []gatewayv1.GRPCRouteRule{
				{
					BackendRefs: []gatewayv1.GRPCBackendRef{
						weightedGRPCBackendRef("valid-grpc", 9000, 80),
						weightedGRPCBackendRef("missing-grpc", 9001, 20),
					},
				},
			},
		},
	}

	builder := ingress.NewGRPCBuilder("cluster.local", nil, cli, nil, nil)
	result := builder.Build(context.Background(), []gatewayv1.GRPCRoute{route})

	require.Len(t, result.FailedRefs, 1)
	assert.Equal(t, "missing-grpc", result.FailedRefs[0].BackendName)
	assert.Equal(t, int32(9001), result.FailedRefs[0].Port)
}
