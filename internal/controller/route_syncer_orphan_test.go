package controller

// Pins what happens to a tunnel's ingress document once no partition claims
// that tunnel any more: the next sync empties it rather than leaving the last
// written rules at the edge.

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/api/v1alpha1"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/cfmetrics"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/config"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/tunnelownership"
)

const orphanClassTunnel = "99999999-9999-4999-8999-999999999999"

// syncTenantTunnel runs the first sync of the two-plane fixture and checks the
// tenant tunnel received the tenant hostname, the state every test below
// starts from.
func syncTenantTunnel(t *testing.T, syncer *RouteSyncer, api *recordingTunnelAPI) {
	t.Helper()

	_, _, err := syncer.SyncAllRoutes(context.Background())
	require.NoError(t, err)
	require.Contains(t, api.hostnamesFor(tenantTunnelUUID), "tenant.example.com")
}

func optOutInfraGateway(t *testing.T, syncer *RouteSyncer) {
	t.Helper()

	var gateway gatewayv1.Gateway
	require.NoError(t, syncer.Get(context.Background(),
		client.ObjectKey{Namespace: "default", Name: "infra-gw"}, &gateway))

	gateway.Spec.Infrastructure = nil
	require.NoError(t, syncer.Update(context.Background(), &gateway))
}

func TestSyncAllRoutes_OptOutEmptiesTheAbandonedTunnel(t *testing.T) {
	t.Parallel()

	api := newRecordingTunnelAPI(t)
	syncer := newPartitionSyncSyncer(t, api, orphanClassTunnel)
	syncTenantTunnel(t, syncer, api)

	optOutInfraGateway(t, syncer)

	_, _, err := syncer.SyncAllRoutes(context.Background())
	require.NoError(t, err)

	assert.Empty(t, api.hostnamesFor(tenantTunnelUUID),
		"a tunnel no partition claims must be emptied, not left with its last rules")
	assert.Contains(t, api.hostnamesFor(orphanClassTunnel), "tenant.example.com",
		"the opted-out Gateway's route now belongs to the shared plane")
}

func TestSyncAllRoutes_RefusalEmptiesTheAbandonedTunnel(t *testing.T) {
	t.Parallel()

	api := newRecordingTunnelAPI(t)
	syncer := newPartitionSyncSyncer(t, api, orphanClassTunnel)
	syncTenantTunnel(t, syncer, api)

	var classConfig v1alpha1.GatewayClassConfig
	require.NoError(t, syncer.Get(context.Background(), client.ObjectKey{Name: "cfg"}, &classConfig))

	classConfig.Spec.MaxDataPlanesPerNamespace = new(int32(0))
	require.NoError(t, syncer.Update(context.Background(), &classConfig))

	_, _, err := syncer.SyncAllRoutes(context.Background())
	require.NoError(t, err)

	assert.Empty(t, api.hostnamesFor(tenantTunnelUUID),
		"a refused Gateway's hostnames must leave its tunnel document with its plane")
}

// A tunnel claim Cloudflare stops confirming is refused and its plane removed,
// so the document goes too.
func TestSyncAllRoutes_RefutedClaimEmptiesTheAbandonedTunnel(t *testing.T) {
	t.Parallel()

	api := newRecordingTunnelAPI(t)
	syncer := newPartitionSyncSyncer(t, api, orphanClassTunnel)

	verifier := &switchableClaimVerifier{}
	verifier.proof.Store(int32(tunnelownership.ProofVerified))
	syncer.ConfigResolver = config.NewResolver(syncer.Client, "default", cfmetrics.NewNoopCollector(),
		config.WithClaimVerifier(verifier))

	syncTenantTunnel(t, syncer, api)

	verifier.proof.Store(int32(tunnelownership.ProofRefuted))

	_, _, err := syncer.SyncAllRoutes(context.Background())
	require.NoError(t, err)

	assert.Empty(t, api.hostnamesFor(tenantTunnelUUID))
}

// The credential that wrote the document is gone together with the Gateway,
// its GatewayConfig and its token Secret; the one remembered from the last
// write still empties the document.
func TestSyncAllRoutes_DeletedGatewayEmptiesItsTunnel(t *testing.T) {
	t.Parallel()

	api := newRecordingTunnelAPI(t)
	syncer := newPartitionSyncSyncer(t, api, orphanClassTunnel)
	syncTenantTunnel(t, syncer, api)

	ctx := context.Background()
	for _, obj := range []client.Object{
		&gatewayv1.Gateway{ObjectMeta: metav1.ObjectMeta{Name: "infra-gw", Namespace: "default"}},
		&v1alpha1.GatewayConfig{ObjectMeta: metav1.ObjectMeta{Name: "infra-config", Namespace: "default"}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "infra-token", Namespace: "default"}},
	} {
		require.NoError(t, syncer.Delete(ctx, obj))
	}

	_, _, err := syncer.SyncAllRoutes(ctx)
	require.NoError(t, err)

	assert.Empty(t, api.hostnamesFor(tenantTunnelUUID))
}

// The class tunnel belongs to the operator, who decides when the shared proxy
// moves off it, so changing the class tunnelID must not empty the old one.
func TestSyncAllRoutes_OldClassTunnelIsNeverEmptied(t *testing.T) {
	t.Parallel()

	api := newRecordingTunnelAPI(t)
	syncer := newPartitionSyncSyncer(t, api, orphanClassTunnel)
	syncTenantTunnel(t, syncer, api)
	require.Contains(t, api.hostnamesFor(orphanClassTunnel), "shared.example.com")

	var classConfig v1alpha1.GatewayClassConfig
	require.NoError(t, syncer.Get(context.Background(), client.ObjectKey{Name: "cfg"}, &classConfig))

	classConfig.Spec.TunnelID = "88888888-8888-4888-8888-888888888888"
	require.NoError(t, syncer.Update(context.Background(), &classConfig))

	_, _, err := syncer.SyncAllRoutes(context.Background())
	require.NoError(t, err)

	require.Contains(t, api.hostnamesFor("88888888-8888-4888-8888-888888888888"), "shared.example.com")
	assert.Contains(t, api.hostnamesFor(orphanClassTunnel), "shared.example.com",
		"the old class tunnel keeps its document until the operator moves the shared proxy")
}

// Moving the class tunnelID onto a tunnel only a dedicated plane held refuses
// that plane; the shared document the same sync writes there must stay.
func TestSyncAllRoutes_ClassMovedOntoTenantTunnelKeepsItsDocument(t *testing.T) {
	t.Parallel()

	api := newRecordingTunnelAPI(t)
	syncer := newPartitionSyncSyncer(t, api, orphanClassTunnel)
	syncTenantTunnel(t, syncer, api)

	var classConfig v1alpha1.GatewayClassConfig
	require.NoError(t, syncer.Get(context.Background(), client.ObjectKey{Name: "cfg"}, &classConfig))

	classConfig.Spec.TunnelID = tenantTunnelUUID
	require.NoError(t, syncer.Update(context.Background(), &classConfig))

	_, _, err := syncer.SyncAllRoutes(context.Background())
	require.NoError(t, err)

	assert.Contains(t, api.hostnamesFor(tenantTunnelUUID), "shared.example.com",
		"a tunnel this sync wrote is claimed, whichever partition claims it")
}

// A Gateway whose config stops resolving keeps its last-good plane running, so
// its tunnel keeps the document that plane serves.
func TestSyncAllRoutes_BrokenGatewayKeepsItsTunnelDocument(t *testing.T) {
	t.Parallel()

	api := newRecordingTunnelAPI(t)
	syncer := newPartitionSyncSyncer(t, api, orphanClassTunnel)
	syncTenantTunnel(t, syncer, api)

	require.NoError(t, syncer.Delete(context.Background(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "infra-token", Namespace: "default"},
	}))

	_, _, err := syncer.SyncAllRoutes(context.Background())
	require.NoError(t, err)

	assert.Contains(t, api.hostnamesFor(tenantTunnelUUID), "tenant.example.com",
		"an unresolvable Gateway is not proof its tunnel is abandoned")
}

func TestSyncAllRoutes_TransientlyBrokenGatewayKeepsItsTunnelDocument(t *testing.T) {
	t.Parallel()

	api := newRecordingTunnelAPI(t)

	var failTokenRead bool

	syncer := newPartitionSyncSyncer(t, api, orphanClassTunnel, interceptor.Funcs{
		Get: func(ctx context.Context, cli client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if failTokenRead && key.Name == "infra-token" {
				return apierrors.NewInternalError(assert.AnError)
			}

			return cli.Get(ctx, key, obj, opts...)
		},
	})
	syncTenantTunnel(t, syncer, api)

	failTokenRead = true

	_, _, err := syncer.SyncAllRoutes(context.Background())
	require.NoError(t, err)

	assert.Contains(t, api.hostnamesFor(tenantTunnelUUID), "tenant.example.com")
}

// syncStatusRecorder keeps the status of every recorded sync duration.
type syncStatusRecorder struct {
	cfmetrics.NoopCollector

	mu       sync.Mutex
	statuses []string
}

func (r *syncStatusRecorder) RecordSyncDuration(_ context.Context, status string, _ time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.statuses = append(r.statuses, status)
}

// A sync whose only write empties an abandoned tunnel reports a write, not a
// steady-state skip.
func TestSyncAllRoutes_EmptyingCountsAsWrite(t *testing.T) {
	t.Parallel()

	api := newRecordingTunnelAPI(t)
	syncer := newPartitionSyncSyncer(t, api, orphanClassTunnel)
	syncTenantTunnel(t, syncer, api)

	require.NoError(t, syncer.Delete(context.Background(),
		&gatewayv1.Gateway{ObjectMeta: metav1.ObjectMeta{Name: "infra-gw", Namespace: "default"}}))

	recorder := &syncStatusRecorder{}
	syncer.Metrics = recorder

	_, _, err := syncer.SyncAllRoutes(context.Background())
	require.NoError(t, err)
	require.Empty(t, api.hostnamesFor(tenantTunnelUUID))

	recorder.mu.Lock()
	defer recorder.mu.Unlock()

	assert.Equal(t, []string{"success"}, recorder.statuses)
}

// A write Cloudflare refuses outright, such as one to a tunnel deleted in the
// dashboard, is not retried: no later sync would get a different answer.
func TestSyncAllRoutes_RefusedEmptyingIsNotRetried(t *testing.T) {
	t.Parallel()

	api := newRecordingTunnelAPI(t)
	syncer := newPartitionSyncSyncer(t, api, orphanClassTunnel)
	syncTenantTunnel(t, syncer, api)

	optOutInfraGateway(t, syncer)
	api.failTunnel(tenantTunnelUUID)
	api.mu.Lock()
	api.failStatus = http.StatusNotFound
	api.mu.Unlock()

	_, _, err := syncer.SyncAllRoutes(context.Background())
	require.NoError(t, err)

	api.failTunnel("")

	_, _, err = syncer.SyncAllRoutes(context.Background())
	require.NoError(t, err)

	assert.Contains(t, api.hostnamesFor(tenantTunnelUUID), "tenant.example.com",
		"a tunnel Cloudflare refused to empty is dropped, not written again")
}

// A failed emptying write requeues the sync and is retried instead of
// forgotten, so a quiet cluster does not keep the stale document.
func TestSyncAllRoutes_FailedEmptyingIsRetried(t *testing.T) {
	t.Parallel()

	api := newRecordingTunnelAPI(t)
	syncer := newPartitionSyncSyncer(t, api, orphanClassTunnel)
	syncTenantTunnel(t, syncer, api)

	optOutInfraGateway(t, syncer)
	api.failTunnel(tenantTunnelUUID)

	result, _, err := syncer.SyncAllRoutes(context.Background())
	require.NoError(t, err, "emptying an abandoned tunnel is housekeeping, never a route sync error")
	require.Contains(t, api.hostnamesFor(tenantTunnelUUID), "tenant.example.com")
	assert.Positive(t, result.RequeueAfter, "a pending emptying must bring the sync back")

	api.failTunnel("")

	result, _, err = syncer.SyncAllRoutes(context.Background())
	require.NoError(t, err)

	assert.Empty(t, api.hostnamesFor(tenantTunnelUUID))
	assert.Zero(t, result.RequeueAfter, "nothing is left pending once the tunnel is emptied")
}

// TestSyncAllRoutes_FailedEmptyingIsCounted pins that a failed emptying write
// is counted as a sync error, like every other failed document write.
func TestSyncAllRoutes_FailedEmptyingIsCounted(t *testing.T) {
	t.Parallel()

	api := newRecordingTunnelAPI(t)
	syncer := newPartitionSyncSyncer(t, api, orphanClassTunnel)
	syncTenantTunnel(t, syncer, api)

	reg := prometheus.NewRegistry()
	syncer.Metrics = cfmetrics.NewCollector(reg)

	optOutInfraGateway(t, syncer)
	api.failTunnel(tenantTunnelUUID)

	_, _, err := syncer.SyncAllRoutes(context.Background())
	require.NoError(t, err)

	assert.InDelta(t, 1, gatheredCounterTotal(t, reg, "cftunnel_sync_errors_total"), 0)
}
