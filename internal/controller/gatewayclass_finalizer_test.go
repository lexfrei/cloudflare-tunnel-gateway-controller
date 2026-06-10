package controller

// Tests for the gateway-exists-finalizer lifecycle (Gateway API GatewayClass
// godoc: implementations SHOULD add the finalizer while any Gateway uses the
// class and remove it when none do, so an in-use GatewayClass cannot be
// deleted out from under its Gateways).

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	"sigs.k8s.io/gateway-api/pkg/consts"
)

// finalizerGateway builds a Gateway bound to the given class name.
func finalizerGateway(name, gatewayClassName string) *gatewayv1.Gateway {
	return &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(gatewayClassName),
			Listeners: []gatewayv1.Listener{
				{Name: "https", Port: 443, Protocol: gatewayv1.HTTPSProtocolType},
			},
		},
	}
}

func reconcileGatewayClassOnce(t *testing.T, r *GatewayClassReconciler, name string) {
	t.Helper()

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: name},
	})
	require.NoError(t, err)
}

func TestGatewayClassForGateway_MapsToClassName(t *testing.T) {
	t.Parallel()

	requests := gatewayClassForGateway(context.Background(), finalizerGateway("gw-1", "cloudflare-tunnel"))

	require.Len(t, requests, 1)
	assert.Equal(t, "cloudflare-tunnel", requests[0].Name)
}

func TestGatewayClassForGateway_NonGatewayObjectIgnored(t *testing.T) {
	t.Parallel()

	requests := gatewayClassForGateway(context.Background(), &gatewayv1.GatewayClass{})

	assert.Empty(t, requests)
}

func TestGatewayClassReconciler_Finalizer_AddedWhenGatewayUsesClass(t *testing.T) {
	t.Parallel()

	scheme := gatewayClassSchemeWithCRD(t)

	gatewayClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "cloudflare-tunnel", Generation: 1},
		Spec:       gatewayv1.GatewayClassSpec{ControllerName: "cloudflare-tunnel-controller"},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(gatewayClass, finalizerGateway("gw-1", "cloudflare-tunnel"), gatewayClassCRDObject(consts.BundleVersion)).
		WithStatusSubresource(gatewayClass).
		Build()

	r := &GatewayClassReconciler{
		Client:              fakeClient,
		Scheme:              scheme,
		ControllerName:      "cloudflare-tunnel-controller",
		BundleVersionReader: fakeClient,
	}

	reconcileGatewayClassOnce(t, r, "cloudflare-tunnel")

	var updated gatewayv1.GatewayClass
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{Name: "cloudflare-tunnel"}, &updated))
	assert.Contains(t, updated.Finalizers, gatewayv1.GatewayClassFinalizerGatewaysExist,
		"a GatewayClass in use by a Gateway must carry the gateway-exists finalizer")
}

func TestGatewayClassReconciler_Finalizer_RemovedWhenNoGatewayUsesClass(t *testing.T) {
	t.Parallel()

	scheme := gatewayClassSchemeWithCRD(t)

	gatewayClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "cloudflare-tunnel",
			Generation: 1,
			Finalizers: []string{gatewayv1.GatewayClassFinalizerGatewaysExist},
		},
		Spec: gatewayv1.GatewayClassSpec{ControllerName: "cloudflare-tunnel-controller"},
	}

	// A Gateway exists but is bound to a DIFFERENT class -- it must not keep
	// the finalizer alive.
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(gatewayClass, finalizerGateway("gw-other", "some-other-class"), gatewayClassCRDObject(consts.BundleVersion)).
		WithStatusSubresource(gatewayClass).
		Build()

	r := &GatewayClassReconciler{
		Client:              fakeClient,
		Scheme:              scheme,
		ControllerName:      "cloudflare-tunnel-controller",
		BundleVersionReader: fakeClient,
	}

	reconcileGatewayClassOnce(t, r, "cloudflare-tunnel")

	var updated gatewayv1.GatewayClass
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{Name: "cloudflare-tunnel"}, &updated))
	assert.NotContains(t, updated.Finalizers, gatewayv1.GatewayClassFinalizerGatewaysExist,
		"the finalizer must be removed once no Gateway uses the class")
}

func TestGatewayClassReconciler_Finalizer_ForeignClassUntouched(t *testing.T) {
	t.Parallel()

	scheme := gatewayClassSchemeWithCRD(t)

	foreignClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "foreign", Generation: 1},
		Spec:       gatewayv1.GatewayClassSpec{ControllerName: "example.com/other-controller"},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(foreignClass, finalizerGateway("gw-1", "foreign"), gatewayClassCRDObject(consts.BundleVersion)).
		WithStatusSubresource(foreignClass).
		Build()

	r := &GatewayClassReconciler{
		Client:              fakeClient,
		Scheme:              scheme,
		ControllerName:      "cloudflare-tunnel-controller",
		BundleVersionReader: fakeClient,
	}

	reconcileGatewayClassOnce(t, r, "foreign")

	var updated gatewayv1.GatewayClass
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{Name: "foreign"}, &updated))
	assert.NotContains(t, updated.Finalizers, gatewayv1.GatewayClassFinalizerGatewaysExist,
		"another controller's GatewayClass must not be touched")
}

func TestGatewayClassReconciler_Finalizer_PreservesForeignFinalizers(t *testing.T) {
	t.Parallel()

	scheme := gatewayClassSchemeWithCRD(t)

	const foreignFinalizer = "example.com/keep-me"

	gatewayClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "cloudflare-tunnel",
			Generation: 1,
			Finalizers: []string{foreignFinalizer, gatewayv1.GatewayClassFinalizerGatewaysExist},
		},
		Spec: gatewayv1.GatewayClassSpec{ControllerName: "cloudflare-tunnel-controller"},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(gatewayClass, gatewayClassCRDObject(consts.BundleVersion)).
		WithStatusSubresource(gatewayClass).
		Build()

	r := &GatewayClassReconciler{
		Client:              fakeClient,
		Scheme:              scheme,
		ControllerName:      "cloudflare-tunnel-controller",
		BundleVersionReader: fakeClient,
	}

	reconcileGatewayClassOnce(t, r, "cloudflare-tunnel")

	var updated gatewayv1.GatewayClass
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{Name: "cloudflare-tunnel"}, &updated))
	assert.NotContains(t, updated.Finalizers, gatewayv1.GatewayClassFinalizerGatewaysExist,
		"own finalizer removed when class is unused")
	assert.Contains(t, updated.Finalizers, foreignFinalizer,
		"foreign finalizers must survive the removal of our own")
}

// TestGatewayClassReconciler_Finalizer_DeletionUnblocksCleanly is the
// deletion-path contract: a deleted-but-finalizer-held class whose last
// Gateway disappears must have the finalizer removed, actually vanish, and
// the reconcile must return cleanly -- the post-removal status update must
// not surface the inevitable NotFound as an error.
func TestGatewayClassReconciler_Finalizer_DeletionUnblocksCleanly(t *testing.T) {
	t.Parallel()

	scheme := gatewayClassSchemeWithCRD(t)

	gatewayClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "cloudflare-tunnel",
			Generation: 1,
			Finalizers: []string{gatewayv1.GatewayClassFinalizerGatewaysExist},
		},
		Spec: gatewayv1.GatewayClassSpec{ControllerName: "cloudflare-tunnel-controller"},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(gatewayClass, gatewayClassCRDObject(consts.BundleVersion)).
		WithStatusSubresource(gatewayClass).
		Build()

	// Delete while the finalizer is held: the object gains a
	// deletionTimestamp and stays around -- exactly the blocked state the
	// finalizer exists to create.
	require.NoError(t, fakeClient.Delete(context.Background(), gatewayClass))

	var blocked gatewayv1.GatewayClass
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{Name: "cloudflare-tunnel"}, &blocked))
	require.False(t, blocked.DeletionTimestamp.IsZero(), "deletion must be blocked by the finalizer, not immediate")

	r := &GatewayClassReconciler{
		Client:              fakeClient,
		Scheme:              scheme,
		ControllerName:      "cloudflare-tunnel-controller",
		BundleVersionReader: fakeClient,
	}

	// No Gateway uses the class: the reconcile must remove the finalizer,
	// let the object go, and return no error.
	reconcileGatewayClassOnce(t, r, "cloudflare-tunnel")

	var gone gatewayv1.GatewayClass
	getErr := fakeClient.Get(context.Background(), types.NamespacedName{Name: "cloudflare-tunnel"}, &gone)
	assert.True(t, apierrors.IsNotFound(getErr),
		"removing the last finalizer on a deleting class must let the object vanish")
}

// TestGatewayClassReconciler_Finalizer_NotAddedToDeletingClass pins the
// API-server contract that no finalizer may be ADDED to an object already
// being deleted: a deleting class held by a foreign finalizer must not get
// ours stamped on, even while a Gateway still references it (a real
// apiserver would reject the update and the reconcile would error-loop).
func TestGatewayClassReconciler_Finalizer_NotAddedToDeletingClass(t *testing.T) {
	t.Parallel()

	scheme := gatewayClassSchemeWithCRD(t)

	const foreignFinalizer = "example.com/hold"

	gatewayClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "cloudflare-tunnel",
			Generation: 1,
			Finalizers: []string{foreignFinalizer},
		},
		Spec: gatewayv1.GatewayClassSpec{ControllerName: "cloudflare-tunnel-controller"},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(gatewayClass, finalizerGateway("gw-1", "cloudflare-tunnel"), gatewayClassCRDObject(consts.BundleVersion)).
		WithStatusSubresource(gatewayClass).
		Build()

	require.NoError(t, fakeClient.Delete(context.Background(), gatewayClass))

	r := &GatewayClassReconciler{
		Client:              fakeClient,
		Scheme:              scheme,
		ControllerName:      "cloudflare-tunnel-controller",
		BundleVersionReader: fakeClient,
	}

	reconcileGatewayClassOnce(t, r, "cloudflare-tunnel")

	var updated gatewayv1.GatewayClass
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{Name: "cloudflare-tunnel"}, &updated))
	assert.NotContains(t, updated.Finalizers, gatewayv1.GatewayClassFinalizerGatewaysExist,
		"a finalizer must never be added to an object that is already being deleted")
}
