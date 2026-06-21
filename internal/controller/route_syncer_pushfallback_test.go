package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/cfmetrics"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/config"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
)

// hasProxyPushDiagnostic reports whether any diagnostic surfaces a sustained
// proxy-push failure (#487).
func hasProxyPushDiagnostic(diags []proxy.RouteDiagnostic) bool {
	for _, diag := range diags {
		if diag.Target == proxy.DiagnosticProxyConfigPush {
			return true
		}
	}

	return false
}

// TestPushPartitionConfigs_SustainedPushFailureSurfacesDiagnostic pins the
// #487 no-flap contract: a push that fails is NOT surfaced on route status on
// the first attempts (a transient blip must not flip a condition); only once it
// has failed for a sustained run (the threshold) does a DiagnosticProxyConfigPush
// appear, stamped on the partition's own route.
func TestPushPartitionConfigs_SustainedPushFailureSurfacesDiagnostic(t *testing.T) {
	t.Parallel()

	failing := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(failing.Close)

	testClient := fake.NewClientBuilder().WithScheme(runtime.NewScheme()).Build()
	proxySyncer := NewProxySyncer("cluster.local", "shared-token", "", testClient, slog.Default())
	ctx := context.Background()

	newResult := func() *SyncResult {
		return &SyncResult{
			Partitions: []routePartition{
				{Key: sharedPartitionKey, HTTPRoutes: []gatewayv1.HTTPRoute{*pushFallbackRoute("web", "web.example.com")}},
			},
		}
	}

	params := syncUpdateParams{
		routeSyncer:    &RouteSyncer{ClusterDomain: "cluster.local", Metrics: cfmetrics.NewNoopCollector()},
		proxySyncer:    proxySyncer,
		proxyEndpoints: []string{failing.URL + "/config"},
		pushProxy:      true,
	}

	for attempt := 1; attempt < pushFailureSurfaceThreshold; attempt++ {
		diags := pushPartitionConfigs(ctx, slog.Default(), params, newResult())
		assert.False(t, hasProxyPushDiagnostic(diags),
			"a push failure must not surface before the threshold (attempt %d)", attempt)
	}

	diags := pushPartitionConfigs(ctx, slog.Default(), params, newResult())
	require.True(t, hasProxyPushDiagnostic(diags), "a sustained push failure must surface at the threshold")

	var pushDiag proxy.RouteDiagnostic

	for _, diag := range diags {
		if diag.Target == proxy.DiagnosticProxyConfigPush {
			pushDiag = diag
		}
	}

	assert.Equal(t, "default", pushDiag.Namespace, "the diagnostic stamps the partition's own route")
	assert.Equal(t, "web", pushDiag.Name)
}

// TestPushPartitionConfigs_PushFailureClearsOnRecovery pins that the surfaced
// condition clears: once the push succeeds, the failure streak resets and no
// DiagnosticProxyConfigPush is produced (the rebuilt status drops the condition).
func TestPushPartitionConfigs_PushFailureClearsOnRecovery(t *testing.T) {
	t.Parallel()

	var healthy bool

	var mu sync.Mutex

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		ok := healthy
		mu.Unlock()

		if ok {
			writer.WriteHeader(http.StatusOK)

			return
		}

		writer.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)

	testClient := fake.NewClientBuilder().WithScheme(runtime.NewScheme()).Build()
	proxySyncer := NewProxySyncer("cluster.local", "shared-token", "", testClient, slog.Default())
	ctx := context.Background()

	newResult := func() *SyncResult {
		return &SyncResult{
			Partitions: []routePartition{
				{Key: sharedPartitionKey, HTTPRoutes: []gatewayv1.HTTPRoute{*pushFallbackRoute("web", "web.example.com")}},
			},
		}
	}
	params := syncUpdateParams{
		routeSyncer:    &RouteSyncer{ClusterDomain: "cluster.local", Metrics: cfmetrics.NewNoopCollector()},
		proxySyncer:    proxySyncer,
		proxyEndpoints: []string{server.URL + "/config"},
		pushProxy:      true,
	}

	for range pushFailureSurfaceThreshold {
		pushPartitionConfigs(ctx, slog.Default(), params, newResult())
	}

	require.True(t, hasProxyPushDiagnostic(pushPartitionConfigs(ctx, slog.Default(), params, newResult())),
		"sustained failure must be surfaced before recovery")

	mu.Lock()
	healthy = true
	mu.Unlock()

	diags := pushPartitionConfigs(ctx, slog.Default(), params, newResult())
	assert.False(t, hasProxyPushDiagnostic(diags), "a recovered push must clear the failure diagnostic")
}

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

// TestSyncPartition_ConcurrentPushesDoNotSerializeOnLock pins the #489 lock-free
// push: SyncPartition releases syncMu around the network push, so pushes to
// distinct partitions run concurrently instead of serializing on the lock. With
// the lock held across the push (the old behaviour), N pushes to a slow endpoint
// would take ~N×delay; lock-free they finish in ~one delay. Run under -race to
// pin that the split lock phases are data-race-free.
func TestSyncPartition_ConcurrentPushesDoNotSerializeOnLock(t *testing.T) {
	t.Parallel()

	const (
		partitionCount = 8
		pushDelay      = 60 * time.Millisecond
	)

	slow := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		time.Sleep(pushDelay)
		writer.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(slow.Close)

	testClient := fake.NewClientBuilder().WithScheme(runtime.NewScheme()).Build()
	proxySyncer := NewProxySyncer("cluster.local", "", "", testClient, slog.Default())
	ctx := context.Background()

	var wg sync.WaitGroup

	start := time.Now()

	for i := range partitionCount {
		wg.Go(func() {
			_, err := proxySyncer.SyncPartition(ctx, fmt.Sprintf("team-%d/gw", i), "",
				[]string{slow.URL + "/config"},
				[]*gatewayv1.HTTPRoute{pushFallbackRoute(fmt.Sprintf("r-%d", i), fmt.Sprintf("h%d.example.com", i))},
				nil, nil, nil)
			assert.NoError(t, err)
		})
	}

	wg.Wait()

	elapsed := time.Since(start)

	// Serialized (lock held across the push) would be partitionCount×pushDelay
	// (480ms). Concurrent finishes in ~one delay plus overhead; assert well
	// under the serialized bound with wide margin so a loaded -race run is not flaky.
	assert.Less(t, elapsed, (partitionCount/2)*pushDelay,
		"partition pushes must run concurrently, not serialize on syncMu")
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

// TestPushPartitionConfigs_RetainsTransientBrokenCache pins the A4 contract: a
// Gateway whose resolve failed TRANSIENTLY has no partition this sync, but its
// push cache must survive RetainPartitions (via TransientBrokenKeys) so a pod
// joining during the blip is still replayed the last config — rather than being
// evicted and left at /readyz 503 until an unrelated reconcile.
func TestPushPartitionConfigs_RetainsTransientBrokenCache(t *testing.T) {
	t.Parallel()

	sharedServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
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

	// A healthy sync seeded the tenant partition's cache before the blip.
	tenantRoute := pushFallbackRoute("tenant-r", "tenant.example.com")
	_, err := proxySyncer.SyncPartition(ctx, "default/tenant-gw", "tenant-token",
		[]string{tenantServer.URL + "/config"}, []*gatewayv1.HTTPRoute{tenantRoute}, nil, nil, nil)
	require.NoError(t, err)

	// This sync: the tenant Gateway failed to resolve transiently, so it has NO
	// partition — only the shared partition split — but it is flagged transient.
	syncResult := &SyncResult{
		Partitions:          []routePartition{{Key: sharedPartitionKey}},
		TransientBrokenKeys: []string{"default/tenant-gw"},
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

	pushPartitionConfigs(ctx, slog.Default(), params, syncResult)

	proxySyncer.syncMu.Lock()
	_, exists := proxySyncer.targets["default/tenant-gw"]
	proxySyncer.syncMu.Unlock()

	assert.True(t, exists,
		"a transient-broken Gateway's push cache must be retained, not evicted")
}

// TestPushPartitionConfigs_SameTunnelPartitionsEachGetUnion pins the C7
// integration: when pushPartitionConfigs sees two partitions on the SAME
// tunnel, it unions their routes before pushing, so each endpoint receives the
// merged set (the edge load-balances a tunnel's connectors). Here the shared
// endpoint must receive BOTH the shared and the per-Gateway route, with the
// shared/default token. (The per-Gateway endpoint's own-token delivery is
// pinned by TestProxySyncer_SyncPartition_IsolatesTargets; its push here
// targets a cluster-DNS address that does not resolve in-test.)
func TestPushPartitionConfigs_SameTunnelPartitionsEachGetUnion(t *testing.T) {
	t.Parallel()

	var (
		mu          sync.Mutex
		sharedHosts []string
		sharedToken string
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
			sharedToken = req.Header.Get("Authorization")

			for _, rule := range cfg.Rules {
				sharedHosts = append(sharedHosts, rule.Hostnames...)
			}
			mu.Unlock()
		}

		writer.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(sharedServer.Close)

	testClient := fake.NewClientBuilder().WithScheme(runtime.NewScheme()).Build()
	proxySyncer := NewProxySyncer("cluster.local", "shared-token", "", testClient, slog.Default())

	const sharedTunnel = "550e8400-e29b-41d4-a716-446655440000"

	syncResult := &SyncResult{
		SharedTunnelID: sharedTunnel,
		Partitions: []routePartition{
			{
				Key:        sharedPartitionKey,
				HTTPRoutes: []gatewayv1.HTTPRoute{*pushFallbackRoute("shared-r", "shared.example.com")},
			},
			{
				Key:        "default/tenant-gw",
				Gateway:    &gatewayv1.Gateway{ObjectMeta: metav1.ObjectMeta{Name: "tenant-gw", Namespace: "default"}},
				PerGateway: &config.PerGatewayConfig{ResolvedConfig: config.ResolvedConfig{TunnelID: sharedTunnel}, AuthToken: "tenant-token"},
				HTTPRoutes: []gatewayv1.HTTPRoute{*pushFallbackRoute("tenant-r", "tenant.example.com")},
			},
		},
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

	pushPartitionConfigs(context.Background(), slog.Default(), params, syncResult)

	mu.Lock()
	defer mu.Unlock()

	assert.Contains(t, sharedHosts, "shared.example.com")
	assert.Contains(t, sharedHosts, "tenant.example.com",
		"a same-tunnel per-Gateway partition's route must be unioned into the shared endpoint's push")
	assert.Equal(t, "Bearer shared-token", sharedToken, "the shared endpoint authenticates with the default token")
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
