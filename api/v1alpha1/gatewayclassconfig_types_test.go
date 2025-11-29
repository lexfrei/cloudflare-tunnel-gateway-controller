package v1alpha1

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const testModifiedValue = "modified"

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

func TestSecretReference_DeepCopy(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   *SecretReference
	}{
		{
			name: "nil input",
			in:   nil,
		},
		{
			name: "empty struct",
			in:   &SecretReference{},
		},
		{
			name: "full struct",
			in: &SecretReference{
				Name:      "test-secret",
				Namespace: "test-ns",
				Key:       "test-key",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			out := tt.in.DeepCopy()

			if tt.in == nil {
				assert.Nil(t, out)
				return
			}

			require.NotNil(t, out)
			assert.Equal(t, tt.in.Name, out.Name)
			assert.Equal(t, tt.in.Namespace, out.Namespace)
			assert.Equal(t, tt.in.Key, out.Key)

			out.Name = testModifiedValue
			assert.NotEqual(t, tt.in.Name, out.Name)
		})
	}
}

func TestAWGConfig_DeepCopy(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   *AWGConfig
	}{
		{
			name: "nil input",
			in:   nil,
		},
		{
			name: "empty struct",
			in:   &AWGConfig{},
		},
		{
			name: "full struct",
			in: &AWGConfig{
				SecretName:      "awg-secret",
				InterfacePrefix: "awg-test",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			out := tt.in.DeepCopy()

			if tt.in == nil {
				assert.Nil(t, out)
				return
			}

			require.NotNil(t, out)
			assert.Equal(t, tt.in.SecretName, out.SecretName)
			assert.Equal(t, tt.in.InterfacePrefix, out.InterfacePrefix)

			out.SecretName = testModifiedValue
			assert.NotEqual(t, tt.in.SecretName, out.SecretName)
		})
	}
}

func TestCloudflaredConfig_DeepCopy(t *testing.T) {
	t.Parallel()

	enabled := true
	tests := []struct {
		name string
		in   *CloudflaredConfig
	}{
		{
			name: "nil input",
			in:   nil,
		},
		{
			name: "empty struct",
			in:   &CloudflaredConfig{},
		},
		{
			name: "with enabled pointer",
			in: &CloudflaredConfig{
				Enabled:   &enabled,
				Replicas:  2,
				Namespace: "cf-ns",
				Protocol:  "quic",
			},
		},
		{
			name: "with AWG config",
			in: &CloudflaredConfig{
				Enabled: &enabled,
				AWG: &AWGConfig{
					SecretName:      "awg-secret",
					InterfacePrefix: "awg-test",
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			out := tt.in.DeepCopy()

			if tt.in == nil {
				assert.Nil(t, out)
				return
			}

			require.NotNil(t, out)
			assert.Equal(t, tt.in.Replicas, out.Replicas)
			assert.Equal(t, tt.in.Namespace, out.Namespace)
			assert.Equal(t, tt.in.Protocol, out.Protocol)

			if tt.in.Enabled != nil {
				require.NotNil(t, out.Enabled)
				assert.Equal(t, *tt.in.Enabled, *out.Enabled)

				newVal := false
				out.Enabled = &newVal
				assert.NotEqual(t, *tt.in.Enabled, *out.Enabled)
			}

			if tt.in.AWG != nil {
				require.NotNil(t, out.AWG)
				assert.Equal(t, tt.in.AWG.SecretName, out.AWG.SecretName)

				out.AWG.SecretName = testModifiedValue
				assert.NotEqual(t, tt.in.AWG.SecretName, out.AWG.SecretName)
			}
		})
	}
}

func TestGatewayClassConfigSpec_DeepCopy(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   *GatewayClassConfigSpec
	}{
		{
			name: "nil input",
			in:   nil,
		},
		{
			name: "empty struct",
			in:   &GatewayClassConfigSpec{},
		},
		{
			name: "with tunnel token ref",
			in: &GatewayClassConfigSpec{
				CloudflareCredentialsSecretRef: SecretReference{
					Name:      "cf-creds",
					Namespace: "cf-ns",
				},
				AccountID: "test-account",
				TunnelID:  "test-tunnel",
				TunnelTokenSecretRef: &SecretReference{
					Name: "tunnel-token",
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			out := tt.in.DeepCopy()

			if tt.in == nil {
				assert.Nil(t, out)
				return
			}

			require.NotNil(t, out)
			assert.Equal(t, tt.in.AccountID, out.AccountID)
			assert.Equal(t, tt.in.TunnelID, out.TunnelID)

			if tt.in.TunnelTokenSecretRef != nil {
				require.NotNil(t, out.TunnelTokenSecretRef)
				assert.Equal(t, tt.in.TunnelTokenSecretRef.Name, out.TunnelTokenSecretRef.Name)

				out.TunnelTokenSecretRef.Name = testModifiedValue
				assert.NotEqual(t, tt.in.TunnelTokenSecretRef.Name, out.TunnelTokenSecretRef.Name)
			}
		})
	}
}

func TestGatewayClassConfigStatus_DeepCopy(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   *GatewayClassConfigStatus
	}{
		{
			name: "nil input",
			in:   nil,
		},
		{
			name: "empty struct",
			in:   &GatewayClassConfigStatus{},
		},
		{
			name: "with conditions",
			in: &GatewayClassConfigStatus{
				Conditions: []metav1.Condition{
					{
						Type:    "Ready",
						Status:  metav1.ConditionTrue,
						Reason:  "Configured",
						Message: "Config is valid",
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			out := tt.in.DeepCopy()

			if tt.in == nil {
				assert.Nil(t, out)
				return
			}

			require.NotNil(t, out)
			assert.Len(t, out.Conditions, len(tt.in.Conditions))

			if len(tt.in.Conditions) > 0 {
				assert.Equal(t, tt.in.Conditions[0].Type, out.Conditions[0].Type)

				out.Conditions[0].Type = testModifiedValue
				assert.NotEqual(t, tt.in.Conditions[0].Type, out.Conditions[0].Type)
			}
		})
	}
}

func TestGatewayClassConfig_DeepCopy(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   *GatewayClassConfig
	}{
		{
			name: "nil input",
			in:   nil,
		},
		{
			name: "empty struct",
			in:   &GatewayClassConfig{},
		},
		{
			name: "full struct",
			in: &GatewayClassConfig{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-config",
				},
				Spec: GatewayClassConfigSpec{
					TunnelID: "test-tunnel",
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			out := tt.in.DeepCopy()

			if tt.in == nil {
				assert.Nil(t, out)
				return
			}

			require.NotNil(t, out)
			assert.Equal(t, tt.in.Name, out.Name)
			assert.Equal(t, tt.in.Spec.TunnelID, out.Spec.TunnelID)

			out.Name = testModifiedValue
			assert.NotEqual(t, tt.in.Name, out.Name)
		})
	}
}

func TestGatewayClassConfig_DeepCopyObject(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   *GatewayClassConfig
	}{
		{
			name: "nil input",
			in:   nil,
		},
		{
			name: "valid struct",
			in: &GatewayClassConfig{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-config",
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if tt.in == nil {
				var nilConfig *GatewayClassConfig
				obj := nilConfig.DeepCopyObject()
				assert.Nil(t, obj)
				return
			}

			obj := tt.in.DeepCopyObject()
			require.NotNil(t, obj)

			gcc, ok := obj.(*GatewayClassConfig)
			require.True(t, ok)
			assert.Equal(t, tt.in.Name, gcc.Name)
		})
	}
}

func TestGatewayClassConfigList_DeepCopy(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   *GatewayClassConfigList
	}{
		{
			name: "nil input",
			in:   nil,
		},
		{
			name: "empty list",
			in:   &GatewayClassConfigList{},
		},
		{
			name: "with items",
			in: &GatewayClassConfigList{
				Items: []GatewayClassConfig{
					{
						ObjectMeta: metav1.ObjectMeta{Name: "config-1"},
						Spec:       GatewayClassConfigSpec{TunnelID: "tunnel-1"},
					},
					{
						ObjectMeta: metav1.ObjectMeta{Name: "config-2"},
						Spec:       GatewayClassConfigSpec{TunnelID: "tunnel-2"},
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			out := tt.in.DeepCopy()

			if tt.in == nil {
				assert.Nil(t, out)
				return
			}

			require.NotNil(t, out)
			assert.Len(t, out.Items, len(tt.in.Items))

			if len(tt.in.Items) > 0 {
				assert.Equal(t, tt.in.Items[0].Name, out.Items[0].Name)

				out.Items[0].Name = testModifiedValue
				assert.NotEqual(t, tt.in.Items[0].Name, out.Items[0].Name)
			}
		})
	}
}

func TestGatewayClassConfigList_DeepCopyObject(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   *GatewayClassConfigList
	}{
		{
			name: "nil input",
			in:   nil,
		},
		{
			name: "valid list",
			in: &GatewayClassConfigList{
				Items: []GatewayClassConfig{
					{ObjectMeta: metav1.ObjectMeta{Name: "config-1"}},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if tt.in == nil {
				var nilList *GatewayClassConfigList
				obj := nilList.DeepCopyObject()
				assert.Nil(t, obj)
				return
			}

			obj := tt.in.DeepCopyObject()
			require.NotNil(t, obj)

			list, ok := obj.(*GatewayClassConfigList)
			require.True(t, ok)
			assert.Len(t, list.Items, len(tt.in.Items))
		})
	}
}
