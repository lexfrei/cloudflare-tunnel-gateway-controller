package controller

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/cfmetrics"
)

// TestPushPartitionConfigs_EarlyErrorResultDoesNotLeakToShared pins the
// early-error isolation contract: when a sync fails before partitioning
// (buildResultForError produces a SyncResult with ALL relevant routes but no
// Partitions), the push step must do NOTHING — pushing the unpartitioned
// route set to the shared endpoints would serve tenant routes from the
// shared data plane (a cross-tenant leak), and evicting the partition caches
// would drop the tenants' replay state over a transient error.
func TestPushPartitionConfigs_EarlyErrorResultDoesNotLeakToShared(t *testing.T) {
	t.Parallel()

	var (
		mu          sync.Mutex
		sharedPuts  int
		sharedHosts []string
	)

	sharedServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPut {
			var cfg struct {
				Rules []struct {
					Hostnames []string `json:"hostnames"`
				} `json:"rules"`
			}
			require.NoError(t, json.NewDecoder(req.Body).Decode(&cfg))

			mu.Lock()
			sharedPuts++

			for _, rule := range cfg.Rules {
				sharedHosts = append(sharedHosts, rule.Hostnames...)
			}
			mu.Unlock()
		}

		writer.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(sharedServer.Close)

	tenantServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(tenantServer.Close)

	testClient := fake.NewClientBuilder().WithScheme(runtime.NewScheme()).Build()
	proxySyncer := NewProxySyncer("cluster.local", "shared-token", "", testClient, slog.Default())

	ctx := context.Background()

	// Populate a tenant partition cache first — a healthy sync happened before
	// the transient error.
	tenantRoute := pushFallbackRoute("tenant-r", "tenant.example.com")

	_, err := proxySyncer.SyncPartition(ctx, "default/tenant-gw", "tenant-token",
		[]string{tenantServer.URL + "/config"}, []*gatewayv1.HTTPRoute{tenantRoute}, nil, nil, nil)
	require.NoError(t, err)

	// The early-error SyncResult: all relevant routes, NO partition split.
	syncResult := &SyncResult{
		HTTPRoutes: []gatewayv1.HTTPRoute{*pushFallbackRoute("shared-r", "shared.example.com"), *tenantRoute},
	}

	params := syncUpdateParams{
		routeSyncer: &RouteSyncer{
			ClusterDomain: "cluster.local",
			Metrics:       cfmetrics.NewNoopCollector(),
		},
		proxySyncer:    proxySyncer,
		proxyEndpoints: []string{sharedServer.URL + "/config"},
		pushProxy:      true,
	}

	diagnostics := pushPartitionConfigs(ctx, slog.Default(), params, syncResult)
	assert.Empty(t, diagnostics)

	mu.Lock()
	assert.Zero(t, sharedPuts, "an unpartitioned (early-error) result must not be pushed anywhere")
	assert.NotContains(t, sharedHosts, "tenant.example.com",
		"tenant hostnames must never reach the shared data plane")
	mu.Unlock()

	// The tenant partition cache must survive: ResyncPartition still replays.
	require.NoError(t, proxySyncer.ResyncPartition(ctx, "default/tenant-gw"),
		"a transient sync error must not evict tenant partition caches")
}

// TestResyncTarget_DoesNotResurrectEvictedPartition pins the TOCTOU edge
// between ResyncPartition's unlock and resyncTarget's re-lock: a concurrent
// RetainPartitions can evict the key in that window, and resyncTarget must
// NOT re-create an empty push target for it — the resurrected garbage entry
// would linger in the map until the next retain pass.
func TestResyncTarget_DoesNotResurrectEvictedPartition(t *testing.T) {
	t.Parallel()

	testClient := fake.NewClientBuilder().WithScheme(runtime.NewScheme()).Build()
	proxySyncer := NewProxySyncer("cluster.local", "", "", testClient, slog.Default())

	require.NoError(t, proxySyncer.resyncTarget(context.Background(), "default/evicted", nil, ""))

	proxySyncer.syncMu.Lock()
	_, exists := proxySyncer.targets["default/evicted"]
	proxySyncer.syncMu.Unlock()

	assert.False(t, exists, "a resync of an evicted partition must not re-create its push state")
}

func pushFallbackRoute(name, hostname string) *gatewayv1.HTTPRoute {
	pathPrefix := gatewayv1.PathMatchPathPrefix
	port := gatewayv1.PortNumber(80)

	return &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: gatewayv1.HTTPRouteSpec{
			Hostnames: []gatewayv1.Hostname{gatewayv1.Hostname(hostname)},
			Rules: []gatewayv1.HTTPRouteRule{
				{
					Matches: []gatewayv1.HTTPRouteMatch{
						{Path: &gatewayv1.HTTPPathMatch{Type: &pathPrefix, Value: new("/")}},
					},
					BackendRefs: []gatewayv1.HTTPBackendRef{
						{BackendRef: gatewayv1.BackendRef{
							BackendObjectReference: gatewayv1.BackendObjectReference{
								Name: "svc", Port: &port,
							},
							Weight: new(int32(1)),
						}},
					},
				},
			},
		},
	}
}
