package listenermerge_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/listenermerge"
)

// TestMerge_GatewayOwnConflict_NoWinner pins the Gateway rule that an
// implementation "MUST NOT pick one conflicting Listener as the winner": every
// Gateway-owned listener of a conflicting set is marked, while a distinct
// listener stays clean. A ListenerSet entry still loses to the Gateway.
func TestMerge_GatewayOwnConflict_NoWinner(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		listeners []gatewayv1.Listener
		reason    gatewayv1.ListenerConditionReason
	}{
		{
			name: "hostname conflict",
			listeners: []gatewayv1.Listener{
				{Name: "c1", Port: 80, Protocol: gatewayv1.HTTPProtocolType, Hostname: hostnamePtr("a.example.com")},
				{Name: "c2", Port: 80, Protocol: gatewayv1.HTTPProtocolType, Hostname: hostnamePtr("a.example.com")},
				{Name: "ok", Port: 80, Protocol: gatewayv1.HTTPProtocolType, Hostname: hostnamePtr("b.example.com")},
			},
			reason: gatewayv1.ListenerReasonHostnameConflict,
		},
		{
			name: "protocol conflict",
			listeners: []gatewayv1.Listener{
				{Name: "c1", Port: 8080, Protocol: gatewayv1.HTTPProtocolType, Hostname: hostnamePtr("a.example.com")},
				{Name: "c2", Port: 8080, Protocol: gatewayv1.HTTPSProtocolType, Hostname: hostnamePtr("a.example.com")},
				{Name: "ok", Port: 80, Protocol: gatewayv1.HTTPProtocolType, Hostname: hostnamePtr("b.example.com")},
			},
			reason: gatewayv1.ListenerReasonProtocolConflict,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			gw := &gatewayv1.Gateway{
				ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "infra"},
				Spec:       gatewayv1.GatewaySpec{Listeners: tt.listeners},
			}
			ls := newListenerSet("ls", "infra", "2026-01-01T00:00:00Z", []gatewayv1.ListenerEntry{
				{Name: "ls-l1", Port: tt.listeners[0].Port, Protocol: tt.listeners[0].Protocol, Hostname: tt.listeners[0].Hostname},
			})

			res := listenermerge.Merge(gw, []*gatewayv1.ListenerSet{ls})
			require.Len(t, res.Listeners, 4)

			assert.Equal(t, tt.reason, res.Listeners[0].ConflictReason, "no winner: the first of the pair is refused too")
			assert.Equal(t, tt.reason, res.Listeners[1].ConflictReason)
			assert.Empty(t, res.Listeners[2].ConflictReason, "a distinct listener stays valid")
			assert.NotEmpty(t, res.Listeners[3].ConflictReason, "a ListenerSet entry still loses to the Gateway")
		})
	}
}

// TestMerge_GatewayOwnProtocolConflict_AllRefused pins that no Gateway-owned
// listener wins a protocol conflict: every one of them on a port that mixes
// protocols is refused, including one whose protocol matches the first.
func TestMerge_GatewayOwnProtocolConflict_AllRefused(t *testing.T) {
	t.Parallel()

	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "infra"},
		Spec: gatewayv1.GatewaySpec{Listeners: []gatewayv1.Listener{
			{Name: "a", Port: 8080, Protocol: gatewayv1.HTTPProtocolType, Hostname: hostnamePtr("a.example.com")},
			{Name: "b", Port: 8080, Protocol: gatewayv1.HTTPSProtocolType, Hostname: hostnamePtr("b.example.com")},
			{Name: "d", Port: 8080, Protocol: gatewayv1.HTTPProtocolType, Hostname: hostnamePtr("d.example.com")},
		}},
	}

	res := listenermerge.Merge(gw, nil)
	require.Len(t, res.Listeners, 3)

	for _, listener := range res.Listeners {
		assert.Equal(t, gatewayv1.ListenerReasonProtocolConflict, listener.ConflictReason, "listener %s", listener.Name)
	}
}

// TestMerge_ListenerSetEntryMeetsEveryGatewayProtocol pins that a ListenerSet
// entry is checked against every protocol the Gateway uses on its port, not
// only the first: with HTTP and HTTPS Gateway listeners on one port, an HTTP
// entry there conflicts too. The Gateway still takes precedence over the
// entry, and a later ListenerSet still loses to an earlier one.
func TestMerge_ListenerSetEntryMeetsEveryGatewayProtocol(t *testing.T) {
	t.Parallel()

	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "infra"},
		Spec: gatewayv1.GatewaySpec{Listeners: []gatewayv1.Listener{
			{Name: "a", Port: 80, Protocol: gatewayv1.HTTPProtocolType, Hostname: hostnamePtr("a.example.com")},
			{Name: "b", Port: 80, Protocol: gatewayv1.HTTPSProtocolType, Hostname: hostnamePtr("b.example.com")},
		}},
	}
	older := newListenerSet("older", "infra", "2026-01-01T00:00:00Z", []gatewayv1.ListenerEntry{
		{Name: "c", Port: 80, Protocol: gatewayv1.HTTPProtocolType, Hostname: hostnamePtr("c.example.com")},
		{Name: "x", Port: 81, Protocol: gatewayv1.HTTPProtocolType},
	})
	newer := newListenerSet("newer", "infra", "2026-06-01T00:00:00Z", []gatewayv1.ListenerEntry{
		{Name: "y", Port: 81, Protocol: gatewayv1.HTTPSProtocolType},
	})

	res := listenermerge.Merge(gw, []*gatewayv1.ListenerSet{older, newer})
	require.Len(t, res.Listeners, 5)

	reasons := map[gatewayv1.SectionName]gatewayv1.ListenerConditionReason{}
	for _, listener := range res.Listeners {
		reasons[listener.Name] = listener.ConflictReason
	}

	assert.Equal(t, map[gatewayv1.SectionName]gatewayv1.ListenerConditionReason{
		"a": gatewayv1.ListenerReasonProtocolConflict,
		"b": gatewayv1.ListenerReasonProtocolConflict,
		"c": gatewayv1.ListenerReasonProtocolConflict,
		"x": "",
		"y": gatewayv1.ListenerReasonProtocolConflict,
	}, reasons)
}
