package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// reconcileSelectorListenerSet reconciles a ListenerSet with the given entries
// under a Gateway that admits ListenerSets from its own namespace, and returns
// the ListenerSet as stored.
func reconcileSelectorListenerSet(t *testing.T, entries []gatewayv1.ListenerEntry) *gatewayv1.ListenerSet {
	t.Helper()

	fromSame := gatewayv1.NamespacesFromSame

	gc := managedGatewayClass()
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "infra"},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(gc.Name),
			AllowedListeners: &gatewayv1.AllowedListeners{Namespaces: &gatewayv1.ListenerNamespaces{From: &fromSame}},
			Listeners:        []gatewayv1.Listener{{Name: "gw-l1", Port: 80, Protocol: gatewayv1.HTTPProtocolType}},
		},
	}
	ls := &gatewayv1.ListenerSet{
		ObjectMeta: metav1.ObjectMeta{Name: "ls", Namespace: "infra", Generation: 1},
		Spec: gatewayv1.ListenerSetSpec{
			ParentRef: gatewayv1.ParentGatewayReference{Name: gatewayv1.ObjectName(gw.Name)},
			Listeners: entries,
		},
	}

	r, cli := newListenerSetReconciler(t, gc, gw, ls)

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: ls.Name, Namespace: ls.Namespace},
	})
	require.NoError(t, err)

	return getListenerSet(t, cli, ls.Name, ls.Namespace)
}

func entryStatusNamed(t *testing.T, ls *gatewayv1.ListenerSet, name gatewayv1.SectionName) gatewayv1.ListenerEntryStatus {
	t.Helper()

	for _, status := range ls.Status.Listeners {
		if status.Name == name {
			return status
		}
	}

	require.Failf(t, "no entry status", "entry %s", name)

	return gatewayv1.ListenerEntryStatus{}
}

func invalidSelectorEntry(name gatewayv1.SectionName, port gatewayv1.PortNumber) gatewayv1.ListenerEntry {
	fromSelector := gatewayv1.NamespacesFromSelector

	return gatewayv1.ListenerEntry{
		Name: name, Port: port, Protocol: gatewayv1.HTTPProtocolType,
		AllowedRoutes: &gatewayv1.AllowedRoutes{
			Namespaces: &gatewayv1.RouteNamespaces{From: &fromSelector, Selector: bogusNamespaceSelector()},
		},
	}
}

// TestListenerSetEntryStatus_InvalidAllowedRoutesSelector pins the #901 fix
// for ListenerSet entries, which are listeners too: an entry whose
// allowedRoutes.namespaces.selector does not parse is not Accepted and not
// Programmed, and says the selector is invalid without quoting it. A sibling
// entry still serves, so the ListenerSet stays Accepted with ListenersNotValid.
func TestListenerSetEntryStatus_InvalidAllowedRoutesSelector(t *testing.T) {
	t.Parallel()

	ls := reconcileSelectorListenerSet(t, []gatewayv1.ListenerEntry{
		invalidSelectorEntry("broken", 8080),
		{Name: "healthy", Port: 8081, Protocol: gatewayv1.HTTPProtocolType},
	})

	broken := entryStatusNamed(t, ls, "broken")

	accepted := findCondition(broken.Conditions, string(gatewayv1.ListenerConditionAccepted))
	require.NotNil(t, accepted)
	assert.Equal(t, metav1.ConditionFalse, accepted.Status)
	assert.Equal(t, string(gatewayv1.ListenerReasonUnsupportedValue), accepted.Reason)
	assert.Contains(t, accepted.Message, "allowedRoutes.namespaces.selector is invalid")
	assert.NotContains(t, accepted.Message, "BogusOperator", "the status says the selector is invalid, not what it is")

	programmed := findCondition(broken.Conditions, string(gatewayv1.ListenerConditionProgrammed))
	require.NotNil(t, programmed)
	assert.Equal(t, metav1.ConditionFalse, programmed.Status)
	assert.Equal(t, string(gatewayv1.ListenerReasonInvalid), programmed.Reason)

	healthyAccepted := findCondition(entryStatusNamed(t, ls, "healthy").Conditions, string(gatewayv1.ListenerConditionAccepted))
	require.NotNil(t, healthyAccepted)
	assert.Equal(t, metav1.ConditionTrue, healthyAccepted.Status)

	lsAccepted := findCondition(ls.Status.Conditions, string(gatewayv1.ListenerSetConditionAccepted))
	require.NotNil(t, lsAccepted)
	assert.Equal(t, metav1.ConditionTrue, lsAccepted.Status, "the healthy entry still serves")
	assert.Equal(t, string(gatewayv1.ListenerSetReasonListenersNotValid), lsAccepted.Reason)
	assert.Contains(t, lsAccepted.Message, "invalid allowedRoutes.namespaces.selector")
	assert.NotContains(t, lsAccepted.Message, "BogusOperator")
}

// TestListenerSetEntryStatus_AllSelectorsInvalid pins the ListenerSet verdict
// when no entry is usable: Accepted=False with ListenersNotValid, and the
// parent Gateway does not count it as attached.
func TestListenerSetEntryStatus_AllSelectorsInvalid(t *testing.T) {
	t.Parallel()

	ls := reconcileSelectorListenerSet(t, []gatewayv1.ListenerEntry{
		invalidSelectorEntry("a", 8080),
		invalidSelectorEntry("b", 8081),
	})

	lsAccepted := findCondition(ls.Status.Conditions, string(gatewayv1.ListenerSetConditionAccepted))
	require.NotNil(t, lsAccepted)
	assert.Equal(t, metav1.ConditionFalse, lsAccepted.Status)
	assert.Equal(t, string(gatewayv1.ListenerSetReasonListenersNotValid), lsAccepted.Reason)
	assert.Contains(t, lsAccepted.Message, "allowedRoutes.namespaces.selector")
}

// TestListenerSetEntriesAccepted_SkipsInvalidSelector pins the Gateway's
// attachedListenerSets count to the ListenerSet's own verdict: a ListenerSet
// whose only entry has an invalid selector is not attached.
func TestListenerSetEntriesAccepted_SkipsInvalidSelector(t *testing.T) {
	t.Parallel()

	ls := &gatewayv1.ListenerSet{
		ObjectMeta: metav1.ObjectMeta{Name: "ls", Namespace: "infra"},
		Spec:       gatewayv1.ListenerSetSpec{Listeners: []gatewayv1.ListenerEntry{invalidSelectorEntry("only", 8080)}},
	}

	cli := buildGatewayFakeClient(t, ls)

	assert.False(t, listenerSetEntriesAccepted(context.Background(), cli, ls, nil))
}

// TestListenerSetAggregate_UnsupportedProtocolEntry pins that an entry with a
// protocol this controller does not serve drives the ListenerSet aggregate the
// way the vendored ListenerSetReasonListenersNotValid defines: Accepted=True
// with ListenersNotValid while another entry serves, Accepted=False when none
// does, and the parent Gateway does not count a ListenerSet with no usable entry.
func TestListenerSetAggregate_UnsupportedProtocolEntry(t *testing.T) {
	t.Parallel()

	tcpEntry := func(name gatewayv1.SectionName, port gatewayv1.PortNumber) gatewayv1.ListenerEntry {
		return gatewayv1.ListenerEntry{Name: name, Port: port, Protocol: gatewayv1.TCPProtocolType}
	}

	tests := []struct {
		name     string
		entries  []gatewayv1.ListenerEntry
		accepted metav1.ConditionStatus
	}{
		{
			name: "a sibling serves",
			entries: []gatewayv1.ListenerEntry{
				tcpEntry("tcp", 9000),
				{Name: "healthy", Port: 8081, Protocol: gatewayv1.HTTPProtocolType},
			},
			accepted: metav1.ConditionTrue,
		},
		{
			name:     "no entry serves",
			entries:  []gatewayv1.ListenerEntry{tcpEntry("tcp", 9000)},
			accepted: metav1.ConditionFalse,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ls := reconcileSelectorListenerSet(t, tt.entries)

			lsAccepted := findCondition(ls.Status.Conditions, string(gatewayv1.ListenerSetConditionAccepted))
			require.NotNil(t, lsAccepted)
			assert.Equal(t, tt.accepted, lsAccepted.Status)
			assert.Equal(t, string(gatewayv1.ListenerSetReasonListenersNotValid), lsAccepted.Reason)
			assert.Contains(t, lsAccepted.Message, "protocol")

			spec := &gatewayv1.ListenerSet{
				ObjectMeta: metav1.ObjectMeta{Name: "ls", Namespace: "infra"},
				Spec:       gatewayv1.ListenerSetSpec{Listeners: tt.entries},
			}
			assert.Equal(t, tt.accepted == metav1.ConditionTrue,
				listenerSetEntriesAccepted(context.Background(), buildGatewayFakeClient(t, spec), spec, nil),
				"the Gateway's attachedListenerSets follows the ListenerSet's own verdict")
		})
	}
}
