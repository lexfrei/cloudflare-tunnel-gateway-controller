//go:build e2e

package e2e

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/api/v1alpha1"
)

// sharedTunnelTokenSecret is the Secret hack/conformance-setup.sh creates for
// the chart's shared proxy; the per-Gateway e2e reuses the same connector
// token (same tunnel — the controller merges same-tunnel documents and unions
// the proxy configs, so this is a supported configuration).
const sharedTunnelTokenSecret = "cloudflare-tunnel-token"

// TestPerGatewayDataPlaneEndToEnd pins the dedicated data-plane machinery
// (#479) against a real cluster and tunnel:
//
//   - a Gateway opting in via infrastructure.parametersRef gets its own proxy
//     Deployment and config Service rendered in ITS namespace,
//   - the Gateway reaches Programmed=True only once the rendered plane has
//     ready replicas (= registered tunnel connectors),
//   - a route bound to that Gateway serves real traffic through the edge,
//   - deleting the Gateway garbage-collects the rendered resources.
func TestPerGatewayDataPlaneEndToEnd(t *testing.T) {
	cfg := loadTestConfig(t)
	httpClient := tunnelClient()
	k8sClient := newK8sClient(t, cfg.KubeContext)
	ctx := context.Background()

	setupTestNamespace(t, k8sClient, cfg)
	setupEchoBackends(t, k8sClient, cfg)

	copyTunnelTokenSecret(ctx, t, k8sClient, cfg)

	gwConfig := &v1alpha1.GatewayConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "pg-config", Namespace: cfg.TestNamespace},
		Spec: v1alpha1.GatewayConfigSpec{
			TunnelTokenSecretRef: v1alpha1.LocalSecretReference{Name: "pg-tunnel-token"},
			Replicas:             new(int32(1)),
		},
	}
	applyObject(ctx, t, k8sClient, gwConfig)

	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), gwConfig) })

	gateway := buildPerGatewayGateway(cfg)
	applyObject(ctx, t, k8sClient, gateway)

	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), gateway) })

	deploymentKey := types.NamespacedName{Name: "cf-proxy-pg-gateway", Namespace: cfg.TestNamespace}
	waitForPerGatewayDeploymentReady(ctx, t, k8sClient, deploymentKey)

	waitForGatewayProgrammed(ctx, t, k8sClient, types.NamespacedName{
		Name: gateway.Name, Namespace: gateway.Namespace,
	})

	route := buildPerGatewayRoute(cfg, gateway.Name)
	createHTTPRoute(t, k8sClient, route)

	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), route) })

	waitForBackend(t, httpClient, cfg.TunnelHostname, "/pg-e2e", "echo-v2-", 120*time.Second)

	echo, resp, err := makeRequest(ctx, t, httpClient, cfg.TunnelHostname, http.MethodGet, "/pg-e2e", nil)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.True(t, strings.HasPrefix(echo.Pod, "echo-v2-"), "expected echo-v2 backend, got %s", echo.Pod)

	// Scale up: a newly-joined per-Gateway pod must receive the cached config
	// via the EndpointSlice watch and become Ready. That path relies on
	// Kubernetes mirroring the rendered Service's cf.k8s.lex.la/gateway label
	// onto its EndpointSlices (kubernetes/kubernetes#94443) to trigger
	// ResyncPartition — if the mirror were broken the second pod would sit
	// configless and never report Ready. This is the live canary for that
	// dependency (envtest cannot cover it: it runs no kube-controller-manager).
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "pg-config", Namespace: cfg.TestNamespace}, gwConfig))
	gwConfig.Spec.Replicas = new(int32(2))
	require.NoError(t, k8sClient.Update(ctx, gwConfig))

	scaleErr := wait.PollUntilContextTimeout(ctx, 3*time.Second, 5*time.Minute, true,
		func(pollCtx context.Context) (bool, error) {
			var deployment appsv1.Deployment

			getErr := k8sClient.Get(pollCtx, deploymentKey, &deployment)
			if getErr != nil {
				return false, nil //nolint:nilerr // rendering is asynchronous; retry until timeout
			}

			return deployment.Status.ReadyReplicas >= 2, nil
		},
	)
	require.NoError(t, scaleErr,
		"a scaled-up per-Gateway pod never became Ready — the EndpointSlice label-mirror resync is likely broken")

	// GC: deleting the Gateway must collect the rendered data plane.
	require.NoError(t, k8sClient.Delete(ctx, gateway))
	waitForObjectGone(ctx, t, k8sClient, deploymentKey, &appsv1.Deployment{})
	waitForObjectGone(ctx, t, k8sClient,
		types.NamespacedName{Name: "cf-proxy-pg-gateway-config", Namespace: cfg.TestNamespace}, &corev1.Service{})
}

// copyTunnelTokenSecret clones the shared connector token into the test
// namespace (per-Gateway secrets are namespace-local to their Gateway by
// design — credentials cannot cross the tenant boundary).
func copyTunnelTokenSecret(ctx context.Context, t *testing.T, k8sClient client.Client, cfg testConfig) {
	t.Helper()

	var source corev1.Secret
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: sharedTunnelTokenSecret, Namespace: cfg.Namespace}, &source),
		"the shared tunnel token secret must exist (created by hack/conformance-setup.sh)")

	clone := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "pg-tunnel-token", Namespace: cfg.TestNamespace},
		Data:       source.Data,
	}
	applyObject(ctx, t, k8sClient, clone)

	//nolint:contextcheck // cleanup runs after the test context may be done
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), clone) })
}

func buildPerGatewayGateway(cfg testConfig) *gatewayv1.Gateway {
	return &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "pg-gateway", Namespace: cfg.TestNamespace},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "cloudflare-tunnel",
			Infrastructure: &gatewayv1.GatewayInfrastructure{
				ParametersRef: &gatewayv1.LocalParametersReference{
					Group: "cf.k8s.lex.la", Kind: "GatewayConfig", Name: "pg-config",
				},
			},
			Listeners: []gatewayv1.Listener{
				{
					Name:     "https",
					Port:     443,
					Protocol: gatewayv1.HTTPSProtocolType,
					AllowedRoutes: &gatewayv1.AllowedRoutes{
						Namespaces: &gatewayv1.RouteNamespaces{
							From: new(gatewayv1.NamespacesFromSame),
						},
					},
				},
			},
		},
	}
}

func buildPerGatewayRoute(cfg testConfig, gatewayName string) *gatewayv1.HTTPRoute {
	hostname := gatewayv1.Hostname(cfg.TunnelHostname)

	return &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "pg-e2e-route", Namespace: cfg.TestNamespace},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Name: gatewayv1.ObjectName(gatewayName)},
				},
			},
			Hostnames: []gatewayv1.Hostname{hostname},
			Rules: []gatewayv1.HTTPRouteRule{
				{
					Matches: []gatewayv1.HTTPRouteMatch{{Path: pathPrefix("/pg-e2e")}},
					BackendRefs: []gatewayv1.HTTPBackendRef{
						backendRef("echo-v2", 80, nil),
					},
				},
			},
		},
	}
}

func waitForPerGatewayDeploymentReady(
	ctx context.Context,
	t *testing.T,
	k8sClient client.Client,
	key types.NamespacedName,
) {
	t.Helper()

	err := wait.PollUntilContextTimeout(ctx, 3*time.Second, 5*time.Minute, true,
		func(pollCtx context.Context) (bool, error) {
			var deployment appsv1.Deployment

			getErr := k8sClient.Get(pollCtx, key, &deployment)
			if getErr != nil {
				return false, nil //nolint:nilerr // rendering is asynchronous; retry until timeout
			}

			return deployment.Status.ReadyReplicas >= 1, nil
		},
	)
	require.NoError(t, err, "per-gateway proxy deployment never became ready")
}

func waitForGatewayProgrammed(
	ctx context.Context,
	t *testing.T,
	k8sClient client.Client,
	key types.NamespacedName,
) {
	t.Helper()

	err := wait.PollUntilContextTimeout(ctx, 2*time.Second, 2*time.Minute, true,
		func(pollCtx context.Context) (bool, error) {
			var gateway gatewayv1.Gateway

			getErr := k8sClient.Get(pollCtx, key, &gateway)
			if getErr != nil {
				return false, nil //nolint:nilerr // transient API errors while polling
			}

			for _, condition := range gateway.Status.Conditions {
				if condition.Type == string(gatewayv1.GatewayConditionProgrammed) &&
					condition.Status == metav1.ConditionTrue {
					return true, nil
				}
			}

			return false, nil
		},
	)
	require.NoError(t, err, "gateway never reached Programmed=True")
}

func waitForObjectGone(
	ctx context.Context,
	t *testing.T,
	k8sClient client.Client,
	key types.NamespacedName,
	obj client.Object,
) {
	t.Helper()

	err := wait.PollUntilContextTimeout(ctx, 2*time.Second, 2*time.Minute, true,
		func(pollCtx context.Context) (bool, error) {
			getErr := k8sClient.Get(pollCtx, key, obj)

			return apierrors.IsNotFound(getErr), nil
		},
	)
	require.NoError(t, err, "rendered %T was not garbage-collected", obj)
}
