package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	"sigs.k8s.io/gateway-api/pkg/consts"
)

func managedClass(name, configName string) *gatewayv1.GatewayClass {
	return &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: name, Generation: 1},
		Spec:       gatewayv1.GatewayClassSpec{ControllerName: "test-controller", ParametersRef: classConfigRef(configName)},
	}
}

// reconcileClassAccepted reconciles the named class against objs and returns
// its Accepted condition.
func reconcileClassAccepted(t *testing.T, name string, objs ...client.Object) metav1.Condition {
	t.Helper()

	ctx := context.Background()
	scheme := gatewayClassSchemeWithConfig(t)

	var statusObjs []client.Object

	for _, obj := range objs {
		if _, ok := obj.(*gatewayv1.GatewayClass); ok {
			statusObjs = append(statusObjs, obj)
		}
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(gatewayAPICRDObjects(consts.BundleVersion, objs...)...).
		WithStatusSubresource(statusObjs...).
		Build()

	r := &GatewayClassReconciler{
		Client:              fakeClient,
		Scheme:              scheme,
		ControllerName:      "test-controller",
		BundleVersionReader: fakeClient,
	}

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: name}})
	require.NoError(t, err)

	var class gatewayv1.GatewayClass
	require.NoError(t, fakeClient.Get(ctx, types.NamespacedName{Name: name}, &class))

	accepted := findGatewayClassCondition(class.Status.Conditions, string(gatewayv1.GatewayClassConditionStatusAccepted))
	require.NotNil(t, accepted)

	return *accepted
}

// TestGatewayClassReconciler_ClassConfigConflict pins that a GatewayClass is
// Accepted only when a Gateway on it would be provisioned. While managed
// classes in use disagree on parametersRef, route sync refuses every Gateway,
// so a class whose Gateways would join that conflict is not Accepted.
func TestGatewayClassReconciler_ClassConfigConflict(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		reconciled string
		objs       []client.Object
		want       metav1.ConditionStatus
	}{
		{
			name:       "class in use beside another in-use class with a different config",
			reconciled: "alpha",
			objs: []client.Object{
				managedClass("alpha", "config-a"), managedClass("beta", "config-b"),
				finalizerGateway("gw-a", "alpha"), finalizerGateway("gw-b", "beta"),
			},
			want: metav1.ConditionFalse,
		},
		{
			name:       "unused class whose Gateways would conflict with the class in use",
			reconciled: "alpha",
			objs: []client.Object{
				managedClass("alpha", "config-a"), managedClass("beta", "config-b"),
				finalizerGateway("gw-b", "beta"),
			},
			want: metav1.ConditionFalse,
		},
		{
			name:       "class in use while an unused class has a different config",
			reconciled: "beta",
			objs: []client.Object{
				managedClass("alpha", "config-a"), managedClass("beta", "config-b"),
				finalizerGateway("gw-b", "beta"),
			},
			want: metav1.ConditionTrue,
		},
		{
			name:       "classes in use sharing one config",
			reconciled: "alpha",
			objs: []client.Object{
				managedClass("alpha", "config-a"), managedClass("beta", "config-a"),
				finalizerGateway("gw-a", "alpha"), finalizerGateway("gw-b", "beta"),
			},
			want: metav1.ConditionTrue,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			objs := append([]client.Object{classConfigObject("config-a"), classConfigObject("config-b")}, tt.objs...)
			accepted := reconcileClassAccepted(t, tt.reconciled, objs...)

			assert.Equal(t, tt.want, accepted.Status)

			if tt.want == metav1.ConditionFalse {
				assert.Equal(t, string(gatewayv1.GatewayClassReasonInvalidParameters), accepted.Reason)
				assert.Contains(t, accepted.Message, "conflicting parametersRef")
			}
		})
	}
}

// TestGatewayClassReconciler_ConflictEnqueuesEveryManagedClass pins that a
// GatewayClass or Gateway event re-evaluates every managed class, since one
// class's parametersRef or one Gateway's class decides whether the others
// conflict.
func TestGatewayClassReconciler_ConflictEnqueuesEveryManagedClass(t *testing.T) {
	t.Parallel()

	foreign := managedClass("foreign", "config-a")
	foreign.Spec.ControllerName = "someone-else"

	fakeClient := fake.NewClientBuilder().
		WithScheme(gatewayClassSchemeWithConfig(t)).
		WithObjects(managedClass("alpha", "config-a"), managedClass("beta", "config-b"), foreign).
		Build()

	r := &GatewayClassReconciler{Client: fakeClient, ControllerName: "test-controller"}

	want := []reconcile.Request{{NamespacedName: types.NamespacedName{Name: "alpha"}}, {NamespacedName: types.NamespacedName{Name: "beta"}}}

	assert.ElementsMatch(t, want, r.managedGatewayClasses(context.Background(), managedClass("beta", "config-c")))
	assert.ElementsMatch(t, want, r.managedGatewayClasses(context.Background(), finalizerGateway("gw", "alpha")))
}

// TestGatewayClassReconciler_ClassConflictListErrorKeepsAccepted pins that a
// failed Gateway listing is a transient read, not a verdict: Accepted stays as
// it stood and the error requeues.
func TestGatewayClassReconciler_ClassConflictListErrorKeepsAccepted(t *testing.T) {
	t.Parallel()

	class := managedClass("alpha", "config-a")
	stood := metav1.Condition{
		Type: string(gatewayv1.GatewayClassConditionStatusAccepted), Status: metav1.ConditionTrue,
		Reason: string(gatewayv1.GatewayClassReasonAccepted), Message: "as it stood",
	}
	class.Status.Conditions = []metav1.Condition{stood}

	fakeClient := fake.NewClientBuilder().
		WithScheme(gatewayClassSchemeWithConfig(t)).
		WithObjects(gatewayAPICRDObjects(consts.BundleVersion, class, classConfigObject("config-a"))...).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, ok := list.(*gatewayv1.GatewayList); ok {
					return errTransientRead
				}

				return c.List(ctx, list, opts...)
			},
		}).
		Build()

	r := &GatewayClassReconciler{Client: fakeClient, ControllerName: "test-controller", BundleVersionReader: fakeClient}

	require.ErrorIs(t, r.setAcceptedConditions(context.Background(), class), errTransientRead)

	accepted := findGatewayClassCondition(class.Status.Conditions, string(gatewayv1.GatewayClassConditionStatusAccepted))
	require.NotNil(t, accepted)
	assert.Equal(t, "as it stood", accepted.Message)
}
