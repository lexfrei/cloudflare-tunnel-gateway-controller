package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/coregroup"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/parentref"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/referencegrant"
)

// backendTargetMatch classifies how a route backend relates to a
// BackendTLSPolicy's targetRefs. Higher values win when a route has several.
type backendTargetMatch int

const (
	backendTargetNone backendTargetMatch = iota
	// backendTargetUnsupported: the backend is a target of a kind this
	// controller does not attach BackendTLSPolicy to.
	backendTargetUnsupported
	backendTargetApplies
)

// policyAncestor is one Gateway entry of a policy's Status.Ancestors.
type policyAncestor struct {
	gateway    gatewayv1.Gateway
	conditions []metav1.Condition
}

// normalizedGroupKind spells the core group "" and an omitted kind Service, the
// defaults of both a backendRef and a targetRef.
func normalizedGroupKind(group, kind string) (string, string) {
	if coregroup.Is(group) {
		group = ""
	}

	if kind == "" {
		kind = serviceKind
	}

	return group, kind
}

// backendRefNamespace is the namespace a backendRef points into.
func backendRefNamespace(ref gatewayv1.BackendObjectReference, routeNamespace string) string {
	if ref.Namespace != nil {
		return string(*ref.Namespace)
	}

	return routeNamespace
}

// matchBackendToPolicy reports whether ref names one of the policy's targets.
// TargetRefs are local, so only a backend in the policy's namespace can match.
func matchBackendToPolicy(policy *gatewayv1.BackendTLSPolicy, ref gatewayv1.BackendObjectReference, routeNamespace string) backendTargetMatch {
	if backendRefNamespace(ref, routeNamespace) != policy.Namespace {
		return backendTargetNone
	}

	var refGroup, refKind string
	if ref.Group != nil {
		refGroup = string(*ref.Group)
	}

	if ref.Kind != nil {
		refKind = string(*ref.Kind)
	}

	refGroup, refKind = normalizedGroupKind(refGroup, refKind)
	match := backendTargetNone

	for _, target := range policy.Spec.TargetRefs {
		targetGroup, targetKind := normalizedGroupKind(string(target.Group), string(target.Kind))
		if target.Name != ref.Name || targetGroup != refGroup || targetKind != refKind {
			continue
		}

		if isServiceTargetRef(target.LocalPolicyTargetReference) {
			return backendTargetApplies
		}

		match = backendTargetUnsupported
	}

	return match
}

// httpRouteBackends returns every backend the route sends traffic to,
// RequestMirror destinations included.
func httpRouteBackends(route *gatewayv1.HTTPRoute) []gatewayv1.BackendObjectReference {
	var refs []gatewayv1.BackendObjectReference

	for ruleIdx := range route.Spec.Rules {
		rule := &route.Spec.Rules[ruleIdx]
		refs = appendMirrors(refs, rule.Filters, httpMirror)

		for refIdx := range rule.BackendRefs {
			refs = append(refs, rule.BackendRefs[refIdx].BackendObjectReference)
			refs = appendMirrors(refs, rule.BackendRefs[refIdx].Filters, httpMirror)
		}
	}

	return refs
}

// grpcRouteBackends is the GRPCRoute counterpart of httpRouteBackends.
func grpcRouteBackends(route *gatewayv1.GRPCRoute) []gatewayv1.BackendObjectReference {
	var refs []gatewayv1.BackendObjectReference

	for ruleIdx := range route.Spec.Rules {
		rule := &route.Spec.Rules[ruleIdx]
		refs = appendMirrors(refs, rule.Filters, grpcMirror)

		for refIdx := range rule.BackendRefs {
			refs = append(refs, rule.BackendRefs[refIdx].BackendObjectReference)
			refs = appendMirrors(refs, rule.BackendRefs[refIdx].Filters, grpcMirror)
		}
	}

	return refs
}

func httpMirror(filter *gatewayv1.HTTPRouteFilter) *gatewayv1.HTTPRequestMirrorFilter {
	return filter.RequestMirror
}

func grpcMirror(filter *gatewayv1.GRPCRouteFilter) *gatewayv1.HTTPRequestMirrorFilter {
	return filter.RequestMirror
}

func appendMirrors[F any](
	refs []gatewayv1.BackendObjectReference,
	filters []F,
	mirror func(*F) *gatewayv1.HTTPRequestMirrorFilter,
) []gatewayv1.BackendObjectReference {
	for idx := range filters {
		if requestMirror := mirror(&filters[idx]); requestMirror != nil {
			refs = append(refs, requestMirror.BackendRef)
		}
	}

	return refs
}

// policyMatchesAnyBackend reports whether any of refs names one of the
// policy's targets, ignoring ReferenceGrants: the watch mappers use it to
// decide what to re-evaluate, and over-enqueueing is harmless.
func policyMatchesAnyBackend(policy *gatewayv1.BackendTLSPolicy, refs []gatewayv1.BackendObjectReference, routeNamespace string) bool {
	for _, ref := range refs {
		if matchBackendToPolicy(policy, ref, routeNamespace) != backendTargetNone {
			return true
		}
	}

	return false
}

// routeAncestorCandidate is a route reduced to what the ancestor walk needs.
type routeAncestorCandidate struct {
	kind       string
	namespace  string
	backends   []gatewayv1.BackendObjectReference
	parentRefs []gatewayv1.ParentReference
	statuses   []gatewayv1.RouteParentStatus
}

// routeMatch is the strongest match among the route's backends. A backend in
// another namespace counts only when a ReferenceGrant lets the route use it,
// because without one the data plane never dials it.
func routeMatch(
	ctx context.Context,
	validator *referencegrant.Validator,
	policy *gatewayv1.BackendTLSPolicy,
	route *routeAncestorCandidate,
) (backendTargetMatch, error) {
	best := backendTargetNone

	for _, ref := range route.backends {
		match := matchBackendToPolicy(policy, ref, route.namespace)
		if match <= best {
			continue
		}

		if route.namespace != policy.Namespace {
			allowed, err := crossNamespaceBackendAllowed(ctx, validator, route, ref)
			if err != nil {
				return backendTargetNone, err
			}

			if !allowed {
				continue
			}
		}

		best = match
	}

	return best, nil
}

func crossNamespaceBackendAllowed(
	ctx context.Context,
	validator *referencegrant.Validator,
	route *routeAncestorCandidate,
	ref gatewayv1.BackendObjectReference,
) (bool, error) {
	var group, kind string
	if ref.Group != nil {
		group = string(*ref.Group)
	}

	if ref.Kind != nil {
		kind = string(*ref.Kind)
	}

	group, kind = normalizedGroupKind(group, kind)

	allowed, err := validator.IsReferenceAllowed(ctx,
		referencegrant.Reference{Group: gatewayv1.GroupName, Kind: route.kind, Namespace: route.namespace},
		referencegrant.Reference{Group: group, Kind: kind, Namespace: backendRefNamespace(ref, route.namespace), Name: string(ref.Name)},
	)
	if err != nil {
		return false, fmt.Errorf("checking ReferenceGrants for %s in namespace %s: %w", route.kind, route.namespace, err)
	}

	return allowed, nil
}

// listRouteCandidates lists every HTTPRoute and GRPCRoute in the cluster: a
// route in another namespace reaches the policy's Service through a
// ReferenceGrant.
func (r *BackendTLSPolicyReconciler) listRouteCandidates(ctx context.Context) ([]routeAncestorCandidate, error) {
	var httpRoutes gatewayv1.HTTPRouteList
	if err := r.List(ctx, &httpRoutes); err != nil {
		return nil, fmt.Errorf("list httproutes: %w", err)
	}

	var grpcRoutes gatewayv1.GRPCRouteList
	if err := r.List(ctx, &grpcRoutes); err != nil {
		return nil, fmt.Errorf("list grpcroutes: %w", err)
	}

	candidates := make([]routeAncestorCandidate, 0, len(httpRoutes.Items)+len(grpcRoutes.Items))

	for idx := range httpRoutes.Items {
		route := &httpRoutes.Items[idx]
		candidates = append(candidates, routeAncestorCandidate{
			kind: "HTTPRoute", namespace: route.Namespace,
			backends: httpRouteBackends(route), parentRefs: route.Spec.ParentRefs, statuses: route.Status.Parents,
		})
	}

	for idx := range grpcRoutes.Items {
		route := &grpcRoutes.Items[idx]
		candidates = append(candidates, routeAncestorCandidate{
			kind: "GRPCRoute", namespace: route.Namespace,
			backends: grpcRouteBackends(route), parentRefs: route.Spec.ParentRefs, statuses: route.Status.Parents,
		})
	}

	return candidates, nil
}

// policyAncestorGateways returns the managed Gateways whose routes use one of
// the policy's targets, keyed by the strongest match, sorted by {namespace,
// name} so a truncated Status.Ancestors stays stable across reconciles.
func (r *BackendTLSPolicyReconciler) policyAncestorGateways(
	ctx context.Context,
	policy *gatewayv1.BackendTLSPolicy,
) ([]gatewayv1.Gateway, map[client.ObjectKey]backendTargetMatch, error) {
	if len(policy.Spec.TargetRefs) == 0 {
		return nil, nil, nil
	}

	candidates, err := r.listRouteCandidates(ctx)
	if err != nil {
		return nil, nil, err
	}

	validator := referencegrant.NewValidator(r.Client)
	matches := map[client.ObjectKey]backendTargetMatch{}

	for idx := range candidates {
		match, err := routeMatch(ctx, validator, policy, &candidates[idx])
		if err != nil {
			return nil, nil, err
		}

		if match == backendTargetNone {
			continue
		}

		keys, err := r.parentGatewayKeys(ctx, r.acceptedParentRefs(&candidates[idx]), candidates[idx].namespace)
		if err != nil {
			return nil, nil, err
		}

		for _, key := range keys {
			matches[key] = max(matches[key], match)
		}
	}

	gateways, err := r.managedGateways(ctx, matches)
	if err != nil {
		return nil, nil, err
	}

	return gateways, matches, nil
}

// acceptedParentRefs returns the route's parentRefs this controller has
// accepted the route on: a parent that rejected the route carries none of its
// traffic. Acceptance through a ListenerSet already requires its Gateway to
// accept the ListenerSet.
func (r *BackendTLSPolicyReconciler) acceptedParentRefs(route *routeAncestorCandidate) []gatewayv1.ParentReference {
	accepted := make([]gatewayv1.ParentReference, 0, len(route.parentRefs))

	for _, ref := range route.parentRefs {
		if parentRefAcceptedInStatus(route.statuses, ref, route.namespace, r.ControllerName) {
			accepted = append(accepted, ref)
		}
	}

	return accepted
}

// parentGatewayKeys resolves route parentRefs to Gateway keys: a Gateway parent
// directly, a ListenerSet parent through its spec.parentRef. An absent
// ListenerSet contributes nothing; one that cannot be read is an error, so the
// caller retries instead of dropping its Gateway.
func (r *BackendTLSPolicyReconciler) parentGatewayKeys(
	ctx context.Context,
	parentRefs []gatewayv1.ParentReference,
	routeNamespace string,
) ([]client.ObjectKey, error) {
	keys := make([]client.ObjectKey, 0, len(parentRefs))

	for _, parentRef := range parentRefs {
		if parentRefIsGateway(parentRef) {
			keys = append(keys, parentReferenceToKey(parentRef, routeNamespace))

			continue
		}

		if !parentref.InGatewayAPIGroup(parentRef) || parentRef.Kind == nil || *parentRef.Kind != kindListenerSet {
			continue
		}

		var listenerSet gatewayv1.ListenerSet
		if err := r.Get(ctx, parentReferenceToKey(parentRef, routeNamespace), &listenerSet); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}

			return nil, fmt.Errorf("reading ListenerSet parent %s: %w", parentRef.Name, err)
		}

		keys = append(keys, listenerSetParentKey(&listenerSet))
	}

	return keys, nil
}

// managedGateways reads the Gateways named in matches and keeps those whose
// class binds to this controller, sorted by {namespace, name}. A Gateway or
// class that cannot be read is an error rather than a non-ancestor.
func (r *BackendTLSPolicyReconciler) managedGateways(
	ctx context.Context,
	matches map[client.ObjectKey]backendTargetMatch,
) ([]gatewayv1.Gateway, error) {
	keys := make([]client.ObjectKey, 0, len(matches))
	for key := range matches {
		keys = append(keys, key)
	}

	sort.Slice(keys, func(left, right int) bool {
		if keys[left].Namespace != keys[right].Namespace {
			return keys[left].Namespace < keys[right].Namespace
		}

		return keys[left].Name < keys[right].Name
	})

	gateways := make([]gatewayv1.Gateway, 0, len(keys))

	for _, key := range keys {
		var gateway gatewayv1.Gateway
		if err := r.Get(ctx, key, &gateway); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}

			return nil, fmt.Errorf("reading Gateway %s: %w", key, err)
		}

		managed, err := r.gatewayManagedByUs(ctx, &gateway)
		if err != nil {
			return nil, err
		}

		if managed {
			gateways = append(gateways, gateway)
		}
	}

	return gateways, nil
}

// targetNotFoundConditions replaces Accepted in conditions with
// Accepted=False/TargetNotFound naming the policy's unsupported targets, for a
// Gateway whose routes use the policy's target only through such a target.
func targetNotFoundConditions(policy *gatewayv1.BackendTLSPolicy, conditions []metav1.Condition) []metav1.Condition {
	unsupported := make([]string, 0, len(policy.Spec.TargetRefs))

	for _, target := range policy.Spec.TargetRefs {
		if !isServiceTargetRef(target.LocalPolicyTargetReference) {
			unsupported = append(unsupported, fmt.Sprintf("%s/%s %s", target.Group, target.Kind, target.Name))
		}
	}

	out := make([]metav1.Condition, 0, len(conditions))

	for _, condition := range conditions {
		if condition.Type == string(gatewayv1.PolicyConditionAccepted) {
			condition.Status = metav1.ConditionFalse
			condition.Reason = string(gatewayv1.PolicyReasonTargetNotFound)
			condition.Message = fmt.Sprintf(
				"targetRef %s is not supported: BackendTLSPolicy applies to core Services only",
				strings.Join(unsupported, ", "))
		}

		out = append(out, condition)
	}

	return out
}
