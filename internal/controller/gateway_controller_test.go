package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/api/v1alpha1"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/config"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/metrics"
)

func TestGatewayReconciler_WrongGatewayClass(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gateway",
			Namespace: "default",
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "other-class",
			Listeners: []gatewayv1.Listener{
				{
					Name:     "http",
					Port:     80,
					Protocol: "HTTP",
				},
			},
		},
	}

	fakeClient := setupGatewayFakeClient(gateway)

	reconciler := &GatewayReconciler{
		Client:           fakeClient,
		Scheme:           fakeClient.Scheme(),
		GatewayClassName: "cloudflare-tunnel",
		ControllerName:   "test-controller",
		ConfigResolver:   config.NewResolver(fakeClient, "default", metrics.NewNoopCollector()),
	}

	result, err := reconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      "test-gateway",
			Namespace: "default",
		},
	})

	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, result)
}

func TestGatewayReconciler_NotFound(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	fakeClient := setupGatewayFakeClient()

	reconciler := &GatewayReconciler{
		Client:           fakeClient,
		Scheme:           fakeClient.Scheme(),
		GatewayClassName: "cloudflare-tunnel",
		ControllerName:   "test-controller",
		ConfigResolver:   config.NewResolver(fakeClient, "default", metrics.NewNoopCollector()),
	}

	result, err := reconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      "non-existent",
			Namespace: "default",
		},
	})

	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, result)
}

func TestGatewayReconciler_ConfigResolutionError(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gateway",
			Namespace: "default",
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "cloudflare-tunnel",
			Listeners: []gatewayv1.Listener{
				{
					Name:     "http",
					Port:     80,
					Protocol: "HTTP",
				},
			},
		},
	}

	gatewayClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: "cloudflare-tunnel",
		},
		Spec: gatewayv1.GatewayClassSpec{
			ControllerName: "test-controller",
			ParametersRef: &gatewayv1.ParametersReference{
				Group: config.ParametersRefGroup,
				Kind:  config.ParametersRefKind,
				Name:  "missing-config",
			},
		},
	}

	fakeClient := setupGatewayFakeClient(gateway, gatewayClass)

	reconciler := &GatewayReconciler{
		Client:           fakeClient,
		Scheme:           fakeClient.Scheme(),
		GatewayClassName: "cloudflare-tunnel",
		ControllerName:   "test-controller",
		ConfigResolver:   config.NewResolver(fakeClient, "default", metrics.NewNoopCollector()),
	}

	result, err := reconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      "test-gateway",
			Namespace: "default",
		},
	})

	require.NoError(t, err)
	assert.Equal(t, configErrorRequeueDelay, result.RequeueAfter)
}

func TestGatewayReconciler_UpdateStatus(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gateway",
			Namespace: "default",
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "cloudflare-tunnel",
			Listeners: []gatewayv1.Listener{
				{
					Name:     "http",
					Port:     80,
					Protocol: "HTTP",
				},
			},
		},
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cf-credentials",
			Namespace: "default",
		},
		Data: map[string][]byte{
			"api-token": []byte("test-token"),
		},
	}

	disabled := false
	gatewayClassConfig := &v1alpha1.GatewayClassConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-config",
		},
		Spec: v1alpha1.GatewayClassConfigSpec{
			CloudflareCredentialsSecretRef: v1alpha1.SecretReference{
				Name:      "cf-credentials",
				Namespace: "default",
			},
			TunnelID: "12345678-1234-1234-1234-123456789abc",
			Cloudflared: v1alpha1.CloudflaredConfig{
				Enabled: &disabled,
			},
		},
	}

	gatewayClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: "cloudflare-tunnel",
		},
		Spec: gatewayv1.GatewayClassSpec{
			ControllerName: "test-controller",
			ParametersRef: &gatewayv1.ParametersReference{
				Group: config.ParametersRefGroup,
				Kind:  config.ParametersRefKind,
				Name:  "test-config",
			},
		},
	}

	fakeClient := setupGatewayFakeClient(gateway, secret, gatewayClassConfig, gatewayClass)

	reconciler := &GatewayReconciler{
		Client:           fakeClient,
		Scheme:           fakeClient.Scheme(),
		GatewayClassName: "cloudflare-tunnel",
		ControllerName:   "test-controller",
		ConfigResolver:   config.NewResolver(fakeClient, "default", metrics.NewNoopCollector()),
		HelmManager:      nil,
	}

	result, err := reconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      "test-gateway",
			Namespace: "default",
		},
	})

	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, result)

	var updatedGateway gatewayv1.Gateway
	err = fakeClient.Get(ctx, types.NamespacedName{
		Name:      "test-gateway",
		Namespace: "default",
	}, &updatedGateway)

	require.NoError(t, err)
	assert.Len(t, updatedGateway.Status.Addresses, 1)
	assert.Equal(t, "12345678-1234-1234-1234-123456789abc.cfargotunnel.com", updatedGateway.Status.Addresses[0].Value)
}

func TestCloudflaredReleaseName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		gateway     *gatewayv1.Gateway
		expected    string
		maxLen      int
		shouldTrunc bool
	}{
		{
			name: "short name",
			gateway: &gatewayv1.Gateway{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "gw",
					Namespace: "ns",
				},
			},
			expected:    "cfd-ns-gw",
			maxLen:      53,
			shouldTrunc: false,
		},
		{
			name: "normal name",
			gateway: &gatewayv1.Gateway{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "my-gateway",
					Namespace: "default",
				},
			},
			expected:    "cfd-default-my-gateway",
			maxLen:      53,
			shouldTrunc: false,
		},
		{
			name: "long name gets truncated",
			gateway: &gatewayv1.Gateway{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "very-long-gateway-name-that-exceeds-the-limit",
					Namespace: "very-long-namespace-name",
				},
			},
			expected:    "cfd-very-long-namespace-name-very-long-gateway-name-t",
			maxLen:      53,
			shouldTrunc: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			result := cloudflaredReleaseName(tt.gateway)
			assert.Equal(t, tt.expected, result)
			assert.LessOrEqual(t, len(result), tt.maxLen)
		})
	}
}

func TestGatewayReconciler_CountAttachedRoutes(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gateway",
			Namespace: "default",
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "cloudflare-tunnel",
			Listeners: []gatewayv1.Listener{
				{
					Name:     "http",
					Port:     80,
					Protocol: "HTTP",
				},
				{
					Name:     "https",
					Port:     443,
					Protocol: "HTTPS",
				},
			},
		},
	}

	ns := gatewayv1.Namespace("default")
	httpSection := gatewayv1.SectionName("http")
	route1 := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "route-1",
			Namespace: "default",
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{
						Name:        "test-gateway",
						Namespace:   &ns,
						SectionName: &httpSection,
					},
				},
			},
		},
	}

	route2 := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "route-2",
			Namespace: "default",
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{
						Name:      "test-gateway",
						Namespace: &ns,
					},
				},
			},
		},
	}

	route3 := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "route-3",
			Namespace: "other-ns",
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{
						Name: "test-gateway",
					},
				},
			},
		},
	}

	fakeClient := setupGatewayFakeClient(gateway, route1, route2, route3)

	reconciler := &GatewayReconciler{
		Client:           fakeClient,
		Scheme:           fakeClient.Scheme(),
		GatewayClassName: "cloudflare-tunnel",
		ControllerName:   "test-controller",
	}

	counts := reconciler.countAttachedRoutes(ctx, gateway)

	assert.Equal(t, int32(2), counts["http"])
	assert.Equal(t, int32(1), counts["https"])
}

func TestGatewayReconciler_RefMatchesGateway(t *testing.T) {
	t.Parallel()

	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gateway",
			Namespace: "default",
		},
	}

	reconciler := &GatewayReconciler{}

	tests := []struct {
		name           string
		ref            gatewayv1.ParentReference
		routeNamespace string
		expected       bool
	}{
		{
			name: "matching gateway same namespace",
			ref: gatewayv1.ParentReference{
				Name: "test-gateway",
			},
			routeNamespace: "default",
			expected:       true,
		},
		{
			name: "matching gateway explicit namespace",
			ref: gatewayv1.ParentReference{
				Name:      "test-gateway",
				Namespace: ptr(gatewayv1.Namespace("default")),
			},
			routeNamespace: "other",
			expected:       true,
		},
		{
			name: "wrong name",
			ref: gatewayv1.ParentReference{
				Name: "other-gateway",
			},
			routeNamespace: "default",
			expected:       false,
		},
		{
			name: "wrong namespace",
			ref: gatewayv1.ParentReference{
				Name:      "test-gateway",
				Namespace: ptr(gatewayv1.Namespace("other")),
			},
			routeNamespace: "default",
			expected:       false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			result := reconciler.refMatchesGateway(tt.ref, gateway, tt.routeNamespace)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestGatewayReconciler_GatewayClassToGateways(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	gateway1 := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "gateway-1",
			Namespace: "default",
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "cloudflare-tunnel",
		},
	}

	gateway2 := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "gateway-2",
			Namespace: "other",
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "cloudflare-tunnel",
		},
	}

	gateway3 := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "gateway-3",
			Namespace: "default",
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "other-class",
		},
	}

	gatewayClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: "cloudflare-tunnel",
		},
		Spec: gatewayv1.GatewayClassSpec{
			ControllerName: "test-controller",
		},
	}

	fakeClient := setupGatewayFakeClient(gateway1, gateway2, gateway3, gatewayClass)

	reconciler := &GatewayReconciler{
		Client:           fakeClient,
		Scheme:           fakeClient.Scheme(),
		GatewayClassName: "cloudflare-tunnel",
		ControllerName:   "test-controller",
	}

	requests := reconciler.gatewayClassToGateways(ctx, gatewayClass)

	assert.Len(t, requests, 2)

	names := make([]string, len(requests))
	for i, req := range requests {
		names[i] = req.Name
	}

	assert.Contains(t, names, "gateway-1")
	assert.Contains(t, names, "gateway-2")
	assert.NotContains(t, names, "gateway-3")
}

func TestGatewayReconciler_GatewayClassToGateways_WrongClass(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	otherGatewayClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: "other-class",
		},
		Spec: gatewayv1.GatewayClassSpec{
			ControllerName: "other-controller",
		},
	}

	fakeClient := setupGatewayFakeClient(otherGatewayClass)

	reconciler := &GatewayReconciler{
		Client:           fakeClient,
		Scheme:           fakeClient.Scheme(),
		GatewayClassName: "cloudflare-tunnel",
		ControllerName:   "test-controller",
	}

	requests := reconciler.gatewayClassToGateways(ctx, otherGatewayClass)

	assert.Nil(t, requests)
}

func TestGatewayReconciler_GetAllGatewaysForClass(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	gateway1 := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "gateway-1",
			Namespace: "ns1",
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "cloudflare-tunnel",
		},
	}

	gateway2 := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "gateway-2",
			Namespace: "ns2",
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "other-class",
		},
	}

	fakeClient := setupGatewayFakeClient(gateway1, gateway2)

	reconciler := &GatewayReconciler{
		Client:           fakeClient,
		Scheme:           fakeClient.Scheme(),
		GatewayClassName: "cloudflare-tunnel",
		ControllerName:   "test-controller",
	}

	requests := reconciler.getAllGatewaysForClass(ctx)

	require.Len(t, requests, 1)
	assert.Equal(t, "gateway-1", requests[0].Name)
	assert.Equal(t, "ns1", requests[0].Namespace)
}

func TestPtr(t *testing.T) {
	t.Parallel()

	strVal := "test"
	strPtr := ptr(strVal)
	assert.Equal(t, strVal, *strPtr)

	intVal := 42
	intPtr := ptr(intVal)
	assert.Equal(t, intVal, *intPtr)
}

func TestGatewayReconciler_BuildCloudflaredValues(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cfg  *config.ResolvedConfig
	}{
		{
			name: "basic values",
			cfg: &config.ResolvedConfig{
				TunnelToken:         "test-token",
				CloudflaredProtocol: "quic",
				CloudflaredReplicas: 2,
			},
		},
		{
			name: "with AWG sidecar",
			cfg: &config.ResolvedConfig{
				TunnelToken:         "test-token",
				CloudflaredProtocol: "http2",
				CloudflaredReplicas: 1,
				AWGSecretName:       "awg-config",
				AWGInterfacePrefix:  "my-awg",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			reconciler := &GatewayReconciler{}
			result := reconciler.buildCloudflaredValues(tt.cfg)

			require.NotNil(t, result)
			assert.Equal(t, tt.cfg.CloudflaredReplicas, int32(result["replicaCount"].(int)))

			cloudflare, ok := result["cloudflare"].(map[string]any)
			require.True(t, ok)
			assert.Equal(t, tt.cfg.TunnelToken, cloudflare["tunnelToken"])
			assert.Equal(t, "remote", cloudflare["mode"])

			if tt.cfg.CloudflaredProtocol != "" {
				assert.Equal(t, tt.cfg.CloudflaredProtocol, result["protocol"])
			}

			if tt.cfg.AWGSecretName != "" {
				_, hasSidecar := result["sidecar"]
				assert.True(t, hasSidecar)
			}
		})
	}
}

func setupGatewayFakeClient(objs ...client.Object) client.WithWatch {
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(gatewayv1.Install(scheme))
	utilruntime.Must(v1alpha1.AddToScheme(scheme))

	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&gatewayv1.Gateway{}).
		Build()
}

func TestGatewayReconciler_GatewayClassToGateways_WrongType(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	fakeClient := setupGatewayFakeClient()

	reconciler := &GatewayReconciler{
		Client:           fakeClient,
		Scheme:           fakeClient.Scheme(),
		GatewayClassName: "cloudflare-tunnel",
	}

	notGatewayClass := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "not-a-gateway-class",
			Namespace: "default",
		},
	}

	requests := reconciler.gatewayClassToGateways(ctx, notGatewayClass)

	assert.Nil(t, requests)
}

func TestGatewayReconciler_HandleDeletion_NoFinalizer(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-gateway",
			Namespace:  "default",
			Finalizers: []string{},
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "cloudflare-tunnel",
		},
	}

	disabled := false
	cfg := &config.ResolvedConfig{
		CloudflaredEnabled: disabled,
	}

	fakeClient := setupGatewayFakeClient(gateway)

	reconciler := &GatewayReconciler{
		Client:           fakeClient,
		Scheme:           fakeClient.Scheme(),
		GatewayClassName: "cloudflare-tunnel",
	}

	result, err := reconciler.handleDeletion(ctx, gateway, cfg)

	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, result)
}

func TestGatewayReconciler_SetConfigErrorStatus(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-gateway",
			Namespace:  "default",
			Generation: 1,
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "cloudflare-tunnel",
		},
	}

	fakeClient := setupGatewayFakeClient(gateway)

	reconciler := &GatewayReconciler{
		Client: fakeClient,
		Scheme: fakeClient.Scheme(),
	}

	configErr := assert.AnError
	err := reconciler.setConfigErrorStatus(ctx, gateway, configErr)

	require.NoError(t, err)

	var updated gatewayv1.Gateway
	err = fakeClient.Get(ctx, types.NamespacedName{
		Name:      "test-gateway",
		Namespace: "default",
	}, &updated)
	require.NoError(t, err)

	require.Len(t, updated.Status.Conditions, 1)
	assert.Equal(t, string(gatewayv1.GatewayConditionAccepted), updated.Status.Conditions[0].Type)
	assert.Equal(t, metav1.ConditionFalse, updated.Status.Conditions[0].Status)
	assert.Equal(t, "InvalidParameters", updated.Status.Conditions[0].Reason)
}

func setupGatewayTestReconcilerWithManagedCloudflared() (*GatewayReconciler, client.WithWatch) {
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(gatewayv1.Install(scheme))
	utilruntime.Must(v1alpha1.AddToScheme(scheme))

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&gatewayv1.Gateway{}).
		Build()

	return &GatewayReconciler{
		Client:           fakeClient,
		Scheme:           scheme,
		GatewayClassName: "cloudflare-tunnel",
		ControllerName:   "test-controller",
		ConfigResolver:   config.NewResolver(fakeClient, "default", metrics.NewNoopCollector()),
		HelmManager:      nil,
	}, fakeClient
}

func TestGatewayReconciler_ReturnsReconcileRequest(t *testing.T) {
	t.Parallel()

	reconciler, _ := setupGatewayTestReconcilerWithManagedCloudflared()

	requests := reconciler.getAllGatewaysForClass(context.Background())

	assert.Empty(t, requests)
}

func TestGatewayReconciler_MapperIntegration(t *testing.T) {
	t.Parallel()

	fakeClient := setupGatewayFakeClient()

	reconciler := &GatewayReconciler{
		Client:           fakeClient,
		Scheme:           fakeClient.Scheme(),
		GatewayClassName: "cloudflare-tunnel",
		ControllerName:   "test-controller",
		ConfigResolver:   config.NewResolver(fakeClient, "default", metrics.NewNoopCollector()),
	}

	mapper := &ConfigMapper{
		Client:           reconciler.Client,
		GatewayClassName: reconciler.GatewayClassName,
		ConfigResolver:   reconciler.ConfigResolver,
	}

	assert.NotNil(t, mapper)
	assert.Equal(t, reconciler.GatewayClassName, mapper.GatewayClassName)
}

func TestConstants(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "cloudflare-tunnel.gateway.networking.k8s.io/cloudflared", cloudflaredFinalizer)
	assert.Equal(t, ".cfargotunnel.com", cfArgotunnelSuffix)
	assert.Equal(t, 53, maxHelmReleaseName)
}

func TestGatewayStatusAddressFormat(t *testing.T) {
	t.Parallel()

	tunnelID := "12345678-1234-1234-1234-123456789abc"
	expected := tunnelID + cfArgotunnelSuffix

	assert.Equal(t, "12345678-1234-1234-1234-123456789abc.cfargotunnel.com", expected)
}

func newReconcileRequest(name, namespace string) reconcile.Request {
	return reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name:      name,
			Namespace: namespace,
		},
	}
}

func TestNewReconcileRequest(t *testing.T) {
	t.Parallel()

	req := newReconcileRequest("test", "default")

	assert.Equal(t, "test", req.Name)
	assert.Equal(t, "default", req.Namespace)
}
