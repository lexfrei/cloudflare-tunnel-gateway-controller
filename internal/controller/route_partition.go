package controller

import (
	"context"
	"slices"
	"strings"

	"github.com/cockroachdb/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/config"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/logging"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/tunnelownership"
)

// sharedPartitionKey identifies the chart-deployed shared data plane (one
// proxy pool + the class tunnel serving every non-opted-in Gateway).
const sharedPartitionKey = "shared"

// routePartition is one data-plane partition: the shared plane, or one
// Gateway's dedicated proxy + tunnel. Partition membership IS the isolation
// guarantee — a partition's routes are the only routes its tunnel document
// and its proxy config ever see.
type routePartition struct {
	// Key is sharedPartitionKey or the infra Gateway's "namespace/name".
	Key string
	// Gateway is the opted-in Gateway; nil for the shared partition.
	Gateway *gatewayv1.Gateway
	// PerGateway carries the resolved tunnel identity, connector token, and
	// push auth for the dedicated plane; nil for the shared partition.
	PerGateway *config.PerGatewayConfig

	HTTPRoutes []gatewayv1.HTTPRoute
	GRPCRoutes []gatewayv1.GRPCRoute
}

// infraGateway pairs an opted-in Gateway with its resolved per-Gateway
// config.
type infraGateway struct {
	gateway    gatewayv1.Gateway
	perGateway *config.PerGatewayConfig
}

// infraGateways is the per-sync view of opted-in Gateways: resolved holds the
// data planes that can be served; broken holds the Gateways that OPTED IN but
// whose configuration did not resolve. The distinction is load-bearing:
// resolved Gateways get their own partitions, broken ones FAIL CLOSED — their
// routes belong to no partition at all, and in particular never fall back to
// the shared plane (that fallback would be a cross-tenant leak).
type infraGateways struct {
	resolved map[string]*infraGateway
	broken   map[string]bool
	// rejected holds the Gateways refused by the tunnel-ownership rule,
	// mapped to the reason the Gateway reconciler puts in status. Like
	// broken they contribute no partition; unlike broken their config
	// resolved fine — they just claimed a tunnel that is not theirs.
	rejected map[string]tunnelownership.Rejection
	// transient is the subset of broken whose resolve failure was retryable
	// (an apiserver blip, not a deterministic config error). These fail closed
	// like any broken Gateway, but their push cache must be RETAINED and the
	// reconcile requeued so a newly-joined pod is not stranded configless until
	// an unrelated event.
	transient map[string]bool
}

// isBroken reports whether the Gateway key opted in but failed to resolve.
// Nil-safe (a nil view has no infra Gateways at all).
func (g *infraGateways) isBroken(key string) bool {
	return g != nil && g.broken[key]
}

// applyTunnelOwnership drops every Gateway whose token claims a tunnel it does
// not own, recording why in rejected so the Gateway reconciler can say so in
// status. A dropped Gateway leaves resolved entirely: it contributes no
// partition, so it is neither sent another tenant's routes nor allowed to put
// its own into another tenant's tunnel document.
//
// The claim set is built by collectTunnelClaims and shared with every other
// layer that arbitrates: feeding the same function different inputs is how two
// layers reach different verdicts, and this one deciding "no conflict" while
// the status layer decides "refused" is the unsafe direction.
func applyTunnelOwnership(
	infra *infraGateways,
	sharedTunnelID string,
	allowSharedTunnels bool,
	claims []tunnelownership.Claim,
) {
	// Not gated on resolved being non-empty: a Gateway whose token Secret is
	// gone resolves to nothing yet still claims through its advertised
	// address, and recording that refusal is what lets its routes say the
	// tunnel was refused instead of the generic broken-data-plane sentence.
	if infra == nil || len(claims) == 0 {
		return
	}

	// The operator opted every party on a shared tunnel into seeing the
	// others' routes. Nothing to arbitrate: the pre-existing merge behaviour
	// is what they asked for.
	if allowSharedTunnels {
		return
	}

	rejections := tunnelownership.Arbitrate(sharedTunnelID, claims)
	if len(rejections) == 0 {
		return
	}

	if infra.rejected == nil {
		infra.rejected = make(map[string]tunnelownership.Rejection, len(rejections))
	}

	if infra.broken == nil {
		infra.broken = make(map[string]bool, len(rejections))
	}

	for key, rejection := range rejections {
		infra.rejected[key] = rejection
		delete(infra.resolved, key)

		// Also mark it broken so every fail-closed path already keyed on that
		// set applies unchanged — in particular partitionKeysFor, which would
		// otherwise let the rejected Gateway's routes fall back to the SHARED
		// partition and hand them to the plane this rule exists to protect.
		// A rejection is a decision, not a blip: clear any transient mark so
		// the push cache is not retained for a plane that will keep being
		// refused, and so gatewaySyncError (which checks rejected first)
		// cannot report a refused claim for what was really an apiserver
		// hiccup.
		infra.broken[key] = true
		delete(infra.transient, key)
	}
}

// tunnelRejection reports the refusal recorded for the Gateway key, if any.
// Nil-safe.
func (g *infraGateways) tunnelRejection(key string) (tunnelownership.Rejection, bool) {
	if g == nil {
		return tunnelownership.Rejection{}, false
	}

	rejection, ok := g.rejected[key]

	return rejection, ok
}

// rejectionHolderSuffix qualifies a refusal for a TENANT-visible surface.
//
// It deliberately never names the holding Gateway. The refused tenant reads
// their own Gateway and route status, so naming the holder would hand them the
// namespace and name of a neighbour — the same cross-namespace disclosure this
// rule exists to prevent, in miniature. The operator gets both sides in the
// controller log, which tenants cannot read.
//
// The class tunnel is exempt: it is the operator's, its ID is already
// published in every Gateway's status for external-dns, and saying so is what
// makes the message actionable.
func rejectionHolderSuffix(rejection tunnelownership.Rejection) string {
	if rejection.IsClassTunnel {
		return " (it is the GatewayClass tunnel)"
	}

	return " (it is already in use)"
}

// transientKeys returns the sorted Gateway keys whose resolve failure was
// transient (retryable). Nil-safe.
func (g *infraGateways) transientKeys() []string {
	if g == nil {
		return nil
	}

	keys := make([]string, 0, len(g.transient))
	for key := range g.transient {
		keys = append(keys, key)
	}

	slices.Sort(keys)

	return keys
}

// isResolved reports whether the key is a Gateway that opted in and resolved
// to its own data plane (and therefore its own partition). Nil-safe.
func (g *infraGateways) isResolved(key string) bool {
	if g == nil {
		return false
	}

	_, ok := g.resolved[key]

	return ok
}

// managedInfraGateways lists every Gateway on a GatewayClass this controller
// owns that has opted into a dedicated data plane.
//
// It exists so the partitioner and the claim collector cannot disagree on which
// Gateways are in scope. A Gateway one of them sees and the other does not is
// how the layers reach different verdicts about the same tunnel: arbitration
// that never hears a claim cannot refuse it, and a partition built without it
// serves its routes anyway.
func managedInfraGateways(
	ctx context.Context,
	cli client.Client,
	controllerName string,
) ([]*gatewayv1.Gateway, error) {
	classNames, err := managedClassNames(ctx, cli, controllerName)
	if err != nil {
		return nil, errors.Wrap(err, "listing managed gateway classes")
	}

	var gateways gatewayv1.GatewayList
	if err := cli.List(ctx, &gateways); err != nil {
		return nil, errors.Wrap(err, "listing gateways")
	}

	out := make([]*gatewayv1.Gateway, 0, len(gateways.Items))

	for i := range gateways.Items {
		gateway := &gateways.Items[i]
		if classNames[string(gateway.Spec.GatewayClassName)] && config.HasInfrastructureParametersRef(gateway) {
			out = append(out, gateway)
		}
	}

	return out, nil
}

// resolveInfraGateways returns the per-sync view of every managed Gateway
// opted into a dedicated data plane, keyed "namespace/name". A Gateway whose
// parametersRef does not resolve lands in the broken set (not an error): its
// routes then belong to no partition — deliberately not served anywhere,
// fail closed — and the Gateway reconciler surfaces InvalidParameters on its
// status.
func (s *RouteSyncer) resolveInfraGateways(ctx context.Context) (*infraGateways, error) {
	gateways, err := managedInfraGateways(ctx, s.Client, s.ControllerName)
	if err != nil {
		return nil, err
	}

	logger := logging.FromContext(ctx)
	out := &infraGateways{
		resolved:  make(map[string]*infraGateway),
		broken:    make(map[string]bool),
		transient: make(map[string]bool),
	}

	for _, gateway := range gateways {
		key := gateway.Namespace + "/" + gateway.Name

		perGateway, resolveErr := s.ConfigResolver.ResolveForGateway(ctx, gateway)
		if resolveErr != nil || perGateway == nil {
			// DELIBERATE: transient resolve failures land in broken alongside
			// deterministic ErrInvalidParameters ones. For an isolation
			// feature, fail-closed is the right bias — serving a tenant's
			// routes from a possibly-wrong plane during a blip is worse than
			// briefly not programming route CHANGES (the running data plane
			// keeps its last pushed config either way). The Gateway
			// reconciler still distinguishes the classes for status.
			logger.Warn("per-gateway configuration did not resolve; failing the gateway's routes closed",
				"gateway", key, "error", resolveErr)

			out.broken[key] = true

			// A retryable (non-deterministic) failure is a blip, not a config
			// error: mark it transient so the push cache is retained and the
			// reconcile requeues. A deterministic ErrInvalidParameters (or a
			// nil perGateway with no error) stays evict-and-wait-for-status.
			if resolveErr != nil && !errors.Is(resolveErr, config.ErrInvalidParameters) {
				out.transient[key] = true
			}

			continue
		}

		out.resolved[key] = &infraGateway{
			gateway:    *gateway,
			perGateway: perGateway,
		}
	}

	return out, nil
}

// partitionRoutes assigns every ACCEPTED route to its data-plane
// partition(s): the partition of each opted-in Gateway it is accepted on,
// plus the shared partition when it is accepted on at least one non-opted-in
// Gateway. A multi-parent route appears in each relevant partition. The
// shared partition always exists (the shared plane must converge to an empty
// config when no routes remain) and comes first; infra partitions follow in
// sorted key order for deterministic output.
func partitionRoutes(
	httpResult *httpRouteResult,
	grpcResult *grpcRouteResult,
	infra *infraGateways,
) []routePartition {
	byKey := map[string]*routePartition{
		sharedPartitionKey: {Key: sharedPartitionKey},
	}

	if infra != nil {
		for key, entry := range infra.resolved {
			byKey[key] = &routePartition{
				Key:        key,
				Gateway:    &entry.gateway,
				PerGateway: entry.perGateway,
			}
		}
	}

	for i := range httpResult.accepted {
		route := &httpResult.accepted[i]
		binding := httpResult.bindings[route.Namespace+"/"+route.Name]

		for _, key := range partitionKeysFor(binding, infra) {
			byKey[key].HTTPRoutes = append(byKey[key].HTTPRoutes, httpResult.accepted[i])
		}
	}

	for i := range grpcResult.accepted {
		route := &grpcResult.accepted[i]
		binding := grpcResult.bindings[route.Namespace+"/"+route.Name]

		for _, key := range partitionKeysFor(binding, infra) {
			byKey[key].GRPCRoutes = append(byKey[key].GRPCRoutes, grpcResult.accepted[i])
		}
	}

	keys := make([]string, 0, len(byKey))

	for key := range byKey {
		if key != sharedPartitionKey {
			keys = append(keys, key)
		}
	}

	slices.Sort(keys)

	partitions := make([]routePartition, 0, len(byKey))
	partitions = append(partitions, *byKey[sharedPartitionKey])

	for _, key := range keys {
		partitions = append(partitions, *byKey[key])
	}

	return partitions
}

// partitionKeysFor maps a route's accepted Gateways onto partition keys:
// every RESOLVED infra Gateway contributes its own key; a BROKEN infra
// Gateway contributes nothing at all (fail closed — falling back to shared
// would leak the tenant's hostnames into another data plane); any accepted
// non-infra Gateway contributes the shared key (once).
func partitionKeysFor(binding routeBindingInfo, infra *infraGateways) []string {
	keys := make([]string, 0, len(binding.acceptedGateways))
	sharedSeen := false

	for gatewayKey := range binding.acceptedGateways {
		if infra.isResolved(gatewayKey) {
			keys = append(keys, gatewayKey)

			continue
		}

		if infra.isBroken(gatewayKey) {
			// Opted in but unresolvable: serve nowhere.
			continue
		}

		if !sharedSeen {
			keys = append(keys, sharedPartitionKey)
			sharedSeen = true
		}
	}

	slices.Sort(keys)

	return keys
}

// unionPartitionRoutes rewrites each partition's route set to the UNION of
// all partitions sharing its tunnel. Cloudflare load-balances a tunnel's
// requests across ALL its connectors, so every data plane on one tunnel must
// know every route of that tunnel — otherwise a request landing on the
// "wrong" plane's connector 404s nondeterministically. Partitions on
// distinct tunnels keep their disjoint configs: that distinctness IS the
// isolation, and merging only happens when the operator already chose to
// share a tunnel.
func unionPartitionRoutes(partitions []routePartition, sharedTunnelID string) []routePartition {
	tunnelOf := func(partition *routePartition) string {
		if partition.PerGateway != nil {
			return partition.PerGateway.TunnelID
		}

		return sharedTunnelID
	}

	type routeUnion struct {
		http     []gatewayv1.HTTPRoute
		grpc     []gatewayv1.GRPCRoute
		seenHTTP map[string]bool
		seenGRPC map[string]bool
	}

	unions := make(map[string]*routeUnion)

	for i := range partitions {
		partition := &partitions[i]
		tunnelID := tunnelOf(partition)

		union, ok := unions[tunnelID]
		if !ok {
			union = &routeUnion{seenHTTP: make(map[string]bool), seenGRPC: make(map[string]bool)}
			unions[tunnelID] = union
		}

		for routeIdx := range partition.HTTPRoutes {
			key := partition.HTTPRoutes[routeIdx].Namespace + "/" + partition.HTTPRoutes[routeIdx].Name
			if union.seenHTTP[key] {
				continue
			}

			union.seenHTTP[key] = true

			union.http = append(union.http, partition.HTTPRoutes[routeIdx])
		}

		for routeIdx := range partition.GRPCRoutes {
			key := partition.GRPCRoutes[routeIdx].Namespace + "/" + partition.GRPCRoutes[routeIdx].Name
			if union.seenGRPC[key] {
				continue
			}

			union.seenGRPC[key] = true

			union.grpc = append(union.grpc, partition.GRPCRoutes[routeIdx])
		}
	}

	out := make([]routePartition, len(partitions))

	// Every partition on the same tunnel shares ONE union slice (read-only):
	// the merged route set is identical for all of them. Treat these slices as
	// immutable downstream — an in-place sort or filter would mutate every
	// sibling partition's view. Copy before mutating if you ever need to.
	for i := range partitions {
		out[i] = partitions[i]
		union := unions[tunnelOf(&partitions[i])]
		out[i].HTTPRoutes = union.http
		out[i].GRPCRoutes = union.grpc
	}

	return out
}

// partitionDisplay renders partition keys for logs.
func partitionDisplay(partitions []routePartition) string {
	keys := make([]string, 0, len(partitions))
	for i := range partitions {
		keys = append(keys, partitions[i].Key)
	}

	return strings.Join(keys, ",")
}
