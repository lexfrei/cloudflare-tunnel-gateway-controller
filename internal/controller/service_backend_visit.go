package controller

import (
	"context"

	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
)

// serviceBackendAction is invoked once per distinct authorized, unmarked Service
// backend referenced by the routes. The caller's closure captures cli/cfg/
// clusterDomain; the visitor supplies the resolved Service identity.
type serviceBackendAction func(ctx context.Context, svcNamespace, name string, port int32)

// visitAuthorizedServiceBackends invokes action once per distinct Service backend
// that is present and unmarked in cfg (proxy.UnmarkedBackendHosts) and referenced
// by the given HTTP or gRPC routes. Gating on cfg membership keeps authorization
// symmetric across the post-conversion passes (headless expansion, zero-endpoint
// 503 marking): a ref the converter dropped — unauthorized cross-namespace,
// invalid kind/port — or one already marked is never visited. Each Service
// identity (host) is visited once per reconcile even when many rules reference it.
func visitAuthorizedServiceBackends(
	ctx context.Context,
	cfg *proxy.Config,
	clusterDomain string,
	routes []*gatewayv1.HTTPRoute,
	grpcRoutes []*gatewayv1.GRPCRoute,
	action serviceBackendAction,
) {
	authorized := proxy.UnmarkedBackendHosts(cfg)
	if len(authorized) == 0 {
		return
	}

	visitor := &serviceBackendVisitor{
		clusterDomain: clusterDomain,
		authorized:    authorized,
		seen:          make(map[string]struct{}),
		action:        action,
	}

	for _, route := range routes {
		for ruleIdx := range route.Spec.Rules {
			for _, ref := range route.Spec.Rules[ruleIdx].BackendRefs {
				visitor.visit(ctx, route.Namespace, ref.BackendObjectReference)
			}
		}
	}

	for _, route := range grpcRoutes {
		for ruleIdx := range route.Spec.Rules {
			for _, ref := range route.Spec.Rules[ruleIdx].BackendRefs {
				visitor.visit(ctx, route.Namespace, ref.BackendObjectReference)
			}
		}
	}
}

// serviceBackendVisitor carries the per-reconcile state shared by the
// post-conversion Service-backend passes: the set of authorized, unmarked backend
// hosts from cfg, the set of Service identities already visited (dedup), and the
// action to run per distinct backend.
type serviceBackendVisitor struct {
	clusterDomain string
	authorized    map[string]struct{}
	seen          map[string]struct{}
	action        serviceBackendAction
}

// visit runs the action for one backendRef unless it is not a Service ref, was
// dropped/marked by the converter (absent from the authorized set), or has
// already been visited this reconcile.
func (v *serviceBackendVisitor) visit(ctx context.Context, routeNamespace string, ref gatewayv1.BackendObjectReference) {
	if !proxy.IsServiceBackendRef(ref) {
		return
	}

	svcNamespace := routeNamespace
	if ref.Namespace != nil {
		svcNamespace = string(*ref.Namespace)
	}

	port := defaultBackendPort
	if ref.Port != nil {
		// gatewayv1.PortNumber is a type alias for int32, so no conversion.
		port = *ref.Port
	}

	name := string(ref.Name)

	host := proxy.ServiceBackendHost(v.clusterDomain, svcNamespace, name, port)
	if _, ok := v.authorized[host]; !ok {
		return
	}

	if _, dup := v.seen[host]; dup {
		return
	}

	v.seen[host] = struct{}{}

	v.action(ctx, svcNamespace, name, port)
}
