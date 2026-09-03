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

// TestConvertHTTPRoutes_UncompilableRegex_DropsRuleWithDiagnostic pins that a
// match pattern the data plane cannot compile is caught here rather than pushed
// out. A pattern reaching the proxy uncompilable costs its own rule and shows up
// only in a proxy log, which the tenant who wrote it cannot read; caught here it
// lands on their route status instead. WholeRule is true because a rule whose
// match never compiles cannot serve anything.
func TestConvertHTTPRoutes_UncompilableRegex_DropsRuleWithDiagnostic(t *testing.T) {
	t.Parallel()

	regularExpression := gatewayv1.PathMatchRegularExpression
	pathPrefix := gatewayv1.PathMatchPathPrefix

	routes := []*gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						Matches: []gatewayv1.HTTPRouteMatch{
							{Path: &gatewayv1.HTTPPathMatch{Type: &regularExpression, Value: new("[")}},
						},
						BackendRefs: []gatewayv1.HTTPBackendRef{backendRef("web-svc", 80, 1)},
					},
					{
						Matches: []gatewayv1.HTTPRouteMatch{
							{Path: &gatewayv1.HTTPPathMatch{Type: &pathPrefix, Value: new("/ok")}},
						},
						BackendRefs: []gatewayv1.HTTPBackendRef{backendRef("web-svc", 80, 1)},
					},
				},
			},
		},
	}

	cfg := proxy.ConvertHTTPRoutes(context.Background(), routes, "cluster.local", nil, nil, nil, nil)

	require.Len(t, cfg.Rules, 1, "the rule carrying the uncompilable pattern must not be pushed")
	require.Len(t, cfg.Rules[0].Matches, 1)
	require.NotNil(t, cfg.Rules[0].Matches[0].Path)
	assert.Equal(t, "/ok", cfg.Rules[0].Matches[0].Path.Value,
		"the surviving rule must be the one that compiles")

	require.Len(t, cfg.Provenance, len(cfg.Rules),
		"provenance is indexed by rule, so dropping a rule must drop its entry")

	require.Len(t, cfg.Diagnostics, 1)
	diag := cfg.Diagnostics[0]
	assert.Equal(t, "web", diag.Name)
	assert.Equal(t, 0, diag.RuleIndex, "the diagnostic must name the rule as the tenant wrote it")
	assert.Equal(t, proxy.DiagnosticAccepted, diag.Target)
	assert.Equal(t, string(gatewayv1.RouteReasonUnsupportedValue), diag.Reason)
	assert.True(t, diag.WholeRule, "a rule whose match never compiles cannot serve")
}

// TestConvertHTTPRoutes_AllRulesUncompilable_NothingPushed pins the degenerate
// end: when no rule of a route compiles, nothing of it is pushed and every rule
// carries a WholeRule diagnostic, which is what the status layer turns into
// Accepted=False with UnsupportedValue.
func TestConvertHTTPRoutes_AllRulesUncompilable_NothingPushed(t *testing.T) {
	t.Parallel()

	regularExpression := gatewayv1.PathMatchRegularExpression

	routes := []*gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						Matches:     []gatewayv1.HTTPRouteMatch{{Path: &gatewayv1.HTTPPathMatch{Type: &regularExpression, Value: new("[")}}},
						BackendRefs: []gatewayv1.HTTPBackendRef{backendRef("web-svc", 80, 1)},
					},
					{
						Matches:     []gatewayv1.HTTPRouteMatch{{Path: &gatewayv1.HTTPPathMatch{Type: &regularExpression, Value: new("(")}}},
						BackendRefs: []gatewayv1.HTTPBackendRef{backendRef("web-svc", 80, 1)},
					},
				},
			},
		},
	}

	cfg := proxy.ConvertHTTPRoutes(context.Background(), routes, "cluster.local", nil, nil, nil, nil)

	assert.Empty(t, cfg.Rules, "no rule compiles, so nothing is pushed")
	assert.Empty(t, cfg.Provenance, "provenance follows the rules")

	require.Len(t, cfg.Diagnostics, 2)

	for i, diag := range cfg.Diagnostics {
		assert.Equal(t, i, diag.RuleIndex, "diagnostics keep the rule index the tenant wrote")
		assert.Equal(t, proxy.DiagnosticAccepted, diag.Target)
		assert.Equal(t, string(gatewayv1.RouteReasonUnsupportedValue), diag.Reason)
		assert.True(t, diag.WholeRule, "every rule is wholly unservable")
	}
}

// TestConvertHTTPRoutes_DiagnosticQuotesTheFailingPattern pins which pattern the
// diagnostic names when a match carries several regex-typed values. Quoting the
// first one would tell the tenant that a valid pattern is broken.
func TestConvertHTTPRoutes_DiagnosticQuotesTheFailingPattern(t *testing.T) {
	t.Parallel()

	pathRegex := gatewayv1.PathMatchRegularExpression
	headerRegex := gatewayv1.HeaderMatchRegularExpression
	queryRegex := gatewayv1.QueryParamMatchRegularExpression

	tests := []struct {
		name       string
		match      gatewayv1.HTTPRouteMatch
		wantQuoted string
		wantAbsent string
	}{
		{
			name: "valid path regex beside a broken header regex names the header",
			match: gatewayv1.HTTPRouteMatch{
				Path:    &gatewayv1.HTTPPathMatch{Type: &pathRegex, Value: new("/api/.*")},
				Headers: []gatewayv1.HTTPHeaderMatch{{Type: &headerRegex, Name: "x-tenant", Value: "["}},
			},
			wantQuoted: `"["`,
			wantAbsent: "/api/.*",
		},
		{
			name: "valid header regex beside a broken one names the broken one",
			match: gatewayv1.HTTPRouteMatch{
				Headers: []gatewayv1.HTTPHeaderMatch{
					{Type: &headerRegex, Name: "x-ok", Value: "ok.*"},
					{Type: &headerRegex, Name: "x-bad", Value: "(unclosed"},
				},
			},
			wantQuoted: `"(unclosed"`,
			wantAbsent: "ok.*",
		},
		{
			name: "broken query regex is named",
			match: gatewayv1.HTTPRouteMatch{
				QueryParams: []gatewayv1.HTTPQueryParamMatch{{Type: &queryRegex, Name: "q", Value: "*bad"}},
			},
			wantQuoted: `"*bad"`,
			wantAbsent: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			routes := []*gatewayv1.HTTPRoute{{
				ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
				Spec: gatewayv1.HTTPRouteSpec{
					Hostnames: []gatewayv1.Hostname{"example.com"},
					Rules: []gatewayv1.HTTPRouteRule{{
						Matches:     []gatewayv1.HTTPRouteMatch{tt.match},
						BackendRefs: []gatewayv1.HTTPBackendRef{backendRef("web-svc", 80, 1)},
					}},
				},
			}}

			cfg := proxy.ConvertHTTPRoutes(context.Background(), routes, "cluster.local", nil, nil, nil, nil)

			require.Len(t, cfg.Diagnostics, 1)
			assert.Contains(t, cfg.Diagnostics[0].Message, "pattern "+tt.wantQuoted)

			if tt.wantAbsent != "" {
				assert.NotContains(t, cfg.Diagnostics[0].Message, tt.wantAbsent,
					"a valid pattern must not be named as the broken one")
			}
		})
	}
}
