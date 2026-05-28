package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/listenermerge"
)

// TestSummariseListenerSet_ConflictFreeButUnresolvedRefsRejected pins the
// interaction the listenerset-reference-grant conformance scenario exercises:
// an entry that is conflict-free in the merged view but whose TLS cert ref
// did NOT resolve must still drive the aggregate to
// Accepted=False/ListenersNotValid.
func TestSummariseListenerSet_ConflictFreeButUnresolvedRefsRejected(t *testing.T) {
	t.Parallel()

	host := gatewayv1.Hostname("ls.example.com")
	gw := &gatewayv1.Gateway{ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "infra"}}
	ls := &gatewayv1.ListenerSet{
		ObjectMeta: metav1.ObjectMeta{Name: "ls", Namespace: "infra"},
		Spec: gatewayv1.ListenerSetSpec{
			Listeners: []gatewayv1.ListenerEntry{
				{Name: "https", Port: 443, Protocol: gatewayv1.HTTPSProtocolType, Hostname: &host},
			},
		},
	}

	merged := listenermerge.Merge(gw, []*gatewayv1.ListenerSet{ls})

	// Conflict-free entry, but its TLS ref failed to resolve.
	refChecks := map[gatewayv1.SectionName]listenerEntryRefsCheck{
		"https": {Status: metav1.ConditionFalse, Reason: string(gatewayv1.ListenerReasonRefNotPermitted)},
	}

	accepted, reason, _ := summariseListenerSet(merged, ls, refChecks)
	assert.False(t, accepted, "a conflict-free entry with unresolved refs must not accept the ListenerSet")
	assert.Equal(t, gatewayv1.ListenerSetReasonListenersNotValid, reason)
}

// TestSummariseListenerSet_AcceptedWhenConflictFreeAndRefsResolved is the
// positive counterpart.
func TestSummariseListenerSet_AcceptedWhenConflictFreeAndRefsResolved(t *testing.T) {
	t.Parallel()

	host := gatewayv1.Hostname("ls.example.com")
	gw := &gatewayv1.Gateway{ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "infra"}}
	ls := &gatewayv1.ListenerSet{
		ObjectMeta: metav1.ObjectMeta{Name: "ls", Namespace: "infra"},
		Spec: gatewayv1.ListenerSetSpec{
			Listeners: []gatewayv1.ListenerEntry{
				{Name: "https", Port: 443, Protocol: gatewayv1.HTTPSProtocolType, Hostname: &host},
			},
		},
	}

	merged := listenermerge.Merge(gw, []*gatewayv1.ListenerSet{ls})

	refChecks := map[gatewayv1.SectionName]listenerEntryRefsCheck{
		"https": {Status: metav1.ConditionTrue, Reason: string(gatewayv1.ListenerReasonResolvedRefs)},
	}

	accepted, reason, _ := summariseListenerSet(merged, ls, refChecks)
	assert.True(t, accepted)
	assert.Equal(t, gatewayv1.ListenerSetReasonAccepted, reason)
}

// TestSummariseAttachedListenerSets_BrokenTLSRefNotCounted pins that the
// Gateway status.attachedListenerSets count agrees with the per-ListenerSet
// Accepted condition: a ListenerSet whose only entry has a non-existent TLS
// cert Secret is NOT counted as attached, even though it is conflict-free.
func TestSummariseAttachedListenerSets_BrokenTLSRefNotCounted(t *testing.T) {
	t.Parallel()

	fromSame := gatewayv1.NamespacesFromSame
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "infra"},
		Spec: gatewayv1.GatewaySpec{
			AllowedListeners: &gatewayv1.AllowedListeners{
				Namespaces: &gatewayv1.ListenerNamespaces{From: &fromSame},
			},
		},
	}

	host := gatewayv1.Hostname("broken.example.com")
	ls := &gatewayv1.ListenerSet{
		ObjectMeta: metav1.ObjectMeta{Name: "ls", Namespace: "infra"},
		Spec: gatewayv1.ListenerSetSpec{
			ParentRef: gatewayv1.ParentGatewayReference{Name: "gw"},
			Listeners: []gatewayv1.ListenerEntry{
				{
					Name: "https", Port: 443, Protocol: gatewayv1.HTTPSProtocolType, Hostname: &host,
					TLS: &gatewayv1.ListenerTLSConfig{
						CertificateRefs: []gatewayv1.SecretObjectReference{{Name: "missing-cert"}},
					},
				},
			},
		},
	}

	cli := buildTLSFakeClient(t, ls, gw)

	count, err := summariseAttachedListenerSets(context.Background(), cli, gw)
	require.NoError(t, err)
	assert.Equal(t, 0, count, "a ListenerSet with an unresolved TLS cert ref must not be counted as attached")
}

// TestSummariseAttachedListenerSets_HealthyCounted is the positive case: a
// ListenerSet with no TLS material (so refs trivially resolve) and no
// conflict is counted.
func TestSummariseAttachedListenerSets_HealthyCounted(t *testing.T) {
	t.Parallel()

	fromSame := gatewayv1.NamespacesFromSame
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "infra"},
		Spec: gatewayv1.GatewaySpec{
			AllowedListeners: &gatewayv1.AllowedListeners{
				Namespaces: &gatewayv1.ListenerNamespaces{From: &fromSame},
			},
		},
	}

	host := gatewayv1.Hostname("ok.example.com")
	ls := &gatewayv1.ListenerSet{
		ObjectMeta: metav1.ObjectMeta{Name: "ls", Namespace: "infra"},
		Spec: gatewayv1.ListenerSetSpec{
			ParentRef: gatewayv1.ParentGatewayReference{Name: "gw"},
			Listeners: []gatewayv1.ListenerEntry{
				{Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType, Hostname: &host},
			},
		},
	}

	cli := buildTLSFakeClient(t, ls, gw)

	count, err := summariseAttachedListenerSets(context.Background(), cli, gw)
	require.NoError(t, err)
	assert.Equal(t, 1, count)
}
