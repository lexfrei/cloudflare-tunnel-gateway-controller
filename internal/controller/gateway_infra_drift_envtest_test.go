//go:build envtest

package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
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
		Client:              envK8sClient,
		Scheme:              envScheme,
		ControllerName:      "test-controller",
		ConfigResolver:      config.NewResolver(envK8sClient, "default", cfmetrics.NewNoopCollector()),
		RenderDefaults:      render.Defaults{ProxyImage: "example.com/proxy:v1"},
		ControllerNamespace: "cf-system",
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
		input := &render.Input{
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

	t.Run("auth-token rotation rolls the deployment", func(t *testing.T) {
		namespace := driftNamespace(ctx, t)
		gateway := driftGateway(namespace)
		base := &render.Input{
			Gateway:     gateway,
			TunnelToken: "token",
			AuthToken:   "auth-token-v1",
			Defaults:    reconciler.RenderDefaults,
			Config: &v1alpha1.GatewayConfig{
				ObjectMeta: metav1.ObjectMeta{Name: "edge-config", Namespace: namespace},
				Spec:       v1alpha1.GatewayConfigSpec{TunnelTokenSecretRef: v1alpha1.LocalSecretReference{Name: "edge-token"}},
			},
		}

		op, err := reconciler.applyDeployment(ctx, gateway, base)
		require.NoError(t, err)
		require.Equal(t, controllerutil.OperationResultCreated, op, "first apply must create")

		// Re-applying the SAME token must converge: the new auth-token-hash
		// annotation must not introduce spurious drift.
		op, err = reconciler.applyDeployment(ctx, gateway, base)
		require.NoError(t, err)
		require.Equal(t, controllerutil.OperationResultNone, op, "a stable auth token must not roll the pods")

		// Rotating the auth token must change the pod template so the Deployment
		// rolls and the proxy re-reads PROXY_AUTH_TOKEN — without this the
		// running pod keeps the old token and the controller's pushes 401.
		rotated := *base
		rotated.AuthToken = "auth-token-v2"

		op, err = reconciler.applyDeployment(ctx, gateway, &rotated)
		require.NoError(t, err)
		assert.Equal(t, controllerutil.OperationResultUpdated, op, "a rotated auth token must roll the deployment")

		var deployment appsv1.Deployment
		require.NoError(t, reconciler.Get(ctx, client.ObjectKeyFromObject(render.ProxyDeployment(&rotated)), &deployment))
		assert.NotEmpty(t, deployment.Spec.Template.Annotations["cf.k8s.lex.la/auth-token-hash"],
			"the rolled pod template must carry the auth-token hash")
	})

	t.Run("deployment (autoscaling mode, nil replicas)", func(t *testing.T) {
		namespace := driftNamespace(ctx, t)
		gateway := driftGateway(namespace)
		minReplicas := int32(2)
		input := &render.Input{
			Gateway:     gateway,
			TunnelToken: "token",
			Defaults:    reconciler.RenderDefaults,
			Config: &v1alpha1.GatewayConfig{
				ObjectMeta: metav1.ObjectMeta{Name: "edge-config", Namespace: namespace},
				Spec: v1alpha1.GatewayConfigSpec{
					TunnelTokenSecretRef: v1alpha1.LocalSecretReference{Name: "edge-token"},
					Autoscaling:          &v1alpha1.ProxyAutoscaling{MinReplicas: &minReplicas, MaxReplicas: 5, TargetInflightPerPod: 100},
				},
			},
		}

		op, err := reconciler.applyDeployment(ctx, gateway, input)
		require.NoError(t, err)
		require.Equal(t, controllerutil.OperationResultCreated, op, "first apply must create")

		// Render leaves replicas nil so the HPA owns the count. On CREATE the
		// apiserver defaults nil to 1 (NOT the configured min) — a brief
		// startup window at 1 replica until the HPA's first reconcile lifts it
		// to min. The applies must still converge: re-apply preserves the
		// (apiserver/HPA-owned) value, so it is a no-op, never a hot-loop.
		for i := range 2 {
			op, err := reconciler.applyDeployment(ctx, gateway, input)
			require.NoError(t, err)
			assert.Equalf(t, controllerutil.OperationResultNone, op,
				"autoscaling-mode re-apply %d must be a no-op (HPA-owned replicas preserved)", i+1)
		}

		var deployment appsv1.Deployment
		require.NoError(t, reconciler.Get(ctx, client.ObjectKeyFromObject(render.ProxyDeployment(input)), &deployment))
		require.NotNil(t, deployment.Spec.Replicas)
		assert.Equal(t, int32(1), *deployment.Spec.Replicas,
			"autoscaling mode starts at the apiserver default of 1 until the HPA scales to min")
	})

	t.Run("service", func(t *testing.T) {
		namespace := driftNamespace(ctx, t)
		gateway := driftGateway(namespace)
		input := &render.Input{
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

	t.Run("networkpolicy", func(t *testing.T) {
		namespace := driftNamespace(ctx, t)
		gateway := driftGateway(namespace)

		op, err := reconciler.applyNetworkPolicy(ctx, gateway)
		require.NoError(t, err)
		require.Equal(t, controllerutil.OperationResultCreated, op, "first apply must create")

		for i := range 2 {
			op, err := reconciler.applyNetworkPolicy(ctx, gateway)
			require.NoError(t, err)
			assert.Equalf(t, controllerutil.OperationResultNone, op,
				"networkpolicy re-apply %d must be a no-op (PolicyTypes rendered explicitly so the apiserver never infers and drifts it)", i+1)
		}
	})

	t.Run("autoscaler", func(t *testing.T) {
		namespace := driftNamespace(ctx, t)
		gateway := driftGateway(namespace)
		minReplicas := int32(2)
		maxReplicas := int32(5)
		input := &render.Input{
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

		op, err := reconciler.applyAutoscaler(ctx, gateway, input)
		require.NoError(t, err)
		require.Equal(t, controllerutil.OperationResultCreated, op, "first apply must create")

		for i := range 2 {
			op, err := reconciler.applyAutoscaler(ctx, gateway, input)
			require.NoError(t, err)
			assert.Equalf(t, controllerutil.OperationResultNone, op, "autoscaler re-apply %d must be a no-op", i+1)
		}
	})
}
