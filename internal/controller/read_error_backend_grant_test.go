package controller

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
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
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/cfmetrics"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/ingress"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
)

// TestRouteStatus_UndecidedRefKeepsResolvedRefs pins that a sync whose backend
// reference could not be evaluated, in the ingress builder or in the proxy
// converter, leaves the route's ResolvedRefs condition as the previous sync
// wrote it, whichever way that went.
func TestRouteStatus_UndecidedRefKeepsResolvedRefs(t *testing.T) {
	t.Parallel()

	denied := []ingress.BackendRefError{{
		RouteNamespace: "ns", RouteName: "r", BackendName: "api", BackendNS: "backend",
		Reason: string(gatewayv1.RouteReasonRefNotPermitted), Message: "denied",
	}}
	undecided := []ingress.BackendRefError{{
		RouteNamespace: "ns", RouteName: "r", BackendName: "api", BackendNS: "backend", Undecided: true,
	}}
	undecidedDiag := []proxy.RouteDiagnostic{{
		Kind: "HTTPRoute", Namespace: "ns", Name: "r",
		Target: proxy.DiagnosticResolvedRefs, Reason: routeReasonRefsUndecided,
	}}

	tests := map[string]struct {
		priorRefs []ingress.BackendRefError
		refs      []ingress.BackendRefError
		diags     []proxy.RouteDiagnostic
	}{
		"builder, prior denial":   {priorRefs: denied, refs: undecided},
		"builder, prior resolved": {refs: undecided},
		"proxy, prior denial":     {priorRefs: denied, diags: undecidedDiag},
		"proxy, prior resolved":   {diags: undecidedDiag},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			gatewayClass, gateway := routeStatusTransitionFixtures()
			route := &gatewayv1.HTTPRoute{
				ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "ns", Generation: 1},
				Spec: gatewayv1.HTTPRouteSpec{
					CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: []gatewayv1.ParentReference{{Name: "gw"}}},
				},
			}

			cli := fake.NewClientBuilder().WithScheme(newListenerSetScheme(t)).
				WithObjects(gatewayClass, gateway, route).WithStatusSubresource(route).Build()

			ctx := context.Background()
			routeKey := types.NamespacedName{Name: "r", Namespace: "ns"}
			resolvedRefs := func() *metav1.Condition {
				var current gatewayv1.HTTPRoute
				require.NoError(t, cli.Get(ctx, routeKey, &current))
				require.Len(t, current.Status.Parents, 1)

				return meta.FindStatusCondition(current.Status.Parents[0].Conditions, string(gatewayv1.RouteConditionResolvedRefs))
			}

			params := &routeStatusUpdateParams{k8sClient: cli, controllerName: "test", reconciledGeneration: 1}
			require.NoError(t, updateRouteStatusGeneric(ctx, params, routeKey, newHTTPRouteAccessor, routeBindingInfo{}, tt.priorRefs, nil))

			before := resolvedRefs()
			require.NotNil(t, before)

			params.diagnostics = tt.diags
			require.NoError(t, updateRouteStatusGeneric(ctx, params, routeKey, newHTTPRouteAccessor, routeBindingInfo{}, tt.refs, nil))

			assert.Equal(t, *before, *resolvedRefs())
		})
	}
}

// crossNamespaceBackendRoute is a route on shared-gw whose only backend lives
// in the backend namespace.
func crossNamespaceBackendRoute() *gatewayv1.HTTPRoute {
	route := partitionSyncRoute("probe", "shared-gw", "probe.example.com")
	route.Spec.Rules[0].BackendRefs[0].Namespace = new(gatewayv1.Namespace("backend"))

	return route
}

func backendServiceGrant() *gatewayv1beta1.ReferenceGrant {
	return &gatewayv1beta1.ReferenceGrant{
		ObjectMeta: metav1.ObjectMeta{Name: "grant", Namespace: "backend"},
		Spec: gatewayv1beta1.ReferenceGrantSpec{
			From: []gatewayv1beta1.ReferenceGrantFrom{{Group: gatewayv1.GroupName, Kind: "HTTPRoute", Namespace: "default"}},
			To:   []gatewayv1beta1.ReferenceGrantTo{{Group: "", Kind: "Service"}},
		},
	}
}

// TestSyncAllRoutes_UnlistableBackendGrantRequeues pins that a sync whose
// backend ReferenceGrants cannot be listed is retried, and settles once the
// List recovers.
func TestSyncAllRoutes_UnlistableBackendGrantRequeues(t *testing.T) {
	t.Parallel()

	var fail atomic.Bool

	objects := append(partitionSyncObjects(t, "99999999-9999-4999-8999-999999999999", false),
		crossNamespaceBackendRoute(), backendServiceGrant(),
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "backend"}})
	syncer := partitionSyncSyncerFor(t, newRecordingTunnelAPI(t), objects,
		failLists[*gatewayv1beta1.ReferenceGrantList](&fail))

	fail.Store(true)

	result, syncResult, err := syncer.SyncAllRoutes(context.Background())
	require.NoError(t, err)
	require.Len(t, syncResult.HTTPFailedRefs, 1)
	assert.True(t, syncResult.HTTPFailedRefs[0].Undecided)
	assert.Equal(t, apiErrorRequeueDelay, result.RequeueAfter)

	fail.Store(false)

	result, syncResult, err = syncer.SyncAllRoutes(context.Background())
	require.NoError(t, err)
	assert.Empty(t, syncResult.HTTPFailedRefs)
	assert.Zero(t, result.RequeueAfter)
}

// TestPushPartitionConfigs_UndecidedGrantSkipsPush pins that a partition
// whose backend ReferenceGrants could not be evaluated, by the ingress builder
// or by the proxy converter, is not pushed: the data plane keeps serving the
// config it has, and the sync is retried. A mirror the converter could not
// evaluate is reported undecided, never RefNotPermitted.
func TestPushPartitionConfigs_UndecidedGrantSkipsPush(t *testing.T) {
	t.Parallel()

	mirrorRoute := func() *gatewayv1.HTTPRoute {
		route := pushGrantRoute()
		route.Spec.Rules[0].Filters = []gatewayv1.HTTPRouteFilter{{
			Type: gatewayv1.HTTPRouteFilterRequestMirror,
			RequestMirror: &gatewayv1.HTTPRequestMirrorFilter{BackendRef: gatewayv1.BackendObjectReference{
				Name: "shadow", Namespace: new(gatewayv1.Namespace("mirror")), Port: new(gatewayv1.PortNumber(80)),
			}},
		}}

		return route
	}

	tests := map[string]struct {
		route      *gatewayv1.HTTPRoute
		failedRefs []ingress.BackendRefError
		failList   bool
		wantPush   bool
	}{
		"decided": {route: pushGrantRoute(), wantPush: true},
		"builder undecided": {route: pushGrantRoute(), failedRefs: []ingress.BackendRefError{{
			RouteNamespace: "default", RouteName: "probe", BackendName: "svc", BackendNS: "default", Undecided: true,
		}}},
		"converter undecided": {route: mirrorRoute(), failList: true},
		"undecided ref of a route in another partition": {
			route: pushGrantRoute(), wantPush: true,
			failedRefs: []ingress.BackendRefError{{
				RouteNamespace: "default", RouteName: "elsewhere", BackendName: "svc", BackendNS: "backend", Undecided: true,
			}},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var pushes atomic.Int32

			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				pushes.Add(1)
				writer.WriteHeader(http.StatusOK)
			}))
			t.Cleanup(server.Close)

			var fail atomic.Bool

			fail.Store(tt.failList)

			gc := managedGatewayClass()
			gateway := &gatewayv1.Gateway{
				ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
				Spec: gatewayv1.GatewaySpec{
					GatewayClassName: gatewayv1.ObjectName(gc.Name),
					Listeners: []gatewayv1.Listener{{
						Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType,
						Hostname: new(gatewayv1.Hostname("probe.example.com")),
					}},
				},
			}

			scheme := runtime.NewScheme()
			require.NoError(t, gatewayv1.Install(scheme))
			require.NoError(t, gatewayv1beta1.Install(scheme))
			require.NoError(t, corev1.AddToScheme(scheme))

			cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(gc, gateway).
				WithInterceptorFuncs(failLists[*gatewayv1beta1.ReferenceGrantList](&fail)).Build()

			params := syncUpdateParams{
				routeSyncer:    &RouteSyncer{ClusterDomain: "cluster.local", Metrics: cfmetrics.NewNoopCollector()},
				proxySyncer:    NewProxySyncer("cluster.local", "shared-token", testListenerSetController, cli, slog.Default()),
				proxyEndpoints: []string{server.URL + "/config"},
				pushProxy:      true,
			}

			diags, outcome := pushPartitionConfigs(context.Background(), slog.Default(), &params, &SyncResult{
				Partitions:     []routePartition{{Key: sharedPartitionKey, HTTPRoutes: []gatewayv1.HTTPRoute{*tt.route}}},
				HTTPFailedRefs: tt.failedRefs,
			})

			assert.Equal(t, tt.wantPush, pushes.Load() > 0)
			assert.Equal(t, !tt.wantPush, outcome.undecided)
			assert.False(t, slices.ContainsFunc(diags, func(diag proxy.RouteDiagnostic) bool {
				return diag.Reason == string(gatewayv1.RouteReasonRefNotPermitted)
			}), "an unread grant is not a denial")

			if tt.failList {
				assert.True(t, slices.ContainsFunc(diags, func(diag proxy.RouteDiagnostic) bool {
					return diag.Name == "probe" && diag.Reason == routeReasonRefsUndecided
				}))
			}
		})
	}
}

func pushGrantRoute() *gatewayv1.HTTPRoute {
	return partitionSyncRoute("probe", "gw", "probe.example.com")
}

// TestSyncOutcome_UndecidedPushRequeues pins that a partition left unpushed on
// an undecided reference brings the sync back, without lengthening a sooner
// requeue.
func TestSyncOutcome_UndecidedPushRequeues(t *testing.T) {
	t.Parallel()

	result, err := syncOutcome(ctrl.Result{}, pushOutcome{undecided: true}, nil, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, apiErrorRequeueDelay, result.RequeueAfter)

	result, err = syncOutcome(ctrl.Result{}, pushOutcome{undecided: true, lostRace: true}, nil, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, lostRacePushRequeueDelay, result.RequeueAfter)

	result, err = syncOutcome(ctrl.Result{RequeueAfter: time.Hour}, pushOutcome{undecided: true}, nil, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, apiErrorRequeueDelay, result.RequeueAfter)
}

// TestPushPartitionConfigs_UndecidedMarksOnlyUnreadGrants pins that a grant
// read failure relabels only the RefNotPermitted the converter derived from
// it: a route whose grants read fine keeps its denial, and a mirror dropped
// for another reason keeps that reason.
func TestPushPartitionConfigs_UndecidedMarksOnlyUnreadGrants(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	mirrorRule := func(namespace string, port gatewayv1.PortNumber) gatewayv1.HTTPRouteRule {
		return gatewayv1.HTTPRouteRule{
			Filters: []gatewayv1.HTTPRouteFilter{{
				Type: gatewayv1.HTTPRouteFilterRequestMirror,
				RequestMirror: &gatewayv1.HTTPRequestMirrorFilter{BackendRef: gatewayv1.BackendObjectReference{
					Name: "shadow", Namespace: new(gatewayv1.Namespace(namespace)), Port: &port,
				}},
			}},
			BackendRefs: []gatewayv1.HTTPBackendRef{{BackendRef: gatewayv1.BackendRef{
				BackendObjectReference: gatewayv1.BackendObjectReference{Name: "svc", Port: new(gatewayv1.PortNumber(80))},
			}}},
		}
	}
	route := func(namespace, hostname string, rules ...gatewayv1.HTTPRouteRule) gatewayv1.HTTPRoute {
		return gatewayv1.HTTPRoute{
			ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: namespace},
			Spec: gatewayv1.HTTPRouteSpec{
				CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: []gatewayv1.ParentReference{{
					Name: "gw", Namespace: new(gatewayv1.Namespace("default")),
				}}},
				Hostnames: []gatewayv1.Hostname{gatewayv1.Hostname(hostname)},
				Rules:     rules,
			},
		}
	}

	gc := managedGatewayClass()
	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(gc.Name),
			Listeners: []gatewayv1.Listener{{
				Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType,
				AllowedRoutes: &gatewayv1.AllowedRoutes{Namespaces: &gatewayv1.RouteNamespaces{From: namespacesFromAllPtr()}},
			}},
		},
	}

	scheme := runtime.NewScheme()
	require.NoError(t, gatewayv1.Install(scheme))
	require.NoError(t, gatewayv1beta1.Install(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))

	cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(gc, gateway).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, cli client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				listOpts := &client.ListOptions{}
				listOpts.ApplyOptions(opts)

				if _, ok := list.(*gatewayv1beta1.ReferenceGrantList); ok && listOpts.Namespace == "unreadable" {
					return errSimulatedCacheMiss
				}

				return cli.List(ctx, list, opts...)
			},
		}).Build()

	params := syncUpdateParams{
		routeSyncer:    &RouteSyncer{ClusterDomain: "cluster.local", Metrics: cfmetrics.NewNoopCollector()},
		proxySyncer:    NewProxySyncer("cluster.local", "shared-token", testListenerSetController, cli, slog.Default()),
		proxyEndpoints: []string{server.URL + "/config"},
		pushProxy:      true,
	}

	diags, outcome := pushPartitionConfigs(context.Background(), slog.Default(), &params, &SyncResult{
		Partitions: []routePartition{{Key: sharedPartitionKey, HTTPRoutes: []gatewayv1.HTTPRoute{
			route("team-a", "a.example.com", mirrorRule("unreadable", 80), mirrorRule("unreadable", 0)),
			route("team-b", "b.example.com", mirrorRule("readable", 80), mirrorRule("readable", 0)),
		}}},
	})
	require.True(t, outcome.undecided)

	reasons := func(namespace string) []string {
		var out []string

		for i := range diags {
			if diags[i].Namespace == namespace && diags[i].Target == proxy.DiagnosticResolvedRefs {
				out = append(out, diags[i].Reason)
			}
		}

		return out
	}

	assert.ElementsMatch(t, []string{routeReasonRefsUndecided, string(gatewayv1.RouteReasonUnsupportedValue)}, reasons("team-a"))
	assert.ElementsMatch(t, []string{
		string(gatewayv1.RouteReasonRefNotPermitted), string(gatewayv1.RouteReasonUnsupportedValue),
	}, reasons("team-b"))
}
