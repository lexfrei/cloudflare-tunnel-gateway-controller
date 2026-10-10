//go:build envtest

package controller

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/api/v1alpha1"
)

// The default GatewayConfig name is looked up as an object name, so anything
// that cannot name a Kubernetes object is refused at admission instead of
// refusing every Gateway of the class later.
func TestGatewayClassConfig_DefaultGatewayConfigNameSchema(t *testing.T) {
	t.Parallel()

	require.NotNil(t, envK8sClient, "envtest must be wired up; see suite_envtest_test.go")

	cases := []struct {
		name        string
		defaultName string
		wantErr     bool
	}{
		{name: "dns subdomain accepted", defaultName: "tenant.plane-1"},
		{name: "uppercase rejected", defaultName: "Tenant", wantErr: true},
		{name: "slash rejected", defaultName: "ns/name", wantErr: true},
		{name: "leading dash rejected", defaultName: "-plane", wantErr: true},
		{name: "longer than an object name rejected", defaultName: strings.Repeat("a", 254), wantErr: true},
	}

	for idx, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			gcc := &v1alpha1.GatewayClassConfig{
				ObjectMeta: metav1.ObjectMeta{Name: stringHashSuffix("default-plane", idx, tc.name)},
				Spec: v1alpha1.GatewayClassConfigSpec{
					TunnelID:                       "12345678-1234-1234-1234-123456789012",
					CloudflareCredentialsSecretRef: v1alpha1.SecretReference{Name: "cloudflare-credentials"},
					PerGatewayDataPlanes: &v1alpha1.PerGatewayDataPlanes{
						DefaultGatewayConfigName: tc.defaultName,
					},
				},
			}

			ctx := context.Background()

			err := envK8sClient.Create(ctx, gcc)
			if tc.wantErr {
				require.Error(t, err, "%q must be rejected at admission", tc.defaultName)

				return
			}

			require.NoError(t, err)

			defer func() { _ = envK8sClient.Delete(ctx, gcc) }()

			var stored v1alpha1.GatewayClassConfig
			require.NoError(t, envK8sClient.Get(ctx, client.ObjectKeyFromObject(gcc), &stored))
			assert.Equal(t, tc.defaultName, stored.Spec.DefaultGatewayConfigName(),
				"a CRD without the field prunes it, and the class silently keeps the shared plane")
		})
	}
}
