package controller

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/api/v1alpha1"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/cfmetrics"
)

const cacheTestTunnel = "99999999-9999-4999-8999-999999999999"

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.now = c.now.Add(d)
}

func (a *recordingTunnelAPI) getsFor(tunnelID string) int {
	a.mu.Lock()
	defer a.mu.Unlock()

	return a.gets[tunnelID]
}

func (a *recordingTunnelAPI) failGets(tunnelID string) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.failGetID = tunnelID
}

func newCacheTestSyncer(t *testing.T) (*RouteSyncer, *recordingTunnelAPI, *fakeClock) {
	t.Helper()

	api := newRecordingTunnelAPI(t)
	syncer := newPartitionSyncSyncer(t, api, cacheTestTunnel)
	clock := &fakeClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	syncer.now = clock.Now

	return syncer, api, clock
}

func mustSync(t *testing.T, syncer *RouteSyncer) {
	t.Helper()

	_, _, err := syncer.SyncAllRoutes(context.Background())
	require.NoError(t, err)
}

// removeSharedRoute changes the class tunnel's desired document.
func removeSharedRoute(t *testing.T, syncer *RouteSyncer) {
	t.Helper()

	require.NoError(t, syncer.Delete(context.Background(), partitionSyncRoute("shared-route", "shared-gw", "")))
}

func restoreSharedRoute(t *testing.T, syncer *RouteSyncer) {
	t.Helper()

	require.NoError(t, syncer.Create(context.Background(),
		partitionSyncRoute("shared-route", "shared-gw", "shared.example.com")))
}

func TestSyncAllRoutes_UnchangedDocumentIsNotReread(t *testing.T) {
	t.Parallel()

	syncer, api, clock := newCacheTestSyncer(t)

	mustSync(t, syncer)
	mustSync(t, syncer)
	assert.Equal(t, 1, api.getsFor(cacheTestTunnel), "a document this controller just wrote is not read again")

	written := api.lastIngress(t, cacheTestTunnel)
	api.seed(cacheTestTunnel, nil) // an edit outside the controller

	clock.Advance(tunnelDocumentTTL - time.Second)
	mustSync(t, syncer)
	assert.Equal(t, 1, api.getsFor(cacheTestTunnel), "the document is trusted until the TTL passes")

	clock.Advance(time.Second)
	mustSync(t, syncer)
	assert.Equal(t, 2, api.getsFor(cacheTestTunnel), "the document is read again once the TTL passes")
	assert.Equal(t, written, api.lastIngress(t, cacheTestTunnel), "the outside edit is reverted")
}

func TestSyncAllRoutes_SkippedReadIsCounted(t *testing.T) {
	t.Parallel()

	syncer, _, _ := newCacheTestSyncer(t)
	registry := prometheus.NewRegistry()
	syncer.Metrics = cfmetrics.NewCollector(registry)

	mustSync(t, syncer)
	mustSync(t, syncer)

	families, err := registry.Gather()
	require.NoError(t, err)

	var skipped float64

	for _, family := range families {
		if family.GetName() == "cftunnel_cloudflare_api_calls_skipped_total" {
			for _, metric := range family.GetMetric() {
				skipped += metric.GetCounter().GetValue()
			}
		}
	}

	assert.InDelta(t, 2, skipped, 0, "the second sync skips the read of both tunnels")
}

func TestSyncAllRoutes_DocumentReadUnchangedIsNotReread(t *testing.T) {
	t.Parallel()

	previous, api, _ := newCacheTestSyncer(t)
	mustSync(t, previous) // a former controller process wrote the document

	syncer := newPartitionSyncSyncer(t, api, cacheTestTunnel)
	mustSync(t, syncer)
	mustSync(t, syncer)

	assert.Equal(t, 2, api.getsFor(cacheTestTunnel), "a document read back unchanged is not read again")
}

func TestSyncAllRoutes_ChangedDocumentIsReread(t *testing.T) {
	t.Parallel()

	syncer, api, _ := newCacheTestSyncer(t)

	mustSync(t, syncer)
	removeSharedRoute(t, syncer)
	mustSync(t, syncer)

	assert.Equal(t, 2, api.getsFor(cacheTestTunnel))
	assert.NotContains(t, api.hostnamesFor(cacheTestTunnel), "shared.example.com")
}

// A failed write may still have landed, so after it the cached document no
// longer says what is deployed.
func TestSyncAllRoutes_FailedWriteDropsCachedDocument(t *testing.T) {
	t.Parallel()

	syncer, api, _ := newCacheTestSyncer(t)

	mustSync(t, syncer)
	api.failTunnel(cacheTestTunnel)
	removeSharedRoute(t, syncer)
	mustSync(t, syncer)

	api.failTunnel("")
	restoreSharedRoute(t, syncer)
	mustSync(t, syncer)

	assert.Equal(t, 3, api.getsFor(cacheTestTunnel), "a failed write must drop the cached document")
}

func TestSyncAllRoutes_FailedReadDropsCachedDocument(t *testing.T) {
	t.Parallel()

	syncer, api, _ := newCacheTestSyncer(t)

	mustSync(t, syncer)
	api.failGets(cacheTestTunnel)
	removeSharedRoute(t, syncer)
	mustSync(t, syncer)

	api.failGets("")
	restoreSharedRoute(t, syncer)
	mustSync(t, syncer)

	assert.Equal(t, 2, api.getsFor(cacheTestTunnel), "a failed read must drop the cached document")
}

func TestSyncAllRoutes_CachedDocumentIsBoundToCredentials(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		rotate func(t *testing.T, c client.Client)
	}{
		{
			name: "API token rotated",
			rotate: func(t *testing.T, c client.Client) {
				t.Helper()

				var secret corev1.Secret
				require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: "creds", Namespace: "default"}, &secret))
				secret.Data["api-token"] = []byte("rotated-token")
				require.NoError(t, c.Update(context.Background(), &secret))
			},
		},
		{
			name: "account ID changed",
			rotate: func(t *testing.T, c client.Client) {
				t.Helper()

				var cfg v1alpha1.GatewayClassConfig
				require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: "cfg"}, &cfg))
				cfg.Spec.AccountID = "other-account"
				require.NoError(t, c.Update(context.Background(), &cfg))
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			syncer, api, _ := newCacheTestSyncer(t)

			mustSync(t, syncer)
			tt.rotate(t, syncer.Client)
			mustSync(t, syncer)

			assert.Equal(t, 2, api.getsFor(cacheTestTunnel), "a cached document is not served to other credentials")
		})
	}
}

func TestSyncAllRoutes_EmptiedTunnelLeavesNoCachedDocument(t *testing.T) {
	t.Parallel()

	syncer, api, _ := newCacheTestSyncer(t)
	syncTenantTunnel(t, syncer, api)
	require.Contains(t, syncer.documents, tenantTunnelUUID)

	optOutInfraGateway(t, syncer)
	mustSync(t, syncer)

	require.Empty(t, api.hostnamesFor(tenantTunnelUUID))
	assert.NotContains(t, syncer.documents, tenantTunnelUUID,
		"a tunnel no Gateway claims any more is never synced again, so its entry would only pile up")
}
