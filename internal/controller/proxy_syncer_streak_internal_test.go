package controller

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// TestResync_LeavesThePushFailureStreakToSyncs pins that replays neither feed
// nor clear the streak behind the sustained-push-failure route condition. A
// replay delivers the cached document, which may be older than the one a
// failing sync is trying to deliver, so its success must not hide that
// failure; its failure is a pod's, not the routes'.
func TestResync_LeavesThePushFailureStreakToSyncs(t *testing.T) {
	t.Parallel()

	var failing atomic.Bool

	replica := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		if failing.Load() {
			writer.WriteHeader(http.StatusInternalServerError)

			return
		}

		writer.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(replica.Close)

	const key = "default/tenant-gw"

	testClient := fake.NewClientBuilder().WithScheme(runtime.NewScheme()).Build()
	syncer := NewProxySyncer("cluster.local", "", "", testClient, slog.Default())
	ctx := context.Background()
	endpoints := []string{replica.URL + "/config"}

	sync := func(hostname string) error {
		route := &gatewayv1.HTTPRoute{
			ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "default"},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{gatewayv1.Hostname(hostname)},
				Rules:     []gatewayv1.HTTPRouteRule{{}},
			},
		}

		_, err := syncer.SyncPartition(ctx, 0, key, "", endpoints, []*gatewayv1.HTTPRoute{route}, nil, nil, nil)

		return err
	}

	require.NoError(t, sync("v1.example.com"), "seeding push must succeed")

	failing.Store(true)

	for range pushFailureSurfaceThreshold {
		require.Error(t, sync("v2.example.com"))
	}

	require.Equal(t, pushFailureSurfaceThreshold, syncer.pushFailureStreak(key))

	failing.Store(false)
	require.NoError(t, syncer.ResyncPartition(ctx, key))
	assert.Equal(t, pushFailureSurfaceThreshold, syncer.pushFailureStreak(key),
		"a successful replay of the cached document must not clear a failing sync's streak")

	failing.Store(true)
	require.Error(t, syncer.ResyncPartition(ctx, key))
	assert.Equal(t, pushFailureSurfaceThreshold, syncer.pushFailureStreak(key),
		"a failed replay must not extend the streak")
}
