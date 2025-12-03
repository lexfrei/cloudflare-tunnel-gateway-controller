package config_test

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
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/api/v1alpha1"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/config"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/metrics"
)

func TestNewResolver(t *testing.T) {
	t.Parallel()

	fakeClient := setupFakeClient()
	resolver := config.NewResolver(fakeClient, "default", metrics.NewNoopCollector())

	require.NotNil(t, resolver)
}

func TestResolveFromGatewayClass_Valid(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cf-credentials",
			Namespace: "default",
		},
		Data: map[string][]byte{
			"api-token": []byte("test-api-token"),
		},
	}

	tunnelSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "tunnel-token",
			Namespace: "default",
		},
		Data: map[string][]byte{
			"tunnel-token": []byte("test-tunnel-token"),
		},
	}

	enabled := true
	gatewayClassConfig := &v1alpha1.GatewayClassConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-config",
		},
		Spec: v1alpha1.GatewayClassConfigSpec{
			CloudflareCredentialsSecretRef: v1alpha1.SecretReference{
				Name:      "cf-credentials",
				Namespace: "default",
			},
			AccountID: "test-account-id",
			TunnelID:  "12345678-1234-1234-1234-123456789abc",
			TunnelTokenSecretRef: &v1alpha1.SecretReference{
				Name:      "tunnel-token",
				Namespace: "default",
			},
			Cloudflared: v1alpha1.CloudflaredConfig{
				Enabled:   &enabled,
				Replicas:  2,
				Namespace: "cf-system",
				Protocol:  "quic",
			},
		},
	}

	gatewayClass := newGatewayClass("test-class", "test-config")

	fakeClient := setupFakeClient(secret, tunnelSecret, gatewayClassConfig, gatewayClass)
	resolver := config.NewResolver(fakeClient, "default", metrics.NewNoopCollector())

	resolved, err := resolver.ResolveFromGatewayClass(ctx, gatewayClass)

	require.NoError(t, err)
	require.NotNil(t, resolved)
	assert.Equal(t, "test-api-token", resolved.APIToken)
	assert.Equal(t, "test-account-id", resolved.AccountID)
	assert.Equal(t, "12345678-1234-1234-1234-123456789abc", resolved.TunnelID)
	assert.Equal(t, "test-tunnel-token", resolved.TunnelToken)
	assert.True(t, resolved.CloudflaredEnabled)
	assert.Equal(t, int32(2), resolved.CloudflaredReplicas)
	assert.Equal(t, "cf-system", resolved.CloudflaredNamespace)
	assert.Equal(t, "quic", resolved.CloudflaredProtocol)
	assert.Equal(t, "test-config", resolved.ConfigName)
}

func TestResolveFromGatewayClass_MissingParametersRef(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	gatewayClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-class",
		},
		Spec: gatewayv1.GatewayClassSpec{
			ControllerName: "test-controller",
			ParametersRef:  nil,
		},
	}

	fakeClient := setupFakeClient(gatewayClass)
	resolver := config.NewResolver(fakeClient, "default", metrics.NewNoopCollector())

	_, err := resolver.ResolveFromGatewayClass(ctx, gatewayClass)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "has no parametersRef")
}

func TestResolveFromGatewayClass_WrongGroup(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	gatewayClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-class",
		},
		Spec: gatewayv1.GatewayClassSpec{
			ControllerName: "test-controller",
			ParametersRef: &gatewayv1.ParametersReference{
				Group: "wrong.group",
				Kind:  "GatewayClassConfig",
				Name:  "test-config",
			},
		},
	}

	fakeClient := setupFakeClient(gatewayClass)
	resolver := config.NewResolver(fakeClient, "default", metrics.NewNoopCollector())

	_, err := resolver.ResolveFromGatewayClass(ctx, gatewayClass)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported parametersRef group")
}

func TestResolveFromGatewayClass_WrongKind(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	gatewayClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-class",
		},
		Spec: gatewayv1.GatewayClassSpec{
			ControllerName: "test-controller",
			ParametersRef: &gatewayv1.ParametersReference{
				Group: config.ParametersRefGroup,
				Kind:  "WrongKind",
				Name:  "test-config",
			},
		},
	}

	fakeClient := setupFakeClient(gatewayClass)
	resolver := config.NewResolver(fakeClient, "default", metrics.NewNoopCollector())

	_, err := resolver.ResolveFromGatewayClass(ctx, gatewayClass)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported parametersRef kind")
}

func TestResolveFromGatewayClass_ConfigNotFound(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	gatewayClass := newGatewayClass("test-class", "non-existent-config")

	fakeClient := setupFakeClient(gatewayClass)
	resolver := config.NewResolver(fakeClient, "default", metrics.NewNoopCollector())

	_, err := resolver.ResolveFromGatewayClass(ctx, gatewayClass)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to get GatewayClassConfig")
}

func TestResolveFromGatewayClass_SecretNotFound(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	gatewayClassConfig := &v1alpha1.GatewayClassConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-config",
		},
		Spec: v1alpha1.GatewayClassConfigSpec{
			CloudflareCredentialsSecretRef: v1alpha1.SecretReference{
				Name:      "non-existent-secret",
				Namespace: "default",
			},
			TunnelID: "12345678-1234-1234-1234-123456789abc",
		},
	}

	gatewayClass := newGatewayClass("test-class", "test-config")

	fakeClient := setupFakeClient(gatewayClassConfig, gatewayClass)
	resolver := config.NewResolver(fakeClient, "default", metrics.NewNoopCollector())

	_, err := resolver.ResolveFromGatewayClass(ctx, gatewayClass)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to get Cloudflare credentials secret")
}

func TestResolveFromGatewayClass_MissingAPIToken(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cf-credentials",
			Namespace: "default",
		},
		Data: map[string][]byte{
			"wrong-key": []byte("test-api-token"),
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
			TunnelID: "12345678-1234-1234-1234-123456789abc",
		},
	}

	gatewayClass := newGatewayClass("test-class", "test-config")

	fakeClient := setupFakeClient(secret, gatewayClassConfig, gatewayClass)
	resolver := config.NewResolver(fakeClient, "default", metrics.NewNoopCollector())

	_, err := resolver.ResolveFromGatewayClass(ctx, gatewayClass)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not contain key api-token")
}

func TestResolveFromGatewayClassName_Valid(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cf-credentials",
			Namespace: "default",
		},
		Data: map[string][]byte{
			"api-token": []byte("test-api-token"),
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

	gatewayClass := newGatewayClass("test-class", "test-config")

	fakeClient := setupFakeClient(secret, gatewayClassConfig, gatewayClass)
	resolver := config.NewResolver(fakeClient, "default", metrics.NewNoopCollector())

	resolved, err := resolver.ResolveFromGatewayClassName(ctx, "test-class")

	require.NoError(t, err)
	require.NotNil(t, resolved)
	assert.Equal(t, "test-api-token", resolved.APIToken)
}

func TestResolveFromGatewayClassName_NotFound(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	fakeClient := setupFakeClient()
	resolver := config.NewResolver(fakeClient, "default", metrics.NewNoopCollector())

	_, err := resolver.ResolveFromGatewayClassName(ctx, "non-existent")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to get GatewayClass")
}

func TestResolveConfig_CloudflaredEnabled_MissingTunnelTokenRef(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cf-credentials",
			Namespace: "default",
		},
		Data: map[string][]byte{
			"api-token": []byte("test-api-token"),
		},
	}

	enabled := true
	gatewayClassConfig := &v1alpha1.GatewayClassConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-config",
		},
		Spec: v1alpha1.GatewayClassConfigSpec{
			CloudflareCredentialsSecretRef: v1alpha1.SecretReference{
				Name:      "cf-credentials",
				Namespace: "default",
			},
			TunnelID:             "12345678-1234-1234-1234-123456789abc",
			TunnelTokenSecretRef: nil,
			Cloudflared: v1alpha1.CloudflaredConfig{
				Enabled: &enabled,
			},
		},
	}

	gatewayClass := newGatewayClass("test-class", "test-config")

	fakeClient := setupFakeClient(secret, gatewayClassConfig, gatewayClass)
	resolver := config.NewResolver(fakeClient, "default", metrics.NewNoopCollector())

	_, err := resolver.ResolveFromGatewayClass(ctx, gatewayClass)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "tunnelTokenSecretRef is required")
}

func TestResolveConfig_CloudflaredEnabled_TunnelSecretNotFound(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cf-credentials",
			Namespace: "default",
		},
		Data: map[string][]byte{
			"api-token": []byte("test-api-token"),
		},
	}

	enabled := true
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
			TunnelTokenSecretRef: &v1alpha1.SecretReference{
				Name:      "non-existent-tunnel-secret",
				Namespace: "default",
			},
			Cloudflared: v1alpha1.CloudflaredConfig{
				Enabled: &enabled,
			},
		},
	}

	gatewayClass := newGatewayClass("test-class", "test-config")

	fakeClient := setupFakeClient(secret, gatewayClassConfig, gatewayClass)
	resolver := config.NewResolver(fakeClient, "default", metrics.NewNoopCollector())

	_, err := resolver.ResolveFromGatewayClass(ctx, gatewayClass)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to get tunnel token secret")
}

func TestResolveConfig_CloudflaredEnabled_MissingTunnelToken(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cf-credentials",
			Namespace: "default",
		},
		Data: map[string][]byte{
			"api-token": []byte("test-api-token"),
		},
	}

	tunnelSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "tunnel-token",
			Namespace: "default",
		},
		Data: map[string][]byte{
			"wrong-key": []byte("test-tunnel-token"),
		},
	}

	enabled := true
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
			TunnelTokenSecretRef: &v1alpha1.SecretReference{
				Name:      "tunnel-token",
				Namespace: "default",
			},
			Cloudflared: v1alpha1.CloudflaredConfig{
				Enabled: &enabled,
			},
		},
	}

	gatewayClass := newGatewayClass("test-class", "test-config")

	fakeClient := setupFakeClient(secret, tunnelSecret, gatewayClassConfig, gatewayClass)
	resolver := config.NewResolver(fakeClient, "default", metrics.NewNoopCollector())

	_, err := resolver.ResolveFromGatewayClass(ctx, gatewayClass)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not contain key tunnel-token")
}

func TestResolveConfig_CloudflaredDisabled(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cf-credentials",
			Namespace: "default",
		},
		Data: map[string][]byte{
			"api-token": []byte("test-api-token"),
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

	gatewayClass := newGatewayClass("test-class", "test-config")

	fakeClient := setupFakeClient(secret, gatewayClassConfig, gatewayClass)
	resolver := config.NewResolver(fakeClient, "default", metrics.NewNoopCollector())

	resolved, err := resolver.ResolveFromGatewayClass(ctx, gatewayClass)

	require.NoError(t, err)
	assert.False(t, resolved.CloudflaredEnabled)
	assert.Empty(t, resolved.TunnelToken)
}

func TestResolveConfig_AWGConfig(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cf-credentials",
			Namespace: "default",
		},
		Data: map[string][]byte{
			"api-token": []byte("test-api-token"),
		},
	}

	tunnelSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "tunnel-token",
			Namespace: "default",
		},
		Data: map[string][]byte{
			"tunnel-token": []byte("test-tunnel-token"),
		},
	}

	enabled := true
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
			TunnelTokenSecretRef: &v1alpha1.SecretReference{
				Name:      "tunnel-token",
				Namespace: "default",
			},
			Cloudflared: v1alpha1.CloudflaredConfig{
				Enabled: &enabled,
				AWG: &v1alpha1.AWGConfig{
					SecretName:      "awg-secret",
					InterfacePrefix: "my-awg",
				},
			},
		},
	}

	gatewayClass := newGatewayClass("test-class", "test-config")

	fakeClient := setupFakeClient(secret, tunnelSecret, gatewayClassConfig, gatewayClass)
	resolver := config.NewResolver(fakeClient, "default", metrics.NewNoopCollector())

	resolved, err := resolver.ResolveFromGatewayClass(ctx, gatewayClass)

	require.NoError(t, err)
	assert.Equal(t, "awg-secret", resolved.AWGSecretName)
	assert.Equal(t, "my-awg", resolved.AWGInterfacePrefix)
}

func TestResolveConfig_AWGConfig_DefaultPrefix(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cf-credentials",
			Namespace: "default",
		},
		Data: map[string][]byte{
			"api-token": []byte("test-api-token"),
		},
	}

	tunnelSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "tunnel-token",
			Namespace: "default",
		},
		Data: map[string][]byte{
			"tunnel-token": []byte("test-tunnel-token"),
		},
	}

	enabled := true
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
			TunnelTokenSecretRef: &v1alpha1.SecretReference{
				Name:      "tunnel-token",
				Namespace: "default",
			},
			Cloudflared: v1alpha1.CloudflaredConfig{
				Enabled: &enabled,
				AWG: &v1alpha1.AWGConfig{
					SecretName:      "awg-secret",
					InterfacePrefix: "",
				},
			},
		},
	}

	gatewayClass := newGatewayClass("test-class", "test-config")

	fakeClient := setupFakeClient(secret, tunnelSecret, gatewayClassConfig, gatewayClass)
	resolver := config.NewResolver(fakeClient, "default", metrics.NewNoopCollector())

	resolved, err := resolver.ResolveFromGatewayClass(ctx, gatewayClass)

	require.NoError(t, err)
	assert.Equal(t, "awg-cfd", resolved.AWGInterfacePrefix)
}

func TestResolveConfig_DefaultNamespace(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cf-credentials",
			Namespace: "my-default-ns",
		},
		Data: map[string][]byte{
			"api-token": []byte("test-api-token"),
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
				Namespace: "",
			},
			TunnelID: "12345678-1234-1234-1234-123456789abc",
			Cloudflared: v1alpha1.CloudflaredConfig{
				Enabled: &disabled,
			},
		},
	}

	gatewayClass := newGatewayClass("test-class", "test-config")

	fakeClient := setupFakeClient(secret, gatewayClassConfig, gatewayClass)
	resolver := config.NewResolver(fakeClient, "my-default-ns", metrics.NewNoopCollector())

	resolved, err := resolver.ResolveFromGatewayClass(ctx, gatewayClass)

	require.NoError(t, err)
	assert.Equal(t, "test-api-token", resolved.APIToken)
}

func TestResolveConfig_AccountIDFromSecret(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cf-credentials",
			Namespace: "default",
		},
		Data: map[string][]byte{
			"api-token":  []byte("test-api-token"),
			"account-id": []byte("secret-account-id"),
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
			AccountID: "",
			TunnelID:  "12345678-1234-1234-1234-123456789abc",
			Cloudflared: v1alpha1.CloudflaredConfig{
				Enabled: &disabled,
			},
		},
	}

	gatewayClass := newGatewayClass("test-class", "test-config")

	fakeClient := setupFakeClient(secret, gatewayClassConfig, gatewayClass)
	resolver := config.NewResolver(fakeClient, "default", metrics.NewNoopCollector())

	resolved, err := resolver.ResolveFromGatewayClass(ctx, gatewayClass)

	require.NoError(t, err)
	assert.Equal(t, "secret-account-id", resolved.AccountID)
}

func TestResolveConfig_AccountIDFromSpec(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cf-credentials",
			Namespace: "default",
		},
		Data: map[string][]byte{
			"api-token":  []byte("test-api-token"),
			"account-id": []byte("secret-account-id"),
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
			AccountID: "spec-account-id",
			TunnelID:  "12345678-1234-1234-1234-123456789abc",
			Cloudflared: v1alpha1.CloudflaredConfig{
				Enabled: &disabled,
			},
		},
	}

	gatewayClass := newGatewayClass("test-class", "test-config")

	fakeClient := setupFakeClient(secret, gatewayClassConfig, gatewayClass)
	resolver := config.NewResolver(fakeClient, "default", metrics.NewNoopCollector())

	resolved, err := resolver.ResolveFromGatewayClass(ctx, gatewayClass)

	require.NoError(t, err)
	assert.Equal(t, "spec-account-id", resolved.AccountID)
}

func TestResolveConfig_CustomAPITokenKey(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cf-credentials",
			Namespace: "default",
		},
		Data: map[string][]byte{
			"my-custom-token-key": []byte("test-api-token"),
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
				Key:       "my-custom-token-key",
			},
			TunnelID: "12345678-1234-1234-1234-123456789abc",
			Cloudflared: v1alpha1.CloudflaredConfig{
				Enabled: &disabled,
			},
		},
	}

	gatewayClass := newGatewayClass("test-class", "test-config")

	fakeClient := setupFakeClient(secret, gatewayClassConfig, gatewayClass)
	resolver := config.NewResolver(fakeClient, "default", metrics.NewNoopCollector())

	resolved, err := resolver.ResolveFromGatewayClass(ctx, gatewayClass)

	require.NoError(t, err)
	assert.Equal(t, "test-api-token", resolved.APIToken)
}

func TestResolveConfig_CustomTunnelTokenKey(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cf-credentials",
			Namespace: "default",
		},
		Data: map[string][]byte{
			"api-token": []byte("test-api-token"),
		},
	}

	tunnelSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "tunnel-token",
			Namespace: "default",
		},
		Data: map[string][]byte{
			"my-custom-tunnel-key": []byte("test-tunnel-token"),
		},
	}

	enabled := true
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
			TunnelTokenSecretRef: &v1alpha1.SecretReference{
				Name:      "tunnel-token",
				Namespace: "default",
				Key:       "my-custom-tunnel-key",
			},
			Cloudflared: v1alpha1.CloudflaredConfig{
				Enabled: &enabled,
			},
		},
	}

	gatewayClass := newGatewayClass("test-class", "test-config")

	fakeClient := setupFakeClient(secret, tunnelSecret, gatewayClassConfig, gatewayClass)
	resolver := config.NewResolver(fakeClient, "default", metrics.NewNoopCollector())

	resolved, err := resolver.ResolveFromGatewayClass(ctx, gatewayClass)

	require.NoError(t, err)
	assert.Equal(t, "test-tunnel-token", resolved.TunnelToken)
}

func TestCreateCloudflareClient(t *testing.T) {
	t.Parallel()

	fakeClient := setupFakeClient()
	resolver := config.NewResolver(fakeClient, "default", metrics.NewNoopCollector())

	resolved := &config.ResolvedConfig{
		APIToken: "test-api-token",
	}

	cfClient := resolver.CreateCloudflareClient(resolved)

	require.NotNil(t, cfClient)
}

func TestResolveAccountID_FromResolvedConfig(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	fakeClient := setupFakeClient()
	resolver := config.NewResolver(fakeClient, "default", metrics.NewNoopCollector())

	resolved := &config.ResolvedConfig{
		AccountID:  "already-resolved-account-id",
		ConfigName: "test-config",
	}

	accountID, err := resolver.ResolveAccountID(ctx, nil, resolved)

	require.NoError(t, err)
	assert.Equal(t, "already-resolved-account-id", accountID)
}

func TestGetConfigForGatewayClass_Valid(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	gatewayClassConfig := &v1alpha1.GatewayClassConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-config",
		},
		Spec: v1alpha1.GatewayClassConfigSpec{
			TunnelID: "12345678-1234-1234-1234-123456789abc",
		},
	}

	gatewayClass := newGatewayClass("test-class", "test-config")

	fakeClient := setupFakeClient(gatewayClassConfig, gatewayClass)
	resolver := config.NewResolver(fakeClient, "default", metrics.NewNoopCollector())

	cfg, err := resolver.GetConfigForGatewayClass(ctx, gatewayClass)

	require.NoError(t, err)
	assert.Equal(t, "test-config", cfg.Name)
	assert.Equal(t, "12345678-1234-1234-1234-123456789abc", cfg.Spec.TunnelID)
}

func TestGetConfigForGatewayClass_MissingParametersRef(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	gatewayClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-class",
		},
		Spec: gatewayv1.GatewayClassSpec{
			ControllerName: "test-controller",
			ParametersRef:  nil,
		},
	}

	fakeClient := setupFakeClient(gatewayClass)
	resolver := config.NewResolver(fakeClient, "default", metrics.NewNoopCollector())

	_, err := resolver.GetConfigForGatewayClass(ctx, gatewayClass)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "has no parametersRef")
}

func TestGetConfigForGatewayClass_WrongGroupKind(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	gatewayClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-class",
		},
		Spec: gatewayv1.GatewayClassSpec{
			ControllerName: "test-controller",
			ParametersRef: &gatewayv1.ParametersReference{
				Group: "wrong.group",
				Kind:  "WrongKind",
				Name:  "test-config",
			},
		},
	}

	fakeClient := setupFakeClient(gatewayClass)
	resolver := config.NewResolver(fakeClient, "default", metrics.NewNoopCollector())

	_, err := resolver.GetConfigForGatewayClass(ctx, gatewayClass)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported parametersRef")
}

func TestGetConfigForGatewayClass_ConfigNotFound(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	gatewayClass := newGatewayClass("test-class", "non-existent-config")

	fakeClient := setupFakeClient(gatewayClass)
	resolver := config.NewResolver(fakeClient, "default", metrics.NewNoopCollector())

	_, err := resolver.GetConfigForGatewayClass(ctx, gatewayClass)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to get GatewayClassConfig")
}

func TestResolveConfig_DefaultCloudflaredSettings(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cf-credentials",
			Namespace: "default",
		},
		Data: map[string][]byte{
			"api-token": []byte("test-api-token"),
		},
	}

	tunnelSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "tunnel-token",
			Namespace: "default",
		},
		Data: map[string][]byte{
			"tunnel-token": []byte("test-tunnel-token"),
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
			TunnelID: "12345678-1234-1234-1234-123456789abc",
			TunnelTokenSecretRef: &v1alpha1.SecretReference{
				Name:      "tunnel-token",
				Namespace: "default",
			},
			Cloudflared: v1alpha1.CloudflaredConfig{},
		},
	}

	gatewayClass := newGatewayClass("test-class", "test-config")

	fakeClient := setupFakeClient(secret, tunnelSecret, gatewayClassConfig, gatewayClass)
	resolver := config.NewResolver(fakeClient, "default", metrics.NewNoopCollector())

	resolved, err := resolver.ResolveFromGatewayClass(ctx, gatewayClass)

	require.NoError(t, err)
	assert.True(t, resolved.CloudflaredEnabled)
	assert.Equal(t, int32(1), resolved.CloudflaredReplicas)
	assert.Equal(t, "cloudflare-tunnel-system", resolved.CloudflaredNamespace)
}

func setupFakeClient(objs ...client.Object) client.Client {
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(gatewayv1.Install(scheme))
	utilruntime.Must(v1alpha1.AddToScheme(scheme))

	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		Build()
}

func newGatewayClass(name, configName string) *gatewayv1.GatewayClass {
	return &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: gatewayv1.GatewayClassSpec{
			ControllerName: "test-controller",
			ParametersRef: &gatewayv1.ParametersReference{
				Group: config.ParametersRefGroup,
				Kind:  config.ParametersRefKind,
				Name:  configName,
			},
		},
	}
}
