package controller

import (
	"context"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/config"
)

// A per-Gateway plane is pushed to on the config-API port it is rendered with,
// not the proxy's default.
func TestPushPartitionsConcurrently_UsesConfiguredConfigAPIPort(t *testing.T) {
	t.Parallel()

	proxySyncer := NewProxySyncer("cluster.local", "shared-token", "",
		fake.NewClientBuilder().WithScheme(runtime.NewScheme()).Build(), slog.Default())

	params := &syncUpdateParams{
		routeSyncer: &RouteSyncer{ClusterDomain: "cluster.local", ProxyConfigAPIPort: 9091},
		proxySyncer: proxySyncer,
	}

	partitions := []routePartition{{
		Key:        "default/tenant-gw",
		Gateway:    &gatewayv1.Gateway{ObjectMeta: metav1.ObjectMeta{Name: "tenant-gw", Namespace: "default"}},
		PerGateway: &config.PerGatewayConfig{AuthToken: "tenant-token"},
	}}

	pushPartitionsConcurrently(context.Background(), params, &SyncResult{}, partitions)

	proxySyncer.syncMu.Lock()
	defer proxySyncer.syncMu.Unlock()

	target, ok := proxySyncer.targets["default/tenant-gw"]
	require.True(t, ok)
	assert.Equal(t, []string{"http://cf-proxy-tenant-gw-config.default.svc.cluster.local:9091/config"},
		target.endpointURLs)
}
