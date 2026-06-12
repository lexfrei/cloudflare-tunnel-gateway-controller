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

// resolveInfraGateways returns every managed Gateway opted into a dedicated
// data plane, keyed "namespace/name", with its resolved configuration. A
// Gateway whose parametersRef does not resolve is SKIPPED (not an error):
// its routes then belong to no partition — deliberately not served anywhere,
// fail closed — and the Gateway reconciler surfaces InvalidParameters on its
// status.
func (s *RouteSyncer) resolveInfraGateways(ctx context.Context) (map[string]*infraGateway, error) {
	classNames, err := managedClassNames(ctx, s.Client, s.ControllerName)
	if err != nil {
		return nil, errors.Wrap(err, "listing managed gateway classes")
	}

	var gateways gatewayv1.GatewayList
	if err := s.List(ctx, &gateways); err != nil {
		return nil, errors.Wrap(err, "listing gateways")
	}

	logger := logging.FromContext(ctx)
	out := make(map[string]*infraGateway)

	for i := range gateways.Items {
		gateway := &gateways.Items[i]

		if !classNames[string(gateway.Spec.GatewayClassName)] || !config.HasInfrastructureParametersRef(gateway) {
			continue
		}

		perGateway, resolveErr := s.ConfigResolver.ResolveForGateway(ctx, gateway)
		if resolveErr != nil || perGateway == nil {
			logger.Warn("skipping per-gateway partition: configuration did not resolve",
				"gateway", gateway.Namespace+"/"+gateway.Name, "error", resolveErr)

			continue
		}

		out[gateway.Namespace+"/"+gateway.Name] = &infraGateway{
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
	infra map[string]*infraGateway,
) []routePartition {
	byKey := map[string]*routePartition{
		sharedPartitionKey: {Key: sharedPartitionKey},
	}

	for key, entry := range infra {
		byKey[key] = &routePartition{
			Key:        key,
			Gateway:    &entry.gateway,
			PerGateway: entry.perGateway,
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
// every accepted infra Gateway contributes its own key; any accepted
// non-infra Gateway contributes the shared key (once).
func partitionKeysFor(binding routeBindingInfo, infra map[string]*infraGateway) []string {
	keys := make([]string, 0, len(binding.acceptedGateways))
	sharedSeen := false

	for gatewayKey := range binding.acceptedGateways {
		if _, isInfra := infra[gatewayKey]; isInfra {
			keys = append(keys, gatewayKey)

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
