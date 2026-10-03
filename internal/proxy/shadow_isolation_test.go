package proxy_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
)

// TestDetectShadowedRules_IsolatedClaimDoesNotShadow pins that a route whose
// claim on a hostname is removed by listener isolation does not shadow the
// route attached to the listener that owns the hostname: the older route on
// the hostname-less listener never answers these hosts.
func TestDetectShadowedRules_IsolatedClaimDoesNotShadow(t *testing.T) {
	t.Parallel()

	cfg := &proxy.Config{
		ListenerHostnames: map[string][]string{isolationGateway: {"", "*.example.com"}},
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"*.example.com"}, Matches: []proxy.RouteMatch{pathPrefixMatch("/")},
				Listeners: map[string][]string{isolationGateway: {""}},
			},
			{
				Hostnames: []string{"*.example.com"}, Matches: []proxy.RouteMatch{pathPrefixMatch("/")},
				Listeners: map[string][]string{isolationGateway: {"*.example.com"}},
			},
		},
		Provenance: []proxy.RuleProvenance{
			prov("HTTPRoute", "team-a", "on-empty", shadowT0, 0),
			prov("HTTPRoute", "team-b", "on-wildcard", shadowT1, 0),
		},
	}

	assert.Empty(t, proxy.DetectShadowedRules(cfg))
}

// TestDetectShadowedRules_SameListenerStillShadows pins that two routes on
// the same listener keep colliding as before.
func TestDetectShadowedRules_SameListenerStillShadows(t *testing.T) {
	t.Parallel()

	attached := map[string][]string{isolationGateway: {"*.example.com"}}
	cfg := &proxy.Config{
		ListenerHostnames: map[string][]string{isolationGateway: {"", "*.example.com"}},
		Rules: []proxy.RouteRule{
			{Hostnames: []string{"*.example.com"}, Matches: []proxy.RouteMatch{pathPrefixMatch("/")}, Listeners: attached},
			{Hostnames: []string{"*.example.com"}, Matches: []proxy.RouteMatch{pathPrefixMatch("/")}, Listeners: attached},
		},
		Provenance: []proxy.RuleProvenance{
			prov("HTTPRoute", "team-a", "first", shadowT0, 0),
			prov("HTTPRoute", "team-b", "second", shadowT1, 0),
		},
	}

	diags := proxy.DetectShadowedRules(cfg)
	require.Len(t, diags, 1)
	assert.Equal(t, "second", diags[0].Name)
}

// TestDetectShadowedRules_IsolationAppliesToEveryKeyKind covers the exact and
// default-bucket keys: the older route's claim is removed by isolation in
// both, so the route on the owning listener is not reported.
func TestDetectShadowedRules_IsolationAppliesToEveryKeyKind(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		hostnames []string
		listeners []string
		loser     string
		owner     string
	}{
		"exact hostname": {
			hostnames: []string{"foo.example.com"},
			listeners: []string{"*.example.com", "foo.example.com"},
			loser:     "*.example.com", owner: "foo.example.com",
		},
		"default bucket": {
			listeners: []string{"", "*.example.com"},
			loser:     "*.example.com", owner: "",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			cfg := &proxy.Config{
				ListenerHostnames: map[string][]string{isolationGateway: tc.listeners},
				Rules: []proxy.RouteRule{
					{
						Hostnames: tc.hostnames, Matches: []proxy.RouteMatch{pathPrefixMatch("/")},
						Listeners: map[string][]string{isolationGateway: {tc.loser}},
					},
					{
						Hostnames: tc.hostnames, Matches: []proxy.RouteMatch{pathPrefixMatch("/")},
						Listeners: map[string][]string{isolationGateway: {tc.owner}},
					},
				},
				Provenance: []proxy.RuleProvenance{
					prov("HTTPRoute", "team-a", "isolated", shadowT0, 0),
					prov("HTTPRoute", "team-b", "owner", shadowT1, 0),
				},
			}

			assert.Empty(t, proxy.DetectShadowedRules(cfg))
		})
	}
}
