package controller

import (
	"context"
	"net/http"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/ingress"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
)

// TestMarkUnavailableBackends_MarksInvalidKeepsValid pins the per-fraction 500
// behaviour: in a rule with a valid high-weight backend and an invalid
// (nonexistent Service) low-weight backend, the invalid one is marked 500 and
// kept in the weighted pool while the valid one is untouched — so the invalid
// backend's traffic fraction returns 500 (not 502) and the valid fraction
// keeps serving.
func TestMarkUnavailableBackends_MarksInvalidKeepsValid(t *testing.T) {
	t.Parallel()

	cfg := &proxy.Config{
		Rules: []proxy.RouteRule{
			{
				Backends: []proxy.BackendRef{
					{URL: "http://valid-svc.default.svc.cluster.local:80", Weight: 80},
					{URL: "http://missing-svc.default.svc.cluster.local:8080", Weight: 20},
				},
			},
		},
	}

	failedRefs := []ingress.BackendRefError{
		{RouteNamespace: "default", RouteName: "r", BackendName: "missing-svc", BackendNS: "default", Port: 8080},
	}

	markUnavailableBackends(cfg, "cluster.local", kindHTTPRouteDiag, failedRefs)

	backends := cfg.Rules[0].Backends
	require.Len(t, backends, 2, "both backends must remain in the pool")
	assert.Zero(t, backends[0].UnavailableStatus, "valid backend untouched")
	assert.Equal(t, http.StatusInternalServerError, backends[1].UnavailableStatus, "invalid backend marked 500")
	assert.Equal(t, int32(80), backends[0].Weight)
	assert.Equal(t, int32(20), backends[1].Weight, "weight preserved for the invalid backend's fraction")
}

// TestMarkUnavailableBackends_AllInvalidAllMarked proves that when every backend
// in a rule is invalid, all are marked 500 — so 100% of the rule's traffic
// returns 500, matching the all-invalid spec contract.
func TestMarkUnavailableBackends_AllInvalidAllMarked(t *testing.T) {
	t.Parallel()

	cfg := &proxy.Config{
		Rules: []proxy.RouteRule{
			{
				Backends: []proxy.BackendRef{
					{URL: "http://a.default.svc.cluster.local:80", Weight: 1},
					{URL: "http://b.default.svc.cluster.local:80", Weight: 1},
				},
			},
		},
	}

	failedRefs := []ingress.BackendRefError{
		{RouteNamespace: "default", RouteName: "r", BackendName: "a", BackendNS: "default", Port: 80},
		{RouteNamespace: "default", RouteName: "r", BackendName: "b", BackendNS: "default", Port: 80},
	}

	markUnavailableBackends(cfg, "cluster.local", kindHTTPRouteDiag, failedRefs)

	for i := range cfg.Rules[0].Backends {
		assert.Equal(t, http.StatusInternalServerError, cfg.Rules[0].Backends[i].UnavailableStatus)
	}
}

// TestMarkUnavailableBackends_ServiceImportDomain proves a ServiceImport
// failed-ref is matched against the clusterset host (carried on the ref's
// Domain), not the local cluster domain — without it the host would not match
// the converter-synthesized clusterset.local URL and the 500 would be lost.
func TestMarkUnavailableBackends_ServiceImportDomain(t *testing.T) {
	t.Parallel()

	cfg := &proxy.Config{
		Rules: []proxy.RouteRule{
			{
				Backends: []proxy.BackendRef{
					{URL: "http://imported.default.svc.clusterset.local:80", Weight: 1},
				},
			},
		},
	}

	failedRefs := []ingress.BackendRefError{
		{
			RouteNamespace: "default", RouteName: "r",
			BackendName: "imported", BackendNS: "default", Port: 80,
			Reason: "BackendNotFound", Domain: "clusterset.local",
		},
	}

	markUnavailableBackends(cfg, "cluster.local", kindHTTPRouteDiag, failedRefs)

	assert.Equal(t, http.StatusInternalServerError, cfg.Rules[0].Backends[0].UnavailableStatus,
		"a ServiceImport backend must be marked 500 via its clusterset host")
}

// TestMarkUnavailableBackends_NoFailedRefsNoop proves a clean config is left
// untouched when there are no failed refs.
func TestMarkUnavailableBackends_NoFailedRefsNoop(t *testing.T) {
	t.Parallel()

	cfg := &proxy.Config{
		Rules: []proxy.RouteRule{
			{Backends: []proxy.BackendRef{{URL: "http://svc.default.svc.cluster.local:80", Weight: 1}}},
		},
	}

	markUnavailableBackends(cfg, "cluster.local", kindHTTPRouteDiag, nil)

	assert.Zero(t, cfg.Rules[0].Backends[0].UnavailableStatus)
}

// TestMarkUnavailableBackends_ScopedToTheFailingRoute pins that a backendRef
// refused for one route marks only that route's rules: another route granted
// the same backend keeps serving it, and a gRPC route's failure does not reach
// an HTTPRoute of the same name.
func TestMarkUnavailableBackends_ScopedToTheFailingRoute(t *testing.T) {
	t.Parallel()

	shared := "http://svc.backend.svc.cluster.local:80"
	cfg := &proxy.Config{
		Rules: []proxy.RouteRule{
			{Backends: []proxy.BackendRef{{URL: shared, Weight: 1}}},
			{Backends: []proxy.BackendRef{{URL: shared, Weight: 1}}},
			{Backends: []proxy.BackendRef{{URL: shared, Weight: 1}}},
			{Backends: []proxy.BackendRef{{URL: shared, Weight: 1}}},
		},
		Provenance: []proxy.RuleProvenance{
			{Kind: kindHTTPRouteDiag, Namespace: "team-a", Name: "r"},
			{Kind: kindHTTPRouteDiag, Namespace: "team-b", Name: "r"},
			{Kind: kindGRPCRouteDiag, Namespace: "team-b", Name: "r"},
			{Kind: kindHTTPRouteDiag, Namespace: "team-a", Name: "other"},
		},
	}

	failedRefs := []ingress.BackendRefError{{
		RouteNamespace: "team-a", RouteName: "r", BackendName: "svc", BackendNS: "backend", Port: 80,
		Reason: "RefNotPermitted",
	}}

	markUnavailableBackends(cfg, "cluster.local", kindHTTPRouteDiag, failedRefs)

	assert.Equal(t, http.StatusInternalServerError, cfg.Rules[0].Backends[0].UnavailableStatus)
	assert.Zero(t, cfg.Rules[1].Backends[0].UnavailableStatus, "another route's grant is its own")
	assert.Zero(t, cfg.Rules[2].Backends[0].UnavailableStatus, "another route's grant is its own")
	assert.Zero(t, cfg.Rules[3].Backends[0].UnavailableStatus, "another route's grant is its own, in the same namespace too")

	markUnavailableBackends(cfg, "cluster.local", kindGRPCRouteDiag, []ingress.BackendRefError{{
		RouteNamespace: "team-b", RouteName: "r", BackendName: "svc", BackendNS: "backend", Port: 80,
	}})

	assert.Zero(t, cfg.Rules[1].Backends[0].UnavailableStatus, "an HTTPRoute is not the GRPCRoute of the same name")
	assert.Equal(t, http.StatusInternalServerError, cfg.Rules[2].Backends[0].UnavailableStatus)
}

// TestBuildProxyConfig_RefusedRefLeavesGrantedRouteServing builds a real
// config: one route is refused a cross-namespace Service that a ReferenceGrant
// gives to another route, and the other route's backend must stay unmarked.
func TestBuildProxyConfig_RefusedRefLeavesGrantedRouteServing(t *testing.T) {
	t.Parallel()

	gc := managedGatewayClass()
	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(gc.Name),
			Listeners: []gatewayv1.Listener{{
				Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType,
				AllowedRoutes: &gatewayv1.AllowedRoutes{Namespaces: &gatewayv1.RouteNamespaces{From: namespacesFromAllPtr()}},
			}},
		},
	}
	grant := &gatewayv1beta1.ReferenceGrant{
		ObjectMeta: metav1.ObjectMeta{Name: "grant", Namespace: "backend"},
		Spec: gatewayv1beta1.ReferenceGrantSpec{
			From: []gatewayv1beta1.ReferenceGrantFrom{{Group: gatewayv1.GroupName, Kind: "HTTPRoute", Namespace: "team-b"}},
			To:   []gatewayv1beta1.ReferenceGrantTo{{Group: "", Kind: "Service"}},
		},
	}
	route := func(namespace, hostname string) *gatewayv1.HTTPRoute {
		return &gatewayv1.HTTPRoute{
			ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: namespace},
			Spec: gatewayv1.HTTPRouteSpec{
				CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: []gatewayv1.ParentReference{{
					Name: "gw", Namespace: new(gatewayv1.Namespace("default")),
				}}},
				Hostnames: []gatewayv1.Hostname{gatewayv1.Hostname(hostname)},
				Rules: []gatewayv1.HTTPRouteRule{{BackendRefs: []gatewayv1.HTTPBackendRef{{BackendRef: gatewayv1.BackendRef{
					BackendObjectReference: gatewayv1.BackendObjectReference{
						Name: "svc", Namespace: new(gatewayv1.Namespace("backend")), Port: new(gatewayv1.PortNumber(80)),
					},
				}}}}},
			},
		}
	}

	scheme := runtime.NewScheme()
	require.NoError(t, gatewayv1.Install(scheme))
	require.NoError(t, gatewayv1beta1.Install(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))

	cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(gc, gateway, grant).Build()
	syncer := NewProxySyncer("cluster.local", "token", testListenerSetController, cli, nil)

	failedRefs := []ingress.BackendRefError{{
		RouteNamespace: "team-a", RouteName: "r", BackendName: "svc", BackendNS: "backend", Port: 80,
		Reason: string(gatewayv1.RouteReasonRefNotPermitted),
	}}

	cfg := syncer.buildProxyConfig(context.Background(),
		[]*gatewayv1.HTTPRoute{route("team-a", "a.example.com"), route("team-b", "b.example.com")}, nil,
		failedRefs, nil, clientCertParents{})

	var granted *proxy.RouteRule

	for i := range cfg.Rules {
		if slices.Contains(cfg.Rules[i].Hostnames, "b.example.com") {
			granted = &cfg.Rules[i]
		}
	}

	require.NotNil(t, granted)
	require.NotEmpty(t, granted.Backends)
	assert.Zero(t, granted.Backends[0].UnavailableStatus)
}
