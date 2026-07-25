package controller

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/api/v1alpha1"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/cfmetrics"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/config"
)

// newStartTestRouteSyncer builds a RouteSyncer over a fake client holding no
// GatewayClass at all, so every sync attempt fails at config resolution.
func newStartTestRouteSyncer(t *testing.T) (*RouteSyncer, *runtime.Scheme, client.Client) {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, gatewayv1.Install(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, v1alpha1.AddToScheme(scheme))

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	configResolver := config.NewResolver(fakeClient, "default", cfmetrics.NewNoopCollector())
	routeSyncer := NewRouteSyncer(
		fakeClient, scheme, "cluster.local", "test-controller", configResolver, cfmetrics.NewNoopCollector(), nil)

	return routeSyncer, scheme, fakeClient
}

func newStartTestHTTPRouteReconciler(t *testing.T) *HTTPRouteReconciler {
	t.Helper()

	routeSyncer, scheme, fakeClient := newStartTestRouteSyncer(t)

	return &HTTPRouteReconciler{
		Client:         fakeClient,
		Scheme:         scheme,
		ControllerName: "test-controller",
		RouteSyncer:    routeSyncer,
	}
}

func newStartTestGRPCRouteReconciler(t *testing.T) *GRPCRouteReconciler {
	t.Helper()

	routeSyncer, scheme, fakeClient := newStartTestRouteSyncer(t)

	return &GRPCRouteReconciler{
		Client:         fakeClient,
		Scheme:         scheme,
		ControllerName: "test-controller",
		RouteSyncer:    routeSyncer,
	}
}

func TestRunStartupSync_ImmediateSuccess(t *testing.T) {
	t.Parallel()

	var attempts, completions atomic.Int32

	runStartupSync(context.Background(), slog.Default(), time.Millisecond,
		func() { completions.Add(1) },
		func(_ context.Context) error {
			attempts.Add(1)

			return nil
		},
	)

	assert.Equal(t, int32(1), attempts.Load(), "a successful first attempt needs no retry")
	assert.Equal(t, int32(1), completions.Load(), "markComplete fires exactly once")
}

func TestRunStartupSync_RetriesUntilSuccess(t *testing.T) {
	t.Parallel()

	// The regression this pins: a resolve failure at startup (e.g. the
	// GatewayClassConfig not yet visible on a fresh install) must be retried
	// until it succeeds, not swallowed after one attempt -- a one-shot sync
	// left the data plane permanently configless (#581).
	const failures = 3

	var attempts atomic.Int32

	var completions atomic.Int32

	completeAfterAttempts := int32(-1)

	runStartupSync(context.Background(), slog.Default(), time.Millisecond,
		func() {
			completions.Add(1)
			completeAfterAttempts = attempts.Load()
		},
		func(_ context.Context) error {
			if attempts.Add(1) <= failures {
				return errors.New("GatewayClassConfig not found")
			}

			return nil
		},
	)

	assert.Equal(t, int32(failures+1), attempts.Load(), "must retry until the sync succeeds")
	assert.Equal(t, int32(1), completions.Load(), "markComplete fires exactly once")
	assert.Equal(t, int32(1), completeAfterAttempts,
		"markComplete must fire after the FIRST attempt, not after eventual success -- route reconciles are gated on the attempt having happened")
}

func TestRunStartupSync_StopsOnContextCancel(t *testing.T) {
	t.Parallel()

	var attempts atomic.Int32

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})

	go func() {
		defer close(done)

		runStartupSync(ctx, slog.Default(), time.Millisecond,
			func() {},
			func(_ context.Context) error {
				attempts.Add(1)

				return errors.New("permanently failing")
			},
		)
	}()

	// Let at least one retry happen, then cancel.
	require.Eventually(t, func() bool { return attempts.Load() >= 2 }, 5*time.Second, time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runStartupSync did not return after context cancellation")
	}
}

// assertStartKeepsRetrying drives a reconciler's Start with a permanently
// failing REAL sync (no GatewayClass in the fake client) and pins the three
// behaviors of the retry loop: startupComplete flips after the first attempt,
// Start does NOT return while the sync keeps failing (the sync error is
// swallowed into a RequeueAfter by the reconcile-facing path -- treating that
// as success is the #581 regression), and cancellation shuts it down cleanly.
func assertStartKeepsRetrying(t *testing.T, startupComplete *atomic.Bool, start func(context.Context) error) {
	t.Helper()

	assert.False(t, startupComplete.Load())

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)

	go func() { done <- start(ctx) }()

	require.Eventually(t, func() bool { return startupComplete.Load() }, 5*time.Second, 5*time.Millisecond,
		"startupComplete must flip after the first attempt even though the sync keeps failing")

	select {
	case <-done:
		t.Fatal("Start returned while the sync was still failing -- the swallowed sync error was treated as success")
	case <-time.After(500 * time.Millisecond):
	}

	cancel()

	select {
	case err := <-done:
		assert.NoError(t, err, "shutdown via context cancellation is not an error")
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return after context cancellation")
	}
}

// TestRunStartupSync_QuietOnShutdown pins that a failure caused by manager
// shutdown produces no alarming Error/Warn logs -- the context ending is the
// expected way for the retry loop to stop.
func TestRunStartupSync_QuietOnShutdown(t *testing.T) {
	t.Parallel()

	var logBuf strings.Builder

	logger := slog.New(slog.NewTextHandler(&logBuf, nil))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	runStartupSync(ctx, logger, time.Millisecond,
		func() {},
		func(ctx context.Context) error { return ctx.Err() },
	)

	assert.NotContains(t, logBuf.String(), "level=ERROR",
		"a shutdown-caused failure must not log at ERROR")
	assert.NotContains(t, logBuf.String(), "level=WARN",
		"a shutdown-caused failure must not log at WARN")
}

// TestReplayableTarget_MissingPerGatewayPartitionLogsInfo pins the log split:
// a missing per-Gateway partition is ambiguous (not yet pushed vs already
// evicted), so it logs at INFO, not the WARN reserved for the shared
// partition's bootstrap gap.
func TestReplayableTarget_MissingPerGatewayPartitionLogsInfo(t *testing.T) {
	t.Parallel()

	var logBuf strings.Builder

	logger := slog.New(slog.NewTextHandler(&logBuf, nil))

	testClient := fake.NewClientBuilder().WithScheme(runtime.NewScheme()).Build()
	syncer := NewProxySyncer("cluster.local", "", "", testClient, logger)

	target, ok := syncer.replayableTarget(logger, "gw-tenant-a")

	assert.Nil(t, target)
	assert.False(t, ok)
	assert.Contains(t, logBuf.String(), "level=INFO", "missing per-Gateway partition logs at INFO")
	assert.NotContains(t, logBuf.String(), "level=WARN", "the shared-partition WARN must not fire here")
}

// TestStartupAttempt_Outcomes pins the composition contract of a single
// startup attempt: the attempt fails when the sync path returned an error,
// when a sync error was swallowed into a requeue interval, OR when the proxy
// config push failed -- the initial push is what makes route-less proxy
// replicas ready, so a sync-succeeded/push-failed attempt must be retried
// (#581).
func TestStartupAttempt_Outcomes(t *testing.T) {
	t.Parallel()

	returnedErr := errors.New("returned")
	swallowedErr := errors.New("swallowed sync error")
	pushErr := errors.New("push failed")

	tests := []struct {
		name    string
		run     func(context.Context, *syncUpdateParams) (ctrl.Result, error)
		wantErr error
	}{
		{
			name: "clean attempt succeeds",
			run: func(_ context.Context, _ *syncUpdateParams) (ctrl.Result, error) {
				return ctrl.Result{}, nil
			},
			wantErr: nil,
		},
		{
			name: "returned error fails the attempt",
			run: func(_ context.Context, _ *syncUpdateParams) (ctrl.Result, error) {
				return ctrl.Result{}, returnedErr
			},
			wantErr: returnedErr,
		},
		{
			name: "swallowed sync error fails the attempt",
			run: func(_ context.Context, params *syncUpdateParams) (ctrl.Result, error) {
				params.onSyncError(swallowedErr)

				return ctrl.Result{RequeueAfter: time.Second}, nil
			},
			wantErr: swallowedErr,
		},
		{
			name: "push failure fails the attempt",
			run: func(_ context.Context, params *syncUpdateParams) (ctrl.Result, error) {
				params.onPushError(pushErr)

				return ctrl.Result{}, nil
			},
			wantErr: pushErr,
		},
		{
			// Degraded success: SyncAllRoutes signals a partial tunnel-group
			// failure or a transiently-unresolvable per-Gateway plane via
			// RequeueAfter with a nil error and no callback. On a route-less
			// cluster the startup loop is the only requeue channel, so the
			// attempt must fail and be retried.
			name: "requeue request without an error fails the attempt",
			run: func(_ context.Context, _ *syncUpdateParams) (ctrl.Result, error) {
				return ctrl.Result{RequeueAfter: time.Second}, nil
			},
			wantErr: errRequeueRequested,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			attempt := startupAttempt(&syncUpdateParams{}, tt.run)

			err := attempt(context.Background())
			if tt.wantErr == nil {
				assert.NoError(t, err)
			} else {
				assert.ErrorIs(t, err, tt.wantErr)
			}
		})
	}
}

// TestPushPartitionConfigs_ReportsPushFailureViaCallback pins the plumbing the
// startup retry depends on: a failed partition push must reach onPushError,
// and a successful one must not.
func TestPushPartitionConfigs_ReportsPushFailureViaCallback(t *testing.T) {
	t.Parallel()

	failing := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(failing.Close)

	working := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(working.Close)

	newResult := func() *SyncResult {
		return &SyncResult{
			Partitions: []routePartition{{Key: sharedPartitionKey}},
		}
	}

	newParams := func(endpoint string, capture *error) *syncUpdateParams {
		testClient := fake.NewClientBuilder().WithScheme(runtime.NewScheme()).Build()

		return &syncUpdateParams{
			routeSyncer:    &RouteSyncer{ClusterDomain: "cluster.local", Metrics: cfmetrics.NewNoopCollector()},
			proxySyncer:    NewProxySyncer("cluster.local", "", "", testClient, slog.Default()),
			proxyEndpoints: []string{endpoint + "/config"},
			pushProxy:      true,
			onPushError:    func(err error) { *capture = err },
		}
	}

	var failedPushErr error

	pushPartitionConfigs(context.Background(), slog.Default(), newParams(failing.URL, &failedPushErr), newResult())
	assert.Error(t, failedPushErr, "a failed partition push must reach onPushError")

	var cleanPushErr error

	pushPartitionConfigs(context.Background(), slog.Default(), newParams(working.URL, &cleanPushErr), newResult())
	assert.NoError(t, cleanPushErr, "a successful push must not report an error")
}

func TestHTTPRouteReconciler_Start_RetriesAndUnblocksReconciles(t *testing.T) {
	t.Parallel()

	r := newStartTestHTTPRouteReconciler(t)

	assertStartKeepsRetrying(t, &r.startupComplete, r.Start)
}

func TestGRPCRouteReconciler_Start_RetriesAndUnblocksReconciles(t *testing.T) {
	t.Parallel()

	r := newStartTestGRPCRouteReconciler(t)

	assertStartKeepsRetrying(t, &r.startupComplete, r.Start)
}
