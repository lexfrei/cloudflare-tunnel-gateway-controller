package controller

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/cockroachdb/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/cfmetrics"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/ingress"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
)

// versionedConfigProxy mimics the real proxy config API's version handling
// (api.go + Router.UpdateConfig): a PUT below the current version is rejected
// with 409, a GET reports the current version. Unlike raceBarrierProxy it
// applies every PUT as it arrives, so the ORDER the pushes land in is the
// order the test writes them.
type versionedConfigProxy struct {
	server *httptest.Server

	mu      sync.Mutex
	version int64
	host    string
}

func newVersionedConfigProxy(t *testing.T) *versionedConfigProxy {
	t.Helper()

	replica := &versionedConfigProxy{}
	replica.server = httptest.NewServer(http.HandlerFunc(replica.handle))
	t.Cleanup(replica.server.Close)

	return replica
}

func (vp *versionedConfigProxy) handle(writer http.ResponseWriter, req *http.Request) {
	if req.Method == http.MethodGet {
		vp.mu.Lock()
		status := proxy.ConfigStatus{Version: vp.version, Ready: true}
		vp.mu.Unlock()

		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusOK)

		data, _ := json.Marshal(status)
		_, _ = writer.Write(data)

		return
	}

	if req.Method != http.MethodPut {
		writer.WriteHeader(http.StatusMethodNotAllowed)

		return
	}

	var cfg proxy.Config

	if err := json.NewDecoder(req.Body).Decode(&cfg); err != nil {
		writer.WriteHeader(http.StatusBadRequest)

		return
	}

	vp.mu.Lock()
	defer vp.mu.Unlock()

	if cfg.Version > 0 && cfg.Version < vp.version {
		writer.WriteHeader(http.StatusConflict)

		return
	}

	vp.version = cfg.Version
	vp.host = firstConfigHost(&cfg)
	writer.WriteHeader(http.StatusOK)
}

func (vp *versionedConfigProxy) snapshot() (int64, string) {
	vp.mu.Lock()
	defer vp.mu.Unlock()

	return vp.version, vp.host
}

func (vp *versionedConfigProxy) endpoints() []string {
	return []string{vp.server.URL + "/config"}
}

// TestSyncPartition_StaleSnapshotBuiltLastDoesNotOverwriteFresherSnapshot pins
// that a partition push carries the version assigned when its routes were
// LISTED, not when its config was built. Two overlapping reconciles are staged
// in the order that inverts the two: the reconcile that listed FIRST (older
// route set) builds and pushes SECOND. Carrying the list-time version makes its
// PUT the stale one, so the replica keeps the fresher route set and the losing
// reconcile reports the race instead of silently force-overwriting.
func TestSyncPartition_StaleSnapshotBuiltLastDoesNotOverwriteFresherSnapshot(t *testing.T) {
	t.Parallel()

	replica := newVersionedConfigProxy(t)

	testClient := fake.NewClientBuilder().WithScheme(runtime.NewScheme()).Build()
	syncer := NewProxySyncer("cluster.local", "", "", testClient, slog.Default())
	ctx := context.Background()

	// Reconcile A lists first and misses the change; reconcile B lists second
	// and sees it. Versions follow the list order.
	staleVersion := proxy.NextConfigVersion()
	freshVersion := proxy.NextConfigVersion()

	// B builds and pushes first.
	_, err := syncer.SyncPartition(ctx, freshVersion, sharedPartitionKey, "", replica.endpoints(),
		[]*gatewayv1.HTTPRoute{pushFallbackRoute("route-b", "b.example.com")}, nil, nil, nil)
	require.NoError(t, err)

	appliedVersion, appliedHost := replica.snapshot()
	require.Equal(t, freshVersion, appliedVersion,
		"the pushed config must carry the version assigned when its routes were listed")
	require.Equal(t, "b.example.com", appliedHost)

	// A builds and pushes second, carrying the older snapshot.
	_, staleErr := syncer.SyncPartition(ctx, staleVersion, sharedPartitionKey, "", replica.endpoints(),
		[]*gatewayv1.HTTPRoute{pushFallbackRoute("route-a", "a.example.com")}, nil, nil, nil)

	require.Error(t, staleErr, "the older snapshot must not be accepted after the newer one")
	assert.True(t, errors.Is(staleErr, proxy.ErrLostConfigPushRace),
		"an older snapshot arriving late is a lost race, not a silent overwrite")

	_, finalHost := replica.snapshot()
	assert.Equal(t, "b.example.com", finalHost,
		"the replica must keep the fresher route set")
}

// TestSyncPartition_UnversionedSnapshotKeepsBuildTimeVersion pins the fallback:
// a caller that supplies no snapshot version (0) still pushes a config with the
// build-time counter value, so the replica's monotonic-version guard has
// something to compare against.
func TestSyncPartition_UnversionedSnapshotKeepsBuildTimeVersion(t *testing.T) {
	t.Parallel()

	replica := newVersionedConfigProxy(t)

	testClient := fake.NewClientBuilder().WithScheme(runtime.NewScheme()).Build()
	syncer := NewProxySyncer("cluster.local", "", "", testClient, slog.Default())

	before := proxy.NextConfigVersion()

	_, err := syncer.SyncPartition(context.Background(), 0, sharedPartitionKey, "", replica.endpoints(),
		[]*gatewayv1.HTTPRoute{pushFallbackRoute("route-a", "a.example.com")}, nil, nil, nil)
	require.NoError(t, err)

	appliedVersion, _ := replica.snapshot()
	assert.Greater(t, appliedVersion, before,
		"without a snapshot version the build-time counter value must still be pushed")
}

// TestSyncAllRoutes_ReplicaAppliesTheVersionTheSyncReserved closes the loop the
// two tests above only cover in halves: a real SyncAllRoutes must RESERVE a
// config version while it holds its route snapshot, and that exact version must
// be what the replica ends up serving. Equality is the pin — a version minted
// during the build would be strictly higher, because the converter increments
// the same counter after the reservation.
func TestSyncAllRoutes_ReplicaAppliesTheVersionTheSyncReserved(t *testing.T) {
	t.Parallel()

	api := newFakeTunnelAPI(t, []map[string]any{{"service": ingress.CatchAllService}})
	syncer := newSkipTestSyncer(t, api)

	ctx := context.Background()

	_, syncResult, err := syncer.SyncAllRoutes(ctx)
	require.NoError(t, err)
	require.NotNil(t, syncResult)
	require.Positive(t, syncResult.ConfigVersion,
		"the sync must reserve a config version while it holds its route snapshot")

	replica := newVersionedConfigProxy(t)
	testClient := fake.NewClientBuilder().WithScheme(runtime.NewScheme()).Build()
	params := syncUpdateParams{
		routeSyncer:    &RouteSyncer{ClusterDomain: "cluster.local", Metrics: cfmetrics.NewNoopCollector()},
		proxySyncer:    NewProxySyncer("cluster.local", "", "", testClient, slog.Default()),
		proxyEndpoints: replica.endpoints(),
		pushProxy:      true,
	}

	_, lostRace := pushPartitionConfigs(ctx, slog.Default(), &params, syncResult)
	require.False(t, lostRace, "a single push against a fresh replica cannot lose a race")

	appliedVersion, _ := replica.snapshot()
	assert.Equal(t, syncResult.ConfigVersion, appliedVersion,
		"the replica must serve exactly the version the sync reserved at route-list time")
}
