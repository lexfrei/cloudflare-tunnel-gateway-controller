package routebinding

import (
	"context"

	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

const (
	defaultRejectionMessage = "Route not accepted"
	routeAcceptedMessage    = "Route accepted"
)

// RouteInfo contains information about a route for binding validation.
type RouteInfo struct {
	Name        string
	Namespace   string
	Hostnames   []gatewayv1.Hostname
	Kind        gatewayv1.Kind
	SectionName *gatewayv1.SectionName
	Port        *gatewayv1.PortNumber
}

// BindingResult represents the result of route-to-listener binding validation.
type BindingResult struct {
	Accepted         bool
	Reason           gatewayv1.RouteConditionReason
	Message          string
	MatchedListeners []gatewayv1.SectionName
}

// ValidateBinding validates whether a route can bind to a gateway's listeners.
// It returns a BindingResult indicating acceptance status, reason, and matched listeners.
func (v *Validator) ValidateBinding(
	ctx context.Context,
	gateway *gatewayv1.Gateway,
	route *RouteInfo,
) (BindingResult, error) {
	listeners := gateway.Spec.Listeners

	matched, rejectionReason, err := findMatchingEntries(
		len(listeners),
		func(i int) (gatewayv1.SectionName, gatewayv1.PortNumber) {
			return listeners[i].Name, listeners[i].Port
		},
		func(i int) (gatewayv1.RouteConditionReason, error) {
			return v.listenerAcceptsRoute(ctx, &listeners[i], gateway.Namespace, route)
		},
		route.SectionName,
		route.Port,
	)
	if err != nil {
		return BindingResult{}, err
	}

	return makeBindingResult(matched, rejectionReason), nil
}

// makeBindingResult turns the (matched, rejectionReason) tuple returned by
// findMatchingEntries into a public BindingResult, applying the standard
// Accepted=True/Reason=Accepted treatment when at least one entry matched.
func makeBindingResult(
	matched []gatewayv1.SectionName,
	rejectionReason gatewayv1.RouteConditionReason,
) BindingResult {
	if len(matched) == 0 {
		return BindingResult{
			Accepted:         false,
			Reason:           rejectionReason,
			Message:          getReasonMessage(rejectionReason),
			MatchedListeners: nil,
		}
	}

	return BindingResult{
		Accepted:         true,
		Reason:           gatewayv1.RouteReasonAccepted,
		Message:          routeAcceptedMessage,
		MatchedListeners: matched,
	}
}

// findMatchingEntries is the shared section/port match + accept iteration used
// by both Gateway listener binding and ListenerSet entry binding. The accept
// callback returns the per-entry route condition reason; entries with reason
// == Accepted are collected into the matched-section list. The first
// observed non-accepted reason becomes the fallback rejection reason when no
// entry matches.
func findMatchingEntries(
	count int,
	nameAndPort func(int) (gatewayv1.SectionName, gatewayv1.PortNumber),
	accept func(int) (gatewayv1.RouteConditionReason, error),
	routeSectionName *gatewayv1.SectionName,
	routePort *gatewayv1.PortNumber,
) ([]gatewayv1.SectionName, gatewayv1.RouteConditionReason, error) {
	if count == 0 {
		return nil, gatewayv1.RouteReasonNoMatchingParent, nil
	}

	var (
		matched             []gatewayv1.SectionName
		lastRejectionReason gatewayv1.RouteConditionReason
	)

	for i := range count {
		name, port := nameAndPort(i)

		if routeSectionName != nil && *routeSectionName != name {
			continue
		}

		if routePort != nil && *routePort != port {
			continue
		}

		reason, err := accept(i)
		if err != nil {
			return nil, "", err
		}

		if reason == gatewayv1.RouteReasonAccepted {
			matched = append(matched, name)
		} else {
			lastRejectionReason = reason
		}
	}

	if len(matched) == 0 {
		if routeSectionName != nil || routePort != nil {
			return nil, gatewayv1.RouteReasonNoMatchingParent, nil
		}

		if lastRejectionReason == "" {
			return nil, gatewayv1.RouteReasonNoMatchingParent, nil
		}

		return nil, lastRejectionReason, nil
	}

	return matched, "", nil
}

// listenerAcceptsRoute checks if a single listener accepts the route.
// Returns RouteReasonAccepted if accepted, or rejection reason otherwise.
func (v *Validator) listenerAcceptsRoute(
	ctx context.Context,
	listener *gatewayv1.Listener,
	gatewayNamespace string,
	route *RouteInfo,
) (gatewayv1.RouteConditionReason, error) {
	return v.evaluateListenerBinding(
		ctx, listener.Hostname, listener.AllowedRoutes, listener.Protocol,
		gatewayNamespace, route,
	)
}

// evaluateListenerBinding is the shared listener-vs-route check used by both
// Gateway listeners and ListenerSet entries. The two carry different concrete
// types (Listener vs ListenerEntry) but only the same set of fields matter
// for binding: hostname, allowedRoutes, and protocol.
func (v *Validator) evaluateListenerBinding(
	ctx context.Context,
	listenerHostname *gatewayv1.Hostname,
	allowedRoutes *gatewayv1.AllowedRoutes,
	protocol gatewayv1.ProtocolType,
	parentNamespace string,
	route *RouteInfo,
) (gatewayv1.RouteConditionReason, error) {
	if !HostnamesIntersect(listenerHostname, route.Hostnames) {
		return gatewayv1.RouteReasonNoMatchingListenerHostname, nil
	}

	allowed, err := v.IsNamespaceAllowed(ctx, allowedRoutes, parentNamespace, route.Namespace)
	if err != nil {
		return "", err
	}

	if !allowed {
		return gatewayv1.RouteReasonNotAllowedByListeners, nil
	}

	if !IsRouteKindAllowed(allowedRoutes, protocol, route.Kind) {
		return gatewayv1.RouteReasonNotAllowedByListeners, nil
	}

	return gatewayv1.RouteReasonAccepted, nil
}

// getReasonMessage returns a human-readable message for a route condition reason.
func getReasonMessage(reason gatewayv1.RouteConditionReason) string {
	switch reason {
	case gatewayv1.RouteReasonNoMatchingListenerHostname:
		return "No listener hostname matches route hostnames"
	case gatewayv1.RouteReasonNotAllowedByListeners:
		return "Route not allowed by listener allowedRoutes policy"
	case gatewayv1.RouteReasonNoMatchingParent:
		return "No matching listener found"
	case gatewayv1.RouteReasonAccepted,
		gatewayv1.RouteReasonPending,
		gatewayv1.RouteReasonUnsupportedValue,
		gatewayv1.RouteReasonIncompatibleFilters,
		gatewayv1.RouteReasonResolvedRefs,
		gatewayv1.RouteReasonRefNotPermitted,
		gatewayv1.RouteReasonInvalidKind,
		gatewayv1.RouteReasonBackendNotFound,
		gatewayv1.RouteReasonUnsupportedProtocol:
		return defaultRejectionMessage
	}

	return defaultRejectionMessage
}
