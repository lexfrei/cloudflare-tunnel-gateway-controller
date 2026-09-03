package controller

import (
	"context"
	"encoding/json"
	"errors"
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
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/cfmetrics"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/config"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
)

// firstOf discards pushPartitionConfigs' lost-race flag where a test only
// inspects diagnostics.
func firstOf(diags []proxy.RouteDiagnostic, _ bool) []proxy.RouteDiagnostic {
	return diags
}

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

// TestPushPartitionConfigs_LostRaceReportsRequeueSignal pins the re-delivery
// guarantee behind the stale-version race fix: a partition push abandoned as a
// lost race must surface through the second return value, because on a quiet
// cluster no further event would otherwise trigger the sync that re-delivers
// the current desired config. With versions reserved at route-snapshot time an
// abandoned push normally carries the older snapshot; the signal remains the
// safety net for unversioned pushes and cross-process races, where version
// order and snapshot order are not tied.
func TestPushPartitionConfigs_LostRaceReportsRequeueSignal(t *testing.T) {
	t.Parallel()

	// A replica that always 409s the PUT and reports a low version on GET is
	// classified as a lost race (its version is below the process counter,
	// which is wall-clock seeded and therefore far higher).
	replica := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPut {
			writer.WriteHeader(http.StatusConflict)

			return
		}

		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"version": 1, "ready": true}`))
	}))
	t.Cleanup(replica.Close)

	testClient := fake.NewClientBuilder().WithScheme(runtime.NewScheme()).Build()
	params := syncUpdateParams{
		routeSyncer:    &RouteSyncer{ClusterDomain: "cluster.local", Metrics: cfmetrics.NewNoopCollector()},
		proxySyncer:    NewProxySyncer("cluster.local", "", "", testClient, slog.Default()),
		proxyEndpoints: []string{replica.URL + "/config"},
		pushProxy:      true,
	}

	syncResult := &SyncResult{Partitions: []routePartition{{Key: sharedPartitionKey}}}

	_, lostRace := pushPartitionConfigs(context.Background(), slog.Default(), &params, syncResult)
	assert.True(t, lostRace, "an abandoned lost-race push must request a requeue")

	// A plain failure (500) is NOT a lost race: the existing failure handling
	// (skip-key invalidation + reconcile-path requeue semantics) covers it.
	failing := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(failing.Close)

	params.proxySyncer = NewProxySyncer("cluster.local", "", "", testClient, slog.Default())
	params.proxyEndpoints = []string{failing.URL + "/config"}

	_, lostRace = pushPartitionConfigs(context.Background(), slog.Default(), &params, syncResult)
	assert.False(t, lostRace, "a plain push failure must not claim the lost-race requeue")
}

// TestWithLostRacePushRequeue pins the requeue mapping: a lost race forces a
// short requeue interval (re-list + rebuild gets the highest version and
// re-delivers), never lengthens an existing shorter one, and leaves the result
// untouched when no race was lost.
func TestWithLostRacePushRequeue(t *testing.T) {
	t.Parallel()

	assert.Equal(t, lostRacePushRequeueDelay,
		withLostRacePushRequeue(ctrl.Result{}, true).RequeueAfter,
		"a lost race on a result without requeue must set the short interval")

	assert.Equal(t, lostRacePushRequeueDelay,
		withLostRacePushRequeue(ctrl.Result{RequeueAfter: apiErrorRequeueDelay}, true).RequeueAfter,
		"a lost race must shorten a longer pending requeue")

	assert.Equal(t, time.Second,
		withLostRacePushRequeue(ctrl.Result{RequeueAfter: time.Second}, true).RequeueAfter,
		"a lost race must not lengthen an already-shorter requeue")

	assert.Equal(t, ctrl.Result{},
		withLostRacePushRequeue(ctrl.Result{}, false),
		"no lost race leaves the result untouched")
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
		diags, _ := pushPartitionConfigs(ctx, slog.Default(), &params, newResult())
		assert.False(t, hasProxyPushDiagnostic(diags),
			"a push failure must not surface before the threshold (attempt %d)", attempt)
	}

	diags, _ := pushPartitionConfigs(ctx, slog.Default(), &params, newResult())
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
		pushPartitionConfigs(ctx, slog.Default(), &params, newResult())
	}

	require.True(t, hasProxyPushDiagnostic(firstOf(pushPartitionConfigs(ctx, slog.Default(), &params, newResult()))),
		"sustained failure must be surfaced before recovery")

	mu.Lock()
	healthy = true
	mu.Unlock()

	diags, _ := pushPartitionConfigs(ctx, slog.Default(), &params, newResult())
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

	_, err := proxySyncer.SyncPartition(ctx, 0, "default/tenant-gw", "tenant-token",
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

	diagnostics, _ := pushPartitionConfigs(ctx, slog.Default(), &params, syncResult)
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
			_, err := proxySyncer.SyncPartition(ctx, 0, fmt.Sprintf("team-%d/gw", i), "",
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
	_, err := proxySyncer.SyncPartition(ctx, 0, "default/tenant-gw", "tenant-token",
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

	_, _ = pushPartitionConfigs(ctx, slog.Default(), &params, syncResult)

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

	_, _ = pushPartitionConfigs(context.Background(), slog.Default(), &params, syncResult)

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

// TestResyncTarget_DoesNotHoldLockAcrossPush pins for resyncTarget what
// TestSyncPartition_ConcurrentPushesDoNotSerializeOnLock pins for SyncPartition:
// syncMu guards the in-memory push state, not the network call, so a replay to a
// wedged connector must not block every other partition's config update. The
// probe is TryLock rather than a wall-clock bound because the question is binary
// and a timing threshold would be flaky under -race.
func TestResyncTarget_DoesNotHoldLockAcrossPush(t *testing.T) {
	t.Parallel()

	entered := make(chan struct{})
	release := make(chan struct{})

	fast := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(fast.Close)

	var once sync.Once

	wedged := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		once.Do(func() { close(entered) })
		<-release
		writer.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(wedged.Close)

	testClient := fake.NewClientBuilder().WithScheme(runtime.NewScheme()).Build()
	proxySyncer := NewProxySyncer("cluster.local", "", "", testClient, slog.Default())
	ctx := context.Background()

	const key = "team-a/gw"

	_, err := proxySyncer.SyncPartition(ctx, 0, key, "",
		[]string{fast.URL + "/config"},
		[]*gatewayv1.HTTPRoute{pushFallbackRoute("r-a", "a.example.com")},
		nil, nil, nil)
	require.NoError(t, err)

	done := make(chan struct{})

	go func() {
		defer close(done)

		_ = proxySyncer.resyncTarget(ctx, key, []string{wedged.URL + "/config"}, "")
	}()

	<-entered

	acquired := proxySyncer.syncMu.TryLock()
	if acquired {
		proxySyncer.syncMu.Unlock()
	}

	close(release)
	<-done

	assert.True(t, acquired, "syncMu must be free while a replay push is in flight")
}

// TestRecordResync_DoesNotOverwriteANewerSkipKey pins the ordering the lock
// split made reachable. A replay reads its config under the lock and releases
// it; a concurrent sync can then push a newer document and record it first. If
// the replay writes its own hash on top, the skip key claims a document the
// endpoints no longer hold, and the next sync that rebuilds it byte for byte is
// skipped while the plane keeps serving the newer one.
func TestRecordResync_DoesNotOverwriteANewerSkipKey(t *testing.T) {
	t.Parallel()

	fast := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(fast.Close)

	testClient := fake.NewClientBuilder().WithScheme(runtime.NewScheme()).Build()
	proxySyncer := NewProxySyncer("cluster.local", "", "", testClient, slog.Default())
	ctx := context.Background()

	const key = "team-a/gw"

	endpoints := []string{fast.URL + "/config"}

	_, err := proxySyncer.SyncPartition(ctx, 0, key, "", endpoints,
		[]*gatewayv1.HTTPRoute{pushFallbackRoute("r-a", "a.example.com")},
		nil, nil, nil)
	require.NoError(t, err)

	proxySyncer.syncMu.Lock()
	replayed := proxySyncer.targets[key].lastCfg
	observed := proxySyncer.targets[key].lastRecordSeq
	proxySyncer.syncMu.Unlock()

	require.NotNil(t, replayed)

	// The document a concurrent sync pushed and recorded while the replay was
	// still in flight.
	newer := &proxy.Config{Version: replayed.Version + 1}
	newerHash := hashProxyConfig(newer)

	proxySyncer.recordPush(key, "", newerHash, newer, endpoints, nil)

	// The replay lands second with the older document it read before unlocking.
	proxySyncer.recordResync(key, replayed, observed, "", endpoints, false)

	proxySyncer.syncMu.Lock()
	gotHash := proxySyncer.targets[key].lastPushedHash
	proxySyncer.syncMu.Unlock()

	assert.Equal(t, newerHash, gotHash,
		"a replay must not claim the endpoints hold the document it replayed when a newer one was recorded after it")
}

// TestRecordResync_DoesNotResurrectAPartitionEvictedDuringThePush covers the
// window the lock split opened: RetainPartitions can evict a partition while a
// replay's push is in flight, and recording the outcome afterwards must not
// re-create the entry. A resurrected target would linger with no Gateway behind
// it until the next retain pass.
func TestRecordResync_DoesNotResurrectAPartitionEvictedDuringThePush(t *testing.T) {
	t.Parallel()

	entered := make(chan struct{})
	release := make(chan struct{})

	fast := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(fast.Close)

	var once sync.Once

	wedged := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		once.Do(func() { close(entered) })
		<-release
		writer.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(wedged.Close)

	testClient := fake.NewClientBuilder().WithScheme(runtime.NewScheme()).Build()
	proxySyncer := NewProxySyncer("cluster.local", "", "", testClient, slog.Default())
	ctx := context.Background()

	const key = "team-a/gw"

	_, err := proxySyncer.SyncPartition(ctx, 0, key, "",
		[]string{fast.URL + "/config"},
		[]*gatewayv1.HTTPRoute{pushFallbackRoute("r-a", "a.example.com")},
		nil, nil, nil)
	require.NoError(t, err)

	done := make(chan struct{})

	go func() {
		defer close(done)

		_ = proxySyncer.resyncTarget(ctx, key, []string{wedged.URL + "/config"}, "")
	}()

	<-entered

	// The Gateway went away while the replay was in flight.
	proxySyncer.RetainPartitions(map[string]bool{})

	close(release)
	<-done

	proxySyncer.syncMu.Lock()
	_, exists := proxySyncer.targets[key]
	proxySyncer.syncMu.Unlock()

	assert.False(t, exists, "recording a replay must not re-create a partition evicted during its push")
}

var errPartialPush = errors.New("partial push failure")

// TestRecordResync_DoesNotRestoreAKeyAConcurrentFailureCleared is the other half
// of the ordering the lock split opened. recordPush's failure branch clears the
// skip key but leaves lastCfg alone, so a replay that finishes afterwards still
// sees its own document cached. Writing its key on top would restore exactly the
// claim the failed push cleared, and a later sync that rebuilds that document
// identically would be skipped while a replica still holds the newer one.
func TestRecordResync_DoesNotRestoreAKeyAConcurrentFailureCleared(t *testing.T) {
	t.Parallel()

	fast := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(fast.Close)

	testClient := fake.NewClientBuilder().WithScheme(runtime.NewScheme()).Build()
	proxySyncer := NewProxySyncer("cluster.local", "", "", testClient, slog.Default())
	ctx := context.Background()

	const key = "team-a/gw"

	endpoints := []string{fast.URL + "/config"}

	_, err := proxySyncer.SyncPartition(ctx, 0, key, "", endpoints,
		[]*gatewayv1.HTTPRoute{pushFallbackRoute("r-a", "a.example.com")},
		nil, nil, nil)
	require.NoError(t, err)

	proxySyncer.syncMu.Lock()
	replayed := proxySyncer.targets[key].lastCfg
	observed := proxySyncer.targets[key].lastRecordSeq
	proxySyncer.syncMu.Unlock()

	require.NotNil(t, replayed)

	// A concurrent sync pushed and failed at some replicas, clearing the key so
	// the next sync re-pushes unconditionally. lastCfg is untouched by that.
	proxySyncer.recordPush(key, "", "", replayed, endpoints, errPartialPush)

	// The replay lands afterwards with the document it read before unlocking.
	proxySyncer.recordResync(key, replayed, observed, "", endpoints, false)

	proxySyncer.syncMu.Lock()
	gotHash := proxySyncer.targets[key].lastPushedHash
	proxySyncer.syncMu.Unlock()

	assert.Empty(t, gotHash,
		"a replay must not restore the skip key a concurrent failed push cleared")
}

// TestRecordResync_EmptyObservedKeyIsNotAToken pins the case a comparison on
// the key's value cannot see. Every failed record clears the key, so a replay
// that read an already-empty key and a concurrent partial failure that emptied
// it again look identical by value. Some replicas hold the newer document, so
// the replay must still not write its own key on top.
func TestRecordResync_EmptyObservedKeyIsNotAToken(t *testing.T) {
	t.Parallel()

	fast := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(fast.Close)

	testClient := fake.NewClientBuilder().WithScheme(runtime.NewScheme()).Build()
	proxySyncer := NewProxySyncer("cluster.local", "", "", testClient, slog.Default())
	ctx := context.Background()

	const key = "team-a/gw"

	endpoints := []string{fast.URL + "/config"}

	_, err := proxySyncer.SyncPartition(ctx, 0, key, "", endpoints,
		[]*gatewayv1.HTTPRoute{pushFallbackRoute("r-a", "a.example.com")},
		nil, nil, nil)
	require.NoError(t, err)

	proxySyncer.syncMu.Lock()
	replayed := proxySyncer.targets[key].lastCfg
	proxySyncer.syncMu.Unlock()

	require.NotNil(t, replayed)

	// An earlier partial failure cleared the key before the replay took its
	// snapshot, so the replay starts from an empty key.
	proxySyncer.recordPush(key, "", "", replayed, endpoints, errPartialPush)

	proxySyncer.syncMu.Lock()
	observed := proxySyncer.targets[key].lastRecordSeq
	clearedKey := proxySyncer.targets[key].lastPushedHash
	proxySyncer.syncMu.Unlock()

	require.Empty(t, clearedKey, "the replay starts from a cleared key")

	// A concurrent newer sync partially fails during the push window. The key
	// is cleared again, so by value nothing has changed since the snapshot.
	newer := &proxy.Config{Version: replayed.Version + 1}
	proxySyncer.recordPush(key, "", "", newer, endpoints, errPartialPush)

	proxySyncer.recordResync(key, replayed, observed, "", endpoints, false)

	proxySyncer.syncMu.Lock()
	gotHash := proxySyncer.targets[key].lastPushedHash
	proxySyncer.syncMu.Unlock()

	assert.Empty(t, gotHash,
		"an empty observed key is not a token: a record happened even though the value did not change")
}

// TestRecordResync_DoesNotOutliveAnEvictedAndRecreatedPartition pins the case a
// per-partition counter cannot see. A replay snapshots the partition, the
// partition is evicted while the push is in flight, and a fresh one is created
// and pushed under the same key. A counter that restarts from zero on the new
// partition can land on the very value the replay observed, so the replay
// writes its old document's key over the new partition's.
func TestRecordResync_DoesNotOutliveAnEvictedAndRecreatedPartition(t *testing.T) {
	t.Parallel()

	fast := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(fast.Close)

	testClient := fake.NewClientBuilder().WithScheme(runtime.NewScheme()).Build()
	proxySyncer := NewProxySyncer("cluster.local", "", "", testClient, slog.Default())
	ctx := context.Background()

	const key = "team-a/gw"

	endpoints := []string{fast.URL + "/config"}

	_, err := proxySyncer.SyncPartition(ctx, 0, key, "", endpoints,
		[]*gatewayv1.HTTPRoute{pushFallbackRoute("r-a", "a.example.com")},
		nil, nil, nil)
	require.NoError(t, err)

	proxySyncer.syncMu.Lock()
	replayed := proxySyncer.targets[key].lastCfg
	observed := proxySyncer.targets[key].lastRecordSeq
	proxySyncer.syncMu.Unlock()

	require.NotNil(t, replayed)

	// The partition goes away and comes back under the same key with a
	// different document while the replay's push is in flight.
	proxySyncer.RetainPartitions(map[string]bool{})

	_, err = proxySyncer.SyncPartition(ctx, 0, key, "", endpoints,
		[]*gatewayv1.HTTPRoute{pushFallbackRoute("r-b", "b.example.com")},
		nil, nil, nil)
	require.NoError(t, err)

	proxySyncer.syncMu.Lock()
	recreatedHash := proxySyncer.targets[key].lastPushedHash
	proxySyncer.syncMu.Unlock()

	require.NotEmpty(t, recreatedHash)

	proxySyncer.recordResync(key, replayed, observed, "", endpoints, false)

	proxySyncer.syncMu.Lock()
	gotHash := proxySyncer.targets[key].lastPushedHash
	proxySyncer.syncMu.Unlock()

	assert.Equal(t, recreatedHash, gotHash,
		"a replay from before the partition was recreated must not claim the new partition's endpoints hold its old document")
}
