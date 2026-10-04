package controller

import (
	"context"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/cockroachdb/errors"
	"golang.org/x/time/rate"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/logging"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/render"
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
// (a new proxy pod appears, an old one drains, the Service is
// rebuilt), it triggers ProxySyncer.resyncEndpoints so the cached config
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
	// passes them through to ProxySyncer.resyncEndpoints unchanged; the
	// syncer's resolveEndpoints does the DNS expansion to per-pod IPs.
	ProxyEndpoints []string

	// TriggerRouteSync runs a full route sync, the one that builds and pushes
	// every partition's config. It is used when the Service's slices list pods
	// but the partition has no config cached to replay. nil disables it.
	TriggerRouteSync func(context.Context) (ctrl.Result, error)

	// targets is parsed from ProxyEndpoints at SetupWithManager time and
	// drives the EndpointSlice watch predicate. Each entry corresponds
	// to one headless Service whose churn should trigger a resync.
	targets []proxyServiceTarget

	// coldSyncFailures maps a slice to the resourceVersion at which its
	// cold-start route sync built nothing, so that version does not run the
	// sync again. An entry matches only the version it recorded, so a stale
	// one is harmless; it is dropped when its slice is deleted, which a removed
	// plane's slice is with its Service. Guarded by coldSyncFailuresMu.
	coldSyncFailuresMu sync.Mutex
	coldSyncFailures   map[types.NamespacedName]string
}

// Reconcile implements reconcile.Reconciler. It is invoked whenever an
// EndpointSlice for one of the proxy headless Services changes, and hands the
// full endpoint URL list off to ProxySyncer, which re-resolves DNS and pushes
// the cached config to every replica it finds. The addresses of every slice of
// the slice's Service are checked against what the replay reached: DNS can lag
// the slices, and a pod the replay missed would otherwise wait for the next
// full sync.
func (r *ProxyEndpointReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := logging.Component(ctx, "proxy-endpoint-reconciler")
	logger.Info("replaying cached proxy config for EndpointSlice",
		"endpointslice", req.String(),
	)

	ctx = logging.WithLogger(ctx, logger)

	var slice discoveryv1.EndpointSlice
	if err := r.Client.Get(ctx, req.NamespacedName, &slice); err != nil {
		if apierrors.IsNotFound(err) {
			r.forgetColdSyncFailure(req.NamespacedName)
		}

		// Deleted or unreadable: we cannot attribute the event to one data
		// plane, so replay every cached partition — cheap and correct.
		return replayResult(ctx, r.ProxySyncer.ResyncAllPartitions(ctx), "resync all proxy partitions")
	}

	coverage := r.serviceCoverage(ctx, &slice)

	// A per-Gateway data plane's EndpointSlice carries the Gateway label
	// (mirrored from its rendered Service); resync just that partition.
	if labelValue := slice.Labels[render.GatewayLabel]; labelValue != "" {
		key, ok := r.partitionKeyForLabel(ctx, slice.Namespace, labelValue)
		if !ok {
			// The label value cannot be attributed to a live Gateway (it is
			// a truncated form of a name that no longer exists, or foreign):
			// replay every cached partition — cheap and correct.
			return replayResult(ctx, r.ProxySyncer.ResyncAllPartitions(ctx), "resync all proxy partitions")
		}

		return r.replay(ctx, key, coverage, "resync per-gateway proxy partition", func() error {
			return r.ProxySyncer.resyncPartitionCovering(ctx, key, coverage)
		})
	}

	return r.replay(ctx, sharedPartitionKey, coverage, "resync proxy endpoints", func() error {
		return r.ProxySyncer.resyncEndpoints(ctx, r.ProxyEndpoints, coverage)
	})
}

// replay runs resync for the partition keyed by key. A partition no replica
// has accepted a config from, while its Service's slices list pods, goes
// through configureColdPartition first: its earlier sync reached no pod, and
// nothing else would configure the one that joined.
func (r *ProxyEndpointReconciler) replay(
	ctx context.Context, key string, coverage *sliceCoverage, action string, resync func() error,
) (ctrl.Result, error) {
	if _, cached := r.ProxySyncer.replaySource(key); r.TriggerRouteSync != nil && len(coverage.want) > 0 && !cached {
		if result, stop := r.configureColdPartition(ctx, key, coverage); stop {
			return result, nil
		}
	}

	return replayResult(ctx, resync(), action)
}

// configureColdPartition gets a config to a partition no replica has accepted
// one from, and reports whether the reconcile stops with result; when it does
// not, a replica holds the config and the normal replay follows. It pushes the
// config the partition's last sync built, and runs a route sync only when none
// was built yet: a sync reads every tunnel's configuration from Cloudflare.
//
// A built config that lost the push race to a newer one requeues shortly, as a
// superseded replay does. Any other undelivered config is pushed again after
// replayRetryDelay, or sooner when the sync asked for it, as a requeue rather
// than an error, because the capped backoff starts at milliseconds.
func (r *ProxyEndpointReconciler) configureColdPartition(
	ctx context.Context, key string, coverage *sliceCoverage,
) (ctrl.Result, bool) {
	built, pushErr := r.ProxySyncer.pushBuiltConfig(ctx, key)

	switch {
	case !built:
		return r.syncColdPartition(ctx, key, coverage)
	case pushErr == nil:
		return ctrl.Result{}, false
	case onlyMarked(pushErr, errReplaySuperseded):
		return ctrl.Result{RequeueAfter: lostRacePushRequeueDelay}, true
	default:
		return retryUndelivered(ctx, key, ctrl.Result{}), true
	}
}

// syncColdPartition runs a route sync for a partition that has no built
// config. A sync that builds none, because it failed before building or
// produced no partition for this plane, is recorded against the slice version
// and not run again for it: this reconcile comes back every ten seconds, and
// without the record each return would run another full sync. The route sync
// retrier owns the retry of that one global sync. The reconcile still
// comes back after replayRetryDelay, which costs no Cloudflare call, and pushes
// the config as soon as another sync has built it. A new version of the slice
// runs the sync again; each slice of the plane's Service keeps its own record.
func (r *ProxyEndpointReconciler) syncColdPartition(
	ctx context.Context, key string, coverage *sliceCoverage,
) (ctrl.Result, bool) {
	if failedAt, ok := r.coldSyncFailedAt(coverage.slice); ok && failedAt == coverage.sliceVersion {
		return ctrl.Result{RequeueAfter: replayRetryDelay}, true
	}

	result, err := r.TriggerRouteSync(ctx)
	if err != nil {
		logging.FromContext(ctx).Error("route sync for a partition with no config to replay failed",
			"partition", key, "error", err.Error())
	}

	// A sync can build and push the partition's config and still return an
	// error for something else; the built config then takes the push retry.
	built, delivered := r.ProxySyncer.replaySource(key)

	switch {
	case delivered:
		return ctrl.Result{}, false
	case built:
		return retryUndelivered(ctx, key, result), true
	default:
		r.recordColdSyncFailure(coverage.slice, coverage.sliceVersion)

		return ctrl.Result{RequeueAfter: replayRetryDelay}, true
	}
}

// recordColdSyncFailure notes that the cold-start route sync run for the slice
// built nothing while the slice was at version.
func (r *ProxyEndpointReconciler) recordColdSyncFailure(slice types.NamespacedName, version string) {
	r.coldSyncFailuresMu.Lock()
	defer r.coldSyncFailuresMu.Unlock()

	if r.coldSyncFailures == nil {
		r.coldSyncFailures = make(map[types.NamespacedName]string)
	}

	r.coldSyncFailures[slice] = version
}

// coldSyncFailedAt returns the slice version the slice's cold-start route sync
// last built nothing at, if it did.
func (r *ProxyEndpointReconciler) coldSyncFailedAt(slice types.NamespacedName) (string, bool) {
	r.coldSyncFailuresMu.Lock()
	defer r.coldSyncFailuresMu.Unlock()

	version, ok := r.coldSyncFailures[slice]

	return version, ok
}

func (r *ProxyEndpointReconciler) forgetColdSyncFailure(slice types.NamespacedName) {
	r.coldSyncFailuresMu.Lock()
	defer r.coldSyncFailuresMu.Unlock()

	delete(r.coldSyncFailures, slice)
}

// retryUndelivered requeues a partition whose built config no pod has taken
// yet, after replayRetryDelay or the sooner requeue the sync asked for.
func retryUndelivered(ctx context.Context, key string, syncResult ctrl.Result) ctrl.Result {
	retryAfter := replayRetryDelay
	if syncResult.RequeueAfter > 0 && syncResult.RequeueAfter < retryAfter {
		retryAfter = syncResult.RequeueAfter
	}

	logging.FromContext(ctx).Warn("no proxy pod of the partition has taken its config yet; retrying",
		"partition", key, "retryAfter", retryAfter.String())

	return ctrl.Result{RequeueAfter: retryAfter}
}

// partitionKeyForLabel maps a GatewayLabel value back onto the partition key
// (full "namespace/name") by scanning the namespace's Gateways through the
// same truncation function that produced the value. No length shortcut: a
// truncated value is NOT length-distinguishable from a literal name —
// truncateName trims trailing dashes before appending the hash, so a long
// name cut on a dash boundary yields a SUB-63 value, and treating that as a
// literal name would resync a partition that does not exist (silently
// starving the Gateway's data plane of endpoint-driven replays). The List is
// cache-served and namespace-bounded.
func (r *ProxyEndpointReconciler) partitionKeyForLabel(
	ctx context.Context,
	namespace, labelValue string,
) (string, bool) {
	var gateways gatewayv1.GatewayList
	if err := r.Client.List(ctx, &gateways, client.InNamespace(namespace)); err != nil {
		return "", false
	}

	for i := range gateways.Items {
		// First match wins. The label value is a truncated name with an 8-hex
		// hash suffix, so a collision needs two Gateway names that both truncate
		// AND hash-collide in one namespace — astronomically unlikely. Even
		// then the only consequence is a misdirected endpoint RESYNC (a re-push
		// of already-correct config), never a cross-tenant leak, and it
		// self-heals on the next genuine config change.
		if render.GatewayLabelValue(gateways.Items[i].Name) == labelValue {
			return namespace + "/" + gateways.Items[i].Name, true
		}
	}

	return "", false
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

	options := replayControllerOptions()

	if err := ctrl.NewControllerManagedBy(mgr).
		Named("proxy-endpoint-reconciler").
		For(&discoveryv1.EndpointSlice{}, builder.WithPredicates(r.endpointSliceMatchesProxy())).
		WithOptions(options).
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

		// Per-Gateway data planes: the EndpointSlice controller mirrors the
		// rendered Service's labels (including the Gateway marker) onto its
		// EndpointSlices (kubernetes/kubernetes#94443, stable since 1.20), so a
		// newly-joined/restarted per-Gateway pod's slice carries the marker and
		// triggers ResyncPartition. Pinned by the e2e scale test + the predicate
		// unit test (TestEndpointSliceMatchesProxy).
		if slice.Labels[render.GatewayLabel] != "" {
			return true
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

		target, ok := serviceTargetOfHost(parsed.Hostname())
		if !ok {
			continue
		}

		if _, ok := seen[target]; ok {
			continue
		}

		seen[target] = struct{}{}

		out = append(out, target)
	}

	return out
}

// serviceTargetOfHost reads the Service a cluster-DNS host name refers to:
// `<svc>.<ns>`, `<svc>.<ns>.svc` or `<svc>.<ns>.svc.<cluster-domain>`. A bare
// IP or a name without a namespace refers to no Service.
func serviceTargetOfHost(host string) (proxyServiceTarget, bool) {
	if host == "" || strings.ContainsAny(host, ":") || isProbablyIP(host) {
		return proxyServiceTarget{}, false
	}

	parts := strings.Split(host, ".")
	if len(parts) < 2 {
		return proxyServiceTarget{}, false
	}

	return proxyServiceTarget{name: parts[0], namespace: parts[1]}, true
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

// replayRetryDelay bounds how long a replay waits before it is retried. A new
// pod joins the slice before its image is pulled, then causes no further slice
// change while it waits NotReady for its config, and the proxy gives up on
// that wait after two minutes. controller-runtime's default backoff grows to
// minutes, so this controller caps it here.
const replayRetryDelay = 10 * time.Second

// replayRetryBaseDelay is the first retry delay of a failed replay, doubled on
// each further failure up to replayRetryDelay.
const replayRetryBaseDelay = 5 * time.Millisecond

// replayRetryQPS and replayRetryBurst bound the retries of all replays
// together, as controller-runtime's default rate limiter does.
const (
	replayRetryQPS   = 10
	replayRetryBurst = 100
)

// replayControllerOptions exists so a test can pin the options the controller
// runs with; SetupWithManager takes them from here.
func replayControllerOptions() controller.Options {
	return controller.Options{RateLimiter: replayRateLimiter()}
}

// replayRateLimiter is controller-runtime's default rate limiter with the
// per-item backoff capped at replayRetryDelay instead of 1000s: the larger of
// that backoff and the controller-wide bucket of 10 retries per second with a
// burst of 100.
func replayRateLimiter() workqueue.TypedRateLimiter[reconcile.Request] {
	return replayRateLimiterWith(newReplayBucket())
}

func newReplayBucket() *rate.Limiter {
	return rate.NewLimiter(rate.Limit(replayRetryQPS), replayRetryBurst)
}

func replayRateLimiterWith(bucket *rate.Limiter) workqueue.TypedRateLimiter[reconcile.Request] {
	return workqueue.NewTypedMaxOfRateLimiter(
		workqueue.NewTypedItemExponentialFailureRateLimiter[reconcile.Request](replayRetryBaseDelay, replayRetryDelay),
		&workqueue.TypedBucketRateLimiter[reconcile.Request]{Limiter: bucket},
	)
}

// replayResult maps a replay's error onto the reconcile result. A replay that
// every partition reports superseded requeues shortly: the newer document is
// cached or still being pushed, so the next replay carries it. A replay that
// reached every address DNS returned but missed a slice pod is not a failure,
// DNS has not caught up yet, so it is retried after replayRetryDelay without
// an error. Any other failure keeps the error, which the capped backoff of
// replayRateLimiter retries.
func replayResult(ctx context.Context, err error, action string) (ctrl.Result, error) {
	if err == nil {
		return ctrl.Result{}, nil
	}

	if onlyMarked(err, errReplaySuperseded) {
		return ctrl.Result{RequeueAfter: lostRacePushRequeueDelay}, nil
	}

	if onlyMarked(err, errReplayMissedPods) {
		logging.FromContext(ctx).Info("proxy config replay missed pods DNS did not return yet; retrying",
			"action", action, "error", err.Error(), "retryAfter", replayRetryDelay.String())

		return ctrl.Result{RequeueAfter: replayRetryDelay}, nil
	}

	return ctrl.Result{}, errors.Wrap(err, action)
}

// sliceCoverage is what a replay triggered by an EndpointSlice must reach.
type sliceCoverage struct {
	// want holds the addresses, across every slice of the Service, of pods
	// that are neither terminating nor marked not ready.
	want []string
	// known holds every address the Service's slices list.
	known []string
	// slice and sliceVersion identify the slice and its resourceVersion.
	slice        types.NamespacedName
	sliceVersion string
	// service is the Service the slice belongs to, when its label names one.
	service    proxyServiceTarget
	hasService bool
}

// isSliceService reports whether an endpoint host names the Service this
// slice belongs to.
func (c *sliceCoverage) isSliceService(host string) bool {
	target, ok := serviceTargetOfHost(host)

	return ok && c.hasService && target == c.service
}

// replayCoverage returns the coverage a replay triggered by slice is checked
// against. A terminating pod is left out of want: it is on its way out, and
// DNS may already have dropped it. So is a pod the slice marks not ready:
// DNS returns only ready pods unless the Service publishes not-ready ones,
// and then the slice marks every pod ready. A slice of host names needs no
// special case: none of them matches a resolved address, so
// sliceCoverage.missedBy skips the check.
func replayCoverage(slice *discoveryv1.EndpointSlice) *sliceCoverage {
	coverage := &sliceCoverage{slice: client.ObjectKeyFromObject(slice), sliceVersion: slice.ResourceVersion}

	if name := slice.Labels[discoveryv1.LabelServiceName]; name != "" {
		coverage.service = proxyServiceTarget{name: name, namespace: slice.Namespace}
		coverage.hasService = true
	}

	coverage.add(slice)

	return coverage
}

// serviceCoverage is replayCoverage over every EndpointSlice of the slice's
// Service and address type. A Service split over several slices can put a new
// pod in a slice that holds no pod of a stale DNS answer, and only the pods of
// the other slices show that the answer is stale. The other address family is
// left out: a resolver that returns only A records would otherwise miss every
// IPv6 pod of a dual-stack Service forever. A failed List leaves the coverage
// to the triggering slice alone.
func (r *ProxyEndpointReconciler) serviceCoverage(ctx context.Context, slice *discoveryv1.EndpointSlice) *sliceCoverage {
	coverage := replayCoverage(slice)
	if !coverage.hasService {
		return coverage
	}

	var siblings discoveryv1.EndpointSliceList

	err := r.Client.List(ctx, &siblings, client.InNamespace(slice.Namespace),
		client.MatchingLabels{discoveryv1.LabelServiceName: coverage.service.name})
	if err != nil {
		logging.FromContext(ctx).Warn("cannot list the other EndpointSlices of the proxy Service; checking this slice only",
			"service", coverage.service.name, "error", err.Error())

		return coverage
	}

	for i := range siblings.Items {
		if siblings.Items[i].Name != slice.Name && siblings.Items[i].AddressType == slice.AddressType {
			coverage.add(&siblings.Items[i])
		}
	}

	return coverage
}

func (c *sliceCoverage) add(slice *discoveryv1.EndpointSlice) {
	for i := range slice.Endpoints {
		endpoint := &slice.Endpoints[i]
		c.known = append(c.known, endpoint.Addresses...)

		if endpoint.Conditions.Terminating != nil && *endpoint.Conditions.Terminating {
			continue
		}

		if endpoint.Conditions.Ready != nil && !*endpoint.Conditions.Ready {
			continue
		}

		c.want = append(c.want, endpoint.Addresses...)
	}
}

// onlyMarked reports whether err is sentinel, or a joined error whose every
// branch is. It walks the chain one wrapper at a time instead of using
// errors.Is, which would also match a single marked branch of a joined error
// whose other branch failed for real.
func onlyMarked(err, sentinel error) bool {
	for err != nil {
		if joined, ok := err.(interface{ Unwrap() []error }); ok {
			branches := joined.Unwrap()
			for _, branch := range branches {
				if !onlyMarked(branch, sentinel) {
					return false
				}
			}

			return len(branches) > 0
		}

		//nolint:err113,errorlint // compared one wrapper at a time on purpose; see above.
		if err == sentinel {
			return true
		}

		err = errors.UnwrapOnce(err)
	}

	return false
}
