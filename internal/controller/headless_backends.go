package controller

import (
	"context"
	"net/http"
	"sort"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
)

// expandHeadlessBackends replaces the Service-FQDN backend of every headless
// Service (spec.clusterIP: None) with one backend per ready endpoint, dialing the
// endpoint targetPort instead of the Service port. A headless Service has no VIP,
// so its FQDN resolves straight to the pod IPs and dialing the Service port
// (8080) reaches a pod listening on the targetPort (3000) and fails (502). This
// pass resolves the EndpointSlices and hands the addresses to the proxy's
// weighted multi-backend selection.
//
// It runs after the 500 invalid-ref marking and before the 503 zero-endpoint
// marking: a headless Service with ready endpoints is expanded (so its FQDN host
// disappears from cfg and the 503 pass skips it), while a headless Service with
// no ready endpoints is left as the FQDN backend so the 503 pass marks it.
//
// Only backends that are present and unmarked in cfg are inspected — the same
// proxy.UnmarkedBackendHosts gate as the 503 pass — so a ref the converter
// dropped (unauthorized cross-namespace, invalid kind/port) is never read,
// keeping ReferenceGrant authorization symmetric across passes. Each Service
// identity is inspected once per reconcile.
func expandHeadlessBackends(
	ctx context.Context,
	cli client.Client,
	cfg *proxy.Config,
	clusterDomain string,
	routes []*gatewayv1.HTTPRoute,
	grpcRoutes []*gatewayv1.GRPCRoute,
) {
	if cli == nil || cfg == nil {
		return
	}

	visitAuthorizedServiceBackends(ctx, cfg, clusterDomain, routes, grpcRoutes,
		func(ctx context.Context, svcNamespace, name string, port int32) {
			expandBackendIfHeadless(ctx, cli, cfg, clusterDomain, svcNamespace, name, port)
		})
}

// expandBackendIfHeadless expands the backend when the named Service is headless
// (clusterIP: None) and has at least one ready endpoint. A normal Service keeps
// routing through its VIP; an ExternalName Service is never expanded; a headless
// Service with no ready endpoints is left for the 503 pass.
func expandBackendIfHeadless(
	ctx context.Context,
	cli client.Client,
	cfg *proxy.Config,
	clusterDomain, svcNamespace, name string,
	port int32,
) {
	var svc corev1.Service
	if err := cli.Get(ctx, client.ObjectKey{Name: name, Namespace: svcNamespace}, &svc); err != nil {
		// Missing Service is the 500 invalid-ref path; any other error leaves the
		// backend to its normal dial behaviour. Either way, do not expand.
		return
	}

	if svc.Spec.Type == corev1.ServiceTypeExternalName || svc.Spec.ClusterIP != corev1.ClusterIPNone {
		return
	}

	endpoints, hadReady := resolveHeadlessEndpoints(ctx, cli, &svc, svcNamespace, name, port)
	if len(endpoints) > 0 {
		proxy.ExpandHeadlessBackend(cfg, clusterDomain, svcNamespace, name, port, endpoints)

		return
	}

	if hadReady {
		// Ready endpoints exist but none could be resolved to a dialable port
		// (e.g. a named targetPort absent from the EndpointSlice). Fail closed:
		// mark the backend 503 rather than leaving the FQDN backend to be dialed
		// at the Service port and 502. The zero-endpoint 503 pass cannot catch
		// this — it sees the endpoints as ready — so the marking must happen here.
		proxy.MarkUnavailableBackends(cfg, clusterDomain, svcNamespace, name, port, http.StatusServiceUnavailable)
	}

	// No ready endpoints at all: leave the FQDN backend for the zero-endpoint pass.
}

// resolveHeadlessEndpoints lists the Service's EndpointSlices and returns one
// ResolvedEndpoint per ready endpoint address, dialing the endpoint port that
// corresponds (by port name) to the Service port the backendRef selected. The
// second return reports whether any ready endpoint existed at all (even one whose
// port could not be resolved), so the caller can distinguish "no ready endpoints"
// (leave for the zero-endpoint 503 pass) from "ready endpoints but unresolvable
// port" (fail closed).
func resolveHeadlessEndpoints(
	ctx context.Context,
	cli client.Client,
	svc *corev1.Service,
	namespace, name string,
	port int32,
) ([]proxy.ResolvedEndpoint, bool) {
	portName, fallbackPort := serviceTargetPort(svc, port)

	var slices discoveryv1.EndpointSliceList
	if err := cli.List(ctx, &slices,
		client.InNamespace(namespace),
		client.MatchingLabels{discoveryv1.LabelServiceName: name},
	); err != nil {
		return nil, false
	}

	var endpoints []proxy.ResolvedEndpoint

	hadReady := false

	for sliceIdx := range slices.Items {
		eps, ready := sliceEndpoints(&slices.Items[sliceIdx], portName, fallbackPort)
		endpoints = append(endpoints, eps...)
		hadReady = hadReady || ready
	}

	// The informer cache lists EndpointSlices in map-iteration order, so a
	// Service whose endpoints span several slices (dual-stack, >100
	// endpoints) would otherwise produce a different backend order on every
	// rebuild -- flapping the proxy-config content hash and silently
	// disabling the steady-state push skip. Sort for a deterministic config.
	sort.Slice(endpoints, func(i, j int) bool {
		if endpoints[i].Host != endpoints[j].Host {
			return endpoints[i].Host < endpoints[j].Host
		}

		return endpoints[i].Port < endpoints[j].Port
	})

	return endpoints, hadReady
}

// serviceTargetPort returns the name of the Service port matching the backendRef
// port (used to select the EndpointSlice port) and a numeric fallback: the Service
// port's numeric targetPort, or 0 when the targetPort is a named port or no
// Service port matches. A 0 fallback means only a port resolved from the
// EndpointSlice is safe to dial — see sliceEndpoints. The Service port itself is
// never a fallback: dialing it on a headless endpoint reaches a pod listening on
// the (different) targetPort and 502s, which is the bug this pass fixes.
func serviceTargetPort(svc *corev1.Service, port int32) (string, int32) {
	for i := range svc.Spec.Ports {
		svcPort := svc.Spec.Ports[i]
		if svcPort.Port != port {
			continue
		}

		if svcPort.TargetPort.Type == intstr.Int && svcPort.TargetPort.IntVal > 0 {
			return svcPort.Name, svcPort.TargetPort.IntVal
		}

		return svcPort.Name, 0
	}

	return "", 0
}

// sliceEndpoints returns the ready endpoint addresses of one EndpointSlice, each
// paired with the resolved endpoint port, plus whether the slice had any ready
// endpoint at all. When the slice does not unambiguously resolve the port and there
// is no numeric targetPort fallback, its endpoints are not dialed (a headless
// endpoint dialed at the Service port 502s) — but a ready-endpoint sighting is
// still reported via the second return so the caller can fail the backend closed
// (503) instead of letting the FQDN backend fall through to a 502 dial.
func sliceEndpoints(slice *discoveryv1.EndpointSlice, portName string, fallback int32) ([]proxy.ResolvedEndpoint, bool) {
	endpointPort, resolvable := endpointSlicePort(slice, portName)
	if !resolvable && fallback > 0 {
		endpointPort = fallback
		resolvable = true
	}

	var out []proxy.ResolvedEndpoint

	hadReady := false

	for epIdx := range slice.Endpoints {
		ready := slice.Endpoints[epIdx].Conditions.Ready
		if ready == nil || !*ready {
			continue
		}

		hadReady = true

		if !resolvable {
			continue
		}

		for _, addr := range slice.Endpoints[epIdx].Addresses {
			out = append(out, proxy.ResolvedEndpoint{Host: addr, Port: endpointPort})
		}
	}

	return out, hadReady
}

// endpointSlicePort resolves the endpoint port from the slice: the port whose name
// matches the selected Service port, or the sole port of a single-port slice. The
// bool is false when the slice does not unambiguously resolve the port (a
// multi-port slice with no name match), so the caller can fall back to a numeric
// targetPort or skip rather than dial a guessed port.
func endpointSlicePort(slice *discoveryv1.EndpointSlice, portName string) (int32, bool) {
	for i := range slice.Ports {
		slicePort := slice.Ports[i]
		if slicePort.Port == nil {
			continue
		}

		named := slicePort.Name != nil && *slicePort.Name == portName
		unnamed := slicePort.Name == nil && portName == ""

		if named || unnamed {
			return *slicePort.Port, true
		}
	}

	if len(slice.Ports) == 1 && slice.Ports[0].Port != nil {
		return *slice.Ports[0].Port, true
	}

	return 0, false
}
