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
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
)

// --- headless test fixtures -------------------------------------------------

func svcPort(name string, port, target int32) corev1.ServicePort {
	return corev1.ServicePort{
		Name:       name,
		Port:       port,
		TargetPort: intstr.FromInt32(target),
		Protocol:   corev1.ProtocolTCP,
	}
}

// svcPortNamedTarget builds a Service port whose targetPort is a NAMED container
// port (not numeric), so it can only be resolved to a number via the EndpointSlice.
func svcPortNamedTarget(name string, port int32, targetName string) corev1.ServicePort {
	return corev1.ServicePort{
		Name:       name,
		Port:       port,
		TargetPort: intstr.FromString(targetName),
		Protocol:   corev1.ProtocolTCP,
	}
}

func headlessSvc(name, namespace string, ports ...corev1.ServicePort) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: corev1.ServiceSpec{
			Type:      corev1.ServiceTypeClusterIP,
			ClusterIP: corev1.ClusterIPNone,
			Ports:     ports,
		},
	}
}

func epPort(name string, port int32) discoveryv1.EndpointPort {
	pn := name
	pp := port

	return discoveryv1.EndpointPort{Name: &pn, Port: &pp}
}

type epAddr struct {
	addr  string
	ready bool
}

func headlessSlice(
	sliceName, namespace, svcName string,
	addrType discoveryv1.AddressType,
	ports []discoveryv1.EndpointPort,
	addrs []epAddr,
) *discoveryv1.EndpointSlice {
	endpoints := make([]discoveryv1.Endpoint, 0, len(addrs))
	for _, a := range addrs {
		ready := a.ready
		endpoints = append(endpoints, discoveryv1.Endpoint{
			Addresses:  []string{a.addr},
			Conditions: discoveryv1.EndpointConditions{Ready: &ready},
		})
	}

	return &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sliceName,
			Namespace: namespace,
			Labels:    map[string]string{discoveryv1.LabelServiceName: svcName},
		},
		AddressType: addrType,
		Ports:       ports,
		Endpoints:   endpoints,
	}
}

// --- tests ------------------------------------------------------------------

// TestExpandHeadlessBackends_HeadlessWithReadyEndpoints proves a headless Service
// is expanded into one backend per ready endpoint, dialing the endpoint
// targetPort (3000) rather than the Service port (8080).
func TestExpandHeadlessBackends_HeadlessWithReadyEndpoints(t *testing.T) {
	t.Parallel()

	cli := fake.NewClientBuilder().WithScheme(zeScheme(t)).WithObjects(
		headlessSvc("head", "default", svcPort("first-port", 8080, 3000)),
		headlessSlice("head-ip4", "default", "head", discoveryv1.AddressTypeIPv4,
			[]discoveryv1.EndpointPort{epPort("first-port", 3000)},
			[]epAddr{{"10.1.0.1", true}, {"10.1.0.2", true}}),
	).Build()

	cfg := &proxy.Config{Rules: []proxy.RouteRule{{Backends: []proxy.BackendRef{
		{URL: "http://head.default.svc.cluster.local:8080", Weight: 1},
	}}}}

	routes := []*gatewayv1.HTTPRoute{httpRouteToSvc("r", "default", "head", 8080)}

	expandHeadlessBackends(context.Background(), cli, cfg, "cluster.local", routes, nil)

	assert.ElementsMatch(t, []string{
		"http://10.1.0.1:3000",
		"http://10.1.0.2:3000",
	}, backendURLsCtrl(cfg.Rules[0].Backends), "headless Service dials each ready endpoint at the targetPort")
}

// TestExpandHeadlessBackends_NonHeadlessUnchanged proves a normal ClusterIP
// Service (which has a VIP and is translated by kube-proxy) is left routing
// through the Service FQDN.
func TestExpandHeadlessBackends_NonHeadlessUnchanged(t *testing.T) {
	t.Parallel()

	cli := fake.NewClientBuilder().WithScheme(zeScheme(t)).WithObjects(
		clusterIPSvc("app", "default"),
		headlessSlice("app-ip4", "default", "app", discoveryv1.AddressTypeIPv4,
			[]discoveryv1.EndpointPort{epPort("first-port", 3000)},
			[]epAddr{{"10.1.0.1", true}}),
	).Build()

	cfg := &proxy.Config{Rules: []proxy.RouteRule{{Backends: []proxy.BackendRef{
		{URL: "http://app.default.svc.cluster.local:8080", Weight: 1},
	}}}}

	routes := []*gatewayv1.HTTPRoute{httpRouteToSvc("r", "default", "app", 8080)}

	expandHeadlessBackends(context.Background(), cli, cfg, "cluster.local", routes, nil)

	assert.Equal(t, []string{"http://app.default.svc.cluster.local:8080"},
		backendURLsCtrl(cfg.Rules[0].Backends), "a ClusterIP Service must keep routing through its VIP")
}

// TestExpandHeadlessBackends_ExternalNameUnchanged proves an ExternalName Service
// is never expanded (it has no ClusterIP and no EndpointSlices).
func TestExpandHeadlessBackends_ExternalNameUnchanged(t *testing.T) {
	t.Parallel()

	extSvc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "ext", Namespace: "default"},
		Spec:       corev1.ServiceSpec{Type: corev1.ServiceTypeExternalName, ExternalName: "api.example.com"},
	}
	cli := fake.NewClientBuilder().WithScheme(zeScheme(t)).WithObjects(extSvc).Build()

	cfg := &proxy.Config{Rules: []proxy.RouteRule{{Backends: []proxy.BackendRef{
		{URL: "http://ext.default.svc.cluster.local:8080", Weight: 1},
	}}}}

	routes := []*gatewayv1.HTTPRoute{httpRouteToSvc("r", "default", "ext", 8080)}

	expandHeadlessBackends(context.Background(), cli, cfg, "cluster.local", routes, nil)

	assert.Equal(t, []string{"http://ext.default.svc.cluster.local:8080"},
		backendURLsCtrl(cfg.Rules[0].Backends))
}

// TestExpandHeadlessBackends_NoReadyEndpoints_LeavesFQDN proves a headless
// Service with zero ready endpoints keeps its FQDN backend so the downstream
// zero-endpoint pass marks it 503 (rather than silently dropping all backends).
func TestExpandHeadlessBackends_NoReadyEndpoints_LeavesFQDN(t *testing.T) {
	t.Parallel()

	cli := fake.NewClientBuilder().WithScheme(zeScheme(t)).WithObjects(
		headlessSvc("head", "default", svcPort("first-port", 8080, 3000)),
		headlessSlice("head-ip4", "default", "head", discoveryv1.AddressTypeIPv4,
			[]discoveryv1.EndpointPort{epPort("first-port", 3000)},
			[]epAddr{{"10.1.0.1", false}}),
	).Build()

	cfg := &proxy.Config{Rules: []proxy.RouteRule{{Backends: []proxy.BackendRef{
		{URL: "http://head.default.svc.cluster.local:8080", Weight: 1},
	}}}}

	routes := []*gatewayv1.HTTPRoute{httpRouteToSvc("r", "default", "head", 8080)}

	expandHeadlessBackends(context.Background(), cli, cfg, "cluster.local", routes, nil)

	assert.Equal(t, []string{"http://head.default.svc.cluster.local:8080"},
		backendURLsCtrl(cfg.Rules[0].Backends), "no ready endpoints → leave FQDN for the 503 path")
}

// TestExpandHeadlessBackends_OnlyReadyEndpoints proves a not-ready endpoint in an
// otherwise-ready slice is excluded from the expansion.
func TestExpandHeadlessBackends_OnlyReadyEndpoints(t *testing.T) {
	t.Parallel()

	cli := fake.NewClientBuilder().WithScheme(zeScheme(t)).WithObjects(
		headlessSvc("head", "default", svcPort("first-port", 8080, 3000)),
		headlessSlice("head-ip4", "default", "head", discoveryv1.AddressTypeIPv4,
			[]discoveryv1.EndpointPort{epPort("first-port", 3000)},
			[]epAddr{{"10.1.0.1", true}, {"10.1.0.2", false}}),
	).Build()

	cfg := &proxy.Config{Rules: []proxy.RouteRule{{Backends: []proxy.BackendRef{
		{URL: "http://head.default.svc.cluster.local:8080", Weight: 1},
	}}}}

	routes := []*gatewayv1.HTTPRoute{httpRouteToSvc("r", "default", "head", 8080)}

	expandHeadlessBackends(context.Background(), cli, cfg, "cluster.local", routes, nil)

	assert.Equal(t, []string{"http://10.1.0.1:3000"},
		backendURLsCtrl(cfg.Rules[0].Backends), "only ready endpoints are expanded")
}

// TestExpandHeadlessBackends_TargetPortNameMatch proves the endpoint port is
// resolved by matching the EndpointSlice port NAME to the Service port the
// backendRef selected — not by position — when a Service exposes multiple ports.
func TestExpandHeadlessBackends_TargetPortNameMatch(t *testing.T) {
	t.Parallel()

	cli := fake.NewClientBuilder().WithScheme(zeScheme(t)).WithObjects(
		headlessSvc("head", "default",
			svcPort("a", 8080, 3000),
			svcPort("b", 9090, 4000)),
		headlessSlice("head-ip4", "default", "head", discoveryv1.AddressTypeIPv4,
			[]discoveryv1.EndpointPort{epPort("a", 3000), epPort("b", 4000)},
			[]epAddr{{"10.1.0.1", true}}),
	).Build()

	cfg := &proxy.Config{Rules: []proxy.RouteRule{{Backends: []proxy.BackendRef{
		{URL: "http://head.default.svc.cluster.local:8080", Weight: 1},
	}}}}

	// backendRef selects Service port 8080 (name "a") → endpoint port 3000.
	routes := []*gatewayv1.HTTPRoute{httpRouteToSvc("r", "default", "head", 8080)}

	expandHeadlessBackends(context.Background(), cli, cfg, "cluster.local", routes, nil)

	assert.Equal(t, []string{"http://10.1.0.1:3000"},
		backendURLsCtrl(cfg.Rules[0].Backends), "the matching-named port (3000), not the other port (4000)")
}

// TestExpandHeadlessBackends_IPv6Endpoint proves an IPv6 endpoint address is
// bracketed in the dial URL.
func TestExpandHeadlessBackends_IPv6Endpoint(t *testing.T) {
	t.Parallel()

	cli := fake.NewClientBuilder().WithScheme(zeScheme(t)).WithObjects(
		headlessSvc("head", "default", svcPort("first-port", 8080, 3000)),
		headlessSlice("head-ip6", "default", "head", discoveryv1.AddressTypeIPv6,
			[]discoveryv1.EndpointPort{epPort("first-port", 3000)},
			[]epAddr{{"2001:db8::1", true}}),
	).Build()

	cfg := &proxy.Config{Rules: []proxy.RouteRule{{Backends: []proxy.BackendRef{
		{URL: "http://head.default.svc.cluster.local:8080", Weight: 1},
	}}}}

	routes := []*gatewayv1.HTTPRoute{httpRouteToSvc("r", "default", "head", 8080)}

	expandHeadlessBackends(context.Background(), cli, cfg, "cluster.local", routes, nil)

	assert.Equal(t, []string{"http://[2001:db8::1]:3000"},
		backendURLsCtrl(cfg.Rules[0].Backends))
}

// TestExpandHeadlessBackends_SkipsUnauthorizedCrossNamespace proves the pass is
// gated on cfg membership (proxy.UnmarkedBackendHosts): a cross-namespace headless
// backend the converter dropped (no ReferenceGrant) is absent from cfg and must
// never be read, keeping authorization symmetric with the other passes.
func TestExpandHeadlessBackends_SkipsUnauthorizedCrossNamespace(t *testing.T) {
	t.Parallel()

	cli, counts := countingClient(t,
		headlessSvc("secret", "other", svcPort("first-port", 8080, 3000)),
		headlessSlice("secret-ip4", "other", "secret", discoveryv1.AddressTypeIPv4,
			[]discoveryv1.EndpointPort{epPort("first-port", 3000)},
			[]epAddr{{"10.9.0.1", true}}),
	)

	// The converter dropped the unauthorized cross-namespace ref, so it is not
	// in cfg — only the authorized in-namespace backend is.
	cfg := &proxy.Config{Rules: []proxy.RouteRule{{Backends: []proxy.BackendRef{
		{URL: "http://app.default.svc.cluster.local:80", Weight: 1},
	}}}}

	otherNS := gatewayv1.Namespace("other")
	pSecret := gatewayv1.PortNumber(8080)
	pApp := gatewayv1.PortNumber(80)
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "default"},
		Spec: gatewayv1.HTTPRouteSpec{Rules: []gatewayv1.HTTPRouteRule{{
			BackendRefs: []gatewayv1.HTTPBackendRef{
				{BackendRef: gatewayv1.BackendRef{BackendObjectReference: gatewayv1.BackendObjectReference{
					Name: "app", Port: &pApp,
				}}},
				{BackendRef: gatewayv1.BackendRef{BackendObjectReference: gatewayv1.BackendObjectReference{
					Name: "secret", Namespace: &otherNS, Port: &pSecret,
				}}},
			},
		}}},
	}

	expandHeadlessBackends(context.Background(), cli, cfg, "cluster.local",
		[]*gatewayv1.HTTPRoute{route}, nil)

	assert.Zero(t, counts.serviceGets["other/secret"],
		"a dropped cross-namespace backend must never be read by the headless pass")
	assert.Equal(t, []string{"http://app.default.svc.cluster.local:80"},
		backendURLsCtrl(cfg.Rules[0].Backends), "the authorized backend is untouched")
}

// TestExpandHeadlessBackends_GRPCRoute proves a GRPCRoute referencing a headless
// Service is expanded too, preserving the h2c backend protocol.
func TestExpandHeadlessBackends_GRPCRoute(t *testing.T) {
	t.Parallel()

	cli := fake.NewClientBuilder().WithScheme(zeScheme(t)).WithObjects(
		headlessSvc("grpc", "default", svcPort("grpc", 8080, 3000)),
		headlessSlice("grpc-ip4", "default", "grpc", discoveryv1.AddressTypeIPv4,
			[]discoveryv1.EndpointPort{epPort("grpc", 3000)},
			[]epAddr{{"10.1.0.5", true}}),
	).Build()

	cfg := &proxy.Config{Rules: []proxy.RouteRule{{Backends: []proxy.BackendRef{
		{URL: "http://grpc.default.svc.cluster.local:8080", Weight: 1, Protocol: proxy.BackendProtocolH2C},
	}}}}

	port := gatewayv1.PortNumber(8080)
	grpcRoute := &gatewayv1.GRPCRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "g", Namespace: "default"},
		Spec: gatewayv1.GRPCRouteSpec{Rules: []gatewayv1.GRPCRouteRule{{
			BackendRefs: []gatewayv1.GRPCBackendRef{
				{BackendRef: gatewayv1.BackendRef{BackendObjectReference: gatewayv1.BackendObjectReference{
					Name: "grpc", Port: &port,
				}}},
			},
		}}},
	}

	expandHeadlessBackends(context.Background(), cli, cfg, "cluster.local", nil,
		[]*gatewayv1.GRPCRoute{grpcRoute})

	require.Len(t, cfg.Rules[0].Backends, 1)
	assert.Equal(t, "http://10.1.0.5:3000", cfg.Rules[0].Backends[0].URL)
	assert.Equal(t, proxy.BackendProtocolH2C, cfg.Rules[0].Backends[0].Protocol,
		"gRPC h2c protocol is inherited by the expanded endpoint")
}

// TestExpandHeadlessBackends_NamedTargetPortUnresolvable_FailsClosed proves the
// fallback never reintroduces the 502: when a headless Service has READY endpoints
// whose NAMED targetPort the EndpointSlice cannot resolve to a number (multi-port
// slice, no matching port name), the backend is marked 503 rather than left as the
// FQDN backend, which the proxy would dial at the Service port (8080) against a pod
// listening on the targetPort → 502. The zero-endpoint pass cannot catch this (the
// endpoints ARE ready), so the expand pass must fail it closed itself.
func TestExpandHeadlessBackends_NamedTargetPortUnresolvable_FailsClosed(t *testing.T) {
	t.Parallel()

	cli := fake.NewClientBuilder().WithScheme(zeScheme(t)).WithObjects(
		headlessSvc("head", "default",
			svcPortNamedTarget("web", 8080, "http"),
			svcPortNamedTarget("admin", 9090, "adminhttp")),
		// Slice port names do not match the Service port names, so the named
		// targetPort cannot be resolved to a number from the slice.
		headlessSlice("head-ip4", "default", "head", discoveryv1.AddressTypeIPv4,
			[]discoveryv1.EndpointPort{epPort("mismatch-a", 3000), epPort("mismatch-b", 4000)},
			[]epAddr{{"10.1.0.1", true}}),
	).Build()

	cfg := &proxy.Config{Rules: []proxy.RouteRule{{Backends: []proxy.BackendRef{
		{URL: "http://head.default.svc.cluster.local:8080", Weight: 1},
	}}}}

	routes := []*gatewayv1.HTTPRoute{httpRouteToSvc("r", "default", "head", 8080)}

	expandHeadlessBackends(context.Background(), cli, cfg, "cluster.local", routes, nil)

	require.Len(t, cfg.Rules[0].Backends, 1)
	assert.Equal(t, "http://head.default.svc.cluster.local:8080", cfg.Rules[0].Backends[0].URL,
		"an unresolvable named targetPort must NOT be expanded to dial the Service port (8080)")
	assert.Equal(t, http.StatusServiceUnavailable, cfg.Rules[0].Backends[0].UnavailableStatus,
		"ready-but-unresolvable endpoints must fail closed to 503, not fall through to a 502 dial")
}

// TestExpandHeadlessBackends_NumericTargetPortFallback proves that when the
// EndpointSlice port name does not match (multi-port slice) but the Service port
// has a NUMERIC targetPort, that numeric targetPort — the real pod port — is used,
// never the Service port.
func TestExpandHeadlessBackends_NumericTargetPortFallback(t *testing.T) {
	t.Parallel()

	cli := fake.NewClientBuilder().WithScheme(zeScheme(t)).WithObjects(
		headlessSvc("head", "default",
			svcPort("a", 8080, 3000),
			svcPort("b", 9090, 4000)),
		// Slice port names do not match "a"/"b", forcing the numeric-targetPort fallback.
		headlessSlice("head-ip4", "default", "head", discoveryv1.AddressTypeIPv4,
			[]discoveryv1.EndpointPort{epPort("x", 3000), epPort("y", 4000)},
			[]epAddr{{"10.1.0.1", true}}),
	).Build()

	cfg := &proxy.Config{Rules: []proxy.RouteRule{{Backends: []proxy.BackendRef{
		{URL: "http://head.default.svc.cluster.local:8080", Weight: 1},
	}}}}

	routes := []*gatewayv1.HTTPRoute{httpRouteToSvc("r", "default", "head", 8080)}

	expandHeadlessBackends(context.Background(), cli, cfg, "cluster.local", routes, nil)

	assert.Equal(t, []string{"http://10.1.0.1:3000"},
		backendURLsCtrl(cfg.Rules[0].Backends),
		"a numeric targetPort (3000) is the real pod port and must be used as the fallback, not the Service port")
}

// TestExpandHeadlessBackends_DualStack proves a dual-stack headless Service (one
// IPv4 and one IPv6 EndpointSlice) expands to endpoints of both families.
func TestExpandHeadlessBackends_DualStack(t *testing.T) {
	t.Parallel()

	cli := fake.NewClientBuilder().WithScheme(zeScheme(t)).WithObjects(
		headlessSvc("head", "default", svcPort("first-port", 8080, 3000)),
		headlessSlice("head-ip4", "default", "head", discoveryv1.AddressTypeIPv4,
			[]discoveryv1.EndpointPort{epPort("first-port", 3000)},
			[]epAddr{{"10.1.0.1", true}}),
		headlessSlice("head-ip6", "default", "head", discoveryv1.AddressTypeIPv6,
			[]discoveryv1.EndpointPort{epPort("first-port", 3000)},
			[]epAddr{{"2001:db8::1", true}}),
	).Build()

	cfg := &proxy.Config{Rules: []proxy.RouteRule{{Backends: []proxy.BackendRef{
		{URL: "http://head.default.svc.cluster.local:8080", Weight: 1},
	}}}}

	routes := []*gatewayv1.HTTPRoute{httpRouteToSvc("r", "default", "head", 8080)}

	expandHeadlessBackends(context.Background(), cli, cfg, "cluster.local", routes, nil)

	assert.ElementsMatch(t, []string{
		"http://10.1.0.1:3000",
		"http://[2001:db8::1]:3000",
	}, backendURLsCtrl(cfg.Rules[0].Backends), "both IP families are expanded for a dual-stack headless Service")
}

// backendURLsCtrl collects backend URLs for assertions in controller-package tests.
func backendURLsCtrl(backends []proxy.BackendRef) []string {
	urls := make([]string, 0, len(backends))
	for i := range backends {
		urls = append(urls, backends[i].URL)
	}

	return urls
}

// TestResolveHeadlessEndpoints_DeterministicAcrossSliceOrder pins the
// ordering contract the proxy-config content hash depends on: a Service
// whose endpoints span several EndpointSlices (dual-stack, >100 endpoints)
// must resolve to the SAME backend order regardless of the order the
// informer cache lists the slices in -- the cache iterates a map, so
// without sorting the config hash would flap between identical rebuilds
// and silently disable the steady-state push skip.
func TestResolveHeadlessEndpoints_DeterministicAcrossSliceOrder(t *testing.T) {
	t.Parallel()

	svc := headlessSvc("head", "default", svcPort("first-port", 8080, 3000))
	sliceOne := headlessSlice("head-a", "default", "head", discoveryv1.AddressTypeIPv4,
		[]discoveryv1.EndpointPort{epPort("first-port", 3000)},
		[]epAddr{{"10.1.0.9", true}, {"10.1.0.2", true}})
	sliceTwo := headlessSlice("head-b", "default", "head", discoveryv1.AddressTypeIPv4,
		[]discoveryv1.EndpointPort{epPort("first-port", 3000)},
		[]epAddr{{"10.1.0.5", true}, {"10.1.0.1", true}})

	expected := []proxy.ResolvedEndpoint{
		{Host: "10.1.0.1", Port: 3000},
		{Host: "10.1.0.2", Port: 3000},
		{Host: "10.1.0.5", Port: 3000},
		{Host: "10.1.0.9", Port: 3000},
	}

	for _, objects := range [][]struct {
		name  string
		slice *discoveryv1.EndpointSlice
	}{
		{{"one-first", sliceOne}, {"two-second", sliceTwo}},
		{{"two-first", sliceTwo}, {"one-second", sliceOne}},
	} {
		cli := fake.NewClientBuilder().WithScheme(zeScheme(t)).WithObjects(
			svc.DeepCopy(), objects[0].slice.DeepCopy(), objects[1].slice.DeepCopy(),
		).Build()

		endpoints, hadReady := resolveHeadlessEndpoints(context.Background(), cli, svc, "default", "head", 8080)

		assert.True(t, hadReady)
		assert.Equal(t, expected, endpoints,
			"endpoint order must be deterministic regardless of slice list order")
	}
}
