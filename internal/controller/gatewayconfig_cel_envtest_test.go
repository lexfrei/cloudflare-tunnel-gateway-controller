//go:build envtest

package controller

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/api/v1alpha1"
)

// TestGatewayConfig_CELValidation pins the CRD-level CEL rules on the
// per-Gateway data-plane config: replicas and autoscaling are mutually
// exclusive (the HPA owns the replica count), and maxReplicas must cover
// minReplicas. CEL attaches to the CRD schema and is evaluated by the
// kube-apiserver, so this runs through the real envtest control plane.
func TestGatewayConfig_CELValidation(t *testing.T) {
	t.Parallel()

	require.NotNil(t, envK8sClient, "envtest must be wired up; see suite_envtest_test.go")

	two := int32(2)
	five := int32(5)

	cases := []struct {
		name    string
		spec    v1alpha1.GatewayConfigSpec
		wantErr bool
		wantSub string
	}{
		{
			name: "token ref alone accepted",
			spec: v1alpha1.GatewayConfigSpec{
				TunnelTokenSecretRef: v1alpha1.LocalSecretReference{Name: "tunnel-token"},
			},
		},
		{
			name: "fixed replicas accepted",
			spec: v1alpha1.GatewayConfigSpec{
				TunnelTokenSecretRef: v1alpha1.LocalSecretReference{Name: "tunnel-token"},
				Replicas:             &two,
			},
		},
		{
			name: "autoscaling accepted",
			spec: v1alpha1.GatewayConfigSpec{
				TunnelTokenSecretRef: v1alpha1.LocalSecretReference{Name: "tunnel-token"},
				Autoscaling: &v1alpha1.ProxyAutoscaling{
					MaxReplicas:          5,
					TargetInflightPerPod: 50,
				},
			},
		},
		{
			name: "replicas plus autoscaling rejected",
			spec: v1alpha1.GatewayConfigSpec{
				TunnelTokenSecretRef: v1alpha1.LocalSecretReference{Name: "tunnel-token"},
				Replicas:             &two,
				Autoscaling: &v1alpha1.ProxyAutoscaling{
					MaxReplicas:          5,
					TargetInflightPerPod: 50,
				},
			},
			wantErr: true,
			wantSub: "mutually exclusive",
		},
		{
			name: "maxReplicas below minReplicas rejected",
			spec: v1alpha1.GatewayConfigSpec{
				TunnelTokenSecretRef: v1alpha1.LocalSecretReference{Name: "tunnel-token"},
				Autoscaling: &v1alpha1.ProxyAutoscaling{
					MinReplicas:          &five,
					MaxReplicas:          2,
					TargetInflightPerPod: 50,
				},
			},
			wantErr: true,
			wantSub: "maxReplicas must be >= minReplicas",
		},
		{
			name:    "missing tunnel token ref rejected",
			spec:    v1alpha1.GatewayConfigSpec{},
			wantErr: true,
			wantSub: "tunnelTokenSecretRef",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			obj := &v1alpha1.GatewayConfig{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "cel-gwconfig-" + strings.ReplaceAll(strings.ToLower(tc.name), " ", "-"),
					Namespace: "default",
				},
				Spec: tc.spec,
			}

			err := envK8sClient.Create(context.Background(), obj)

			if tc.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantSub)

				return
			}

			require.NoError(t, err)
			require.NoError(t, envK8sClient.Delete(context.Background(), obj))
		})
	}
}
