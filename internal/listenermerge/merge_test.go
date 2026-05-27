package listenermerge_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/listenermerge"
)

func TestMerge_PrecedenceOrdering(t *testing.T) {
	t.Parallel()

	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "infra"},
		Spec: gatewayv1.GatewaySpec{
			Listeners: []gatewayv1.Listener{
				{Name: "gw-l1", Port: 80, Protocol: gatewayv1.HTTPProtocolType},
			},
		},
	}
	older := newListenerSet("older", "infra", "2026-01-01T00:00:00Z", []gatewayv1.ListenerEntry{
		{Name: "older-l1", Port: 81, Protocol: gatewayv1.HTTPProtocolType},
	})
	newer := newListenerSet("newer", "infra", "2026-06-01T00:00:00Z", []gatewayv1.ListenerEntry{
		{Name: "newer-l1", Port: 82, Protocol: gatewayv1.HTTPProtocolType},
	})

	// Pass newer first to verify Merge sorts by creation time.
	res := listenermerge.Merge(gw, []*gatewayv1.ListenerSet{newer, older})

	require.Len(t, res.Listeners, 3)

	assert.Equal(t, listenermerge.ParentKindGateway, res.Listeners[0].ParentKind)
	assert.Equal(t, gatewayv1.SectionName("gw-l1"), res.Listeners[0].Name)

	assert.Equal(t, listenermerge.ParentKindListenerSet, res.Listeners[1].ParentKind)
	assert.Equal(t, "older", res.Listeners[1].ListenerSet.Name)

	assert.Equal(t, "newer", res.Listeners[2].ListenerSet.Name)
}

func TestMerge_PrecedenceOrdering_SameCreationTime_AlphabeticalNamespaceName(t *testing.T) {
	t.Parallel()

	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "infra"},
	}
	tsame := "2026-01-01T00:00:00Z"
	lsAlpha := newListenerSet("alpha", "ns1", tsame, []gatewayv1.ListenerEntry{
		{Name: "a", Port: 80, Protocol: gatewayv1.HTTPProtocolType},
	})
	lsBeta := newListenerSet("beta", "ns1", tsame, []gatewayv1.ListenerEntry{
		{Name: "b", Port: 80, Protocol: gatewayv1.HTTPProtocolType, Hostname: hostnamePtr("beta.example.com")},
	})

	res := listenermerge.Merge(gw, []*gatewayv1.ListenerSet{lsBeta, lsAlpha})

	require.Len(t, res.Listeners, 2)
	assert.Equal(t, "alpha", res.Listeners[0].ListenerSet.Name)
	assert.Equal(t, "beta", res.Listeners[1].ListenerSet.Name)
}

func TestMerge_HostnameConflict_LowerPrecedenceMarked(t *testing.T) {
	t.Parallel()

	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "infra"},
		Spec: gatewayv1.GatewaySpec{
			Listeners: []gatewayv1.Listener{
				{
					Name:     "gw-l1",
					Port:     80,
					Protocol: gatewayv1.HTTPProtocolType,
					Hostname: hostnamePtr("conflict.example.com"),
				},
			},
		},
	}
	ls := newListenerSet("ls1", "infra", "2026-01-01T00:00:00Z", []gatewayv1.ListenerEntry{
		{
			Name:     "ls-l1",
			Port:     80,
			Protocol: gatewayv1.HTTPProtocolType,
			Hostname: hostnamePtr("conflict.example.com"),
		},
	})

	res := listenermerge.Merge(gw, []*gatewayv1.ListenerSet{ls})

	require.Len(t, res.Listeners, 2)
	assert.Empty(t, res.Listeners[0].ConflictReason, "Gateway listener wins")
	assert.Equal(t, gatewayv1.ListenerReasonHostnameConflict, res.Listeners[1].ConflictReason)
}

func TestMerge_ProtocolConflict_DifferentProtocolsSamePort(t *testing.T) {
	t.Parallel()

	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "infra"},
		Spec: gatewayv1.GatewaySpec{
			Listeners: []gatewayv1.Listener{
				{
					Name:     "gw-l1",
					Port:     80,
					Protocol: gatewayv1.HTTPProtocolType,
					Hostname: hostnamePtr("a.example.com"),
				},
			},
		},
	}
	ls := newListenerSet("ls1", "infra", "2026-01-01T00:00:00Z", []gatewayv1.ListenerEntry{
		{
			Name:     "ls-l1",
			Port:     80,
			Protocol: gatewayv1.TCPProtocolType,
		},
	})

	res := listenermerge.Merge(gw, []*gatewayv1.ListenerSet{ls})

	require.Len(t, res.Listeners, 2)
	assert.Empty(t, res.Listeners[0].ConflictReason)
	assert.Equal(t, gatewayv1.ListenerReasonProtocolConflict, res.Listeners[1].ConflictReason)
}

func TestMerge_NoConflict_SamePortSameProtocolDifferentHostnames(t *testing.T) {
	t.Parallel()

	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "infra"},
		Spec: gatewayv1.GatewaySpec{
			Listeners: []gatewayv1.Listener{
				{
					Name:     "gw-a",
					Port:     80,
					Protocol: gatewayv1.HTTPProtocolType,
					Hostname: hostnamePtr("a.example.com"),
				},
				{
					Name:     "gw-b",
					Port:     80,
					Protocol: gatewayv1.HTTPProtocolType,
					Hostname: hostnamePtr("b.example.com"),
				},
			},
		},
	}

	res := listenermerge.Merge(gw, nil)

	require.Len(t, res.Listeners, 2)
	assert.Empty(t, res.Listeners[0].ConflictReason)
	assert.Empty(t, res.Listeners[1].ConflictReason)
}

func newListenerSet(name, namespace, creationTime string, entries []gatewayv1.ListenerEntry) *gatewayv1.ListenerSet {
	ts, err := time.Parse(time.RFC3339, creationTime)
	if err != nil {
		panic(err)
	}

	return &gatewayv1.ListenerSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         namespace,
			CreationTimestamp: metav1.NewTime(ts),
		},
		Spec: gatewayv1.ListenerSetSpec{
			Listeners: entries,
		},
	}
}

func hostnamePtr(h string) *gatewayv1.Hostname {
	hostname := gatewayv1.Hostname(h)

	return &hostname
}
