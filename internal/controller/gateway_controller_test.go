package controller

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1release "helm.sh/helm/v4/pkg/release/v1"
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
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/api/v1alpha1"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/cfmetrics"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/config"
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
		ConfigResolver:   config.NewResolver(fakeClient, "default", cfmetrics.NewNoopCollector()),
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
		ConfigResolver:   config.NewResolver(fakeClient, "default", cfmetrics.NewNoopCollector()),
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
		ConfigResolver:   config.NewResolver(fakeClient, "default", cfmetrics.NewNoopCollector()),
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
		ConfigResolver:   config.NewResolver(fakeClient, "default", cfmetrics.NewNoopCollector()),
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
				Namespace: new(gatewayv1.Namespace("default")),
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
				Namespace: new(gatewayv1.Namespace("other")),
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
	strPtr := new(strVal)
	assert.Equal(t, strVal, *strPtr)

	intVal := 42
	intPtr := new(intVal)
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

	require.Len(t, updated.Status.Conditions, 2)

	// Verify Accepted condition
	assert.Equal(t, string(gatewayv1.GatewayConditionAccepted), updated.Status.Conditions[0].Type)
	assert.Equal(t, metav1.ConditionFalse, updated.Status.Conditions[0].Status)
	assert.Equal(t, string(gatewayv1.GatewayReasonInvalidParameters), updated.Status.Conditions[0].Reason)

	// Verify Programmed condition
	assert.Equal(t, string(gatewayv1.GatewayConditionProgrammed), updated.Status.Conditions[1].Type)
	assert.Equal(t, metav1.ConditionFalse, updated.Status.Conditions[1].Status)
	assert.Equal(t, string(gatewayv1.GatewayReasonInvalid), updated.Status.Conditions[1].Reason)

	// Verify addresses and listeners are cleared
	assert.Nil(t, updated.Status.Addresses)
	assert.Nil(t, updated.Status.Listeners)
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
		ConfigResolver:   config.NewResolver(fakeClient, "default", cfmetrics.NewNoopCollector()),
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
		ConfigResolver:   config.NewResolver(fakeClient, "default", cfmetrics.NewNoopCollector()),
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

func TestGetNestedString(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		m        map[string]any
		keys     []string
		expected string
	}{
		{
			name:     "nil map",
			m:        nil,
			keys:     []string{"key"},
			expected: "",
		},
		{
			name:     "empty keys",
			m:        map[string]any{"key": "value"},
			keys:     []string{},
			expected: "",
		},
		{
			name:     "simple key",
			m:        map[string]any{"key": "value"},
			keys:     []string{"key"},
			expected: "value",
		},
		{
			name: "nested key",
			m: map[string]any{
				"cloudflare": map[string]any{
					"tunnelToken": "test-token-123",
				},
			},
			keys:     []string{"cloudflare", "tunnelToken"},
			expected: "test-token-123",
		},
		{
			name:     "missing key",
			m:        map[string]any{"other": "value"},
			keys:     []string{"missing"},
			expected: "",
		},
		{
			name: "non-string value",
			m: map[string]any{
				"number": 123,
			},
			keys:     []string{"number"},
			expected: "",
		},
		{
			name: "intermediate not a map",
			m: map[string]any{
				"cloudflare": "not-a-map",
			},
			keys:     []string{"cloudflare", "tunnelToken"},
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			result := getNestedString(tt.m, tt.keys...)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestCloudflaredValuesChanged(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		currentConfig map[string]any
		desiredConfig map[string]any
		expected      bool
	}{
		{
			name: "same token - no change",
			currentConfig: map[string]any{
				"cloudflare": map[string]any{
					"tunnelToken": "token-123",
				},
			},
			desiredConfig: map[string]any{
				"cloudflare": map[string]any{
					"tunnelToken": "token-123",
				},
			},
			expected: false,
		},
		{
			name: "different token - changed",
			currentConfig: map[string]any{
				"cloudflare": map[string]any{
					"tunnelToken": "old-token",
				},
			},
			desiredConfig: map[string]any{
				"cloudflare": map[string]any{
					"tunnelToken": "new-token",
				},
			},
			expected: true,
		},
		{
			name:          "current config nil - changed",
			currentConfig: nil,
			desiredConfig: map[string]any{
				"cloudflare": map[string]any{
					"tunnelToken": "new-token",
				},
			},
			expected: true,
		},
		{
			name: "current token missing - changed",
			currentConfig: map[string]any{
				"otherKey": "value",
			},
			desiredConfig: map[string]any{
				"cloudflare": map[string]any{
					"tunnelToken": "new-token",
				},
			},
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Create a mock release with the current config
			rel := &v1release.Release{
				Config: tt.currentConfig,
			}

			result := cloudflaredValuesChanged(rel, tt.desiredConfig)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestTruncateMessage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "short message unchanged",
			input:    "short error",
			expected: "short error",
		},
		{
			name:     "empty message",
			input:    "",
			expected: "",
		},
		{
			name:     "exactly at limit",
			input:    strings.Repeat("x", maxConditionMessageLength),
			expected: strings.Repeat("x", maxConditionMessageLength),
		},
		{
			name:     "over limit gets truncated with ellipsis",
			input:    strings.Repeat("x", maxConditionMessageLength+50),
			expected: strings.Repeat("x", maxConditionMessageLength-3) + "...",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			result := truncateMessage(tt.input)
			assert.Equal(t, tt.expected, result)
			assert.LessOrEqual(t, len(result), maxConditionMessageLength)
		})
	}
}

func TestGatewayReconciler_SetCloudflaredErrorStatus(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-gateway",
			Namespace:  "default",
			Generation: 2,
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

	cfg := &config.ResolvedConfig{
		TunnelID: "test-tunnel-id",
	}

	cloudflaredErr := assert.AnError
	err := reconciler.setCloudflaredErrorStatus(ctx, gateway, cfg, cloudflaredErr)
	require.NoError(t, err)

	var updated gatewayv1.Gateway
	err = fakeClient.Get(ctx, types.NamespacedName{
		Name:      "test-gateway",
		Namespace: "default",
	}, &updated)
	require.NoError(t, err)

	// Verify addresses are set (tunnel exists even if cloudflared failed)
	require.Len(t, updated.Status.Addresses, 1)
	assert.Equal(t, "test-tunnel-id"+cfArgotunnelSuffix, updated.Status.Addresses[0].Value)

	// Verify conditions
	require.Len(t, updated.Status.Conditions, 2)

	assert.Equal(t, string(gatewayv1.GatewayConditionAccepted), updated.Status.Conditions[0].Type)
	assert.Equal(t, metav1.ConditionTrue, updated.Status.Conditions[0].Status)

	assert.Equal(t, string(gatewayv1.GatewayConditionProgrammed), updated.Status.Conditions[1].Type)
	assert.Equal(t, metav1.ConditionFalse, updated.Status.Conditions[1].Status)
	assert.Equal(t, "DeploymentFailed", updated.Status.Conditions[1].Reason)

	// Verify listeners are cleared
	assert.Nil(t, updated.Status.Listeners)
}

func TestGatewayReconciler_HandleDeletion_WithFinalizer_NoHelmManager(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-gateway",
			Namespace:  "default",
			Finalizers: []string{cloudflaredFinalizer},
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "cloudflare-tunnel",
		},
	}

	cfg := &config.ResolvedConfig{
		CloudflaredEnabled: true,
	}

	fakeClient := setupGatewayFakeClient(gateway)

	reconciler := &GatewayReconciler{
		Client:           fakeClient,
		Scheme:           fakeClient.Scheme(),
		GatewayClassName: "cloudflare-tunnel",
		HelmManager:      nil, // No Helm manager
	}

	result, err := reconciler.handleDeletion(ctx, gateway, cfg)
	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, result)

	// Verify finalizer was removed
	var updated gatewayv1.Gateway
	err = fakeClient.Get(ctx, types.NamespacedName{
		Name:      "test-gateway",
		Namespace: "default",
	}, &updated)
	require.NoError(t, err)
	assert.NotContains(t, updated.Finalizers, cloudflaredFinalizer)
}

func TestGatewayReconciler_HandleDeletion_WithFinalizer_CloudflaredDisabled(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-gateway",
			Namespace:  "default",
			Finalizers: []string{cloudflaredFinalizer},
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "cloudflare-tunnel",
		},
	}

	cfg := &config.ResolvedConfig{
		CloudflaredEnabled: false,
	}

	fakeClient := setupGatewayFakeClient(gateway)

	reconciler := &GatewayReconciler{
		Client:           fakeClient,
		Scheme:           fakeClient.Scheme(),
		GatewayClassName: "cloudflare-tunnel",
		HelmManager:      nil,
	}

	result, err := reconciler.handleDeletion(ctx, gateway, cfg)
	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, result)

	// Verify finalizer was removed even when cloudflared is disabled
	var updated gatewayv1.Gateway
	err = fakeClient.Get(ctx, types.NamespacedName{
		Name:      "test-gateway",
		Namespace: "default",
	}, &updated)
	require.NoError(t, err)
	assert.NotContains(t, updated.Finalizers, cloudflaredFinalizer)
}

func TestGatewayReconciler_BuildResolvedRefsCondition(t *testing.T) {
	t.Parallel()

	now := metav1.Now()
	generation := int64(3)

	reconciler := &GatewayReconciler{}

	tests := []struct {
		name           string
		hasValidKind   bool
		hasInvalidKind bool
		tlsStatus      metav1.ConditionStatus
		tlsReason      string
		tlsMessage     string
		expectedStatus metav1.ConditionStatus
		expectedReason string
	}{
		{
			name:           "no valid kinds",
			hasValidKind:   false,
			hasInvalidKind: false,
			tlsStatus:      metav1.ConditionTrue,
			expectedStatus: metav1.ConditionFalse,
			expectedReason: string(gatewayv1.ListenerReasonInvalidRouteKinds),
		},
		{
			name:           "some invalid kinds",
			hasValidKind:   true,
			hasInvalidKind: true,
			tlsStatus:      metav1.ConditionTrue,
			expectedStatus: metav1.ConditionFalse,
			expectedReason: string(gatewayv1.ListenerReasonInvalidRouteKinds),
		},
		{
			name:           "tls validation failed",
			hasValidKind:   true,
			hasInvalidKind: false,
			tlsStatus:      metav1.ConditionFalse,
			tlsReason:      string(gatewayv1.ListenerReasonInvalidCertificateRef),
			tlsMessage:     "cert not found",
			expectedStatus: metav1.ConditionFalse,
			expectedReason: string(gatewayv1.ListenerReasonInvalidCertificateRef),
		},
		{
			name:           "all valid",
			hasValidKind:   true,
			hasInvalidKind: false,
			tlsStatus:      metav1.ConditionTrue,
			expectedStatus: metav1.ConditionTrue,
			expectedReason: string(gatewayv1.ListenerReasonResolvedRefs),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			condition := reconciler.buildResolvedRefsCondition(
				generation, now, tt.hasValidKind, tt.hasInvalidKind,
				tt.tlsStatus, tt.tlsReason, tt.tlsMessage,
			)

			assert.Equal(t, string(gatewayv1.ListenerConditionResolvedRefs), condition.Type)
			assert.Equal(t, tt.expectedStatus, condition.Status)
			assert.Equal(t, tt.expectedReason, condition.Reason)
			assert.Equal(t, generation, condition.ObservedGeneration)
		})
	}
}

func TestGatewayReconciler_ValidateTLSCertificateRefs_NoTLS(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	fakeClient := setupGatewayFakeClient()
	reconciler := &GatewayReconciler{
		Client: fakeClient,
		Scheme: fakeClient.Scheme(),
	}

	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gw",
			Namespace: "default",
		},
	}

	listener := &gatewayv1.Listener{
		Name:     "http",
		Port:     80,
		Protocol: "HTTP",
		TLS:      nil,
	}

	status, reason, _ := reconciler.validateTLSCertificateRefs(ctx, gateway, listener)
	assert.Equal(t, metav1.ConditionTrue, status)
	assert.Equal(t, string(gatewayv1.ListenerReasonResolvedRefs), reason)
}

func TestGatewayReconciler_ValidateSecretExists(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	validCert := []byte("-----BEGIN CERTIFICATE-----\nMIIBhTCCASugAwIBAgIQaR0K\n-----END CERTIFICATE-----\n")
	validKey := []byte("-----BEGIN PRIVATE KEY-----\nMIGHAgEA\n-----END PRIVATE KEY-----\n")

	tests := []struct {
		name           string
		secret         *corev1.Secret
		expectedStatus metav1.ConditionStatus
		expectedMsg    string
	}{
		{
			name: "valid tls secret",
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "tls-secret",
					Namespace: "default",
				},
				Type: corev1.SecretTypeTLS,
				Data: map[string][]byte{
					corev1.TLSCertKey:       validCert,
					corev1.TLSPrivateKeyKey: validKey,
				},
			},
			expectedStatus: metav1.ConditionTrue,
		},
		{
			name: "wrong secret type",
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "tls-secret",
					Namespace: "default",
				},
				Type: corev1.SecretTypeOpaque,
				Data: map[string][]byte{
					corev1.TLSCertKey:       validCert,
					corev1.TLSPrivateKeyKey: validKey,
				},
			},
			expectedStatus: metav1.ConditionFalse,
			expectedMsg:    "not of type kubernetes.io/tls",
		},
		{
			name: "missing tls.crt",
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "tls-secret",
					Namespace: "default",
				},
				Type: corev1.SecretTypeTLS,
				Data: map[string][]byte{
					corev1.TLSPrivateKeyKey: validKey,
				},
			},
			expectedStatus: metav1.ConditionFalse,
			expectedMsg:    "missing tls.crt data",
		},
		{
			name: "missing tls.key",
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "tls-secret",
					Namespace: "default",
				},
				Type: corev1.SecretTypeTLS,
				Data: map[string][]byte{
					corev1.TLSCertKey: validCert,
				},
			},
			expectedStatus: metav1.ConditionFalse,
			expectedMsg:    "missing tls.key data",
		},
		{
			name: "invalid PEM certificate data",
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "tls-secret",
					Namespace: "default",
				},
				Type: corev1.SecretTypeTLS,
				Data: map[string][]byte{
					corev1.TLSCertKey:       []byte("not-valid-pem"),
					corev1.TLSPrivateKeyKey: validKey,
				},
			},
			expectedStatus: metav1.ConditionFalse,
			expectedMsg:    "invalid certificate PEM data",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fakeClient := setupGatewayFakeClient(tt.secret)
			reconciler := &GatewayReconciler{
				Client: fakeClient,
				Scheme: fakeClient.Scheme(),
			}

			ref := gatewayv1.SecretObjectReference{
				Name: gatewayv1.ObjectName("tls-secret"),
			}

			status, _, msg := reconciler.validateSecretExists(ctx, "default", ref)
			assert.Equal(t, tt.expectedStatus, status)

			if tt.expectedMsg != "" {
				assert.Contains(t, msg, tt.expectedMsg)
			}
		})
	}
}

func TestGatewayReconciler_ValidateSecretExists_NotFound(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	fakeClient := setupGatewayFakeClient()
	reconciler := &GatewayReconciler{
		Client: fakeClient,
		Scheme: fakeClient.Scheme(),
	}

	ref := gatewayv1.SecretObjectReference{
		Name: gatewayv1.ObjectName("nonexistent-secret"),
	}

	status, reason, msg := reconciler.validateSecretExists(ctx, "default", ref)
	assert.Equal(t, metav1.ConditionFalse, status)
	assert.Equal(t, string(gatewayv1.ListenerReasonInvalidCertificateRef), reason)
	assert.Contains(t, msg, "not found")
}

func TestGatewayReconciler_ValidateSingleCertRef_UnsupportedKind(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	fakeClient := setupGatewayFakeClient()
	reconciler := &GatewayReconciler{
		Client: fakeClient,
		Scheme: fakeClient.Scheme(),
	}

	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gw",
			Namespace: "default",
		},
	}

	unsupportedKind := gatewayv1.Kind("ConfigMap")
	ref := gatewayv1.SecretObjectReference{
		Kind: &unsupportedKind,
		Name: "some-ref",
	}

	status, reason, msg := reconciler.validateSingleCertRef(ctx, gateway, ref)
	assert.Equal(t, metav1.ConditionFalse, status)
	assert.Equal(t, string(gatewayv1.ListenerReasonInvalidCertificateRef), reason)
	assert.Contains(t, msg, "Unsupported certificate ref kind")
}

func TestGatewayReconciler_ValidateSingleCertRef_UnsupportedGroup(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	fakeClient := setupGatewayFakeClient()
	reconciler := &GatewayReconciler{
		Client: fakeClient,
		Scheme: fakeClient.Scheme(),
	}

	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gw",
			Namespace: "default",
		},
	}

	nonCoreGroup := gatewayv1.Group("custom.io")
	ref := gatewayv1.SecretObjectReference{
		Group: &nonCoreGroup,
		Name:  "some-ref",
	}

	status, reason, _ := reconciler.validateSingleCertRef(ctx, gateway, ref)
	assert.Equal(t, metav1.ConditionFalse, status)
	assert.Equal(t, string(gatewayv1.ListenerReasonInvalidCertificateRef), reason)
}

func TestGatewayReconciler_GatewayReferencesSecretsInNamespace(t *testing.T) {
	t.Parallel()

	reconciler := &GatewayReconciler{}

	certNs := gatewayv1.Namespace("cert-ns")

	tests := []struct {
		name      string
		gateway   *gatewayv1.Gateway
		namespace string
		expected  bool
	}{
		{
			name: "no listeners with TLS",
			gateway: &gatewayv1.Gateway{
				ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
				Spec: gatewayv1.GatewaySpec{
					Listeners: []gatewayv1.Listener{
						{Name: "http", Port: 80, Protocol: "HTTP"},
					},
				},
			},
			namespace: "cert-ns",
			expected:  false,
		},
		{
			name: "TLS with cert in same namespace",
			gateway: &gatewayv1.Gateway{
				ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
				Spec: gatewayv1.GatewaySpec{
					Listeners: []gatewayv1.Listener{
						{
							Name:     "https",
							Port:     443,
							Protocol: "HTTPS",
							TLS: &gatewayv1.ListenerTLSConfig{
								CertificateRefs: []gatewayv1.SecretObjectReference{
									{Name: "cert"},
								},
							},
						},
					},
				},
			},
			namespace: "default",
			expected:  true,
		},
		{
			name: "TLS with cert in different namespace via explicit ref",
			gateway: &gatewayv1.Gateway{
				ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
				Spec: gatewayv1.GatewaySpec{
					Listeners: []gatewayv1.Listener{
						{
							Name:     "https",
							Port:     443,
							Protocol: "HTTPS",
							TLS: &gatewayv1.ListenerTLSConfig{
								CertificateRefs: []gatewayv1.SecretObjectReference{
									{Name: "cert", Namespace: &certNs},
								},
							},
						},
					},
				},
			},
			namespace: "cert-ns",
			expected:  true,
		},
		{
			name: "TLS with cert in different namespace no match",
			gateway: &gatewayv1.Gateway{
				ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
				Spec: gatewayv1.GatewaySpec{
					Listeners: []gatewayv1.Listener{
						{
							Name:     "https",
							Port:     443,
							Protocol: "HTTPS",
							TLS: &gatewayv1.ListenerTLSConfig{
								CertificateRefs: []gatewayv1.SecretObjectReference{
									{Name: "cert", Namespace: &certNs},
								},
							},
						},
					},
				},
			},
			namespace: "other-ns",
			expected:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			result := reconciler.gatewayReferencesSecretsInNamespace(tt.gateway, tt.namespace)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestGatewayReconciler_GrantAllowsGateway(t *testing.T) {
	t.Parallel()

	reconciler := &GatewayReconciler{}

	tests := []struct {
		name             string
		grant            *gatewayv1beta1.ReferenceGrant
		gatewayNamespace string
		expected         bool
	}{
		{
			name: "grant allows gateway from namespace",
			grant: &gatewayv1beta1.ReferenceGrant{
				Spec: gatewayv1beta1.ReferenceGrantSpec{
					From: []gatewayv1beta1.ReferenceGrantFrom{
						{
							Group:     gatewayv1.GroupName,
							Kind:      "Gateway",
							Namespace: "gw-ns",
						},
					},
				},
			},
			gatewayNamespace: "gw-ns",
			expected:         true,
		},
		{
			name: "grant for wrong namespace",
			grant: &gatewayv1beta1.ReferenceGrant{
				Spec: gatewayv1beta1.ReferenceGrantSpec{
					From: []gatewayv1beta1.ReferenceGrantFrom{
						{
							Group:     gatewayv1.GroupName,
							Kind:      "Gateway",
							Namespace: "other-ns",
						},
					},
				},
			},
			gatewayNamespace: "gw-ns",
			expected:         false,
		},
		{
			name: "grant for wrong kind",
			grant: &gatewayv1beta1.ReferenceGrant{
				Spec: gatewayv1beta1.ReferenceGrantSpec{
					From: []gatewayv1beta1.ReferenceGrantFrom{
						{
							Group:     gatewayv1.GroupName,
							Kind:      "HTTPRoute",
							Namespace: "gw-ns",
						},
					},
				},
			},
			gatewayNamespace: "gw-ns",
			expected:         false,
		},
		{
			name: "grant for wrong group",
			grant: &gatewayv1beta1.ReferenceGrant{
				Spec: gatewayv1beta1.ReferenceGrantSpec{
					From: []gatewayv1beta1.ReferenceGrantFrom{
						{
							Group:     "other.io",
							Kind:      "Gateway",
							Namespace: "gw-ns",
						},
					},
				},
			},
			gatewayNamespace: "gw-ns",
			expected:         false,
		},
		{
			name: "empty from list",
			grant: &gatewayv1beta1.ReferenceGrant{
				Spec: gatewayv1beta1.ReferenceGrantSpec{
					From: []gatewayv1beta1.ReferenceGrantFrom{},
				},
			},
			gatewayNamespace: "gw-ns",
			expected:         false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			result := reconciler.grantAllowsGateway(tt.grant, tt.gatewayNamespace)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func setupGatewayFakeClientWithBeta1(objs ...client.Object) client.WithWatch {
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(gatewayv1.Install(scheme))
	utilruntime.Must(gatewayv1beta1.Install(scheme))
	utilruntime.Must(v1alpha1.AddToScheme(scheme))

	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&gatewayv1.Gateway{}).
		Build()
}

func TestGatewayReconciler_CheckSecretReferenceGrant(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	tests := []struct {
		name             string
		gatewayNamespace string
		targetNamespace  string
		refName          string
		grants           []*gatewayv1beta1.ReferenceGrant
		expectedAllowed  bool
	}{
		{
			name:             "grant allows access to specific secret",
			gatewayNamespace: "gw-ns",
			targetNamespace:  "secret-ns",
			refName:          "my-cert",
			grants: []*gatewayv1beta1.ReferenceGrant{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "allow-gw",
						Namespace: "secret-ns",
					},
					Spec: gatewayv1beta1.ReferenceGrantSpec{
						From: []gatewayv1beta1.ReferenceGrantFrom{
							{
								Group:     gatewayv1.GroupName,
								Kind:      "Gateway",
								Namespace: "gw-ns",
							},
						},
						To: []gatewayv1beta1.ReferenceGrantTo{
							{
								Group: "",
								Kind:  "Secret",
								Name:  new(gatewayv1beta1.ObjectName("my-cert")),
							},
						},
					},
				},
			},
			expectedAllowed: true,
		},
		{
			name:             "grant allows access to all secrets in namespace",
			gatewayNamespace: "gw-ns",
			targetNamespace:  "secret-ns",
			refName:          "any-cert",
			grants: []*gatewayv1beta1.ReferenceGrant{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "allow-all",
						Namespace: "secret-ns",
					},
					Spec: gatewayv1beta1.ReferenceGrantSpec{
						From: []gatewayv1beta1.ReferenceGrantFrom{
							{
								Group:     gatewayv1.GroupName,
								Kind:      "Gateway",
								Namespace: "gw-ns",
							},
						},
						To: []gatewayv1beta1.ReferenceGrantTo{
							{
								Group: "",
								Kind:  "Secret",
								// nil Name means all secrets
							},
						},
					},
				},
			},
			expectedAllowed: true,
		},
		{
			name:             "no matching grant",
			gatewayNamespace: "gw-ns",
			targetNamespace:  "secret-ns",
			refName:          "my-cert",
			grants:           []*gatewayv1beta1.ReferenceGrant{},
			expectedAllowed:  false,
		},
		{
			name:             "grant for wrong secret name",
			gatewayNamespace: "gw-ns",
			targetNamespace:  "secret-ns",
			refName:          "my-cert",
			grants: []*gatewayv1beta1.ReferenceGrant{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "allow-other",
						Namespace: "secret-ns",
					},
					Spec: gatewayv1beta1.ReferenceGrantSpec{
						From: []gatewayv1beta1.ReferenceGrantFrom{
							{
								Group:     gatewayv1.GroupName,
								Kind:      "Gateway",
								Namespace: "gw-ns",
							},
						},
						To: []gatewayv1beta1.ReferenceGrantTo{
							{
								Group: "",
								Kind:  "Secret",
								Name:  new(gatewayv1beta1.ObjectName("other-cert")),
							},
						},
					},
				},
			},
			expectedAllowed: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var objs []client.Object
			for _, grant := range tt.grants {
				objs = append(objs, grant)
			}

			fakeClient := setupGatewayFakeClientWithBeta1(objs...)
			reconciler := &GatewayReconciler{
				Client: fakeClient,
				Scheme: fakeClient.Scheme(),
			}

			gateway := &gatewayv1.Gateway{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-gw",
					Namespace: tt.gatewayNamespace,
				},
			}

			ref := gatewayv1.SecretObjectReference{
				Name: gatewayv1.ObjectName(tt.refName),
			}

			allowed, err := reconciler.checkSecretReferenceGrant(ctx, gateway, tt.targetNamespace, ref)
			require.NoError(t, err)
			assert.Equal(t, tt.expectedAllowed, allowed)
		})
	}
}

func TestGatewayReconciler_ReferenceGrantToGateways(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	certNs := gatewayv1.Namespace("cert-ns")

	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gw",
			Namespace: "default",
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "cloudflare-tunnel",
			Listeners: []gatewayv1.Listener{
				{
					Name:     "https",
					Port:     443,
					Protocol: "HTTPS",
					TLS: &gatewayv1.ListenerTLSConfig{
						CertificateRefs: []gatewayv1.SecretObjectReference{
							{Name: "cert", Namespace: &certNs},
						},
					},
				},
			},
		},
	}

	grant := &gatewayv1beta1.ReferenceGrant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "allow-gateway",
			Namespace: "cert-ns",
		},
		Spec: gatewayv1beta1.ReferenceGrantSpec{
			From: []gatewayv1beta1.ReferenceGrantFrom{
				{
					Group:     gatewayv1.GroupName,
					Kind:      kindGateway,
					Namespace: "default",
				},
			},
			To: []gatewayv1beta1.ReferenceGrantTo{
				{
					Group: "",
					Kind:  kindSecret,
				},
			},
		},
	}

	fakeClient := setupGatewayFakeClientWithBeta1(gateway, grant)

	reconciler := &GatewayReconciler{
		Client:           fakeClient,
		Scheme:           fakeClient.Scheme(),
		GatewayClassName: "cloudflare-tunnel",
	}

	requests := reconciler.referenceGrantToGateways(ctx, grant)
	require.Len(t, requests, 1)
	assert.Equal(t, "test-gw", requests[0].Name)
	assert.Equal(t, "default", requests[0].Namespace)
}

func TestGatewayReconciler_ReferenceGrantToGateways_WrongType(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	fakeClient := setupGatewayFakeClientWithBeta1()
	reconciler := &GatewayReconciler{
		Client:           fakeClient,
		Scheme:           fakeClient.Scheme(),
		GatewayClassName: "cloudflare-tunnel",
	}

	// Pass a Secret instead of ReferenceGrant
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "not-a-grant", Namespace: "default"},
	}

	requests := reconciler.referenceGrantToGateways(ctx, secret)
	assert.Nil(t, requests)
}

func TestGatewayReconciler_ReferenceGrantToGateways_IrrelevantGrant(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	// Grant that allows HTTPRoute (not Gateway) access to Services (not Secrets)
	grant := &gatewayv1beta1.ReferenceGrant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "irrelevant-grant",
			Namespace: "some-ns",
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

	fakeClient := setupGatewayFakeClientWithBeta1(grant)
	reconciler := &GatewayReconciler{
		Client:           fakeClient,
		Scheme:           fakeClient.Scheme(),
		GatewayClassName: "cloudflare-tunnel",
	}

	requests := reconciler.referenceGrantToGateways(ctx, grant)
	assert.Nil(t, requests)
}

func TestGatewayReconciler_CountAttachedRoutes_NoRoutes(t *testing.T) {
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
				{Name: "http", Port: 80, Protocol: "HTTP"},
				{Name: "https", Port: 443, Protocol: "HTTPS"},
			},
		},
	}

	fakeClient := setupGatewayFakeClient(gateway)

	reconciler := &GatewayReconciler{
		Client:           fakeClient,
		Scheme:           fakeClient.Scheme(),
		GatewayClassName: "cloudflare-tunnel",
	}

	counts := reconciler.countAttachedRoutes(ctx, gateway)
	assert.Equal(t, int32(0), counts["http"])
	assert.Equal(t, int32(0), counts["https"])
}

func TestGatewayReconciler_Reconcile_ConfigError_SetsStatus(t *testing.T) {
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
			Listeners: []gatewayv1.Listener{
				{Name: "http", Port: 80, Protocol: "HTTP"},
			},
		},
	}

	// GatewayClass referencing a missing config
	gatewayClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: "cloudflare-tunnel",
		},
		Spec: gatewayv1.GatewayClassSpec{
			ControllerName: "test-controller",
			ParametersRef: &gatewayv1.ParametersReference{
				Group: config.ParametersRefGroup,
				Kind:  config.ParametersRefKind,
				Name:  "nonexistent-config",
			},
		},
	}

	fakeClient := setupGatewayFakeClient(gateway, gatewayClass)

	reconciler := &GatewayReconciler{
		Client:           fakeClient,
		Scheme:           fakeClient.Scheme(),
		GatewayClassName: "cloudflare-tunnel",
		ControllerName:   "test-controller",
		ConfigResolver:   config.NewResolver(fakeClient, "default", cfmetrics.NewNoopCollector()),
	}

	result, err := reconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-gateway", Namespace: "default"},
	})

	require.NoError(t, err)
	assert.Equal(t, configErrorRequeueDelay, result.RequeueAfter)

	// Verify the gateway status was set to config error
	var updated gatewayv1.Gateway
	err = fakeClient.Get(ctx, types.NamespacedName{Name: "test-gateway", Namespace: "default"}, &updated)
	require.NoError(t, err)

	require.Len(t, updated.Status.Conditions, 2)
	assert.Equal(t, metav1.ConditionFalse, updated.Status.Conditions[0].Status)
	assert.Equal(t, string(gatewayv1.GatewayReasonInvalidParameters), updated.Status.Conditions[0].Reason)
	assert.Nil(t, updated.Status.Addresses)
}

func TestGatewayReconciler_ValidateSingleCertRef_CrossNamespace_NoGrant(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	fakeClient := setupGatewayFakeClientWithBeta1()
	reconciler := &GatewayReconciler{
		Client: fakeClient,
		Scheme: fakeClient.Scheme(),
	}

	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gw",
			Namespace: "gw-ns",
		},
	}

	otherNs := gatewayv1.Namespace("secret-ns")
	ref := gatewayv1.SecretObjectReference{
		Name:      "cross-ns-secret",
		Namespace: &otherNs,
	}

	status, reason, msg := reconciler.validateSingleCertRef(ctx, gateway, ref)
	assert.Equal(t, metav1.ConditionFalse, status)
	assert.Equal(t, string(gatewayv1.ListenerReasonRefNotPermitted), reason)
	assert.Contains(t, msg, "not permitted")
}
