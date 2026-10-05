package controller

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/cfmetrics"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/config"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
)

// configRecorder is a replica config API that keeps every pushed config.
type configRecorder struct {
	mu      sync.Mutex
	configs []proxy.Config
}

func newConfigRecorder(t *testing.T) (*configRecorder, string) {
	t.Helper()

	recorder := &configRecorder{}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPut {
			var cfg proxy.Config
			if err := json.NewDecoder(req.Body).Decode(&cfg); err != nil {
				writer.WriteHeader(http.StatusBadRequest)

				return
			}

			recorder.mu.Lock()
			recorder.configs = append(recorder.configs, cfg)
			recorder.mu.Unlock()
		}

		writer.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	return recorder, server.URL + "/config"
}

func (r *configRecorder) pushed() []proxy.Config {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]proxy.Config(nil), r.configs...)
}

func tlsBackendRoute(namespace, hostname string) *gatewayv1.HTTPRoute {
	return &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "secure", Namespace: namespace},
		Spec: gatewayv1.HTTPRouteSpec{
			Hostnames: []gatewayv1.Hostname{gatewayv1.Hostname(hostname)},
			Rules: []gatewayv1.HTTPRouteRule{{
				BackendRefs: []gatewayv1.HTTPBackendRef{{BackendRef: gatewayv1.BackendRef{
					BackendObjectReference: gatewayv1.BackendObjectReference{
						Name: "svc", Port: new(gatewayv1.PortNumber(443)),
					},
				}}},
			}},
		},
	}
}

// listFailingClient serves a BackendTLSPolicy for ns/svc and fails every
// BackendTLSPolicy List in namespace ns while failing is set.
func listFailingClient(t *testing.T, failing *atomic.Bool) client.Client {
	t.Helper()

	return fake.NewClientBuilder().
		WithScheme(newBackendTLSPolicyScheme(t)).
		WithObjects(
			backendTLSPolicyFor("ns", "p", "svc", "cm", time.Time{}),
			caConfigMap("ns", "cm", generateSelfSignedCAPEM(t)),
		).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				listOpts := (&client.ListOptions{}).ApplyOptions(opts)
				if _, ok := list.(*gatewayv1.BackendTLSPolicyList); ok && failing.Load() && listOpts.Namespace == "ns" {
					return errSimulatedCacheMiss
				}

				return c.List(ctx, list, opts...)
			},
		}).
		Build()
}

func requireTLSBackend(t *testing.T, cfg *proxy.Config) {
	t.Helper()

	require.Len(t, cfg.Rules, 1)
	require.Len(t, cfg.Rules[0].Backends, 1)

	backend := cfg.Rules[0].Backends[0]
	require.NotNil(t, backend.TLS, "the policy's backend must be dialed over TLS")
	assert.NotEmpty(t, backend.TLS.CABundlePEM)
	assert.Equal(t, "test.example.com", backend.TLS.ServerName)
}

// TestProxySyncer_UnlistableBackendTLSPolicyHoldsBackPush pins that a config
// built while BackendTLSPolicies cannot be listed never reaches a replica: the
// replica keeps the config it accepted last, and the next sync after the List
// recovers pushes the change with the policy's TLS settings.
func TestProxySyncer_UnlistableBackendTLSPolicyHoldsBackPush(t *testing.T) {
	t.Parallel()

	var failing atomic.Bool

	recorder, endpoint := newConfigRecorder(t)
	syncer := NewProxySyncer("cluster.local", "token", "", listFailingClient(t, &failing), slog.Default())
	ctx := context.Background()

	_, err := syncer.SyncRoutes(ctx, 0, []string{endpoint},
		[]*gatewayv1.HTTPRoute{tlsBackendRoute("ns", "v1.example.com")}, nil, nil, nil)
	require.NoError(t, err)
	require.Len(t, recorder.pushed(), 1)
	requireTLSBackend(t, &recorder.pushed()[0])

	failing.Store(true)

	_, err = syncer.SyncRoutes(ctx, 0, []string{endpoint},
		[]*gatewayv1.HTTPRoute{tlsBackendRoute("ns", "v2.example.com")}, nil, nil, nil)
	require.ErrorIs(t, err, errBackendTLSUnresolved)
	assert.Len(t, recorder.pushed(), 1, "a config built without the policy must not be pushed")

	failing.Store(false)

	_, err = syncer.SyncRoutes(ctx, 0, []string{endpoint},
		[]*gatewayv1.HTTPRoute{tlsBackendRoute("ns", "v2.example.com")}, nil, nil, nil)
	require.NoError(t, err)

	pushed := recorder.pushed()
	require.Len(t, pushed, 2)
	assert.Equal(t, []string{"v2.example.com"}, pushed[1].Rules[0].Hostnames)
	requireTLSBackend(t, &pushed[1])
}

// TestProxySyncer_UnlistableBackendTLSPolicyLeavesNothingToReplay pins the
// cold path: a partition whose first build cannot list policies has no config
// for a joining replica to be given.
func TestProxySyncer_UnlistableBackendTLSPolicyLeavesNothingToReplay(t *testing.T) {
	t.Parallel()

	var failing atomic.Bool

	failing.Store(true)

	recorder, endpoint := newConfigRecorder(t)
	syncer := NewProxySyncer("cluster.local", "token", "", listFailingClient(t, &failing), slog.Default())
	ctx := context.Background()

	_, err := syncer.SyncRoutes(ctx, 0, []string{endpoint},
		[]*gatewayv1.HTTPRoute{tlsBackendRoute("ns", "v1.example.com")}, nil, nil, nil)
	require.ErrorIs(t, err, errBackendTLSUnresolved)

	pushedBuilt, err := syncer.pushBuiltConfig(ctx, sharedPartitionKey)
	require.NoError(t, err)
	assert.False(t, pushedBuilt)
	require.NoError(t, syncer.ResyncEndpointsForTest(ctx, []string{endpoint}))
	assert.Empty(t, recorder.pushed())
}

// TestRecordingTLSResolver_FailedLookupFailsClosed pins the converter-facing
// result of a failed lookup: an unenforceable TLS config, never nil, with the
// error recorded on the build's collector.
func TestRecordingTLSResolver_FailedLookupFailsClosed(t *testing.T) {
	t.Parallel()

	ctx, readErrs := withBuildReadErrors(context.Background())
	resolver := recordingTLSResolver(func(context.Context, string, string, int32, bool) (*proxy.BackendTLSConfig, error) {
		return nil, errBackendTLSUnresolved
	})

	got := resolver(ctx, "ns", "svc", 443, true)
	require.NotNil(t, got)
	assert.Empty(t, got.CABundlePEM)
	assert.NotEmpty(t, got.Unenforceable)
	require.ErrorIs(t, readErrs.err(), errBackendTLSUnresolved)
}

// TestRecordingProtocolResolver_FailedLookupFailsClosed pins the
// converter-facing result of a failed appProtocol lookup: a TLS appProtocol,
// with the error recorded on the build's collector.
func TestRecordingProtocolResolver_FailedLookupFailsClosed(t *testing.T) {
	t.Parallel()

	ctx, readErrs := withBuildReadErrors(context.Background())
	resolver := recordingProtocolResolver(func(context.Context, string, string, int32) (string, error) {
		return "", errBackendTLSUnresolved
	})

	assert.Equal(t, "https", resolver(ctx, "ns", "svc", 443))
	require.ErrorIs(t, readErrs.err(), errBackendTLSUnresolved)
}

// TestPushPartitionConfigs_HeldBackPushRequestsRequeue pins the retry of a
// held-back push: no further event is guaranteed to arrive once the List
// recovers, so the sync itself must ask to run again. The hold is counted as
// a proxy_push sync error, since it raises no route condition.
func TestPushPartitionConfigs_HeldBackPushRequestsRequeue(t *testing.T) {
	t.Parallel()

	var failing atomic.Bool

	failing.Store(true)

	reg := prometheus.NewRegistry()
	_, endpoint := newConfigRecorder(t)
	params := syncUpdateParams{
		routeSyncer:    &RouteSyncer{ClusterDomain: "cluster.local", Metrics: cfmetrics.NewCollector(reg)},
		proxySyncer:    NewProxySyncer("cluster.local", "", "", listFailingClient(t, &failing), slog.Default()),
		proxyEndpoints: []string{endpoint},
		pushProxy:      true,
	}

	syncResult := &SyncResult{Partitions: []routePartition{{
		Key:        sharedPartitionKey,
		HTTPRoutes: []gatewayv1.HTTPRoute{*tlsBackendRoute("ns", "v1.example.com")},
	}}}

	_, outcome := pushPartitionConfigs(context.Background(), slog.Default(), &params, syncResult)
	assert.True(t, outcome.undecided, "a held-back push must request a requeue")
	assert.InDelta(t, 1, gatheredCounterTotal(t, reg, "cftunnel_sync_errors_total"), 0)
}

// TestPushPartitionConfigs_HeldBackPartitionDoesNotBlockOthers pins that only
// the partition whose policies cannot be read waits: another data plane in
// the same sync still receives its config.
func TestPushPartitionConfigs_HeldBackPartitionDoesNotBlockOthers(t *testing.T) {
	t.Parallel()

	var failing atomic.Bool

	failing.Store(true)

	recorder, endpoint := newConfigRecorder(t)
	params := syncUpdateParams{
		routeSyncer:    &RouteSyncer{ClusterDomain: "cluster.local", Metrics: cfmetrics.NewNoopCollector()},
		proxySyncer:    NewProxySyncer("cluster.local", "", "", listFailingClient(t, &failing), slog.Default()),
		proxyEndpoints: []string{endpoint},
		pushProxy:      true,
	}

	syncResult := &SyncResult{
		SharedTunnelID: "550e8400-e29b-41d4-a716-446655440000",
		Partitions: []routePartition{
			{
				Key:        sharedPartitionKey,
				HTTPRoutes: []gatewayv1.HTTPRoute{*tlsBackendRoute("other", "shared.example.com")},
			},
			{
				Key:     "ns/tenant-gw",
				Gateway: &gatewayv1.Gateway{ObjectMeta: metav1.ObjectMeta{Name: "tenant-gw", Namespace: "ns"}},
				PerGateway: &config.PerGatewayConfig{
					ResolvedConfig: config.ResolvedConfig{TunnelID: "6ba7b810-9dad-11d1-80b4-00c04fd430c8"},
					AuthToken:      "tenant-token",
				},
				HTTPRoutes: []gatewayv1.HTTPRoute{*tlsBackendRoute("ns", "tenant.example.com")},
			},
		},
	}

	_, outcome := pushPartitionConfigs(context.Background(), slog.Default(), &params, syncResult)
	assert.True(t, outcome.undecided)

	pushed := recorder.pushed()
	require.Len(t, pushed, 1, "the shared partition must push while the tenant partition is held back")
	require.Len(t, pushed[0].Rules, 1)
	assert.Equal(t, []string{"shared.example.com"}, pushed[0].Rules[0].Hostnames)
}

// TestBuildProxyConfig_MirrorToUncachedServiceKeepsSectionNamePolicy pins the
// mirror leg: a RequestMirror destination has no not-found check of its own,
// so a sectionName-scoped policy on a Service the cache does not hold yet
// must still put TLS on the mirror dial.
func TestBuildProxyConfig_MirrorToUncachedServiceKeepsSectionNamePolicy(t *testing.T) {
	t.Parallel()

	policy := backendTLSPolicyFor("ns", "p", "mirror-svc", "cm", time.Time{})
	policy.Spec.TargetRefs[0].SectionName = new(gatewayv1.SectionName("https"))

	cli := fake.NewClientBuilder().
		WithScheme(newBackendTLSPolicyScheme(t)).
		WithObjects(policy, caConfigMap("ns", "cm", generateSelfSignedCAPEM(t))).
		Build()
	syncer := NewProxySyncer("cluster.local", "token", "", cli, slog.Default())

	route := tlsBackendRoute("ns", "mirror.example.com")
	route.Spec.Rules[0].Filters = []gatewayv1.HTTPRouteFilter{{
		Type: gatewayv1.HTTPRouteFilterRequestMirror,
		RequestMirror: &gatewayv1.HTTPRequestMirrorFilter{BackendRef: gatewayv1.BackendObjectReference{
			Name: "mirror-svc", Port: new(gatewayv1.PortNumber(443)),
		}},
	}}

	cfg := syncer.buildProxyConfig(context.Background(), []*gatewayv1.HTTPRoute{route}, nil, nil, nil, clientCertParents{})

	require.Len(t, cfg.Rules, 1)

	var mirror *proxy.MirrorConfig

	for i := range cfg.Rules[0].Filters {
		if cfg.Rules[0].Filters[i].RequestMirror != nil {
			mirror = cfg.Rules[0].Filters[i].RequestMirror
		}
	}

	require.NotNil(t, mirror)
	require.NotNil(t, mirror.TLS, "the mirror leg must dial TLS while the Service's port names are unknown")
	assert.True(t, strings.HasPrefix(mirror.BackendURL, "https://"), mirror.BackendURL)
}

// TestProxySyncer_HeldBackSyncMarksResolvedRefsUndecided pins route status
// during a held-back push: the ResolvedRefs diagnostics of a config that was
// never pushed are undecided, so the route keeps its previous verdict.
func TestProxySyncer_HeldBackSyncMarksResolvedRefsUndecided(t *testing.T) {
	t.Parallel()

	var failing atomic.Bool

	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "svc"},
		Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{
			Name: "https", Port: 443, AppProtocol: new("https"),
		}}},
	}

	cli := fake.NewClientBuilder().
		WithScheme(newBackendTLSPolicyScheme(t)).
		WithObjects(service).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, ok := list.(*gatewayv1.BackendTLSPolicyList); ok && failing.Load() {
					return errSimulatedCacheMiss
				}

				return c.List(ctx, list, opts...)
			},
		}).
		Build()

	_, endpoint := newConfigRecorder(t)
	syncer := NewProxySyncer("cluster.local", "token", "", cli, slog.Default())
	ctx := context.Background()

	healthy, err := syncer.SyncRoutes(ctx, 0, []string{endpoint},
		[]*gatewayv1.HTTPRoute{tlsBackendRoute("ns", "v1.example.com")}, nil, nil, nil)
	require.NoError(t, err)
	require.True(t, hasReason(healthy, string(gatewayv1.RouteReasonUnsupportedProtocol)),
		"a TLS appProtocol with no policy is reported on the route")

	failing.Store(true)

	held, err := syncer.SyncRoutes(ctx, 0, []string{endpoint},
		[]*gatewayv1.HTTPRoute{tlsBackendRoute("ns", "v2.example.com")}, nil, nil, nil)
	require.ErrorIs(t, err, errBackendRefsUndecided)
	require.ErrorIs(t, err, errBackendTLSUnresolved)
	assert.True(t, hasReason(held, routeReasonRefsUndecided))
	assert.False(t, hasReason(held, string(gatewayv1.RouteReasonUnsupportedProtocol)))
	assert.False(t, hasReason(held, string(proxy.ReasonInvalidBackendTLSPolicy)))
}

func hasReason(diags []proxy.RouteDiagnostic, reason string) bool {
	for i := range diags {
		if diags[i].Reason == reason {
			return true
		}
	}

	return false
}

// serviceGetFailingClient serves ns/svc with appProtocol https and no
// BackendTLSPolicy, and fails every Service Get while failing is set.
func serviceGetFailingClient(t *testing.T, failing *atomic.Bool) client.Client {
	t.Helper()

	return fake.NewClientBuilder().
		WithScheme(newBackendTLSPolicyScheme(t)).
		WithObjects(&corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "svc"},
			Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{
				Name: "https", Port: 443, AppProtocol: new("https"),
			}}},
		}).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*corev1.Service); ok && failing.Load() {
					return errSimulatedCacheMiss
				}

				return c.Get(ctx, key, obj, opts...)
			},
		}).
		Build()
}

// TestBackendProtocolResolver_ReadErrors pins the appProtocol lookup: a
// missing Service has no appProtocol, any other read error is unresolved.
func TestBackendProtocolResolver_ReadErrors(t *testing.T) {
	t.Parallel()

	var failing atomic.Bool

	lookup := newBackendProtocolResolver(serviceGetFailingClient(t, &failing))

	got, err := lookup(context.Background(), "ns", "svc", 443)
	require.NoError(t, err)
	assert.Equal(t, "https", got)

	got, err = lookup(context.Background(), "ns", "absent", 443)
	require.NoError(t, err)
	assert.Empty(t, got)

	got, err = newBackendProtocolResolver(fake.NewClientBuilder().WithScheme(runtime.NewScheme()).Build())(
		context.Background(), "ns", "svc", 443)
	require.NoError(t, err, "a client without the Service kind has no appProtocol to wait for")
	assert.Empty(t, got)

	failing.Store(true)

	got, err = lookup(context.Background(), "ns", "svc", 443)
	require.ErrorIs(t, err, errBackendTLSUnresolved)
	require.ErrorIs(t, err, errSimulatedCacheMiss)
	assert.Empty(t, got)
}

// TestProxySyncer_UnreadableServiceHoldsBackPush pins the appProtocol side of
// the held-back push: while the Service cannot be read, nothing is pushed and
// nothing is left to replay; once it can, the TLS appProtocol with no policy
// fails the backend closed.
func TestProxySyncer_UnreadableServiceHoldsBackPush(t *testing.T) {
	t.Parallel()

	var failing atomic.Bool

	failing.Store(true)

	recorder, endpoint := newConfigRecorder(t)
	syncer := NewProxySyncer("cluster.local", "token", "", serviceGetFailingClient(t, &failing), slog.Default())
	ctx := context.Background()
	sync := func(hostname string) ([]proxy.RouteDiagnostic, error) {
		return syncer.SyncRoutes(ctx, 0, []string{endpoint},
			[]*gatewayv1.HTTPRoute{tlsBackendRoute("ns", hostname)}, nil, nil, nil)
	}

	cold, err := sync("v1.example.com")
	require.ErrorIs(t, err, errBackendTLSUnresolved)
	assert.Empty(t, recorder.pushed())
	assert.True(t, hasReason(cold, routeReasonRefsUndecided),
		"the fail-closed appProtocol of an unread Service is no verdict")

	built, err := syncer.pushBuiltConfig(ctx, sharedPartitionKey)
	require.NoError(t, err)
	assert.False(t, built)

	failing.Store(false)

	healthy, err := sync("v1.example.com")
	require.NoError(t, err)
	require.Len(t, recorder.pushed(), 1)
	assert.NotZero(t, recorder.pushed()[0].Rules[0].Backends[0].UnavailableStatus,
		"a TLS appProtocol with no policy fails the backend closed")
	require.True(t, hasReason(healthy, string(gatewayv1.RouteReasonUnsupportedProtocol)))

	failing.Store(true)

	_, err = sync("v2.example.com")
	require.ErrorIs(t, err, errBackendTLSUnresolved)
	assert.Len(t, recorder.pushed(), 1)
}

// TestPushPartitionConfigs_HeldBackPushLeavesTheFailureStreak pins that a
// held-back push neither starts a push-failure streak nor ends one: a healthy
// partition never surfaces ProxyConfigPushed=False from holds alone, and a
// partition already past the threshold keeps surfacing it.
func TestPushPartitionConfigs_HeldBackPushLeavesTheFailureStreak(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		replicaStatus int
		wantSurfaced  bool
	}{
		{name: "healthy partition", replicaStatus: http.StatusOK},
		{name: "partition already failing", replicaStatus: http.StatusInternalServerError, wantSurfaced: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			replica := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.WriteHeader(tt.replicaStatus)
			}))
			t.Cleanup(replica.Close)

			var failing atomic.Bool

			params := syncUpdateParams{
				routeSyncer:    &RouteSyncer{ClusterDomain: "cluster.local", Metrics: cfmetrics.NewNoopCollector()},
				proxySyncer:    NewProxySyncer("cluster.local", "", "", listFailingClient(t, &failing), slog.Default()),
				proxyEndpoints: []string{replica.URL + "/config"},
				pushProxy:      true,
			}

			route := *tlsBackendRoute("ns", "v1.example.com")
			push := func() []proxy.RouteDiagnostic {
				diags, _ := pushPartitionConfigs(context.Background(), slog.Default(), &params, &SyncResult{
					HTTPRoutes: []gatewayv1.HTTPRoute{route},
					Partitions: []routePartition{{Key: sharedPartitionKey, HTTPRoutes: []gatewayv1.HTTPRoute{route}}},
				})

				return diags
			}

			for range pushFailureSurfaceThreshold {
				push()
			}

			streak := params.proxySyncer.pushFailureStreak(sharedPartitionKey)

			failing.Store(true)

			var diags []proxy.RouteDiagnostic
			for range pushFailureSurfaceThreshold + 1 {
				diags = push()
			}

			assert.Equal(t, streak, params.proxySyncer.pushFailureStreak(sharedPartitionKey))
			assert.Equal(t, tt.wantSurfaced, hasTarget(diags, proxy.DiagnosticProxyConfigPush))
		})
	}
}

func hasTarget(diags []proxy.RouteDiagnostic, target proxy.DiagnosticTarget) bool {
	for i := range diags {
		if diags[i].Target == target {
			return true
		}
	}

	return false
}

// TestSyncPartition_UnreadableClientCertHoldsBackPush pins the backend client
// certificate side of the held-back push: when the parent Gateway's
// certificate Secret, or the ReferenceGrant a cross-namespace one needs,
// cannot be read, the config built without the certificate is not pushed.
func TestSyncPartition_UnreadableClientCertHoldsBackPush(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		secretNamespace *gatewayv1.Namespace
		fail            interceptor.Funcs
		wantErr         error
	}{
		{
			name:    "Secret read fails",
			fail:    failGetOf[*corev1.Secret](),
			wantErr: errGatewayClientCertTransientError,
		},
		{
			name:            "ReferenceGrant list fails",
			secretNamespace: new(gatewayv1.Namespace("certs")),
			fail: interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList,
				opts ...client.ListOption,
			) error {
				if _, ok := list.(*gatewayv1beta1.ReferenceGrantList); ok {
					return errSimulatedCacheMiss
				}

				return c.List(ctx, list, opts...)
			}},
			wantErr: errGatewayClientCertTransientError,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			gateway := gatewayWithClientCertRef("b", "gw-a", "client-cert-a", tt.secretNamespace)
			gateway.Spec.Listeners = []gatewayv1.Listener{{Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType}}
			cli := fake.NewClientBuilder().WithScheme(newClientCertScheme(t)).
				WithObjects(gateway).WithInterceptorFuncs(tt.fail).Build()

			recorder, endpoint := newConfigRecorder(t)
			syncer := NewProxySyncer("cluster.local", "token", "", cli, slog.Default())
			syncer.tlsResolver = func(context.Context, string, string, int32, bool) *proxy.BackendTLSConfig {
				return &proxy.BackendTLSConfig{ServerName: "backend.example.com"}
			}

			_, err := syncer.syncPartition(context.Background(), 0, sharedPartitionKey, "token", []string{endpoint},
				[]*gatewayv1.HTTPRoute{certParentHTTPRoute()}, nil, nil, nil,
				clientCertParents{http: certParentSet("b/r", certParentGatewayA)})
			require.ErrorIs(t, err, errBackendRefsUndecided)
			require.ErrorIs(t, err, tt.wantErr)
			assert.Empty(t, recorder.pushed())
		})
	}
}

// failGetOf fails every Get of an object of type T.
func failGetOf[T client.Object]() interceptor.Funcs {
	return interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey,
		obj client.Object, opts ...client.GetOption,
	) error {
		if _, ok := obj.(T); ok {
			return errSimulatedCacheMiss
		}

		return c.Get(ctx, key, obj, opts...)
	}}
}

// TestClientCertResolver_UnreadableParentIsRecorded pins the parent reads
// behind a backend client certificate: a Gateway, its GatewayClass, or a
// ListenerSet parent that cannot be read is recorded on the build collector,
// which holds the push, rather than taken for a parent without a certificate.
// One that does not exist records nothing, so a stale parentRef cannot hold
// the push.
func TestClientCertResolver_UnreadableParentIsRecorded(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		controllerName string
		parentKind     gatewayv1.Kind
		parent         string
		fail           interceptor.Funcs
		wantErr        error
	}{
		{
			name: "unreadable Gateway", parentKind: kindGateway, parent: "gw-a",
			fail: failGetOf[*gatewayv1.Gateway](), wantErr: errSimulatedCacheMiss,
		},
		{
			name: "unreadable GatewayClass", controllerName: "example.com/controller", parentKind: kindGateway, parent: "gw-a",
			fail: failGetOf[*gatewayv1.GatewayClass](), wantErr: errSimulatedCacheMiss,
		},
		{
			name: "unreadable ListenerSet", parentKind: kindListenerSet, parent: "ls",
			fail: failGetOf[*gatewayv1.ListenerSet](), wantErr: errSimulatedCacheMiss,
		},
		{name: "missing Gateway", parentKind: kindGateway, parent: "absent"},
		{name: "missing GatewayClass", controllerName: "example.com/controller", parentKind: kindGateway, parent: "gw-a"},
		{name: "missing ListenerSet", parentKind: kindListenerSet, parent: "ls"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			gateway := gatewayWithClientCertRef("b", "gw-a", "client-cert-a", nil)
			gateway.Spec.GatewayClassName = "cls"
			cli := fake.NewClientBuilder().WithScheme(newClientCertScheme(t)).
				WithObjects(gateway).WithInterceptorFuncs(tt.fail).Build()

			syncer := NewProxySyncer("cluster.local", "token", tt.controllerName, cli, slog.Default())
			resolve := syncer.clientCertResolver(certParentSet("b/r", certParentGatewayA, "b/absent"))

			ctx, readErrs := withBuildReadErrors(context.Background())
			got := resolve(ctx, types.NamespacedName{Namespace: "b", Name: "r"},
				types.NamespacedName{Namespace: "b", Name: tt.parent}, tt.parentKind)
			assert.Nil(t, got)

			if tt.wantErr == nil {
				assert.NoError(t, readErrs.err())
			} else {
				require.ErrorIs(t, readErrs.err(), tt.wantErr)
			}
		})
	}
}
