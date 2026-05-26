package routebinding

import (
	"context"

	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// ListenerSetAcceptance describes whether a Gateway accepts a particular
// ListenerSet attachment based on the Gateway's spec.allowedListeners filter.
type ListenerSetAcceptance struct {
	Accepted bool
	Reason   gatewayv1.ListenerSetConditionReason
}

// EvaluateListenerSetAcceptance applies the parent Gateway's
// spec.allowedListeners.namespaces filter to decide if the given ListenerSet
// is allowed to attach. The default (unset) is From=None, i.e. attachment is
// rejected unless the Gateway opts in.
func (v *Validator) EvaluateListenerSetAcceptance(
	ctx context.Context,
	gateway *gatewayv1.Gateway,
	listenerSet *gatewayv1.ListenerSet,
) (ListenerSetAcceptance, error) {
	from := getListenerNamespaceFrom(gateway.Spec.AllowedListeners)

	switch from {
	case gatewayv1.NamespacesFromAll:
		return acceptedListenerSet(), nil
	case gatewayv1.NamespacesFromSame:
		if gateway.Namespace == listenerSet.Namespace {
			return acceptedListenerSet(), nil
		}

		return rejectedListenerSet(), nil
	case gatewayv1.NamespacesFromSelector:
		ok, err := v.listenerSetNamespaceMatchesSelector(ctx, gateway.Spec.AllowedListeners, listenerSet.Namespace)
		if err != nil {
			return ListenerSetAcceptance{}, err
		}

		if ok {
			return acceptedListenerSet(), nil
		}

		return rejectedListenerSet(), nil
	case gatewayv1.NamespacesFromNone:
		return rejectedListenerSet(), nil
	}

	return rejectedListenerSet(), nil
}

func acceptedListenerSet() ListenerSetAcceptance {
	return ListenerSetAcceptance{Accepted: true, Reason: gatewayv1.ListenerSetReasonAccepted}
}

func rejectedListenerSet() ListenerSetAcceptance {
	return ListenerSetAcceptance{Accepted: false, Reason: gatewayv1.ListenerSetReasonNotAllowed}
}

// getListenerNamespaceFrom extracts the From field from allowedListeners,
// defaulting to None when unset (per Gateway API spec — ListenerSets are
// rejected unless the Gateway explicitly opts in).
func getListenerNamespaceFrom(allowed *gatewayv1.AllowedListeners) gatewayv1.FromNamespaces {
	if allowed == nil || allowed.Namespaces == nil || allowed.Namespaces.From == nil {
		return gatewayv1.NamespacesFromNone
	}

	return *allowed.Namespaces.From
}

// listenerSetNamespaceMatchesSelector evaluates the namespace label selector
// for a ListenerSet's namespace against the Gateway's allowedListeners filter.
// A missing namespace is treated as "not matching", consistent with the
// existing route-namespace selector handling.
func (v *Validator) listenerSetNamespaceMatchesSelector(
	ctx context.Context,
	allowed *gatewayv1.AllowedListeners,
	namespace string,
) (bool, error) {
	if allowed == nil || allowed.Namespaces == nil || allowed.Namespaces.Selector == nil {
		return false, nil
	}

	// Reuse the route-namespace selector code path by wrapping the selector in
	// an AllowedRoutes shell.
	wrapped := &gatewayv1.AllowedRoutes{
		Namespaces: &gatewayv1.RouteNamespaces{
			Selector: allowed.Namespaces.Selector,
		},
	}

	return v.namespaceMatchesSelector(ctx, wrapped, namespace)
}

// ValidateBindingForListenerSet validates whether a route can bind to one of
// the entries of a ListenerSet. The semantics mirror ValidateBinding for
// Gateway listeners: hostname intersection, namespace allowance, route-kind
// allowance, sectionName + port filters.
func (v *Validator) ValidateBindingForListenerSet(
	ctx context.Context,
	listenerSet *gatewayv1.ListenerSet,
	route *RouteInfo,
) (BindingResult, error) {
	entries := listenerSet.Spec.Listeners

	matched, rejectionReason, err := findMatchingEntries(
		len(entries),
		func(i int) (gatewayv1.SectionName, gatewayv1.PortNumber) {
			return entries[i].Name, entries[i].Port
		},
		func(i int) (gatewayv1.RouteConditionReason, error) {
			entry := &entries[i]

			return v.evaluateListenerBinding(
				ctx, entry.Hostname, entry.AllowedRoutes, entry.Protocol,
				listenerSet.Namespace, route,
			)
		},
		route.SectionName,
		route.Port,
	)
	if err != nil {
		return BindingResult{}, err
	}

	return makeBindingResult(matched, rejectionReason), nil
}
