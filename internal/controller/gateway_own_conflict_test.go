package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/api/v1alpha1"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/cfmetrics"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/config"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/routebinding"
)

// ownConflictListeners returns two Gateway listeners that conflict on
// a.example.com plus a distinct one on b.example.com, all admitting routes
// from every namespace.
func ownConflictListeners() []gatewayv1.Listener {
	fromAll := &gatewayv1.AllowedRoutes{Namespaces: &gatewayv1.RouteNamespaces{From: namespacesFromAllPtr()}}
	hostA := gatewayv1.Hostname("a.example.com")
	hostB := gatewayv1.Hostname("b.example.com")

	return []gatewayv1.Listener{
		{Name: "c1", Port: 80, Protocol: gatewayv1.HTTPProtocolType, Hostname: &hostA, AllowedRoutes: fromAll},
		{Name: "c2", Port: 80, Protocol: gatewayv1.HTTPProtocolType, Hostname: &hostA, AllowedRoutes: fromAll},
		{Name: "ok", Port: 80, Protocol: gatewayv1.HTTPProtocolType, Hostname: &hostB, AllowedRoutes: fromAll},
	}
}

// TestResolveRouteParentBinding_GatewayOwnConflictHasNoWinner pins that a
// route does not bind to either listener of a conflicting pair on the Gateway
// itself: the spec forbids picking one of them as the winner. A route for the
// distinct listener still binds.
func TestResolveRouteParentBinding_GatewayOwnConflictHasNoWinner(t *testing.T) {
	t.Parallel()

	c1 := gatewayv1.SectionName("c1")

	tests := []struct {
		name     string
		hostname gatewayv1.Hostname
		section  *gatewayv1.SectionName
		accepted bool
	}{
		{name: "conflicting hostname", hostname: "a.example.com"},
		{name: "pinned to the first of the pair", hostname: "a.example.com", section: &c1},
		{name: "distinct hostname", hostname: "b.example.com", accepted: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			gc := managedGatewayClass()
			gw := &gatewayv1.Gateway{
				ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "infra"},
				Spec:       gatewayv1.GatewaySpec{GatewayClassName: gatewayv1.ObjectName(gc.Name), Listeners: ownConflictListeners()},
			}

			cli := buildGatewayFakeClient(t, gc, gw)

			gwKind := gatewayv1.Kind(kindGateway)
			gwNS := gatewayv1.Namespace("infra")
			ref := gatewayv1.ParentReference{Kind: &gwKind, Name: "gw", Namespace: &gwNS, SectionName: tt.section}
			routeInfo := &routebinding.RouteInfo{
				Name: "r", Namespace: "team-a", Kind: routebinding.KindHTTPRoute,
				Hostnames: []gatewayv1.Hostname{tt.hostname}, SectionName: tt.section,
			}

			binding, err := resolveRouteParentBinding(context.Background(), cli, routebinding.NewValidator(cli),
				testListenerSetController, ref, "team-a", routeInfo, nil)
			require.NoError(t, err)
			assert.Equal(t, tt.accepted, binding.Result.Accepted)

			if tt.accepted {
				assert.Equal(t, []gatewayv1.SectionName{"ok"}, binding.Result.MatchedListeners)
			}
		})
	}
}

// reconcileOwnListenersGateway reconciles a managed Gateway with the given
// listeners and returns it as stored.
func reconcileOwnListenersGateway(t *testing.T, listeners []gatewayv1.Listener) *gatewayv1.Gateway {
	t.Helper()

	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "test-gateway", Namespace: "default"},
		Spec:       gatewayv1.GatewaySpec{GatewayClassName: "cloudflare-tunnel", Listeners: listeners},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "cf-credentials", Namespace: "default"},
		Data:       map[string][]byte{"api-token": []byte("test-token")},
	}
	classConfig := &v1alpha1.GatewayClassConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "test-config"},
		Spec: v1alpha1.GatewayClassConfigSpec{
			CloudflareCredentialsSecretRef: v1alpha1.SecretReference{Name: "cf-credentials", Namespace: "default"},
			TunnelID:                       "12345678-1234-1234-1234-123456789abc",
		},
	}
	gatewayClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "cloudflare-tunnel"},
		Spec: gatewayv1.GatewayClassSpec{
			ControllerName: "test-controller",
			ParametersRef: &gatewayv1.ParametersReference{
				Group: config.ParametersRefGroup, Kind: config.ParametersRefKind, Name: "test-config",
			},
		},
	}

	fakeClient := setupGatewayFakeClient(gateway, secret, classConfig, gatewayClass)
	reconciler := &GatewayReconciler{
		Client:         fakeClient,
		Scheme:         fakeClient.Scheme(),
		ControllerName: "test-controller",
		ConfigResolver: config.NewResolver(fakeClient, "default", cfmetrics.NewNoopCollector(), verifiedClaims()),
	}

	key := types.NamespacedName{Name: "test-gateway", Namespace: "default"}

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
	require.NoError(t, err)

	var updated gatewayv1.Gateway
	require.NoError(t, fakeClient.Get(context.Background(), key, &updated))

	return &updated
}

// TestGatewayReconciler_OwnConflictKeepsGatewayAccepted pins the Gateway verdict
// for conflicting own listeners next to a distinct one: Accepted=True with
// ListenersNotValid, as for any other invalid listener, and both listeners of
// the conflicting pair carry Conflicted=True.
func TestGatewayReconciler_OwnConflictKeepsGatewayAccepted(t *testing.T) {
	t.Parallel()

	updated := reconcileOwnListenersGateway(t, ownConflictListeners())

	accepted := meta.FindStatusCondition(updated.Status.Conditions, string(gatewayv1.GatewayConditionAccepted))
	require.NotNil(t, accepted)
	assert.Equal(t, metav1.ConditionTrue, accepted.Status, "the distinct listener still serves")
	assert.Equal(t, string(gatewayv1.GatewayReasonListenersNotValid), accepted.Reason)
	assert.Contains(t, accepted.Message, "c1")
	assert.Contains(t, accepted.Message, "c2")

	for _, name := range []gatewayv1.SectionName{"c1", "c2"} {
		var status *gatewayv1.ListenerStatus

		for i := range updated.Status.Listeners {
			if updated.Status.Listeners[i].Name == name {
				status = &updated.Status.Listeners[i]
			}
		}

		require.NotNil(t, status, "listener %s", name)

		conflicted := meta.FindStatusCondition(status.Conditions, string(gatewayv1.ListenerConditionConflicted))
		require.NotNil(t, conflicted, "listener %s", name)
		assert.Equal(t, metav1.ConditionTrue, conflicted.Status, "listener %s", name)
	}
}

// TestGatewayReconciler_UnservedListenerLeavesHTTPUsable pins that a TCP
// listener sharing a port with an HTTP listener does not take the HTTP one down
// with it: the TCP listener is refused as UnsupportedProtocol, the HTTP listener
// stays Accepted, the Gateway is Accepted=True/ListenersNotValid, and a route
// still binds to the HTTP listener.
func TestGatewayReconciler_UnservedListenerLeavesHTTPUsable(t *testing.T) {
	t.Parallel()

	fromAll := &gatewayv1.AllowedRoutes{Namespaces: &gatewayv1.RouteNamespaces{From: namespacesFromAllPtr()}}
	listeners := []gatewayv1.Listener{
		{Name: "raw", Port: 80, Protocol: gatewayv1.TCPProtocolType, AllowedRoutes: fromAll},
		{Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType, AllowedRoutes: fromAll},
	}

	updated := reconcileOwnListenersGateway(t, listeners)

	accepted := meta.FindStatusCondition(updated.Status.Conditions, string(gatewayv1.GatewayConditionAccepted))
	require.NotNil(t, accepted)
	assert.Equal(t, metav1.ConditionTrue, accepted.Status, "the HTTP listener still serves")
	assert.Equal(t, string(gatewayv1.GatewayReasonListenersNotValid), accepted.Reason)

	statuses := map[gatewayv1.SectionName][]metav1.Condition{}
	for i := range updated.Status.Listeners {
		statuses[updated.Status.Listeners[i].Name] = updated.Status.Listeners[i].Conditions
	}

	httpAccepted := meta.FindStatusCondition(statuses["http"], string(gatewayv1.ListenerConditionAccepted))
	require.NotNil(t, httpAccepted)
	assert.Equal(t, metav1.ConditionTrue, httpAccepted.Status)
	assert.False(t, meta.IsStatusConditionTrue(statuses["http"], string(gatewayv1.ListenerConditionConflicted)))

	rawAccepted := meta.FindStatusCondition(statuses["raw"], string(gatewayv1.ListenerConditionAccepted))
	require.NotNil(t, rawAccepted)
	assert.Equal(t, metav1.ConditionFalse, rawAccepted.Status)
	assert.Equal(t, string(gatewayv1.ListenerReasonUnsupportedProtocol), rawAccepted.Reason)

	gc := managedGatewayClass()
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "infra"},
		Spec:       gatewayv1.GatewaySpec{GatewayClassName: gatewayv1.ObjectName(gc.Name), Listeners: listeners},
	}
	cli := buildGatewayFakeClient(t, gc, gw)

	gwKind := gatewayv1.Kind(kindGateway)
	gwNS := gatewayv1.Namespace("infra")
	ref := gatewayv1.ParentReference{Kind: &gwKind, Name: "gw", Namespace: &gwNS}
	routeInfo := &routebinding.RouteInfo{Name: "r", Namespace: "team-a", Kind: routebinding.KindHTTPRoute}

	binding, err := resolveRouteParentBinding(context.Background(), cli, routebinding.NewValidator(cli),
		testListenerSetController, ref, "team-a", routeInfo, nil)
	require.NoError(t, err)
	assert.True(t, binding.Result.Accepted, "a route binds to the HTTP listener")
	assert.Equal(t, []gatewayv1.SectionName{"http"}, binding.Result.MatchedListeners)
}
