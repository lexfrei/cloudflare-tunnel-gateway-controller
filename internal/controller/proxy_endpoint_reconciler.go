package controller

import (
	"context"
	"net/url"
	"strings"

	"github.com/cockroachdb/errors"
	discoveryv1 "k8s.io/api/discovery/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/logging"
)

// ipv4SegmentCount is the number of dotted segments in an IPv4 address;
// pulled out to avoid a magic-number lint hit in isProbablyIP.
const ipv4SegmentCount = 4

// proxyServiceTarget identifies one proxy headless Service the reconciler
// must watch. It is the (namespace, service-name) pair extracted from a
// --proxy-endpoints URL by parseProxyServiceTargets, and the EndpointSlice
// watch predicate matches against the EndpointSlice's
// `kubernetes.io/service-name` label and its namespace.
type proxyServiceTarget struct {
	namespace string
	name      string
}

// ProxyEndpointReconciler watches EndpointSlices for the proxy headless
// Services named in --proxy-endpoints. Whenever the endpoint set changes
// (a new proxy pod becomes Ready, an old one drains, the Service is
// rebuilt), it triggers ProxySyncer.ResyncEndpoints so the cached config
// gets pushed to every replica -- including replicas that joined AFTER
// the most recent HTTPRoute reconcile.
//
// Without this, a proxy pod that joins the EndpointSlice between
// HTTPRoute reconciles stays at /readyz == 503 forever (issue #293):
// the controller's push logic is HTTPRoute-driven and never re-iterates
// the endpoint list on Service churn. The workaround was
// `kubectl rollout restart deployment <controller>`, which is easy to
// forget during a chart bump.
type ProxyEndpointReconciler struct {
	Client      client.Client
	ProxySyncer *ProxySyncer
	// ProxyEndpoints holds the raw --proxy-endpoints URLs. The reconciler
	// passes them through to ProxySyncer.ResyncEndpoints unchanged; the
	// syncer's resolveEndpoints does the DNS expansion to per-pod IPs.
	ProxyEndpoints []string

	// targets is parsed from ProxyEndpoints at SetupWithManager time and
	// drives the EndpointSlice watch predicate. Each entry corresponds
	// to one headless Service whose churn should trigger a resync.
	targets []proxyServiceTarget
}

// Reconcile implements reconcile.Reconciler. It is invoked whenever an
// EndpointSlice for one of the proxy headless Services changes; the
// concrete EndpointSlice contents are intentionally ignored -- we only
// care that "something moved", at which point we hand the full
// endpoint URL list off to ProxySyncer.ResyncEndpoints which re-resolves
// DNS and pushes the cached config to every replica it finds.
func (r *ProxyEndpointReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := logging.Component(ctx, "proxy-endpoint-reconciler")
	logger.Info("proxy EndpointSlice change observed; resyncing cached config",
		"endpointslice", req.String(),
	)

	ctx = logging.WithLogger(ctx, logger)

	if err := r.ProxySyncer.ResyncEndpoints(ctx, r.ProxyEndpoints); err != nil {
		// Non-fatal: the next endpoint-change event (or the next
		// HTTPRoute reconcile) gets another chance. Surface as an
		// error so controller-runtime exponentially backs off.
		return ctrl.Result{}, errors.Wrap(err, "resync proxy endpoints")
	}

	return ctrl.Result{}, nil
}

// SetupWithManager wires the reconciler into the manager with an
// EndpointSlice watch filtered to the proxy headless Services. Filtering
// is done via a predicate matching the EndpointSlice's
// `kubernetes.io/service-name` label against the parsed target list --
// cheaper than a label-selector list-watch and resilient to the rest of
// the cluster's EndpointSlice churn.
func (r *ProxyEndpointReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.targets = parseProxyServiceTargets(r.ProxyEndpoints)

	if len(r.targets) == 0 {
		// No parseable Service targets means we cannot scope the watch
		// to anything meaningful. The controller-bootstrap layer already
		// rejects an empty --proxy-endpoints list, so this branch only
		// fires when every endpoint URL was a bare IP or other shape
		// that doesn't map to a Service. Skip the watch rather than
		// drown the controller in cluster-wide EndpointSlice events.
		return nil
	}

	if err := ctrl.NewControllerManagedBy(mgr).
		Named("proxy-endpoint-reconciler").
		For(&discoveryv1.EndpointSlice{}, builder.WithPredicates(r.endpointSliceMatchesProxy())).
		Complete(r); err != nil {
		return errors.Wrap(err, "setup proxy endpoint reconciler")
	}

	return nil
}

// endpointSliceMatchesProxy returns a predicate that fires only on
// EndpointSlices whose owning Service matches one of our parsed
// proxy-endpoint targets. controller-runtime's predicate stack runs
// once per event before enqueueing the reconcile request.
func (r *ProxyEndpointReconciler) endpointSliceMatchesProxy() predicate.Predicate {
	matches := func(obj client.Object) bool {
		slice, ok := obj.(*discoveryv1.EndpointSlice)
		if !ok {
			return false
		}

		serviceName := slice.Labels[discoveryv1.LabelServiceName]
		if serviceName == "" {
			return false
		}

		for _, target := range r.targets {
			if target.name == serviceName && target.namespace == slice.Namespace {
				return true
			}
		}

		return false
	}

	return predicate.Funcs{
		CreateFunc:  func(e event.CreateEvent) bool { return matches(e.Object) },
		UpdateFunc:  func(e event.UpdateEvent) bool { return matches(e.ObjectNew) },
		DeleteFunc:  func(e event.DeleteEvent) bool { return matches(e.Object) },
		GenericFunc: func(e event.GenericEvent) bool { return matches(e.Object) },
	}
}

// parseProxyServiceTargets extracts (namespace, service-name) pairs from a
// list of --proxy-endpoints URLs. Recognises the Kubernetes cluster-DNS
// shapes: `<svc>`, `<svc>.<ns>`, `<svc>.<ns>.svc`, and the fully-qualified
// `<svc>.<ns>.svc.<cluster-domain>`. A URL whose host is a bare IP or an
// unrecognised shape is silently skipped -- the caller treats an empty
// target list as "no watch", which is the conservative outcome.
func parseProxyServiceTargets(endpoints []string) []proxyServiceTarget {
	seen := map[proxyServiceTarget]struct{}{}
	out := make([]proxyServiceTarget, 0, len(endpoints))

	for _, raw := range endpoints {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" {
			continue
		}

		parsed, err := url.Parse(trimmed)
		if err != nil || parsed.Host == "" {
			continue
		}

		host := parsed.Hostname()
		if host == "" {
			continue
		}

		// Bare IP is unparseable as a Service ref; skip.
		if strings.ContainsAny(host, ":") || isProbablyIP(host) {
			continue
		}

		parts := strings.Split(host, ".")
		if len(parts) < 2 {
			// `service` alone -- no namespace info, skip.
			continue
		}

		target := proxyServiceTarget{name: parts[0], namespace: parts[1]}
		if _, ok := seen[target]; ok {
			continue
		}

		seen[target] = struct{}{}

		out = append(out, target)
	}

	return out
}

// isProbablyIP returns true for an IPv4-looking dotted-quad. We don't try
// to be exhaustive about IPv6 because Cluster-DNS Service names never
// look like one and a false negative here just means the URL falls
// through to the multi-segment Service-name parser.
func isProbablyIP(host string) bool {
	parts := strings.Split(host, ".")
	if len(parts) != ipv4SegmentCount {
		return false
	}

	for _, p := range parts {
		if p == "" {
			return false
		}

		for _, c := range p {
			if c < '0' || c > '9' {
				return false
			}
		}
	}

	return true
}
