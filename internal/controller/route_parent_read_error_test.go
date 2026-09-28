package controller

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/api/v1alpha1"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/cfmetrics"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/config"
)

// TestBindRouteParents_UnreadableParentIsPending pins that a parent whose
// Gateway, ListenerSet or ListenerSet parent Gateway cannot be read is recorded
// as Pending. Left out of the binding results, the status writer's own read
// could succeed and report the route Accepted=True on a parent it never bound
// to. A parent that does not exist is absent and gets no entry.
func TestBindRouteParents_UnreadableParentIsPending(t *testing.T) {
	t.Parallel()

	listenerSetKind := gatewayv1.Kind(kindListenerSet)

	tests := []struct {
		name      string
		ref       gatewayv1.ParentReference
		failOn    func(obj client.Object, key client.ObjectKey) bool
		wantEntry bool
	}{
		{
			name: "gateway read fails",
			ref:  gatewayv1.ParentReference{Name: "other"},
			failOn: func(obj client.Object, key client.ObjectKey) bool {
				_, ok := obj.(*gatewayv1.Gateway)

				return ok && key.Name == "other"
			},
			wantEntry: true,
		},
		{
			name: "listenerset read fails",
			ref:  gatewayv1.ParentReference{Name: "extra", Kind: &listenerSetKind},
			failOn: func(obj client.Object, _ client.ObjectKey) bool {
				_, ok := obj.(*gatewayv1.ListenerSet)

				return ok
			},
			wantEntry: true,
		},
		{
			name: "listenerset parent gateway read fails",
			ref:  gatewayv1.ParentReference{Name: "extra", Kind: &listenerSetKind},
			failOn: func(obj client.Object, key client.ObjectKey) bool {
				_, ok := obj.(*gatewayv1.Gateway)

				return ok && key.Name == "other"
			},
			wantEntry: true,
		},
		{
			name:      "gateway does not exist",
			ref:       gatewayv1.ParentReference{Name: "missing"},
			failOn:    func(client.Object, client.ObjectKey) bool { return false },
			wantEntry: false,
		},
		{
			name:      "listenerset does not exist",
			ref:       gatewayv1.ParentReference{Name: "missing", Kind: &listenerSetKind},
			failOn:    func(client.Object, client.ObjectKey) bool { return false },
			wantEntry: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			syncer := unreadableParentSyncer(t, tt.failOn, []gatewayv1.ParentReference{{Name: "healthy"}, tt.ref})

			result, err := syncer.getRelevantHTTPRoutes(context.Background(), nil)
			require.NoError(t, err)
			require.Len(t, result.accepted, 1, "the healthy parent still admits the route")

			bindingResult, recorded := result.bindings["default/r"].bindingResults[1]
			require.Equal(t, tt.wantEntry, recorded)

			if tt.wantEntry {
				assert.False(t, bindingResult.Accepted)
				assert.Equal(t, gatewayv1.RouteReasonPending, bindingResult.Reason)
			}
		})
	}
}

// unreadableParentSyncer serves two Gateways that admit every route, healthy
// and other, a ListenerSet under other, and one route with the given
// parentRefs. Reads matching failOn fail with a transient error.
func unreadableParentSyncer(
	t *testing.T,
	failOn func(obj client.Object, key client.ObjectKey) bool,
	parentRefs []gatewayv1.ParentReference,
) *RouteSyncer {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, gatewayv1.Install(scheme))
	require.NoError(t, v1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))

	fromAll := gatewayv1.NamespacesFromAll
	allowAll := &gatewayv1.AllowedRoutes{Namespaces: &gatewayv1.RouteNamespaces{From: &fromAll}}
	listener := gatewayv1.Listener{Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType, AllowedRoutes: allowAll}

	gatewayFor := func(name string) *gatewayv1.Gateway {
		return &gatewayv1.Gateway{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: gatewayv1.GatewaySpec{
				GatewayClassName: "cloudflare-tunnel",
				Listeners:        []gatewayv1.Listener{listener},
				AllowedListeners: &gatewayv1.AllowedListeners{
					Namespaces: &gatewayv1.ListenerNamespaces{From: &fromAll},
				},
			},
		}
	}

	objects := []client.Object{
		&gatewayv1.GatewayClass{
			ObjectMeta: metav1.ObjectMeta{Name: "cloudflare-tunnel"},
			Spec:       gatewayv1.GatewayClassSpec{ControllerName: "cloudflare-tunnel"},
		},
		gatewayFor("healthy"),
		gatewayFor("other"),
		&gatewayv1.ListenerSet{
			ObjectMeta: metav1.ObjectMeta{Name: "extra", Namespace: "default"},
			Spec: gatewayv1.ListenerSetSpec{
				ParentRef: gatewayv1.ParentGatewayReference{Name: "other"},
				Listeners: []gatewayv1.ListenerEntry{
					{Name: "extra", Port: 8080, Protocol: gatewayv1.HTTPProtocolType, AllowedRoutes: allowAll},
				},
			},
		},
		&gatewayv1.HTTPRoute{
			ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "default"},
			Spec: gatewayv1.HTTPRouteSpec{
				CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: parentRefs},
			},
		},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cli client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if failOn(obj, key) {
					return errSimulatedCacheMiss
				}

				return cli.Get(ctx, key, obj, opts...)
			},
		}).Build()

	return NewRouteSyncer(fakeClient, scheme, "cluster.local", "cloudflare-tunnel",
		config.NewResolver(fakeClient, "default", cfmetrics.NewNoopCollector(), verifiedClaims()),
		cfmetrics.NewNoopCollector(), nil)
}

// TestSyncAllRoutes_UnevaluatedParentRequeues pins that a sync which recorded a
// parent as Pending because it could not be evaluated is retried, so the parent
// is re-evaluated once the read recovers. Here the parent's GatewayClass cannot
// be read. The retry must not depend on whether a sibling parent exists or
// already covers the route's hostnames: nothing else brings the sync back on a
// quiet cluster.
func TestSyncAllRoutes_UnevaluatedParentRequeues(t *testing.T) {
	t.Parallel()

	const classTunnel = "99999999-9999-4999-8999-999999999999"

	tests := []struct {
		name       string
		parentRefs []gatewayv1.ParentReference
		pendingRef int
	}{
		{
			name:       "a sibling parent covers the declared hostname",
			parentRefs: []gatewayv1.ParentReference{{Name: "shared-gw"}, {Name: "unreadable-class-gw"}},
			pendingRef: 1,
		},
		{
			name:       "the unevaluated parent is the only one",
			parentRefs: []gatewayv1.ParentReference{{Name: "unreadable-class-gw"}},
			pendingRef: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			route := partitionSyncRoute("probe", "shared-gw", "probe.example.com")
			route.Spec.ParentRefs = tt.parentRefs

			objects := append(partitionSyncObjects(t, classTunnel, false),
				&gatewayv1.GatewayClass{
					ObjectMeta: metav1.ObjectMeta{Name: "unreadable-class"},
					Spec: gatewayv1.GatewayClassSpec{
						ControllerName: skipTestControllerName,
						ParametersRef: &gatewayv1.ParametersReference{
							Group: config.ParametersRefGroup, Kind: config.ParametersRefKind, Name: "cfg",
						},
					},
				},
				&gatewayv1.Gateway{
					ObjectMeta: metav1.ObjectMeta{Name: "unreadable-class-gw", Namespace: "default"},
					Spec:       gatewayv1.GatewaySpec{GatewayClassName: "unreadable-class", Listeners: httpListener()},
				},
				route,
			)

			var recovered atomic.Bool

			syncer := partitionSyncSyncerFor(t, newRecordingTunnelAPI(t), objects, interceptor.Funcs{
				Get: func(ctx context.Context, cli client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if _, ok := obj.(*gatewayv1.GatewayClass); ok && key.Name == "unreadable-class" && !recovered.Load() {
						return errSimulatedCacheMiss
					}

					return cli.Get(ctx, key, obj, opts...)
				},
			})

			result, syncResult, err := syncer.SyncAllRoutes(context.Background())
			require.NoError(t, err)
			require.NotNil(t, syncResult)

			bindingResult, recorded := syncResult.HTTPRouteBindings["default/probe"].bindingResults[tt.pendingRef]
			require.True(t, recorded)
			assert.Equal(t, gatewayv1.RouteReasonPending, bindingResult.Reason)

			assert.Equal(t, apiErrorRequeueDelay, result.RequeueAfter, "a Pending parent must be retried")

			recovered.Store(true)

			result, syncResult, err = syncer.SyncAllRoutes(context.Background())
			require.NoError(t, err)

			assert.True(t, syncResult.HTTPRouteBindings["default/probe"].bindingResults[tt.pendingRef].Accepted,
				"the retried sync binds the parent once its read recovers")
			assert.Zero(t, result.RequeueAfter)
		})
	}
}
