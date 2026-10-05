package controller

// Pins the per-tunnel Cloudflare sync split (#479): routes bound to a Gateway
// with a dedicated data plane go to THAT Gateway's tunnel document; shared
// routes go to the class tunnel; partitions resolving to the SAME tunnel are
// merged into one document write (no last-writer-wins).

import (
	"bytes"
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/cloudflare/cloudflare-go/v7"
	"github.com/cloudflare/cloudflare-go/v7/option"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/api/v1alpha1"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/cfmetrics"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/config"
)

const tenantTunnelUUID = "550e8400-e29b-41d4-a716-446655440000"

// recordingTunnelAPI records each PUT's tunnel ID and the hostnames it carried,
// and serves GET from the last document PUT to that tunnel (catch-all only
// before the first write), so consecutive syncs see what the previous one left.
type recordingTunnelAPI struct {
	server *httptest.Server

	mu           sync.Mutex
	puts         map[string][]string         // tunnelID -> hostnames in the written document
	docs         map[string][]map[string]any // tunnelID -> ingress rules last written
	gets         map[string]int              // tunnelID -> GETs answered successfully
	failTunnelID string                      // PUTs to this tunnel ID fail
	failStatus   int                         // the failing PUT's status; 0 means 500
	failGetID    string                      // GETs of this tunnel ID fail
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

	api := &recordingTunnelAPI{
		puts: make(map[string][]string), docs: make(map[string][]map[string]any), gets: make(map[string]int),
	}

	api.server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		writer.Header().Set("Content-Type", "application/json")

		// Path: /accounts/<acct>/cfd_tunnel/<tunnelID>/configurations
		segments := strings.Split(strings.Trim(req.URL.Path, "/"), "/")
		tunnelID := segments[len(segments)-2]

		switch req.Method {
		case http.MethodGet:
			api.mu.Lock()
			rules, ok := api.docs[tunnelID]
			failGet := api.failGetID == tunnelID

			if !failGet {
				api.gets[tunnelID]++
			}
			api.mu.Unlock()

			if failGet {
				writer.WriteHeader(http.StatusInternalServerError)
				_ = json.NewEncoder(writer).Encode(map[string]any{
					"success": false,
					"errors":  []any{map[string]any{"code": 1000, "message": "simulated tunnel read failure"}},
				})

				return
			}

			if !ok {
				rules = []map[string]any{{"service": "http_status:404"}}
			}

			_ = json.NewEncoder(writer).Encode(map[string]any{
				"success": true, "errors": []any{},
				"result": map[string]any{"config": map[string]any{"ingress": rules}},
			})
		case http.MethodPut:
			if api.shouldFail(tunnelID) {
				api.mu.Lock()
				status := cmp.Or(api.failStatus, http.StatusInternalServerError)
				api.mu.Unlock()

				writer.WriteHeader(status)
				_ = json.NewEncoder(writer).Encode(map[string]any{
					"success": false,
					"errors":  []any{map[string]any{"code": 1000, "message": "simulated tunnel write failure"}},
				})

				return
			}

			var body struct {
				Config struct {
					Ingress []map[string]any `json:"ingress"`
				} `json:"config"`
			}
			_ = json.NewDecoder(req.Body).Decode(&body)

			hostnames := make([]string, 0, len(body.Config.Ingress))

			for _, rule := range body.Config.Ingress {
				if hostname, _ := rule["hostname"].(string); hostname != "" {
					hostnames = append(hostnames, hostname)
				}
			}

			api.mu.Lock()
			api.puts[tunnelID] = hostnames
			api.docs[tunnelID] = body.Config.Ingress
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

// lastIngress returns the ingress rules last written to tunnelID.
func (a *recordingTunnelAPI) lastIngress(t *testing.T, tunnelID string) []map[string]any {
	t.Helper()

	a.mu.Lock()
	defer a.mu.Unlock()

	rules, ok := a.docs[tunnelID]
	require.True(t, ok, "tunnel %s was never written", tunnelID)

	return rules
}

// seed makes GET serve rules for tunnelID, as if an earlier controller had
// written them.
func (a *recordingTunnelAPI) seed(tunnelID string, rules []map[string]any) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.docs[tunnelID] = rules
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

// newPartitionSyncSyncer builds the two-gateway world — shared-gw on the class
// tunnel, infra-gw with a dedicated data plane on the token's tunnel — with a
// class config that refuses shared tunnels, the shipped default.
func newPartitionSyncSyncer(t *testing.T, api *recordingTunnelAPI, classTunnelID string, interceptors ...interceptor.Funcs) *RouteSyncer {
	t.Helper()

	return partitionSyncSyncer(t, api, classTunnelID, false, interceptors...)
}

// newSharingPartitionSyncSyncer is the same with sharing opted in, for the
// tests that still need two partitions to merge onto one tunnel.
func newSharingPartitionSyncSyncer(t *testing.T, api *recordingTunnelAPI, classTunnelID string, interceptors ...interceptor.Funcs) *RouteSyncer {
	t.Helper()

	return partitionSyncSyncer(t, api, classTunnelID, true, interceptors...)
}

func partitionSyncSyncer(t *testing.T, api *recordingTunnelAPI, classTunnelID string, allowShared bool, interceptors ...interceptor.Funcs) *RouteSyncer {
	t.Helper()

	return partitionSyncSyncerFor(t, api, partitionSyncObjects(t, classTunnelID, allowShared), interceptors...)
}

// partitionSyncObjects is the shared fixture set: a managed class, the shared
// Gateway, one opted-in Gateway with its own tunnel, and a route on each.
func partitionSyncObjects(t *testing.T, classTunnelID string, allowShared bool) []runtime.Object {
	t.Helper()

	return []runtime.Object{
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
				AllowSharedTunnels:             allowShared,
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
}

// partitionSyncSyncerFor builds a RouteSyncer over an explicit object set.
func partitionSyncSyncerFor(t *testing.T, api *recordingTunnelAPI, objects []runtime.Object, interceptors ...interceptor.Funcs) *RouteSyncer {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, gatewayv1.Install(scheme))
	require.NoError(t, v1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, gatewayv1beta1.Install(scheme))

	builder := fake.NewClientBuilder().WithScheme(scheme)
	for _, obj := range objects {
		builder = builder.WithRuntimeObjects(obj)
	}

	if len(interceptors) > 0 {
		builder = builder.WithInterceptorFuncs(interceptors[0])
	}

	fakeClient := builder.Build()

	resolver := config.NewResolver(fakeClient, "default", cfmetrics.NewNoopCollector(), verifiedClaims())
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

// TestSyncAllRoutes_TunnelWriteFailureLeavesRouteStatus pins that a failed
// Cloudflare write never reaches route status. The edge routes a hostname by
// its DNS record and the proxy serves it, so the document only feeds the
// dashboard: a route whose tunnel write failed is still served. The healthy
// tunnel is still written, and the sync is retried.
func TestSyncAllRoutes_TunnelWriteFailureLeavesRouteStatus(t *testing.T) {
	t.Parallel()

	api := newRecordingTunnelAPI(t)
	const sharedTunnel = "99999999-9999-4999-8999-999999999999"

	syncer := newPartitionSyncSyncer(t, api, sharedTunnel)
	api.failTunnel(tenantTunnelUUID) // the tenant (infra-gw) tunnel write fails

	ctrlResult, result, err := syncer.SyncAllRoutes(context.Background())
	require.NoError(t, err, "a tunnel write failure must not become a sync error")
	require.NotNil(t, result)
	assert.Positive(t, ctrlResult.RequeueAfter, "the failed write is retried")

	assert.Contains(t, api.hostnamesFor(sharedTunnel), "shared.example.com",
		"the healthy tunnel's document must be written despite the other tunnel failing")

	for _, key := range []string{"default/tenant-route", "default/shared-route"} {
		binding, ok := result.HTTPRouteBindings[key]
		require.True(t, ok)
		assert.Empty(t, binding.syncErrByGateway, "%s must not carry a sync error for a document write", key)
	}
}

// TestSyncAllRoutes_ClassTunnelClaimIsRejectedNotMerged pins the tunnel-
// ownership rule at the sync layer. A dedicated Gateway whose token names the
// CLASS tunnel used to merge into that tunnel's document, which handed the
// dedicated plane every shared route (and their backend-mTLS keys, since the
// union is pushed to the partition's own endpoints). The claim is now refused:
// the class document carries only the shared routes, and the claimant
// contributes nothing anywhere.
func TestSyncAllRoutes_ClassTunnelClaimIsRejectedNotMerged(t *testing.T) {
	t.Parallel()

	api := newRecordingTunnelAPI(t)
	syncer := newPartitionSyncSyncer(t, api, tenantTunnelUUID)

	_, result, err := syncer.SyncAllRoutes(context.Background())
	require.NoError(t, err, "a refused claim is a status outcome, not a sync error")
	require.NotNil(t, result)

	assert.Equal(t, 1, api.tunnelsWritten(), "only the class tunnel is written")

	hosts := api.hostnamesFor(tenantTunnelUUID)
	assert.Contains(t, hosts, "shared.example.com",
		"the class tunnel keeps serving the routes that legitimately belong to it")
	assert.NotContains(t, hosts, "tenant.example.com",
		"the refused Gateway must not get its routes into the class tunnel's document")
}

// TestSyncAllRoutes_SharingOptInMergesBothPartitions pins the escape hatch that
// keeps an existing shared-tunnel install working across the upgrade: with
// AllowSharedTunnels the dedicated Gateway's claim on the class tunnel is
// honoured instead of refused, and BOTH parties' hostnames reach the edge in the
// one document that tunnel gets.
func TestSyncAllRoutes_SharingOptInMergesBothPartitions(t *testing.T) {
	t.Parallel()

	api := newRecordingTunnelAPI(t)
	syncer := newSharingPartitionSyncSyncer(t, api, tenantTunnelUUID)

	_, result, err := syncer.SyncAllRoutes(context.Background())
	require.NoError(t, err)
	require.NotNil(t, result)

	assert.Equal(t, 1, api.tunnelsWritten(),
		"both partitions name one tunnel, so exactly one document is written")

	hosts := api.hostnamesFor(tenantTunnelUUID)
	assert.Contains(t, hosts, "shared.example.com",
		"the class tunnel keeps serving its own routes")
	assert.Contains(t, hosts, "tenant.example.com",
		"the opted-in dedicated Gateway's routes must reach the tunnel it shares")
}

// TestSyncAllRoutes_TransientInfraResolveRequeues pins the A4 contract: when an
// opted-in Gateway's config resolve fails TRANSIENTLY (a retryable apiserver
// error, not a deterministic ErrInvalidParameters), the Gateway is flagged
// transient-broken and the sync requeues so the next pass re-resolves — rather
// than failing closed and waiting for an unrelated event.
func TestSyncAllRoutes_TransientInfraResolveRequeues(t *testing.T) {
	t.Parallel()

	api := newRecordingTunnelAPI(t)
	syncer := newPartitionSyncSyncer(t, api, tenantTunnelUUID, interceptor.Funcs{
		Get: func(ctx context.Context, cli client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			// A non-NotFound error on the infra Gateway's token Secret read is
			// retryable, so ResolveForGateway returns a transient (not
			// ErrInvalidParameters) error.
			if key.Name == "infra-token" {
				return apierrors.NewInternalError(assert.AnError)
			}

			return cli.Get(ctx, key, obj, opts...)
		},
	})

	result, syncResult, err := syncer.SyncAllRoutes(context.Background())
	require.NoError(t, err, "a transient resolve failure must not become a global sync error")
	require.NotNil(t, syncResult)

	assert.Contains(t, syncResult.TransientBrokenKeys, "default/infra-gw",
		"a retryable resolve failure must be classified transient")
	assert.Positive(t, result.RequeueAfter,
		"a transient infra-resolve failure must requeue to re-resolve")
}

// TestSyncAllRoutes_ClassTunnelClaimRejectionIsCanonicalFormInsensitive pins that
// the ownership check keys on the CANONICAL tunnel UUID, not the raw string.
// The shared plane carries the GatewayClassConfig's raw tunnelID while a
// per-Gateway plane carries the UUID parsed from its connector token (always
// canonical lowercase). Spelling the class tunnelID in a different-but-equivalent
// form must not make the two look like distinct tunnels: that would read as an
// uncontested claim and let the dedicated plane onto the class tunnel after all.
func TestSyncAllRoutes_ClassTunnelClaimRejectionIsCanonicalFormInsensitive(t *testing.T) {
	t.Parallel()

	api := newRecordingTunnelAPI(t)
	syncer := newPartitionSyncSyncer(t, api, strings.ToUpper(tenantTunnelUUID))

	_, result, err := syncer.SyncAllRoutes(context.Background())
	require.NoError(t, err)
	require.NotNil(t, result)

	assert.Equal(t, 1, api.tunnelsWritten(),
		"a non-canonical class tunnelID still names the same physical tunnel")

	hosts := api.hostnamesFor(tenantTunnelUUID)
	assert.Contains(t, hosts, "shared.example.com")
	assert.NotContains(t, hosts, "tenant.example.com",
		"spelling the same UUID in another case must not evade the ownership check")
}

// TestSyncAllRoutes_EveryTunnelWriteFailingIsRetriedNotFatal covers the
// write failing on the only tunnel there is, here the one the shared and a
// dedicated Gateway merge onto: the routes are still served, so the sync
// returns no error and asks to be run again.
func TestSyncAllRoutes_EveryTunnelWriteFailingIsRetriedNotFatal(t *testing.T) {
	t.Parallel()

	api := newRecordingTunnelAPI(t)
	syncer := newSharingPartitionSyncSyncer(t, api, tenantTunnelUUID)
	api.failTunnel(tenantTunnelUUID)

	ctrlResult, result, err := syncer.SyncAllRoutes(context.Background())
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Positive(t, ctrlResult.RequeueAfter)

	for key, binding := range result.HTTPRouteBindings {
		assert.Empty(t, binding.syncErrByGateway, "%s must not carry a sync error for a document write", key)
	}
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

// cappedPartitionSyncObjects returns the shared fixture with the class cap set
// and an OLDER opted-in Gateway added in the same namespace, so "infra-gw" is
// the one the cap refuses.
func cappedPartitionSyncObjects(t *testing.T, classTunnelID string, capacity int32) []runtime.Object {
	t.Helper()

	elder := metav1.NewTime(time.Date(2020, time.January, 1, 0, 0, 0, 0, time.UTC))

	objects := partitionSyncObjects(t, classTunnelID, false)
	for _, obj := range objects {
		switch typed := obj.(type) {
		case *v1alpha1.GatewayClassConfig:
			typed.Spec.MaxDataPlanesPerNamespace = &capacity
		case *gatewayv1.Gateway:
			if typed.Name == "infra-gw" {
				typed.CreationTimestamp = metav1.NewTime(elder.AddDate(1, 0, 0))
			}
		}
	}

	return append(objects,
		&gatewayv1.Gateway{
			ObjectMeta: metav1.ObjectMeta{
				Name: "elder-gw", Namespace: "default", UID: "elder-gw", CreationTimestamp: elder,
			},
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
		// The elder MUST resolve, so its partition survives and the refused
		// Gateway's absence can only be the cap. Without this Secret it lands in
		// the broken set for an unrelated reason and the test passes vacuously.
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "cf-proxy-elder-gw-auth", Namespace: "default"},
			Data:       map[string][]byte{"auth-token": []byte("generated-bearer")},
		},
	)
}

// TestSyncAllRoutes_SharedTunnelsDoNotWaiveTheQuota pins the same ordering at
// the route-sync layer. applyTunnelOwnership returns early on the sharing
// opt-out; applyDataPlaneQuota is correct only because it sits OUTSIDE that
// call. Folding it in would leave a tenant's routes programmed on a plane the
// infra reconciler refuses to render.
func TestSyncAllRoutes_SharedTunnelsDoNotWaiveTheQuota(t *testing.T) {
	t.Parallel()

	const classTunnel = "99999999-9999-4999-8999-999999999999"

	objects := cappedPartitionSyncObjects(t, classTunnel, 1)
	for _, obj := range objects {
		if classConfig, ok := obj.(*v1alpha1.GatewayClassConfig); ok {
			classConfig.Spec.AllowSharedTunnels = true
		}
	}

	api := newRecordingTunnelAPI(t)

	_, result, err := partitionSyncSyncerFor(t, api, objects).SyncAllRoutes(context.Background())
	require.NoError(t, err)
	require.NotNil(t, result)

	keys := make([]string, 0, len(result.Partitions))
	for i := range result.Partitions {
		keys = append(keys, result.Partitions[i].Key)
	}

	assert.NotContains(t, keys, "default/infra-gw",
		"sharing tunnels must not waive the data-plane cap")
}

// TestSyncAllRoutes_ZeroCapRefusesEveryDedicatedPartition pins the route-sync
// half of the same rule. A cap of 0 reaches the sync only from a CRD predating
// Minimum=1 or a hand-edited one, and reading it as unlimited would program
// every dedicated plane the operator asked to have none of.
func TestSyncAllRoutes_ZeroCapRefusesEveryDedicatedPartition(t *testing.T) {
	t.Parallel()

	const classTunnel = "99999999-9999-4999-8999-999999999999"

	syncer := partitionSyncSyncerFor(t, newRecordingTunnelAPI(t), cappedPartitionSyncObjects(t, classTunnel, 0))

	_, result, err := syncer.SyncAllRoutes(context.Background())
	require.NoError(t, err)
	require.NotNil(t, result)

	keys := make([]string, 0, len(result.Partitions))
	for i := range result.Partitions {
		keys = append(keys, result.Partitions[i].Key)
	}

	assert.NotContains(t, keys, "default/infra-gw")
	assert.NotContains(t, keys, "default/elder-gw", "a zero cap leaves no dedicated plane standing")
	assert.Contains(t, keys, sharedPartitionKey, "the shared plane is not a dedicated one; the cap does not touch it")
}

// TestSyncAllRoutes_OverQuotaGatewayGetsNoPartition pins that the cap reaches
// the route sync, not only the Gateway status: a Gateway past its namespace's
// limit contributes no partition, and its hostname is written to NO tunnel
// document — in particular not to the shared one, which every other tenant
// reads.
func TestSyncAllRoutes_OverQuotaGatewayGetsNoPartition(t *testing.T) {
	t.Parallel()

	const classTunnel = "99999999-9999-4999-8999-999999999999"

	api := newRecordingTunnelAPI(t)
	syncer := partitionSyncSyncerFor(t, api, cappedPartitionSyncObjects(t, classTunnel, 1))

	_, result, err := syncer.SyncAllRoutes(context.Background())
	require.NoError(t, err)
	require.NotNil(t, result)

	keys := make([]string, 0, len(result.Partitions))
	for i := range result.Partitions {
		keys = append(keys, result.Partitions[i].Key)
	}

	assert.NotContains(t, keys, "default/infra-gw", "the Gateway over the cap contributes no partition")
	assert.Contains(t, keys, "default/elder-gw", "the Gateway within the cap keeps its own")
	assert.Contains(t, keys, sharedPartitionKey)

	sharedHosts := api.hostnamesFor(classTunnel)
	assert.Contains(t, sharedHosts, "shared.example.com")
	assert.NotContains(t, sharedHosts, "tenant.example.com",
		"a refused Gateway's routes must fail closed, not fall back to the shared plane")

	assert.NotContains(t, api.hostnamesFor(tenantTunnelUUID), "tenant.example.com",
		"no document may be written for a data plane that is never rendered")
}

var errLongCloudflareBody = errors.New("simulated long Cloudflare error body")

// regardingRecorder records which object each Event was about.
type regardingRecorder struct {
	mu     sync.Mutex
	events []string
	notes  []string
}

func (r *regardingRecorder) Eventf(regarding, _ runtime.Object, eventtype, reason, _, note string, args ...any) {
	object, ok := regarding.(client.Object)
	if !ok {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	r.notes = append(r.notes, fmt.Sprintf(note, args...))

	r.events = append(r.events, fmt.Sprintf("%s %s %T %s/%s",
		eventtype, reason, regarding, object.GetNamespace(), object.GetName()))
}

func (r *regardingRecorder) recorded() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	return slices.Clone(r.events)
}

// TestSyncAllRoutes_TunnelWriteFailureEmitsEvent pins where a failed document
// write is reported now that route status no longer carries it: on every
// Gateway served from the failed tunnel, never on routes.
func TestSyncAllRoutes_TunnelWriteFailureEmitsEvent(t *testing.T) {
	t.Parallel()

	const sharedTunnel = "99999999-9999-4999-8999-999999999999"

	tests := []struct {
		name   string
		failed string
		want   string
	}{
		{
			name:   "class tunnel",
			failed: sharedTunnel,
			want:   "Warning TunnelDocumentWriteFailed *v1.Gateway default/shared-gw",
		},
		{
			name:   "dedicated tunnel",
			failed: tenantTunnelUUID,
			want:   "Warning TunnelDocumentWriteFailed *v1.Gateway default/infra-gw",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			api := newRecordingTunnelAPI(t)
			syncer := newPartitionSyncSyncer(t, api, sharedTunnel)
			recorder := &regardingRecorder{}
			syncer.Recorder = recorder
			api.failTunnel(tt.failed)

			_, _, err := syncer.SyncAllRoutes(context.Background())
			require.NoError(t, err)

			assert.Equal(t, []string{tt.want}, recorder.recorded())
		})
	}
}

// TestReportDocumentWriteFailure_NoteFitsTheAPILimit pins the note cut: the
// API server rejects an events.k8s.io note over 1024 bytes, and a Cloudflare
// error body can be longer than that.
func TestReportDocumentWriteFailure_NoteFitsTheAPILimit(t *testing.T) {
	t.Parallel()

	recorder := &regardingRecorder{}
	syncer := &RouteSyncer{Recorder: recorder}
	group := &tunnelGroup{
		resolved:   &config.ResolvedConfig{TunnelID: tenantTunnelUUID},
		partitions: []*routePartition{{Gateway: &gatewayv1.Gateway{ObjectMeta: metav1.ObjectMeta{Name: "gw"}}}},
	}

	syncer.reportDocumentWriteFailure(context.Background(), slog.Default(), group,
		fmt.Errorf("%w: %s", errLongCloudflareBody, strings.Repeat("é", 5000)))

	require.Len(t, recorder.notes, 1)
	assert.LessOrEqual(t, len(recorder.notes[0]), 1024)
	assert.True(t, utf8.ValidString(recorder.notes[0]), "the cut must not split a rune")
}

// TestSyncAllRoutes_PersistentWriteFailureLogsErrorOnce pins that a write
// failing the same way on every retry, a bad token or an API outage, is
// logged at error level once rather than every retry.
func TestSyncAllRoutes_PersistentWriteFailureLogsErrorOnce(t *testing.T) {
	t.Parallel()

	api := newRecordingTunnelAPI(t)
	syncer := newPartitionSyncSyncer(t, api, "99999999-9999-4999-8999-999999999999")
	api.failTunnel(tenantTunnelUUID)

	var logged bytes.Buffer

	syncer.Logger = slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug}))

	for range 3 {
		_, _, err := syncer.SyncAllRoutes(context.Background())
		require.NoError(t, err)
	}

	errorLines := 0

	for line := range strings.Lines(logged.String()) {
		if strings.Contains(line, "level=ERROR") && strings.Contains(line, "writing the tunnel ingress document failed") {
			errorLines++
		}
	}

	assert.Equal(t, 1, errorLines)
}
