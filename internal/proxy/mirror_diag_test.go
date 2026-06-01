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

// mirrorRoute builds a one-rule HTTPRoute whose single rule carries a
// RequestMirror filter pointing at the given backend ref, plus a normal
// backend so the rule itself still serves.
func mirrorRoute(mirrorRef gatewayv1.BackendObjectReference) *gatewayv1.HTTPRoute {
	pathPrefix := gatewayv1.PathMatchPathPrefix

	return &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
		Spec: gatewayv1.HTTPRouteSpec{
			Hostnames: []gatewayv1.Hostname{"example.com"},
			Rules: []gatewayv1.HTTPRouteRule{
				{
					Matches: []gatewayv1.HTTPRouteMatch{{Path: &gatewayv1.HTTPPathMatch{Type: &pathPrefix, Value: new("/")}}},
					Filters: []gatewayv1.HTTPRouteFilter{
						{
							Type:          gatewayv1.HTTPRouteFilterRequestMirror,
							RequestMirror: &gatewayv1.HTTPRequestMirrorFilter{BackendRef: mirrorRef},
						},
					},
					BackendRefs: []gatewayv1.HTTPBackendRef{backendRef("web-svc", 80, 1)},
				},
			},
		},
	}
}

// TestConvertHTTPRoutes_MirrorNonServiceKind_ResolvedRefsDiagnostic pins that a
// RequestMirror pointing at a non-Service kind is dropped (the main request
// still flows) and records a ResolvedRefs-target InvalidKind diagnostic so the
// dropped mirror is visible on the route status.
func TestConvertHTTPRoutes_MirrorNonServiceKind_ResolvedRefsDiagnostic(t *testing.T) {
	t.Parallel()

	group := gatewayv1.Group("example.net")
	kind := gatewayv1.Kind("Widget")
	port := gatewayv1.PortNumber(80)
	ref := gatewayv1.BackendObjectReference{Group: &group, Kind: &kind, Name: "mirror-target", Port: &port}

	cfg := proxy.ConvertHTTPRoutes(context.Background(), []*gatewayv1.HTTPRoute{mirrorRoute(ref)},
		"cluster.local", nil, nil, nil, nil)

	require.Len(t, cfg.Rules, 1)
	assert.Empty(t, cfg.Rules[0].Filters, "the unservable mirror filter is dropped")
	require.Len(t, cfg.Rules[0].Backends, 1, "the main backend still serves")
	assert.Zero(t, cfg.Rules[0].UnavailableStatus, "a dropped mirror does not fail the rule closed")

	require.Len(t, cfg.Diagnostics, 1)
	diag := cfg.Diagnostics[0]
	assert.Equal(t, "web", diag.Name)
	assert.Equal(t, proxy.DiagnosticResolvedRefs, diag.Target)
	assert.Equal(t, string(gatewayv1.RouteReasonInvalidKind), diag.Reason)
	assert.False(t, diag.WholeRule)
	assert.Contains(t, diag.Message, "mirror", "message must name the dropped mirror")
}

// TestConvertHTTPRoutes_MirrorUnauthorizedCrossNamespace_RefNotPermitted pins
// that a cross-namespace mirror ref blocked by a missing ReferenceGrant is
// dropped and recorded as ResolvedRefs / RefNotPermitted.
func TestConvertHTTPRoutes_MirrorUnauthorizedCrossNamespace_RefNotPermitted(t *testing.T) {
	t.Parallel()

	otherNS := gatewayv1.Namespace("other")
	port := gatewayv1.PortNumber(80)
	ref := gatewayv1.BackendObjectReference{Name: "mirror-target", Namespace: &otherNS, Port: &port}

	// validator returns false → cross-namespace ref not permitted.
	validator := func(_ context.Context, _ string, _ gatewayv1.BackendObjectReference) bool { return false }

	cfg := proxy.ConvertHTTPRoutes(context.Background(), []*gatewayv1.HTTPRoute{mirrorRoute(ref)},
		"cluster.local", validator, nil, nil, nil)

	require.Len(t, cfg.Rules, 1)
	assert.Empty(t, cfg.Rules[0].Filters, "the unauthorized mirror filter is dropped")

	require.Len(t, cfg.Diagnostics, 1)
	diag := cfg.Diagnostics[0]
	assert.Equal(t, proxy.DiagnosticResolvedRefs, diag.Target)
	assert.Equal(t, string(gatewayv1.RouteReasonRefNotPermitted), diag.Reason)
	assert.False(t, diag.WholeRule)
}

// TestConvertHTTPRoutes_MirrorValid_NoDiagnostic confirms a valid same-namespace
// mirror produces the filter and no diagnostic.
func TestConvertHTTPRoutes_MirrorValid_NoDiagnostic(t *testing.T) {
	t.Parallel()

	port := gatewayv1.PortNumber(80)
	ref := gatewayv1.BackendObjectReference{Name: "mirror-target", Port: &port}

	cfg := proxy.ConvertHTTPRoutes(context.Background(), []*gatewayv1.HTTPRoute{mirrorRoute(ref)},
		"cluster.local", nil, nil, nil, nil)

	require.Len(t, cfg.Rules, 1)
	require.Len(t, cfg.Rules[0].Filters, 1, "a valid mirror is preserved")
	assert.Equal(t, proxy.FilterRequestMirror, cfg.Rules[0].Filters[0].Type)
	assert.Empty(t, cfg.Diagnostics)
}
