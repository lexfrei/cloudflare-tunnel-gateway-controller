package controller

import (
	"context"
	"slices"

	"github.com/cockroachdb/errors"

	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/routebinding"
)

// redirectSchemeHTTP and redirectSchemeHTTPS are the only two values the
// Gateway API permits for HTTPRequestRedirectFilter.Scheme
// (+kubebuilder:validation:Enum=http;https).
const (
	redirectSchemeHTTP  = "http"
	redirectSchemeHTTPS = "https"
)

// withDefaultRedirectScheme returns copies of the given routes in which every
// RequestRedirect filter, at rule or backendRef level, that leaves Scheme
// empty has it defaulted to the scheme implied by the parent listener the
// route binds to, and its Port, when also empty, to that listener's port (see
// redirectPort).
//
// Why: the Gateway API says of HTTPRequestRedirectFilter.Scheme "when empty,
// the scheme of the request is used". Behind a Cloudflare Tunnel the proxy
// never sees the original wire scheme (cloudflared terminates TLS at the edge
// and hands the origin a scheme-less server request), so "the scheme of the
// request" has to be reconstructed from the listener the route is attached to:
// an HTTP listener implies http, an HTTPS/TLS listener implies https. Without
// this the proxy falls back to a hardcoded https for every scheme-less
// redirect, which violates the spec for routes on an HTTP listener.
//
// Only listeners that actually ACCEPT the route contribute, reusing the same
// binding validator as hostname-intersection narrowing, and only on Gateways
// this controller manages — another implementation's listener says nothing
// about the scheme a request reaching US arrived on. When no managed parent
// resolves, the filter's Scheme is left nil and the proxy's own fallback
// applies.
//
// The function never mutates the input routes; a route is deep-copied only
// when it has at least one scheme-less redirect filter AND a parent scheme was
// resolved.
func withDefaultRedirectScheme(
	ctx context.Context,
	cli client.Client,
	controllerName string,
	routes []*gatewayv1.HTTPRoute,
	views *listenerViewCache,
) []*gatewayv1.HTTPRoute {
	if len(routes) == 0 {
		return routes
	}

	views = views.orNew(cli)
	validator := routebinding.NewValidator(cli)
	out := make([]*gatewayv1.HTTPRoute, len(routes))

	for i, route := range routes {
		out[i] = defaultRedirectSchemeForRoute(ctx, cli, controllerName, validator, route, views)
	}

	return out
}

// defaultRedirectSchemeForRoute returns route unchanged when it carries no
// scheme-less redirect filter or no parent scheme resolves; otherwise it
// returns a deep copy with the empty redirect schemes filled in.
func defaultRedirectSchemeForRoute(
	ctx context.Context,
	cli client.Client,
	controllerName string,
	validator *routebinding.Validator,
	route *gatewayv1.HTTPRoute,
	views *listenerViewCache,
) *gatewayv1.HTTPRoute {
	if !routeHasEmptyRedirectScheme(route) {
		return route
	}

	scheme, port := acceptedListenerScheme(ctx, cli, controllerName, validator, HTTPRouteWrapper{route}, views)
	if scheme == "" {
		return route
	}

	clone := route.DeepCopy()
	applyDefaultRedirectScheme(clone, scheme, port)

	return clone
}

// redirectFilters returns pointers to every RequestRedirect filter on the
// route, at BOTH the rule level (Rules[].Filters) and the backendRef level
// (Rules[].BackendRefs[].Filters). The Gateway API permits RequestRedirect in
// both places (backendRef-level support is Extended) and the proxy executes
// both — rule filters via result.Filters and backendRef filters via
// result.BackendFilters — so scheme defaulting must reach both. The pointers
// alias the route's backing arrays, so mutating through them mutates the route
// (callers pass a deep copy when they intend to write).
func redirectFilters(route *gatewayv1.HTTPRoute) []*gatewayv1.HTTPRouteFilter {
	var out []*gatewayv1.HTTPRouteFilter

	for ruleIdx := range route.Spec.Rules {
		rule := &route.Spec.Rules[ruleIdx]

		for filterIdx := range rule.Filters {
			out = append(out, &rule.Filters[filterIdx])
		}

		for backendIdx := range rule.BackendRefs {
			backendFilters := rule.BackendRefs[backendIdx].Filters
			for filterIdx := range backendFilters {
				out = append(out, &backendFilters[filterIdx])
			}
		}
	}

	return out
}

// routeHasEmptyRedirectScheme reports whether any RequestRedirect filter on the
// route (rule level or backendRef level) leaves Scheme unset — the only case
// the defaulting touches.
func routeHasEmptyRedirectScheme(route *gatewayv1.HTTPRoute) bool {
	return slices.ContainsFunc(redirectFilters(route), isEmptySchemeRedirect)
}

// applyDefaultRedirectScheme sets the resolved scheme on every scheme-less
// RequestRedirect filter of the (already cloned) route, at both the rule and
// backendRef levels, and the listener port where the filter names none: "If
// redirect scheme is empty, the redirect port MUST be the Gateway Listener
// port". A nil port is the scheme's well-known one, which Location leaves out.
func applyDefaultRedirectScheme(route *gatewayv1.HTTPRoute, scheme string, port *gatewayv1.PortNumber) {
	for _, filter := range redirectFilters(route) {
		if !isEmptySchemeRedirect(filter) {
			continue
		}

		filter.RequestRedirect.Scheme = new(scheme)

		if filter.RequestRedirect.Port == nil && port != nil {
			filter.RequestRedirect.Port = new(*port)
		}
	}
}

func isEmptySchemeRedirect(filter *gatewayv1.HTTPRouteFilter) bool {
	return filter.Type == gatewayv1.HTTPRouteFilterRequestRedirect &&
		filter.RequestRedirect != nil &&
		filter.RequestRedirect.Scheme == nil
}

// acceptedListener is the protocol and port of one listener that accepts the
// route.
type acceptedListener struct {
	protocol gatewayv1.ProtocolType
	port     gatewayv1.PortNumber
}

// acceptedListenerScheme returns the redirect scheme implied by the listeners
// that accept the route: "https" if any accepting listener terminates HTTPS,
// otherwise "http" if at least one accepting HTTP listener resolves, or "" when
// no managed L7 parent accepts the route. HTTPS wins ties so a route bound to
// both an HTTP and an HTTPS listener defaults to the more secure scheme.
//
// The port is that of a listener with the winning protocol, picked by
// redirectPort. The proxy cannot see which listener a request arrived on, so
// with several candidates the well-known port wins, then the lowest one.
//
// Only HTTP and HTTPS listeners are considered: a TLS/TCP/UDP listener never
// accepts an HTTPRoute (the binding validator's default kinds for those
// protocols exclude HTTPRoute), so such a listener never appears in the
// accepted set and contributes no scheme. TLS in particular is terminated at
// the Cloudflare edge and is not a supported frontend listener for HTTPRoute.
func acceptedListenerScheme(
	ctx context.Context,
	cli client.Client,
	controllerName string,
	validator *routebinding.Validator,
	route Route,
	views *listenerViewCache,
) (string, *gatewayv1.PortNumber) {
	byProtocol := make(map[gatewayv1.ProtocolType][]gatewayv1.PortNumber)

	for _, ref := range route.GetParentRefs() {
		for _, listener := range acceptedListenersForParentRef(ctx, cli, controllerName, validator, route, ref, views) {
			byProtocol[listener.protocol] = append(byProtocol[listener.protocol], listener.port)
		}
	}

	if ports := byProtocol[gatewayv1.HTTPSProtocolType]; len(ports) > 0 {
		return redirectSchemeHTTPS, redirectPort(ports, edgeHTTPSPorts())
	}

	if ports := byProtocol[gatewayv1.HTTPProtocolType]; len(ports) > 0 {
		return redirectSchemeHTTP, redirectPort(ports, edgeHTTPPorts())
	}

	return "", nil
}

// edgeHTTPPorts and edgeHTTPSPorts return the ports the Cloudflare edge
// proxies for each scheme, well-known port first
// (https://developers.cloudflare.com/fundamentals/reference/network-ports/).
func edgeHTTPPorts() []gatewayv1.PortNumber {
	return []gatewayv1.PortNumber{80, 8080, 8880, 2052, 2082, 2086, 2095}
}

func edgeHTTPSPorts() []gatewayv1.PortNumber {
	return []gatewayv1.PortNumber{443, 2053, 2083, 2087, 2096, 8443}
}

// redirectPort picks the Location port from the accepting listeners' ports:
// nil when one of them is the scheme's well-known port or none is a port the
// edge serves for the scheme, else the lowest edge-served one. A client
// reaches the listener only through the edge, so a port the edge does not
// serve cannot be where the request arrived, and redirecting to it would
// fail; such a redirect carries no port, so the scheme's well-known one
// applies.
func redirectPort(ports, edgePorts []gatewayv1.PortNumber) *gatewayv1.PortNumber {
	if slices.Contains(ports, edgePorts[0]) {
		return nil
	}

	served := slices.DeleteFunc(slices.Clone(ports), func(port gatewayv1.PortNumber) bool {
		return !slices.Contains(edgePorts, port)
	})
	if len(served) == 0 {
		return nil
	}

	return new(slices.Min(served))
}

// acceptedListenersForParentRef mirrors effectiveHostnamesForParentRef but
// collects the protocol and port of the listeners that accept the route, so
// the redirect scheme and port defaults can be inferred from the parent
// listener. Both share
// the parentRef → managed Gateway / ListenerSet resolution in
// resolveParentRefListeners; only the per-listener value extracted differs.
func acceptedListenersForParentRef(
	ctx context.Context,
	cli client.Client,
	controllerName string,
	validator *routebinding.Validator,
	route Route,
	ref gatewayv1.ParentReference,
	views *listenerViewCache,
) []acceptedListener {
	// A parent that cannot be evaluated lends no protocol. The hostname pass
	// runs first and has already left out any route that no other parent
	// lends a hostname to.
	protocols, _ := resolveParentRefListeners(ctx, cli, controllerName, validator, route, ref, views,
		gatewayAcceptedListeners, listenerSetAcceptedListeners)

	return protocols
}

func gatewayAcceptedListeners(
	ctx context.Context,
	cli client.Client,
	controllerName string,
	validator *routebinding.Validator,
	namespace, name string,
	routeInfo *routebinding.RouteInfo,
	views *listenerViewCache,
) ([]acceptedListener, error) {
	var gateway gatewayv1.Gateway
	if err := cli.Get(ctx, client.ObjectKey{Name: name, Namespace: namespace}, &gateway); err != nil {
		return nil, errors.Wrap(client.IgnoreNotFound(err), "reading Gateway")
	}

	if gatewayOwnedElsewhere(ctx, cli, &gateway, controllerName) {
		return nil, nil
	}

	result, err := bindGatewayListeners(ctx, cli, validator, &gateway, routeInfo, views)
	if err != nil || !result.Accepted {
		return nil, err
	}

	byName := make(map[gatewayv1.SectionName]acceptedListener, len(gateway.Spec.Listeners))
	for i := range gateway.Spec.Listeners {
		listener := &gateway.Spec.Listeners[i]
		byName[listener.Name] = acceptedListener{protocol: listener.Protocol, port: listener.Port}
	}

	return listenersForSections(result.MatchedListeners, byName), nil
}

func listenerSetAcceptedListeners(
	ctx context.Context,
	cli client.Client,
	controllerName string,
	validator *routebinding.Validator,
	namespace, name string,
	routeInfo *routebinding.RouteInfo,
	views *listenerViewCache,
) ([]acceptedListener, error) {
	var listenerSet gatewayv1.ListenerSet
	if err := cli.Get(ctx, client.ObjectKey{Name: name, Namespace: namespace}, &listenerSet); err != nil {
		return nil, errors.Wrap(client.IgnoreNotFound(err), "reading ListenerSet")
	}

	if excluded, err := listenerSetExcluded(ctx, cli, controllerName, validator, &listenerSet); err != nil || excluded {
		return nil, err
	}

	result, err := validator.ValidateBindingForListenerSet(ctx, &listenerSet, routeInfo)
	if err != nil || !result.Accepted {
		return nil, errors.Wrap(err, "validating binding against ListenerSet")
	}

	// Drop sections whose merged-view entry is conflicted — a conflicted
	// listener is not programmed, so a route must not infer its scheme from
	// that listener's protocol. Same step the hostname-narrowing path takes.
	matched := nonConflictedSections(ctx, cli, &listenerSet, result.MatchedListeners, views)

	byName := make(map[gatewayv1.SectionName]acceptedListener, len(listenerSet.Spec.Listeners))
	for i := range listenerSet.Spec.Listeners {
		entry := &listenerSet.Spec.Listeners[i]
		byName[entry.Name] = acceptedListener{protocol: entry.Protocol, port: entry.Port}
	}

	return listenersForSections(matched, byName), nil
}

// listenersForSections maps each accepted listener section to its protocol
// and port, mirroring effectiveHostnamesForSections on the hostname-narrowing
// path.
func listenersForSections(
	sections []gatewayv1.SectionName,
	byName map[gatewayv1.SectionName]acceptedListener,
) []acceptedListener {
	var out []acceptedListener

	for _, section := range sections {
		if listener, ok := byName[section]; ok && listener.protocol != "" {
			out = append(out, listener)
		}
	}

	return out
}
