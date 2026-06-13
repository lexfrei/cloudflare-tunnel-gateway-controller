package controller

import (
	"context"
	"slices"
	"strings"

	"github.com/cockroachdb/errors"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/config"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/logging"
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
}

// isBroken reports whether the Gateway key opted in but failed to resolve.
// Nil-safe (a nil view has no infra Gateways at all).
func (g *infraGateways) isBroken(key string) bool {
	return g != nil && g.broken[key]
}

// resolvedEntry returns the resolved data plane for the key, if any. Nil-safe.
func (g *infraGateways) resolvedEntry(key string) (*infraGateway, bool) {
	if g == nil {
		return nil, false
	}

	entry, ok := g.resolved[key]

	return entry, ok
}

// resolveInfraGateways returns the per-sync view of every managed Gateway
// opted into a dedicated data plane, keyed "namespace/name". A Gateway whose
// parametersRef does not resolve lands in the broken set (not an error): its
// routes then belong to no partition — deliberately not served anywhere,
// fail closed — and the Gateway reconciler surfaces InvalidParameters on its
// status.
func (s *RouteSyncer) resolveInfraGateways(ctx context.Context) (*infraGateways, error) {
	classNames, err := managedClassNames(ctx, s.Client, s.ControllerName)
	if err != nil {
		return nil, errors.Wrap(err, "listing managed gateway classes")
	}

	var gateways gatewayv1.GatewayList
	if err := s.List(ctx, &gateways); err != nil {
		return nil, errors.Wrap(err, "listing gateways")
	}

	logger := logging.FromContext(ctx)
	out := &infraGateways{
		resolved: make(map[string]*infraGateway),
		broken:   make(map[string]bool),
	}

	for i := range gateways.Items {
		gateway := &gateways.Items[i]

		if !classNames[string(gateway.Spec.GatewayClassName)] || !config.HasInfrastructureParametersRef(gateway) {
			continue
		}

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

			continue
		}

		out.resolved[key] = &infraGateway{
			gateway:    gateways.Items[i],
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
		if _, isInfra := infra.resolvedEntry(gatewayKey); isInfra {
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

	for i := range partitions {
		out[i] = partitions[i]
		union := unions[tunnelOf(&partitions[i])]
		out[i].HTTPRoutes = union.http
		out[i].GRPCRoutes = union.grpc
	}

	return out
}

// routeKeysOfPartition returns the "namespace/name" keys of every route in
// the partition — used to map a tunnel-group sync failure back onto exactly
// the routes it affects.
func routeKeysOfPartition(partition *routePartition) []string {
	keys := make([]string, 0, len(partition.HTTPRoutes)+len(partition.GRPCRoutes))

	for i := range partition.HTTPRoutes {
		keys = append(keys, partition.HTTPRoutes[i].Namespace+"/"+partition.HTTPRoutes[i].Name)
	}

	for i := range partition.GRPCRoutes {
		keys = append(keys, partition.GRPCRoutes[i].Namespace+"/"+partition.GRPCRoutes[i].Name)
	}

	return keys
}

// partitionDisplay renders partition keys for logs.
func partitionDisplay(partitions []routePartition) string {
	keys := make([]string, 0, len(partitions))
	for i := range partitions {
		keys = append(keys, partitions[i].Key)
	}

	return strings.Join(keys, ",")
}
