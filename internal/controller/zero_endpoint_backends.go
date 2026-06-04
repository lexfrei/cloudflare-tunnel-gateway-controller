package controller

import (
	"context"
	"net/http"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
)

const defaultBackendPort int32 = 80

// markZeroEndpointBackends marks a backend 503 when its Service exists but has
// no ready endpoints. Per the Gateway API spec a backendRef pointing at a
// Service with no ready endpoints SHOULD return 503 for that backend's traffic
// fraction (the same per-fraction rules as the 500 invalid-ref case). The
// backend stays in the weighted pool so the fraction is preserved.
//
// Only backends that are present and unmarked in cfg are inspected. The
// converter emits a backend solely for a ref that passed kind/port validation
// and (for cross-namespace refs) ReferenceGrant authorization, so gating on cfg
// membership keeps authorization symmetric with the 500 invalid-ref path: a ref
// the converter dropped is never read here. Backends already marked (500) are
// excluded too, so the first-marking-wins precedence holds without a redundant
// lookup. Each Service identity (host) is inspected once per reconcile even when
// many rules reference it.
//
// A nonexistent Service is skipped (it is the 500 path); an ExternalName Service
// is skipped because it has no EndpointSlices yet is legitimately reachable.
func markZeroEndpointBackends(
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
			markBackendIfZeroEndpoints(ctx, cli, cfg, clusterDomain, svcNamespace, name, port)
		})
}

// markBackendIfZeroEndpoints marks the backend 503 when the named Service is a
// cluster Service (not ExternalName) that exists and has no ready endpoints. The
// caller has already confirmed the backend is authorized and present in cfg.
func markBackendIfZeroEndpoints(
	ctx context.Context,
	cli client.Client,
	cfg *proxy.Config,
	clusterDomain, svcNamespace, name string,
	port int32,
) {
	var svc corev1.Service
	if err := cli.Get(ctx, client.ObjectKey{Name: name, Namespace: svcNamespace}, &svc); err != nil {
		// Service missing → that's the 500 invalid-ref path, handled by the
		// ingress builder's failed-ref reporting. Any other lookup error →
		// leave the backend to its normal dial behaviour. Either way, do not
		// mark 503 here.
		return
	}

	// ExternalName Services have no EndpointSlices but are reachable.
	if svc.Spec.Type == corev1.ServiceTypeExternalName {
		return
	}

	if serviceHasReadyEndpoint(ctx, cli, svcNamespace, name) {
		return
	}

	proxy.MarkUnavailableBackends(cfg, clusterDomain, svcNamespace, name, port, http.StatusServiceUnavailable)
}

// serviceHasReadyEndpoint reports whether any EndpointSlice of the named
// Service carries at least one Ready endpoint.
func serviceHasReadyEndpoint(ctx context.Context, cli client.Client, namespace, serviceName string) bool {
	var slices discoveryv1.EndpointSliceList

	err := cli.List(ctx, &slices,
		client.InNamespace(namespace),
		client.MatchingLabels{discoveryv1.LabelServiceName: serviceName},
	)
	if err != nil {
		// On a list error, assume ready so we never wrongly 503 a backend we
		// failed to inspect — the dial path still applies.
		return true
	}

	for sliceIdx := range slices.Items {
		for endpointIdx := range slices.Items[sliceIdx].Endpoints {
			ready := slices.Items[sliceIdx].Endpoints[endpointIdx].Conditions.Ready
			if ready != nil && *ready {
				return true
			}
		}
	}

	return false
}
