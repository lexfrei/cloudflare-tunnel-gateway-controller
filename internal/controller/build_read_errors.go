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

// errBackendRefsUndecided reports a partition left unpushed because an input
// of its config could not be read: a backend reference's ReferenceGrants, or
// a backend's TLS, appProtocol or client-certificate settings. The data plane
// keeps the config it has, and the sync is retried.
var errBackendRefsUndecided = errors.New("a backend's settings could not be read; proxy config not pushed")

// buildReadErrors collects the read failures of one proxy config build. The
// converter's validators and resolvers only answer with a value, so a failure
// is recorded here and the build is not pushed.
type buildReadErrors struct {
	mu         sync.Mutex
	namespaces map[string]bool
	backend    bool
	first      error
}

type buildReadErrorsKey struct{}

// withBuildReadErrors returns a context whose validators and resolvers record
// read failures in the returned collector.
func withBuildReadErrors(ctx context.Context) (context.Context, *buildReadErrors) {
	collected := &buildReadErrors{namespaces: map[string]bool{}}

	return context.WithValue(ctx, buildReadErrorsKey{}, collected), collected
}

// noteGrantReadError records that a reference from fromNamespace could not be
// evaluated. Without a collector on ctx it does nothing.
func noteGrantReadError(ctx context.Context, fromNamespace string, err error) {
	recordBuildReadError(ctx, func(collected *buildReadErrors) { collected.namespaces[fromNamespace] = true }, err)
}

// noteBackendReadError records that a backend's TLS, appProtocol or client
// certificate settings could not be read. Without a collector on ctx it does
// nothing.
func noteBackendReadError(ctx context.Context, err error) {
	recordBuildReadError(ctx, func(collected *buildReadErrors) { collected.backend = true }, err)
}

func recordBuildReadError(ctx context.Context, mark func(*buildReadErrors), err error) {
	collected, ok := ctx.Value(buildReadErrorsKey{}).(*buildReadErrors)
	if !ok {
		return
	}

	collected.mu.Lock()
	defer collected.mu.Unlock()

	mark(collected)

	if collected.first == nil {
		collected.first = err
	}
}

// err returns the first recorded failure, or nil.
func (g *buildReadErrors) err() error {
	g.mu.Lock()
	defer g.mu.Unlock()

	return g.first
}

// markUndecided turns the ResolvedRefs diagnostics the converter derived from
// an unread input into undecided ones, so route status keeps its previous
// ResolvedRefs verdict. A grant read failure was taken for a denial, so the
// RefNotPermitted diagnostics of routes in that namespace are marked; it keys
// on the route's namespace alone, as a diagnostic does not name its target,
// so a real denial beside an unread grant waits for the retry too. A backend
// settings read failure resolved to fail-closed values whose diagnostics name
// no route either, so every ResolvedRefs diagnostic is marked. The build is
// not pushed either way.
func (g *buildReadErrors) markUndecided(diagnostics []proxy.RouteDiagnostic) {
	g.mu.Lock()
	defer g.mu.Unlock()

	for i := range diagnostics {
		diag := &diagnostics[i]
		if diag.Target != proxy.DiagnosticResolvedRefs {
			continue
		}

		if g.backend || g.namespaces[diag.Namespace] && diag.Reason == string(gatewayv1.RouteReasonRefNotPermitted) {
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
