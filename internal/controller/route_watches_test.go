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

var watchTestRoute = reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "app"}}

// watchTestParams wires a route controller's watches against a fake cluster in
// which every mapper has something to match: a managed class whose config
// names the credentials Secret, and a BackendTLSPolicy naming the CA ConfigMap.
// Every mapper that would enqueue answers with watchTestRoute.
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
		k8sClient:                    fakeClient,
		controllerName:               watchTestController,
		configResolver:               config.NewResolver(fakeClient, "default", cfmetrics.NewNoopCollector()),
		findRoutesForGateway:         enqueueRoute,
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

// updateReachesQueue sends one UPDATE event through the watch registered for
// newObj's type, predicates first and handler second, the way the controller
// does, and reports what landed in the queue.
func updateReachesQueue(t *testing.T, oldObj, newObj client.Object) []reconcile.Request {
	t.Helper()

	var watch *routeWatch

	for _, candidate := range routeWatches(watchTestParams()) {
		if reflect.TypeOf(candidate.object) == reflect.TypeOf(newObj) {
			watch = &candidate

			break
		}
	}

	if watch == nil {
		t.Fatalf("no route-controller watch registered for %T", newObj)
	}

	updateEvent := event.UpdateEvent{ObjectOld: oldObj, ObjectNew: newObj}
	for _, pred := range watch.predicates {
		if !pred.Update(updateEvent) {
			return nil
		}
	}

	queue := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[reconcile.Request]())
	defer queue.ShutDown()

	watch.handler.Update(context.Background(), updateEvent, queue)

	var queued []reconcile.Request

	for queue.Len() > 0 {
		item, _ := queue.Get()
		queued = append(queued, item)
		queue.Done(item)
	}

	return queued
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
