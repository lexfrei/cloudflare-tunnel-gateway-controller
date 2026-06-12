package controller_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/controller"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
)

// recordingConfigServer captures pushed configs and Authorization headers.
type recordingConfigServer struct {
	mu      sync.Mutex
	configs []proxy.Config
	tokens  []string
	server  *httptest.Server
}

func newRecordingConfigServer(t *testing.T) *recordingConfigServer {
	t.Helper()

	recorder := &recordingConfigServer{}
	recorder.server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPut {
			var cfg proxy.Config
			if err := json.NewDecoder(req.Body).Decode(&cfg); err != nil {
				writer.WriteHeader(http.StatusBadRequest)

				return
			}

			recorder.mu.Lock()
			recorder.configs = append(recorder.configs, cfg)
			recorder.tokens = append(recorder.tokens, req.Header.Get("Authorization"))
			recorder.mu.Unlock()
		}

		writer.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(recorder.server.Close)

	return recorder
}

func (r *recordingConfigServer) lastConfig(t *testing.T) proxy.Config {
	t.Helper()

	r.mu.Lock()
	defer r.mu.Unlock()
	require.NotEmpty(t, r.configs)

	return r.configs[len(r.configs)-1]
}

func (r *recordingConfigServer) pushCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	return len(r.configs)
}

func (r *recordingConfigServer) lastToken(t *testing.T) string {
	t.Helper()

	r.mu.Lock()
	defer r.mu.Unlock()
	require.NotEmpty(t, r.tokens)

	return r.tokens[len(r.tokens)-1]
}

func partitionTestRoute(name, hostname, svc string) *gatewayv1.HTTPRoute {
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
								Name: gatewayv1.ObjectName(svc), Port: &port,
							},
							Weight: new(int32(1)),
						}},
					},
				},
			},
		},
	}
}

// TestProxySyncer_SyncPartition_IsolatesTargets pins the per-partition push
// contract — the heart of data-plane isolation: each partition's endpoint
// receives ONLY its own routes, authenticated with its own token, and the
// steady-state skip cache is per partition (a change in one partition must
// not suppress or trigger pushes in another).
func TestProxySyncer_SyncPartition_IsolatesTargets(t *testing.T) {
	t.Parallel()

	sharedServer := newRecordingConfigServer(t)
	tenantServer := newRecordingConfigServer(t)

	testClient := fake.NewClientBuilder().WithScheme(runtime.NewScheme()).Build()
	syncer := controller.NewProxySyncer("cluster.local", "shared-token", "", testClient, slog.Default())

	ctx := context.Background()

	sharedRoutes := []*gatewayv1.HTTPRoute{partitionTestRoute("shared-r", "shared.example.com", "svc-a")}
	tenantRoutes := []*gatewayv1.HTTPRoute{partitionTestRoute("tenant-r", "tenant.example.com", "svc-b")}

	_, err := syncer.SyncRoutes(ctx,
		[]string{sharedServer.server.URL + "/config"}, sharedRoutes, nil, nil, nil)
	require.NoError(t, err)

	_, err = syncer.SyncPartition(ctx, "default/tenant-gw", "tenant-token",
		[]string{tenantServer.server.URL + "/config"}, tenantRoutes, nil, nil, nil)
	require.NoError(t, err)

	sharedCfg := sharedServer.lastConfig(t)
	require.Len(t, sharedCfg.Rules, 1)
	assert.Equal(t, []string{"shared.example.com"}, sharedCfg.Rules[0].Hostnames)
	assert.Equal(t, "Bearer shared-token", sharedServer.lastToken(t),
		"the shared partition authenticates with the default token")

	tenantCfg := tenantServer.lastConfig(t)
	require.Len(t, tenantCfg.Rules, 1)
	assert.Equal(t, []string{"tenant.example.com"}, tenantCfg.Rules[0].Hostnames)
	assert.Equal(t, "Bearer tenant-token", tenantServer.lastToken(t),
		"a per-Gateway partition authenticates with ITS token")

	// Steady-state skip must be per partition: re-syncing the unchanged
	// tenant partition is a no-op even though the shared partition pushed in
	// between.
	tenantPushesBefore := tenantServer.pushCount()

	_, err = syncer.SyncPartition(ctx, "default/tenant-gw", "tenant-token",
		[]string{tenantServer.server.URL + "/config"}, tenantRoutes, nil, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, tenantPushesBefore, tenantServer.pushCount(), "unchanged partition must skip the push")

	// A genuine change in the tenant partition pushes again.
	changed := []*gatewayv1.HTTPRoute{partitionTestRoute("tenant-r", "tenant-v2.example.com", "svc-b")}

	_, err = syncer.SyncPartition(ctx, "default/tenant-gw", "tenant-token",
		[]string{tenantServer.server.URL + "/config"}, changed, nil, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, tenantPushesBefore+1, tenantServer.pushCount())
}

// TestProxySyncer_ResyncPartition_ReplaysCachedConfig pins the new-pod
// catch-up path for dedicated planes: a partition resync replays the cached
// config to the partition's stored endpoints without rebuilding from routes.
func TestProxySyncer_ResyncPartition_ReplaysCachedConfig(t *testing.T) {
	t.Parallel()

	tenantServer := newRecordingConfigServer(t)

	testClient := fake.NewClientBuilder().WithScheme(runtime.NewScheme()).Build()
	syncer := controller.NewProxySyncer("cluster.local", "", "", testClient, slog.Default())

	ctx := context.Background()
	routes := []*gatewayv1.HTTPRoute{partitionTestRoute("tenant-r", "tenant.example.com", "svc-b")}

	_, err := syncer.SyncPartition(ctx, "default/tenant-gw", "tenant-token",
		[]string{tenantServer.server.URL + "/config"}, routes, nil, nil, nil)
	require.NoError(t, err)

	pushesBefore := tenantServer.pushCount()

	require.NoError(t, syncer.ResyncPartition(ctx, "default/tenant-gw"))
	assert.Equal(t, pushesBefore+1, tenantServer.pushCount(), "resync must replay the cached config")
	assert.Equal(t, "Bearer tenant-token", tenantServer.lastToken(t), "resync must reuse the partition's token")

	// Unknown partitions are a clean no-op.
	require.NoError(t, syncer.ResyncPartition(ctx, "default/unknown"))
}

// TestProxySyncer_RetainPartitions_EvictsStaleCaches pins cache eviction: a
// deleted Gateway's partition cache must not survive (and must not be
// replayable afterwards).
func TestProxySyncer_RetainPartitions_EvictsStaleCaches(t *testing.T) {
	t.Parallel()

	tenantServer := newRecordingConfigServer(t)

	testClient := fake.NewClientBuilder().WithScheme(runtime.NewScheme()).Build()
	syncer := controller.NewProxySyncer("cluster.local", "", "", testClient, slog.Default())

	ctx := context.Background()
	routes := []*gatewayv1.HTTPRoute{partitionTestRoute("tenant-r", "tenant.example.com", "svc-b")}

	_, err := syncer.SyncPartition(ctx, "default/tenant-gw", "",
		[]string{tenantServer.server.URL + "/config"}, routes, nil, nil, nil)
	require.NoError(t, err)

	syncer.RetainPartitions(map[string]bool{"shared": true})

	pushesBefore := tenantServer.pushCount()
	require.NoError(t, syncer.ResyncPartition(ctx, "default/tenant-gw"))
	assert.Equal(t, pushesBefore, tenantServer.pushCount(), "an evicted partition must not be replayable")
}
