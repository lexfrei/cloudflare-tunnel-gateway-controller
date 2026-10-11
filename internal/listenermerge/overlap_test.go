package listenermerge_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/listenermerge"
)

func tlsListener(name string, port gatewayv1.PortNumber, protocol gatewayv1.ProtocolType, hostname string) gatewayv1.Listener {
	listener := gatewayv1.Listener{Name: gatewayv1.SectionName(name), Port: port, Protocol: protocol}
	if hostname != "" {
		listener.Hostname = new(gatewayv1.Hostname(hostname))
	}

	return listener
}

func overlapping(res *listenermerge.MergeResult) map[gatewayv1.SectionName]bool {
	out := make(map[gatewayv1.SectionName]bool)

	for i := range res.Listeners {
		if res.Listeners[i].OverlappingTLS {
			out[res.Listeners[i].Name] = true
		}
	}

	return out
}

// TestMerge_OverlappingTLSHostnames pins ListenerConditionOverlappingTLSConfig
// detection: every HTTPS listener whose hostname can match a host another
// HTTPS listener on the same port matches is flagged, and the rest are not.
func TestMerge_OverlappingTLSHostnames(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		listeners []gatewayv1.Listener
		want      []gatewayv1.SectionName
	}{
		{
			name: "the spec's example",
			listeners: []gatewayv1.Listener{
				tlsListener("a", 443, gatewayv1.HTTPSProtocolType, "foo.example.com"),
				tlsListener("b", 443, gatewayv1.HTTPSProtocolType, "foo.example.org"),
				tlsListener("c", 443, gatewayv1.HTTPSProtocolType, "*.example.com"),
			},
			want: []gatewayv1.SectionName{"a", "c"},
		},
		{
			name: "listeners on different ports do not overlap",
			listeners: []gatewayv1.Listener{
				tlsListener("a", 443, gatewayv1.HTTPSProtocolType, "*.example.com"),
				tlsListener("b", 8443, gatewayv1.HTTPSProtocolType, "foo.example.com"),
			},
		},
		{
			name: "a listener without hostname matches every host",
			listeners: []gatewayv1.Listener{
				tlsListener("a", 443, gatewayv1.HTTPSProtocolType, ""),
				tlsListener("b", 443, gatewayv1.HTTPSProtocolType, "foo.example.com"),
			},
			want: []gatewayv1.SectionName{"a", "b"},
		},
		{
			name: "a conflicted listener admits nothing and overlaps nothing",
			listeners: []gatewayv1.Listener{
				tlsListener("a", 443, gatewayv1.HTTPSProtocolType, "foo.example.com"),
				tlsListener("b", 443, gatewayv1.HTTPSProtocolType, "foo.example.com"),
				tlsListener("c", 443, gatewayv1.HTTPSProtocolType, "*.example.com"),
			},
		},
		{
			name: "a TLS listener is not served here and takes no part",
			listeners: []gatewayv1.Listener{
				tlsListener("a", 443, gatewayv1.HTTPSProtocolType, "*.example.com"),
				tlsListener("b", 443, gatewayv1.TLSProtocolType, "foo.example.com"),
			},
		},
		{
			name: "nested wildcards",
			listeners: []gatewayv1.Listener{
				tlsListener("a", 443, gatewayv1.HTTPSProtocolType, "*.example.com"),
				tlsListener("b", 443, gatewayv1.HTTPSProtocolType, "*.foo.example.com"),
			},
			want: []gatewayv1.SectionName{"a", "b"},
		},
		{
			name: "plain HTTP takes no part",
			listeners: []gatewayv1.Listener{
				tlsListener("a", 443, gatewayv1.HTTPSProtocolType, "foo.example.com"),
				tlsListener("b", 80, gatewayv1.HTTPProtocolType, "foo.example.com"),
			},
		},
		{
			name: "a wildcard does not match its own apex",
			listeners: []gatewayv1.Listener{
				tlsListener("a", 443, gatewayv1.HTTPSProtocolType, "example.com"),
				tlsListener("b", 443, gatewayv1.HTTPSProtocolType, "*.example.com"),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			gw := &gatewayv1.Gateway{
				ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "infra"},
				Spec:       gatewayv1.GatewaySpec{Listeners: tt.listeners},
			}

			got := overlapping(listenermerge.Merge(gw, nil))

			assert.Len(t, got, len(tt.want))

			for _, name := range tt.want {
				assert.True(t, got[name], "listener %s overlaps", name)
			}
		})
	}
}

// TestMerge_OverlappingTLSAcrossListenerSet pins that a ListenerSet entry is
// compared with the Gateway's own listeners it is merged with.
func TestMerge_OverlappingTLSAcrossListenerSet(t *testing.T) {
	t.Parallel()

	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "infra"},
		Spec: gatewayv1.GatewaySpec{Listeners: []gatewayv1.Listener{
			tlsListener("wild", 443, gatewayv1.HTTPSProtocolType, "*.example.com"),
		}},
	}
	set := newListenerSet("team", "infra", "2026-01-01T00:00:00Z", []gatewayv1.ListenerEntry{
		{Name: "app", Port: 443, Protocol: gatewayv1.HTTPSProtocolType, Hostname: new(gatewayv1.Hostname("app.example.com"))},
	})

	res := listenermerge.Merge(gw, []*gatewayv1.ListenerSet{set})

	require.Len(t, res.Listeners, 2)
	assert.True(t, res.Listeners[0].OverlappingTLS)
	assert.True(t, res.Listeners[1].OverlappingTLS)
}
