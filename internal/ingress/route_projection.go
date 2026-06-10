package ingress

import (
	"context"
	"fmt"

	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// projectedMatch is the kind-neutral form of a single route match: the
// tunnel-ingress path it contributes plus its sorting priority (1 = exact,
// 0 = prefix).
type projectedMatch struct {
	path     string
	priority int
}

// projectedRule is the kind-neutral form of a single route rule. Adapters
// project their typed rules (HTTPRouteRule / GRPCRouteRule) into this shape;
// everything downstream — filter logging, backend resolution, entry
// assembly — is shared and cannot diverge between route kinds.
type projectedRule struct {
	ignoredFilters int
	backendRefs    []gatewayv1.BackendRef
	matches        []projectedMatch
}

// extractProjectedEntries is the shared rule-walking skeleton behind every
// adapter's entry extraction. The per-hostname loop deliberately re-projects
// and re-resolves per hostname, matching the historical behaviour (logging
// and failed-ref accounting repeat per hostname).
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

	for _, hostname := range hostnames {
		for _, rule := range adapter.ProjectRules(route, resolver) {
			logIgnoredFilters(resolver, namespace, name, rule.ignoredFilters)

			service, ruleFailedRefs := resolveRuleBackendRefs(
				ctx, resolver, namespace, name, adapter.GatewayKind(), rule.backendRefs,
			)
			failedRefs = append(failedRefs, ruleFailedRefs...)

			if service == "" {
				continue
			}

			if len(rule.matches) == 0 {
				entries = append(entries, routeEntry{
					hostname: string(hostname),
					path:     "",
					service:  service,
					priority: 0,
				})

				continue
			}

			for _, match := range rule.matches {
				entries = append(entries, routeEntry{
					hostname: string(hostname),
					path:     match.path,
					service:  service,
					priority: match.priority,
				})
			}
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

	logMultipleBackends(resolver, namespace, routeName, len(refs))
	logBackendWeights(resolver, namespace, routeName, refs)

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

// logIgnoredFilters reports rule-level filters, which the Cloudflare tunnel
// ingress path cannot express (the in-process proxy serves them instead).
func logIgnoredFilters(resolver *backendResolver, namespace, name string, count int) {
	if count > 0 {
		resolver.logger.Info("route configuration partially applied",
			"route", fmt.Sprintf("%s/%s", namespace, name),
			"reason", "filters not supported by Cloudflare Tunnel",
			"ignored_filters", count,
		)
	}
}

func logMultipleBackends(resolver *backendResolver, namespace, routeName string, totalBackends int) {
	if totalBackends > 1 {
		resolver.logger.Info("route configuration partially applied",
			"route", fmt.Sprintf("%s/%s", namespace, routeName),
			"reason", "multiple backendRefs specified, using only highest weight",
			"total_backends", totalBackends,
			"ignored_backends", totalBackends-1,
		)
	}
}

func logBackendWeights(resolver *backendResolver, namespace, routeName string, refs []gatewayv1.BackendRef) {
	for i, backendRef := range refs {
		if backendRef.Weight != nil && *backendRef.Weight != 1 {
			resolver.logger.Info("route configuration partially applied",
				"route", fmt.Sprintf("%s/%s", namespace, routeName),
				"reason", "backendRef weight ignored, traffic splitting not supported",
				"backend", string(backendRef.Name),
				"backend_index", i,
				"weight", *backendRef.Weight,
			)
		}
	}
}
