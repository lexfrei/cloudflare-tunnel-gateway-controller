package controller

import (
	"context"
	"slices"
	"sync"

	"github.com/cockroachdb/errors"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/ingress"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
)

// errBackendRefsUndecided reports a partition left unpushed because a backend
// reference in it could not be evaluated. The data plane keeps the config it
// has, and the sync is retried.
var errBackendRefsUndecided = errors.New("a backend reference's ReferenceGrants could not be read; proxy config not pushed")

// grantReadErrors collects the ReferenceGrant read failures of one proxy
// config build. The converter's validator only answers allowed or not, so a
// failure is recorded here and the build is not pushed.
type grantReadErrors struct {
	mu         sync.Mutex
	namespaces map[string]bool
	first      error
}

type grantReadErrorsKey struct{}

// withGrantReadErrors returns a context whose backend validators record read
// failures in the returned collector.
func withGrantReadErrors(ctx context.Context) (context.Context, *grantReadErrors) {
	collected := &grantReadErrors{namespaces: map[string]bool{}}

	return context.WithValue(ctx, grantReadErrorsKey{}, collected), collected
}

// noteGrantReadError records that a reference from fromNamespace could not be
// evaluated. Without a collector on ctx it does nothing.
func noteGrantReadError(ctx context.Context, fromNamespace string, err error) {
	collected, ok := ctx.Value(grantReadErrorsKey{}).(*grantReadErrors)
	if !ok {
		return
	}

	collected.mu.Lock()
	defer collected.mu.Unlock()

	collected.namespaces[fromNamespace] = true

	if collected.first == nil {
		collected.first = err
	}
}

// err returns the first recorded failure, or nil.
func (g *grantReadErrors) err() error {
	g.mu.Lock()
	defer g.mu.Unlock()

	return g.first
}

// markUndecided turns the RefNotPermitted ResolvedRefs diagnostics of routes in
// a namespace whose grants could not be read into undecided ones: the
// converter took the read failure for a denial. It keys on the route's
// namespace alone, as a diagnostic does not name its target, so a real denial
// beside an unread grant waits for the retry too; the build is not pushed
// either way.
func (g *grantReadErrors) markUndecided(diagnostics []proxy.RouteDiagnostic) {
	g.mu.Lock()
	defer g.mu.Unlock()

	for i := range diagnostics {
		diag := &diagnostics[i]
		if diag.Target == proxy.DiagnosticResolvedRefs && g.namespaces[diag.Namespace] &&
			diag.Reason == string(gatewayv1.RouteReasonRefNotPermitted) {
			diag.Reason = routeReasonRefsUndecided
		}
	}
}

// partitionRefsUndecided reports whether the ingress builder could not
// evaluate a backend reference of one of the partition's routes.
func partitionRefsUndecided(
	routes []*gatewayv1.HTTPRoute,
	grpcRoutes []*gatewayv1.GRPCRoute,
	failedRefs, grpcFailedRefs []ingress.BackendRefError,
) bool {
	undecidedIn := func(refs []ingress.BackendRefError, namespace, name string) bool {
		return slices.ContainsFunc(refs, func(ref ingress.BackendRefError) bool {
			return ref.Undecided && ref.RouteNamespace == namespace && ref.RouteName == name
		})
	}

	return slices.ContainsFunc(routes, func(route *gatewayv1.HTTPRoute) bool {
		return undecidedIn(failedRefs, route.Namespace, route.Name)
	}) || slices.ContainsFunc(grpcRoutes, func(route *gatewayv1.GRPCRoute) bool {
		return undecidedIn(grpcFailedRefs, route.Namespace, route.Name)
	})
}
