//go:build envtest

package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/api/v1alpha1"
)

// TestGatewayClassConfig_MaxDataPlanesPerNamespaceSchema exercises the cap
// field against the real apiserver, which is the only place two of its
// properties hold: an unknown field is silently PRUNED rather than rejected (so
// a CRD missing the field would make every cap read back as unlimited), and the
// Minimum=1 bound is enforced at admission rather than by the controller.
//
// 0 is rejected rather than accepted as a second spelling of unlimited. It is
// what an operator writes for "no dedicated planes in this cluster", and a
// field that granted the opposite would fail open on a security control.
func TestGatewayClassConfig_MaxDataPlanesPerNamespaceSchema(t *testing.T) {
	t.Parallel()

	require.NotNil(t, envK8sClient, "envtest must be wired up; see suite_envtest_test.go")

	cases := []struct {
		name    string
		cap     *int32
		wantErr bool
	}{
		{name: "unset stays unset", cap: nil},
		{name: "zero rejected: unlimited is spelled by omitting the field", cap: new(int32(0)), wantErr: true},
		{name: "positive cap accepted", cap: new(int32(3))},
		{name: "negative cap rejected", cap: new(int32(-1)), wantErr: true},
	}

	for idx, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			gcc := &v1alpha1.GatewayClassConfig{
				ObjectMeta: metav1.ObjectMeta{Name: stringHashSuffix("quota-test", idx, tc.name)},
				Spec: v1alpha1.GatewayClassConfigSpec{
					TunnelID: "12345678-1234-1234-1234-123456789012",
					CloudflareCredentialsSecretRef: v1alpha1.SecretReference{
						Name: "cloudflare-credentials",
					},
					MaxDataPlanesPerNamespace: tc.cap,
				},
			}

			ctx := context.Background()

			err := envK8sClient.Create(ctx, gcc)
			if tc.wantErr {
				require.Error(t, err, "a cap below 1 must be rejected at admission")

				return
			}

			require.NoError(t, err)

			defer func() { _ = envK8sClient.Delete(ctx, gcc) }()

			var stored v1alpha1.GatewayClassConfig
			require.NoError(t, envK8sClient.Get(ctx, client.ObjectKeyFromObject(gcc), &stored))

			if tc.cap == nil {
				assert.Nil(t, stored.Spec.MaxDataPlanesPerNamespace)

				return
			}

			require.NotNil(t, stored.Spec.MaxDataPlanesPerNamespace,
				"the cap must survive a round trip; a CRD without the field prunes it and every namespace reads as unlimited")
			assert.Equal(t, *tc.cap, *stored.Spec.MaxDataPlanesPerNamespace)
		})
	}
}
