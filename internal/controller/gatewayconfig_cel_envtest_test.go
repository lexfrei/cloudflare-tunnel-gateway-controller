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
			// Replicas has Minimum=1: zero connectors is not "scaled to zero",
			// it is a Gateway with no data plane at all, which must be rejected
			// at admission rather than rendered as a 0-replica Deployment.
			name: "zero fixed replicas rejected",
			spec: v1alpha1.GatewayConfigSpec{
				TunnelTokenSecretRef: v1alpha1.LocalSecretReference{Name: "tunnel-token"},
				Replicas:             new(int32(0)),
			},
			wantErr: true,
			wantSub: "greater than or equal to 1",
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
			name: "maxReplicas below defaulted minReplicas rejected",
			spec: v1alpha1.GatewayConfigSpec{
				TunnelTokenSecretRef: v1alpha1.LocalSecretReference{Name: "tunnel-token"},
				Autoscaling: &v1alpha1.ProxyAutoscaling{
					// minReplicas unset → defaults to 2; max=1 must not pass
					// just because the default is implicit.
					MaxReplicas:          1,
					TargetInflightPerPod: 50,
				},
			},
			wantErr: true,
			wantSub: "maxReplicas",
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
			// The boundary: maxReplicas == minReplicas is the smallest valid
			// HPA range and MUST be accepted, so a regression tightening the
			// CEL `>=` to `>` is caught here, not only the reject cases above.
			name: "maxReplicas equal to minReplicas accepted",
			spec: v1alpha1.GatewayConfigSpec{
				TunnelTokenSecretRef: v1alpha1.LocalSecretReference{Name: "tunnel-token"},
				Autoscaling: &v1alpha1.ProxyAutoscaling{
					MinReplicas:          &five,
					MaxReplicas:          5,
					TargetInflightPerPod: 50,
				},
			},
		},
		{
			name:    "missing tunnel token ref rejected",
			spec:    v1alpha1.GatewayConfigSpec{},
			wantErr: true,
			wantSub: "tunnelTokenSecretRef",
		},
		{
			// Exercises the spec.image MinLength+Pattern markers: a value with
			// leading junk / whitespace is rejected at admission, not at
			// pod-pull time.
			name: "malformed image rejected",
			spec: v1alpha1.GatewayConfigSpec{
				TunnelTokenSecretRef: v1alpha1.LocalSecretReference{Name: "tunnel-token"},
				Image:                " not a valid image",
			},
			wantErr: true,
			wantSub: "spec.image",
		},
		{
			name: "valid image accepted",
			spec: v1alpha1.GatewayConfigSpec{
				TunnelTokenSecretRef: v1alpha1.LocalSecretReference{Name: "tunnel-token"},
				Image:                "ghcr.io/lexfrei/proxy:v1.2.3",
			},
		},
		// Replica counts are tenant-controlled input on a shared cluster: an
		// unbounded value is a noisy-neighbour attack (schedule 100k proxy
		// pods), defeating the isolation guarantee the CRD exists for.
		{
			name: "huge fixed replicas rejected",
			spec: v1alpha1.GatewayConfigSpec{
				TunnelTokenSecretRef: v1alpha1.LocalSecretReference{Name: "tunnel-token"},
				Replicas:             new(int32(100000)),
			},
			wantErr: true,
			wantSub: "less than or equal to 100",
		},
		{
			name: "huge autoscaling maxReplicas rejected",
			spec: v1alpha1.GatewayConfigSpec{
				TunnelTokenSecretRef: v1alpha1.LocalSecretReference{Name: "tunnel-token"},
				Autoscaling: &v1alpha1.ProxyAutoscaling{
					MaxReplicas:          100000,
					TargetInflightPerPod: 50,
				},
			},
			wantErr: true,
			wantSub: "less than or equal to 100",
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
