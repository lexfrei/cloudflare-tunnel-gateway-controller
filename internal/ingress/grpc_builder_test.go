package ingress_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/ingress"
)

func TestNewGRPCBuilder(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		clusterDomain string
	}{
		{
			name:          "default cluster domain",
			clusterDomain: "cluster.local",
		},
		{
			name:          "custom cluster domain",
			clusterDomain: "my-cluster.example.com",
		},
		{
			name:          "empty cluster domain",
			clusterDomain: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			builder := ingress.NewGRPCBuilder(tt.clusterDomain, nil)
			require.NotNil(t, builder)
			assert.Equal(t, tt.clusterDomain, builder.ClusterDomain)
		})
	}
}

func TestGRPCBuild_EmptyRoutes(t *testing.T) {
	t.Parallel()

	builder := ingress.NewGRPCBuilder("cluster.local", nil)
	routes := []gatewayv1.GRPCRoute{}

	buildResult := builder.Build(context.Background(), routes)

	require.Empty(t, buildResult.Rules)
}

func TestGRPCBuild_SingleRoute(t *testing.T) {
	t.Parallel()

	builder := ingress.NewGRPCBuilder("cluster.local", nil)
	routes := []gatewayv1.GRPCRoute{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-route",
				Namespace: "default",
			},
			Spec: gatewayv1.GRPCRouteSpec{
				Hostnames: []gatewayv1.Hostname{"grpc.example.com"},
				Rules: []gatewayv1.GRPCRouteRule{
					{
						BackendRefs: []gatewayv1.GRPCBackendRef{
							newGRPCBackendRef("grpc-service", nil, int32Ptr(50051)),
						},
					},
				},
			},
		},
	}

	buildResult := builder.Build(context.Background(), routes)

	require.Len(t, buildResult.Rules, 1)
	assert.Equal(t, "grpc.example.com", buildResult.Rules[0].Hostname.Value)
	assert.Equal(t, "http://grpc-service.default.svc.cluster.local:50051", buildResult.Rules[0].Service.Value)
}

func TestGRPCBuild_ServiceMethodMatch(t *testing.T) {
	t.Parallel()

	builder := ingress.NewGRPCBuilder("cluster.local", nil)
	service := testGRPCService
	method := testGRPCMethod
	routes := []gatewayv1.GRPCRoute{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-route",
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
							newGRPCBackendRef("grpc-service", nil, int32Ptr(50051)),
						},
					},
				},
			},
		},
	}

	buildResult := builder.Build(context.Background(), routes)

	require.Len(t, buildResult.Rules, 1)
	assert.Equal(t, "/mypackage.MyService/GetUser", buildResult.Rules[0].Path.Value)
}

func TestGRPCBuild_ServiceOnlyMatch(t *testing.T) {
	t.Parallel()

	builder := ingress.NewGRPCBuilder("cluster.local", nil)
	service := testGRPCService
	routes := []gatewayv1.GRPCRoute{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-route",
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
							newGRPCBackendRef("grpc-service", nil, int32Ptr(50051)),
						},
					},
				},
			},
		},
	}

	buildResult := builder.Build(context.Background(), routes)

	require.Len(t, buildResult.Rules, 1)
	assert.Equal(t, "/mypackage.MyService/*", buildResult.Rules[0].Path.Value)
}

func TestGRPCBuild_NoMethodMatch(t *testing.T) {
	t.Parallel()

	builder := ingress.NewGRPCBuilder("cluster.local", nil)
	routes := []gatewayv1.GRPCRoute{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-route",
				Namespace: "default",
			},
			Spec: gatewayv1.GRPCRouteSpec{
				Hostnames: []gatewayv1.Hostname{"grpc.example.com"},
				Rules: []gatewayv1.GRPCRouteRule{
					{
						Matches: []gatewayv1.GRPCRouteMatch{
							{
								Method: nil,
							},
						},
						BackendRefs: []gatewayv1.GRPCBackendRef{
							newGRPCBackendRef("grpc-service", nil, int32Ptr(50051)),
						},
					},
				},
			},
		},
	}

	buildResult := builder.Build(context.Background(), routes)

	require.Len(t, buildResult.Rules, 1)
	assert.False(t, buildResult.Rules[0].Path.Present)
}

func TestGRPCBuild_MultipleHostnames(t *testing.T) {
	t.Parallel()

	builder := ingress.NewGRPCBuilder("cluster.local", nil)
	routes := []gatewayv1.GRPCRoute{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-route",
				Namespace: "default",
			},
			Spec: gatewayv1.GRPCRouteSpec{
				Hostnames: []gatewayv1.Hostname{"grpc1.example.com", "grpc2.example.com"},
				Rules: []gatewayv1.GRPCRouteRule{
					{
						BackendRefs: []gatewayv1.GRPCBackendRef{
							newGRPCBackendRef("grpc-service", nil, int32Ptr(50051)),
						},
					},
				},
			},
		},
	}

	buildResult := builder.Build(context.Background(), routes)

	require.Len(t, buildResult.Rules, 2)
	assert.Equal(t, "grpc1.example.com", buildResult.Rules[0].Hostname.Value)
	assert.Equal(t, "grpc2.example.com", buildResult.Rules[1].Hostname.Value)
}

func TestGRPCBuild_NoHostnames(t *testing.T) {
	t.Parallel()

	builder := ingress.NewGRPCBuilder("cluster.local", nil)
	routes := []gatewayv1.GRPCRoute{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-route",
				Namespace: "default",
			},
			Spec: gatewayv1.GRPCRouteSpec{
				Hostnames: []gatewayv1.Hostname{},
				Rules: []gatewayv1.GRPCRouteRule{
					{
						BackendRefs: []gatewayv1.GRPCBackendRef{
							newGRPCBackendRef("grpc-service", nil, int32Ptr(50051)),
						},
					},
				},
			},
		},
	}

	buildResult := builder.Build(context.Background(), routes)

	require.Len(t, buildResult.Rules, 1)
	assert.Equal(t, "*", buildResult.Rules[0].Hostname.Value)
}

func TestGRPCBuild_NoBackendRefs(t *testing.T) {
	t.Parallel()

	builder := ingress.NewGRPCBuilder("cluster.local", nil)
	routes := []gatewayv1.GRPCRoute{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-route",
				Namespace: "default",
			},
			Spec: gatewayv1.GRPCRouteSpec{
				Hostnames: []gatewayv1.Hostname{"grpc.example.com"},
				Rules: []gatewayv1.GRPCRouteRule{
					{
						BackendRefs: []gatewayv1.GRPCBackendRef{},
					},
				},
			},
		},
	}

	buildResult := builder.Build(context.Background(), routes)

	require.Empty(t, buildResult.Rules)
}

func TestGRPCBuild_Sorting(t *testing.T) {
	t.Parallel()

	builder := ingress.NewGRPCBuilder("cluster.local", nil)
	serviceA := "a.Service"
	serviceB := "b.Service"
	methodX := "MethodX"
	routes := []gatewayv1.GRPCRoute{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-route",
				Namespace: "default",
			},
			Spec: gatewayv1.GRPCRouteSpec{
				Hostnames: []gatewayv1.Hostname{"grpc.example.com"},
				Rules: []gatewayv1.GRPCRouteRule{
					{
						Matches: []gatewayv1.GRPCRouteMatch{
							{
								Method: &gatewayv1.GRPCMethodMatch{
									Service: &serviceB,
								},
							},
						},
						BackendRefs: []gatewayv1.GRPCBackendRef{
							newGRPCBackendRef("prefix-service", nil, int32Ptr(50051)),
						},
					},
					{
						Matches: []gatewayv1.GRPCRouteMatch{
							{
								Method: &gatewayv1.GRPCMethodMatch{
									Service: &serviceA,
									Method:  &methodX,
								},
							},
						},
						BackendRefs: []gatewayv1.GRPCBackendRef{
							newGRPCBackendRef("exact-service", nil, int32Ptr(50051)),
						},
					},
				},
			},
		},
	}

	buildResult := builder.Build(context.Background(), routes)

	require.Len(t, buildResult.Rules, 2)
	assert.Contains(t, buildResult.Rules[0].Service.Value, "exact-service")
	assert.Contains(t, buildResult.Rules[1].Service.Value, "prefix-service")
}

func TestGRPCBuild_CustomNamespace(t *testing.T) {
	t.Parallel()

	builder := ingress.NewGRPCBuilder("cluster.local", nil)
	ns := gatewayv1.Namespace("other-namespace")
	routes := []gatewayv1.GRPCRoute{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-route",
				Namespace: "default",
			},
			Spec: gatewayv1.GRPCRouteSpec{
				Hostnames: []gatewayv1.Hostname{"grpc.example.com"},
				Rules: []gatewayv1.GRPCRouteRule{
					{
						BackendRefs: []gatewayv1.GRPCBackendRef{
							newGRPCBackendRef("grpc-service", &ns, int32Ptr(50051)),
						},
					},
				},
			},
		},
	}

	buildResult := builder.Build(context.Background(), routes)

	require.Len(t, buildResult.Rules, 1)
	assert.Equal(t, "http://grpc-service.other-namespace.svc.cluster.local:50051", buildResult.Rules[0].Service.Value)
}

func TestGRPCBuild_HTTPSPort(t *testing.T) {
	t.Parallel()

	builder := ingress.NewGRPCBuilder("cluster.local", nil)
	routes := []gatewayv1.GRPCRoute{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-route",
				Namespace: "default",
			},
			Spec: gatewayv1.GRPCRouteSpec{
				Hostnames: []gatewayv1.Hostname{"grpc.example.com"},
				Rules: []gatewayv1.GRPCRouteRule{
					{
						BackendRefs: []gatewayv1.GRPCBackendRef{
							newGRPCBackendRef("grpc-service", nil, int32Ptr(443)),
						},
					},
				},
			},
		},
	}

	buildResult := builder.Build(context.Background(), routes)

	require.Len(t, buildResult.Rules, 1)
	assert.Equal(t, "https://grpc-service.default.svc.cluster.local:443", buildResult.Rules[0].Service.Value)
}

func TestGRPCBuild_DefaultPort(t *testing.T) {
	t.Parallel()

	builder := ingress.NewGRPCBuilder("cluster.local", nil)
	routes := []gatewayv1.GRPCRoute{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-route",
				Namespace: "default",
			},
			Spec: gatewayv1.GRPCRouteSpec{
				Hostnames: []gatewayv1.Hostname{"grpc.example.com"},
				Rules: []gatewayv1.GRPCRouteRule{
					{
						BackendRefs: []gatewayv1.GRPCBackendRef{
							newGRPCBackendRef("grpc-service", nil, nil),
						},
					},
				},
			},
		},
	}

	buildResult := builder.Build(context.Background(), routes)

	require.Len(t, buildResult.Rules, 1)
	assert.Equal(t, "http://grpc-service.default.svc.cluster.local:80", buildResult.Rules[0].Service.Value)
}

func TestGRPCBuild_NonServiceBackend(t *testing.T) {
	t.Parallel()

	builder := ingress.NewGRPCBuilder("cluster.local", nil)
	kind := gatewayv1.Kind("Deployment")
	routes := []gatewayv1.GRPCRoute{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-route",
				Namespace: "default",
			},
			Spec: gatewayv1.GRPCRouteSpec{
				Hostnames: []gatewayv1.Hostname{"grpc.example.com"},
				Rules: []gatewayv1.GRPCRouteRule{
					{
						BackendRefs: []gatewayv1.GRPCBackendRef{
							{
								BackendRef: gatewayv1.BackendRef{
									BackendObjectReference: gatewayv1.BackendObjectReference{
										Kind: &kind,
										Name: "my-deployment",
									},
								},
							},
						},
					},
				},
			},
		},
	}

	buildResult := builder.Build(context.Background(), routes)

	require.Empty(t, buildResult.Rules)
}

func TestGRPCBuild_NonCoreGroup(t *testing.T) {
	t.Parallel()

	builder := ingress.NewGRPCBuilder("cluster.local", nil)
	group := gatewayv1.Group("apps")
	routes := []gatewayv1.GRPCRoute{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-route",
				Namespace: "default",
			},
			Spec: gatewayv1.GRPCRouteSpec{
				Hostnames: []gatewayv1.Hostname{"grpc.example.com"},
				Rules: []gatewayv1.GRPCRouteRule{
					{
						BackendRefs: []gatewayv1.GRPCBackendRef{
							{
								BackendRef: gatewayv1.BackendRef{
									BackendObjectReference: gatewayv1.BackendObjectReference{
										Group: &group,
										Name:  "grpc-service",
									},
								},
							},
						},
					},
				},
			},
		},
	}

	buildResult := builder.Build(context.Background(), routes)

	require.Empty(t, buildResult.Rules)
}

func TestGRPCBuild_MethodOnlyMatch(t *testing.T) {
	t.Parallel()

	builder := ingress.NewGRPCBuilder("cluster.local", nil)
	method := testGRPCMethod
	routes := []gatewayv1.GRPCRoute{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-route",
				Namespace: "default",
			},
			Spec: gatewayv1.GRPCRouteSpec{
				Hostnames: []gatewayv1.Hostname{"grpc.example.com"},
				Rules: []gatewayv1.GRPCRouteRule{
					{
						Matches: []gatewayv1.GRPCRouteMatch{
							{
								Method: &gatewayv1.GRPCMethodMatch{
									Method: &method,
								},
							},
						},
						BackendRefs: []gatewayv1.GRPCBackendRef{
							newGRPCBackendRef("grpc-service", nil, int32Ptr(50051)),
						},
					},
				},
			},
		},
	}

	buildResult := builder.Build(context.Background(), routes)

	require.Len(t, buildResult.Rules, 1)
	assert.False(t, buildResult.Rules[0].Path.Present)
}

func TestGRPCBuild_EmptyServiceAndMethod(t *testing.T) {
	t.Parallel()

	builder := ingress.NewGRPCBuilder("cluster.local", nil)
	emptyService := ""
	emptyMethod := ""
	routes := []gatewayv1.GRPCRoute{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-route",
				Namespace: "default",
			},
			Spec: gatewayv1.GRPCRouteSpec{
				Hostnames: []gatewayv1.Hostname{"grpc.example.com"},
				Rules: []gatewayv1.GRPCRouteRule{
					{
						Matches: []gatewayv1.GRPCRouteMatch{
							{
								Method: &gatewayv1.GRPCMethodMatch{
									Service: &emptyService,
									Method:  &emptyMethod,
								},
							},
						},
						BackendRefs: []gatewayv1.GRPCBackendRef{
							newGRPCBackendRef("grpc-service", nil, int32Ptr(50051)),
						},
					},
				},
			},
		},
	}

	buildResult := builder.Build(context.Background(), routes)

	require.Len(t, buildResult.Rules, 1)
	assert.False(t, buildResult.Rules[0].Path.Present)
}

func TestGRPCBuild_MultipleRoutes(t *testing.T) {
	t.Parallel()

	builder := ingress.NewGRPCBuilder("cluster.local", nil)
	routes := []gatewayv1.GRPCRoute{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "route-1",
				Namespace: "default",
			},
			Spec: gatewayv1.GRPCRouteSpec{
				Hostnames: []gatewayv1.Hostname{"grpc1.example.com"},
				Rules: []gatewayv1.GRPCRouteRule{
					{
						BackendRefs: []gatewayv1.GRPCBackendRef{
							newGRPCBackendRef("service-1", nil, int32Ptr(50051)),
						},
					},
				},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "route-2",
				Namespace: "default",
			},
			Spec: gatewayv1.GRPCRouteSpec{
				Hostnames: []gatewayv1.Hostname{"grpc2.example.com"},
				Rules: []gatewayv1.GRPCRouteRule{
					{
						BackendRefs: []gatewayv1.GRPCBackendRef{
							newGRPCBackendRef("service-2", nil, int32Ptr(50052)),
						},
					},
				},
			},
		},
	}

	buildResult := builder.Build(context.Background(), routes)

	require.Len(t, buildResult.Rules, 2)
	assert.Equal(t, "grpc1.example.com", buildResult.Rules[0].Hostname.Value)
	assert.Equal(t, "grpc2.example.com", buildResult.Rules[1].Hostname.Value)
}

func TestGRPCBuild_WeightSelection(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		backendWeights  []*int32
		expectedService string
	}{
		{
			name:            "highest weight wins",
			backendWeights:  []*int32{int32Ptr(20), int32Ptr(80)},
			expectedService: "svc-b",
		},
		{
			name:            "equal weights uses first",
			backendWeights:  []*int32{int32Ptr(50), int32Ptr(50)},
			expectedService: "svc-a",
		},
		{
			name:            "nil weights use default and first wins",
			backendWeights:  []*int32{nil, nil},
			expectedService: "svc-a",
		},
		{
			name:            "mixed weights selects highest",
			backendWeights:  []*int32{nil, int32Ptr(100), int32Ptr(50)},
			expectedService: "svc-b",
		},
		{
			name:            "zero weight loses to default",
			backendWeights:  []*int32{int32Ptr(0), nil},
			expectedService: "svc-b",
		},
		{
			name:            "single backend with weight",
			backendWeights:  []*int32{int32Ptr(100)},
			expectedService: "svc-a",
		},
		{
			name:            "single backend without weight",
			backendWeights:  []*int32{nil},
			expectedService: "svc-a",
		},
	}

	serviceNames := []string{"svc-a", "svc-b", "svc-c"}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			builder := ingress.NewGRPCBuilder("cluster.local", nil)

			backendRefs := make([]gatewayv1.GRPCBackendRef, len(tt.backendWeights))
			for i, w := range tt.backendWeights {
				backendRefs[i] = gatewayv1.GRPCBackendRef{
					BackendRef: gatewayv1.BackendRef{
						BackendObjectReference: gatewayv1.BackendObjectReference{
							Name: gatewayv1.ObjectName(serviceNames[i]),
							Port: portNumPtr(50051),
						},
						Weight: w,
					},
				}
			}

			routes := []gatewayv1.GRPCRoute{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-route",
						Namespace: "default",
					},
					Spec: gatewayv1.GRPCRouteSpec{
						Hostnames: []gatewayv1.Hostname{"grpc.example.com"},
						Rules: []gatewayv1.GRPCRouteRule{
							{
								BackendRefs: backendRefs,
							},
						},
					},
				},
			}

			buildResult := builder.Build(context.Background(), routes)

			require.Len(t, buildResult.Rules, 1)
			assert.Contains(t, buildResult.Rules[0].Service.Value, tt.expectedService)
		})
	}
}

func TestGRPCBuild_AllBackendsDisabled(t *testing.T) {
	t.Parallel()

	builder := ingress.NewGRPCBuilder("cluster.local", nil)
	routes := []gatewayv1.GRPCRoute{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-route",
				Namespace: "default",
			},
			Spec: gatewayv1.GRPCRouteSpec{
				Hostnames: []gatewayv1.Hostname{"grpc.example.com"},
				Rules: []gatewayv1.GRPCRouteRule{
					{
						BackendRefs: []gatewayv1.GRPCBackendRef{
							{
								BackendRef: gatewayv1.BackendRef{
									BackendObjectReference: gatewayv1.BackendObjectReference{
										Name: "disabled-svc",
										Port: portNumPtr(50051),
									},
									Weight: int32Ptr(0),
								},
							},
						},
					},
				},
			},
		},
	}

	buildResult := builder.Build(context.Background(), routes)

	// No rules should be present (GRPCBuilder doesn't add catch-all)
	require.Empty(t, buildResult.Rules)
}

func newGRPCBackendRef(name string, namespace *gatewayv1.Namespace, port *int32) gatewayv1.GRPCBackendRef {
	ref := gatewayv1.GRPCBackendRef{
		BackendRef: gatewayv1.BackendRef{
			BackendObjectReference: gatewayv1.BackendObjectReference{
				Name:      gatewayv1.ObjectName(name),
				Namespace: namespace,
			},
		},
	}
	if port != nil {
		portNum := gatewayv1.PortNumber(*port)
		ref.BackendRef.Port = &portNum
	}

	return ref
}
