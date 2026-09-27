package controller

// Pins what happens to a tunnel's ingress document once no partition claims
// that tunnel any more: the next sync empties it rather than leaving the last
// written rules at the edge.

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/api/v1alpha1"
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

// A failed emptying write is retried by the next sync instead of forgotten.
func TestSyncAllRoutes_FailedEmptyingIsRetried(t *testing.T) {
	t.Parallel()

	api := newRecordingTunnelAPI(t)
	syncer := newPartitionSyncSyncer(t, api, orphanClassTunnel)
	syncTenantTunnel(t, syncer, api)

	optOutInfraGateway(t, syncer)
	api.failTunnel(tenantTunnelUUID)

	_, _, err := syncer.SyncAllRoutes(context.Background())
	require.NoError(t, err, "emptying an abandoned tunnel is housekeeping, never a route sync error")
	require.Contains(t, api.hostnamesFor(tenantTunnelUUID), "tenant.example.com")

	api.failTunnel("")

	_, _, err = syncer.SyncAllRoutes(context.Background())
	require.NoError(t, err)

	assert.Empty(t, api.hostnamesFor(tenantTunnelUUID))
}
