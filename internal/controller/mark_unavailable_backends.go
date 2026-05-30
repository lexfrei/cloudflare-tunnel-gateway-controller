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
// service host:port (see proxy.MarkUnavailableBackends), so it applies
// uniformly across HTTP and gRPC rules without depending on rule ordering.
func markUnavailableBackends(cfg *proxy.Config, clusterDomain string, failedRefs []ingress.BackendRefError) {
	for i := range failedRefs {
		ref := &failedRefs[i]
		proxy.MarkUnavailableBackends(
			cfg, clusterDomain, ref.BackendNS, ref.BackendName, ref.Port, http.StatusInternalServerError,
		)
	}
}
