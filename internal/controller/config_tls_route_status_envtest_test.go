//go:build envtest

package controller

import (
	"context"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/cfmetrics"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
)

// TestConfigTLSPushFailure_ReachesRouteStatusAgainstAPIServer pins the whole
// path from a failed config API handshake to the route: a plane presenting a
// certificate this controller did not issue fails the push, and once the
// failure is sustained the route's ProxyConfigPushed=False condition, as the
// API server stores it, names that reason.
func TestConfigTLSPushFailure_ReachesRouteStatusAgainstAPIServer(t *testing.T) {
	ctx := context.Background()
	namespace := driftNamespace(ctx, t)
	controllerName := "example.com/config-tls-" + namespace

	gatewayClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: namespace},
		Spec:       gatewayv1.GatewayClassSpec{ControllerName: gatewayv1.GatewayController(controllerName)},
	}
	require.NoError(t, envK8sClient.Create(ctx, gatewayClass))
	t.Cleanup(func() { _ = envK8sClient.Delete(context.Background(), gatewayClass) })

	require.NoError(t, envK8sClient.Create(ctx, &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "edge", Namespace: namespace},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(namespace),
			Listeners:        []gatewayv1.Listener{{Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType}},
		},
	}))

	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: namespace},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: []gatewayv1.ParentReference{{Name: "edge"}}},
			Hostnames:       []gatewayv1.Hostname{"web.example.com"},
		},
	}
	require.NoError(t, envK8sClient.Create(ctx, route))

	// httptest's own certificate: a plane serving TLS, but not with a leaf
	// from this controller's CA.
	plane := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusOK)
	}))
	plane.Config.ErrorLog = log.New(io.Discard, "", 0)
	plane.StartTLS()
	t.Cleanup(plane.Close)

	params := syncUpdateParams{
		routeSyncer: &RouteSyncer{ClusterDomain: "cluster.local", Metrics: cfmetrics.NewNoopCollector()},
		proxySyncer: NewProxySyncer("cluster.local", "token", "", envK8sClient,
			slog.New(slog.DiscardHandler), WithConfigAPIAuthority(testAuthority(t))),
		proxyEndpoints: []string{plane.URL + "/config"},
		pushProxy:      true,
	}

	var diags []proxy.RouteDiagnostic

	for range pushFailureSurfaceThreshold {
		diags, _ = pushPartitionConfigs(ctx, slog.New(slog.DiscardHandler), &params, &SyncResult{
			Partitions: []routePartition{{Key: sharedPartitionKey, HTTPRoutes: []gatewayv1.HTTPRoute{*route}}},
		})
	}

	require.True(t, hasProxyPushDiagnostic(diags), "a sustained handshake failure must surface on the route")

	reconciler := &HTTPRouteReconciler{Client: envK8sClient, Scheme: envScheme, ControllerName: controllerName}
	// The sync records the partition serving the parent; the status writer
	// keeps only that partition's data-plane diagnostics on it.
	binding := routeBindingInfo{parentPartitions: map[int]string{0: sharedPartitionKey}}
	require.NoError(t, reconciler.updateRouteStatus(ctx, route, binding, nil, diags, nil))

	var stored gatewayv1.HTTPRoute
	require.NoError(t, envK8sClient.Get(ctx, types.NamespacedName{Name: "web", Namespace: namespace}, &stored))
	require.Len(t, stored.Status.Parents, 1)

	pushed := findCondition(stored.Status.Parents[0].Conditions, routeConditionProxyConfigPushed)
	require.NotNil(t, pushed, "the stored route status must carry ProxyConfigPushed")
	assert.Equal(t, metav1.ConditionFalse, pushed.Status)
	assert.Equal(t, routeReasonProxyConfigPushFailed, pushed.Reason)
	assert.Contains(t, pushed.Message, "not issued by this controller")
}
