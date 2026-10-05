package proxy_test

import (
	"fmt"
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
		GatewayListeners: map[string][]proxy.Listener{isolationGateway: hostListeners("", "*.example.com")},
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"*.example.com"}, Matches: []proxy.RouteMatch{pathPrefixMatch("/")},
				Listeners: map[string][]proxy.Listener{isolationGateway: hostListeners("")},
			},
			{
				Hostnames: []string{"*.example.com"}, Matches: []proxy.RouteMatch{pathPrefixMatch("/")},
				Listeners: map[string][]proxy.Listener{isolationGateway: hostListeners("*.example.com")},
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

	attached := map[string][]proxy.Listener{isolationGateway: hostListeners("*.example.com")}
	cfg := &proxy.Config{
		GatewayListeners: map[string][]proxy.Listener{isolationGateway: hostListeners("", "*.example.com")},
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
				GatewayListeners: map[string][]proxy.Listener{isolationGateway: hostListeners(tc.listeners...)},
				Rules: []proxy.RouteRule{
					{
						Hostnames: tc.hostnames, Matches: []proxy.RouteMatch{pathPrefixMatch("/")},
						Listeners: map[string][]proxy.Listener{isolationGateway: hostListeners(tc.loser)},
					},
					{
						Hostnames: tc.hostnames, Matches: []proxy.RouteMatch{pathPrefixMatch("/")},
						Listeners: map[string][]proxy.Listener{isolationGateway: hostListeners(tc.owner)},
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

// TestDetectShadowedRules_ListenersOnDifferentPortsDoNotShadow pins that a
// rule is shadowed only when it loses on every port it is reached on: two
// routes with the same match on listeners of different ports both serve, a
// route with no listener data also serves ports no listener has, and a route
// on two ports that loses on one still serves the other.
func TestDetectShadowedRules_ListenersOnDifferentPortsDoNotShadow(t *testing.T) {
	t.Parallel()

	rule := func(listeners map[string][]proxy.Listener) proxy.RouteRule {
		return proxy.RouteRule{Matches: []proxy.RouteMatch{pathPrefixMatch("/")}, Listeners: listeners}
	}

	cases := map[string]struct {
		rules      []proxy.RouteRule
		wantLosers []string
	}{
		"one port each": {
			rules: []proxy.RouteRule{
				rule(map[string][]proxy.Listener{"infra/gw": {{Port: 80}}}),
				rule(map[string][]proxy.Listener{"infra/gw": {{Port: 443}}}),
			},
		},
		"no listener data": {
			rules: []proxy.RouteRule{
				rule(map[string][]proxy.Listener{"infra/gw": {{Port: 80}}}),
				rule(map[string][]proxy.Listener{"infra/gw": {{Port: 443}}}),
				rule(nil),
			},
		},
		"loses on one of two ports": {
			rules: []proxy.RouteRule{
				rule(map[string][]proxy.Listener{"infra/gw": {{Port: 80}}}),
				rule(map[string][]proxy.Listener{"infra/gw": {{Port: 80}, {Port: 443}}}),
			},
		},
		"loses on every port": {
			rules: []proxy.RouteRule{
				rule(map[string][]proxy.Listener{"infra/gw": {{Port: 80}, {Port: 443}}}),
				rule(map[string][]proxy.Listener{"infra/gw": {{Port: 80}, {Port: 443}}}),
			},
			wantLosers: []string{"r1"},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			cfg := &proxy.Config{
				GatewayListeners: map[string][]proxy.Listener{"infra/gw": {{Port: 80}, {Port: 443}}},
				Rules:            tc.rules,
			}

			for idx := range tc.rules {
				created := shadowT0
				if idx > 0 {
					created = shadowT1
				}

				cfg.Provenance = append(cfg.Provenance, prov("HTTPRoute", "team", fmt.Sprintf("r%d", idx), created, 0))
			}

			var losers []string
			for _, diag := range proxy.DetectShadowedRules(cfg) {
				losers = append(losers, diag.Name)
			}

			assert.Equal(t, tc.wantLosers, losers)
		})
	}
}
