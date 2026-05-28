package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
