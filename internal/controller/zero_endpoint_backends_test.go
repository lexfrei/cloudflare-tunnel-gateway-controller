package controller

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
)

// apiReadCounts records how many Service Gets (keyed "namespace/name") and
// EndpointSlice Lists (keyed by namespace) the code under test performed, so a
// test can assert read-amplification and authorization behaviour.
type apiReadCounts struct {
	serviceGets        map[string]int
	endpointSliceLists map[string]int
}

// countingClient wraps a fake client and tallies Service Gets and EndpointSlice
// Lists into the returned apiReadCounts.
func countingClient(t *testing.T, objs ...client.Object) (client.Client, *apiReadCounts) {
	t.Helper()

	counts := &apiReadCounts{
		serviceGets:        map[string]int{},
		endpointSliceLists: map[string]int{},
	}

	base := fake.NewClientBuilder().WithScheme(zeScheme(t)).WithObjects(objs...).Build()

	wrapped := interceptor.NewClient(base, interceptor.Funcs{
		Get: func(
			ctx context.Context, c client.WithWatch, key client.ObjectKey,
			obj client.Object, opts ...client.GetOption,
		) error {
			if _, ok := obj.(*corev1.Service); ok {
				counts.serviceGets[key.Namespace+"/"+key.Name]++
			}

			return c.Get(ctx, key, obj, opts...)
		},
		List: func(
			ctx context.Context, c client.WithWatch,
			list client.ObjectList, opts ...client.ListOption,
		) error {
			if _, ok := list.(*discoveryv1.EndpointSliceList); ok {
				lo := &client.ListOptions{}
				for _, o := range opts {
					o.ApplyToList(lo)
				}
				counts.endpointSliceLists[lo.Namespace]++
			}

			return c.List(ctx, list, opts...)
		},
	})

	return wrapped, counts
}

func zeScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, discoveryv1.AddToScheme(scheme))
	require.NoError(t, gatewayv1.Install(scheme))

	return scheme
}

func clusterIPSvc(name, namespace string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP, ClusterIP: "10.0.0.1"},
	}
}

func endpointSlice(name, namespace, svcName string, ready bool) *discoveryv1.EndpointSlice {
	return &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    map[string]string{discoveryv1.LabelServiceName: svcName},
		},
		Endpoints: []discoveryv1.Endpoint{
			{Addresses: []string{"10.1.0.1"}, Conditions: discoveryv1.EndpointConditions{Ready: &ready}},
		},
	}
}

func httpRouteToSvc(routeName, namespace, svcName string, port gatewayv1.PortNumber) *gatewayv1.HTTPRoute {
	p := port

	return &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: routeName, Namespace: namespace},
		Spec: gatewayv1.HTTPRouteSpec{
			Rules: []gatewayv1.HTTPRouteRule{
				{BackendRefs: []gatewayv1.HTTPBackendRef{
					{BackendRef: gatewayv1.BackendRef{BackendObjectReference: gatewayv1.BackendObjectReference{
						Name: gatewayv1.ObjectName(svcName), Port: &p,
					}}},
				}},
			},
		},
	}
}

// TestMarkZeroEndpointBackends_MarksServiceWithNoReadyEndpoints pins the 503
// path (spec SHOULD): a backend whose Service exists but has zero ready
// endpoints is marked 503 so its traffic fraction returns 503 instead of
// dialing and surfacing a 502.
func TestMarkZeroEndpointBackends_MarksServiceWithNoReadyEndpoints(t *testing.T) {
	t.Parallel()

	cli := fake.NewClientBuilder().WithScheme(zeScheme(t)).WithObjects(
		clusterIPSvc("dead", "default"),
		endpointSlice("dead-abc", "default", "dead", false),
	).Build()

	cfg := &proxy.Config{Rules: []proxy.RouteRule{{Backends: []proxy.BackendRef{
		{URL: "http://dead.default.svc.cluster.local:80", Weight: 1},
	}}}}

	routes := []*gatewayv1.HTTPRoute{httpRouteToSvc("r", "default", "dead", 80)}

	markZeroEndpointBackends(context.Background(), cli, cfg, "cluster.local", routes, nil)

	assert.Equal(t, http.StatusServiceUnavailable, cfg.Rules[0].Backends[0].UnavailableStatus,
		"a Service with zero ready endpoints must mark the backend 503")
}

// TestMarkZeroEndpointBackends_LeavesReadyServiceUntouched proves a Service with
// at least one ready endpoint is not marked.
func TestMarkZeroEndpointBackends_LeavesReadyServiceUntouched(t *testing.T) {
	t.Parallel()

	cli := fake.NewClientBuilder().WithScheme(zeScheme(t)).WithObjects(
		clusterIPSvc("live", "default"),
		endpointSlice("live-abc", "default", "live", true),
	).Build()

	cfg := &proxy.Config{Rules: []proxy.RouteRule{{Backends: []proxy.BackendRef{
		{URL: "http://live.default.svc.cluster.local:80", Weight: 1},
	}}}}

	routes := []*gatewayv1.HTTPRoute{httpRouteToSvc("r", "default", "live", 80)}

	markZeroEndpointBackends(context.Background(), cli, cfg, "cluster.local", routes, nil)

	assert.Zero(t, cfg.Rules[0].Backends[0].UnavailableStatus, "a ready Service must not be marked")
}

// TestMarkZeroEndpointBackends_SkipsExternalNameService proves an ExternalName
// Service (which legitimately has no EndpointSlices) is never marked 503.
func TestMarkZeroEndpointBackends_SkipsExternalNameService(t *testing.T) {
	t.Parallel()

	extSvc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "ext", Namespace: "default"},
		Spec:       corev1.ServiceSpec{Type: corev1.ServiceTypeExternalName, ExternalName: "api.example.com"},
	}
	cli := fake.NewClientBuilder().WithScheme(zeScheme(t)).WithObjects(extSvc).Build()

	cfg := &proxy.Config{Rules: []proxy.RouteRule{{Backends: []proxy.BackendRef{
		{URL: "http://ext.default.svc.cluster.local:80", Weight: 1},
	}}}}

	routes := []*gatewayv1.HTTPRoute{httpRouteToSvc("r", "default", "ext", 80)}

	markZeroEndpointBackends(context.Background(), cli, cfg, "cluster.local", routes, nil)

	assert.Zero(t, cfg.Rules[0].Backends[0].UnavailableStatus, "ExternalName Service must not be marked 503")
}

// TestMarkZeroEndpointBackends_SkipsMissingService proves a nonexistent Service
// is left for the 500 invalid-ref path (not marked 503 here).
func TestMarkZeroEndpointBackends_SkipsMissingService(t *testing.T) {
	t.Parallel()

	cli := fake.NewClientBuilder().WithScheme(zeScheme(t)).Build()

	cfg := &proxy.Config{Rules: []proxy.RouteRule{{Backends: []proxy.BackendRef{
		{URL: "http://gone.default.svc.cluster.local:80", Weight: 1},
	}}}}

	routes := []*gatewayv1.HTTPRoute{httpRouteToSvc("r", "default", "gone", 80)}

	markZeroEndpointBackends(context.Background(), cli, cfg, "cluster.local", routes, nil)

	assert.Zero(t, cfg.Rules[0].Backends[0].UnavailableStatus,
		"a nonexistent Service is the 500 invalid-ref path, not 503")
}

// TestMarkZeroEndpointBackends_SkipsBackendAbsentFromConfig proves the 503 path
// is gated on cfg membership: a cross-namespace backend the converter dropped
// (no ReferenceGrant) is absent from cfg and must never be read, keeping
// authorization symmetric with the 500 path. The in-namespace backend that IS
// in cfg is still inspected.
func TestMarkZeroEndpointBackends_SkipsBackendAbsentFromConfig(t *testing.T) {
	t.Parallel()

	cli, counts := countingClient(t,
		clusterIPSvc("app", "default"),
		endpointSlice("app-abc", "default", "app", true),
		// The unauthorized cross-namespace Service exists with zero ready
		// endpoints — it WOULD be marked 503 if the 503 path inspected it.
		clusterIPSvc("secret", "other"),
		endpointSlice("secret-abc", "other", "secret", false),
	)

	// The converter emitted only the authorized in-namespace backend; the
	// cross-namespace ref to other/secret was dropped, so it is not in cfg.
	cfg := &proxy.Config{Rules: []proxy.RouteRule{{Backends: []proxy.BackendRef{
		{URL: "http://app.default.svc.cluster.local:80", Weight: 1},
	}}}}

	otherNS := gatewayv1.Namespace("other")
	p := gatewayv1.PortNumber(80)
	pApp := gatewayv1.PortNumber(80)
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "default"},
		Spec: gatewayv1.HTTPRouteSpec{Rules: []gatewayv1.HTTPRouteRule{{
			BackendRefs: []gatewayv1.HTTPBackendRef{
				{BackendRef: gatewayv1.BackendRef{BackendObjectReference: gatewayv1.BackendObjectReference{
					Name: "app", Port: &pApp,
				}}},
				{BackendRef: gatewayv1.BackendRef{BackendObjectReference: gatewayv1.BackendObjectReference{
					Name: "secret", Namespace: &otherNS, Port: &p,
				}}},
			},
		}}},
	}

	markZeroEndpointBackends(context.Background(), cli, cfg, "cluster.local",
		[]*gatewayv1.HTTPRoute{route}, nil)

	assert.Zero(t, counts.serviceGets["other/secret"],
		"a backend the converter dropped (unauthorized cross-namespace) must never be read by the 503 path")
	assert.Zero(t, cfg.Rules[0].Backends[0].UnavailableStatus,
		"the authorized in-cfg backend has a ready endpoint and stays unmarked")
}

// TestMarkZeroEndpointBackends_InspectsServiceOncePerReconcile proves a Service
// referenced by multiple routes is read once per reconcile, not once per
// reference, bounding API reads.
func TestMarkZeroEndpointBackends_InspectsServiceOncePerReconcile(t *testing.T) {
	t.Parallel()

	cli, counts := countingClient(t,
		clusterIPSvc("dead", "default"),
		endpointSlice("dead-abc", "default", "dead", false),
	)

	cfg := &proxy.Config{Rules: []proxy.RouteRule{{Backends: []proxy.BackendRef{
		{URL: "http://dead.default.svc.cluster.local:80", Weight: 1},
	}}}}

	routes := []*gatewayv1.HTTPRoute{
		httpRouteToSvc("r1", "default", "dead", 80),
		httpRouteToSvc("r2", "default", "dead", 80),
	}

	markZeroEndpointBackends(context.Background(), cli, cfg, "cluster.local", routes, nil)

	assert.Equal(t, 1, counts.serviceGets["default/dead"],
		"a Service referenced by multiple routes must be read once per reconcile")
	assert.Equal(t, 1, counts.endpointSliceLists["default"],
		"EndpointSlices for a repeated Service must be listed once per reconcile")
	assert.Equal(t, http.StatusServiceUnavailable, cfg.Rules[0].Backends[0].UnavailableStatus,
		"the zero-endpoint backend is still marked 503")
}

// TestMarkZeroEndpointBackends_MarksHeadlessServiceWithNoReadyEndpoints covers a
// headless Service (ClusterIP: None) with zero ready endpoints: it has
// EndpointSlices like any selector Service, so it is marked 503.
func TestMarkZeroEndpointBackends_MarksHeadlessServiceWithNoReadyEndpoints(t *testing.T) {
	t.Parallel()

	headless := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "head", Namespace: "default"},
		Spec:       corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP, ClusterIP: corev1.ClusterIPNone},
	}
	cli := fake.NewClientBuilder().WithScheme(zeScheme(t)).WithObjects(
		headless,
		endpointSlice("head-abc", "default", "head", false),
	).Build()

	cfg := &proxy.Config{Rules: []proxy.RouteRule{{Backends: []proxy.BackendRef{
		{URL: "http://head.default.svc.cluster.local:80", Weight: 1},
	}}}}

	routes := []*gatewayv1.HTTPRoute{httpRouteToSvc("r", "default", "head", 80)}

	markZeroEndpointBackends(context.Background(), cli, cfg, "cluster.local", routes, nil)

	assert.Equal(t, http.StatusServiceUnavailable, cfg.Rules[0].Backends[0].UnavailableStatus,
		"a headless Service with zero ready endpoints must be marked 503")
}

// TestMarkZeroEndpointBackends_MarksNonDefaultPort proves the 503 path works on
// a backend whose port is not 80. The cfg-membership key (built from the
// converter's URL via proxy.UnmarkedBackendHosts) and the per-ref key (built by
// proxy.ServiceBackendHost from the backendRef port) must agree on the port, or
// the 503 marking silently no-ops. A regression in how either side derives the
// port is caught here.
func TestMarkZeroEndpointBackends_MarksNonDefaultPort(t *testing.T) {
	t.Parallel()

	cli := fake.NewClientBuilder().WithScheme(zeScheme(t)).WithObjects(
		clusterIPSvc("dead", "default"),
		endpointSlice("dead-abc", "default", "dead", false),
	).Build()

	cfg := &proxy.Config{Rules: []proxy.RouteRule{{Backends: []proxy.BackendRef{
		{URL: "http://dead.default.svc.cluster.local:8443", Weight: 1},
	}}}}

	routes := []*gatewayv1.HTTPRoute{httpRouteToSvc("r", "default", "dead", 8443)}

	markZeroEndpointBackends(context.Background(), cli, cfg, "cluster.local", routes, nil)

	assert.Equal(t, http.StatusServiceUnavailable, cfg.Rules[0].Backends[0].UnavailableStatus,
		"a zero-ready-endpoint backend on a non-default port must still be marked 503")
}

// TestFindRoutesForEndpointSlice_EnqueuesReferencingRoute pins the watch mapper:
// an EndpointSlice change enqueues the routes whose backendRefs target the
// owning Service (identified by the kubernetes.io/service-name label).
func TestFindRoutesForEndpointSlice_EnqueuesReferencingRoute(t *testing.T) {
	t.Parallel()

	route := httpRouteToSvc("web", "default", "dead", 80)
	routes := []Route{HTTPRouteWrapper{route}}

	slice := endpointSlice("dead-abc", "default", "dead", false)

	got := FindRoutesForEndpointSlice(slice, routes)

	require.Len(t, got, 1)
	assert.Equal(t, "web", got[0].Name)
	assert.Equal(t, "default", got[0].Namespace)
}

// TestFindRoutesForEndpointSlice_IgnoresUnrelated proves a slice for a Service
// no route references, and a non-EndpointSlice object, enqueue nothing.
func TestFindRoutesForEndpointSlice_IgnoresUnrelated(t *testing.T) {
	t.Parallel()

	routes := []Route{HTTPRouteWrapper{httpRouteToSvc("web", "default", "dead", 80)}}

	assert.Empty(t, FindRoutesForEndpointSlice(endpointSlice("other-abc", "default", "other", true), routes),
		"a slice for an unreferenced Service must enqueue nothing")
	assert.Nil(t, FindRoutesForEndpointSlice(clusterIPSvc("dead", "default"), routes),
		"a non-EndpointSlice object must be ignored")
}
