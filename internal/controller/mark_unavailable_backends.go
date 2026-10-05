package controller

import (
	"net/http"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/ingress"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
)

// markUnavailableBackends flags every invalid backendRef (a nonexistent
// Service, reported by the ingress builder) in the pushed proxy config so the
// proxy returns 500 for that backend's traffic fraction instead of dialing a
// dead address and surfacing a 502.
//
// Unlike the previous whole-rule clearing, the invalid backend stays in the
// weighted pool with its weight: per the Gateway API spec the proportion of
// requests routed to an invalid backend MUST receive a 500, while valid
// sibling backends keep serving their share. Matching is content-addressed by
// service host:port within the rules of the route the ref belongs to (see
// proxy.MarkUnavailableRouteBackends): a ref refused for one route leaves
// another route granted the same Service serving.
func markUnavailableBackends(cfg *proxy.Config, clusterDomain, routeKind string, failedRefs []ingress.BackendRefError) {
	for i := range failedRefs {
		ref := &failedRefs[i]

		// A ServiceImport failed-ref carries its own (clusterset) domain so the
		// matched host equals the URL the converter synthesized; an empty Domain
		// means the local cluster domain (the default for a Service).
		domain := clusterDomain
		if ref.Domain != "" {
			domain = ref.Domain
		}

		route := proxy.RuleProvenance{Kind: routeKind, Namespace: ref.RouteNamespace, Name: ref.RouteName}

		proxy.MarkUnavailableRouteBackends(
			cfg, &route, domain, ref.BackendNS, ref.BackendName, ref.Port, http.StatusInternalServerError,
		)
	}
}
