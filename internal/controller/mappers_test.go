package controller

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
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/api/v1alpha1"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/config"
)

func TestConfigMapper_MapConfigToRequests_ValidConfig(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	gatewayClassConfig := &v1alpha1.GatewayClassConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-config",
		},
		Spec: v1alpha1.GatewayClassConfigSpec{
			TunnelID: "test-tunnel",
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

	fakeClient := setupMapperFakeClient(gatewayClassConfig, gatewayClass)
	mapper := &ConfigMapper{
		Client:           fakeClient,
		GatewayClassName: "cloudflare-tunnel",
		ConfigResolver:   config.NewResolver(fakeClient, "default"),
	}

	expectedRequests := []reconcile.Request{
		{NamespacedName: client.ObjectKey{Name: "test", Namespace: "default"}},
	}

	mapFunc := mapper.MapConfigToRequests(func(_ context.Context) []reconcile.Request {
		return expectedRequests
	})

	result := mapFunc(ctx, gatewayClassConfig)

	require.NotNil(t, result)
	assert.Equal(t, expectedRequests, result)
}

func TestConfigMapper_MapConfigToRequests_WrongConfig(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	gatewayClassConfig := &v1alpha1.GatewayClassConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name: "wrong-config",
		},
		Spec: v1alpha1.GatewayClassConfigSpec{
			TunnelID: "test-tunnel",
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
				Name:  "correct-config",
			},
		},
	}

	fakeClient := setupMapperFakeClient(gatewayClassConfig, gatewayClass)
	mapper := &ConfigMapper{
		Client:           fakeClient,
		GatewayClassName: "cloudflare-tunnel",
		ConfigResolver:   config.NewResolver(fakeClient, "default"),
	}

	mapFunc := mapper.MapConfigToRequests(func(_ context.Context) []reconcile.Request {
		return []reconcile.Request{{NamespacedName: client.ObjectKey{Name: "test"}}}
	})

	result := mapFunc(ctx, gatewayClassConfig)

	assert.Nil(t, result)
}

func TestConfigMapper_MapConfigToRequests_WrongType(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	fakeClient := setupMapperFakeClient()
	mapper := &ConfigMapper{
		Client:           fakeClient,
		GatewayClassName: "cloudflare-tunnel",
	}

	mapFunc := mapper.MapConfigToRequests(func(_ context.Context) []reconcile.Request {
		return []reconcile.Request{{NamespacedName: client.ObjectKey{Name: "test"}}}
	})

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "not-a-config",
			Namespace: "default",
		},
	}

	result := mapFunc(ctx, secret)

	assert.Nil(t, result)
}

func TestConfigMapper_MapConfigToRequests_GatewayClassNotFound(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	gatewayClassConfig := &v1alpha1.GatewayClassConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-config",
		},
	}

	fakeClient := setupMapperFakeClient(gatewayClassConfig)
	mapper := &ConfigMapper{
		Client:           fakeClient,
		GatewayClassName: "non-existent-class",
		ConfigResolver:   config.NewResolver(fakeClient, "default"),
	}

	mapFunc := mapper.MapConfigToRequests(func(_ context.Context) []reconcile.Request {
		return []reconcile.Request{{NamespacedName: client.ObjectKey{Name: "test"}}}
	})

	result := mapFunc(ctx, gatewayClassConfig)

	assert.Nil(t, result)
}

func TestConfigMapper_MapSecretToRequests_ValidSecret(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cf-credentials",
			Namespace: "default",
		},
		Data: map[string][]byte{
			"api-token": []byte("test-token"),
		},
	}

	gatewayClassConfig := &v1alpha1.GatewayClassConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-config",
		},
		Spec: v1alpha1.GatewayClassConfigSpec{
			CloudflareCredentialsSecretRef: v1alpha1.SecretReference{
				Name:      "cf-credentials",
				Namespace: "default",
			},
			TunnelID: "test-tunnel",
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

	fakeClient := setupMapperFakeClient(secret, gatewayClassConfig, gatewayClass)
	mapper := &ConfigMapper{
		Client:           fakeClient,
		GatewayClassName: "cloudflare-tunnel",
		ConfigResolver:   config.NewResolver(fakeClient, "default"),
	}

	expectedRequests := []reconcile.Request{
		{NamespacedName: client.ObjectKey{Name: "test", Namespace: "default"}},
	}

	mapFunc := mapper.MapSecretToRequests(func(_ context.Context) []reconcile.Request {
		return expectedRequests
	})

	result := mapFunc(ctx, secret)

	require.NotNil(t, result)
	assert.Equal(t, expectedRequests, result)
}

func TestConfigMapper_MapSecretToRequests_WrongSecret(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "wrong-secret",
			Namespace: "default",
		},
	}

	gatewayClassConfig := &v1alpha1.GatewayClassConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-config",
		},
		Spec: v1alpha1.GatewayClassConfigSpec{
			CloudflareCredentialsSecretRef: v1alpha1.SecretReference{
				Name:      "cf-credentials",
				Namespace: "default",
			},
			TunnelID: "test-tunnel",
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

	fakeClient := setupMapperFakeClient(secret, gatewayClassConfig, gatewayClass)
	mapper := &ConfigMapper{
		Client:           fakeClient,
		GatewayClassName: "cloudflare-tunnel",
		ConfigResolver:   config.NewResolver(fakeClient, "default"),
	}

	mapFunc := mapper.MapSecretToRequests(func(_ context.Context) []reconcile.Request {
		return []reconcile.Request{{NamespacedName: client.ObjectKey{Name: "test"}}}
	})

	result := mapFunc(ctx, secret)

	assert.Nil(t, result)
}

func TestConfigMapper_MapSecretToRequests_WrongType(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	fakeClient := setupMapperFakeClient()
	mapper := &ConfigMapper{
		Client:           fakeClient,
		GatewayClassName: "cloudflare-tunnel",
	}

	mapFunc := mapper.MapSecretToRequests(func(_ context.Context) []reconcile.Request {
		return []reconcile.Request{{NamespacedName: client.ObjectKey{Name: "test"}}}
	})

	gatewayClassConfig := &v1alpha1.GatewayClassConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name: "not-a-secret",
		},
	}

	result := mapFunc(ctx, gatewayClassConfig)

	assert.Nil(t, result)
}

func TestConfigMapper_MapSecretToRequests_GatewayClassNotFound(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cf-credentials",
			Namespace: "default",
		},
	}

	fakeClient := setupMapperFakeClient(secret)
	mapper := &ConfigMapper{
		Client:           fakeClient,
		GatewayClassName: "non-existent-class",
		ConfigResolver:   config.NewResolver(fakeClient, "default"),
	}

	mapFunc := mapper.MapSecretToRequests(func(_ context.Context) []reconcile.Request {
		return []reconcile.Request{{NamespacedName: client.ObjectKey{Name: "test"}}}
	})

	result := mapFunc(ctx, secret)

	assert.Nil(t, result)
}

func TestSecretMatchesConfig_CredentialsSecret(t *testing.T) {
	t.Parallel()

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cf-credentials",
			Namespace: "default",
		},
	}

	cfg := &v1alpha1.GatewayClassConfig{
		Spec: v1alpha1.GatewayClassConfigSpec{
			CloudflareCredentialsSecretRef: v1alpha1.SecretReference{
				Name:      "cf-credentials",
				Namespace: "default",
			},
		},
	}

	assert.True(t, SecretMatchesConfig(secret, cfg))
}

func TestSecretMatchesConfig_CredentialsSecretEmptyNamespace(t *testing.T) {
	t.Parallel()

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cf-credentials",
			Namespace: "any-ns",
		},
	}

	cfg := &v1alpha1.GatewayClassConfig{
		Spec: v1alpha1.GatewayClassConfigSpec{
			CloudflareCredentialsSecretRef: v1alpha1.SecretReference{
				Name:      "cf-credentials",
				Namespace: "",
			},
		},
	}

	assert.True(t, SecretMatchesConfig(secret, cfg))
}

func TestSecretMatchesConfig_TunnelTokenSecret(t *testing.T) {
	t.Parallel()

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "tunnel-token",
			Namespace: "default",
		},
	}

	cfg := &v1alpha1.GatewayClassConfig{
		Spec: v1alpha1.GatewayClassConfigSpec{
			CloudflareCredentialsSecretRef: v1alpha1.SecretReference{
				Name:      "cf-credentials",
				Namespace: "default",
			},
			TunnelTokenSecretRef: &v1alpha1.SecretReference{
				Name:      "tunnel-token",
				Namespace: "default",
			},
		},
	}

	assert.True(t, SecretMatchesConfig(secret, cfg))
}

func TestSecretMatchesConfig_TunnelTokenSecretEmptyNamespace(t *testing.T) {
	t.Parallel()

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "tunnel-token",
			Namespace: "other-ns",
		},
	}

	cfg := &v1alpha1.GatewayClassConfig{
		Spec: v1alpha1.GatewayClassConfigSpec{
			CloudflareCredentialsSecretRef: v1alpha1.SecretReference{
				Name: "cf-credentials",
			},
			TunnelTokenSecretRef: &v1alpha1.SecretReference{
				Name:      "tunnel-token",
				Namespace: "",
			},
		},
	}

	assert.True(t, SecretMatchesConfig(secret, cfg))
}

func TestSecretMatchesConfig_NoMatch(t *testing.T) {
	t.Parallel()

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "random-secret",
			Namespace: "default",
		},
	}

	cfg := &v1alpha1.GatewayClassConfig{
		Spec: v1alpha1.GatewayClassConfigSpec{
			CloudflareCredentialsSecretRef: v1alpha1.SecretReference{
				Name:      "cf-credentials",
				Namespace: "default",
			},
			TunnelTokenSecretRef: &v1alpha1.SecretReference{
				Name:      "tunnel-token",
				Namespace: "default",
			},
		},
	}

	assert.False(t, SecretMatchesConfig(secret, cfg))
}

func TestSecretMatchesConfig_NoTunnelTokenRef(t *testing.T) {
	t.Parallel()

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "tunnel-token",
			Namespace: "default",
		},
	}

	cfg := &v1alpha1.GatewayClassConfig{
		Spec: v1alpha1.GatewayClassConfigSpec{
			CloudflareCredentialsSecretRef: v1alpha1.SecretReference{
				Name:      "cf-credentials",
				Namespace: "default",
			},
			TunnelTokenSecretRef: nil,
		},
	}

	assert.False(t, SecretMatchesConfig(secret, cfg))
}

func TestSecretMatchesConfig_WrongNamespace(t *testing.T) {
	t.Parallel()

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cf-credentials",
			Namespace: "wrong-ns",
		},
	}

	cfg := &v1alpha1.GatewayClassConfig{
		Spec: v1alpha1.GatewayClassConfigSpec{
			CloudflareCredentialsSecretRef: v1alpha1.SecretReference{
				Name:      "cf-credentials",
				Namespace: "default",
			},
		},
	}

	assert.False(t, SecretMatchesConfig(secret, cfg))
}

func TestConfigMapper_IsConfigForOurClass_NoParametersRef(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	gatewayClassConfig := &v1alpha1.GatewayClassConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-config",
		},
	}

	gatewayClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: "cloudflare-tunnel",
		},
		Spec: gatewayv1.GatewayClassSpec{
			ControllerName: "test-controller",
			ParametersRef:  nil,
		},
	}

	fakeClient := setupMapperFakeClient(gatewayClassConfig, gatewayClass)
	mapper := &ConfigMapper{
		Client:           fakeClient,
		GatewayClassName: "cloudflare-tunnel",
		ConfigResolver:   config.NewResolver(fakeClient, "default"),
	}

	result := mapper.isConfigForOurClass(ctx, gatewayClassConfig)

	assert.False(t, result)
}

func setupMapperFakeClient(objs ...client.Object) client.Client {
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(gatewayv1.Install(scheme))
	utilruntime.Must(v1alpha1.AddToScheme(scheme))

	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		Build()
}
