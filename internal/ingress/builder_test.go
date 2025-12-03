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
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/ingress"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/referencegrant"
)

func TestNewBuilder(t *testing.T) {
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

			builder := ingress.NewBuilder(tt.clusterDomain, nil, nil, nil, nil)
			require.NotNil(t, builder)
			assert.Equal(t, tt.clusterDomain, builder.ClusterDomain)
		})
	}
}

func TestBuild_EmptyRoutes(t *testing.T) {
	t.Parallel()

	builder := ingress.NewBuilder("cluster.local", nil, nil, nil, nil)
	routes := []gatewayv1.HTTPRoute{}

	buildResult := builder.Build(context.Background(), routes)

	require.Len(t, buildResult.Rules, 1)
	assert.Equal(t, ingress.CatchAllService, buildResult.Rules[0].Service.Value)
	assert.Empty(t, buildResult.FailedRefs)
}

func TestBuild_SingleRoute(t *testing.T) {
	t.Parallel()

	builder := ingress.NewBuilder("cluster.local", nil, nil, nil, nil)
	routes := []gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-route",
				Namespace: "default",
			},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"app.example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						BackendRefs: []gatewayv1.HTTPBackendRef{
							newHTTPBackendRef("my-service", nil, int32Ptr(8080)),
						},
					},
				},
			},
		},
	}

	buildResult := builder.Build(context.Background(), routes)

	require.Len(t, buildResult.Rules, 2)
	assert.Equal(t, "app.example.com", buildResult.Rules[0].Hostname.Value)
	assert.Equal(t, "http://my-service.default.svc.cluster.local:8080", buildResult.Rules[0].Service.Value)
	assert.Equal(t, ingress.CatchAllService, buildResult.Rules[1].Service.Value)
	assert.Empty(t, buildResult.FailedRefs)
}

func TestBuild_MultipleHostnames(t *testing.T) {
	t.Parallel()

	builder := ingress.NewBuilder("cluster.local", nil, nil, nil, nil)
	routes := []gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-route",
				Namespace: "default",
			},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"app1.example.com", "app2.example.com", "app3.example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						BackendRefs: []gatewayv1.HTTPBackendRef{
							newHTTPBackendRef("my-service", nil, int32Ptr(8080)),
						},
					},
				},
			},
		},
	}

	buildResult := builder.Build(context.Background(), routes)

	require.Len(t, buildResult.Rules, 4)
	assert.Equal(t, "app1.example.com", buildResult.Rules[0].Hostname.Value)
	assert.Equal(t, "app2.example.com", buildResult.Rules[1].Hostname.Value)
	assert.Equal(t, "app3.example.com", buildResult.Rules[2].Hostname.Value)
	assert.Equal(t, ingress.CatchAllService, buildResult.Rules[3].Service.Value)
}

func TestBuild_MultipleRoutes(t *testing.T) {
	t.Parallel()

	builder := ingress.NewBuilder("cluster.local", nil, nil, nil, nil)
	routes := []gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "route-1",
				Namespace: "default",
			},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"app1.example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						BackendRefs: []gatewayv1.HTTPBackendRef{
							newHTTPBackendRef("service-1", nil, int32Ptr(8080)),
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
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"app2.example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						BackendRefs: []gatewayv1.HTTPBackendRef{
							newHTTPBackendRef("service-2", nil, int32Ptr(9090)),
						},
					},
				},
			},
		},
	}

	buildResult := builder.Build(context.Background(), routes)

	require.Len(t, buildResult.Rules, 3)
	assert.Equal(t, "app1.example.com", buildResult.Rules[0].Hostname.Value)
	assert.Equal(t, "app2.example.com", buildResult.Rules[1].Hostname.Value)
	assert.Equal(t, ingress.CatchAllService, buildResult.Rules[2].Service.Value)
}

func TestBuild_PathMatching_Exact(t *testing.T) {
	t.Parallel()

	builder := ingress.NewBuilder("cluster.local", nil, nil, nil, nil)
	exactType := gatewayv1.PathMatchExact
	routes := []gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-route",
				Namespace: "default",
			},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"app.example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						Matches: []gatewayv1.HTTPRouteMatch{
							{
								Path: &gatewayv1.HTTPPathMatch{
									Type:  &exactType,
									Value: strPtr("/api/v1"),
								},
							},
						},
						BackendRefs: []gatewayv1.HTTPBackendRef{
							newHTTPBackendRef("my-service", nil, int32Ptr(8080)),
						},
					},
				},
			},
		},
	}

	buildResult := builder.Build(context.Background(), routes)

	require.Len(t, buildResult.Rules, 2)
	assert.Equal(t, "/api/v1", buildResult.Rules[0].Path.Value)
}

func TestBuild_PathMatching_Prefix(t *testing.T) {
	t.Parallel()

	builder := ingress.NewBuilder("cluster.local", nil, nil, nil, nil)
	prefixType := gatewayv1.PathMatchPathPrefix
	routes := []gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-route",
				Namespace: "default",
			},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"app.example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						Matches: []gatewayv1.HTTPRouteMatch{
							{
								Path: &gatewayv1.HTTPPathMatch{
									Type:  &prefixType,
									Value: strPtr("/api"),
								},
							},
						},
						BackendRefs: []gatewayv1.HTTPBackendRef{
							newHTTPBackendRef("my-service", nil, int32Ptr(8080)),
						},
					},
				},
			},
		},
	}

	buildResult := builder.Build(context.Background(), routes)

	require.Len(t, buildResult.Rules, 2)
	assert.Equal(t, "/api*", buildResult.Rules[0].Path.Value)
}

func TestBuild_Sorting(t *testing.T) {
	t.Parallel()

	builder := ingress.NewBuilder("cluster.local", nil, nil, nil, nil)
	exactType := gatewayv1.PathMatchExact
	prefixType := gatewayv1.PathMatchPathPrefix
	routes := []gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-route",
				Namespace: "default",
			},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"app.example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						Matches: []gatewayv1.HTTPRouteMatch{
							{
								Path: &gatewayv1.HTTPPathMatch{
									Type:  &prefixType,
									Value: strPtr("/api"),
								},
							},
						},
						BackendRefs: []gatewayv1.HTTPBackendRef{
							newHTTPBackendRef("prefix-service", nil, int32Ptr(8080)),
						},
					},
					{
						Matches: []gatewayv1.HTTPRouteMatch{
							{
								Path: &gatewayv1.HTTPPathMatch{
									Type:  &exactType,
									Value: strPtr("/api/v1/specific"),
								},
							},
						},
						BackendRefs: []gatewayv1.HTTPBackendRef{
							newHTTPBackendRef("exact-service", nil, int32Ptr(8080)),
						},
					},
					{
						Matches: []gatewayv1.HTTPRouteMatch{
							{
								Path: &gatewayv1.HTTPPathMatch{
									Type:  &prefixType,
									Value: strPtr("/api/v1"),
								},
							},
						},
						BackendRefs: []gatewayv1.HTTPBackendRef{
							newHTTPBackendRef("longer-prefix-service", nil, int32Ptr(8080)),
						},
					},
				},
			},
		},
	}

	buildResult := builder.Build(context.Background(), routes)

	require.Len(t, buildResult.Rules, 4)
	assert.Contains(t, buildResult.Rules[0].Service.Value, "exact-service")
	assert.Contains(t, buildResult.Rules[1].Service.Value, "longer-prefix-service")
	assert.Contains(t, buildResult.Rules[2].Service.Value, "prefix-service")
	assert.Equal(t, ingress.CatchAllService, buildResult.Rules[3].Service.Value)
}

func TestBuild_NoHostnames(t *testing.T) {
	t.Parallel()

	builder := ingress.NewBuilder("cluster.local", nil, nil, nil, nil)
	routes := []gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-route",
				Namespace: "default",
			},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						BackendRefs: []gatewayv1.HTTPBackendRef{
							newHTTPBackendRef("my-service", nil, int32Ptr(8080)),
						},
					},
				},
			},
		},
	}

	buildResult := builder.Build(context.Background(), routes)

	require.Len(t, buildResult.Rules, 2)
	assert.Equal(t, "*", buildResult.Rules[0].Hostname.Value)
}

func TestBuild_NoBackendRefs(t *testing.T) {
	t.Parallel()

	builder := ingress.NewBuilder("cluster.local", nil, nil, nil, nil)
	routes := []gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-route",
				Namespace: "default",
			},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"app.example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						BackendRefs: []gatewayv1.HTTPBackendRef{},
					},
				},
			},
		},
	}

	buildResult := builder.Build(context.Background(), routes)

	require.Len(t, buildResult.Rules, 1)
	assert.Equal(t, ingress.CatchAllService, buildResult.Rules[0].Service.Value)
}

func TestBuild_NonServiceBackend(t *testing.T) {
	t.Parallel()

	builder := ingress.NewBuilder("cluster.local", nil, nil, nil, nil)
	kind := gatewayv1.Kind("Deployment")
	routes := []gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-route",
				Namespace: "default",
			},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"app.example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						BackendRefs: []gatewayv1.HTTPBackendRef{
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

	require.Len(t, buildResult.Rules, 1)
	assert.Equal(t, ingress.CatchAllService, buildResult.Rules[0].Service.Value)
}

func TestBuild_NonCoreGroup(t *testing.T) {
	t.Parallel()

	builder := ingress.NewBuilder("cluster.local", nil, nil, nil, nil)
	group := gatewayv1.Group("apps")
	routes := []gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-route",
				Namespace: "default",
			},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"app.example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						BackendRefs: []gatewayv1.HTTPBackendRef{
							{
								BackendRef: gatewayv1.BackendRef{
									BackendObjectReference: gatewayv1.BackendObjectReference{
										Group: &group,
										Name:  "my-service",
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

	require.Len(t, buildResult.Rules, 1)
	assert.Equal(t, ingress.CatchAllService, buildResult.Rules[0].Service.Value)
}

func TestBuild_CustomNamespace(t *testing.T) {
	t.Parallel()

	builder := ingress.NewBuilder("cluster.local", nil, nil, nil, nil)
	ns := gatewayv1.Namespace("other-namespace")
	routes := []gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-route",
				Namespace: "default",
			},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"app.example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						BackendRefs: []gatewayv1.HTTPBackendRef{
							newHTTPBackendRef("my-service", &ns, int32Ptr(8080)),
						},
					},
				},
			},
		},
	}

	buildResult := builder.Build(context.Background(), routes)

	require.Len(t, buildResult.Rules, 2)
	assert.Contains(t, buildResult.Rules[0].Service.Value, "other-namespace")
	assert.Equal(t, "http://my-service.other-namespace.svc.cluster.local:8080", buildResult.Rules[0].Service.Value)
}

func TestBuild_CustomPort(t *testing.T) {
	t.Parallel()

	builder := ingress.NewBuilder("cluster.local", nil, nil, nil, nil)
	routes := []gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-route",
				Namespace: "default",
			},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"app.example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						BackendRefs: []gatewayv1.HTTPBackendRef{
							newHTTPBackendRef("my-service", nil, int32Ptr(9090)),
						},
					},
				},
			},
		},
	}

	buildResult := builder.Build(context.Background(), routes)

	require.Len(t, buildResult.Rules, 2)
	assert.Equal(t, "http://my-service.default.svc.cluster.local:9090", buildResult.Rules[0].Service.Value)
}

func TestBuild_HTTPSPort(t *testing.T) {
	t.Parallel()

	builder := ingress.NewBuilder("cluster.local", nil, nil, nil, nil)
	routes := []gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-route",
				Namespace: "default",
			},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"app.example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						BackendRefs: []gatewayv1.HTTPBackendRef{
							newHTTPBackendRef("my-service", nil, int32Ptr(443)),
						},
					},
				},
			},
		},
	}

	buildResult := builder.Build(context.Background(), routes)

	require.Len(t, buildResult.Rules, 2)
	assert.Equal(t, "https://my-service.default.svc.cluster.local:443", buildResult.Rules[0].Service.Value)
}

func TestBuild_DefaultPort(t *testing.T) {
	t.Parallel()

	builder := ingress.NewBuilder("cluster.local", nil, nil, nil, nil)
	routes := []gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-route",
				Namespace: "default",
			},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"app.example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						BackendRefs: []gatewayv1.HTTPBackendRef{
							newHTTPBackendRef("my-service", nil, nil),
						},
					},
				},
			},
		},
	}

	buildResult := builder.Build(context.Background(), routes)

	require.Len(t, buildResult.Rules, 2)
	assert.Equal(t, "http://my-service.default.svc.cluster.local:80", buildResult.Rules[0].Service.Value)
}

func TestBuild_NoPathMatches(t *testing.T) {
	t.Parallel()

	builder := ingress.NewBuilder("cluster.local", nil, nil, nil, nil)
	routes := []gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-route",
				Namespace: "default",
			},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"app.example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						Matches: []gatewayv1.HTTPRouteMatch{},
						BackendRefs: []gatewayv1.HTTPBackendRef{
							newHTTPBackendRef("my-service", nil, int32Ptr(8080)),
						},
					},
				},
			},
		},
	}

	buildResult := builder.Build(context.Background(), routes)

	require.Len(t, buildResult.Rules, 2)
	assert.False(t, buildResult.Rules[0].Path.Present)
}

func TestBuild_NilPathMatch(t *testing.T) {
	t.Parallel()

	builder := ingress.NewBuilder("cluster.local", nil, nil, nil, nil)
	routes := []gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-route",
				Namespace: "default",
			},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"app.example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						Matches: []gatewayv1.HTTPRouteMatch{
							{
								Path: nil,
							},
						},
						BackendRefs: []gatewayv1.HTTPBackendRef{
							newHTTPBackendRef("my-service", nil, int32Ptr(8080)),
						},
					},
				},
			},
		},
	}

	buildResult := builder.Build(context.Background(), routes)

	require.Len(t, buildResult.Rules, 2)
	assert.False(t, buildResult.Rules[0].Path.Present)
}

func TestBuild_RegularExpressionPath(t *testing.T) {
	t.Parallel()

	builder := ingress.NewBuilder("cluster.local", nil, nil, nil, nil)
	regexType := gatewayv1.PathMatchRegularExpression
	routes := []gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-route",
				Namespace: "default",
			},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"app.example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						Matches: []gatewayv1.HTTPRouteMatch{
							{
								Path: &gatewayv1.HTTPPathMatch{
									Type:  &regexType,
									Value: strPtr("/api/[0-9]+"),
								},
							},
						},
						BackendRefs: []gatewayv1.HTTPBackendRef{
							newHTTPBackendRef("my-service", nil, int32Ptr(8080)),
						},
					},
				},
			},
		},
	}

	buildResult := builder.Build(context.Background(), routes)

	require.Len(t, buildResult.Rules, 2)
	assert.Equal(t, "/api/[0-9]+*", buildResult.Rules[0].Path.Value)
}

func TestBuild_CustomClusterDomain(t *testing.T) {
	t.Parallel()

	builder := ingress.NewBuilder("my-cluster.example.com", nil, nil, nil, nil)
	routes := []gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-route",
				Namespace: "default",
			},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"app.example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						BackendRefs: []gatewayv1.HTTPBackendRef{
							newHTTPBackendRef("my-service", nil, int32Ptr(8080)),
						},
					},
				},
			},
		},
	}

	buildResult := builder.Build(context.Background(), routes)

	require.Len(t, buildResult.Rules, 2)
	assert.Equal(t, "http://my-service.default.svc.my-cluster.example.com:8080", buildResult.Rules[0].Service.Value)
}

func TestBuild_SortingByHostname(t *testing.T) {
	t.Parallel()

	builder := ingress.NewBuilder("cluster.local", nil, nil, nil, nil)
	routes := []gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "route-z",
				Namespace: "default",
			},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"z.example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						BackendRefs: []gatewayv1.HTTPBackendRef{
							newHTTPBackendRef("z-service", nil, int32Ptr(8080)),
						},
					},
				},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "route-a",
				Namespace: "default",
			},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"a.example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						BackendRefs: []gatewayv1.HTTPBackendRef{
							newHTTPBackendRef("a-service", nil, int32Ptr(8080)),
						},
					},
				},
			},
		},
	}

	buildResult := builder.Build(context.Background(), routes)

	require.Len(t, buildResult.Rules, 3)
	assert.Equal(t, "a.example.com", buildResult.Rules[0].Hostname.Value)
	assert.Equal(t, "z.example.com", buildResult.Rules[1].Hostname.Value)
}

func TestBuild_CoreGroupExplicit(t *testing.T) {
	t.Parallel()

	builder := ingress.NewBuilder("cluster.local", nil, nil, nil, nil)
	coreGroup := gatewayv1.Group("core")
	routes := []gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-route",
				Namespace: "default",
			},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"app.example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						BackendRefs: []gatewayv1.HTTPBackendRef{
							{
								BackendRef: gatewayv1.BackendRef{
									BackendObjectReference: gatewayv1.BackendObjectReference{
										Group: &coreGroup,
										Name:  "my-service",
										Port:  portNumPtr(8080),
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

	require.Len(t, buildResult.Rules, 2)
	assert.Contains(t, buildResult.Rules[0].Service.Value, "my-service")
}

func TestBuild_EmptyGroupExplicit(t *testing.T) {
	t.Parallel()

	builder := ingress.NewBuilder("cluster.local", nil, nil, nil, nil)
	emptyGroup := gatewayv1.Group("")
	routes := []gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-route",
				Namespace: "default",
			},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"app.example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						BackendRefs: []gatewayv1.HTTPBackendRef{
							{
								BackendRef: gatewayv1.BackendRef{
									BackendObjectReference: gatewayv1.BackendObjectReference{
										Group: &emptyGroup,
										Name:  "my-service",
										Port:  portNumPtr(8080),
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

	require.Len(t, buildResult.Rules, 2)
	assert.Contains(t, buildResult.Rules[0].Service.Value, "my-service")
}

func TestBuild_ServiceKindExplicit(t *testing.T) {
	t.Parallel()

	builder := ingress.NewBuilder("cluster.local", nil, nil, nil, nil)
	serviceKind := gatewayv1.Kind("Service")
	routes := []gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-route",
				Namespace: "default",
			},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"app.example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						BackendRefs: []gatewayv1.HTTPBackendRef{
							{
								BackendRef: gatewayv1.BackendRef{
									BackendObjectReference: gatewayv1.BackendObjectReference{
										Kind: &serviceKind,
										Name: "my-service",
										Port: portNumPtr(8080),
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

	require.Len(t, buildResult.Rules, 2)
	assert.Contains(t, buildResult.Rules[0].Service.Value, "my-service")
}

func TestBuild_RootPath(t *testing.T) {
	t.Parallel()

	builder := ingress.NewBuilder("cluster.local", nil, nil, nil, nil)
	prefixType := gatewayv1.PathMatchPathPrefix
	routes := []gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-route",
				Namespace: "default",
			},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"app.example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						Matches: []gatewayv1.HTTPRouteMatch{
							{
								Path: &gatewayv1.HTTPPathMatch{
									Type:  &prefixType,
									Value: strPtr("/"),
								},
							},
						},
						BackendRefs: []gatewayv1.HTTPBackendRef{
							newHTTPBackendRef("my-service", nil, int32Ptr(8080)),
						},
					},
				},
			},
		},
	}

	buildResult := builder.Build(context.Background(), routes)

	require.Len(t, buildResult.Rules, 2)
	assert.False(t, buildResult.Rules[0].Path.Present)
}

func TestBuild_WeightSelection(t *testing.T) {
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

			builder := ingress.NewBuilder("cluster.local", nil, nil, nil, nil)

			backendRefs := make([]gatewayv1.HTTPBackendRef, len(tt.backendWeights))
			for i, w := range tt.backendWeights {
				backendRefs[i] = gatewayv1.HTTPBackendRef{
					BackendRef: gatewayv1.BackendRef{
						BackendObjectReference: gatewayv1.BackendObjectReference{
							Name: gatewayv1.ObjectName(serviceNames[i]),
							Port: portNumPtr(8080),
						},
						Weight: w,
					},
				}
			}

			routes := []gatewayv1.HTTPRoute{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-route",
						Namespace: "default",
					},
					Spec: gatewayv1.HTTPRouteSpec{
						Hostnames: []gatewayv1.Hostname{"app.example.com"},
						Rules: []gatewayv1.HTTPRouteRule{
							{
								BackendRefs: backendRefs,
							},
						},
					},
				},
			}

			buildResult := builder.Build(context.Background(), routes)

			require.Len(t, buildResult.Rules, 2)
			assert.Contains(t, buildResult.Rules[0].Service.Value, tt.expectedService)
		})
	}
}

func TestBuild_AllBackendsDisabled(t *testing.T) {
	t.Parallel()

	builder := ingress.NewBuilder("cluster.local", nil, nil, nil, nil)
	routes := []gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-route",
				Namespace: "default",
			},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"app.example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						BackendRefs: []gatewayv1.HTTPBackendRef{
							{
								BackendRef: gatewayv1.BackendRef{
									BackendObjectReference: gatewayv1.BackendObjectReference{
										Name: "disabled-svc",
										Port: portNumPtr(8080),
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

	// Only catch-all rule should be present, no actual route rules
	require.Len(t, buildResult.Rules, 1)
	assert.Equal(t, ingress.CatchAllService, buildResult.Rules[0].Service.Value)
}

func newHTTPBackendRef(name string, namespace *gatewayv1.Namespace, port *int32) gatewayv1.HTTPBackendRef {
	ref := gatewayv1.HTTPBackendRef{
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

func strPtr(s string) *string {
	return &s
}

func int32Ptr(i int32) *int32 {
	return &i
}

func portNumPtr(p int32) *gatewayv1.PortNumber {
	pn := gatewayv1.PortNumber(p)

	return &pn
}

// ExternalName Service Tests (TDD Phase 1: RED)

func TestBuild_ExternalNameService(t *testing.T) {
	t.Parallel()

	externalSvc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "external-api",
			Namespace: "default",
		},
		Spec: corev1.ServiceSpec{
			Type:         corev1.ServiceTypeExternalName,
			ExternalName: "api.external.com",
		},
	}

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(externalSvc).
		Build()

	builder := ingress.NewBuilder("cluster.local", nil, fakeClient, nil, nil)
	routes := []gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-route",
				Namespace: "default",
			},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"app.example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						BackendRefs: []gatewayv1.HTTPBackendRef{
							newHTTPBackendRef("external-api", nil, int32Ptr(443)),
						},
					},
				},
			},
		},
	}

	buildResult := builder.Build(context.Background(), routes)

	require.Len(t, buildResult.Rules, 2)
	assert.Equal(t, "https://api.external.com:443", buildResult.Rules[0].Service.Value)
	assert.Empty(t, buildResult.FailedRefs)
}

func TestBuild_ExternalNameService_HTTPPort(t *testing.T) {
	t.Parallel()

	externalSvc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "external-api",
			Namespace: "default",
		},
		Spec: corev1.ServiceSpec{
			Type:         corev1.ServiceTypeExternalName,
			ExternalName: "api.external.com",
		},
	}

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(externalSvc).
		Build()

	builder := ingress.NewBuilder("cluster.local", nil, fakeClient, nil, nil)
	routes := []gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-route",
				Namespace: "default",
			},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"app.example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						BackendRefs: []gatewayv1.HTTPBackendRef{
							newHTTPBackendRef("external-api", nil, int32Ptr(80)),
						},
					},
				},
			},
		},
	}

	buildResult := builder.Build(context.Background(), routes)

	require.Len(t, buildResult.Rules, 2)
	assert.Equal(t, "http://api.external.com:80", buildResult.Rules[0].Service.Value)
	assert.Empty(t, buildResult.FailedRefs)
}

func TestBuild_ClusterIPService_WithClient(t *testing.T) {
	t.Parallel()

	clusterIPSvc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-service",
			Namespace: "default",
		},
		Spec: corev1.ServiceSpec{
			Type:      corev1.ServiceTypeClusterIP,
			ClusterIP: "10.0.0.1",
		},
	}

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(clusterIPSvc).
		Build()

	builder := ingress.NewBuilder("cluster.local", nil, fakeClient, nil, nil)
	routes := []gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-route",
				Namespace: "default",
			},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"app.example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						BackendRefs: []gatewayv1.HTTPBackendRef{
							newHTTPBackendRef("my-service", nil, int32Ptr(8080)),
						},
					},
				},
			},
		},
	}

	buildResult := builder.Build(context.Background(), routes)

	require.Len(t, buildResult.Rules, 2)
	assert.Equal(t, "http://my-service.default.svc.cluster.local:8080", buildResult.Rules[0].Service.Value)
	assert.Empty(t, buildResult.FailedRefs)
}

func TestBuild_ServiceNotFound(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		Build()

	builder := ingress.NewBuilder("cluster.local", nil, fakeClient, nil, nil)
	routes := []gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-route",
				Namespace: "default",
			},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"app.example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						BackendRefs: []gatewayv1.HTTPBackendRef{
							newHTTPBackendRef("nonexistent-service", nil, int32Ptr(8080)),
						},
					},
				},
			},
		},
	}

	buildResult := builder.Build(context.Background(), routes)

	require.Len(t, buildResult.Rules, 1)
	assert.Equal(t, ingress.CatchAllService, buildResult.Rules[0].Service.Value)
	require.Len(t, buildResult.FailedRefs, 1)
	assert.Equal(t, "BackendNotFound", buildResult.FailedRefs[0].Reason)
	assert.Equal(t, "nonexistent-service", buildResult.FailedRefs[0].BackendName)
}

func TestBuild_NilClient_FallbackBehavior(t *testing.T) {
	t.Parallel()

	builder := ingress.NewBuilder("cluster.local", nil, nil, nil, nil)
	routes := []gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-route",
				Namespace: "default",
			},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"app.example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						BackendRefs: []gatewayv1.HTTPBackendRef{
							newHTTPBackendRef("my-service", nil, int32Ptr(8080)),
						},
					},
				},
			},
		},
	}

	buildResult := builder.Build(context.Background(), routes)

	require.Len(t, buildResult.Rules, 2)
	assert.Equal(t, "http://my-service.default.svc.cluster.local:8080", buildResult.Rules[0].Service.Value)
	assert.Empty(t, buildResult.FailedRefs)
}

func TestBuild_ExternalNameService_CrossNamespace(t *testing.T) {
	t.Parallel()

	externalSvc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "external-api",
			Namespace: "other-namespace",
		},
		Spec: corev1.ServiceSpec{
			Type:         corev1.ServiceTypeExternalName,
			ExternalName: "api.external.com",
		},
	}

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(externalSvc).
		Build()

	ns := gatewayv1.Namespace("other-namespace")
	builder := ingress.NewBuilder("cluster.local", nil, fakeClient, nil, nil)
	routes := []gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-route",
				Namespace: "default",
			},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"app.example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						BackendRefs: []gatewayv1.HTTPBackendRef{
							newHTTPBackendRef("external-api", &ns, int32Ptr(443)),
						},
					},
				},
			},
		},
	}

	buildResult := builder.Build(context.Background(), routes)

	require.Len(t, buildResult.Rules, 2)
	assert.Equal(t, "https://api.external.com:443", buildResult.Rules[0].Service.Value)
}

func TestBuild_ExternalNameService_CrossNamespace_WithReferenceGrant(t *testing.T) {
	t.Parallel()

	// ExternalName Service in "backend" namespace
	externalSvc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "external-api",
			Namespace: "backend",
		},
		Spec: corev1.ServiceSpec{
			Type:         corev1.ServiceTypeExternalName,
			ExternalName: "api.external.com",
		},
	}

	// ReferenceGrant in "backend" namespace allowing HTTPRoute from "default"
	refGrant := &gatewayv1beta1.ReferenceGrant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "allow-default-to-backend",
			Namespace: "backend",
		},
		Spec: gatewayv1beta1.ReferenceGrantSpec{
			From: []gatewayv1beta1.ReferenceGrantFrom{
				{
					Group:     gatewayv1.GroupName,
					Kind:      "HTTPRoute",
					Namespace: "default",
				},
			},
			To: []gatewayv1beta1.ReferenceGrantTo{
				{
					Group: "",
					Kind:  "Service",
				},
			},
		},
	}

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(gatewayv1beta1.AddToScheme(scheme))

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(externalSvc, refGrant).
		Build()

	validator := referencegrant.NewValidator(fakeClient)
	builder := ingress.NewBuilder("cluster.local", validator, fakeClient, nil, nil)

	ns := gatewayv1.Namespace("backend")
	routes := []gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-route",
				Namespace: "default",
			},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"app.example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						BackendRefs: []gatewayv1.HTTPBackendRef{
							newHTTPBackendRef("external-api", &ns, int32Ptr(443)),
						},
					},
				},
			},
		},
	}

	buildResult := builder.Build(context.Background(), routes)

	// Should succeed with ReferenceGrant
	require.Len(t, buildResult.Rules, 2)
	assert.Equal(t, "https://api.external.com:443", buildResult.Rules[0].Service.Value)
	assert.Empty(t, buildResult.FailedRefs)
}

func TestBuild_ExternalNameService_CrossNamespace_Denied(t *testing.T) {
	t.Parallel()

	// ExternalName Service in "backend" namespace
	externalSvc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "external-api",
			Namespace: "backend",
		},
		Spec: corev1.ServiceSpec{
			Type:         corev1.ServiceTypeExternalName,
			ExternalName: "api.external.com",
		},
	}

	// No ReferenceGrant - cross-namespace should be denied
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(gatewayv1beta1.AddToScheme(scheme))

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(externalSvc).
		Build()

	validator := referencegrant.NewValidator(fakeClient)
	builder := ingress.NewBuilder("cluster.local", validator, fakeClient, nil, nil)

	ns := gatewayv1.Namespace("backend")
	routes := []gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-route",
				Namespace: "default",
			},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"app.example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						BackendRefs: []gatewayv1.HTTPBackendRef{
							newHTTPBackendRef("external-api", &ns, int32Ptr(443)),
						},
					},
				},
			},
		},
	}

	buildResult := builder.Build(context.Background(), routes)

	// Should fail without ReferenceGrant
	require.Len(t, buildResult.Rules, 1)
	assert.Equal(t, ingress.CatchAllService, buildResult.Rules[0].Service.Value)
	require.Len(t, buildResult.FailedRefs, 1)
	assert.Equal(t, "RefNotPermitted", buildResult.FailedRefs[0].Reason)
	assert.Equal(t, "external-api", buildResult.FailedRefs[0].BackendName)
	assert.Equal(t, "backend", buildResult.FailedRefs[0].BackendNS)
}
