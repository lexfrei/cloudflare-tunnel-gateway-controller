package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// permissiveListener builds an HTTP listener with the given allowedRoutes-from
// and hostname pin, for the hostname-capture-risk detection tests.
func permissiveListener(from *gatewayv1.FromNamespaces, hostname string) gatewayv1.Listener {
	listener := gatewayv1.Listener{Name: "web", Port: 80, Protocol: gatewayv1.HTTPProtocolType}

	if from != nil {
		listener.AllowedRoutes = &gatewayv1.AllowedRoutes{
			Namespaces: &gatewayv1.RouteNamespaces{From: from},
		}
	}

	if hostname != "" {
		h := gatewayv1.Hostname(hostname)
		listener.Hostname = &h
	}

	return listener
}

// TestGatewayReconciler_PermissiveListenerSurfacesCaptureRiskCondition pins
// #476's optional follow-up: a listener combining allowedRoutes.namespaces.from:
// All with no hostname pin is the hostname-capture vector (any namespace can
// claim any hostname), so the controller surfaces a dedicated advisory condition
// — without rejecting it (the combination is legal Gateway API).
func TestGatewayReconciler_PermissiveListenerSurfacesCaptureRiskCondition(t *testing.T) {
	t.Parallel()

	fromAll := gatewayv1.NamespacesFromAll
	fromSame := gatewayv1.NamespacesFromSame
	fromSelector := gatewayv1.NamespacesFromSelector

	cases := []struct {
		name     string
		listener gatewayv1.Listener
		wantRisk bool
	}{
		{"from All + unpinned hostname → risk", permissiveListener(&fromAll, ""), true},
		{"from All + pinned hostname → safe", permissiveListener(&fromAll, "*.team.example.com"), false},
		{"from Same + unpinned → safe", permissiveListener(&fromSame, ""), false},
		{"from Selector + unpinned → safe", permissiveListener(&fromSelector, ""), false},
		{"allowedRoutes unset + unpinned → safe (defaults to Same)", permissiveListener(nil, ""), false},
		{
			"namespaces set but from nil + unpinned → safe",
			gatewayv1.Listener{
				Name: "web", Port: 80, Protocol: gatewayv1.HTTPProtocolType,
				AllowedRoutes: &gatewayv1.AllowedRoutes{Namespaces: &gatewayv1.RouteNamespaces{}},
			},
			false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			statuses := reconcileGatewayListeners(t, []gatewayv1.Listener{tc.listener})
			require.Len(t, statuses, 1)

			cond := findCondition(statuses[0].Conditions, listenerConditionPermissiveHostname)

			if tc.wantRisk {
				require.NotNil(t, cond, "the permissive-hostname condition must be present")
				assert.Equal(t, metav1.ConditionTrue, cond.Status)
				assert.Equal(t, listenerReasonUnpinnedHostnameAllowsAll, cond.Reason)
				// The risky combination does not reject the listener — it stays Accepted.
				accepted := findCondition(statuses[0].Conditions, string(gatewayv1.ListenerConditionAccepted))
				require.NotNil(t, accepted)
				assert.Equal(t, metav1.ConditionTrue, accepted.Status, "a permissive listener is still legal/Accepted")
			} else {
				assert.Nil(t, cond, "a pinned or non-All listener must not carry the capture-risk condition")
			}
		})
	}
}

// TestGatewayReconciler_PermissiveCondition_SkippedOnUnservableListener pins that
// the advisory is NOT raised on a listener the controller has already rejected as
// unservable: hostname capture is moot on a protocol with no Host routing through
// our data plane, and the listener is Accepted=False anyway.
func TestGatewayReconciler_PermissiveCondition_SkippedOnUnservableListener(t *testing.T) {
	t.Parallel()

	fromAll := gatewayv1.NamespacesFromAll

	statuses := reconcileGatewayListeners(t, []gatewayv1.Listener{
		{
			Name: "tcp", Port: 9000, Protocol: gatewayv1.TCPProtocolType,
			AllowedRoutes: &gatewayv1.AllowedRoutes{Namespaces: &gatewayv1.RouteNamespaces{From: &fromAll}},
		},
	})
	require.Len(t, statuses, 1)

	accepted := findCondition(statuses[0].Conditions, string(gatewayv1.ListenerConditionAccepted))
	require.NotNil(t, accepted)
	require.Equal(t, metav1.ConditionFalse, accepted.Status, "a TCP listener is unservable here")

	assert.Nil(t, findCondition(statuses[0].Conditions, listenerConditionPermissiveHostname),
		"the capture-risk advisory must not appear on an already-rejected listener")
}

// TestGatewayReconciler_PermissiveCondition_SkippedOnConflictedListener pins that
// a listener marked Conflicted (a lower-precedence collision) does not also carry
// the capture-risk advisory — it is not serving, so the advisory would be noise.
// Two unpinned HTTP listeners on the same port collide on the (port, "") tuple;
// the lower-precedence one is Conflicted.
func TestGatewayReconciler_PermissiveCondition_SkippedOnConflictedListener(t *testing.T) {
	t.Parallel()

	fromAll := gatewayv1.NamespacesFromAll
	allowAll := &gatewayv1.AllowedRoutes{Namespaces: &gatewayv1.RouteNamespaces{From: &fromAll}}

	statuses := reconcileGatewayListeners(t, []gatewayv1.Listener{
		{Name: "first", Port: 80, Protocol: gatewayv1.HTTPProtocolType, AllowedRoutes: allowAll},
		{Name: "second", Port: 80, Protocol: gatewayv1.HTTPProtocolType, AllowedRoutes: allowAll},
	})
	require.Len(t, statuses, 2)

	byName := map[string]gatewayv1.ListenerStatus{}
	for _, s := range statuses {
		byName[string(s.Name)] = s
	}

	conflicted := findCondition(byName["second"].Conditions, string(gatewayv1.ListenerConditionConflicted))
	require.NotNil(t, conflicted, "the lower-precedence same-port listener must be Conflicted")
	require.Equal(t, metav1.ConditionTrue, conflicted.Status)

	assert.Nil(t, findCondition(byName["second"].Conditions, listenerConditionPermissiveHostname),
		"a conflicted (non-serving) listener must not carry the capture-risk advisory")
}

// TestListenerSetEntry_PermissiveHostnameAdvisory pins that the same capture-risk
// advisory is raised on a tenant-authored ListenerSet entry — the highest-risk
// place for hostname capture, since the entry author may be untrusted. A serving
// entry with from: All + no hostname pin carries it; a pinned or scoped entry
// does not.
func TestListenerSetEntry_PermissiveHostnameAdvisory(t *testing.T) {
	t.Parallel()

	fromAll := gatewayv1.NamespacesFromAll
	fromSame := gatewayv1.NamespacesFromSame

	entry := func(name string, from *gatewayv1.FromNamespaces, hostname string) gatewayv1.ListenerEntry {
		e := gatewayv1.ListenerEntry{Name: gatewayv1.SectionName(name), Port: 80, Protocol: gatewayv1.HTTPProtocolType}
		if from != nil {
			e.AllowedRoutes = &gatewayv1.AllowedRoutes{Namespaces: &gatewayv1.RouteNamespaces{From: from}}
		}

		if hostname != "" {
			h := gatewayv1.Hostname(hostname)
			e.Hostname = &h
		}

		return e
	}

	ls := &gatewayv1.ListenerSet{
		ObjectMeta: metav1.ObjectMeta{Name: "tenant", Namespace: "team-a"},
		Spec: gatewayv1.ListenerSetSpec{
			Listeners: []gatewayv1.ListenerEntry{
				entry("risky", &fromAll, ""),
				entry("pinned", &fromAll, "*.team-a.example.com"),
				entry("scoped", &fromSame, ""),
			},
		},
	}

	statuses := buildListenerSetEntryStatuses(ls, listenerSetAcceptanceResult{}, 1, metav1.Now())
	require.Len(t, statuses, 3)

	byName := map[string]gatewayv1.ListenerEntryStatus{}
	for _, s := range statuses {
		byName[string(s.Name)] = s
	}

	risky := findCondition(byName["risky"].Conditions, listenerConditionPermissiveHostname)
	require.NotNil(t, risky, "a from:All + unpinned ListenerSet entry must carry the capture-risk advisory")
	assert.Equal(t, metav1.ConditionTrue, risky.Status)
	assert.Equal(t, listenerReasonUnpinnedHostnameAllowsAll, risky.Reason)

	assert.Nil(t, findCondition(byName["pinned"].Conditions, listenerConditionPermissiveHostname),
		"a pinned entry must not carry the advisory")
	assert.Nil(t, findCondition(byName["scoped"].Conditions, listenerConditionPermissiveHostname),
		"a from:Same entry must not carry the advisory")
}
