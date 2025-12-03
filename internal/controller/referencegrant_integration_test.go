package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/api/v1alpha1"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/ingress"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/metrics"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/referencegrant"
)

// TestRouteSyncer_CrossNamespaceRef_WithoutGrant tests that cross-namespace
// backend references are denied when no ReferenceGrant exists.
func TestRouteSyncer_CrossNamespaceRef_WithoutGrant(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, gatewayv1.Install(scheme))
	require.NoError(t, gatewayv1beta1.AddToScheme(scheme))
	require.NoError(t, v1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))

	// HTTPRoute in "default" namespace referencing Service in "backend" namespace
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cross-ns-route",
			Namespace: "default",
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Name: "test-gateway"},
				},
			},
			Hostnames: []gatewayv1.Hostname{"app.example.com"},
			Rules: []gatewayv1.HTTPRouteRule{
				{
					BackendRefs: []gatewayv1.HTTPBackendRef{
						{
							BackendRef: gatewayv1.BackendRef{
								BackendObjectReference: gatewayv1.BackendObjectReference{
									Name:      "backend-service",
									Namespace: (*gatewayv1.Namespace)(strPtr("backend")),
									Port:      portNumPtr(8080),
								},
							},
						},
					},
				},
			},
		},
	}

	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gateway",
			Namespace: "default",
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "cloudflare-tunnel",
			Listeners:        httpListener(),
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(route, gateway).
		Build()

	syncer := NewRouteSyncer(
		fakeClient,
		scheme,
		"cluster.local",
		"cloudflare-tunnel",
		nil, // No config resolver needed for this test
		metrics.NewNoopCollector(),
		nil,
	)

	ctx := context.Background()

	// Get relevant routes (should include our route)
	routes, bindings, err := syncer.getRelevantHTTPRoutes(ctx)
	require.NoError(t, err)
	require.Len(t, routes, 1)
	assert.Equal(t, "cross-ns-route", routes[0].Name)

	// Build ingress rules
	buildResult := syncer.httpBuilder.Build(ctx, routes)

	// Should have failed refs for cross-namespace reference without ReferenceGrant
	require.Len(t, buildResult.FailedRefs, 1, "Expected one failed backend reference")
	assert.Equal(t, "default", buildResult.FailedRefs[0].RouteNamespace)
	assert.Equal(t, "cross-ns-route", buildResult.FailedRefs[0].RouteName)
	assert.Equal(t, "backend-service", buildResult.FailedRefs[0].BackendName)
	assert.Equal(t, "backend", buildResult.FailedRefs[0].BackendNS)
	assert.Equal(t, string(gatewayv1.RouteReasonRefNotPermitted), buildResult.FailedRefs[0].Reason)

	// Should only have catch-all rule (no actual route rules because backend ref failed)
	require.Len(t, buildResult.Rules, 1)
	assert.Equal(t, ingress.CatchAllService, buildResult.Rules[0].Service.Value)

	// Binding should exist but reference shouldn't be in bindings (it's a backend ref issue, not binding)
	assert.NotNil(t, bindings)
}

// TestReferenceGrant_Validator_Direct tests the validator directly.
func TestReferenceGrant_Validator_Direct(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, gatewayv1.Install(scheme))
	require.NoError(t, gatewayv1beta1.AddToScheme(scheme))
	require.NoError(t, v1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))

	// ReferenceGrant in "backend" namespace
	refGrant := &gatewayv1beta1.ReferenceGrant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-grant",
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

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(refGrant).
		Build()

	validator := referencegrant.NewValidator(fakeClient)

	ctx := context.Background()

	// Test the reference
	allowed, err := validator.IsReferenceAllowed(ctx,
		referencegrant.Reference{
			Group:     gatewayv1.GroupName,
			Kind:      "HTTPRoute",
			Namespace: "default",
			Name:      "test-route",
		},
		referencegrant.Reference{
			Group:     "",
			Kind:      "Service",
			Namespace: "backend",
			Name:      "backend-service",
		},
	)

	require.NoError(t, err)
	assert.True(t, allowed, "Reference should be allowed with valid ReferenceGrant")
}

// TestBuilder_CrossNamespaceRef_WithGrant tests the builder directly with ReferenceGrant.
func TestBuilder_CrossNamespaceRef_WithGrant(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, gatewayv1.Install(scheme))
	require.NoError(t, gatewayv1beta1.AddToScheme(scheme))
	require.NoError(t, v1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))

	backendNS := gatewayv1.Namespace("backend")

	// HTTPRoute in "default" namespace referencing Service in "backend" namespace
	route := gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cross-ns-route",
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
									Name:      "backend-service",
									Namespace: &backendNS,
									Port:      portNumPtr(8080),
								},
							},
						},
					},
				},
			},
		},
	}

	// ReferenceGrant in "backend" namespace allowing HTTPRoute from "default" to access Services
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

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(refGrant).
		Build()

	validator := referencegrant.NewValidator(fakeClient)
	builder := ingress.NewBuilder("cluster.local", validator, nil, nil, nil)

	ctx := context.Background()

	// Build ingress rules
	buildResult := builder.Build(ctx, []gatewayv1.HTTPRoute{route})

	// Should have NO failed refs because ReferenceGrant permits the reference
	assert.Empty(t, buildResult.FailedRefs, "Expected no failed backend references with ReferenceGrant")

	// Should have actual route rule + catch-all
	require.Len(t, buildResult.Rules, 2)
	assert.Equal(t, "app.example.com", buildResult.Rules[0].Hostname.Value)
	assert.Equal(t, "http://backend-service.backend.svc.cluster.local:8080", buildResult.Rules[0].Service.Value)
	assert.Equal(t, ingress.CatchAllService, buildResult.Rules[1].Service.Value)
}

// TestRouteSyncer_GRPCRoute_CrossNamespaceRef_WithGrant tests ReferenceGrant
// support for GRPCRoute resources.
func TestRouteSyncer_GRPCRoute_CrossNamespaceRef_WithGrant(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, gatewayv1.Install(scheme))
	require.NoError(t, gatewayv1beta1.AddToScheme(scheme))
	require.NoError(t, v1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))

	backendNS := gatewayv1.Namespace("backend")

	// GRPCRoute in "default" namespace referencing Service in "backend" namespace
	route := &gatewayv1.GRPCRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cross-ns-grpc-route",
			Namespace: "default",
		},
		Spec: gatewayv1.GRPCRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Name: "test-gateway"},
				},
			},
			Hostnames: []gatewayv1.Hostname{"grpc.example.com"},
			Rules: []gatewayv1.GRPCRouteRule{
				{
					BackendRefs: []gatewayv1.GRPCBackendRef{
						{
							BackendRef: gatewayv1.BackendRef{
								BackendObjectReference: gatewayv1.BackendObjectReference{
									Name:      "grpc-service",
									Namespace: &backendNS,
									Port:      portNumPtr(50051),
								},
							},
						},
					},
				},
			},
		},
	}

	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gateway",
			Namespace: "default",
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "cloudflare-tunnel",
			Listeners:        httpListener(),
		},
	}

	// ReferenceGrant in "backend" namespace allowing GRPCRoute from "default" to access Services
	refGrant := &gatewayv1beta1.ReferenceGrant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "allow-default-grpc",
			Namespace: "backend",
		},
		Spec: gatewayv1beta1.ReferenceGrantSpec{
			From: []gatewayv1beta1.ReferenceGrantFrom{
				{
					Group:     gatewayv1.GroupName,
					Kind:      "GRPCRoute",
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

	// Service in "backend" namespace
	grpcService := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "grpc-service",
			Namespace: "backend",
		},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeClusterIP,
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(route, gateway, refGrant, grpcService).
		Build()

	syncer := NewRouteSyncer(
		fakeClient,
		scheme,
		"cluster.local",
		"cloudflare-tunnel",
		nil,
		metrics.NewNoopCollector(),
		nil,
	)

	ctx := context.Background()

	// Get relevant routes
	routes, _, err := syncer.getRelevantGRPCRoutes(ctx)
	require.NoError(t, err)
	require.Len(t, routes, 1)

	// Build ingress rules
	buildResult := syncer.grpcBuilder.Build(ctx, routes)

	// Should have NO failed refs because ReferenceGrant permits the reference
	assert.Empty(t, buildResult.FailedRefs, "Expected no failed backend references with ReferenceGrant")

	// Should have actual route rule (no catch-all for GRPC builder)
	require.Len(t, buildResult.Rules, 1)
	assert.Equal(t, "grpc.example.com", buildResult.Rules[0].Hostname.Value)
	assert.Equal(t, "http://grpc-service.backend.svc.cluster.local:50051", buildResult.Rules[0].Service.Value)
}

// TestRouteSyncer_ReferenceGrant_SpecificName tests that ReferenceGrant
// can limit access to specific Service names.
func TestRouteSyncer_ReferenceGrant_SpecificName(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, gatewayv1.Install(scheme))
	require.NoError(t, gatewayv1beta1.AddToScheme(scheme))
	require.NoError(t, v1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))

	backendNS := gatewayv1.Namespace("backend")

	// Two routes - one to allowed service, one to denied service
	allowedRoute := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "allowed-route",
			Namespace: "default",
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Name: "test-gateway"},
				},
			},
			Hostnames: []gatewayv1.Hostname{"allowed.example.com"},
			Rules: []gatewayv1.HTTPRouteRule{
				{
					BackendRefs: []gatewayv1.HTTPBackendRef{
						{
							BackendRef: gatewayv1.BackendRef{
								BackendObjectReference: gatewayv1.BackendObjectReference{
									Name:      "allowed-service",
									Namespace: &backendNS,
									Port:      portNumPtr(8080),
								},
							},
						},
					},
				},
			},
		},
	}

	deniedRoute := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "denied-route",
			Namespace: "default",
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Name: "test-gateway"},
				},
			},
			Hostnames: []gatewayv1.Hostname{"denied.example.com"},
			Rules: []gatewayv1.HTTPRouteRule{
				{
					BackendRefs: []gatewayv1.HTTPBackendRef{
						{
							BackendRef: gatewayv1.BackendRef{
								BackendObjectReference: gatewayv1.BackendObjectReference{
									Name:      "denied-service",
									Namespace: &backendNS,
									Port:      portNumPtr(8080),
								},
							},
						},
					},
				},
			},
		},
	}

	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gateway",
			Namespace: "default",
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "cloudflare-tunnel",
			Listeners:        httpListener(),
		},
	}

	// ReferenceGrant that only allows access to "allowed-service"
	refGrant := &gatewayv1beta1.ReferenceGrant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "specific-service-grant",
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
					Name:  (*gatewayv1.ObjectName)(strPtr("allowed-service")),
				},
			},
		},
	}

	// Service in "backend" namespace - only allowed-service exists
	allowedService := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "allowed-service",
			Namespace: "backend",
		},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeClusterIP,
		},
	}

	// Note: denied-service does NOT exist, so it will fail with RefNotPermitted
	// (ReferenceGrant check happens before Service lookup)

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(allowedRoute, deniedRoute, gateway, refGrant, allowedService).
		Build()

	syncer := NewRouteSyncer(
		fakeClient,
		scheme,
		"cluster.local",
		"cloudflare-tunnel",
		nil,
		metrics.NewNoopCollector(),
		nil,
	)

	ctx := context.Background()

	// Get relevant routes
	routes, _, err := syncer.getRelevantHTTPRoutes(ctx)
	require.NoError(t, err)
	require.Len(t, routes, 2)

	// Build ingress rules
	buildResult := syncer.httpBuilder.Build(ctx, routes)

	// Should have one failed ref for denied-service
	require.Len(t, buildResult.FailedRefs, 1)
	assert.Equal(t, "denied-route", buildResult.FailedRefs[0].RouteName)
	assert.Equal(t, "denied-service", buildResult.FailedRefs[0].BackendName)

	// Should have one route rule for allowed-service + catch-all
	require.Len(t, buildResult.Rules, 2)
	assert.Equal(t, "allowed.example.com", buildResult.Rules[0].Hostname.Value)
	assert.Contains(t, buildResult.Rules[0].Service.Value, "allowed-service")
}

// strPtr returns a pointer to a string.
func strPtr(s string) *string {
	return &s
}

// portNumPtr returns a pointer to a PortNumber.
func portNumPtr(p int32) *gatewayv1.PortNumber {
	pn := gatewayv1.PortNumber(p)

	return &pn
}
