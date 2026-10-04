package controller

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/cfmetrics"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/config"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/logging"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/routebinding"
)

// failReads fails every Get of the given type whose name is name, or of any
// name when name is empty, while fail reports true.
func failReads[T client.Object](name string, fail *atomic.Bool) interceptor.Funcs {
	return interceptor.Funcs{
		Get: func(ctx context.Context, cli client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(T); ok && (name == "" || key.Name == name) && fail.Load() {
				return errSimulatedCacheMiss
			}

			return cli.Get(ctx, key, obj, opts...)
		},
	}
}

func failing() *atomic.Bool {
	var fail atomic.Bool

	fail.Store(true)

	return &fail
}

// TestRouteReferencesOurGateways_ReadErrorIsReturned pins that a parent whose
// Gateway, ListenerSet or GatewayClass cannot be read is not taken for a parent
// that is not ours: the error is returned so the reconcile retries. A parent
// that is ours wins over a sibling that cannot be read.
func TestRouteReferencesOurGateways_ReadErrorIsReturned(t *testing.T) {
	t.Parallel()

	listenerSetKind := gatewayv1.Kind(kindListenerSet)

	tests := []struct {
		name    string
		refs    []gatewayv1.ParentReference
		funcs   interceptor.Funcs
		want    bool
		wantErr bool
	}{
		{
			name:    "gateway read fails",
			refs:    []gatewayv1.ParentReference{{Name: "other"}},
			funcs:   failReads[*gatewayv1.Gateway]("other", failing()),
			wantErr: true,
		},
		{
			name:    "listenerset read fails",
			refs:    []gatewayv1.ParentReference{{Name: "extra", Kind: &listenerSetKind}},
			funcs:   failReads[*gatewayv1.ListenerSet]("", failing()),
			wantErr: true,
		},
		{
			name:    "gatewayclass read fails",
			refs:    []gatewayv1.ParentReference{{Name: "other"}},
			funcs:   failReads[*gatewayv1.GatewayClass]("", failing()),
			wantErr: true,
		},
		{
			name:  "a managed sibling decides",
			refs:  []gatewayv1.ParentReference{{Name: "other"}, {Name: "healthy"}},
			funcs: failReads[*gatewayv1.Gateway]("other", failing()),
			want:  true,
		},
		{
			name: "gateway does not exist",
			refs: []gatewayv1.ParentReference{{Name: "missing"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			syncer := unreadableParentSyncer(t, func(client.Object, client.ObjectKey) bool { return false }, tt.refs)
			cli := interceptor.NewClient(syncer.Client.(client.WithWatch), tt.funcs)

			route := &gatewayv1.HTTPRoute{
				ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "default"},
				Spec:       gatewayv1.HTTPRouteSpec{CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: tt.refs}},
			}

			got, err := routeReferencesOurGateways(context.Background(), cli, "cloudflare-tunnel", HTTPRouteWrapper{route})
			if tt.wantErr {
				require.Error(t, err)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestGatewayReconciler_UnreadableClassIsRetried pins that a Gateway whose
// GatewayClass cannot be read is retried rather than skipped as foreign.
func TestGatewayReconciler_UnreadableClassIsRetried(t *testing.T) {
	t.Parallel()

	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
		Spec:       gatewayv1.GatewaySpec{GatewayClassName: "cloudflare-tunnel", Listeners: httpListener()},
	}

	base := setupGatewayFakeClient(gateway)
	cli := interceptor.NewClient(base, failReads[*gatewayv1.GatewayClass]("", failing()))

	reconciler := &GatewayReconciler{
		Client:         cli,
		Scheme:         base.Scheme(),
		ControllerName: "test-controller",
		ConfigResolver: config.NewResolver(cli, "default", cfmetrics.NewNoopCollector(), verifiedClaims()),
	}

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "gw", Namespace: "default"},
	})
	require.Error(t, err)
}

// TestGatewayReconciler_ManagedGatewaysListFailureIsLogged pins that a failed
// Gateway list in the fan-out mapper is logged rather than dropped silently.
func TestGatewayReconciler_ManagedGatewaysListFailureIsLogged(t *testing.T) {
	t.Parallel()

	base := setupGatewayFakeClient()
	cli := interceptor.NewClient(base, interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return errSimulatedCacheMiss
		},
	})

	logger, logs := logging.TestLogger(t)
	ctx := logging.WithLogger(context.Background(), logger)

	reconciler := &GatewayReconciler{Client: cli, Scheme: base.Scheme(), ControllerName: "test-controller"}

	assert.Empty(t, reconciler.getAllManagedGateways(ctx))
	assert.Contains(t, logs.String(), errSimulatedCacheMiss.Error())
}

// TestListenerSetReconciler_UnreadableNamespaceIsPendingAndRetried pins that a
// ListenerSet whose namespace cannot be read for the Gateway's
// allowedListeners selector is reported Pending, not refused, and retried.
func TestListenerSetReconciler_UnreadableNamespaceIsPendingAndRetried(t *testing.T) {
	t.Parallel()

	fromSelector := gatewayv1.NamespacesFromSelector
	gc := managedGatewayClass()
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "infra"},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(gc.Name),
			AllowedListeners: &gatewayv1.AllowedListeners{
				Namespaces: &gatewayv1.ListenerNamespaces{
					From:     &fromSelector,
					Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"team": "a"}},
				},
			},
		},
	}
	ls := &gatewayv1.ListenerSet{
		ObjectMeta: metav1.ObjectMeta{Name: "ls", Namespace: "team-a"},
		Spec: gatewayv1.ListenerSetSpec{
			ParentRef: gatewayv1.ParentGatewayReference{
				Name: gatewayv1.ObjectName(gw.Name), Namespace: new(gatewayv1.Namespace("infra")),
			},
			Listeners: []gatewayv1.ListenerEntry{{Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType}},
		},
	}

	scheme := newListenerSetScheme(t)
	require.NoError(t, corev1.AddToScheme(scheme))

	cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(gc, gw, ls).
		WithStatusSubresource(&gatewayv1.ListenerSet{}, &gatewayv1.Gateway{}).
		WithInterceptorFuncs(failReads[*corev1.Namespace]("", failing())).Build()

	r := &ListenerSetReconciler{Client: cli, Scheme: scheme, ControllerName: testListenerSetController}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: ls.Name, Namespace: ls.Namespace},
	})
	require.Error(t, err, "the reconcile must be retried")

	updated := getListenerSet(t, cli, ls.Name, ls.Namespace)
	accepted := meta.FindStatusCondition(updated.Status.Conditions, string(gatewayv1.ListenerSetConditionAccepted))
	require.NotNil(t, accepted)
	assert.Equal(t, string(gatewayv1.ListenerSetReasonPending), accepted.Reason)
}

// selectorGatewayObjects adds to the partition-sync fixtures a Gateway whose
// only listener admits routes from namespaces labelled team=a, the labelled
// default namespace, and a route on that Gateway.
func selectorGatewayObjects(t *testing.T, selector *metav1.LabelSelector) []runtime.Object {
	t.Helper()

	fromSelector := gatewayv1.NamespacesFromSelector
	listeners := httpListener()
	listeners[0].AllowedRoutes = &gatewayv1.AllowedRoutes{
		Namespaces: &gatewayv1.RouteNamespaces{From: &fromSelector, Selector: selector},
	}

	return append(partitionSyncObjects(t, "99999999-9999-4999-8999-999999999999", false),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default", Labels: map[string]string{"team": "a"}}},
		&gatewayv1.Gateway{
			ObjectMeta: metav1.ObjectMeta{Name: "selector-gw", Namespace: "default"},
			Spec:       gatewayv1.GatewaySpec{GatewayClassName: "cf-test", Listeners: listeners},
		},
		partitionSyncRoute("probe", "selector-gw", "probe.example.com"),
	)
}

// TestSyncAllRoutes_UnreadableRouteNamespaceRequeues pins that a route on a
// selector listener whose namespace cannot be read is Pending on that parent
// and retried, then bound once the read recovers, instead of being refused
// NotAllowedByListeners with nothing to bring it back.
func TestSyncAllRoutes_UnreadableRouteNamespaceRequeues(t *testing.T) {
	t.Parallel()

	fail := failing()
	objects := selectorGatewayObjects(t, &metav1.LabelSelector{MatchLabels: map[string]string{"team": "a"}})
	syncer := partitionSyncSyncerFor(t, newRecordingTunnelAPI(t), objects, failReads[*corev1.Namespace]("", fail))

	result, syncResult, err := syncer.SyncAllRoutes(context.Background())
	require.NoError(t, err)
	assert.Equal(t, gatewayv1.RouteReasonPending, syncResult.HTTPRouteBindings["default/probe"].bindingResults[0].Reason)
	assert.Equal(t, apiErrorRequeueDelay, result.RequeueAfter)

	fail.Store(false)

	result, syncResult, err = syncer.SyncAllRoutes(context.Background())
	require.NoError(t, err)
	assert.True(t, syncResult.HTTPRouteBindings["default/probe"].bindingResults[0].Accepted)
	assert.Zero(t, result.RequeueAfter)
}

// TestSyncAllRoutes_PersistentFailuresLogOncePerState pins that a parent that
// cannot be evaluated, and a listener whose selector does not parse, are each
// reported once while the problem persists across syncs, and again after a
// sync in which it was gone.
func TestSyncAllRoutes_PersistentFailuresLogOncePerState(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		selector *metav1.LabelSelector
		level    string
		recovers bool
	}{
		{
			name:     "unevaluated parent",
			selector: &metav1.LabelSelector{MatchLabels: map[string]string{"team": "a"}},
			level:    `"level":"ERROR"`,
			recovers: true,
		},
		{
			name: "invalid selector",
			selector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
				{Key: "team", Operator: "BogusOperator", Values: []string{"x"}},
			}},
			level: `"level":"WARN"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fail := failing()
			syncer := partitionSyncSyncerFor(t, newRecordingTunnelAPI(t), selectorGatewayObjects(t, tt.selector),
				failReads[*corev1.Namespace]("", fail))

			logger, logs := logging.TestLogger(t)
			ctx := logging.WithLogger(context.Background(), logger)

			count := func() int {
				lines := 0

				for line := range strings.SplitSeq(logs.String(), "\n") {
					if strings.Contains(line, tt.level) && strings.Contains(line, "default/probe") {
						lines++
					}
				}

				return lines
			}

			sync := func(failing bool) {
				fail.Store(failing)

				_, _, err := syncer.SyncAllRoutes(ctx)
				require.NoError(t, err)
			}

			sync(true)
			sync(true)
			assert.Equal(t, 1, count(), logs.String())

			if !tt.recovers {
				return
			}

			sync(false)
			sync(true)
			assert.Equal(t, 2, count(), logs.String())
		})
	}
}

// TestListenerSetReconciler_UnenumerableSiblingsArePendingAndRetried pins that
// a ListenerSet whose sibling ListenerSets cannot be listed is reported Pending
// and retried, as when its namespace cannot be read.
func TestListenerSetReconciler_UnenumerableSiblingsArePendingAndRetried(t *testing.T) {
	t.Parallel()

	fromAll := gatewayv1.NamespacesFromAll
	gc := managedGatewayClass()
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "infra"},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(gc.Name),
			AllowedListeners: &gatewayv1.AllowedListeners{Namespaces: &gatewayv1.ListenerNamespaces{From: &fromAll}},
		},
	}
	ls := &gatewayv1.ListenerSet{
		ObjectMeta: metav1.ObjectMeta{Name: "ls", Namespace: "team-a"},
		Spec: gatewayv1.ListenerSetSpec{
			ParentRef: gatewayv1.ParentGatewayReference{
				Name: gatewayv1.ObjectName(gw.Name), Namespace: new(gatewayv1.Namespace("infra")),
			},
			Listeners: []gatewayv1.ListenerEntry{{Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType}},
		},
	}

	scheme := newListenerSetScheme(t)

	cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(gc, gw, ls).
		WithStatusSubresource(&gatewayv1.ListenerSet{}, &gatewayv1.Gateway{}).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, cli client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, ok := list.(*gatewayv1.ListenerSetList); ok {
					return errSimulatedCacheMiss
				}

				return cli.List(ctx, list, opts...)
			},
		}).Build()

	r := &ListenerSetReconciler{Client: cli, Scheme: scheme, ControllerName: testListenerSetController}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: ls.Name, Namespace: ls.Namespace},
	})
	require.Error(t, err, "the reconcile must be retried")

	updated := getListenerSet(t, cli, ls.Name, ls.Namespace)
	accepted := meta.FindStatusCondition(updated.Status.Conditions, string(gatewayv1.ListenerSetConditionAccepted))
	require.NotNil(t, accepted)
	assert.Equal(t, string(gatewayv1.ListenerSetReasonPending), accepted.Reason)
	assert.NotContains(t, accepted.Message, errSimulatedCacheMiss.Error(), "the error belongs in the log")
}

// TestSyncAllRoutes_UnreadableNamespaceKeepsSiblingListenerBinding pins that a
// namespace read failure on a selector listener does not take the route off a
// sibling listener that admits it: the route stays Accepted and programmed,
// and the sync requeues to evaluate the selector listener again.
func TestSyncAllRoutes_UnreadableNamespaceKeepsSiblingListenerBinding(t *testing.T) {
	t.Parallel()

	objects := selectorGatewayObjects(t, &metav1.LabelSelector{MatchLabels: map[string]string{"team": "a"}})

	for _, obj := range objects {
		if gw, ok := obj.(*gatewayv1.Gateway); ok && gw.Name == "selector-gw" {
			gw.Spec.Listeners = append(gw.Spec.Listeners,
				gatewayv1.Listener{Name: "same", Port: 8080, Protocol: gatewayv1.HTTPProtocolType})
		}
	}

	syncer := partitionSyncSyncerFor(t, newRecordingTunnelAPI(t), objects, failReads[*corev1.Namespace]("", failing()))

	result, syncResult, err := syncer.SyncAllRoutes(context.Background())
	require.NoError(t, err)

	binding := syncResult.HTTPRouteBindings["default/probe"]
	assert.True(t, binding.bindingResults[0].Accepted)
	assert.True(t, binding.acceptedGateways["default/selector-gw"])
	assert.Equal(t, apiErrorRequeueDelay, result.RequeueAfter)
}

// TestSyncAllRoutes_UnreadableNamespaceWithOnlyConflictedSiblingsIsPending pins
// that when every listener that admits the route is conflicted, a selector
// listener that could not be evaluated leaves the parent Pending rather than
// NoMatchingParent: that listener may still admit the route.
func TestSyncAllRoutes_UnreadableNamespaceWithOnlyConflictedSiblingsIsPending(t *testing.T) {
	t.Parallel()

	objects := selectorGatewayObjects(t, &metav1.LabelSelector{MatchLabels: map[string]string{"team": "a"}})

	for _, obj := range objects {
		if gw, ok := obj.(*gatewayv1.Gateway); ok && gw.Name == "selector-gw" {
			gw.Spec.Listeners = append(gw.Spec.Listeners,
				gatewayv1.Listener{Name: "same-a", Port: 8080, Protocol: gatewayv1.HTTPProtocolType},
				gatewayv1.Listener{Name: "same-b", Port: 8080, Protocol: gatewayv1.HTTPProtocolType})
		}
	}

	syncer := partitionSyncSyncerFor(t, newRecordingTunnelAPI(t), objects, failReads[*corev1.Namespace]("", failing()))

	result, syncResult, err := syncer.SyncAllRoutes(context.Background())
	require.NoError(t, err)

	assert.Equal(t, gatewayv1.RouteReasonPending, syncResult.HTTPRouteBindings["default/probe"].bindingResults[0].Reason)
	assert.Equal(t, apiErrorRequeueDelay, result.RequeueAfter)
}

// TestAcceptedListenerSets_UnreadableNamespaceExcludesOnlyThatSet pins that a
// ListenerSet whose namespace cannot be read for the Gateway's allowedListeners
// selector is left out of the Gateway's view on its own: its siblings stay
// accepted, and its error is returned alongside them.
func TestAcceptedListenerSets_UnreadableNamespaceExcludesOnlyThatSet(t *testing.T) {
	t.Parallel()

	fromSelector := gatewayv1.NamespacesFromSelector
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "infra"},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "cf-test",
			AllowedListeners: &gatewayv1.AllowedListeners{
				Namespaces: &gatewayv1.ListenerNamespaces{
					From:     &fromSelector,
					Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"team": "a"}},
				},
			},
		},
	}

	listenerSet := func(namespace string) *gatewayv1.ListenerSet {
		return &gatewayv1.ListenerSet{
			ObjectMeta: metav1.ObjectMeta{Name: "ls", Namespace: namespace},
			Spec: gatewayv1.ListenerSetSpec{
				ParentRef: gatewayv1.ParentGatewayReference{
					Name: gatewayv1.ObjectName(gw.Name), Namespace: new(gatewayv1.Namespace("infra")),
				},
				Listeners: []gatewayv1.ListenerEntry{{Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType}},
			},
		}
	}

	scheme := newListenerSetScheme(t)
	require.NoError(t, corev1.AddToScheme(scheme))

	cli := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(gw, listenerSet("team-a"), listenerSet("team-b"),
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-a", Labels: map[string]string{"team": "a"}}},
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-b", Labels: map[string]string{"team": "a"}}}).
		WithInterceptorFuncs(failReads[*corev1.Namespace]("team-b", failing())).Build()

	accepted, err := collectAcceptedListenerSetsForGateway(context.Background(), cli, gw)
	require.Error(t, err, "the unevaluated ListenerSet is reported")
	require.Len(t, accepted, 1)
	assert.Equal(t, "team-a", accepted[0].Namespace)
}

// TestPushPartitionConfigs_UndecidedParentLogsOncePerState pins that a route
// narrowed because one of its parents cannot be evaluated is reported at Error
// once while the failure persists, not on every sync's proxy push.
func TestPushPartitionConfigs_UndecidedParentLogsOncePerState(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	fromSelector := gatewayv1.NamespacesFromSelector
	gc := managedGatewayClass()
	lending := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "lending", Namespace: "default"},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(gc.Name),
			Listeners: []gatewayv1.Listener{{
				Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType, Hostname: new(gatewayv1.Hostname("lent.example.com")),
			}},
		},
	}
	selecting := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "selecting", Namespace: "default"},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(gc.Name),
			Listeners: []gatewayv1.Listener{{
				Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType,
				AllowedRoutes: &gatewayv1.AllowedRoutes{Namespaces: &gatewayv1.RouteNamespaces{
					From: &fromSelector, Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"team": "a"}},
				}},
			}},
		},
	}

	route := partitionSyncRoute("probe", "lending", "")
	route.Spec.Hostnames = nil
	route.Spec.ParentRefs = append(route.Spec.ParentRefs, gatewayv1.ParentReference{Name: "selecting"})

	scheme := runtime.NewScheme()
	require.NoError(t, gatewayv1.Install(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))

	cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(gc, lending, selecting).
		WithInterceptorFuncs(failReads[*corev1.Namespace]("", failing())).Build()

	repeats := logging.NewRepeats()
	params := syncUpdateParams{
		routeSyncer:    &RouteSyncer{ClusterDomain: "cluster.local", Metrics: cfmetrics.NewNoopCollector(), logRepeats: repeats},
		proxySyncer:    NewProxySyncer("cluster.local", "shared-token", testListenerSetController, cli, slog.Default()),
		proxyEndpoints: []string{server.URL + "/config"},
		pushProxy:      true,
	}

	logger, logs := logging.TestLogger(t)
	ctx := logging.WithLogger(context.Background(), logger)

	for range 2 {
		repeats.NextPass()
		pushPartitionConfigs(ctx, logger, &params, &SyncResult{
			Partitions: []routePartition{{Key: sharedPartitionKey, HTTPRoutes: []gatewayv1.HTTPRoute{*route}}},
		})
	}

	narrowed := 0

	for line := range strings.SplitSeq(logs.String(), "\n") {
		if strings.Contains(line, `"level":"ERROR"`) && strings.Contains(line, "route narrowed to the hostnames its other parents lend") {
			narrowed++
		}
	}

	assert.Equal(t, 1, narrowed, logs.String())
}

// pushDiagnosticsWithUnreadableNamespaces pushes route to the shared partition
// with every namespace read failing and returns the push diagnostics.
func pushDiagnosticsWithUnreadableNamespaces(t *testing.T, route *gatewayv1.HTTPRoute, objects ...client.Object) []proxy.RouteDiagnostic {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	scheme := newListenerSetScheme(t)
	require.NoError(t, corev1.AddToScheme(scheme))

	cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).
		WithInterceptorFuncs(failReads[*corev1.Namespace]("", failing())).Build()

	params := syncUpdateParams{
		routeSyncer:    &RouteSyncer{ClusterDomain: "cluster.local", Metrics: cfmetrics.NewNoopCollector()},
		proxySyncer:    NewProxySyncer("cluster.local", "shared-token", testListenerSetController, cli, slog.Default()),
		proxyEndpoints: []string{server.URL + "/config"},
		pushProxy:      true,
	}

	diags, _ := pushPartitionConfigs(context.Background(), slog.Default(), &params, &SyncResult{
		Partitions: []routePartition{{Key: sharedPartitionKey, HTTPRoutes: []gatewayv1.HTTPRoute{*route}}},
	})

	return diags
}

func hasParentNotEvaluated(diags []proxy.RouteDiagnostic, name string) bool {
	return slices.ContainsFunc(diags, func(diag proxy.RouteDiagnostic) bool {
		return diag.Name == name && diag.Reason == routeReasonParentNotEvaluated
	})
}

func teamSelectorRoutes() *gatewayv1.AllowedRoutes {
	fromSelector := gatewayv1.NamespacesFromSelector

	return &gatewayv1.AllowedRoutes{Namespaces: &gatewayv1.RouteNamespaces{
		From: &fromSelector, Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"team": "a"}},
	}}
}

// TestPushPartitionConfigs_UnevaluatedSiblingListenerReportsNarrowing pins
// that a route served through one listener while a sibling selector listener
// cannot be evaluated is reported as narrowed: the hostnames only the
// unevaluated listener would serve are not routed, and that is not silent.
func TestPushPartitionConfigs_UnevaluatedSiblingListenerReportsNarrowing(t *testing.T) {
	t.Parallel()

	gc := managedGatewayClass()
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(gc.Name),
			Listeners: []gatewayv1.Listener{
				{Name: "same", Port: 80, Protocol: gatewayv1.HTTPProtocolType, Hostname: new(gatewayv1.Hostname("a.example.com"))},
				{
					Name: "sel", Port: 80, Protocol: gatewayv1.HTTPProtocolType, Hostname: new(gatewayv1.Hostname("b.example.com")),
					AllowedRoutes: teamSelectorRoutes(),
				},
			},
		},
	}

	route := partitionSyncRoute("probe", "gw", "a.example.com")
	route.Spec.Hostnames = append(route.Spec.Hostnames, "b.example.com")

	diags := pushDiagnosticsWithUnreadableNamespaces(t, route, gc, gw)
	assert.True(t, hasParentNotEvaluated(diags, "probe"), "%+v", diags)
}

// TestPushPartitionConfigs_UnevaluatedSiblingEntryReportsNarrowing is the
// ListenerSet twin of the test above: a sibling selector entry that cannot be
// evaluated narrows the route, and that is reported.
func TestPushPartitionConfigs_UnevaluatedSiblingEntryReportsNarrowing(t *testing.T) {
	t.Parallel()

	fromSame := gatewayv1.NamespacesFromSame
	gc := managedGatewayClass()
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(gc.Name),
			AllowedListeners: &gatewayv1.AllowedListeners{Namespaces: &gatewayv1.ListenerNamespaces{From: &fromSame}},
		},
	}
	ls := &gatewayv1.ListenerSet{
		ObjectMeta: metav1.ObjectMeta{Name: "ls", Namespace: "default"},
		Spec: gatewayv1.ListenerSetSpec{
			ParentRef: gatewayv1.ParentGatewayReference{Name: gatewayv1.ObjectName(gw.Name)},
			Listeners: []gatewayv1.ListenerEntry{
				{Name: "same", Port: 80, Protocol: gatewayv1.HTTPProtocolType, Hostname: new(gatewayv1.Hostname("a.example.com"))},
				{
					Name: "sel", Port: 80, Protocol: gatewayv1.HTTPProtocolType, Hostname: new(gatewayv1.Hostname("b.example.com")),
					AllowedRoutes: teamSelectorRoutes(),
				},
			},
		},
	}

	route := partitionSyncRoute("probe", "ls", "a.example.com")
	route.Spec.Hostnames = append(route.Spec.Hostnames, "b.example.com")
	route.Spec.ParentRefs[0].Kind = new(gatewayv1.Kind(kindListenerSet))

	diags := pushDiagnosticsWithUnreadableNamespaces(t, route, gc, gw, ls)
	assert.True(t, hasParentNotEvaluated(diags, "probe"), "%+v", diags)
}

// TestPushPartitionConfigs_ConflictedMatchWithUnevaluatedSiblingReportsNarrowing
// pins that a parent whose only matched listeners are conflicted, while
// another of its listeners cannot be evaluated, is reported as undecided when a
// second parent lends the route its hostnames.
func TestPushPartitionConfigs_ConflictedMatchWithUnevaluatedSiblingReportsNarrowing(t *testing.T) {
	t.Parallel()

	gc := managedGatewayClass()
	conflicted := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "conflicted", Namespace: "default"},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(gc.Name),
			Listeners: []gatewayv1.Listener{
				{Name: "same-a", Port: 8080, Protocol: gatewayv1.HTTPProtocolType},
				{Name: "same-b", Port: 8080, Protocol: gatewayv1.HTTPProtocolType},
				{Name: "sel", Port: 80, Protocol: gatewayv1.HTTPProtocolType, AllowedRoutes: teamSelectorRoutes()},
			},
		},
	}
	lending := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "lending", Namespace: "default"},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(gc.Name),
			Listeners: []gatewayv1.Listener{{
				Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType, Hostname: new(gatewayv1.Hostname("lent.example.com")),
			}},
		},
	}

	route := partitionSyncRoute("probe", "conflicted", "")
	route.Spec.Hostnames = nil
	route.Spec.ParentRefs = append(route.Spec.ParentRefs, gatewayv1.ParentReference{Name: "lending"})

	diags := pushDiagnosticsWithUnreadableNamespaces(t, route, gc, conflicted, lending)
	assert.True(t, hasParentNotEvaluated(diags, "probe"), "%+v", diags)
}

// TestGatewayUpdateStatus_UncountableRouteKeepsAttachedRoutes pins that an
// attachedRoutes count that could not be finished, because an Accepted route's
// binding cannot be evaluated, is not written as final: the listener keeps the
// count it had and the reconcile is retried.
func TestGatewayUpdateStatus_UncountableRouteKeepsAttachedRoutes(t *testing.T) {
	t.Parallel()

	// The config-error writer is requeued on a timer whatever it returns, so
	// only the resolved writer has to report the unfinished count.
	writers := map[string]struct {
		write   func(*GatewayReconciler, *gatewayv1.Gateway) error
		wantErr bool
	}{
		"resolved": {write: func(r *GatewayReconciler, gw *gatewayv1.Gateway) error {
			return r.updateStatus(context.Background(), gw, &config.ResolvedConfig{TunnelID: "tunnel"}, false)
		}, wantErr: true},
		"config error": {write: func(r *GatewayReconciler, gw *gatewayv1.Gateway) error {
			return r.setConfigErrorStatus(context.Background(), gw, config.MarkInvalidParameters(errSimulatedCacheMiss))
		}},
	}

	for name, writer := range writers {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			gateway := &gatewayv1.Gateway{
				ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
				Spec: gatewayv1.GatewaySpec{
					GatewayClassName: "cloudflare-tunnel",
					Listeners: []gatewayv1.Listener{
						{Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType, AllowedRoutes: teamSelectorRoutes()},
					},
				},
				Status: gatewayv1.GatewayStatus{Listeners: []gatewayv1.ListenerStatus{{Name: "http", AttachedRoutes: 1}}},
			}
			ref := gatewayv1.ParentReference{Name: "gw"}
			route := attachedRoute("probe", nil, ref)
			route.Status.Parents = []gatewayv1.RouteParentStatus{
				parentStatusFor(ref, "default", "test-controller", metav1.ConditionTrue, string(gatewayv1.RouteReasonAccepted)),
			}

			base := setupGatewayFakeClient(gateway, route)
			cli := interceptor.NewClient(base, failReads[*corev1.Namespace]("", failing()))
			reconciler := &GatewayReconciler{Client: cli, Scheme: base.Scheme(), ControllerName: "test-controller"}

			assert.Equal(t, writer.wantErr, writer.write(reconciler, gateway) != nil)

			var updated gatewayv1.Gateway
			require.NoError(t, base.Get(context.Background(), client.ObjectKeyFromObject(gateway), &updated))
			require.Len(t, updated.Status.Listeners, 1)
			assert.Equal(t, int32(1), updated.Status.Listeners[0].AttachedRoutes)
		})
	}
}

// TestListenerSetReconciler_UncountableRouteKeepsAttachedRoutes is the
// ListenerSet twin: the entry keeps its count, the ListenerSet keeps its
// acceptance instead of going Pending over a count, and the reconcile retries.
func TestListenerSetReconciler_UncountableRouteKeepsAttachedRoutes(t *testing.T) {
	t.Parallel()

	fromSame := gatewayv1.NamespacesFromSame
	gc := managedGatewayClass()
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(gc.Name),
			AllowedListeners: &gatewayv1.AllowedListeners{Namespaces: &gatewayv1.ListenerNamespaces{From: &fromSame}},
		},
	}
	ls := &gatewayv1.ListenerSet{
		ObjectMeta: metav1.ObjectMeta{Name: "ls", Namespace: "default"},
		Spec: gatewayv1.ListenerSetSpec{
			ParentRef: gatewayv1.ParentGatewayReference{Name: gatewayv1.ObjectName(gw.Name)},
			Listeners: []gatewayv1.ListenerEntry{
				{Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType, AllowedRoutes: teamSelectorRoutes()},
			},
		},
		Status: gatewayv1.ListenerSetStatus{Listeners: []gatewayv1.ListenerEntryStatus{{Name: "http", AttachedRoutes: 1}}},
	}
	ref := gatewayv1.ParentReference{Name: "ls", Kind: new(gatewayv1.Kind(kindListenerSet))}
	route := attachedRoute("probe", nil, ref)
	route.Status.Parents = []gatewayv1.RouteParentStatus{
		parentStatusFor(ref, "default", testListenerSetController, metav1.ConditionTrue, string(gatewayv1.RouteReasonAccepted)),
	}

	scheme := newListenerSetScheme(t)
	require.NoError(t, corev1.AddToScheme(scheme))

	cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(gc, gw, ls, route).
		WithStatusSubresource(&gatewayv1.ListenerSet{}, &gatewayv1.Gateway{}).
		WithInterceptorFuncs(failReads[*corev1.Namespace]("", failing())).Build()

	r := &ListenerSetReconciler{Client: cli, Scheme: scheme, ControllerName: testListenerSetController}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "ls", Namespace: "default"}})
	require.Error(t, err, "the reconcile must be retried")

	updated := getListenerSet(t, cli, ls.Name, ls.Namespace)
	accepted := meta.FindStatusCondition(updated.Status.Conditions, string(gatewayv1.ListenerSetConditionAccepted))
	require.NotNil(t, accepted)
	assert.NotEqual(t, string(gatewayv1.ListenerSetReasonPending), accepted.Reason)
	require.Len(t, updated.Status.Listeners, 1)
	assert.Equal(t, int32(1), updated.Status.Listeners[0].AttachedRoutes)
}

// TestGatewayUpdateStatus_UnlistableListenerSetsKeepAttachedListenerSets pins
// the same rule for attachedListenerSets: a failed ListenerSet list keeps the
// count already written instead of reporting 0, and the reconcile is retried.
func TestGatewayUpdateStatus_UnlistableListenerSetsKeepAttachedListenerSets(t *testing.T) {
	t.Parallel()

	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
		Spec:       gatewayv1.GatewaySpec{GatewayClassName: "cloudflare-tunnel", Listeners: httpListener()},
		Status:     gatewayv1.GatewayStatus{AttachedListenerSets: new(int32(2))},
	}

	base := setupGatewayFakeClient(gateway)
	cli := interceptor.NewClient(base, interceptor.Funcs{
		List: func(ctx context.Context, cli client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(*gatewayv1.ListenerSetList); ok {
				return errSimulatedCacheMiss
			}

			return cli.List(ctx, list, opts...)
		},
	})
	reconciler := &GatewayReconciler{Client: cli, Scheme: base.Scheme(), ControllerName: "test-controller"}

	err := reconciler.updateStatus(context.Background(), gateway, &config.ResolvedConfig{TunnelID: "tunnel"}, false)
	require.Error(t, err, "the reconcile must be retried")

	var updated gatewayv1.Gateway
	require.NoError(t, base.Get(context.Background(), client.ObjectKeyFromObject(gateway), &updated))
	require.NotNil(t, updated.Status.AttachedListenerSets)
	assert.Equal(t, int32(2), *updated.Status.AttachedListenerSets)
}

// TestAttachedRouteCount_IncompleteBindingIsUnfinished pins that a route bound
// through one listener while a sibling listener cannot be evaluated leaves the
// count unfinished: the route may attach to that listener too.
func TestAttachedRouteCount_IncompleteBindingIsUnfinished(t *testing.T) {
	t.Parallel()

	listeners := []gatewayv1.Listener{
		{Name: "same", Port: 80, Protocol: gatewayv1.HTTPProtocolType},
		{Name: "sel", Port: 8080, Protocol: gatewayv1.HTTPProtocolType, AllowedRoutes: teamSelectorRoutes()},
	}
	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
		Spec:       gatewayv1.GatewaySpec{GatewayClassName: "cloudflare-tunnel", Listeners: listeners},
	}
	ref := gatewayv1.ParentReference{Name: "gw"}
	route := attachedRoute("probe", nil, ref)
	route.Status.Parents = []gatewayv1.RouteParentStatus{
		parentStatusFor(ref, "default", "test-controller", metav1.ConditionTrue, string(gatewayv1.RouteReasonAccepted)),
	}

	base := setupGatewayFakeClient(gateway, route)
	cli := interceptor.NewClient(base, failReads[*corev1.Namespace]("", failing()))
	reconciler := &GatewayReconciler{Client: cli, Scheme: base.Scheme(), ControllerName: "test-controller"}

	_, err := reconciler.countAttachedRoutes(context.Background(), gateway)
	require.Error(t, err)

	ls := &gatewayv1.ListenerSet{
		ObjectMeta: metav1.ObjectMeta{Name: "ls", Namespace: "default"},
		Spec: gatewayv1.ListenerSetSpec{Listeners: []gatewayv1.ListenerEntry{
			{Name: "same", Port: 80, Protocol: gatewayv1.HTTPProtocolType},
			{Name: "sel", Port: 8080, Protocol: gatewayv1.HTTPProtocolType, AllowedRoutes: teamSelectorRoutes()},
		}},
	}
	lsRef := gatewayv1.ParentReference{Name: "ls", Kind: new(gatewayv1.Kind(kindListenerSet))}
	lsRoute := attachedRoute("probe", nil, lsRef)
	lsRoute.Status.Parents = []gatewayv1.RouteParentStatus{
		parentStatusFor(lsRef, "default", "test-controller", metav1.ConditionTrue, string(gatewayv1.RouteReasonAccepted)),
	}

	err = incrementListenerSetAttachedRoutes(context.Background(), routebinding.NewValidator(cli), "test-controller", ls,
		HTTPRouteWrapper{lsRoute}, map[gatewayv1.SectionName]int32{})
	require.Error(t, err)
}

// TestGatewayUpdateStatus_UnevaluatedListenerSetKeepsAttachedListenerSets pins
// that attachedListenerSets keeps the count already written, and the reconcile
// retries, when a ListenerSet's acceptance cannot be evaluated or its entries'
// TLS references cannot be read.
func TestGatewayUpdateStatus_UnevaluatedListenerSetKeepsAttachedListenerSets(t *testing.T) {
	t.Parallel()

	fromSelector := gatewayv1.NamespacesFromSelector
	fromSame := gatewayv1.NamespacesFromSame
	terminate := gatewayv1.TLSModeTerminate

	tests := map[string]struct {
		allowed *gatewayv1.ListenerNamespaces
		entry   gatewayv1.ListenerEntry
		failing interceptor.Funcs
	}{
		"unreadable ListenerSet namespace": {
			allowed: &gatewayv1.ListenerNamespaces{
				From: &fromSelector, Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"team": "a"}},
			},
			entry:   gatewayv1.ListenerEntry{Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType},
			failing: failReads[*corev1.Namespace]("", failing()),
		},
		"unreadable certificate": {
			allowed: &gatewayv1.ListenerNamespaces{From: &fromSame},
			entry: gatewayv1.ListenerEntry{
				Name: "https", Port: 443, Protocol: gatewayv1.HTTPSProtocolType,
				TLS: &gatewayv1.ListenerTLSConfig{
					Mode:            &terminate,
					CertificateRefs: []gatewayv1.SecretObjectReference{{Name: "cert"}},
				},
			},
			failing: failReads[*corev1.Secret]("", failing()),
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			gateway := &gatewayv1.Gateway{
				ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
				Spec: gatewayv1.GatewaySpec{
					GatewayClassName: "cloudflare-tunnel",
					Listeners:        httpListener(),
					AllowedListeners: &gatewayv1.AllowedListeners{Namespaces: tt.allowed},
				},
				Status: gatewayv1.GatewayStatus{AttachedListenerSets: new(int32(1))},
			}
			ls := &gatewayv1.ListenerSet{
				ObjectMeta: metav1.ObjectMeta{Name: "ls", Namespace: "default"},
				Spec: gatewayv1.ListenerSetSpec{
					ParentRef: gatewayv1.ParentGatewayReference{Name: "gw"},
					Listeners: []gatewayv1.ListenerEntry{tt.entry},
				},
			}

			base := setupGatewayFakeClient(gateway, ls)
			cli := interceptor.NewClient(base, tt.failing)
			reconciler := &GatewayReconciler{Client: cli, Scheme: base.Scheme(), ControllerName: "test-controller"}

			err := reconciler.updateStatus(context.Background(), gateway, &config.ResolvedConfig{TunnelID: "tunnel"}, false)
			require.Error(t, err, "the reconcile must be retried")

			var updated gatewayv1.Gateway
			require.NoError(t, base.Get(context.Background(), client.ObjectKeyFromObject(gateway), &updated))
			require.NotNil(t, updated.Status.AttachedListenerSets)
			assert.Equal(t, int32(1), *updated.Status.AttachedListenerSets)
		})
	}
}

// TestGatewayUpdateStatus_UnlistableRoutesKeepAttachedRoutes pins that a failed
// route List keeps the attachedRoutes already written and retries, for either
// route kind.
func TestGatewayUpdateStatus_UnlistableRoutesKeepAttachedRoutes(t *testing.T) {
	t.Parallel()

	lists := map[string]func(client.ObjectList) bool{
		"HTTPRoute": func(list client.ObjectList) bool { _, ok := list.(*gatewayv1.HTTPRouteList); return ok },
		"GRPCRoute": func(list client.ObjectList) bool { _, ok := list.(*gatewayv1.GRPCRouteList); return ok },
	}

	for name, fails := range lists {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			gateway := &gatewayv1.Gateway{
				ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
				Spec:       gatewayv1.GatewaySpec{GatewayClassName: "cloudflare-tunnel", Listeners: httpListener()},
				Status:     gatewayv1.GatewayStatus{Listeners: []gatewayv1.ListenerStatus{{Name: "http", AttachedRoutes: 1}}},
			}

			base := setupGatewayFakeClient(gateway)
			cli := interceptor.NewClient(base, interceptor.Funcs{
				List: func(ctx context.Context, cli client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					if fails(list) {
						return errSimulatedCacheMiss
					}

					return cli.List(ctx, list, opts...)
				},
			})
			reconciler := &GatewayReconciler{Client: cli, Scheme: base.Scheme(), ControllerName: "test-controller"}

			err := reconciler.updateStatus(context.Background(), gateway, &config.ResolvedConfig{TunnelID: "tunnel"}, false)
			require.Error(t, err, "the reconcile must be retried")

			var updated gatewayv1.Gateway
			require.NoError(t, base.Get(context.Background(), client.ObjectKeyFromObject(gateway), &updated))
			require.Len(t, updated.Status.Listeners, 1)
			assert.Equal(t, int32(1), updated.Status.Listeners[0].AttachedRoutes)
		})
	}
}

// siblingListenerSetObjects is a Selector-mode Gateway and two ListenerSets
// with the same listener, the one in team-b older, so it wins the conflict.
func siblingListenerSetObjects() []client.Object {
	fromSelector := gatewayv1.NamespacesFromSelector
	gc := managedGatewayClass()
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "infra"},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(gc.Name),
			AllowedListeners: &gatewayv1.AllowedListeners{Namespaces: &gatewayv1.ListenerNamespaces{
				From: &fromSelector, Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"team": "a"}},
			}},
		},
	}

	listenerSet := func(namespace string, created time.Time) *gatewayv1.ListenerSet {
		return &gatewayv1.ListenerSet{
			ObjectMeta: metav1.ObjectMeta{Name: "ls", Namespace: namespace, CreationTimestamp: metav1.NewTime(created)},
			Spec: gatewayv1.ListenerSetSpec{
				ParentRef: gatewayv1.ParentGatewayReference{Name: "gw", Namespace: new(gatewayv1.Namespace("infra"))},
				Listeners: []gatewayv1.ListenerEntry{{
					Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType, Hostname: new(gatewayv1.Hostname("a.example.com")),
				}},
			},
		}
	}

	now := time.Now()

	return []client.Object{
		gc, gw, listenerSet("team-a", now), listenerSet("team-b", now.Add(-time.Hour)),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-a", Labels: map[string]string{"team": "a"}}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-b", Labels: map[string]string{"team": "a"}}},
	}
}

// TestListenerSetReconciler_UnevaluatedSiblingIsRetried pins that a ListenerSet
// whose merged view leaves out a sibling that could not be evaluated is
// reconciled again: the sibling may conflict with it once it is read.
func TestListenerSetReconciler_UnevaluatedSiblingIsRetried(t *testing.T) {
	t.Parallel()

	scheme := newListenerSetScheme(t)
	require.NoError(t, corev1.AddToScheme(scheme))

	cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(siblingListenerSetObjects()...).
		WithStatusSubresource(&gatewayv1.ListenerSet{}, &gatewayv1.Gateway{}).
		WithInterceptorFuncs(failReads[*corev1.Namespace]("team-b", failing())).Build()

	r := &ListenerSetReconciler{Client: cli, Scheme: scheme, ControllerName: testListenerSetController}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "ls", Namespace: "team-a"}})
	require.Error(t, err, "the reconcile must be retried")

	updated := getListenerSet(t, cli, "ls", "team-a")
	accepted := meta.FindStatusCondition(updated.Status.Conditions, string(gatewayv1.ListenerSetConditionAccepted))
	require.NotNil(t, accepted)
	assert.Equal(t, metav1.ConditionTrue, accepted.Status, "the acceptance is written as computed, not Pending")
}

// TestRouteParentBinding_UnevaluatedSiblingListenerSetIsIncomplete pins that a
// route on a ListenerSet whose sibling could not be evaluated has an incomplete
// binding, so the sync retries: an older sibling may outrank its entry once it
// is read, and a newer one is treated the same way.
func TestRouteParentBinding_UnevaluatedSiblingListenerSetIsIncomplete(t *testing.T) {
	t.Parallel()

	scheme := newListenerSetScheme(t)
	require.NoError(t, corev1.AddToScheme(scheme))

	ref := gatewayv1.ParentReference{Name: "ls", Kind: new(gatewayv1.Kind(kindListenerSet))}

	tests := map[string]struct {
		routeNamespace, unreadable string
		fail                       bool
	}{
		"older sibling unreadable": {routeNamespace: "team-a", unreadable: "team-b", fail: true},
		"newer sibling unreadable": {routeNamespace: "team-b", unreadable: "team-a", fail: true},
		"sibling readable":         {routeNamespace: "team-b", unreadable: "team-a"},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			reads := &atomic.Bool{}
			reads.Store(tt.fail)

			cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(siblingListenerSetObjects()...).
				WithInterceptorFuncs(failReads[*corev1.Namespace](tt.unreadable, reads)).Build()

			routeInfo := &routebinding.RouteInfo{Name: "r", Namespace: tt.routeNamespace, Kind: routebinding.KindHTTPRoute}

			binding, err := resolveRouteParentBinding(context.Background(), cli, routebinding.NewValidator(cli),
				testListenerSetController, ref, tt.routeNamespace, routeInfo, nil)
			require.NoError(t, err)
			assert.True(t, binding.Result.Accepted)
			assert.Equal(t, tt.fail, binding.Result.Incomplete)
		})
	}
}
