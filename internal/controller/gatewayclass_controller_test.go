package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func TestGatewayClassReconciler_Reconcile_NotFound(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, gatewayv1.Install(scheme))

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		Build()

	r := &GatewayClassReconciler{
		Client:         fakeClient,
		Scheme:         scheme,
		ControllerName: "test-controller",
	}

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name: "non-existent-class",
		},
	}

	result, err := r.Reconcile(context.Background(), req)

	assert.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, result)
}

func TestGatewayClassReconciler_Reconcile_WrongControllerName(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, gatewayv1.Install(scheme))

	gatewayClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: "other-class",
		},
		Spec: gatewayv1.GatewayClassSpec{
			ControllerName: "other-controller",
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(gatewayClass).
		WithStatusSubresource(gatewayClass).
		Build()

	r := &GatewayClassReconciler{
		Client:         fakeClient,
		Scheme:         scheme,
		ControllerName: "test-controller",
	}

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name: "other-class",
		},
	}

	result, err := r.Reconcile(context.Background(), req)

	assert.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, result)
}

func TestGatewayClassReconciler_Reconcile_MatchingController(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, gatewayv1.Install(scheme))

	gatewayClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "cloudflare-tunnel",
			Generation: 1,
		},
		Spec: gatewayv1.GatewayClassSpec{
			ControllerName: "cloudflare-tunnel-controller",
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(gatewayClass).
		WithStatusSubresource(gatewayClass).
		Build()

	r := &GatewayClassReconciler{
		Client:         fakeClient,
		Scheme:         scheme,
		ControllerName: "cloudflare-tunnel-controller",
	}

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name: "cloudflare-tunnel",
		},
	}

	result, err := r.Reconcile(context.Background(), req)

	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, result)

	// Verify status conditions were set
	var updatedClass gatewayv1.GatewayClass
	err = fakeClient.Get(context.Background(), types.NamespacedName{Name: "cloudflare-tunnel"}, &updatedClass)
	require.NoError(t, err)

	require.Len(t, updatedClass.Status.Conditions, 2)

	var acceptedCondition, supportedVersionCondition *metav1.Condition
	for i := range updatedClass.Status.Conditions {
		switch updatedClass.Status.Conditions[i].Type {
		case string(gatewayv1.GatewayClassConditionStatusAccepted):
			acceptedCondition = &updatedClass.Status.Conditions[i]
		case string(gatewayv1.GatewayClassConditionStatusSupportedVersion):
			supportedVersionCondition = &updatedClass.Status.Conditions[i]
		}
	}

	require.NotNil(t, acceptedCondition)
	assert.Equal(t, metav1.ConditionTrue, acceptedCondition.Status)
	assert.Equal(t, string(gatewayv1.GatewayClassReasonAccepted), acceptedCondition.Reason)
	assert.Equal(t, int64(1), acceptedCondition.ObservedGeneration)
	assert.Contains(t, acceptedCondition.Message, "accepted by cloudflare-tunnel controller")

	require.NotNil(t, supportedVersionCondition)
	assert.Equal(t, metav1.ConditionTrue, supportedVersionCondition.Status)
	assert.Equal(t, string(gatewayv1.GatewayClassReasonSupportedVersion), supportedVersionCondition.Reason)
	assert.Equal(t, int64(1), supportedVersionCondition.ObservedGeneration)
}

func TestGatewayClassReconciler_SetAcceptedConditions(t *testing.T) {
	t.Parallel()

	r := &GatewayClassReconciler{
		ControllerName: "test-controller",
	}

	gatewayClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-class",
			Generation: 5,
		},
	}

	r.setAcceptedConditions(gatewayClass)

	require.Len(t, gatewayClass.Status.Conditions, 2)

	var accepted, supportedVersion *metav1.Condition
	for i := range gatewayClass.Status.Conditions {
		switch gatewayClass.Status.Conditions[i].Type {
		case string(gatewayv1.GatewayClassConditionStatusAccepted):
			accepted = &gatewayClass.Status.Conditions[i]
		case string(gatewayv1.GatewayClassConditionStatusSupportedVersion):
			supportedVersion = &gatewayClass.Status.Conditions[i]
		}
	}

	require.NotNil(t, accepted)
	assert.Equal(t, metav1.ConditionTrue, accepted.Status)
	assert.Equal(t, int64(5), accepted.ObservedGeneration)

	require.NotNil(t, supportedVersion)
	assert.Equal(t, metav1.ConditionTrue, supportedVersion.Status)
	assert.Equal(t, int64(5), supportedVersion.ObservedGeneration)
}

func TestGatewayClassReconciler_UpdateStatus_ControllerMismatch(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, gatewayv1.Install(scheme))

	// GatewayClass with a different controller name
	gatewayClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: "other-class",
		},
		Spec: gatewayv1.GatewayClassSpec{
			ControllerName: "other-controller",
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(gatewayClass).
		WithStatusSubresource(gatewayClass).
		Build()

	r := &GatewayClassReconciler{
		Client:         fakeClient,
		Scheme:         scheme,
		ControllerName: "test-controller",
	}

	// updateStatus should silently return nil for non-matching controllers
	err := r.updateStatus(context.Background(), types.NamespacedName{Name: "other-class"})
	assert.NoError(t, err)

	// Verify no conditions were set
	var updatedClass gatewayv1.GatewayClass
	err = fakeClient.Get(context.Background(), types.NamespacedName{Name: "other-class"}, &updatedClass)
	require.NoError(t, err)
	assert.Empty(t, updatedClass.Status.Conditions)
}

func TestGatewayClassReconciler_UpdateStatus_NotFound(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, gatewayv1.Install(scheme))

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		Build()

	r := &GatewayClassReconciler{
		Client:         fakeClient,
		Scheme:         scheme,
		ControllerName: "test-controller",
	}

	err := r.updateStatus(context.Background(), types.NamespacedName{Name: "non-existent"})
	assert.Error(t, err)
}

func TestGatewayClassReconciler_Reconcile_IdempotentStatusUpdate(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, gatewayv1.Install(scheme))

	gatewayClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "cloudflare-tunnel",
			Generation: 2,
		},
		Spec: gatewayv1.GatewayClassSpec{
			ControllerName: "test-controller",
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(gatewayClass).
		WithStatusSubresource(gatewayClass).
		Build()

	r := &GatewayClassReconciler{
		Client:         fakeClient,
		Scheme:         scheme,
		ControllerName: "test-controller",
	}

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name: "cloudflare-tunnel",
		},
	}

	// First reconcile
	result, err := r.Reconcile(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, result)

	// Second reconcile (should be idempotent)
	result, err = r.Reconcile(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, result)

	// Verify conditions still correct
	var updatedClass gatewayv1.GatewayClass
	err = fakeClient.Get(context.Background(), types.NamespacedName{Name: "cloudflare-tunnel"}, &updatedClass)
	require.NoError(t, err)
	assert.Len(t, updatedClass.Status.Conditions, 2)
}

func TestGatewayClassReconciler_Reconcile_WrongType(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, gatewayv1.Install(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		Build()

	r := &GatewayClassReconciler{
		Client:         fakeClient,
		Scheme:         scheme,
		ControllerName: "test-controller",
	}

	// Request for a non-existent GatewayClass
	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name: "missing",
		},
	}

	result, err := r.Reconcile(context.Background(), req)
	assert.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, result)
}
