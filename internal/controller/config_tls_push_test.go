package controller

import (
	"context"
	"crypto/tls"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
)

// TestPushToAPlaintextPlane_NamesTheReason pins the diagnostic against what
// net/http actually returns when a TLS push reaches a proxy still serving
// plain HTTP, which is the state of every old pod during an upgrade.
func TestPushToAPlaintextPlane_NamesTheReason(t *testing.T) {
	t.Parallel()

	plain := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(plain.Close)

	syncer := NewProxySyncer("cluster.local", "token", "", fake.NewClientBuilder().Build(),
		slog.New(slog.DiscardHandler), WithConfigAPIAuthority(testAuthority(t)))

	endpoint := "https://" + strings.TrimPrefix(plain.URL, "http://") + "/config"
	_, err := syncer.pushToEndpoints(context.Background(), slog.New(slog.DiscardHandler), &proxy.Config{Version: 1},
		resolveEndpoints(context.Background(), []string{endpoint}), "token")
	require.Error(t, err)

	assert.Contains(t, proxyPushFailureMessage("tenant-a/edge", err), "does not speak TLS")
}

// TestRetainPartitions_DropsTLSPushersOfRemovedPlanes pins that a deleted
// per-Gateway plane does not leave its pusher and connection pool behind.
func TestRetainPartitions_DropsTLSPushersOfRemovedPlanes(t *testing.T) {
	t.Parallel()

	syncer := NewProxySyncer("cluster.local", "token", "", fake.NewClientBuilder().Build(),
		slog.New(slog.DiscardHandler), WithConfigAPIAuthority(testAuthority(t)))

	_, _ = syncer.SyncRoutes(context.Background(), 0,
		[]string{"https://shared-plane.invalid:8081/config"}, nil, nil, nil, nil)
	_, _ = syncer.SyncPartition(context.Background(), 0, "tenant-a/edge", "token",
		[]string{"https://tenant-plane.invalid:8081/config"}, nil, nil, nil, nil)

	require.Contains(t, syncer.tlsPushers, "tenant-plane.invalid")
	require.Contains(t, syncer.tlsPushers, "shared-plane.invalid")

	syncer.RetainPartitions(map[string]bool{})

	assert.NotContains(t, syncer.tlsPushers, "tenant-plane.invalid")
	assert.Contains(t, syncer.tlsPushers, "shared-plane.invalid", "the shared plane is never evicted")
}

// TestRetainPartitions_ClosesEvictedConnectionsWithTracing pins that evicting
// a plane's pusher closes its idle connections even when tracing wraps the
// transport, a wrapper that does not pass CloseIdleConnections through.
func TestRetainPartitions_ClosesEvictedConnectionsWithTracing(t *testing.T) {
	t.Parallel()

	authority := testAuthority(t)

	certPEM, keyPEM, err := authority.Issue([]string{"127.0.0.1"}, time.Now())
	require.NoError(t, err)

	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	require.NoError(t, err)

	var closed atomic.Bool

	plane := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusOK)
	}))
	plane.TLS = &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS13}
	plane.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateClosed {
			closed.Store(true)
		}
	}
	plane.StartTLS()
	t.Cleanup(plane.Close)

	syncer := NewProxySyncer("cluster.local", "token", "", fake.NewClientBuilder().Build(),
		slog.New(slog.DiscardHandler), WithConfigAPIAuthority(authority), WithSyncerTracing())

	_, err = syncer.SyncPartition(context.Background(), 0, "tenant-a/edge", "token",
		[]string{plane.URL + "/config"}, nil, nil, nil, nil)
	require.NoError(t, err)
	require.False(t, closed.Load(), "the push leaves a kept-alive connection behind")

	syncer.RetainPartitions(map[string]bool{})

	assert.Eventually(t, closed.Load, 5*time.Second, 20*time.Millisecond,
		"the evicted plane's idle connection must be closed")
}

// TestGatewayInfraReconciler_ConfigTLSSlotNotYetCachedRequeuesQuietly pins the
// window right after a create, when the cache has not seen the Secret yet:
// the slot is neither skipped (which would roll the plane) nor reported as a
// render failure; the reconcile simply comes back.
func TestGatewayInfraReconciler_ConfigTLSSlotNotYetCachedRequeuesQuietly(t *testing.T) {
	t.Parallel()

	reconciler, _ := newTLSInfraReconciler(t)
	recorder := events.NewFakeRecorder(10)
	reconciler.Recorder = recorder

	slot := edgeLeafKey("0")
	reconciler.Client = interceptor.NewClient(reconciler.Client.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, inner client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if key == slot {
				return apierrors.NewNotFound(schema.GroupResource{Resource: "secrets"}, key.Name)
			}

			return inner.Get(ctx, key, obj, opts...)
		},
		Create: func(ctx context.Context, inner client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if client.ObjectKeyFromObject(obj) == slot {
				return apierrors.NewAlreadyExists(schema.GroupResource{Resource: "secrets"}, obj.GetName())
			}

			return inner.Create(ctx, obj, opts...)
		},
	})

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "edge", Namespace: infraNamespace},
	})
	require.NoError(t, err)
	assert.Positive(t, result.RequeueAfter)
	assert.False(t, drainedEventContains(recorder, eventReasonRenderFailed))

	var next corev1.Secret
	assert.True(t, apierrors.IsNotFound(reconciler.Get(context.Background(), edgeLeafKey("1"), &next)),
		"a slot that is merely not visible yet must not push the plane onto the next one")
}
