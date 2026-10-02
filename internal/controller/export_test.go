package controller

import (
	"context"

	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/ingress"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
)

// SyncRoutes pushes routes to the shared partition, and SyncPartition to the
// partition key, for tests that do not model certificate parents: no route
// pushed through them presents a backend client certificate.
func (s *ProxySyncer) SyncRoutes(
	ctx context.Context,
	configVersion int64,
	endpoints []string,
	routes []*gatewayv1.HTTPRoute,
	grpcRoutes []*gatewayv1.GRPCRoute,
	failedRefs []ingress.BackendRefError,
	grpcFailedRefs []ingress.BackendRefError,
) ([]proxy.RouteDiagnostic, error) {
	return s.syncPartition(ctx, configVersion, sharedPartitionKey, s.defaultAuthToken,
		endpoints, routes, grpcRoutes, failedRefs, grpcFailedRefs, clientCertParents{})
}

func (s *ProxySyncer) SyncPartition(
	ctx context.Context,
	configVersion int64,
	key string,
	authToken string,
	endpoints []string,
	routes []*gatewayv1.HTTPRoute,
	grpcRoutes []*gatewayv1.GRPCRoute,
	failedRefs []ingress.BackendRefError,
	grpcFailedRefs []ingress.BackendRefError,
) ([]proxy.RouteDiagnostic, error) {
	return s.syncPartition(ctx, configVersion, key, authToken,
		endpoints, routes, grpcRoutes, failedRefs, grpcFailedRefs, clientCertParents{})
}

// ResyncEndpointsForTest lets the controller_test package drive the shared
// plane's endpoint replay, without the EndpointSlice check.
func (s *ProxySyncer) ResyncEndpointsForTest(ctx context.Context, endpoints []string) error {
	return s.resyncEndpoints(ctx, endpoints, nil)
}
