package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/config"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
)

// certParentSet builds the per-route Gateway set a partition hands the
// converter: route key -> Gateway keys.
func certParentSet(routeKey string, gatewayKeys ...string) map[string]map[string]bool {
	gateways := make(map[string]bool, len(gatewayKeys))
	for _, key := range gatewayKeys {
		gateways[key] = true
	}

	return map[string]map[string]bool{routeKey: gateways}
}

// TestPartitionRoutes_ClientCertParentsAreTheServingAcceptedGateways pins
// which Gateways may supply a route's backend client certificate in each
// partition: only Gateways the route is accepted on, and only those whose data
// plane is the partition being built. A broken opted-in Gateway serves no
// partition, so it supplies none.
func TestPartitionRoutes_ClientCertParentsAreTheServingAcceptedGateways(t *testing.T) {
	t.Parallel()

	httpResult := &httpRouteResult{
		accepted: []gatewayv1.HTTPRoute{partRoute("multi"), partRoute("shared-only")},
		bindings: map[string]routeBindingInfo{
			"default/multi": bindingOn(
				"default/shared-gw", "default/other-shared-gw", "default/infra-gw", "default/broken-gw"),
			"default/shared-only": bindingOn("default/shared-gw"),
		},
	}
	grpcResult := &grpcRouteResult{
		accepted: []gatewayv1.GRPCRoute{partGRPCRoute("grpc-infra")},
		bindings: map[string]routeBindingInfo{"default/grpc-infra": bindingOn("default/infra-gw")},
	}

	infra := testInfraGateways("default/infra-gw")
	infra.broken = map[string]bool{"default/broken-gw": true}

	byKey := map[string]routePartition{}
	for _, partition := range partitionRoutes(httpResult, grpcResult, infra) {
		byKey[partition.Key] = partition
	}

	shared := byKey[sharedPartitionKey]
	assert.Equal(t, map[string]bool{"default/shared-gw": true, "default/other-shared-gw": true},
		shared.CertParents.http["default/multi"],
		"the shared plane takes a certificate only from accepted Gateways without a dedicated plane")
	assert.Equal(t, map[string]bool{"default/shared-gw": true}, shared.CertParents.http["default/shared-only"])
	assert.NotContains(t, shared.CertParents.grpc, "default/grpc-infra",
		"a route the shared plane does not serve has no certificate parents there")

	dedicated := byKey["default/infra-gw"]
	assert.Equal(t, map[string]bool{"default/infra-gw": true}, dedicated.CertParents.http["default/multi"],
		"a dedicated plane takes a certificate only from its own Gateway")
	assert.NotContains(t, dedicated.CertParents.http, "default/shared-only")
	assert.Equal(t, map[string]bool{"default/infra-gw": true}, dedicated.CertParents.grpc["default/grpc-infra"])
}

// TestUnionPartitionRoutes_MergesClientCertParents pins the same-tunnel case:
// partitions on one tunnel serve the union of their routes, so each of them
// also takes a route's certificate from any Gateway of that union the route is
// accepted on. A partition on another tunnel keeps its own set.
func TestUnionPartitionRoutes_MergesClientCertParents(t *testing.T) {
	t.Parallel()

	partitions := []routePartition{
		{
			Key:         sharedPartitionKey,
			HTTPRoutes:  []gatewayv1.HTTPRoute{partRoute("multi")},
			CertParents: clientCertParents{http: certParentSet("default/multi", "default/shared-gw")},
		},
		{
			Key:         "default/same-tunnel-gw",
			PerGateway:  &config.PerGatewayConfig{TunnelID: "tunnel-1"},
			HTTPRoutes:  []gatewayv1.HTTPRoute{partRoute("multi")},
			CertParents: clientCertParents{http: certParentSet("default/multi", "default/same-tunnel-gw")},
		},
		{
			Key:        "default/own-tunnel-gw",
			PerGateway: &config.PerGatewayConfig{TunnelID: "tunnel-2"},
			GRPCRoutes: []gatewayv1.GRPCRoute{partGRPCRoute("g")},
			CertParents: clientCertParents{
				grpc: certParentSet("default/g", "default/own-tunnel-gw"),
			},
		},
	}

	unioned := unionPartitionRoutes(partitions, "tunnel-1")

	merged := map[string]bool{"default/shared-gw": true, "default/same-tunnel-gw": true}
	assert.Equal(t, merged, unioned[0].CertParents.http["default/multi"])
	assert.Equal(t, merged, unioned[1].CertParents.http["default/multi"],
		"same-tunnel partitions push one identical document")
	assert.Equal(t, map[string]bool{"default/own-tunnel-gw": true}, unioned[2].CertParents.grpc["default/g"])
	assert.Empty(t, unioned[2].CertParents.http, "a partition on its own tunnel gains no parents")
}

const (
	certParentGatewayA = "b/gw-a"
	certParentGatewayB = "b/gw-b"
)

// certParentSyncer builds a syncer over two Gateways in the route's namespace
// that both configure a backend client certificate, with every backend covered
// by backend TLS so the certificate is attached wherever it is selected. The
// config build does not evaluate acceptance, so each case passes its
// certificate parents directly.
func certParentSyncer(t *testing.T) (*ProxySyncer, map[string][]byte) {
	t.Helper()

	certA, keyA := generateClientKeypair(t)
	certB, keyB := generateClientKeypair(t)

	gatewayA := gatewayWithClientCertRef("b", "gw-a", "client-cert-a", nil)
	gatewayB := gatewayWithClientCertRef("b", "gw-b", "client-cert-b", nil)
	gatewayB.Spec.Listeners = []gatewayv1.Listener{{Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType}}

	cli := fake.NewClientBuilder().WithScheme(newClientCertScheme(t)).WithObjects(
		gatewayA, gatewayB,
		clientCertSecret("b", "client-cert-a", certA, keyA),
		clientCertSecret("b", "client-cert-b", certB, keyB),
	).Build()

	syncer := NewProxySyncer("cluster.local", "token", "", cli, nil)
	syncer.tlsResolver = func(context.Context, string, string, int32) *proxy.BackendTLSConfig {
		return &proxy.BackendTLSConfig{ServerName: "backend.example.com"}
	}

	return syncer, map[string][]byte{certParentGatewayA: certA, certParentGatewayB: certB}
}

// certParentRefs puts gw-a first in spec order, so the cases show that the
// certificate parents, not spec order alone, decide.
func certParentRefs() []gatewayv1.ParentReference {
	return []gatewayv1.ParentReference{{Name: "gw-a"}, {Name: "gw-b"}}
}

func certParentBackend() gatewayv1.BackendRef {
	return gatewayv1.BackendRef{BackendObjectReference: gatewayv1.BackendObjectReference{
		Name: "backend", Port: new(gatewayv1.PortNumber(443)),
	}}
}

func certParentHTTPRoute() *gatewayv1.HTTPRoute {
	return &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Namespace: "b", Name: "r"},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: certParentRefs()},
			Hostnames:       []gatewayv1.Hostname{"r.example.com"},
			Rules: []gatewayv1.HTTPRouteRule{{
				BackendRefs: []gatewayv1.HTTPBackendRef{{BackendRef: certParentBackend()}},
			}},
		},
	}
}

func certParentGRPCRoute() *gatewayv1.GRPCRoute {
	return &gatewayv1.GRPCRoute{
		ObjectMeta: metav1.ObjectMeta{Namespace: "b", Name: "r"},
		Spec: gatewayv1.GRPCRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: certParentRefs()},
			Hostnames:       []gatewayv1.Hostname{"g.example.com"},
			Rules: []gatewayv1.GRPCRouteRule{{
				BackendRefs: []gatewayv1.GRPCBackendRef{{BackendRef: certParentBackend()}},
			}},
		},
	}
}

func backendClientCert(t *testing.T, rule proxy.RouteRule) []byte {
	t.Helper()

	require.NotEmpty(t, rule.Backends)
	require.NotNil(t, rule.Backends[0].TLS)

	return rule.Backends[0].TLS.ClientCertPEM
}

// TestBackendClientCert_ComesFromTheServingAcceptedParent builds the proxy
// config the way a partition push does. The route names gw-a first and gw-b
// second; which certificate its backend presents must follow the partition's
// certificate parents, never the order of spec.parentRefs alone.
func TestBackendClientCert_ComesFromTheServingAcceptedParent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		parents  clientCertParents
		wantHTTP string
		wantGRPC string
	}{
		{
			name: "route accepted only on gw-b presents gw-b's certificate",
			parents: clientCertParents{
				http: certParentSet("b/r", certParentGatewayB),
				grpc: certParentSet("b/r", certParentGatewayB),
			},
			wantHTTP: certParentGatewayB,
			wantGRPC: certParentGatewayB,
		},
		{
			name: "each route kind follows its own parents",
			parents: clientCertParents{
				http: certParentSet("b/r", certParentGatewayA),
				grpc: certParentSet("b/r", certParentGatewayB),
			},
			wantHTTP: certParentGatewayA,
			wantGRPC: certParentGatewayB,
		},
		{
			name: "among qualifying parents the first in spec order wins",
			parents: clientCertParents{
				http: certParentSet("b/r", certParentGatewayA, certParentGatewayB),
				grpc: certParentSet("b/r", certParentGatewayA, certParentGatewayB),
			},
			wantHTTP: certParentGatewayA,
			wantGRPC: certParentGatewayA,
		},
		{
			name:    "route with no certificate parent in this partition presents none",
			parents: clientCertParents{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			syncer, certs := certParentSyncer(t)

			cfg := syncer.buildProxyConfig(context.Background(),
				[]*gatewayv1.HTTPRoute{certParentHTTPRoute()}, []*gatewayv1.GRPCRoute{certParentGRPCRoute()},
				nil, nil, tt.parents)

			require.Len(t, cfg.Rules, 2, "one HTTP rule and one gRPC rule")
			assert.Equal(t, certs[tt.wantHTTP], backendClientCert(t, cfg.Rules[0]), "HTTPRoute backend")
			assert.Equal(t, certs[tt.wantGRPC], backendClientCert(t, cfg.Rules[1]), "GRPCRoute backend")
		})
	}
}

// TestPushPartitionsConcurrently_HandsThePartitionItsCertParents pins the
// wiring between the partition split and the push: the pushed config is built
// from the partition's own certificate parents. Shared and dedicated planes go
// through one call; the shared plane is the one a test can push to.
func TestPushPartitionsConcurrently_HandsThePartitionItsCertParents(t *testing.T) {
	t.Parallel()

	syncer, certs := certParentSyncer(t)
	sharedServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(sharedServer.Close)

	params := &syncUpdateParams{
		routeSyncer:    &RouteSyncer{ClusterDomain: "cluster.local"},
		proxySyncer:    syncer,
		proxyEndpoints: []string{sharedServer.URL + "/config"},
	}

	route := *certParentHTTPRoute()
	partitions := []routePartition{
		{
			Key:         sharedPartitionKey,
			HTTPRoutes:  []gatewayv1.HTTPRoute{route},
			CertParents: clientCertParents{http: certParentSet("b/r", certParentGatewayB)},
		},
	}

	pushPartitionsConcurrently(context.Background(), params, &SyncResult{}, partitions)

	syncer.syncMu.Lock()
	defer syncer.syncMu.Unlock()

	target, ok := syncer.targets[sharedPartitionKey]
	require.True(t, ok)
	require.NotNil(t, target.lastCfg, "the push must have succeeded and cached its config")
	require.Len(t, target.lastCfg.Rules, 1)
	assert.Equal(t, certs[certParentGatewayB], backendClientCert(t, target.lastCfg.Rules[0]))
}
