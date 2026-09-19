package ingress_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/ingress"
)

// attributionRoute builds a route contributing len(hostnames) x matches
// entries to the tunnel document. A nil hostnames slice means the route
// declares none, which the adapter reports as the "*" wildcard.
func attributionRoute(namespace, name string, hostnames []gatewayv1.Hostname, matches int) gatewayv1.HTTPRoute {
	pathPrefix := gatewayv1.PathMatchPathPrefix

	pathMatches := make([]gatewayv1.HTTPRouteMatch, 0, matches)
	for i := range matches {
		pathMatches = append(pathMatches, gatewayv1.HTTPRouteMatch{
			Path: &gatewayv1.HTTPPathMatch{Type: &pathPrefix, Value: new("/p" + string(rune('a'+i)))},
		})
	}

	return gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: gatewayv1.HTTPRouteSpec{
			Hostnames: hostnames,
			Rules: []gatewayv1.HTTPRouteRule{
				{
					Matches:     pathMatches,
					BackendRefs: []gatewayv1.HTTPBackendRef{newHTTPBackendRef("my-service", nil, int32Ptr(8080))},
				},
			},
		},
	}
}

// TestBuild_RulesByNamespace pins the attribution the per-tunnel rule cap
// depends on. The document itself records nothing about where a rule came
// from, so when the cap is hit this map is the only thing that can say whose
// routes filled it.
func TestBuild_RulesByNamespace(t *testing.T) {
	t.Parallel()

	builder := ingress.NewBuilder("cluster.local", nil, nil, nil, nil)
	routes := []gatewayv1.HTTPRoute{
		attributionRoute("team-a", "first", []gatewayv1.Hostname{"a1.example.com", "a2.example.com"}, 3),
		attributionRoute("team-a", "second", []gatewayv1.Hostname{"a3.example.com"}, 1),
		attributionRoute("team-b", "only", []gatewayv1.Hostname{"b.example.com"}, 1),
	}

	result := builder.Build(context.Background(), routes)

	assert.Equal(t, map[string]int{"team-a": 7, "team-b": 1}, result.RulesByNamespace,
		"a namespace's share is the sum over its routes of hostnames x matches")

	// 7 + 1 rules plus the catch-all the builder always appends.
	assert.Len(t, result.Rules, 9, "the attribution accounts for every rule in the document")
}

// TestBuild_RulesByNamespaceExcludesWildcardHostnames covers the one way an
// entry does not become a rule: a route that declares no hostnames projects
// the "*" wildcard, which entriesToIngressRules drops because the proxy
// matches it instead. Counting those would blame a namespace for rules that
// never enter the document — and could report a namespace as the largest
// consumer of a budget it does not touch.
func TestBuild_RulesByNamespaceExcludesWildcardHostnames(t *testing.T) {
	t.Parallel()

	builder := ingress.NewBuilder("cluster.local", nil, nil, nil, nil)
	routes := []gatewayv1.HTTPRoute{
		attributionRoute("team-wildcard", "catch-all", nil, 4),
		attributionRoute("team-real", "pinned", []gatewayv1.Hostname{"real.example.com"}, 2),
	}

	result := builder.Build(context.Background(), routes)

	assert.Equal(t, map[string]int{"team-wildcard": 0, "team-real": 2}, result.RulesByNamespace)
	assert.Len(t, result.Rules, 3, "only the pinned hostname's rules reach the document, plus the catch-all")
}
