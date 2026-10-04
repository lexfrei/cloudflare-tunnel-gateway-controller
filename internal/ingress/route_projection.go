package ingress

import (
	"context"

	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// extractProjectedEntries is the shared rule-walking skeleton behind every
// adapter's entry extraction: one entry per hostname for each rule whose
// backend resolves. Matches, filters and weights contribute nothing, since
// the tunnel document carries hostnames only and the in-process proxy does
// the rest. Backends are resolved exactly once per route — resolution does
// not depend on the hostname — so a BackendRefError, its log lines and the
// failed-ref metric are not repeated per hostname.
func extractProjectedEntries[R any](
	ctx context.Context,
	adapter RouteAdapter[R],
	route *R,
	resolver *backendResolver,
) ([]routeEntry, []BackendRefError) {
	var entries []routeEntry

	var failedRefs []BackendRefError

	namespace, name := adapter.GetMeta(route)
	hostnames := adapter.GetHostnames(route)

	for _, refs := range adapter.RuleBackendRefs(route) {
		service, ruleFailedRefs := resolveRuleBackendRefs(
			ctx, resolver, namespace, name, adapter.GatewayKind(), refs,
		)
		failedRefs = append(failedRefs, ruleFailedRefs...)

		if service == "" {
			continue
		}

		for _, hostname := range hostnames {
			entries = append(entries, routeEntry{hostname: string(hostname), service: service})
		}
	}

	return entries, failedRefs
}

// resolveRuleBackendRefs validates every traffic-receiving backend in the
// rule and returns the highest-weight backend's URL (for the single-backend
// Cloudflare tunnel ingress entry) plus a BackendRefError for each invalid
// backend.
//
// Every backend with weight > 0 is validated — not just the highest-weight
// one — so an invalid lower-weight backend is reported and the proxy can
// return 500 for its traffic fraction per the Gateway API spec. Weight-0
// backends receive no traffic and are skipped.
func resolveRuleBackendRefs(
	ctx context.Context,
	resolver *backendResolver,
	namespace, routeName, routeKind string,
	refs []gatewayv1.BackendRef,
) (string, []BackendRefError) {
	if len(refs) == 0 {
		return "", nil
	}

	selectedIdx := SelectHighestWeightIndex(refs)
	if selectedIdx == -1 {
		return "", nil
	}

	var failedRefs []BackendRefError

	serviceURL := ""

	for i := range refs {
		if effectiveBackendWeight(&refs[i]) == 0 {
			continue
		}

		url, failedRef := resolveValidatedBackend(ctx, resolver, refs[i], namespace, routeName, routeKind)
		if failedRef != nil {
			failedRefs = append(failedRefs, *failedRef)
		}

		if i == selectedIdx {
			serviceURL = url
		}
	}

	return serviceURL, failedRefs
}

// effectiveBackendWeight returns the effective weight of a backend (default 1
// when unset), matching SelectHighestWeightIndex's weight semantics.
func effectiveBackendWeight(ref *gatewayv1.BackendRef) int32 {
	if ref.Weight != nil {
		return *ref.Weight
	}

	return DefaultBackendWeight
}

// hostnamesOrWildcard returns a route's hostnames, or the "*" wildcard when
// it declares none.
func hostnamesOrWildcard(hostnames []gatewayv1.Hostname) []gatewayv1.Hostname {
	if len(hostnames) == 0 {
		return []gatewayv1.Hostname{"*"}
	}

	return hostnames
}

// embeddedBackendRefs returns, per rule, the plain BackendRefs embedded in the
// rule's kind-specific backendRefs; their per-backend filters do not reach the
// tunnel document.
func embeddedBackendRefs[Rule, Ref any](
	rules []Rule,
	refsOf func(*Rule) []Ref,
	embedded func(*Ref) gatewayv1.BackendRef,
) [][]gatewayv1.BackendRef {
	out := make([][]gatewayv1.BackendRef, 0, len(rules))

	for i := range rules {
		typed := refsOf(&rules[i])
		plain := make([]gatewayv1.BackendRef, len(typed))

		for j := range typed {
			plain[j] = embedded(&typed[j])
		}

		out = append(out, plain)
	}

	return out
}
