package routebinding

import (
	"context"

	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

const defaultRejectionMessage = "Route not accepted"

// RouteInfo contains information about a route for binding validation.
type RouteInfo struct {
	Name        string
	Namespace   string
	Hostnames   []gatewayv1.Hostname
	Kind        gatewayv1.Kind
	SectionName *gatewayv1.SectionName
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
	matchedListeners, rejectionReason, err := v.findMatchingListeners(ctx, gateway, route)
	if err != nil {
		return BindingResult{}, err
	}

	if len(matchedListeners) == 0 {
		return BindingResult{
			Accepted:         false,
			Reason:           rejectionReason,
			Message:          getReasonMessage(rejectionReason),
			MatchedListeners: nil,
		}, nil
	}

	return BindingResult{
		Accepted:         true,
		Reason:           gatewayv1.RouteReasonAccepted,
		Message:          "Route accepted",
		MatchedListeners: matchedListeners,
	}, nil
}

// findMatchingListeners finds all listeners that the route can bind to.
// Returns matched listeners, rejection reason (if no matches), and error.
func (v *Validator) findMatchingListeners(
	ctx context.Context,
	gateway *gatewayv1.Gateway,
	route *RouteInfo,
) ([]gatewayv1.SectionName, gatewayv1.RouteConditionReason, error) {
	if len(gateway.Spec.Listeners) == 0 {
		return nil, gatewayv1.RouteReasonNoMatchingParent, nil
	}

	var matchedListeners []gatewayv1.SectionName

	var lastRejectionReason gatewayv1.RouteConditionReason

	for i := range gateway.Spec.Listeners {
		listener := &gateway.Spec.Listeners[i]

		if route.SectionName != nil && *route.SectionName != listener.Name {
			continue
		}

		reason, err := v.listenerAcceptsRoute(ctx, listener, gateway.Namespace, route)
		if err != nil {
			return nil, "", err
		}

		if reason == gatewayv1.RouteReasonAccepted {
			matchedListeners = append(matchedListeners, listener.Name)
		} else {
			lastRejectionReason = reason
		}
	}

	if len(matchedListeners) == 0 {
		if route.SectionName != nil {
			return nil, gatewayv1.RouteReasonNoMatchingParent, nil
		}

		if lastRejectionReason == "" {
			return nil, gatewayv1.RouteReasonNoMatchingParent, nil
		}

		return nil, lastRejectionReason, nil
	}

	return matchedListeners, "", nil
}

// listenerAcceptsRoute checks if a single listener accepts the route.
// Returns RouteReasonAccepted if accepted, or rejection reason otherwise.
func (v *Validator) listenerAcceptsRoute(
	ctx context.Context,
	listener *gatewayv1.Listener,
	gatewayNamespace string,
	route *RouteInfo,
) (gatewayv1.RouteConditionReason, error) {
	if !HostnamesIntersect(listener.Hostname, route.Hostnames) {
		return gatewayv1.RouteReasonNoMatchingListenerHostname, nil
	}

	allowed, err := v.IsNamespaceAllowed(ctx, listener.AllowedRoutes, gatewayNamespace, route.Namespace)
	if err != nil {
		return "", err
	}

	if !allowed {
		return gatewayv1.RouteReasonNotAllowedByListeners, nil
	}

	if !IsRouteKindAllowed(listener.AllowedRoutes, listener.Protocol, route.Kind) {
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
