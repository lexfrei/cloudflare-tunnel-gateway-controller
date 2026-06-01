package controller

import (
	"context"
	"slices"

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
// rule-level RequestRedirect filter that leaves Scheme empty has it defaulted
// to the scheme implied by the parent listener the route binds to.
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
// binding validator as hostname inheritance. When no managed parent resolves,
// the filter's Scheme is left nil and the proxy's own fallback applies.
//
// The function never mutates the input routes; a route is deep-copied only
// when it has at least one scheme-less redirect filter AND a parent scheme was
// resolved.
func withDefaultRedirectScheme(
	ctx context.Context,
	cli client.Client,
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
		out[i] = defaultRedirectSchemeForRoute(ctx, cli, validator, route, views)
	}

	return out
}

// defaultRedirectSchemeForRoute returns route unchanged when it carries no
// scheme-less redirect filter or no parent scheme resolves; otherwise it
// returns a deep copy with the empty redirect schemes filled in.
func defaultRedirectSchemeForRoute(
	ctx context.Context,
	cli client.Client,
	validator *routebinding.Validator,
	route *gatewayv1.HTTPRoute,
	views *listenerViewCache,
) *gatewayv1.HTTPRoute {
	if !routeHasEmptyRedirectScheme(route) {
		return route
	}

	scheme := acceptedListenerScheme(ctx, cli, validator, HTTPRouteWrapper{route}, views)
	if scheme == "" {
		return route
	}

	clone := route.DeepCopy()
	applyDefaultRedirectScheme(clone, scheme)

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
// backendRef levels.
func applyDefaultRedirectScheme(route *gatewayv1.HTTPRoute, scheme string) {
	for _, filter := range redirectFilters(route) {
		if isEmptySchemeRedirect(filter) {
			value := scheme
			filter.RequestRedirect.Scheme = &value
		}
	}
}

func isEmptySchemeRedirect(filter *gatewayv1.HTTPRouteFilter) bool {
	return filter.Type == gatewayv1.HTTPRouteFilterRequestRedirect &&
		filter.RequestRedirect != nil &&
		filter.RequestRedirect.Scheme == nil
}

// acceptedListenerScheme returns the redirect scheme implied by the listeners
// that accept the route: "https" if any accepting listener terminates HTTPS,
// otherwise "http" if at least one accepting HTTP listener resolves, or "" when
// no managed L7 parent accepts the route. HTTPS wins ties so a route bound to
// both an HTTP and an HTTPS listener defaults to the more secure scheme.
//
// Only HTTP and HTTPS listeners are considered: a TLS/TCP/UDP listener never
// accepts an HTTPRoute (the binding validator's default kinds for those
// protocols exclude HTTPRoute), so such a listener never appears in the
// accepted set and contributes no scheme. TLS in particular is terminated at
// the Cloudflare edge and is not a supported frontend listener for HTTPRoute.
func acceptedListenerScheme(
	ctx context.Context,
	cli client.Client,
	validator *routebinding.Validator,
	route Route,
	views *listenerViewCache,
) string {
	sawHTTP := false

	for _, ref := range route.GetParentRefs() {
		for _, protocol := range acceptedProtocolsForParentRef(ctx, cli, validator, route, ref, views) {
			switch protocol {
			case gatewayv1.HTTPSProtocolType:
				return redirectSchemeHTTPS
			case gatewayv1.HTTPProtocolType:
				sawHTTP = true
			case gatewayv1.TLSProtocolType, gatewayv1.TCPProtocolType, gatewayv1.UDPProtocolType:
				// Non-HTTP(S) listeners never accept an HTTPRoute, so they
				// imply no redirect scheme.
			}
		}
	}

	if sawHTTP {
		return redirectSchemeHTTP
	}

	return ""
}

// acceptedProtocolsForParentRef mirrors acceptedHostnamesForParentRef but
// collects the protocols of the listeners that accept the route, so the
// redirect-scheme default can be inferred from the parent listener. It applies
// the same group / kind / namespace resolution before delegating to the
// Gateway or ListenerSet branch.
func acceptedProtocolsForParentRef(
	ctx context.Context,
	cli client.Client,
	validator *routebinding.Validator,
	route Route,
	ref gatewayv1.ParentReference,
	views *listenerViewCache,
) []gatewayv1.ProtocolType {
	if ref.Group != nil && string(*ref.Group) != "" && string(*ref.Group) != gatewayv1.GroupName {
		return nil
	}

	kind := kindGateway
	if ref.Kind != nil {
		kind = string(*ref.Kind)
	}

	namespace := route.GetNamespace()
	if ref.Namespace != nil {
		namespace = string(*ref.Namespace)
	}

	routeInfo := &routebinding.RouteInfo{
		Name:      route.GetName(),
		Namespace: route.GetNamespace(),
		// Real hostnames here — unlike the hostname-inheritance path, which
		// passes nil because it runs only on routes with empty spec.hostnames.
		// Redirect routes may carry hostnames, so they must reach the binding
		// validator for accurate per-listener hostname filtering.
		Hostnames:   route.GetHostnames(),
		Kind:        route.GetRouteKind(),
		SectionName: ref.SectionName,
		Port:        ref.Port,
	}

	switch kind {
	case kindGateway:
		return gatewayAcceptedProtocols(ctx, cli, validator, namespace, string(ref.Name), routeInfo)
	case kindListenerSet:
		return listenerSetAcceptedProtocols(ctx, cli, validator, namespace, string(ref.Name), routeInfo, views)
	}

	return nil
}

func gatewayAcceptedProtocols(
	ctx context.Context,
	cli client.Client,
	validator *routebinding.Validator,
	namespace, name string,
	routeInfo *routebinding.RouteInfo,
) []gatewayv1.ProtocolType {
	var gateway gatewayv1.Gateway
	if err := cli.Get(ctx, client.ObjectKey{Name: name, Namespace: namespace}, &gateway); err != nil {
		return nil
	}

	result, err := validator.ValidateBinding(ctx, &gateway, routeInfo)
	if err != nil || !result.Accepted {
		return nil
	}

	protoByName := make(map[gatewayv1.SectionName]gatewayv1.ProtocolType, len(gateway.Spec.Listeners))
	for i := range gateway.Spec.Listeners {
		protoByName[gateway.Spec.Listeners[i].Name] = gateway.Spec.Listeners[i].Protocol
	}

	return protocolsForSections(result.MatchedListeners, protoByName)
}

func listenerSetAcceptedProtocols(
	ctx context.Context,
	cli client.Client,
	validator *routebinding.Validator,
	namespace, name string,
	routeInfo *routebinding.RouteInfo,
	views *listenerViewCache,
) []gatewayv1.ProtocolType {
	var listenerSet gatewayv1.ListenerSet
	if err := cli.Get(ctx, client.ObjectKey{Name: name, Namespace: namespace}, &listenerSet); err != nil {
		return nil
	}

	result, err := validator.ValidateBindingForListenerSet(ctx, &listenerSet, routeInfo)
	if err != nil || !result.Accepted {
		return nil
	}

	// Drop sections whose merged-view entry is conflicted — a conflicted
	// listener is not programmed, so a route must not infer its scheme from
	// that listener's protocol. Same step the hostname-inheritance path takes.
	matched := nonConflictedSections(ctx, cli, &listenerSet, result.MatchedListeners, views)

	protoByName := make(map[gatewayv1.SectionName]gatewayv1.ProtocolType, len(listenerSet.Spec.Listeners))
	for i := range listenerSet.Spec.Listeners {
		protoByName[listenerSet.Spec.Listeners[i].Name] = listenerSet.Spec.Listeners[i].Protocol
	}

	return protocolsForSections(matched, protoByName)
}

// protocolsForSections maps each accepted listener section to its protocol,
// mirroring hostnamesForSections on the hostname-inheritance path.
func protocolsForSections(
	sections []gatewayv1.SectionName,
	protoByName map[gatewayv1.SectionName]gatewayv1.ProtocolType,
) []gatewayv1.ProtocolType {
	var out []gatewayv1.ProtocolType

	for _, section := range sections {
		if protocol, ok := protoByName[section]; ok && protocol != "" {
			out = append(out, protocol)
		}
	}

	return out
}
