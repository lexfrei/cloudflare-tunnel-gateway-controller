package proxy_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
)

// TestConvertHTTPRoutes_KnativeEndpointProbeRule_PreservesOverrideMatchAndConcreteSet
// locks the contract the in-cluster probe ack depends on: a net-gateway-api
// endpoint-probe rule (header-match K-Network-Hash: override + a
// RequestHeaderModifier that SETS K-Network-Hash to the concrete, ep-/tr-
// prefixed version) must convert to a RouteRule whose Matches carry the override
// HeaderMatch and whose Filters carry the concrete-hash Set verbatim (prefix
// preserved). A converter refactor that drops or rewrites either side would
// silently break the probe; this fails loudly instead.
func TestConvertHTTPRoutes_KnativeEndpointProbeRule_PreservesOverrideMatchAndConcreteSet(t *testing.T) {
	t.Parallel()

	const concreteHash = "ep-4d38b8fcfb28f82a830f349e6073eea79af03222262be5785b2a9216d39f63d9"

	pathPrefix := gatewayv1.PathMatchPathPrefix
	headerExact := gatewayv1.HeaderMatchExact

	routes := []*gatewayv1.HTTPRoute{{
		ObjectMeta: metav1.ObjectMeta{Name: "abcd.foobar76.example.com", Namespace: "default"},
		Spec: gatewayv1.HTTPRouteSpec{
			Hostnames: []gatewayv1.Hostname{"abcd.foobar76.example.com"},
			Rules: []gatewayv1.HTTPRouteRule{{
				Matches: []gatewayv1.HTTPRouteMatch{{
					Path: &gatewayv1.HTTPPathMatch{
						Type:  &pathPrefix,
						Value: new("/.well-known/knative/revision/default/static-sws-site77"),
					},
					Headers: []gatewayv1.HTTPHeaderMatch{{
						Type:  &headerExact,
						Name:  "K-Network-Hash",
						Value: "override",
					}},
				}},
				Filters: []gatewayv1.HTTPRouteFilter{{
					Type: gatewayv1.HTTPRouteFilterRequestHeaderModifier,
					RequestHeaderModifier: &gatewayv1.HTTPHeaderFilter{
						Set: []gatewayv1.HTTPHeader{{Name: "K-Network-Hash", Value: concreteHash}},
					},
				}},
				BackendRefs: []gatewayv1.HTTPBackendRef{backendRef("static-sws-site77", 80, 100)},
			}},
		},
	}}

	cfg := proxy.ConvertHTTPRoutes(context.Background(), routes, "cluster.local", nil, nil, nil, nil)

	require.Len(t, cfg.Rules, 1)
	rule := cfg.Rules[0]

	// The override header match must survive conversion (the prober keys on it).
	require.NotEmpty(t, rule.Matches)
	var foundMatch bool
	for _, match := range rule.Matches {
		for _, header := range match.Headers {
			if header.Name == "K-Network-Hash" && header.Value == "override" {
				foundMatch = true
			}
		}
	}
	assert.True(t, foundMatch, "the override K-Network-Hash header match must be preserved")

	// The concrete-hash Set filter must survive verbatim, prefix intact, so the
	// handler echoes exactly what the prober's version check expects.
	var gotHash string
	for _, filter := range rule.Filters {
		if filter.Type != proxy.FilterRequestHeaderModifier || filter.RequestHeaderModifier == nil {
			continue
		}
		for _, set := range filter.RequestHeaderModifier.Set {
			if set.Name == "K-Network-Hash" {
				gotHash = set.Value
			}
		}
	}
	assert.Equal(t, concreteHash, gotHash,
		"the ep-/tr- prefixed K-Network-Hash Set filter must be preserved verbatim on the rule")
}
