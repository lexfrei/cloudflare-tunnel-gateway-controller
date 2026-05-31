package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

const (
	testListenerSetController = "cloudflare.com/test"
)

func TestListenerSetReconciler_NotFound_NoError(t *testing.T) {
	t.Parallel()

	scheme := newListenerSetScheme(t)
	cli := fake.NewClientBuilder().WithScheme(scheme).Build()

	r := &ListenerSetReconciler{
		Client:         cli,
		Scheme:         scheme,
		ControllerName: testListenerSetController,
	}

	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "missing", Namespace: "default"},
	})

	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, res)
}

func TestListenerSetReconciler_RejectsWhenAllowedListenersUnset(t *testing.T) {
	t.Parallel()

	gc := managedGatewayClass()
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "infra"},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(gc.Name),
			Listeners: []gatewayv1.Listener{
				{Name: "gw-l1", Port: 80, Protocol: gatewayv1.HTTPProtocolType},
			},
		},
	}
	ls := &gatewayv1.ListenerSet{
		ObjectMeta: metav1.ObjectMeta{Name: "ls", Namespace: "infra", Generation: 7},
		Spec: gatewayv1.ListenerSetSpec{
			ParentRef: gatewayv1.ParentGatewayReference{Name: gatewayv1.ObjectName(gw.Name)},
			Listeners: []gatewayv1.ListenerEntry{
				{Name: "ls-l1", Port: 80, Protocol: gatewayv1.HTTPProtocolType},
			},
		},
	}

	r, cli := newListenerSetReconciler(t, gc, gw, ls)

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: ls.Name, Namespace: ls.Namespace},
	})
	require.NoError(t, err)

	updated := getListenerSet(t, cli, ls.Name, ls.Namespace)

	accepted := findCondition(updated.Status.Conditions, string(gatewayv1.ListenerSetConditionAccepted))
	require.NotNil(t, accepted)
	assert.Equal(t, metav1.ConditionFalse, accepted.Status)
	assert.Equal(t, string(gatewayv1.ListenerSetReasonNotAllowed), accepted.Reason)
	assert.Equal(t, int64(7), accepted.ObservedGeneration)

	programmed := findCondition(updated.Status.Conditions, string(gatewayv1.ListenerSetConditionProgrammed))
	require.NotNil(t, programmed)
	assert.Equal(t, metav1.ConditionFalse, programmed.Status)
	assert.Equal(t, string(gatewayv1.ListenerSetReasonNotAllowed), programmed.Reason)
}

func TestListenerSetReconciler_AcceptsSameNamespaceWhenFromSame(t *testing.T) {
	t.Parallel()

	from := gatewayv1.NamespacesFromSame

	gc := managedGatewayClass()
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "infra"},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(gc.Name),
			AllowedListeners: &gatewayv1.AllowedListeners{
				Namespaces: &gatewayv1.ListenerNamespaces{From: &from},
			},
			Listeners: []gatewayv1.Listener{
				{Name: "gw-l1", Port: 80, Protocol: gatewayv1.HTTPProtocolType},
			},
		},
	}
	ls := &gatewayv1.ListenerSet{
		ObjectMeta: metav1.ObjectMeta{Name: "ls", Namespace: "infra"},
		Spec: gatewayv1.ListenerSetSpec{
			ParentRef: gatewayv1.ParentGatewayReference{Name: gatewayv1.ObjectName(gw.Name)},
			Listeners: []gatewayv1.ListenerEntry{
				{
					Name:     "ls-l1",
					Port:     81,
					Protocol: gatewayv1.HTTPProtocolType,
				},
			},
		},
	}

	r, cli := newListenerSetReconciler(t, gc, gw, ls)

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: ls.Name, Namespace: ls.Namespace},
	})
	require.NoError(t, err)

	updated := getListenerSet(t, cli, ls.Name, ls.Namespace)

	accepted := findCondition(updated.Status.Conditions, string(gatewayv1.ListenerSetConditionAccepted))
	require.NotNil(t, accepted)
	assert.Equal(t, metav1.ConditionTrue, accepted.Status)
	assert.Equal(t, string(gatewayv1.ListenerSetReasonAccepted), accepted.Reason)

	programmed := findCondition(updated.Status.Conditions, string(gatewayv1.ListenerSetConditionProgrammed))
	require.NotNil(t, programmed)
	assert.Equal(t, metav1.ConditionTrue, programmed.Status)
	assert.Equal(t, string(gatewayv1.ListenerSetReasonProgrammed), programmed.Reason)

	require.Len(t, updated.Status.Listeners, 1)
	entry := updated.Status.Listeners[0]
	assert.Equal(t, gatewayv1.SectionName("ls-l1"), entry.Name)

	entryAccepted := findCondition(entry.Conditions, string(gatewayv1.ListenerConditionAccepted))
	require.NotNil(t, entryAccepted)
	assert.Equal(t, metav1.ConditionTrue, entryAccepted.Status)

	entryProgrammed := findCondition(entry.Conditions, string(gatewayv1.ListenerConditionProgrammed))
	require.NotNil(t, entryProgrammed)
	assert.Equal(t, metav1.ConditionTrue, entryProgrammed.Status)

	entryResolved := findCondition(entry.Conditions, string(gatewayv1.ListenerConditionResolvedRefs))
	require.NotNil(t, entryResolved)
	assert.Equal(t, metav1.ConditionTrue, entryResolved.Status)
}

func TestListenerSetReconciler_MarksAllListenersValidNotValidWhenAllConflict(t *testing.T) {
	t.Parallel()

	from := gatewayv1.NamespacesFromSame

	gc := managedGatewayClass()
	conflictHost := gatewayv1.Hostname("conflict.example.com")
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "infra"},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(gc.Name),
			AllowedListeners: &gatewayv1.AllowedListeners{
				Namespaces: &gatewayv1.ListenerNamespaces{From: &from},
			},
			Listeners: []gatewayv1.Listener{
				{Name: "gw-l1", Port: 80, Protocol: gatewayv1.HTTPProtocolType, Hostname: &conflictHost},
			},
		},
	}
	ls := &gatewayv1.ListenerSet{
		ObjectMeta: metav1.ObjectMeta{Name: "ls", Namespace: "infra"},
		Spec: gatewayv1.ListenerSetSpec{
			ParentRef: gatewayv1.ParentGatewayReference{Name: gatewayv1.ObjectName(gw.Name)},
			Listeners: []gatewayv1.ListenerEntry{
				{Name: "only", Port: 80, Protocol: gatewayv1.HTTPProtocolType, Hostname: &conflictHost},
			},
		},
	}

	r, cli := newListenerSetReconciler(t, gc, gw, ls)

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: ls.Name, Namespace: ls.Namespace},
	})
	require.NoError(t, err)

	updated := getListenerSet(t, cli, ls.Name, ls.Namespace)

	accepted := findCondition(updated.Status.Conditions, string(gatewayv1.ListenerSetConditionAccepted))
	require.NotNil(t, accepted)
	assert.Equal(t, metav1.ConditionFalse, accepted.Status)
	assert.Equal(t, string(gatewayv1.ListenerSetReasonListenersNotValid), accepted.Reason)

	programmed := findCondition(updated.Status.Conditions, string(gatewayv1.ListenerSetConditionProgrammed))
	require.NotNil(t, programmed)
	assert.Equal(t, metav1.ConditionFalse, programmed.Status)
	assert.Equal(t, string(gatewayv1.ListenerSetReasonListenersNotValid), programmed.Reason)

	require.Len(t, updated.Status.Listeners, 1)
	entry := updated.Status.Listeners[0]

	entryAccepted := findCondition(entry.Conditions, string(gatewayv1.ListenerConditionAccepted))
	require.NotNil(t, entryAccepted)
	assert.Equal(t, metav1.ConditionFalse, entryAccepted.Status)
	assert.Equal(t, string(gatewayv1.ListenerReasonHostnameConflict), entryAccepted.Reason)

	entryConflicted := findCondition(entry.Conditions, string(gatewayv1.ListenerConditionConflicted))
	require.NotNil(t, entryConflicted)
	assert.Equal(t, metav1.ConditionTrue, entryConflicted.Status)
	assert.Equal(t, string(gatewayv1.ListenerReasonHostnameConflict), entryConflicted.Reason)
}

// TestListenerSetReconciler_ConflictedEntryStillCountsAttachedRoutes pins that a
// route attached to a conflicted ListenerSet entry is still counted in
// AttachedRoutes. Per the Gateway API spec, attachment depends solely on
// AllowedRoutes + ParentRefs; the listener's own status (here Conflicted /
// Programmed=False) MUST NOT reduce the count.
func TestListenerSetReconciler_ConflictedEntryStillCountsAttachedRoutes(t *testing.T) {
	t.Parallel()

	from := gatewayv1.NamespacesFromSame
	conflictHost := gatewayv1.Hostname("conflict.example.com")
	section := gatewayv1.SectionName("only")
	lsKind := gatewayv1.Kind(kindListenerSet)

	gc := managedGatewayClass()
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "infra"},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(gc.Name),
			AllowedListeners: &gatewayv1.AllowedListeners{
				Namespaces: &gatewayv1.ListenerNamespaces{From: &from},
			},
			Listeners: []gatewayv1.Listener{
				{Name: "gw-l1", Port: 80, Protocol: gatewayv1.HTTPProtocolType, Hostname: &conflictHost},
			},
		},
	}
	ls := &gatewayv1.ListenerSet{
		ObjectMeta: metav1.ObjectMeta{Name: "ls", Namespace: "infra"},
		Spec: gatewayv1.ListenerSetSpec{
			ParentRef: gatewayv1.ParentGatewayReference{Name: gatewayv1.ObjectName(gw.Name)},
			Listeners: []gatewayv1.ListenerEntry{
				{
					Name: section, Port: 80, Protocol: gatewayv1.HTTPProtocolType, Hostname: &conflictHost,
					AllowedRoutes: &gatewayv1.AllowedRoutes{
						Namespaces: &gatewayv1.RouteNamespaces{From: namespacesFromAllPtr()},
					},
				},
			},
		},
	}
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "infra"},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Kind: &lsKind, Name: gatewayv1.ObjectName(ls.Name), SectionName: &section},
				},
			},
		},
	}

	r, cli := newListenerSetReconcilerWithObjects(t, newListenerSetScheme(t), gc, gw, ls, route)

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: ls.Name, Namespace: ls.Namespace},
	})
	require.NoError(t, err)

	updated := getListenerSet(t, cli, ls.Name, ls.Namespace)
	require.Len(t, updated.Status.Listeners, 1)
	entry := updated.Status.Listeners[0]

	// The entry IS conflicted (and therefore not programmed)...
	entryConflicted := findCondition(entry.Conditions, string(gatewayv1.ListenerConditionConflicted))
	require.NotNil(t, entryConflicted)
	assert.Equal(t, metav1.ConditionTrue, entryConflicted.Status)

	// ...yet the attached route is still counted.
	assert.Equal(t, int32(1), entry.AttachedRoutes,
		"a route attached to a conflicted listener is still counted per Gateway API AttachedRoutes semantics")
}

// TestListenerSetReconciler_UnresolvedTLSRefStillCountsAttachedRoutes pins that
// a route attached to an HTTPS ListenerSet entry whose TLS certificate ref fails
// to resolve (ResolvedRefs=False, Programmed=False) is still counted in
// AttachedRoutes. The Gateway API spec makes attachment depend solely on
// AllowedRoutes + ParentRefs, independent of the listener's own status, so the
// non-zero count is correct, not a defect.
func TestListenerSetReconciler_UnresolvedTLSRefStillCountsAttachedRoutes(t *testing.T) {
	t.Parallel()

	from := gatewayv1.NamespacesFromSame
	section := gatewayv1.SectionName("https")
	lsKind := gatewayv1.Kind(kindListenerSet)

	gc := managedGatewayClass()
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "infra"},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(gc.Name),
			AllowedListeners: &gatewayv1.AllowedListeners{
				Namespaces: &gatewayv1.ListenerNamespaces{From: &from},
			},
			Listeners: []gatewayv1.Listener{
				{Name: "gw-l1", Port: 80, Protocol: gatewayv1.HTTPProtocolType},
			},
		},
	}
	ls := &gatewayv1.ListenerSet{
		ObjectMeta: metav1.ObjectMeta{Name: "ls", Namespace: "infra"},
		Spec: gatewayv1.ListenerSetSpec{
			ParentRef: gatewayv1.ParentGatewayReference{Name: gatewayv1.ObjectName(gw.Name)},
			Listeners: []gatewayv1.ListenerEntry{
				{
					Name: section, Port: 443, Protocol: gatewayv1.HTTPSProtocolType,
					TLS: &gatewayv1.ListenerTLSConfig{
						CertificateRefs: []gatewayv1.SecretObjectReference{{Name: "missing"}},
					},
					AllowedRoutes: &gatewayv1.AllowedRoutes{
						Namespaces: &gatewayv1.RouteNamespaces{From: namespacesFromAllPtr()},
					},
				},
			},
		},
	}
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "infra"},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Kind: &lsKind, Name: gatewayv1.ObjectName(ls.Name), SectionName: &section},
				},
			},
		},
	}

	scheme := newListenerSetScheme(t)
	require.NoError(t, corev1.AddToScheme(scheme))
	r, cli := newListenerSetReconcilerWithObjects(t, scheme, gc, gw, ls, route)

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: ls.Name, Namespace: ls.Namespace},
	})
	require.NoError(t, err)

	updated := getListenerSet(t, cli, ls.Name, ls.Namespace)
	require.Len(t, updated.Status.Listeners, 1)
	entry := updated.Status.Listeners[0]

	// The cert ref did not resolve, so the entry is not programmed...
	entryResolved := findCondition(entry.Conditions, string(gatewayv1.ListenerConditionResolvedRefs))
	require.NotNil(t, entryResolved)
	assert.Equal(t, metav1.ConditionFalse, entryResolved.Status)

	entryProgrammed := findCondition(entry.Conditions, string(gatewayv1.ListenerConditionProgrammed))
	require.NotNil(t, entryProgrammed)
	assert.Equal(t, metav1.ConditionFalse, entryProgrammed.Status)

	// ...yet the attached route is still counted.
	assert.Equal(t, int32(1), entry.AttachedRoutes,
		"a route attached to a listener with unresolved TLS refs is still counted per Gateway API AttachedRoutes semantics")
}

// TestListenerSetReconciler_RejectedListenerSetReportsZeroAttachedRoutes pins
// that a ListenerSet rejected at the resource level (NotAllowed by the parent
// Gateway's allowedListeners) reports AttachedRoutes=0 for its entries even when
// a route targets them. The entries are not part of any merged Gateway, so no
// route can be Accepted on them, and the spec counts only Accepted routes — this
// is deliberately different from a conflicted entry on an accepted ListenerSet,
// which does count its attached routes.
func TestListenerSetReconciler_RejectedListenerSetReportsZeroAttachedRoutes(t *testing.T) {
	t.Parallel()

	section := gatewayv1.SectionName("only")
	lsKind := gatewayv1.Kind(kindListenerSet)

	gc := managedGatewayClass()
	// No AllowedListeners on the Gateway → the ListenerSet is NotAllowed.
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "infra"},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(gc.Name),
			Listeners: []gatewayv1.Listener{
				{Name: "gw-l1", Port: 80, Protocol: gatewayv1.HTTPProtocolType},
			},
		},
	}
	ls := &gatewayv1.ListenerSet{
		ObjectMeta: metav1.ObjectMeta{Name: "ls", Namespace: "infra"},
		Spec: gatewayv1.ListenerSetSpec{
			ParentRef: gatewayv1.ParentGatewayReference{Name: gatewayv1.ObjectName(gw.Name)},
			Listeners: []gatewayv1.ListenerEntry{
				{
					Name: section, Port: 80, Protocol: gatewayv1.HTTPProtocolType,
					AllowedRoutes: &gatewayv1.AllowedRoutes{
						Namespaces: &gatewayv1.RouteNamespaces{From: namespacesFromAllPtr()},
					},
				},
			},
		},
	}
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "infra"},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Kind: &lsKind, Name: gatewayv1.ObjectName(ls.Name), SectionName: &section},
				},
			},
		},
	}

	r, cli := newListenerSetReconcilerWithObjects(t, newListenerSetScheme(t), gc, gw, ls, route)

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: ls.Name, Namespace: ls.Namespace},
	})
	require.NoError(t, err)

	updated := getListenerSet(t, cli, ls.Name, ls.Namespace)

	// The ListenerSet is rejected at the resource level...
	accepted := findCondition(updated.Status.Conditions, string(gatewayv1.ListenerSetConditionAccepted))
	require.NotNil(t, accepted)
	assert.Equal(t, metav1.ConditionFalse, accepted.Status)
	assert.Equal(t, string(gatewayv1.ListenerSetReasonNotAllowed), accepted.Reason)

	// ...so its entries report zero attached routes despite the targeting route.
	require.Len(t, updated.Status.Listeners, 1)
	assert.Equal(t, int32(0), updated.Status.Listeners[0].AttachedRoutes,
		"a route targeting a resource-level-rejected ListenerSet is not Accepted and must not be counted")
}

func TestListenerSetReconciler_SkipsWhenParentNotManaged(t *testing.T) {
	t.Parallel()

	scheme := newListenerSetScheme(t)
	gc := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "other-class"},
		Spec:       gatewayv1.GatewayClassSpec{ControllerName: "other.example.com/other"},
	}
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "infra"},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(gc.Name),
		},
	}
	ls := &gatewayv1.ListenerSet{
		ObjectMeta: metav1.ObjectMeta{Name: "ls", Namespace: "infra"},
		Spec: gatewayv1.ListenerSetSpec{
			ParentRef: gatewayv1.ParentGatewayReference{Name: gatewayv1.ObjectName(gw.Name)},
		},
	}

	cli := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(gc, gw, ls).
		WithStatusSubresource(&gatewayv1.ListenerSet{}).
		Build()

	r := &ListenerSetReconciler{
		Client:         cli,
		Scheme:         scheme,
		ControllerName: testListenerSetController,
	}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: ls.Name, Namespace: ls.Namespace},
	})
	require.NoError(t, err)

	updated := getListenerSet(t, cli, ls.Name, ls.Namespace)
	assert.Empty(t, updated.Status.Conditions, "controller should not touch ListenerSets attached to other-class Gateways")
}

func newListenerSetReconciler(
	t *testing.T,
	gc *gatewayv1.GatewayClass,
	gw *gatewayv1.Gateway,
	ls *gatewayv1.ListenerSet,
) (*ListenerSetReconciler, client.Client) {
	t.Helper()

	scheme := newListenerSetScheme(t)
	cli := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(gc, gw, ls).
		WithStatusSubresource(&gatewayv1.ListenerSet{}, &gatewayv1.Gateway{}).
		Build()

	return &ListenerSetReconciler{
		Client:         cli,
		Scheme:         scheme,
		ControllerName: testListenerSetController,
	}, cli
}

func newListenerSetReconcilerWithObjects(
	t *testing.T,
	scheme *runtime.Scheme,
	objs ...client.Object,
) (*ListenerSetReconciler, client.Client) {
	t.Helper()

	cli := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&gatewayv1.ListenerSet{}, &gatewayv1.Gateway{}).
		Build()

	return &ListenerSetReconciler{
		Client:         cli,
		Scheme:         scheme,
		ControllerName: testListenerSetController,
	}, cli
}

func newListenerSetScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, gatewayv1.Install(scheme))

	return scheme
}

func managedGatewayClass() *gatewayv1.GatewayClass {
	return &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "managed-class"},
		Spec:       gatewayv1.GatewayClassSpec{ControllerName: testListenerSetController},
	}
}

func getListenerSet(t *testing.T, cli client.Client, name, namespace string) *gatewayv1.ListenerSet {
	t.Helper()

	var ls gatewayv1.ListenerSet
	require.NoError(t, cli.Get(context.Background(), types.NamespacedName{Name: name, Namespace: namespace}, &ls))

	return &ls
}
