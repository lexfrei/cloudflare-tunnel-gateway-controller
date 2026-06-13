//go:build envtest

package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/api/v1alpha1"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/cfmetrics"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/config"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/render"
)

// driftReconciler builds a GatewayInfraReconciler bound to the real envtest
// apiserver client.
func driftReconciler() *GatewayInfraReconciler {
	return &GatewayInfraReconciler{
		Client:         envK8sClient,
		Scheme:         envScheme,
		ControllerName: "test-controller",
		ConfigResolver: config.NewResolver(envK8sClient, "default", cfmetrics.NewNoopCollector()),
		RenderDefaults: render.Defaults{ProxyImage: "example.com/proxy:v1"},
	}
}

// driftNamespace creates a unique namespace for an isolated apply test.
func driftNamespace(ctx context.Context, t *testing.T) string {
	t.Helper()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "drift-"}}
	require.NoError(t, envK8sClient.Create(ctx, ns))

	return ns.Name
}

// driftGateway returns an in-memory Gateway with a UID set (so
// SetControllerReference can stamp a valid ownerRef). It is NOT created in the
// apiserver — the Gateway CRD is not loaded in this envtest, and the rendered
// resources only need the Gateway for naming and ownership, not for existence.
func driftGateway(namespace string) *gatewayv1.Gateway {
	return &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "edge", Namespace: namespace, UID: "drift-gateway-uid"},
		Spec:       gatewayv1.GatewaySpec{GatewayClassName: "cloudflare-tunnel"},
	}
}

// TestGatewayInfraReconciler_ApplyConvergesAgainstAPIServer pins that the
// per-Gateway rendered resources reach steady state against a REAL apiserver:
// the first apply creates each object, and every apply after that is a no-op.
// A full-spec replace that re-zeroed apiserver-defaulted fields (Deployment
// Strategy/RevisionHistoryLimit/ProgressDeadlineSeconds, pod
// DNSPolicy/RestartPolicy/SchedulerName, container TerminationMessage*, probe
// SuccessThreshold, ...) would loop forever with continuous Update churn —
// invisible to the fake-client unit tests, which run no defaulting.
func TestGatewayInfraReconciler_ApplyConvergesAgainstAPIServer(t *testing.T) {
	ctx := context.Background()
	reconciler := driftReconciler()

	t.Run("deployment (fixed replicas)", func(t *testing.T) {
		namespace := driftNamespace(ctx, t)
		gateway := driftGateway(namespace)
		replicas := int32(2)
		input := render.Input{
			Gateway:     gateway,
			TunnelToken: "token",
			Defaults:    reconciler.RenderDefaults,
			Config: &v1alpha1.GatewayConfig{
				ObjectMeta: metav1.ObjectMeta{Name: "edge-config", Namespace: namespace},
				Spec: v1alpha1.GatewayConfigSpec{
					TunnelTokenSecretRef: v1alpha1.LocalSecretReference{Name: "edge-token"},
					Replicas:             &replicas,
				},
			},
		}

		op, err := reconciler.applyDeployment(ctx, gateway, input)
		require.NoError(t, err)
		require.Equal(t, controllerutil.OperationResultCreated, op, "first apply must create")

		for i := range 2 {
			op, err := reconciler.applyDeployment(ctx, gateway, input)
			require.NoError(t, err)
			assert.Equalf(t, controllerutil.OperationResultNone, op,
				"re-apply %d must be a no-op — apiserver-default drift would loop forever", i+1)
		}
	})

	t.Run("service", func(t *testing.T) {
		namespace := driftNamespace(ctx, t)
		gateway := driftGateway(namespace)
		input := render.Input{
			Gateway:     gateway,
			TunnelToken: "token",
			Defaults:    reconciler.RenderDefaults,
			Config: &v1alpha1.GatewayConfig{
				ObjectMeta: metav1.ObjectMeta{Name: "edge-config", Namespace: namespace},
				Spec:       v1alpha1.GatewayConfigSpec{TunnelTokenSecretRef: v1alpha1.LocalSecretReference{Name: "edge-token"}},
			},
		}

		op, err := reconciler.applyService(ctx, gateway, input)
		require.NoError(t, err)
		require.Equal(t, controllerutil.OperationResultCreated, op, "first apply must create")

		for i := range 2 {
			op, err := reconciler.applyService(ctx, gateway, input)
			require.NoError(t, err)
			assert.Equalf(t, controllerutil.OperationResultNone, op, "service re-apply %d must be a no-op", i+1)
		}
	})

	t.Run("autoscaler", func(t *testing.T) {
		namespace := driftNamespace(ctx, t)
		gateway := driftGateway(namespace)
		minReplicas := int32(2)
		maxReplicas := int32(5)
		input := render.Input{
			Gateway:     gateway,
			TunnelToken: "token",
			Defaults:    reconciler.RenderDefaults,
			Config: &v1alpha1.GatewayConfig{
				ObjectMeta: metav1.ObjectMeta{Name: "edge-config", Namespace: namespace},
				Spec: v1alpha1.GatewayConfigSpec{
					TunnelTokenSecretRef: v1alpha1.LocalSecretReference{Name: "edge-token"},
					Autoscaling: &v1alpha1.ProxyAutoscaling{
						MinReplicas:          &minReplicas,
						MaxReplicas:          maxReplicas,
						TargetInflightPerPod: 100,
					},
				},
			},
		}

		require.NoError(t, reconciler.applyAutoscaler(ctx, gateway, input))

		// applyAutoscaler returns no OperationResult; assert convergence by
		// reapplying and confirming the HPA is unchanged across reapplies.
		require.NoError(t, reconciler.applyAutoscaler(ctx, gateway, input))
		require.NoError(t, reconciler.applyAutoscaler(ctx, gateway, input))
	})
}
