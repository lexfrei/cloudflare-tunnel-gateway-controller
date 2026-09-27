package controller

import (
	"context"
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/api/v1alpha1"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/cfmetrics"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/config"
)

const watchTestController = "test-controller"

var (
	watchTestRoute = reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "app"}}
	// watchDirectRoute is what the per-Gateway mapper answers, told apart from
	// the full relevant set so a test can see which path a Gateway event took.
	watchDirectRoute = reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "direct"}}
)

// watchTestParams wires a route controller's watches against a fake cluster in
// which every mapper has something to match: a managed class whose config
// names the credentials Secret, and a BackendTLSPolicy naming the CA ConfigMap.
// Every mapper that would enqueue answers with watchTestRoute, except the
// per-Gateway one, which answers with watchDirectRoute.
func watchTestParams() *routeControllerSetupParams {
	gatewayClassConfig := &v1alpha1.GatewayClassConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "test-config"},
		Spec: v1alpha1.GatewayClassConfigSpec{
			CloudflareCredentialsSecretRef: v1alpha1.SecretReference{Name: "cf-credentials", Namespace: "default"},
			TunnelID:                       "test-tunnel",
		},
	}
	gatewayClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "cloudflare-tunnel"},
		Spec: gatewayv1.GatewayClassSpec{
			ControllerName: watchTestController,
			ParametersRef: &gatewayv1.ParametersReference{
				Group: config.ParametersRefGroup,
				Kind:  config.ParametersRefKind,
				Name:  "test-config",
			},
		},
	}
	policy := &gatewayv1.BackendTLSPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "tls", Namespace: "default"},
		Spec: gatewayv1.BackendTLSPolicySpec{
			TargetRefs: []gatewayv1.LocalPolicyTargetReferenceWithSectionName{{
				LocalPolicyTargetReference: gatewayv1.LocalPolicyTargetReference{Kind: serviceKind, Name: "backend"},
			}},
			Validation: gatewayv1.BackendTLSPolicyValidation{
				CACertificateRefs: []gatewayv1.LocalObjectReference{{Kind: configMapKind, Name: "ca"}},
				Hostname:          "backend.example.com",
			},
		},
	}

	fakeClient := setupMapperFakeClient(gatewayClassConfig, gatewayClass, policy)
	enqueueRoute := func(context.Context, client.Object) []reconcile.Request {
		return []reconcile.Request{watchTestRoute}
	}

	return &routeControllerSetupParams{
		k8sClient:      fakeClient,
		controllerName: watchTestController,
		configResolver: config.NewResolver(fakeClient, "default", cfmetrics.NewNoopCollector()),
		findRoutesForGateway: func(context.Context, client.Object) []reconcile.Request {
			return []reconcile.Request{watchDirectRoute}
		},
		findRoutesForListenerSet:     enqueueRoute,
		findRoutesForRefGrant:        enqueueRoute,
		findRoutesForService:         enqueueRoute,
		findRoutesForEndpointSlice:   enqueueRoute,
		findRoutesForExternalBackend: enqueueRoute,
		watchBackendTLS:              true,
		getAllRelevantRoutes: func(context.Context) []reconcile.Request {
			return []reconcile.Request{watchTestRoute}
		},
	}
}

// updateReachesQueue sends one UPDATE event through every watch registered
// for newObj's type, predicates first and handler second, the way the
// controller does, and reports what landed in the shared queue.
func updateReachesQueue(t *testing.T, oldObj, newObj client.Object) []reconcile.Request {
	t.Helper()

	queue := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[reconcile.Request]())
	defer queue.ShutDown()

	updateEvent := event.UpdateEvent{ObjectOld: oldObj, ObjectNew: newObj}
	registered := false

	for _, watch := range routeWatches(watchTestParams()) {
		if reflect.TypeOf(watch.object) != reflect.TypeOf(newObj) {
			continue
		}

		registered = true

		if passesUpdate(watch, updateEvent) {
			watch.handler.Update(context.Background(), updateEvent, queue)
		}
	}

	if !registered {
		t.Fatalf("no route-controller watch registered for %T", newObj)
	}

	var queued []reconcile.Request

	for queue.Len() > 0 {
		item, _ := queue.Get()
		queued = append(queued, item)
		queue.Done(item)
	}

	return queued
}

func passesUpdate(watch routeWatch, updateEvent event.UpdateEvent) bool {
	for _, pred := range watch.predicates {
		if !pred.Update(updateEvent) {
			return false
		}
	}

	return true
}

// TestRouteWatches_UpdateWithoutGenerationReachesQueue pins that an in-place
// edit of a kind the apiserver never stamps a generation on still wakes the
// route controllers. Rotating a credential Secret, editing a Service port or
// replacing a CA bundle is an update with generation 0 on both sides.
func TestRouteWatches_UpdateWithoutGenerationReachesQueue(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		oldObj client.Object
		newObj client.Object
	}{
		{
			name: "credentials Secret rotated in place",
			oldObj: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "cf-credentials", Namespace: "default", ResourceVersion: "1"},
				Data:       map[string][]byte{"api-token": []byte("old")},
			},
			newObj: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "cf-credentials", Namespace: "default", ResourceVersion: "2"},
				Data:       map[string][]byte{"api-token": []byte("new")},
			},
		},
		{
			name: "backend Service port edited",
			oldObj: &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: "backend", Namespace: "default", ResourceVersion: "1"},
				Spec:       corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 80}}},
			},
			newObj: &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: "backend", Namespace: "default", ResourceVersion: "2"},
				Spec:       corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 443, AppProtocol: new("https")}}},
			},
		},
		{
			name: "BackendTLSPolicy CA ConfigMap replaced",
			oldObj: &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: "ca", Namespace: "default", ResourceVersion: "1"},
				Data:       map[string]string{"ca.crt": "old"},
			},
			newObj: &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: "ca", Namespace: "default", ResourceVersion: "2"},
				Data:       map[string]string{"ca.crt": "new"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, []reconcile.Request{watchTestRoute}, updateReachesQueue(t, tt.oldObj, tt.newObj))
		})
	}
}

// TestRouteWatches_GenerationGateKeptWhereGenerationMoves pins the other half:
// kinds whose spec edits do bump generation keep the gate, so a status-only or
// metadata-only update of them still enqueues nothing.
func TestRouteWatches_GenerationGateKeptWhereGenerationMoves(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		oldObj client.Object
		newObj client.Object
	}{
		{
			name: "EndpointSlice relabelled",
			oldObj: &discoveryv1.EndpointSlice{
				ObjectMeta: metav1.ObjectMeta{Name: "backend-abc", Namespace: "default", Generation: 3},
			},
			newObj: &discoveryv1.EndpointSlice{
				ObjectMeta: metav1.ObjectMeta{
					Name: "backend-abc", Namespace: "default", Generation: 3,
					Labels: map[string]string{"example.com/touched": "yes"},
				},
			},
		},
		{
			name: "ReferenceGrant relabelled",
			oldObj: &gatewayv1beta1.ReferenceGrant{
				ObjectMeta: metav1.ObjectMeta{Name: "grant", Namespace: "default", Generation: 1},
			},
			newObj: &gatewayv1beta1.ReferenceGrant{
				ObjectMeta: metav1.ObjectMeta{
					Name: "grant", Namespace: "default", Generation: 1,
					Labels: map[string]string{"example.com/touched": "yes"},
				},
			},
		},
		{
			name: "GatewayClassConfig status written",
			oldObj: &v1alpha1.GatewayClassConfig{
				ObjectMeta: metav1.ObjectMeta{Name: "test-config", Generation: 2},
			},
			newObj: &v1alpha1.GatewayClassConfig{
				ObjectMeta: metav1.ObjectMeta{Name: "test-config", Generation: 2},
				Status: v1alpha1.GatewayClassConfigStatus{
					Conditions: []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Ready"}},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			assert.Empty(t, updateReachesQueue(t, tt.oldObj, tt.newObj))
		})
	}
}

// TestRouteWatches_GatewayClassUpdateReachesQueue pins that a change to one of
// this controller's GatewayClasses wakes the route controllers: acceptance keys
// on the class, and a Gateway's own events do not carry a class edit.
func TestRouteWatches_GatewayClassUpdateReachesQueue(t *testing.T) {
	t.Parallel()

	class := func(controllerName, cfgName string, generation int64) *gatewayv1.GatewayClass {
		return &gatewayv1.GatewayClass{
			ObjectMeta: metav1.ObjectMeta{Name: "cloudflare-tunnel", Generation: generation},
			Spec: gatewayv1.GatewayClassSpec{
				ControllerName: gatewayv1.GatewayController(controllerName),
				ParametersRef: &gatewayv1.ParametersReference{
					Group: config.ParametersRefGroup, Kind: config.ParametersRefKind, Name: cfgName,
				},
			},
		}
	}

	assert.Equal(t, []reconcile.Request{watchTestRoute},
		updateReachesQueue(t, class(watchTestController, "old", 1), class(watchTestController, "new", 2)),
		"a parametersRef edit on a managed class must enqueue the routes")

	assert.Empty(t,
		updateReachesQueue(t, class("example.com/other", "old", 1), class("example.com/other", "new", 2)),
		"a class owned by another controller enqueues nothing")

	withStatus := class(watchTestController, "old", 1)
	withStatus.Status.Conditions = []metav1.Condition{
		{Type: string(gatewayv1.GatewayClassConditionStatusAccepted), Status: metav1.ConditionTrue, Reason: "Accepted"},
	}
	assert.Empty(t, updateReachesQueue(t, class(watchTestController, "old", 1), withStatus),
		"the class status write that follows every class reconcile enqueues nothing")
}

// TestRouteWatches_GatewayConditionFlipReachesQueue pins that a Gateway status
// verdict change wakes the routes. A refusal that clears, or a data plane that
// goes away, flips the Gateway's conditions without touching its spec. The
// flip runs the full relevant set rather than the Gateway's direct routes,
// because a Gateway whose routes all attach through a ListenerSet has none.
func TestRouteWatches_GatewayConditionFlipReachesQueue(t *testing.T) {
	t.Parallel()

	gateway := func(className string, generation int64, accepted metav1.ConditionStatus, reason string,
		attached int32,
	) *gatewayv1.Gateway {
		return &gatewayv1.Gateway{
			ObjectMeta: metav1.ObjectMeta{Name: "edge", Namespace: "default", Generation: generation},
			Spec:       gatewayv1.GatewaySpec{GatewayClassName: gatewayv1.ObjectName(className)},
			Status: gatewayv1.GatewayStatus{
				Conditions: []metav1.Condition{{
					Type: string(gatewayv1.GatewayConditionAccepted), Status: accepted, Reason: reason,
					ObservedGeneration: 4, Message: "verdict for " + reason,
				}},
				Listeners: []gatewayv1.ListenerStatus{{Name: "http", AttachedRoutes: attached}},
			},
		}
	}

	const ours = "cloudflare-tunnel"

	accepted := gateway(ours, 4, metav1.ConditionTrue, "Accepted", 1)
	refused := gateway(ours, 4, metav1.ConditionFalse, "InvalidParameters", 1)

	assert.Equal(t, []reconcile.Request{watchTestRoute}, updateReachesQueue(t, refused, accepted),
		"a refusal clearing must run the full route sync")
	assert.Equal(t, []reconcile.Request{watchTestRoute}, updateReachesQueue(t, accepted, refused),
		"a Gateway losing its data plane must run the full route sync")

	assert.Equal(t, []reconcile.Request{watchDirectRoute},
		updateReachesQueue(t, accepted, gateway(ours, 5, metav1.ConditionTrue, "Accepted", 1)),
		"a spec edit enqueues the Gateway's own routes, as before")

	assert.Empty(t, updateReachesQueue(t, accepted, gateway(ours, 4, metav1.ConditionTrue, "Accepted", 2)),
		"an attachedRoutes count written after a route sync must not enqueue the routes again")

	reworded := gateway(ours, 4, metav1.ConditionFalse, "InvalidParameters", 1)
	reworded.Status.Conditions[0].Message = "same verdict, new text"
	reworded.Status.Conditions[0].LastTransitionTime = metav1.Now()
	assert.Empty(t, updateReachesQueue(t, refused, reworded),
		"a condition rewritten with the same type, status and reason enqueues nothing")

	assert.Empty(t, updateReachesQueue(t,
		gateway("someone-elses-class", 4, metav1.ConditionFalse, "Pending", 0),
		gateway("someone-elses-class", 4, metav1.ConditionTrue, "Accepted", 0)),
		"another controller's Gateway flipping its verdict enqueues nothing")
}
