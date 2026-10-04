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
	eventsv1 "k8s.io/api/events/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/api/v1alpha1"
)

// TestTunnelOwnershipRefusesClassTunnelClaim pins the shipped default of the
// tunnel-ownership rule against a live cluster: with allowSharedTunnels off, a
// dedicated Gateway whose connector token names the GatewayClass tunnel is
// refused, gets no data plane, and its routes are never served.
//
// The suite's install turns allowSharedTunnels on, because the per-Gateway
// tests reuse the class token. A second GatewayClass with its own
// GatewayClassConfig cannot carry the default instead: two managed classes in
// use with different parametersRef refuse every Gateway of the controller. So
// this test switches the flag off on the one GatewayClassConfig for its
// duration, which is safe only because the live e2e tests run serially. A
// killed run leaves it off until the next hack/conformance-setup.sh, which
// installs into a fresh cluster.
func TestTunnelOwnershipRefusesClassTunnelClaim(t *testing.T) {
	cfg := loadTestConfig(t)
	httpClient := tunnelClient()
	k8sClient := newK8sClient(t, cfg.KubeContext)
	ctx := context.Background()

	disallowSharedTunnels(ctx, t, k8sClient)
	setupTestNamespace(t, k8sClient, cfg)
	setupEchoBackends(t, k8sClient, cfg)
	copyTunnelTokenSecret(ctx, t, k8sClient, cfg)

	gwConfig := &v1alpha1.GatewayConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "refused-config", Namespace: cfg.TestNamespace},
		Spec: v1alpha1.GatewayConfigSpec{
			TunnelTokenSecretRef: v1alpha1.LocalSecretReference{Name: "pg-tunnel-token"},
			Replicas:             new(int32(1)),
		},
	}
	applyObject(ctx, t, k8sClient, gwConfig)

	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), gwConfig) })

	gateway := buildPerGatewayGateway(cfg)
	gateway.Name = "refused-gateway"
	gateway.Spec.Infrastructure.ParametersRef.Name = gwConfig.Name
	applyObject(ctx, t, k8sClient, gateway)

	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), gateway) })

	route := buildPerGatewayRoute(cfg, gateway.Name)
	route.Name = "refused-route"
	route.Spec.Rules[0].Matches[0].Path = pathPrefix("/refused-e2e")
	createHTTPRoute(t, k8sClient, route)

	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), route) })

	gatewayKey := types.NamespacedName{Name: gateway.Name, Namespace: gateway.Namespace}
	accepted := waitForGatewayRefused(ctx, t, k8sClient, gatewayKey)
	assert.Equal(t, string(gatewayv1.GatewayReasonInvalidParameters), accepted.Reason)

	waitForTunnelClaimRejectedEvent(ctx, t, k8sClient, gatewayKey)
	waitForRouteParentRefused(ctx, t, k8sClient, types.NamespacedName{Name: route.Name, Namespace: route.Namespace})
	waitForObjectGone(ctx, t, k8sClient,
		types.NamespacedName{Name: "cf-proxy-" + gateway.Name, Namespace: gateway.Namespace}, &appsv1.Deployment{})

	// A refused route that fell back to the shared partition would be served
	// by the shared proxy on the same tunnel. The push and the route status
	// are not ordered for an observer, so the path is watched for a while
	// rather than probed once.
	assertNeverServed(ctx, t, httpClient, cfg.TunnelHostname, "/refused-e2e", 15*time.Second)
}

func assertNeverServed(
	ctx context.Context,
	t *testing.T,
	httpClient *http.Client,
	hostname, path string,
	window time.Duration,
) {
	t.Helper()

	deadline := time.Now().Add(window)
	for time.Now().Before(deadline) {
		_, resp, err := makeRequest(ctx, t, httpClient, hostname, http.MethodGet, path, nil)
		require.NoError(t, err)
		require.Equal(t, http.StatusNotFound, resp.StatusCode, "a refused Gateway's route was served")

		time.Sleep(time.Second)
	}
}

// disallowSharedTunnels turns allowSharedTunnels off on the GatewayClassConfig
// behind the suite's GatewayClass and turns it back on at cleanup, the value
// hack/conformance-setup.sh installs.
func disallowSharedTunnels(ctx context.Context, t *testing.T, k8sClient client.Client) {
	t.Helper()

	var class gatewayv1.GatewayClass
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "cloudflare-tunnel"}, &class))
	require.NotNil(t, class.Spec.ParametersRef, "the suite's GatewayClass must reference a GatewayClassConfig")

	key := types.NamespacedName{Name: class.Spec.ParametersRef.Name}

	var classConfig v1alpha1.GatewayClassConfig
	require.NoError(t, k8sClient.Get(ctx, key, &classConfig))

	if !classConfig.Spec.AllowSharedTunnels {
		t.Logf("GatewayClassConfig %s has allowSharedTunnels off, unlike the install hack/conformance-setup.sh makes; cleanup turns it on", key.Name)
	}

	//nolint:contextcheck // cleanup runs after the test context may be done
	t.Cleanup(func() { setAllowSharedTunnels(context.Background(), t, k8sClient, key, true) })

	setAllowSharedTunnels(ctx, t, k8sClient, key, false)
}

func setAllowSharedTunnels(
	ctx context.Context,
	t *testing.T,
	k8sClient client.Client,
	key types.NamespacedName,
	allow bool,
) {
	t.Helper()

	var classConfig v1alpha1.GatewayClassConfig
	require.NoError(t, k8sClient.Get(ctx, key, &classConfig))

	original := classConfig.DeepCopy()
	classConfig.Spec.AllowSharedTunnels = allow
	require.NoError(t, k8sClient.Patch(ctx, &classConfig, client.MergeFrom(original)))
}

func waitForGatewayRefused(
	ctx context.Context,
	t *testing.T,
	k8sClient client.Client,
	key types.NamespacedName,
) metav1.Condition {
	t.Helper()

	var accepted metav1.Condition

	err := wait.PollUntilContextTimeout(ctx, 2*time.Second, 2*time.Minute, true,
		func(pollCtx context.Context) (bool, error) {
			var gateway gatewayv1.Gateway
			getErr := k8sClient.Get(pollCtx, key, &gateway)
			if getErr != nil {
				return false, nil //nolint:nilerr // transient API errors while polling
			}

			for _, condition := range gateway.Status.Conditions {
				if condition.Type == string(gatewayv1.GatewayConditionAccepted) &&
					condition.Status == metav1.ConditionFalse &&
					strings.Contains(condition.Message, "claims the GatewayClass tunnel") {
					accepted = condition

					return true, nil
				}
			}

			return false, nil
		},
	)
	require.NoError(t, err, "gateway claiming the class tunnel was never refused for it")

	return accepted
}

func waitForTunnelClaimRejectedEvent(
	ctx context.Context,
	t *testing.T,
	k8sClient client.Client,
	gatewayKey types.NamespacedName,
) {
	t.Helper()

	err := wait.PollUntilContextTimeout(ctx, 2*time.Second, time.Minute, true,
		func(pollCtx context.Context) (bool, error) {
			var events eventsv1.EventList
			listErr := k8sClient.List(pollCtx, &events, client.InNamespace(gatewayKey.Namespace))
			if listErr != nil {
				return false, nil //nolint:nilerr // transient API errors while polling
			}

			for i := range events.Items {
				event := &events.Items[i]
				if event.Reason == "TunnelClaimRejected" && event.Type == "Warning" &&
					event.Regarding.Kind == "Gateway" && event.Regarding.Name == gatewayKey.Name {
					return true, nil
				}
			}

			return false, nil
		},
	)
	require.NoError(t, err, "no TunnelClaimRejected Warning Event for the refused gateway")
}

func waitForRouteParentRefused(ctx context.Context, t *testing.T, k8sClient client.Client, key types.NamespacedName) {
	t.Helper()

	err := wait.PollUntilContextTimeout(ctx, 2*time.Second, 2*time.Minute, true,
		func(pollCtx context.Context) (bool, error) {
			var route gatewayv1.HTTPRoute
			getErr := k8sClient.Get(pollCtx, key, &route)
			if getErr != nil {
				return false, nil //nolint:nilerr // transient API errors while polling
			}

			for _, parent := range route.Status.Parents {
				for _, condition := range parent.Conditions {
					if condition.Type == string(gatewayv1.RouteConditionAccepted) &&
						condition.Status == metav1.ConditionFalse &&
						strings.Contains(condition.Message, "does not own") {
						return true, nil
					}
				}
			}

			return false, nil
		},
	)
	require.NoError(t, err, "route on the refused gateway never reported why it is not programmed")
}
