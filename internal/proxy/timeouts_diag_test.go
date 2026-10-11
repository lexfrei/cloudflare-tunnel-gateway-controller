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

// TestConvertHTTPRoutes_InvalidTimeouts_DropsRule pins the spec's "Drop
// Rule(s)" approach for PartiallyInvalid: a rule whose timeouts fail to parse
// is not served without them, it fails closed, and the WholeRule diagnostic
// is what the controller reports as "Dropped Rule".
func TestConvertHTTPRoutes_InvalidTimeouts_DropsRule(t *testing.T) {
	t.Parallel()

	pathPrefix := gatewayv1.PathMatchPathPrefix
	badRequest := gatewayv1.Duration("not-a-duration")
	routes := []*gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						Matches:     []gatewayv1.HTTPRouteMatch{{Path: &gatewayv1.HTTPPathMatch{Type: &pathPrefix, Value: new("/")}}},
						Timeouts:    &gatewayv1.HTTPRouteTimeouts{Request: &badRequest},
						BackendRefs: []gatewayv1.HTTPBackendRef{backendRef("web-svc", 80, 1)},
					},
				},
			},
		},
	}

	cfg := proxy.ConvertHTTPRoutes(context.Background(), routes, "cluster.local", nil, nil, nil, nil)

	require.Len(t, cfg.Rules, 1)
	assert.Equal(t, http.StatusInternalServerError, cfg.Rules[0].UnavailableStatus,
		"a rule with an unparseable timeout must not serve without it")
	require.NoError(t, cfg.Validate(), "the converted config must stay pushable")

	require.Len(t, cfg.Diagnostics, 1)
	diag := cfg.Diagnostics[0]
	assert.Equal(t, "web", diag.Name)
	assert.Equal(t, 0, diag.RuleIndex)
	assert.Equal(t, proxy.DiagnosticAccepted, diag.Target)
	assert.Equal(t, string(gatewayv1.RouteReasonUnsupportedValue), diag.Reason)
	assert.True(t, diag.WholeRule, "the whole rule is dropped")
	assert.Contains(t, diag.Message, "timeout", "message must name the field that failed")
}

// TestConvertHTTPRoutes_ValidTimeouts_NoDiagnostic confirms a parseable timeout
// is applied with no diagnostic.
func TestConvertHTTPRoutes_ValidTimeouts_NoDiagnostic(t *testing.T) {
	t.Parallel()

	pathPrefix := gatewayv1.PathMatchPathPrefix
	good := gatewayv1.Duration("10s")
	routes := []*gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						Matches:     []gatewayv1.HTTPRouteMatch{{Path: &gatewayv1.HTTPPathMatch{Type: &pathPrefix, Value: new("/")}}},
						Timeouts:    &gatewayv1.HTTPRouteTimeouts{Request: &good},
						BackendRefs: []gatewayv1.HTTPBackendRef{backendRef("web-svc", 80, 1)},
					},
				},
			},
		},
	}

	cfg := proxy.ConvertHTTPRoutes(context.Background(), routes, "cluster.local", nil, nil, nil, nil)

	require.Len(t, cfg.Rules, 1)
	require.NotNil(t, cfg.Rules[0].Timeouts)
	assert.Empty(t, cfg.Diagnostics)
}
