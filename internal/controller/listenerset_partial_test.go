package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// reconcileEntriesListenerSet reconciles a ListenerSet with the given entries
// under a Gateway holding one HTTP listener on port 80 with no hostname, with
// Secrets in the scheme so certificate refs resolve or fail normally.
func reconcileEntriesListenerSet(t *testing.T, entries []gatewayv1.ListenerEntry) *gatewayv1.ListenerSet {
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

	scheme := newListenerSetScheme(t)
	require.NoError(t, corev1.AddToScheme(scheme))
	r, cli := newListenerSetReconcilerWithObjects(t, scheme, gc, gw, ls)

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: ls.Name, Namespace: ls.Namespace},
	})
	require.NoError(t, err)

	return getListenerSet(t, cli, ls.Name, ls.Namespace)
}

// TestListenerSetAggregate_PartiallyValid pins the vendored
// ListenerSetReasonListenersNotValid for every kind of unusable entry: a
// conflicted entry, or one whose references do not resolve, next to a usable
// entry leaves the ListenerSet Accepted=True with ListenersNotValid.
func TestListenerSetAggregate_PartiallyValid(t *testing.T) {
	t.Parallel()

	gatewayHost := gatewayv1.Hostname("gw.example.com")
	httpsMode := gatewayv1.TLSModeTerminate

	tests := []struct {
		name    string
		broken  gatewayv1.ListenerEntry
		message string
	}{
		{
			name: "conflicted entry",
			// The Gateway listener gw-l1 holds port 80 with no hostname; the
			// same tuple here conflicts and loses.
			broken:  gatewayv1.ListenerEntry{Name: "broken", Port: 80, Protocol: gatewayv1.HTTPProtocolType},
			message: "conflict",
		},
		{
			name: "unresolved certificate ref",
			broken: gatewayv1.ListenerEntry{
				Name: "broken", Port: 8443, Protocol: gatewayv1.HTTPSProtocolType, Hostname: &gatewayHost,
				TLS: &gatewayv1.ListenerTLSConfig{
					Mode:            &httpsMode,
					CertificateRefs: []gatewayv1.SecretObjectReference{{Name: "missing-cert"}},
				},
			},
			message: "unresolved",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ls := reconcileEntriesListenerSet(t, []gatewayv1.ListenerEntry{
				tt.broken,
				{Name: "healthy", Port: 8081, Protocol: gatewayv1.HTTPProtocolType},
			})

			lsAccepted := findCondition(ls.Status.Conditions, string(gatewayv1.ListenerSetConditionAccepted))
			require.NotNil(t, lsAccepted)
			assert.Equal(t, metav1.ConditionTrue, lsAccepted.Status, "the healthy entry still serves")
			assert.Equal(t, string(gatewayv1.ListenerSetReasonListenersNotValid), lsAccepted.Reason)
			assert.Contains(t, lsAccepted.Message, tt.message)
		})
	}
}
