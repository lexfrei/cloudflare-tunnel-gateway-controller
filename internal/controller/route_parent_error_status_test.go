package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/api/v1alpha1"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/cfmetrics"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/config"
)

// TestRouteStatus_ErroredParentIsNotAccepted pins the status of a parent whose
// ListenerSet cannot be read, next to a Gateway that admits the route. The
// route stays accepted through the first parent, while the second is Pending
// and not reported Accepted=True.
func TestRouteStatus_ErroredParentIsNotAccepted(t *testing.T) {
	t.Parallel()

	syncer := unreadableParentSyncer(t, failListenerSetReads, []gatewayv1.ParentReference{
		{Name: "healthy"},
		{Name: "extra", Kind: new(gatewayv1.Kind(kindListenerSet))},
	})

	result, err := syncer.getRelevantHTTPRoutes(context.Background(), nil)
	require.NoError(t, err)
	require.Len(t, result.accepted, 1, "the healthy parent still admits the route")

	binding := result.bindings["default/r"]

	accepted := buildAcceptedCondition(1, metav1.Now(), binding, 1, nil, nil)
	assert.Equal(t, metav1.ConditionFalse, accepted.Status,
		"a parent whose binding could not be evaluated must not be reported Accepted=True")
	assert.Equal(t, string(gatewayv1.RouteReasonPending), accepted.Reason)
	assert.True(t, binding.unevaluated)

	healthyAccepted := buildAcceptedCondition(1, metav1.Now(), binding, 0, nil, nil)
	assert.Equal(t, metav1.ConditionTrue, healthyAccepted.Status)
}

// TestRouteStatus_SoleErroredParentStillGetsStatus pins the route whose only
// parent cannot be read: it is listed among the rejected routes, so its status
// is written, and that parent carries the Pending result.
func TestRouteStatus_SoleErroredParentStillGetsStatus(t *testing.T) {
	t.Parallel()

	syncer := unreadableParentSyncer(t, failListenerSetReads, []gatewayv1.ParentReference{
		{Name: "extra", Kind: new(gatewayv1.Kind(kindListenerSet))},
	})

	result, err := syncer.getRelevantHTTPRoutes(context.Background(), nil)
	require.NoError(t, err)

	assert.Empty(t, result.accepted)
	require.Len(t, result.rejected, 1, "the route must reach the status writer")

	bindingResult, recorded := result.bindings["default/r"].bindingResults[0]
	require.True(t, recorded)
	assert.False(t, bindingResult.Accepted)
	assert.Equal(t, gatewayv1.RouteReasonPending, bindingResult.Reason)
}

// TestRouteStatus_UnparseableAllowedListenersRefuses pins a ListenerSet parent
// whose Gateway carries an unparseable allowedListeners selector. The parse
// error is decided, so the parent is refused like any ListenerSet the Gateway
// does not allow: not Pending, not marked for a retry, and without the
// Gateway's selector in the route's status.
func TestRouteStatus_UnparseableAllowedListenersRefuses(t *testing.T) {
	t.Parallel()

	syncer := erroredParentSyncer(t, []gatewayv1.ParentReference{
		{Name: "healthy"},
		{Name: "extra", Kind: new(gatewayv1.Kind(kindListenerSet))},
	})

	result, err := syncer.getRelevantHTTPRoutes(context.Background(), nil)
	require.NoError(t, err)
	require.Len(t, result.accepted, 1, "the healthy parent still admits the route")

	binding := result.bindings["default/r"]

	bindingResult, recorded := binding.bindingResults[1]
	require.True(t, recorded)
	assert.False(t, bindingResult.Accepted)
	assert.Equal(t, gatewayv1.RouteReasonNoMatchingParent, bindingResult.Reason)
	assert.NotContains(t, bindingResult.Message, "BogusOperator",
		"the parent Gateway's selector must not reach the status of a route in another namespace")
	assert.False(t, binding.unevaluated, "a parse error does not recover on retry")
}

// erroredParentSyncer serves a Gateway that admits every route, a ListenerSet
// whose parent Gateway carries an unparseable allowedListeners selector, and
// one route with the given parentRefs.
func erroredParentSyncer(t *testing.T, parentRefs []gatewayv1.ParentReference) *RouteSyncer {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, gatewayv1.Install(scheme))
	require.NoError(t, v1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))

	fromAll := gatewayv1.NamespacesFromAll
	fromSelector := gatewayv1.NamespacesFromSelector

	allowAll := &gatewayv1.AllowedRoutes{Namespaces: &gatewayv1.RouteNamespaces{From: &fromAll}}
	listener := gatewayv1.Listener{Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType, AllowedRoutes: allowAll}

	gatewayClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "cloudflare-tunnel"},
		Spec:       gatewayv1.GatewayClassSpec{ControllerName: "cloudflare-tunnel"},
	}
	healthy := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "healthy", Namespace: "default"},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "cloudflare-tunnel",
			Listeners:        []gatewayv1.Listener{listener},
		},
	}
	broken := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "broken", Namespace: "default"},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "cloudflare-tunnel",
			Listeners:        []gatewayv1.Listener{listener},
			AllowedListeners: &gatewayv1.AllowedListeners{
				Namespaces: &gatewayv1.ListenerNamespaces{
					From: &fromSelector,
					Selector: &metav1.LabelSelector{
						MatchExpressions: []metav1.LabelSelectorRequirement{
							{Key: "team", Operator: "BogusOperator", Values: []string{"x"}},
						},
					},
				},
			},
		},
	}
	listenerSet := &gatewayv1.ListenerSet{
		ObjectMeta: metav1.ObjectMeta{Name: "extra", Namespace: "default"},
		Spec: gatewayv1.ListenerSetSpec{
			ParentRef: gatewayv1.ParentGatewayReference{Name: "broken"},
			Listeners: []gatewayv1.ListenerEntry{
				{Name: "extra", Port: 80, Protocol: gatewayv1.HTTPProtocolType, AllowedRoutes: allowAll},
			},
		},
	}

	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "default"},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: parentRefs},
		},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(gatewayClass, healthy, broken, listenerSet, route).Build()

	return NewRouteSyncer(fakeClient, scheme, "cluster.local", "cloudflare-tunnel",
		config.NewResolver(fakeClient, "default", cfmetrics.NewNoopCollector(), verifiedClaims()),
		cfmetrics.NewNoopCollector(), nil)
}
