package proxy_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
)

func singleRuleRoute(rule gatewayv1.HTTPRouteRule) []*gatewayv1.HTTPRoute {
	return []*gatewayv1.HTTPRoute{{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
		Spec: gatewayv1.HTTPRouteSpec{
			Hostnames: []gatewayv1.Hostname{"example.com"},
			Rules:     []gatewayv1.HTTPRouteRule{rule},
		},
	}}
}

func redirectFilter() gatewayv1.HTTPRouteFilter {
	return gatewayv1.HTTPRouteFilter{
		Type:            gatewayv1.HTTPRouteFilterRequestRedirect,
		RequestRedirect: &gatewayv1.HTTPRequestRedirectFilter{StatusCode: new(301)},
	}
}

func rewriteFilter() gatewayv1.HTTPRouteFilter {
	return gatewayv1.HTTPRouteFilter{
		Type:       gatewayv1.HTTPRouteFilterURLRewrite,
		URLRewrite: &gatewayv1.HTTPURLRewriteFilter{Hostname: new(gatewayv1.PreciseHostname("other.example.com"))},
	}
}

func backendWithFilters(name string, filters ...gatewayv1.HTTPRouteFilter) gatewayv1.HTTPBackendRef {
	ref := backendRef(name, 80, 1)
	ref.Filters = filters

	return ref
}

// TestConvertHTTPRoutes_RedirectWithRewriteAcrossLevels_RefusesRule pins that a
// RequestRedirect and a URLRewrite anywhere on the same rule (rule level or any
// backendRef) are refused: the CRD's CEL checks each filter list on its own, so
// these cross-level pairs reach the controller.
func TestConvertHTTPRoutes_RedirectWithRewriteAcrossLevels_RefusesRule(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		rule gatewayv1.HTTPRouteRule
	}{
		{
			name: "rule rewrite, backend redirect",
			rule: gatewayv1.HTTPRouteRule{
				Filters:     []gatewayv1.HTTPRouteFilter{rewriteFilter()},
				BackendRefs: []gatewayv1.HTTPBackendRef{backendWithFilters("web-svc", redirectFilter())},
			},
		},
		{
			name: "redirect and rewrite on sibling backends",
			rule: gatewayv1.HTTPRouteRule{
				BackendRefs: []gatewayv1.HTTPBackendRef{
					backendWithFilters("a-svc", redirectFilter()),
					backendWithFilters("b-svc", rewriteFilter()),
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := proxy.ConvertHTTPRoutes(context.Background(), singleRuleRoute(tt.rule), "cluster.local", nil, nil, nil, nil)

			require.Len(t, cfg.Rules, 1)
			assert.Equal(t, http.StatusInternalServerError, cfg.Rules[0].UnavailableStatus)

			require.Len(t, cfg.Diagnostics, 1)
			diag := cfg.Diagnostics[0]
			assert.Equal(t, proxy.DiagnosticAccepted, diag.Target)
			assert.Equal(t, string(gatewayv1.RouteReasonIncompatibleFilters), diag.Reason)
			assert.True(t, diag.WholeRule)
			assert.Contains(t, diag.Message, "RequestRedirect")
			assert.Contains(t, diag.Message, "URLRewrite")
		})
	}
}

// TestConvertHTTPRoutes_SingleFilterKind_Serves is the control for the
// refusal above: either filter on its own stays servable.
func TestConvertHTTPRoutes_SingleFilterKind_Serves(t *testing.T) {
	t.Parallel()

	rule := gatewayv1.HTTPRouteRule{
		Filters:     []gatewayv1.HTTPRouteFilter{rewriteFilter()},
		BackendRefs: []gatewayv1.HTTPBackendRef{backendWithFilters("web-svc", rewriteFilter())},
	}

	cfg := proxy.ConvertHTTPRoutes(context.Background(), singleRuleRoute(rule), "cluster.local", nil, nil, nil, nil)

	require.Len(t, cfg.Rules, 1)
	assert.Zero(t, cfg.Rules[0].UnavailableStatus)
	assert.Empty(t, cfg.Diagnostics)

	rule = gatewayv1.HTTPRouteRule{BackendRefs: []gatewayv1.HTTPBackendRef{backendWithFilters("web-svc", redirectFilter())}}
	cfg = proxy.ConvertHTTPRoutes(context.Background(), singleRuleRoute(rule), "cluster.local", nil, nil, nil, nil)

	require.Len(t, cfg.Rules, 1)
	assert.Zero(t, cfg.Rules[0].UnavailableStatus)
	assert.Empty(t, cfg.Diagnostics)
}
