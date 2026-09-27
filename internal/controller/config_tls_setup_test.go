package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const sharedTLSEndpoint = "https://release-proxy-headless.cf-system.svc.cluster.local:8081/config"

func TestSetupConfigTLS_OffCreatesNothing(t *testing.T) {
	t.Parallel()

	c := fake.NewClientBuilder().Build()

	setup, err := setupConfigTLS(t.Context(), c, &Config{}, []string{"http://proxy.cf-system.svc.cluster.local:8081/config"})
	require.NoError(t, err)
	assert.Nil(t, setup)

	var secrets corev1.SecretList
	require.NoError(t, c.List(t.Context(), &secrets))
	assert.Empty(t, secrets.Items, "with config API TLS off the controller must not create a CA")
}

func TestSetupConfigTLS_BuildsAuthorityAndLeafNames(t *testing.T) {
	t.Parallel()

	c := fake.NewClientBuilder().Build()
	cfg := &Config{
		ProxyConfigCASecretRef:  "cf-system/release-config-ca",
		ProxyConfigTLSSecretRef: "cf-system/release-proxy-config-tls",
	}

	setup, err := setupConfigTLS(t.Context(), c, cfg, []string{sharedTLSEndpoint})
	require.NoError(t, err)
	require.NotNil(t, setup)
	require.NotNil(t, setup.authority)
	assert.Equal(t, sharedLeafKey(), setup.leafKey)
	assert.Equal(t, []string{"release-proxy-headless.cf-system.svc.cluster.local"}, setup.leafNames)

	var ca corev1.Secret
	require.NoError(t, c.Get(t.Context(), configCAKey(), &ca))
}

// TestSetupConfigTLS_MisconfigurationFailsLoud pins each broken combination to
// a startup error, because every one of them would stop all config pushes.
func TestSetupConfigTLS_MisconfigurationFailsLoud(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		cfg       Config
		endpoints []string
	}{
		{
			name:      "leaf without CA",
			cfg:       Config{ProxyConfigTLSSecretRef: "cf-system/leaf"},
			endpoints: []string{sharedTLSEndpoint},
		},
		{
			name:      "CA without leaf",
			cfg:       Config{ProxyConfigCASecretRef: "cf-system/ca"},
			endpoints: []string{sharedTLSEndpoint},
		},
		{
			name:      "malformed CA ref",
			cfg:       Config{ProxyConfigCASecretRef: "ca", ProxyConfigTLSSecretRef: "cf-system/leaf"},
			endpoints: []string{sharedTLSEndpoint},
		},
		{
			name:      "plaintext endpoint",
			cfg:       Config{ProxyConfigCASecretRef: "cf-system/ca", ProxyConfigTLSSecretRef: "cf-system/leaf"},
			endpoints: []string{"http://proxy.cf-system.svc.cluster.local:8081/config"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := setupConfigTLS(t.Context(), fake.NewClientBuilder().Build(), &tt.cfg, tt.endpoints)
			require.Error(t, err)
		})
	}
}
