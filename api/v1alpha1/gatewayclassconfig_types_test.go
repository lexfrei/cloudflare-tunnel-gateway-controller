package v1alpha1

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIsCloudflaredEnabled_Default(t *testing.T) {
	t.Parallel()

	spec := &GatewayClassConfigSpec{
		Cloudflared: CloudflaredConfig{
			Enabled: nil,
		},
	}

	assert.True(t, spec.IsCloudflaredEnabled())
}

func TestIsCloudflaredEnabled_ExplicitTrue(t *testing.T) {
	t.Parallel()

	enabled := true
	spec := &GatewayClassConfigSpec{
		Cloudflared: CloudflaredConfig{
			Enabled: &enabled,
		},
	}

	assert.True(t, spec.IsCloudflaredEnabled())
}

func TestIsCloudflaredEnabled_ExplicitFalse(t *testing.T) {
	t.Parallel()

	disabled := false
	spec := &GatewayClassConfigSpec{
		Cloudflared: CloudflaredConfig{
			Enabled: &disabled,
		},
	}

	assert.False(t, spec.IsCloudflaredEnabled())
}

func TestGetCloudflaredReplicas_Default(t *testing.T) {
	t.Parallel()

	spec := &GatewayClassConfigSpec{
		Cloudflared: CloudflaredConfig{
			Replicas: 0,
		},
	}

	assert.Equal(t, int32(1), spec.GetCloudflaredReplicas())
}

func TestGetCloudflaredReplicas_Custom(t *testing.T) {
	t.Parallel()

	spec := &GatewayClassConfigSpec{
		Cloudflared: CloudflaredConfig{
			Replicas: 3,
		},
	}

	assert.Equal(t, int32(3), spec.GetCloudflaredReplicas())
}

func TestGetCloudflaredNamespace_Default(t *testing.T) {
	t.Parallel()

	spec := &GatewayClassConfigSpec{
		Cloudflared: CloudflaredConfig{
			Namespace: "",
		},
	}

	assert.Equal(t, "cloudflare-tunnel-system", spec.GetCloudflaredNamespace())
}

func TestGetCloudflaredNamespace_Custom(t *testing.T) {
	t.Parallel()

	spec := &GatewayClassConfigSpec{
		Cloudflared: CloudflaredConfig{
			Namespace: "custom-namespace",
		},
	}

	assert.Equal(t, "custom-namespace", spec.GetCloudflaredNamespace())
}

func TestGetAPITokenKey_Default(t *testing.T) {
	t.Parallel()

	ref := &SecretReference{
		Name: "my-secret",
		Key:  "",
	}

	assert.Equal(t, "api-token", ref.GetAPITokenKey())
}

func TestGetAPITokenKey_Custom(t *testing.T) {
	t.Parallel()

	ref := &SecretReference{
		Name: "my-secret",
		Key:  "custom-key",
	}

	assert.Equal(t, "custom-key", ref.GetAPITokenKey())
}

func TestGetTunnelTokenKey_Default(t *testing.T) {
	t.Parallel()

	ref := &SecretReference{
		Name: "my-secret",
		Key:  "",
	}

	assert.Equal(t, "tunnel-token", ref.GetTunnelTokenKey())
}

func TestGetTunnelTokenKey_Custom(t *testing.T) {
	t.Parallel()

	ref := &SecretReference{
		Name: "my-secret",
		Key:  "custom-tunnel-key",
	}

	assert.Equal(t, "custom-tunnel-key", ref.GetTunnelTokenKey())
}

func TestSecretReference_FieldsPresent(t *testing.T) {
	t.Parallel()

	ref := SecretReference{
		Name:      "test-secret",
		Namespace: "test-ns",
		Key:       "test-key",
	}

	assert.Equal(t, "test-secret", ref.Name)
	assert.Equal(t, "test-ns", ref.Namespace)
	assert.Equal(t, "test-key", ref.Key)
}

func TestAWGConfig_FieldsPresent(t *testing.T) {
	t.Parallel()

	awg := AWGConfig{
		SecretName:      "awg-secret",
		InterfacePrefix: "awg-test",
	}

	assert.Equal(t, "awg-secret", awg.SecretName)
	assert.Equal(t, "awg-test", awg.InterfacePrefix)
}

func TestCloudflaredConfig_FieldsPresent(t *testing.T) {
	t.Parallel()

	enabled := true
	cfg := CloudflaredConfig{
		Enabled:   &enabled,
		Replicas:  2,
		Namespace: "cf-ns",
		Protocol:  "quic",
		AWG: &AWGConfig{
			SecretName: "awg-secret",
		},
	}

	assert.True(t, *cfg.Enabled)
	assert.Equal(t, int32(2), cfg.Replicas)
	assert.Equal(t, "cf-ns", cfg.Namespace)
	assert.Equal(t, "quic", cfg.Protocol)
	assert.NotNil(t, cfg.AWG)
	assert.Equal(t, "awg-secret", cfg.AWG.SecretName)
}

func TestGatewayClassConfigSpec_FieldsPresent(t *testing.T) {
	t.Parallel()

	spec := GatewayClassConfigSpec{
		CloudflareCredentialsSecretRef: SecretReference{
			Name: "cf-creds",
		},
		AccountID: "test-account",
		TunnelID:  "test-tunnel",
		TunnelTokenSecretRef: &SecretReference{
			Name: "tunnel-token",
		},
		Cloudflared: CloudflaredConfig{
			Replicas: 1,
		},
	}

	assert.Equal(t, "cf-creds", spec.CloudflareCredentialsSecretRef.Name)
	assert.Equal(t, "test-account", spec.AccountID)
	assert.Equal(t, "test-tunnel", spec.TunnelID)
	assert.NotNil(t, spec.TunnelTokenSecretRef)
	assert.Equal(t, "tunnel-token", spec.TunnelTokenSecretRef.Name)
}

func TestGatewayClassConfig_TypeMeta(t *testing.T) {
	t.Parallel()

	gcc := GatewayClassConfig{}
	gcc.Kind = "GatewayClassConfig"
	gcc.APIVersion = "cf.k8s.lex.la/v1alpha1"

	assert.Equal(t, "GatewayClassConfig", gcc.Kind)
	assert.Equal(t, "cf.k8s.lex.la/v1alpha1", gcc.APIVersion)
}

func TestGatewayClassConfigList_Items(t *testing.T) {
	t.Parallel()

	list := GatewayClassConfigList{
		Items: []GatewayClassConfig{
			{Spec: GatewayClassConfigSpec{TunnelID: "tunnel-1"}},
			{Spec: GatewayClassConfigSpec{TunnelID: "tunnel-2"}},
		},
	}

	assert.Len(t, list.Items, 2)
	assert.Equal(t, "tunnel-1", list.Items[0].Spec.TunnelID)
	assert.Equal(t, "tunnel-2", list.Items[1].Spec.TunnelID)
}

func TestGatewayClassConfigStatus_Conditions(t *testing.T) {
	t.Parallel()

	status := GatewayClassConfigStatus{}
	assert.Empty(t, status.Conditions)
}
