package controller

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/cockroachdb/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
)

// raceReplica is a proxy config API stand-in. It accepts PUTs until conflict
// is set, then answers every PUT with 409 and reports a version the process
// counter is above, which the pusher classifies as a lost push race. onPut
// runs inside each PUT before the answer, so a test can change the syncer's
// state while a push is in flight.
type raceReplica struct {
	*httptest.Server

	conflict atomic.Bool
	failing  atomic.Bool

	mu    sync.Mutex
	onPut func()
}

func newRaceReplica(t *testing.T) *raceReplica {
	t.Helper()

	replica := &raceReplica{}
	replica.Server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPut {
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write([]byte(`{"version": 1, "ready": true}`))

			return
		}

		replica.mu.Lock()
		hook := replica.onPut
		replica.mu.Unlock()

		if hook != nil {
			hook()
		}

		switch {
		case replica.failing.Load():
			writer.WriteHeader(http.StatusInternalServerError)
		case replica.conflict.Load():
			writer.WriteHeader(http.StatusConflict)
		default:
			writer.WriteHeader(http.StatusOK)
		}
	}))
	t.Cleanup(replica.Close)

	return replica
}

func (r *raceReplica) setOnPut(hook func()) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.onPut = hook
}

func (r *raceReplica) endpoint() string { return r.URL + "/config" }

const replayRaceKey = "team-a/gw"

// seedPartition runs one successful sync so the partition has a cached
// document to replay, and returns that document.
func seedPartition(t *testing.T, syncer *ProxySyncer, endpoints ...string) *proxy.Config {
	t.Helper()

	_, err := syncer.SyncPartition(context.Background(), 0, replayRaceKey, "", endpoints,
		[]*gatewayv1.HTTPRoute{pushFallbackRoute("r-a", "a.example.com")}, nil, nil, nil)
	require.NoError(t, err)

	syncer.syncMu.Lock()
	defer syncer.syncMu.Unlock()

	require.NotNil(t, syncer.targets[replayRaceKey].lastCfg)

	return syncer.targets[replayRaceKey].lastCfg
}

func newReplaySyncer() *ProxySyncer {
	testClient := fake.NewClientBuilder().WithScheme(runtime.NewScheme()).Build()

	return NewProxySyncer("cluster.local", "", "", testClient, slog.Default())
}

func (s *ProxySyncer) syncsInFlightFor(key string) int {
	s.syncMu.Lock()
	defer s.syncMu.Unlock()

	if target, ok := s.targets[key]; ok {
		return target.syncsInFlight
	}

	return -1
}

// TestSyncPartition_CountsItselfInFlight pins the in-flight count a replay
// consults: one while the sync's push is on the wire, none once it recorded.
func TestSyncPartition_CountsItselfInFlight(t *testing.T) {
	t.Parallel()

	replica := newRaceReplica(t)
	syncer := newReplaySyncer()

	var during atomic.Int64

	replica.setOnPut(func() { during.Store(int64(syncer.syncsInFlightFor(replayRaceKey))) })

	seedPartition(t, syncer, replica.endpoint())

	assert.Equal(t, int64(1), during.Load(), "the sync counts itself while its push is on the wire")
	assert.Zero(t, syncer.syncsInFlightFor(replayRaceKey), "and not once it has recorded the outcome")

	// A steady-state skip pushes nothing, so it is never in flight.
	during.Store(-1)
	seedPartition(t, syncer, replica.endpoint())

	assert.Equal(t, int64(-1), during.Load(), "the identical rebuild is skipped")
	assert.Zero(t, syncer.syncsInFlightFor(replayRaceKey))
}

// TestSyncPartition_InFlightFollowsTheTargetItCounted covers a partition
// evicted while its push is on the wire and recreated under the same key: the
// finishing sync must not decrement the new partition's count.
func TestSyncPartition_InFlightFollowsTheTargetItCounted(t *testing.T) {
	t.Parallel()

	replica := newRaceReplica(t)
	syncer := newReplaySyncer()

	replica.setOnPut(func() {
		syncer.RetainPartitions(map[string]bool{})

		syncer.syncMu.Lock()
		syncer.targetLocked(replayRaceKey)
		syncer.syncMu.Unlock()
	})

	_, err := syncer.SyncPartition(context.Background(), 0, replayRaceKey, "", []string{replica.endpoint()},
		[]*gatewayv1.HTTPRoute{pushFallbackRoute("r-a", "a.example.com")}, nil, nil, nil)
	require.NoError(t, err)

	assert.Zero(t, syncer.syncsInFlightFor(replayRaceKey), "the recreated partition has no sync in flight")
}

// TestResyncTarget_LostRaceToACachedNewerConfigIsSuperseded covers a replay
// whose push lost to a newer document that a sync recorded while the replay
// was on the wire. The next replay carries that document, so this one asks for
// a short requeue instead of failing.
func TestResyncTarget_LostRaceToACachedNewerConfigIsSuperseded(t *testing.T) {
	t.Parallel()

	replica := newRaceReplica(t)
	syncer := newReplaySyncer()
	replayed := seedPartition(t, syncer, replica.endpoint())

	newer := &proxy.Config{Version: replayed.Version + 1}

	replica.conflict.Store(true)
	replica.setOnPut(func() {
		syncer.recordPush(nil, replayRaceKey, "", hashProxyConfig(newer), newer, []string{replica.endpoint()}, nil)
	})

	err := syncer.ResyncPartition(context.Background(), replayRaceKey)
	require.Error(t, err)
	assert.ErrorIs(t, err, errReplaySuperseded)
}

// TestResyncTarget_LostRaceToASyncInFlightIsSuperseded covers a sync that
// delivered its newer document but has not recorded it yet.
func TestResyncTarget_LostRaceToASyncInFlightIsSuperseded(t *testing.T) {
	t.Parallel()

	replica := newRaceReplica(t)
	syncer := newReplaySyncer()
	seedPartition(t, syncer, replica.endpoint())

	replica.conflict.Store(true)
	replica.setOnPut(func() {
		syncer.syncMu.Lock()
		syncer.targets[replayRaceKey].syncsInFlight++
		syncer.syncMu.Unlock()
	})

	err := syncer.ResyncPartition(context.Background(), replayRaceKey)
	require.Error(t, err)
	assert.ErrorIs(t, err, errReplaySuperseded)
}

// TestResyncTarget_LostRaceWithNothingNewerIsAnError covers a replay cache
// that is behind a sync that failed, nothing newer is cached or on its way,
// and every replay would lose the same race. That stays an error, so the
// reconciler backs off instead of looping on a short requeue.
func TestResyncTarget_LostRaceWithNothingNewerIsAnError(t *testing.T) {
	t.Parallel()

	replica := newRaceReplica(t)
	syncer := newReplaySyncer()
	seedPartition(t, syncer, replica.endpoint())

	replica.conflict.Store(true)

	err := syncer.ResyncPartition(context.Background(), replayRaceKey)
	require.Error(t, err)
	assert.ErrorIs(t, err, proxy.ErrLostConfigPushRace)
	assert.NotErrorIs(t, err, errReplaySuperseded)
}

// TestResyncTarget_LostRaceBesideAPlainFailureIsAnError pins that a replay
// counts as superseded only when every failed endpoint lost the race: another
// endpoint's plain failure is not something the next replay fixes.
func TestResyncTarget_LostRaceBesideAPlainFailureIsAnError(t *testing.T) {
	t.Parallel()

	racing := newRaceReplica(t)
	broken := newRaceReplica(t)
	syncer := newReplaySyncer()
	replayed := seedPartition(t, syncer, racing.endpoint(), broken.endpoint())

	newer := &proxy.Config{Version: replayed.Version + 1}

	racing.conflict.Store(true)
	broken.failing.Store(true)
	racing.setOnPut(func() {
		syncer.recordPush(nil, replayRaceKey, "", hashProxyConfig(newer), newer,
			[]string{racing.endpoint(), broken.endpoint()}, nil)
	})

	err := syncer.ResyncPartition(context.Background(), replayRaceKey)
	require.Error(t, err)
	assert.NotErrorIs(t, err, errReplaySuperseded)
}

var errPlainReplay = errors.New("replay failed")

// TestReplayResult pins how a replay error reaches the endpoint reconciler: a
// superseded replay, alone or joined from several partitions, is a short
// requeue with no error; anything else keeps the error and its backoff.
func TestReplayResult(t *testing.T) {
	t.Parallel()

	superseded := errors.Wrap(errReplaySuperseded, "partition a")

	tests := []struct {
		name        string
		err         error
		wantRequeue bool
		wantErr     bool
	}{
		{name: "no error"},
		{name: "superseded", err: superseded, wantRequeue: true},
		{name: "wrapped superseded", err: errors.Wrap(superseded, "outer"), wantRequeue: true},
		{name: "all partitions superseded", err: errors.Join(superseded, errors.Wrap(errReplaySuperseded, "b")), wantRequeue: true},
		{name: "one partition failed", err: errors.Join(superseded, errPlainReplay), wantErr: true},
		{name: "plain failure", err: errPlainReplay, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			result, err := replayResult(tt.err, "resync")
			if tt.wantRequeue {
				assert.Equal(t, lostRacePushRequeueDelay, result.RequeueAfter)
			} else {
				assert.Zero(t, result.RequeueAfter)
			}

			assert.Equal(t, tt.wantErr, err != nil, "error: %v", err)
		})
	}
}

// TestProxyEndpointReconcile_SupersededReplayRequeues drives the reconciler
// through both shared-plane paths, a readable EndpointSlice and a missing one,
// and checks each turns a superseded replay into a short requeue.
func TestProxyEndpointReconcile_SupersededReplayRequeues(t *testing.T) {
	t.Parallel()

	for _, sliceExists := range []bool{true, false} {
		replica := newRaceReplica(t)

		scheme := runtime.NewScheme()
		require.NoError(t, discoveryv1.AddToScheme(scheme))

		builder := fake.NewClientBuilder().WithScheme(scheme)
		if sliceExists {
			builder = builder.WithObjects(&discoveryv1.EndpointSlice{
				ObjectMeta: metav1.ObjectMeta{Name: "es", Namespace: "system"},
			})
		}

		testClient := builder.Build()
		syncer := NewProxySyncer("cluster.local", "", "", testClient, slog.Default())

		_, err := syncer.SyncRoutes(context.Background(), 0, []string{replica.endpoint()},
			[]*gatewayv1.HTTPRoute{pushFallbackRoute("r-a", "a.example.com")}, nil, nil, nil)
		require.NoError(t, err)

		syncer.syncMu.Lock()
		replayed := syncer.targets[sharedPartitionKey].lastCfg
		syncer.syncMu.Unlock()

		newer := &proxy.Config{Version: replayed.Version + 1}

		replica.conflict.Store(true)
		replica.setOnPut(func() {
			syncer.recordPush(nil, sharedPartitionKey, "", hashProxyConfig(newer), newer, []string{replica.endpoint()}, nil)
		})

		reconciler := &ProxyEndpointReconciler{Client: testClient, ProxySyncer: syncer, ProxyEndpoints: []string{replica.endpoint()}}

		result, err := reconciler.Reconcile(context.Background(), ctrl.Request{
			NamespacedName: types.NamespacedName{Name: "es", Namespace: "system"},
		})
		require.NoError(t, err, "slice exists: %v", sliceExists)
		assert.Equal(t, lostRacePushRequeueDelay, result.RequeueAfter, "slice exists: %v", sliceExists)
	}
}
