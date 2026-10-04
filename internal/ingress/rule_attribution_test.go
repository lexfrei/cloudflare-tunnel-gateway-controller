package ingress_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/ingress"
)

// attributionRoute builds a route with one rule of the given path matches,
// all exact or prefix, backed by service. A nil hostnames slice means the
// route declares none, which the adapter reports as the "*" wildcard.
func attributionRoute(namespace, name, service string, hostnames []gatewayv1.Hostname, matches int) gatewayv1.HTTPRoute {
	pathPrefix := gatewayv1.PathMatchPathPrefix
	pathExact := gatewayv1.PathMatchExact

	pathMatches := make([]gatewayv1.HTTPRouteMatch, 0, matches)
	for i := range matches {
		matchType := &pathPrefix
		if i%2 == 1 {
			matchType = &pathExact
		}

		pathMatches = append(pathMatches, gatewayv1.HTTPRouteMatch{
			Path: &gatewayv1.HTTPPathMatch{Type: matchType, Value: new("/p" + string(rune('a'+i)))},
		})
	}

	return gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: gatewayv1.HTTPRouteSpec{
			Hostnames: hostnames,
			Rules: []gatewayv1.HTTPRouteRule{
				{
					Matches:     pathMatches,
					BackendRefs: []gatewayv1.HTTPBackendRef{newHTTPBackendRef(service, nil, int32Ptr(8080))},
				},
			},
		},
	}
}

// TestBuild_OneRulePerHostname pins the document's shape: the in-process
// proxy does all path matching, so the tunnel document lists each hostname
// once, without a path, however many rules and matches serve it.
func TestBuild_OneRulePerHostname(t *testing.T) {
	t.Parallel()

	builder := ingress.NewBuilder("cluster.local", nil, nil, nil, nil)
	routes := []gatewayv1.HTTPRoute{
		attributionRoute("team-a", "first", "svc-a", []gatewayv1.Hostname{"a1.example.com", "a2.example.com"}, 5),
		attributionRoute("team-a", "second", "svc-a", []gatewayv1.Hostname{"a1.example.com"}, 3),
	}

	result := builder.Build(context.Background(), routes)

	require.Len(t, result.Rules, 3, "two hostnames plus the catch-all")

	for _, rule := range result.Rules {
		assert.False(t, rule.Path.Present, "no rule carries a path")
	}

	assert.Equal(t, "a1.example.com", result.Rules[0].Hostname.Value)
	assert.Equal(t, "a2.example.com", result.Rules[1].Hostname.Value)
	assert.Equal(t, ingress.CatchAllService, result.Rules[2].Service.Value)
}

// TestBuild_SharedHostnameService pins which backend a hostname's rule names
// when several routes serve it: the lexicographically smallest URL, so the
// document does not change with the order routes are listed in.
func TestBuild_SharedHostnameService(t *testing.T) {
	t.Parallel()

	shared := []gatewayv1.Hostname{"shared.example.com"}
	zeta := attributionRoute("team-z", "zeta", "zeta", shared, 1)
	alpha := attributionRoute("team-a", "alpha", "alpha", shared, 1)

	builder := ingress.NewBuilder("cluster.local", nil, nil, nil, nil)

	for _, routes := range [][]gatewayv1.HTTPRoute{{zeta, alpha}, {alpha, zeta}} {
		result := builder.Build(context.Background(), routes)

		require.Len(t, result.Rules, 2)
		assert.Equal(t, "shared.example.com", result.Rules[0].Hostname.Value)
		assert.Equal(t, "http://alpha.team-a.svc.cluster.local:8080", result.Rules[0].Service.Value)
	}
}

// TestBuild_HostnamesByNamespace pins the attribution the per-tunnel rule cap
// depends on. The document records nothing about where a rule came from, so
// when the cap is hit this is the only thing that can say whose routes filled
// it. A hostname several namespaces serve is attributed to each of them.
func TestBuild_HostnamesByNamespace(t *testing.T) {
	t.Parallel()

	builder := ingress.NewBuilder("cluster.local", nil, nil, nil, nil)
	routes := []gatewayv1.HTTPRoute{
		attributionRoute("team-a", "first", "svc", []gatewayv1.Hostname{"a1.example.com", "shared.example.com"}, 3),
		attributionRoute("team-a", "second", "svc", []gatewayv1.Hostname{"a1.example.com"}, 1),
		attributionRoute("team-b", "only", "svc", []gatewayv1.Hostname{"shared.example.com"}, 1),
		attributionRoute("team-wildcard", "catch-all", "svc", nil, 4),
	}

	result := builder.Build(context.Background(), routes)

	assert.Equal(t, map[string][]string{
		"team-a": {"a1.example.com", "shared.example.com"},
		"team-b": {"shared.example.com"},
	}, result.HostnamesByNamespace,
		"a namespace is attributed the distinct hostnames its routes put in the document; "+
			"a wildcard-only namespace puts none there")

	assert.Len(t, result.Rules, 3, "two distinct hostnames plus the catch-all")
}
