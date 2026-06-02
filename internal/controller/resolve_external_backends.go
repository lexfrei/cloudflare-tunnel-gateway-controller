package controller

import (
	"context"
	"net/http"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/api/v1alpha1"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
)

// resolveExternalBackends rewrites every ExternalBackend sentinel URL in cfg to
// the real scheme://host:port/path from the referenced ExternalBackend's spec.
// The converter has no client and so emits a sentinel encoding the ref's
// namespace/name; this step (running in buildProxyConfig before the config is
// pushed, like the other post-conversion marking steps) reads the CRD and
// substitutes the real URL.
//
// A sentinel whose ExternalBackend is missing is marked 500 for its traffic
// fraction — the placeholder is never dialed. When the resolved scheme is https
// an h2c gRPC backend is switched to plain HTTP transport so the TLS transport
// negotiates HTTP/2 over ALPN instead of attempting cleartext h2c on a TLS port.
func resolveExternalBackends(ctx context.Context, c client.Reader, cfg *proxy.Config) {
	if cfg == nil {
		return
	}

	for ruleIdx := range cfg.Rules {
		backends := cfg.Rules[ruleIdx].Backends
		for backendIdx := range backends {
			namespace, name, ok := proxy.ParseExternalBackendSentinel(backends[backendIdx].URL)
			if !ok {
				continue
			}

			external := &v1alpha1.ExternalBackend{}
			if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, external); err != nil {
				// Missing or unreadable ExternalBackend: 500 for this fraction.
				// The sentinel URL is left in place but never dialed.
				if backends[backendIdx].UnavailableStatus == 0 {
					backends[backendIdx].UnavailableStatus = http.StatusInternalServerError
				}

				continue
			}

			// A malformed spec (e.g. a bare host:port that slipped past the CRD
			// host pattern) would dial a URL that fails to parse and 500 opaquely.
			// Mark it 500 here without pushing the bad URL; the ingress builder
			// surfaces the matching ResolvedRefs=False on the route.
			if external.Spec.Validate() != nil {
				if backends[backendIdx].UnavailableStatus == 0 {
					backends[backendIdx].UnavailableStatus = http.StatusInternalServerError
				}

				continue
			}

			backends[backendIdx].URL = external.Spec.URL()

			if backends[backendIdx].Protocol == proxy.BackendProtocolH2C &&
				external.Spec.Scheme == v1alpha1.ExternalBackendSchemeHTTPS {
				backends[backendIdx].Protocol = proxy.BackendProtocolHTTP
			}
		}
	}
}
