package controller

// Covers no-API mode (a GatewayClassConfig with no cloudflareCredentialsSecretRef):
// the controller serves Gateways and routes exactly as in API mode, while
// making ZERO Cloudflare API calls — no account auto-detection and no tunnel
// ingress-document GET or PUT. The document only feeds the dashboard; the
// in-process proxy routes every request, so nothing a user observes depends on
// it being written.

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/cloudflare/cloudflare-go/v7"
	"github.com/cloudflare/cloudflare-go/v7/option"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/api/v1alpha1"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/cfmetrics"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/config"
)

const noAPITunnelID = "12345678-1234-4234-8234-123456789abc"

// countingAPI is a Cloudflare API stand-in that must never be reached: it
// counts every request and answers 500 so an accidental call also fails the
// sync it came from.
type countingAPI struct {
	server *httptest.Server
	hits   atomic.Int32
}

func newCountingAPI(t *testing.T) *countingAPI {
	t.Helper()

	api := &countingAPI{}
	api.server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		api.hits.Add(1)
		writer.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(api.server.Close)

	return api
}

// noAPIObjects is the smallest serving world with no Cloudflare credentials:
// a managed class whose config names no credentials Secret (and no account,
// so an account auto-detection attempt would have to call the API), a shared
// Gateway, a backend Service and one HTTPRoute.
func noAPIObjects() []runtime.Object {
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
				TunnelID: noAPITunnelID,
			},
		},
		&gatewayv1.Gateway{
			ObjectMeta: metav1.ObjectMeta{Name: "shared-gw", Namespace: "default"},
			Spec:       gatewayv1.GatewaySpec{GatewayClassName: "cf-test", Listeners: httpListener()},
		},
		&corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "default"},
			Spec:       corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 80}}},
		},
		partitionSyncRoute("app-route", "shared-gw", "app.example.com"),
	}
}

func noAPISyncer(t *testing.T, api *countingAPI, objects []runtime.Object) *RouteSyncer {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, gatewayv1.Install(scheme))
	require.NoError(t, v1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))

	builder := fake.NewClientBuilder().WithScheme(scheme)
	for _, obj := range objects {
		builder = builder.WithRuntimeObjects(obj)
	}

	fakeClient := builder.Build()

	resolver := config.NewResolver(fakeClient, "default", cfmetrics.NewNoopCollector(), verifiedClaims())
	syncer := NewRouteSyncer(fakeClient, scheme, "cluster.local", skipTestControllerName, resolver, cfmetrics.NewNoopCollector(), slog.Default())
	syncer.cloudflareClientFactory = func(_ *config.ResolvedConfig) *cloudflare.Client {
		return cloudflare.NewClient(
			option.WithAPIToken("never-used"),
			option.WithBaseURL(api.server.URL),
		)
	}

	return syncer
}

// TestSyncAllRoutes_NoAPIMode_ZeroCloudflareCalls pins the heart of no-API
// mode: a full route sync accepts the route and makes no Cloudflare API call
// at all — no account lookup, no document GET, no document PUT — and leaves
// nothing behind to retry.
func TestSyncAllRoutes_NoAPIMode_ZeroCloudflareCalls(t *testing.T) {
	t.Parallel()

	api := newCountingAPI(t)
	syncer := noAPISyncer(t, api, noAPIObjects())

	result, syncResult, err := syncer.SyncAllRoutes(context.Background())
	require.NoError(t, err)
	require.NotNil(t, syncResult)

	assert.Equal(t, int32(0), api.hits.Load(), "no-API mode must make zero Cloudflare API calls")
	assert.Zero(t, result.RequeueAfter, "nothing failed, so nothing is left to retry")
	require.Len(t, syncResult.HTTPRoutes, 1, "the route must be accepted exactly as in API mode")
	assert.Equal(t, "app-route", syncResult.HTTPRoutes[0].Name)
}

// TestSyncAllRoutes_NoAPIMode_SteadyStateStaysSilent pins that repeated syncs
// stay at zero calls — the 15s route requeue must not turn no-API mode into a
// drip of failing API traffic (the dummy-token non-workaround the design
// refused).
func TestSyncAllRoutes_NoAPIMode_SteadyStateStaysSilent(t *testing.T) {
	t.Parallel()

	api := newCountingAPI(t)
	syncer := noAPISyncer(t, api, noAPIObjects())

	for range 3 {
		_, _, err := syncer.SyncAllRoutes(context.Background())
		require.NoError(t, err)
	}

	assert.Equal(t, int32(0), api.hits.Load())
}

// TestGatewayReconciler_NoAPIMode_ProgrammedWithUnmanagedDocumentNote pins the
// Gateway status in no-API mode: the tunnel address is published and the
// Gateway goes Accepted/Programmed as usual, with the Programmed message
// saying the ingress document is not managed, so an operator comparing the
// Cloudflare dashboard with the cluster is told why they disagree.
func TestGatewayReconciler_NoAPIMode_ProgrammedWithUnmanagedDocumentNote(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "test-gateway", Namespace: "default"},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "cloudflare-tunnel",
			Listeners:        []gatewayv1.Listener{{Name: "http", Port: 80, Protocol: "HTTP"}},
		},
	}

	gatewayClassConfig := &v1alpha1.GatewayClassConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "test-config"},
		Spec:       v1alpha1.GatewayClassConfigSpec{TunnelID: noAPITunnelID},
	}

	fakeClient := setupGatewayFakeClient(gateway, gatewayClassConfig, &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "cloudflare-tunnel"},
		Spec: gatewayv1.GatewayClassSpec{
			ControllerName: "test-controller",
			ParametersRef: &gatewayv1.ParametersReference{
				Group: config.ParametersRefGroup,
				Kind:  config.ParametersRefKind,
				Name:  "test-config",
			},
		},
	})

	reconciler := &GatewayReconciler{
		Client:         fakeClient,
		Scheme:         fakeClient.Scheme(),
		ControllerName: "test-controller",
		ConfigResolver: config.NewResolver(fakeClient, "default", cfmetrics.NewNoopCollector(), verifiedClaims()),
	}

	result, err := reconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-gateway", Namespace: "default"},
	})
	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, result)

	var updated gatewayv1.Gateway
	require.NoError(t, fakeClient.Get(ctx, types.NamespacedName{Name: "test-gateway", Namespace: "default"}, &updated))

	require.Len(t, updated.Status.Addresses, 1)
	assert.Equal(t, noAPITunnelID+".cfargotunnel.com", updated.Status.Addresses[0].Value)

	var programmed *metav1.Condition
	for i := range updated.Status.Conditions {
		if updated.Status.Conditions[i].Type == string(gatewayv1.GatewayConditionProgrammed) {
			programmed = &updated.Status.Conditions[i]
		}
	}

	require.NotNil(t, programmed)
	assert.Equal(t, metav1.ConditionTrue, programmed.Status)
	assert.Contains(t, programmed.Message, "tunnel ingress document is not managed")
}

// TestGatewayClassConfigReconciler_NoAPIMode_Valid pins that a config without
// a credentials ref is valid as it stands: nothing is referenced, so nothing
// can be missing.
func TestGatewayClassConfigReconciler_NoAPIMode_Valid(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, v1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))

	gccConfig := &v1alpha1.GatewayClassConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "test-config", Generation: 1},
		Spec:       v1alpha1.GatewayClassConfigSpec{TunnelID: noAPITunnelID},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(gccConfig).
		WithStatusSubresource(gccConfig).
		Build()

	reconciler := &GatewayClassConfigReconciler{
		Client:           fakeClient,
		Scheme:           scheme,
		DefaultNamespace: "default",
	}

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-config"},
	})
	require.NoError(t, err)

	var updated v1alpha1.GatewayClassConfig
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{Name: "test-config"}, &updated))

	for _, conditionType := range []string{ConditionTypeSecretsResolved, ConditionTypeValid} {
		condition := metav1.Condition{}
		for i := range updated.Status.Conditions {
			if updated.Status.Conditions[i].Type == conditionType {
				condition = updated.Status.Conditions[i]
			}
		}

		assert.Equal(t, metav1.ConditionTrue, condition.Status, "condition %s", conditionType)
	}
}
