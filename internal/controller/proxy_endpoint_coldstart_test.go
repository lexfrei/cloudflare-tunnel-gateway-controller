package controller

import (
	"context"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/logging"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/render"
)

// switchableLookup lets a test change what DNS returns between two syncs.
type switchableLookup struct {
	lookup atomic.Pointer[hostLookup]
}

func (s *switchableLookup) set(lookup hostLookup) { s.lookup.Store(&lookup) }

func (s *switchableLookup) resolve(ctx context.Context, host string) ([]string, error) {
	return (*s.lookup.Load())(ctx, host)
}

// coldStart builds a syncer whose first sync of every partition found no
// pods, so nothing is cached, and whose DNS now returns the replica.
func coldStart(t *testing.T, objects ...client.Object) (*ProxySyncer, client.Client, *switchableLookup) {
	t.Helper()

	testClient := fake.NewClientBuilder().WithScheme(replayScheme(t)).WithObjects(objects...).Build()
	syncer := NewProxySyncer("cluster.local", "", "", testClient, slog.Default())

	dns := &switchableLookup{}
	dns.set(nxdomainLookup)
	syncer.lookupHost = dns.resolve

	return syncer, testClient, dns
}

// TestProxyEndpointReconcile_ColdStartRunsARouteSync covers a data plane that
// no replica has accepted a config from, so there is nothing to replay. When
// its pod joins the slice, the reconciler must configure it instead of logging
// a no-op; otherwise the pod gets no config until an unrelated route change,
// and the proxy exits after its two-minute wait. A plane whose first sync ran
// before it had pods gets the config that sync built. One with no sync yet,
// as after a controller restart, gets a route sync.
func TestProxyEndpointReconcile_ColdStartRunsARouteSync(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ perGateway, priorSync bool }{
		{false, true}, {true, true}, {false, false}, {true, false},
	} {
		perGateway := tc.perGateway
		replica := newRaceReplica(t)
		endpoint := serviceEndpoint(t, replica.endpoint())

		var puts atomic.Int32

		replica.setOnPut(func() { puts.Add(1) })

		namespace, key, labels := "system", sharedPartitionKey, map[string]string(nil)
		if perGateway {
			namespace, key, labels = "team-a", replayRaceKey, map[string]string{render.GatewayLabel: render.GatewayLabelValue("gw")}
		}

		syncer, testClient, dns := coldStart(t,
			&gatewayv1.Gateway{ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "team-a"}},
			proxySlice(namespace, labels, sliceEndpoint("127.0.0.1", false)))

		routeSync := func(ctx context.Context) error {
			_, err := syncer.SyncPartition(ctx, 0, key, "", []string{endpoint},
				[]*gatewayv1.HTTPRoute{pushFallbackRoute("r-a", "a.example.com")}, nil, nil, nil)

			return err
		}

		wantSyncs := int32(1)

		if tc.priorSync {
			require.Error(t, routeSync(context.Background()), "the first sync finds no pods")
			require.Zero(t, puts.Load())

			wantSyncs = 0
		}

		dns.set(staleDNSLookup)

		var syncs atomic.Int32

		reconciler := &ProxyEndpointReconciler{
			Client: testClient, ProxySyncer: syncer, ProxyEndpoints: []string{endpoint},
			TriggerRouteSync: func(ctx context.Context) (ctrl.Result, error) {
				syncs.Add(1)

				return ctrl.Result{}, routeSync(ctx)
			},
		}

		result, err := reconcileSlice(t, reconciler, namespace)
		require.NoError(t, err, "per-Gateway: %v", perGateway)
		assert.Equal(t, wantSyncs, syncs.Load(), "%+v: a route sync only when no config was built yet", tc)
		assert.Positive(t, puts.Load(), "per-Gateway: %v: the joining pod receives the config", perGateway)
		assert.Zero(t, result.RequeueAfter, "per-Gateway: %v: the pod took the config", perGateway)
	}
}

// TestProxyEndpointReconcile_PlaneRenderedBeforeItsPodsGetsItsConfig covers a
// new per-Gateway plane on a quiet cluster. Rendering the plane runs a route
// sync before its pods exist, so the push fails and no replica holds a config.
// No further route event comes. When the pods join the slice, the reconciler
// pushes the config that sync built, without another route sync, and keeps
// pushing it every ten seconds until a pod takes it.
func TestProxyEndpointReconcile_PlaneRenderedBeforeItsPodsGetsItsConfig(t *testing.T) {
	t.Parallel()

	replica := newRaceReplica(t)
	endpoint := serviceEndpoint(t, replica.endpoint())

	var puts atomic.Int32

	replica.setOnPut(func() { puts.Add(1) })

	labels := map[string]string{render.GatewayLabel: render.GatewayLabelValue("gw")}
	syncer, testClient, dns := coldStart(t,
		&gatewayv1.Gateway{ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "team-a"}},
		proxySlice("team-a", labels, sliceEndpoint("127.0.0.1", false)))

	_, err := syncer.SyncPartition(context.Background(), 0, replayRaceKey, "", []string{endpoint},
		[]*gatewayv1.HTTPRoute{pushFallbackRoute("r-a", "a.example.com")}, nil, nil, nil)
	require.Error(t, err, "the render-time sync finds no pods")

	dns.set(staleDNSLookup)
	replica.failing.Store(true)

	reconciler := &ProxyEndpointReconciler{
		Client: testClient, ProxySyncer: syncer, ProxyEndpoints: []string{endpoint},
		TriggerRouteSync: func(context.Context) (ctrl.Result, error) {
			t.Error("the built config is pushed; no route sync is needed")

			return ctrl.Result{}, nil
		},
	}

	result, err := reconcileSlice(t, reconciler, "team-a")
	require.NoError(t, err)
	assert.Equal(t, replayRetryDelay, result.RequeueAfter, "a pod whose config API is not up yet is retried")

	replica.failing.Store(false)

	result, err = reconcileSlice(t, reconciler, "team-a")
	require.NoError(t, err)
	assert.Zero(t, result.RequeueAfter)
	assert.Positive(t, puts.Load(), "the pod received the config built before it existed")
}

// TestProxyEndpointReconcile_ColdStartRetriesUntilAPodTakesIt covers the pod
// that is in the slice before its config API listens: the route sync it
// triggers still reaches nobody, and no further slice change will come while
// it waits NotReady, so the reconciler must come back on its own, ten seconds
// later: each retry is a full route sync, so it must not follow the capped
// backoff, which starts at milliseconds.
func TestProxyEndpointReconcile_ColdStartRetriesUntilAPodTakesIt(t *testing.T) {
	t.Parallel()

	syncer, testClient, _ := coldStart(t, proxySlice("system", nil, sliceEndpoint("127.0.0.1", false)))

	endpoint := "http://proxy-config.system.svc:8081/config"
	reconciler := &ProxyEndpointReconciler{
		Client: testClient, ProxySyncer: syncer, ProxyEndpoints: []string{endpoint},
		TriggerRouteSync: func(ctx context.Context) (ctrl.Result, error) {
			_, _ = syncer.SyncRoutes(ctx, 0, []string{endpoint},
				[]*gatewayv1.HTTPRoute{pushFallbackRoute("r-a", "a.example.com")}, nil, nil, nil)

			return ctrl.Result{}, nil
		},
	}

	var logs strings.Builder

	ctx := logging.WithLogger(context.Background(), slog.New(slog.NewTextHandler(&logs, nil)))

	result, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "es", Namespace: "system"}})
	require.NoError(t, err)
	assert.Equal(t, replayRetryDelay, result.RequeueAfter)

	// The warning is the only signal that the plane's pods keep refusing, so
	// it must survive a filter on level.
	assert.Contains(t, logs.String(), `level=WARN msg="no proxy pod of the partition has taken its config yet; retrying"`)
}

// TestProxyEndpointReconcile_ColdStartRetriesWithoutAnotherRouteSync pins the
// cost of a plane whose pods keep refusing their first config. Each route
// sync reads every tunnel's configuration from Cloudflare, so only the first
// retry may run one; later retries push the config that sync built, and the
// first pod that takes it becomes the replay source.
func TestProxyEndpointReconcile_ColdStartRetriesWithoutAnotherRouteSync(t *testing.T) {
	t.Parallel()

	replica := newRaceReplica(t)
	replica.failing.Store(true)

	endpoint := serviceEndpoint(t, replica.endpoint())

	var puts atomic.Int32

	replica.setOnPut(func() { puts.Add(1) })

	syncer, testClient, dns := coldStart(t, proxySlice("system", nil, sliceEndpoint("127.0.0.1", false)))
	dns.set(staleDNSLookup)

	var syncs atomic.Int32

	reconciler := &ProxyEndpointReconciler{
		Client: testClient, ProxySyncer: syncer, ProxyEndpoints: []string{endpoint},
		TriggerRouteSync: func(ctx context.Context) (ctrl.Result, error) {
			syncs.Add(1)

			_, _ = syncer.SyncRoutes(ctx, 0, []string{endpoint},
				[]*gatewayv1.HTTPRoute{pushFallbackRoute("r-a", "a.example.com")}, nil, nil, nil)

			return ctrl.Result{}, nil
		},
	}

	for attempt := range 3 {
		result, err := reconcileSlice(t, reconciler, "system")
		require.NoError(t, err)
		assert.Equal(t, replayRetryDelay, result.RequeueAfter, "attempt %d", attempt)
	}

	assert.Equal(t, int32(1), syncs.Load(), "only the first attempt runs a route sync")
	assert.GreaterOrEqual(t, puts.Load(), int32(3), "every attempt pushes the built config")

	replica.failing.Store(false)

	result, err := reconcileSlice(t, reconciler, "system")
	require.NoError(t, err)
	assert.Zero(t, result.RequeueAfter, "the pod took the config")
	assert.Equal(t, int32(1), syncs.Load())

	_, cached := syncer.replaySource(sharedPartitionKey)
	assert.True(t, cached, "the config the pod took is the replay source")
}

// TestProxyEndpointReconcile_ColdStartRetryLogsWhyThePushFailed pins that a
// retry that pushes the built config says why each push failed. Otherwise the
// only cause on record is the first sync's line, however long the plane stays
// stuck.
func TestProxyEndpointReconcile_ColdStartRetryLogsWhyThePushFailed(t *testing.T) {
	t.Parallel()

	replica := newRaceReplica(t)
	replica.failing.Store(true)

	endpoint := serviceEndpoint(t, replica.endpoint())

	syncer, testClient, dns := coldStart(t, proxySlice("system", nil, sliceEndpoint("127.0.0.1", false)))
	dns.set(staleDNSLookup)

	_, _ = syncer.SyncRoutes(context.Background(), 0, []string{endpoint},
		[]*gatewayv1.HTTPRoute{pushFallbackRoute("r-a", "a.example.com")}, nil, nil, nil)

	var logs strings.Builder

	ctx := logging.WithLogger(context.Background(), slog.New(slog.NewTextHandler(&logs, nil)))
	reconciler := &ProxyEndpointReconciler{
		Client: testClient, ProxySyncer: syncer, ProxyEndpoints: []string{endpoint},
		TriggerRouteSync: func(context.Context) (ctrl.Result, error) {
			t.Error("the built config is pushed, not synced again")

			return ctrl.Result{}, nil
		},
	}

	result, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "es", Namespace: "system"}})
	require.NoError(t, err)
	assert.Equal(t, replayRetryDelay, result.RequeueAfter)

	var failure string

	for line := range strings.Lines(logs.String()) {
		if strings.Contains(line, "failed to push config to endpoint") {
			failure = line
		}
	}

	require.NotEmpty(t, failure, "the failed push is logged")
	assert.Contains(t, failure, "127.0.0.1:", "it names the endpoint")
	assert.Contains(t, failure, "500", "it carries the error")
}

var errRouteSyncFailed = errors.New("route sync failed")

// TestProxyEndpointReconcile_ColdStartSyncFailurePollsForABuiltConfig pins
// that a cold-start route sync that fails before building anything is logged
// at Error and not run again for the same slice version: such a sync, as with
// no GatewayClass yet, would otherwise read Cloudflare every ten seconds
// forever. The reconcile still comes back every ten seconds, with no Cloudflare
// call, and pushes the plane's config as soon as another sync has built it.
func TestProxyEndpointReconcile_ColdStartSyncFailurePollsForABuiltConfig(t *testing.T) {
	t.Parallel()

	replica := newRaceReplica(t)
	endpoint := serviceEndpoint(t, replica.endpoint())

	var puts atomic.Int32

	replica.setOnPut(func() { puts.Add(1) })

	syncer, testClient, dns := coldStart(t, proxySlice("system", nil, sliceEndpoint("127.0.0.1", false)))
	dns.set(staleDNSLookup)

	var syncs atomic.Int32

	reconciler := &ProxyEndpointReconciler{
		Client: testClient, ProxySyncer: syncer, ProxyEndpoints: []string{endpoint},
		TriggerRouteSync: func(context.Context) (ctrl.Result, error) {
			syncs.Add(1)

			return ctrl.Result{}, errRouteSyncFailed
		},
	}

	var logs strings.Builder

	ctx := logging.WithLogger(context.Background(), slog.New(slog.NewTextHandler(&logs, nil)))
	request := ctrl.Request{NamespacedName: types.NamespacedName{Name: "es", Namespace: "system"}}

	for attempt := range 2 {
		result, err := reconciler.Reconcile(ctx, request)
		require.NoError(t, err, "attempt %d", attempt)
		assert.Equal(t, replayRetryDelay, result.RequeueAfter, "attempt %d: the reconcile keeps polling", attempt)
	}

	assert.Equal(t, int32(1), syncs.Load(), "the same slice does not run the failed sync again")
	assert.Zero(t, puts.Load())
	assert.Contains(t, logs.String(), "level=ERROR")
	assert.Contains(t, logs.String(), errRouteSyncFailed.Error())

	var slice discoveryv1.EndpointSlice

	require.NoError(t, testClient.Get(context.Background(), request.NamespacedName, &slice))

	slice.Annotations = map[string]string{"changed": "true"}
	require.NoError(t, testClient.Update(context.Background(), &slice))

	_, err := reconciler.Reconcile(ctx, request)
	require.NoError(t, err)
	require.Equal(t, int32(2), syncs.Load(), "a change to the slice runs the sync again")

	// Another sync builds the config while the pod still refuses it, as a
	// route reconciler retrying its own error would.
	replica.failing.Store(true)

	_, _ = syncer.SyncRoutes(context.Background(), 0, []string{endpoint},
		[]*gatewayv1.HTTPRoute{pushFallbackRoute("r-a", "a.example.com")}, nil, nil, nil)

	replica.failing.Store(false)

	result, err := reconciler.Reconcile(ctx, request)
	require.NoError(t, err)
	assert.Zero(t, result.RequeueAfter, "the poll delivered the built config")
	assert.Equal(t, int32(2), syncs.Load(), "the poll makes no Cloudflare call")

	_, delivered := syncer.replaySource(sharedPartitionKey)
	assert.True(t, delivered)
}

// TestProxyEndpointReconcile_ColdStartSyncThatBuiltButFailedRetriesThePush pins
// a route sync that built the plane's config, failed to deliver it, and then
// returned an error, for another partition or a status write. The config
// exists, so the reconcile takes the paced-push path: it comes back after ten
// seconds and pushes the built config, with no further route sync.
func TestProxyEndpointReconcile_ColdStartSyncThatBuiltButFailedRetriesThePush(t *testing.T) {
	t.Parallel()

	replica := newRaceReplica(t)
	replica.failing.Store(true)

	endpoint := serviceEndpoint(t, replica.endpoint())

	syncer, testClient, dns := coldStart(t, proxySlice("system", nil, sliceEndpoint("127.0.0.1", false)))
	dns.set(staleDNSLookup)

	var syncs atomic.Int32

	reconciler := &ProxyEndpointReconciler{
		Client: testClient, ProxySyncer: syncer, ProxyEndpoints: []string{endpoint},
		TriggerRouteSync: func(ctx context.Context) (ctrl.Result, error) {
			syncs.Add(1)

			_, _ = syncer.SyncRoutes(ctx, 0, []string{endpoint},
				[]*gatewayv1.HTTPRoute{pushFallbackRoute("r-a", "a.example.com")}, nil, nil, nil)

			return ctrl.Result{}, errRouteSyncFailed
		},
	}

	var logs strings.Builder

	ctx := logging.WithLogger(context.Background(), slog.New(slog.NewTextHandler(&logs, nil)))
	request := ctrl.Request{NamespacedName: types.NamespacedName{Name: "es", Namespace: "system"}}

	result, err := reconciler.Reconcile(ctx, request)
	require.NoError(t, err)
	assert.Equal(t, replayRetryDelay, result.RequeueAfter, "a built config is retried")
	assert.Contains(t, logs.String(), "no proxy pod of the partition has taken its config yet",
		"it is reported as an undelivered config, not only as a failed sync")

	_, recorded := reconciler.coldSyncFailedAt(request.NamespacedName)
	assert.False(t, recorded, "a sync that built the config is not recorded as one that built nothing")

	replica.failing.Store(false)

	result, err = reconcileSlice(t, reconciler, "system")
	require.NoError(t, err)
	assert.Zero(t, result.RequeueAfter, "the pod took the built config")
	assert.Equal(t, int32(1), syncs.Load(), "the retry pushes the built config without another sync")
}

// TestProxyEndpointReconcile_ColdStartHonoursTheRouteSyncRequeue pins that a
// sooner requeue the cold-start route sync asks for, after a lost push race
// say, is kept while its plane still has no pod that took the config.
func TestProxyEndpointReconcile_ColdStartHonoursTheRouteSyncRequeue(t *testing.T) {
	t.Parallel()

	syncer, testClient, _ := coldStart(t, proxySlice("system", nil, sliceEndpoint("127.0.0.1", false)))

	const asked = 2 * time.Second

	endpoint := "http://proxy-config.system.svc:8081/config"
	reconciler := &ProxyEndpointReconciler{
		Client: testClient, ProxySyncer: syncer, ProxyEndpoints: []string{endpoint},
		TriggerRouteSync: func(ctx context.Context) (ctrl.Result, error) {
			_, _ = syncer.SyncRoutes(ctx, 0, []string{endpoint},
				[]*gatewayv1.HTTPRoute{pushFallbackRoute("r-a", "a.example.com")}, nil, nil, nil)

			return ctrl.Result{RequeueAfter: asked}, nil
		},
	}

	result, err := reconcileSlice(t, reconciler, "system")
	require.NoError(t, err)
	assert.Equal(t, asked, result.RequeueAfter)
}

// TestProxyEndpointReconcile_ColdStartSyncThatProducesNoPartitionIsNotRerun
// covers the failure the route sync reports only as a requeue: config
// resolution failing, as before any GatewayClass exists, returns RequeueAfter
// with no error and no partitions. The same slice version must not
// run the sync again; the reconcile polls for a built config instead.
func TestProxyEndpointReconcile_ColdStartSyncThatProducesNoPartitionIsNotRerun(t *testing.T) {
	t.Parallel()

	syncer, testClient, _ := coldStart(t, proxySlice("system", nil, sliceEndpoint("127.0.0.1", false)))

	var syncs atomic.Int32

	reconciler := &ProxyEndpointReconciler{
		Client: testClient, ProxySyncer: syncer, ProxyEndpoints: []string{"http://proxy-config.system.svc:8081/config"},
		TriggerRouteSync: func(context.Context) (ctrl.Result, error) {
			syncs.Add(1)

			return ctrl.Result{RequeueAfter: apiErrorRequeueDelay}, nil
		},
	}

	for attempt := range 2 {
		result, err := reconcileSlice(t, reconciler, "system")
		require.NoError(t, err)
		assert.Equal(t, replayRetryDelay, result.RequeueAfter, "attempt %d: polled, not synced", attempt)
	}

	assert.Equal(t, int32(1), syncs.Load(), "the same slice does not run the sync again")
}

// TestProxyEndpointReconcile_ColdStartForAPlaneTheSyncSkips covers a plane the
// route sync does not produce a partition for, as with a Gateway that is
// broken but keeps its last plane running. Retrying would run a full route
// sync every ten seconds until someone fixes the Gateway, so the reconciler
// runs the one sync and then only polls.
func TestProxyEndpointReconcile_ColdStartForAPlaneTheSyncSkips(t *testing.T) {
	t.Parallel()

	syncer, testClient, _ := coldStart(t,
		&gatewayv1.Gateway{ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "team-a"}},
		proxySlice("team-a", map[string]string{render.GatewayLabel: render.GatewayLabelValue("gw")},
			sliceEndpoint("127.0.0.1", false)))

	var syncs atomic.Int32

	reconciler := &ProxyEndpointReconciler{
		Client: testClient, ProxySyncer: syncer,
		TriggerRouteSync: func(context.Context) (ctrl.Result, error) {
			syncs.Add(1)

			return ctrl.Result{}, nil
		},
	}

	for attempt := range 2 {
		result, err := reconcileSlice(t, reconciler, "team-a")
		require.NoError(t, err)
		assert.Equal(t, replayRetryDelay, result.RequeueAfter, "attempt %d: polled, not synced", attempt)
	}

	assert.Equal(t, int32(1), syncs.Load())
}

// TestProxyEndpointReconcile_ColdStartWithNoPodsSyncsNothing pins that a slice
// listing no pods does not run a route sync: there is nobody to configure.
func TestProxyEndpointReconcile_ColdStartWithNoPodsSyncsNothing(t *testing.T) {
	t.Parallel()

	syncer, testClient, _ := coldStart(t, proxySlice("system", nil))

	var syncs atomic.Int32

	reconciler := &ProxyEndpointReconciler{
		Client: testClient, ProxySyncer: syncer, ProxyEndpoints: []string{"http://proxy-config.system.svc:8081/config"},
		TriggerRouteSync: func(context.Context) (ctrl.Result, error) {
			syncs.Add(1)

			return ctrl.Result{}, nil
		},
	}

	result, err := reconcileSlice(t, reconciler, "system")
	require.NoError(t, err)
	assert.Zero(t, syncs.Load())
	assert.Zero(t, result.RequeueAfter)
}

// TestProxyEndpointReconcile_ColdStartPushThatLostTheRaceIsSuperseded pins
// that a built config the proxy refuses as stale, while a sync's newer push
// is on the wire, is a lost race and not a failed push: no Error line, no
// retry warning, and the short lost-race requeue a replay gets. A stale refusal
// with nothing newer coming stays a failure.
func TestProxyEndpointReconcile_ColdStartPushThatLostTheRaceIsSuperseded(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name          string
		syncInFlight  bool
		wantRequeue   time.Duration
		wantFailLines bool
	}{
		{name: "newer push in flight", syncInFlight: true, wantRequeue: lostRacePushRequeueDelay},
		{name: "nothing newer coming", wantRequeue: replayRetryDelay, wantFailLines: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			replica := newRaceReplica(t)
			replica.failing.Store(true)

			endpoint := serviceEndpoint(t, replica.endpoint())

			syncer, testClient, dns := coldStart(t, proxySlice("system", nil, sliceEndpoint("127.0.0.1", false)))
			dns.set(staleDNSLookup)

			_, _ = syncer.SyncRoutes(context.Background(), 0, []string{endpoint},
				[]*gatewayv1.HTTPRoute{pushFallbackRoute("r-a", "a.example.com")}, nil, nil, nil)

			replica.failing.Store(false)
			replica.conflict.Store(true)

			if tt.syncInFlight {
				syncer.syncMu.Lock()
				syncer.targets[sharedPartitionKey].syncsInFlight++
				syncer.syncMu.Unlock()
			}

			var logs strings.Builder

			ctx := logging.WithLogger(context.Background(), slog.New(slog.NewTextHandler(&logs, nil)))
			reconciler := &ProxyEndpointReconciler{
				Client: testClient, ProxySyncer: syncer, ProxyEndpoints: []string{endpoint},
				TriggerRouteSync: func(context.Context) (ctrl.Result, error) {
					t.Error("the built config is pushed, not synced again")

					return ctrl.Result{}, nil
				},
			}

			result, err := reconciler.Reconcile(ctx,
				ctrl.Request{NamespacedName: types.NamespacedName{Name: "es", Namespace: "system"}})
			require.NoError(t, err)
			assert.Equal(t, tt.wantRequeue, result.RequeueAfter)
			assert.Equal(t, tt.wantFailLines, strings.Contains(logs.String(), "failed to push config to endpoint"))
			assert.Equal(t, tt.wantFailLines, strings.Contains(logs.String(), "has taken its config yet"))
		})
	}
}

// namedGatewaySlice is a per-Gateway plane's slice with one pod, named after
// its Gateway.
func namedGatewaySlice(gateway string) *discoveryv1.EndpointSlice {
	slice := proxySlice("team-a", map[string]string{render.GatewayLabel: render.GatewayLabelValue(gateway)},
		sliceEndpoint("127.0.0.1", false))
	slice.Name = "es-" + gateway

	return slice
}

// TestProxyEndpointReconcile_ColdSyncRecordSurvivesOtherSyncs pins that the
// record of a cold-start sync that built nothing outlives the syncs that run
// after it. A plane the sync skips for good, as for a broken Gateway, is never
// in a sync's partitions, so a record tied to them would be erased by every
// sync, its own or another broken plane's, and each ten-second poll would run
// a full route sync again.
func TestProxyEndpointReconcile_ColdSyncRecordSurvivesOtherSyncs(t *testing.T) {
	t.Parallel()

	syncer, testClient, _ := coldStart(t,
		&gatewayv1.Gateway{ObjectMeta: metav1.ObjectMeta{Name: "gw-a", Namespace: "team-a"}},
		&gatewayv1.Gateway{ObjectMeta: metav1.ObjectMeta{Name: "gw-b", Namespace: "team-a"}},
		namedGatewaySlice("gw-a"), namedGatewaySlice("gw-b"))

	var syncs atomic.Int32

	reconciler := &ProxyEndpointReconciler{
		Client: testClient, ProxySyncer: syncer,
		TriggerRouteSync: func(context.Context) (ctrl.Result, error) {
			syncs.Add(1)
			// A sync that serves neither plane retains only the shared one.
			syncer.RetainPartitions(map[string]bool{sharedPartitionKey: true})

			return ctrl.Result{}, nil
		},
	}

	for range 3 {
		for _, name := range []string{"es-gw-a", "es-gw-b"} {
			result, err := reconciler.Reconcile(context.Background(),
				ctrl.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: "team-a"}})
			require.NoError(t, err)
			assert.Equal(t, replayRetryDelay, result.RequeueAfter)
		}
	}

	assert.Equal(t, int32(2), syncs.Load(), "one sync per slice version, whatever other syncs ran")
}

// TestProxyEndpointReconcile_DeletedSliceDropsItsColdSyncRecord pins that the
// record goes with its slice: a removed plane's slice is deleted with its
// Service, so nothing is left behind for a later plane of the same name.
func TestProxyEndpointReconcile_DeletedSliceDropsItsColdSyncRecord(t *testing.T) {
	t.Parallel()

	syncer, testClient, _ := coldStart(t, proxySlice("system", nil, sliceEndpoint("127.0.0.1", false)))

	reconciler := &ProxyEndpointReconciler{
		Client: testClient, ProxySyncer: syncer, ProxyEndpoints: []string{"http://proxy-config.system.svc:8081/config"},
		TriggerRouteSync: func(context.Context) (ctrl.Result, error) {
			return ctrl.Result{RequeueAfter: apiErrorRequeueDelay}, nil
		},
	}

	request := ctrl.Request{NamespacedName: types.NamespacedName{Name: "es", Namespace: "system"}}

	_, err := reconciler.Reconcile(context.Background(), request)
	require.NoError(t, err)

	_, recorded := reconciler.coldSyncFailedAt(request.NamespacedName)
	require.True(t, recorded)

	var slice discoveryv1.EndpointSlice

	require.NoError(t, testClient.Get(context.Background(), request.NamespacedName, &slice))
	require.NoError(t, testClient.Delete(context.Background(), &slice))

	_, _ = reconciler.Reconcile(context.Background(), request)

	_, recorded = reconciler.coldSyncFailedAt(request.NamespacedName)
	assert.False(t, recorded)
}

// TestProxyEndpointReconcile_ColdSyncLeavesItsRequeueToTheRetrier pins that a
// cold-start route sync that asks for a requeue, for work it left on another
// Gateway, gets one even when this plane is settled: the plane took the config,
// so its own reconcile has nothing left to retry, and the retry is owed to the
// route sync retrier.
func TestProxyEndpointReconcile_ColdSyncLeavesItsRequeueToTheRetrier(t *testing.T) {
	t.Parallel()

	replica := newRaceReplica(t)
	endpoint := serviceEndpoint(t, replica.endpoint())

	syncer, testClient, dns := coldStart(t, proxySlice("system", nil, sliceEndpoint("127.0.0.1", false)))
	dns.set(staleDNSLookup)

	retrier := newRouteSyncRetrier(func(ctx context.Context) (ctrl.Result, error) {
		_, _ = syncer.SyncRoutes(ctx, 0, []string{endpoint},
			[]*gatewayv1.HTTPRoute{pushFallbackRoute("r-a", "a.example.com")}, nil, nil, nil)

		return ctrl.Result{RequeueAfter: apiErrorRequeueDelay}, nil
	}, apiErrorRequeueDelay)

	reconciler := &ProxyEndpointReconciler{
		Client: testClient, ProxySyncer: syncer, ProxyEndpoints: []string{endpoint},
		TriggerRouteSync: retrier.Sync,
	}

	result, err := reconcileSlice(t, reconciler, "system")
	require.NoError(t, err)
	assert.Zero(t, result.RequeueAfter, "the plane took the config")
	assert.True(t, retrier.owed(), "the sync's own requeue is not dropped")
}

// TestProxyEndpointReconcile_ColdStartCountsEverySliceOfTheService covers an
// event from a slice with no pods while another slice of the same Service
// lists the pod that joined: the plane has pods to configure, so it is
// configured rather than skipped as having nothing to replay.
func TestProxyEndpointReconcile_ColdStartCountsEverySliceOfTheService(t *testing.T) {
	t.Parallel()

	replica := newRaceReplica(t)
	endpoint := serviceEndpoint(t, replica.endpoint())

	var puts atomic.Int32

	replica.setOnPut(func() { puts.Add(1) })

	other := proxySlice("system", serviceLabel("proxy-config"), sliceEndpoint("127.0.0.1", false))
	other.Name = "es-other"

	syncer, testClient, dns := coldStart(t, proxySlice("system", serviceLabel("proxy-config")), other)
	dns.set(staleDNSLookup)

	var syncs atomic.Int32

	reconciler := &ProxyEndpointReconciler{
		Client: testClient, ProxySyncer: syncer, ProxyEndpoints: []string{endpoint},
		TriggerRouteSync: func(ctx context.Context) (ctrl.Result, error) {
			syncs.Add(1)

			_, err := syncer.SyncRoutes(ctx, 0, []string{endpoint},
				[]*gatewayv1.HTTPRoute{pushFallbackRoute("r-a", "a.example.com")}, nil, nil, nil)

			return ctrl.Result{}, err
		},
	}

	_, err := reconcileSlice(t, reconciler, "system")
	require.NoError(t, err)
	assert.Equal(t, int32(1), syncs.Load(), "the plane with no config gets a route sync")
	assert.Positive(t, puts.Load(), "the pod in the other slice receives the config")
}
