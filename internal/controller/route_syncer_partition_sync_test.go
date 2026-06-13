package controller

// Pins the per-tunnel Cloudflare sync split (#479): routes bound to a Gateway
// with a dedicated data plane go to THAT Gateway's tunnel document; shared
// routes go to the class tunnel; partitions resolving to the SAME tunnel are
// merged into one document write (no last-writer-wins).

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/cloudflare/cloudflare-go/v7"
	"github.com/cloudflare/cloudflare-go/v7/option"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/api/v1alpha1"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/cfmetrics"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/config"
)

const tenantTunnelUUID = "550e8400-e29b-41d4-a716-446655440000"

// recordingTunnelAPI serves GET (catch-all-only current config) for every
// tunnel and records each PUT's tunnel ID and the hostnames it carried.
type recordingTunnelAPI struct {
	server *httptest.Server

	mu           sync.Mutex
	puts         map[string][]string // tunnelID -> hostnames in the written document
	failTunnelID string              // PUTs to this tunnel ID return 500
}

// failTunnel makes every PUT to tunnelID return a 5xx, simulating one tunnel's
// Cloudflare write failing while others succeed.
func (a *recordingTunnelAPI) failTunnel(tunnelID string) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.failTunnelID = tunnelID
}

func (a *recordingTunnelAPI) shouldFail(tunnelID string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()

	return a.failTunnelID == tunnelID
}

func newRecordingTunnelAPI(t *testing.T) *recordingTunnelAPI {
	t.Helper()

	api := &recordingTunnelAPI{puts: make(map[string][]string)}

	api.server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		writer.Header().Set("Content-Type", "application/json")

		switch req.Method {
		case http.MethodGet:
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"success": true, "errors": []any{},
				"result": map[string]any{"config": map[string]any{"ingress": []map[string]any{
					{"service": "http_status:404"},
				}}},
			})
		case http.MethodPut:
			// Path: /accounts/<acct>/cfd_tunnel/<tunnelID>/configurations
			segments := strings.Split(strings.Trim(req.URL.Path, "/"), "/")
			tunnelID := segments[len(segments)-2]

			if api.shouldFail(tunnelID) {
				writer.WriteHeader(http.StatusInternalServerError)
				_ = json.NewEncoder(writer).Encode(map[string]any{
					"success": false,
					"errors":  []any{map[string]any{"code": 1000, "message": "simulated tunnel write failure"}},
				})

				return
			}

			var body struct {
				Config struct {
					Ingress []struct {
						Hostname string `json:"hostname"`
					} `json:"ingress"`
				} `json:"config"`
			}
			_ = json.NewDecoder(req.Body).Decode(&body)

			hostnames := make([]string, 0, len(body.Config.Ingress))

			for _, rule := range body.Config.Ingress {
				if rule.Hostname != "" {
					hostnames = append(hostnames, rule.Hostname)
				}
			}

			api.mu.Lock()
			api.puts[tunnelID] = hostnames
			api.mu.Unlock()

			_ = json.NewEncoder(writer).Encode(map[string]any{
				"success": true, "errors": []any{},
				"result": map[string]any{"config": map[string]any{}},
			})
		default:
			writer.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(api.server.Close)

	return api
}

func (a *recordingTunnelAPI) hostnamesFor(tunnelID string) []string {
	a.mu.Lock()
	defer a.mu.Unlock()

	return a.puts[tunnelID]
}

func (a *recordingTunnelAPI) tunnelsWritten() int {
	a.mu.Lock()
	defer a.mu.Unlock()

	return len(a.puts)
}

func partitionSyncToken(t *testing.T) string {
	t.Helper()

	payload, err := json.Marshal(map[string]any{
		"a": "abcdef0123456789abcdef0123456789",
		"s": base64.StdEncoding.EncodeToString([]byte("secret")),
		"t": tenantTunnelUUID,
	})
	require.NoError(t, err)

	return base64.StdEncoding.EncodeToString(payload)
}

func partitionSyncRoute(name, gatewayName, hostname string) *gatewayv1.HTTPRoute {
	pathPrefix := gatewayv1.PathMatchPathPrefix
	port := gatewayv1.PortNumber(80)

	return &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{{Name: gatewayv1.ObjectName(gatewayName)}},
			},
			Hostnames: []gatewayv1.Hostname{gatewayv1.Hostname(hostname)},
			Rules: []gatewayv1.HTTPRouteRule{
				{
					Matches: []gatewayv1.HTTPRouteMatch{
						{Path: &gatewayv1.HTTPPathMatch{Type: &pathPrefix, Value: new("/")}},
					},
					BackendRefs: []gatewayv1.HTTPBackendRef{
						{BackendRef: gatewayv1.BackendRef{
							BackendObjectReference: gatewayv1.BackendObjectReference{
								Name: "svc", Port: &port,
							},
							Weight: new(int32(1)),
						}},
					},
				},
			},
		},
	}
}

// newPartitionSyncSyncer builds the two-gateway world: shared-gw on the class
// tunnel, infra-gw with a dedicated data plane on the token's tunnel.
func newPartitionSyncSyncer(t *testing.T, api *recordingTunnelAPI, classTunnelID string) *RouteSyncer {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, gatewayv1.Install(scheme))
	require.NoError(t, v1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))

	objects := []runtime.Object{
		&gatewayv1.GatewayClass{
			ObjectMeta: metav1.ObjectMeta{Name: "cf-test"},
			Spec: gatewayv1.GatewayClassSpec{
				ControllerName: skipTestControllerName,
				ParametersRef: &gatewayv1.ParametersReference{
					Group: config.ParametersRefGroup, Kind: config.ParametersRefKind, Name: "cfg",
				},
			},
		},
		&v1alpha1.GatewayClassConfig{
			ObjectMeta: metav1.ObjectMeta{Name: "cfg"},
			Spec: v1alpha1.GatewayClassConfigSpec{
				CloudflareCredentialsSecretRef: v1alpha1.SecretReference{Name: "creds", Namespace: "default"},
				AccountID:                      "test-account",
				TunnelID:                       classTunnelID,
			},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "creds", Namespace: "default"},
			Data:       map[string][]byte{"api-token": []byte("test-token")},
		},
		&gatewayv1.Gateway{
			ObjectMeta: metav1.ObjectMeta{Name: "shared-gw", Namespace: "default"},
			Spec: gatewayv1.GatewaySpec{
				GatewayClassName: "cf-test",
				Listeners:        httpListener(),
			},
		},
		&gatewayv1.Gateway{
			ObjectMeta: metav1.ObjectMeta{Name: "infra-gw", Namespace: "default"},
			Spec: gatewayv1.GatewaySpec{
				GatewayClassName: "cf-test",
				Listeners:        httpListener(),
				Infrastructure: &gatewayv1.GatewayInfrastructure{
					ParametersRef: &gatewayv1.LocalParametersReference{
						Group: "cf.k8s.lex.la", Kind: "GatewayConfig", Name: "infra-config",
					},
				},
			},
		},
		&v1alpha1.GatewayConfig{
			ObjectMeta: metav1.ObjectMeta{Name: "infra-config", Namespace: "default"},
			Spec: v1alpha1.GatewayConfigSpec{
				TunnelTokenSecretRef: v1alpha1.LocalSecretReference{Name: "infra-token"},
			},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "infra-token", Namespace: "default"},
			Data:       map[string][]byte{"tunnel-token": []byte(partitionSyncToken(t))},
		},
		// The generated config-API auth Secret the infra reconciler would
		// create (no explicit authTokenSecretRef on the GatewayConfig).
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "cf-proxy-infra-gw-auth", Namespace: "default"},
			Data:       map[string][]byte{"auth-token": []byte("generated-bearer")},
		},
		&corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "default"},
			Spec: corev1.ServiceSpec{
				Ports: []corev1.ServicePort{{Port: 80}},
			},
		},
		partitionSyncRoute("shared-route", "shared-gw", "shared.example.com"),
		partitionSyncRoute("tenant-route", "infra-gw", "tenant.example.com"),
	}

	builder := fake.NewClientBuilder().WithScheme(scheme)
	for _, obj := range objects {
		builder = builder.WithRuntimeObjects(obj)
	}

	fakeClient := builder.Build()

	resolver := config.NewResolver(fakeClient, "default", cfmetrics.NewNoopCollector())
	syncer := NewRouteSyncer(fakeClient, scheme, "cluster.local", skipTestControllerName, resolver, cfmetrics.NewNoopCollector(), nil)
	syncer.cloudflareClientFactory = func(_ *config.ResolvedConfig) *cloudflare.Client {
		return cloudflare.NewClient(
			option.WithAPIToken("test-token"),
			option.WithBaseURL(api.server.URL),
		)
	}

	return syncer
}

// TestSyncAllRoutes_PartitionsByTunnel pins the core isolation contract of
// the Cloudflare sync: each tunnel document carries EXACTLY its partition's
// hostnames — the tenant hostname never appears in the shared tunnel and
// vice versa.
func TestSyncAllRoutes_PartitionsByTunnel(t *testing.T) {
	t.Parallel()

	api := newRecordingTunnelAPI(t)
	syncer := newPartitionSyncSyncer(t, api, "99999999-9999-4999-8999-999999999999")

	_, result, err := syncer.SyncAllRoutes(context.Background())
	require.NoError(t, err)
	require.NotNil(t, result)

	assert.Equal(t, 2, api.tunnelsWritten(), "two tunnels, two documents")

	sharedHosts := api.hostnamesFor("99999999-9999-4999-8999-999999999999")
	assert.Contains(t, sharedHosts, "shared.example.com")
	assert.NotContains(t, sharedHosts, "tenant.example.com",
		"tenant hostname leaked into the shared tunnel document")

	tenantHosts := api.hostnamesFor(tenantTunnelUUID)
	assert.Contains(t, tenantHosts, "tenant.example.com")
	assert.NotContains(t, tenantHosts, "shared.example.com",
		"shared hostname leaked into the tenant tunnel document")

	require.Len(t, result.Partitions, 2, "SyncResult must carry the partition split for the proxy push")
}

// TestSyncAllRoutes_DistinctTunnelWriteFailureIsolated pins per-tunnel failure
// isolation end-to-end: when ONE of two distinct tunnels fails its Cloudflare
// write, the healthy tunnel's document is still written and ITS route stays
// unflagged, while only the failed tunnel's route carries a per-parent sync
// error. A single tunnel outage must not flip a sibling tenant's route.
func TestSyncAllRoutes_DistinctTunnelWriteFailureIsolated(t *testing.T) {
	t.Parallel()

	api := newRecordingTunnelAPI(t)
	const sharedTunnel = "99999999-9999-4999-8999-999999999999"

	syncer := newPartitionSyncSyncer(t, api, sharedTunnel)
	api.failTunnel(tenantTunnelUUID) // the tenant (infra-gw) tunnel write fails

	_, result, err := syncer.SyncAllRoutes(context.Background())
	require.NoError(t, err, "a single tunnel's write failure must NOT become a global sync error")
	require.NotNil(t, result)

	// The healthy shared tunnel was still written.
	assert.Contains(t, api.hostnamesFor(sharedTunnel), "shared.example.com",
		"the healthy tunnel's document must be written despite the other tunnel failing")

	// The tenant route's parent carries a sync error; the shared route's does not.
	tenantBinding, ok := result.HTTPRouteBindings["default/tenant-route"]
	require.True(t, ok)
	assert.NotEmpty(t, tenantBinding.syncErrByGateway, "the failed tunnel's route must carry a per-parent sync error")

	sharedBinding, ok := result.HTTPRouteBindings["default/shared-route"]
	require.True(t, ok)
	assert.Empty(t, sharedBinding.syncErrByGateway,
		"the healthy tunnel's route must stay unflagged when a sibling tunnel fails")
}

// TestSyncAllRoutes_SameTunnelPartitionsMerge pins the same-tunnel grouping:
// when a dedicated Gateway's token points at the SAME tunnel as the class
// config (e.g. e2e reusing the CI tunnel), both partitions merge into ONE
// document write instead of last-writer-wins.
func TestSyncAllRoutes_SameTunnelPartitionsMerge(t *testing.T) {
	t.Parallel()

	api := newRecordingTunnelAPI(t)
	syncer := newPartitionSyncSyncer(t, api, tenantTunnelUUID)

	_, result, err := syncer.SyncAllRoutes(context.Background())
	require.NoError(t, err)
	require.NotNil(t, result)

	assert.Equal(t, 1, api.tunnelsWritten(), "same tunnel → one merged document")

	hosts := api.hostnamesFor(tenantTunnelUUID)
	assert.Contains(t, hosts, "shared.example.com")
	assert.Contains(t, hosts, "tenant.example.com")
}

// TestSyncAllRoutes_BrokenGatewayConfigFailsClosed pins the isolation
// fail-mode: when an opted-in Gateway's GatewayConfig cannot resolve (here:
// its token Secret is gone), its routes must NOT fall back into the shared
// tunnel document — fail closed, surfaced on the Gateway status, never
// silently served by another tenant's plane.
func TestSyncAllRoutes_BrokenGatewayConfigFailsClosed(t *testing.T) {
	t.Parallel()

	api := newRecordingTunnelAPI(t)
	syncer := newPartitionSyncSyncer(t, api, "99999999-9999-4999-8999-999999999999")

	// Break the per-Gateway config: delete the token Secret.
	require.NoError(t, syncer.Delete(context.Background(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "infra-token", Namespace: "default"},
	}))

	_, result, err := syncer.SyncAllRoutes(context.Background())
	require.NoError(t, err)
	require.NotNil(t, result)

	sharedHosts := api.hostnamesFor("99999999-9999-4999-8999-999999999999")
	assert.Contains(t, sharedHosts, "shared.example.com")
	assert.NotContains(t, sharedHosts, "tenant.example.com",
		"a broken GatewayConfig must fail closed, not leak the tenant hostname into the shared tunnel")

	assert.Empty(t, api.hostnamesFor(tenantTunnelUUID),
		"no document may be written for the unresolvable tunnel")

	// The route bound only to the broken Gateway is served nowhere, so its
	// status MUST NOT claim Accepted=True: the binding carries a per-parent
	// sync error for the broken Gateway, which the status writer turns into
	// Accepted=False (a route reporting health it does not have is a
	// black-hole).
	binding := result.HTTPRouteBindings["default/tenant-route"]
	require.NotNil(t, binding.syncErrByGateway)
	assert.Error(t, binding.syncErrByGateway["default/infra-gw"],
		"a route on a broken data plane must carry a per-parent sync error so it is not reported Accepted=True")
}
