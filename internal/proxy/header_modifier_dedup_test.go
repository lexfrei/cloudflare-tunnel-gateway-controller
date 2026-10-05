package proxy_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
)

// TestConvertHTTPRoutes_HeaderModifierCaseVariants_FirstWins pins the HTTPHeader
// Name godoc: names are case-insensitive, the first entry with an equivalent
// name is used and later ones are ignored, in both set and add.
func TestConvertHTTPRoutes_HeaderModifierCaseVariants_FirstWins(t *testing.T) {
	t.Parallel()

	modifier := &gatewayv1.HTTPHeaderFilter{
		Set: []gatewayv1.HTTPHeader{{Name: "X-Env", Value: "first"}, {Name: "x-env", Value: "second"}},
		Add: []gatewayv1.HTTPHeader{{Name: "x-tag", Value: "first"}, {Name: "X-TAG", Value: "second"}},
	}
	rule := gatewayv1.HTTPRouteRule{
		Filters: []gatewayv1.HTTPRouteFilter{
			{Type: gatewayv1.HTTPRouteFilterRequestHeaderModifier, RequestHeaderModifier: modifier},
			{Type: gatewayv1.HTTPRouteFilterResponseHeaderModifier, ResponseHeaderModifier: modifier},
		},
		BackendRefs: []gatewayv1.HTTPBackendRef{backendRef("web-svc", 80, 1)},
	}

	cfg := proxy.ConvertHTTPRoutes(context.Background(), singleRuleRoute(rule), "cluster.local", nil, nil, nil, nil)
	require.Len(t, cfg.Rules, 1)
	require.Len(t, cfg.Rules[0].Filters, 2)

	for _, converted := range []*proxy.HeaderModifier{
		cfg.Rules[0].Filters[0].RequestHeaderModifier,
		cfg.Rules[0].Filters[1].ResponseHeaderModifier,
	} {
		resp := &http.Response{Header: http.Header{"X-Env": {"original"}}}
		proxy.NewResponseHeaderModifier(converted).ProcessResponse(resp)

		assert.Equal(t, []string{"first"}, resp.Header.Values("X-Env"))
		assert.Equal(t, []string{"first"}, resp.Header.Values("X-Tag"))
	}
}
