//go:build envtest

package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/api/v1alpha1"
)

// TestGatewayClassConfig_SchemaValidation pins the OpenAPI markers on
// GatewayClassConfigSpec that the CEL and quota envtests do not cover: the
// tunnelID pattern and the credentials Secret name's MinLength. Both are
// enforced by the apiserver from the generated CRD, so they are exercised
// through the real envtest control plane.
func TestGatewayClassConfig_SchemaValidation(t *testing.T) {
	t.Parallel()

	require.NotNil(t, envK8sClient, "envtest must be wired up; see suite_envtest_test.go")

	const validTunnelID = "12345678-1234-1234-1234-123456789012"

	const (
		tunnelIDField   = "spec.tunnelID"
		secretNameField = "spec.cloudflareCredentialsSecretRef.name"
	)

	cases := []struct {
		name       string
		tunnelID   string
		secretName string
		// wantField is the field path the rejection must name; empty means accepted.
		wantField string
	}{
		{name: "valid spec accepted", tunnelID: validTunnelID, secretName: "cf-creds"},
		{name: "uppercase tunnelID rejected", tunnelID: "12345678-1234-1234-1234-12345678901A", secretName: "cf-creds", wantField: tunnelIDField},
		{name: "tunnelID without dashes rejected", tunnelID: "12345678123412341234123456789012", secretName: "cf-creds", wantField: tunnelIDField},
		{name: "empty tunnelID rejected", tunnelID: "", secretName: "cf-creds", wantField: tunnelIDField},
		{name: "empty secret name rejected", tunnelID: validTunnelID, secretName: "", wantField: secretNameField},
	}

	for idx, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			gcc := &v1alpha1.GatewayClassConfig{
				ObjectMeta: metav1.ObjectMeta{Name: stringHashSuffix("schema-test", idx, tc.name)},
				Spec: v1alpha1.GatewayClassConfigSpec{
					TunnelID:                       tc.tunnelID,
					CloudflareCredentialsSecretRef: v1alpha1.SecretReference{Name: tc.secretName},
				},
			}

			err := envK8sClient.Create(context.Background(), gcc)
			if tc.wantField != "" {
				require.Error(t, err, "tunnelID %q with secret name %q must be rejected at admission", tc.tunnelID, tc.secretName)
				require.True(t, apierrors.IsInvalid(err), "want a schema rejection, got: %v", err)
				require.Contains(t, err.Error(), tc.wantField, "the rejection must come from the rule on %s", tc.wantField)

				return
			}

			require.NoError(t, err)

			_ = envK8sClient.Delete(context.Background(), gcc)
		})
	}
}
