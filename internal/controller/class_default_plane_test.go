package controller

import (
	"context"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/api/v1alpha1"
)

// withClassDefault turns a per-Gateway fixture set into its class-default
// twin: every Gateway loses its own parametersRef and every
// GatewayClassConfig names defaultName as the default GatewayConfig.
func withClassDefault[T runtime.Object](objects []T, defaultName string) []T {
	for _, obj := range objects {
		switch typed := any(obj).(type) {
		case *gatewayv1.Gateway:
			typed.Spec.Infrastructure = &gatewayv1.GatewayInfrastructure{
				Labels:      map[gatewayv1.LabelKey]gatewayv1.LabelValue{"team": "a"},
				Annotations: map[gatewayv1.AnnotationKey]gatewayv1.AnnotationValue{"owner": "ops"},
			}
		case *v1alpha1.GatewayClassConfig:
			typed.Spec.PerGatewayDataPlanes = &v1alpha1.PerGatewayDataPlanes{DefaultGatewayConfigName: defaultName}
		}
	}

	return objects
}

func withoutGatewayConfigs(objects []client.Object) []client.Object {
	return slices.DeleteFunc(objects, func(obj client.Object) bool {
		_, isGatewayConfig := obj.(*v1alpha1.GatewayConfig)

		return isGatewayConfig
	})
}

func TestGatewayReconciler_ClassDefault_AddressFromToken(t *testing.T) {
	t.Parallel()

	fakeClient := setupGatewayFakeClient(withClassDefault(perGatewayStatusFixtures(t), "pg-config")...)
	updated := reconcilePGGateway(t, fakeClient)

	require.Len(t, updated.Status.Addresses, 1)
	assert.Equal(t, "550e8400-e29b-41d4-a716-446655440000.cfargotunnel.com", updated.Status.Addresses[0].Value,
		"a class-default Gateway advertises the tunnel of the default GatewayConfig's token")

	programmed := findCondition(updated.Status.Conditions, string(gatewayv1.GatewayConditionProgrammed))
	require.NotNil(t, programmed)
	assert.Equal(t, metav1.ConditionFalse, programmed.Status,
		"Programmed waits for the dedicated plane's Deployment, as for an explicit opt-in")
}

func TestGatewayReconciler_ClassDefault_MissingGatewayConfigIsRefused(t *testing.T) {
	t.Parallel()

	objects := withoutGatewayConfigs(withClassDefault(perGatewayStatusFixtures(t), "pg-config"))
	updated := reconcilePGGateway(t, setupGatewayFakeClient(objects...))

	accepted := findCondition(updated.Status.Conditions, string(gatewayv1.GatewayConditionAccepted))
	require.NotNil(t, accepted)
	assert.Equal(t, metav1.ConditionFalse, accepted.Status, "no fallback to the shared data plane")
	assert.Equal(t, string(gatewayv1.GatewayReasonInvalidParameters), accepted.Reason)
	assert.Contains(t, accepted.Message, "GatewayConfig default/pg-config")
	assert.Empty(t, updated.Status.Addresses, "a refused class-default Gateway must not advertise the class tunnel")
}

func TestGatewayConfigToGateways_ClassDefault(t *testing.T) {
	t.Parallel()

	objects := withClassDefault(perGatewayStatusFixtures(t), "pg-config")
	fakeClient := setupGatewayFakeClient(objects...)
	reconciler := &GatewayReconciler{Client: fakeClient, Scheme: fakeClient.Scheme(), ControllerName: "test-controller"}

	named := reconciler.gatewayConfigToGateways(context.Background(), &v1alpha1.GatewayConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "pg-config", Namespace: "default"},
	})
	assert.Equal(t, []string{"default/pg-gateway"}, requestKeys(named))

	other := reconciler.gatewayConfigToGateways(context.Background(), &v1alpha1.GatewayConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: "default"},
	})
	assert.Empty(t, other)
}

func TestManagedGateways_ClassDefault(t *testing.T) {
	t.Parallel()

	ownRef := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "own", Namespace: "default"},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "cloudflare-tunnel",
			Infrastructure: &gatewayv1.GatewayInfrastructure{ParametersRef: &gatewayv1.LocalParametersReference{
				Group: "cf.k8s.lex.la", Kind: "GatewayConfig", Name: "own-config",
			}},
		},
	}
	foreign := foreignOptedInGateway("default")
	foreign.Spec.Infrastructure = nil

	keys := func(gateways []*gatewayv1.Gateway) []string {
		out := make([]string, 0, len(gateways))
		for _, gateway := range gateways {
			out = append(out, gateway.Namespace+"/"+gateway.Name)
		}

		slices.Sort(out)

		return out
	}

	withDefault := setupGatewayFakeClient(append(withClassDefault(perGatewayStatusFixtures(t), "pg-config"),
		ownRef, foreign)...)

	dedicated, err := managedInfraGateways(context.Background(), withDefault, "test-controller")
	require.NoError(t, err)
	assert.Equal(t, []string{"default/own", "default/pg-gateway"}, keys(dedicated))

	shared, err := managedGateways(context.Background(), withDefault, "test-controller", false)
	require.NoError(t, err)
	assert.Empty(t, shared)

	optedIn := optedInGatewaysInNamespace(context.Background(), withDefault, "test-controller", "default")
	assert.ElementsMatch(t, []string{"default/own", "default/pg-gateway"}, requestKeys(optedIn))

	noDefault := withClassDefault(perGatewayStatusFixtures(t), "pg-config")
	for _, obj := range noDefault {
		if classConfig, ok := obj.(*v1alpha1.GatewayClassConfig); ok {
			classConfig.Spec.PerGatewayDataPlanes = nil
		}
	}

	withoutDefault := setupGatewayFakeClient(noDefault...)

	shared, err = managedGateways(context.Background(), withoutDefault, "test-controller", false)
	require.NoError(t, err)
	assert.Equal(t, []string{"default/pg-gateway"}, keys(shared))
}

// classDefaultInfraFixtures is infraFixtures with the Gateway relying on the
// class default instead of its own parametersRef.
func classDefaultInfraFixtures(t *testing.T) []runtime.Object {
	t.Helper()

	return withClassDefault(infraFixtures(t), "edge-config")
}

func TestGatewayInfraReconciler_ClassDefaultRendersDataPlane(t *testing.T) {
	t.Parallel()

	reconciler := newInfraReconciler(t, classDefaultInfraFixtures(t)...)
	reconcileEdge(t, reconciler)

	var deployment appsv1.Deployment
	require.NoError(t, reconciler.Get(context.Background(),
		types.NamespacedName{Name: "cf-proxy-edge", Namespace: infraNamespace}, &deployment))

	podMeta := deployment.Spec.Template.ObjectMeta
	assert.Equal(t, "a", podMeta.Labels["team"], "spec.infrastructure labels reach the pods")
	assert.Equal(t, "ops", podMeta.Annotations["owner"], "spec.infrastructure annotations reach the pods")
	assert.Equal(t, "edge", podMeta.Labels[gatewayv1.GatewayNameLabelKey])

	var service corev1.Service
	require.NoError(t, reconciler.Get(context.Background(),
		types.NamespacedName{Name: "cf-proxy-edge-config", Namespace: infraNamespace}, &service))
	assert.Equal(t, "a", service.Labels["team"])
}

func TestGatewayInfraReconciler_ClassDefaultTurnedOffCleansUp(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	reconciler := newInfraReconciler(t, classDefaultInfraFixtures(t)...)
	reconcileEdge(t, reconciler)

	var classConfig v1alpha1.GatewayClassConfig
	require.NoError(t, reconciler.Get(ctx, types.NamespacedName{Name: "class-config"}, &classConfig))

	classConfig.Spec.PerGatewayDataPlanes = nil
	require.NoError(t, reconciler.Update(ctx, &classConfig))

	enqueued := reconciler.classConfigInfraGateways(ctx, &classConfig)
	assert.Equal(t, []string{infraNamespace + "/edge"}, requestKeys(enqueued),
		"turning the default off must reach the Gateways that just lost their plane")

	reconcileEdge(t, reconciler)

	err := reconciler.Get(ctx, types.NamespacedName{Name: "cf-proxy-edge", Namespace: infraNamespace},
		&appsv1.Deployment{})
	assert.True(t, apierrors.IsNotFound(err), "back on the shared plane, the dedicated one is removed: %v", err)
}

func TestGatewayInfraReconciler_ClassDefaultMissingGatewayConfigRendersNothing(t *testing.T) {
	t.Parallel()

	objects := slices.DeleteFunc(classDefaultInfraFixtures(t), func(obj runtime.Object) bool {
		_, isGatewayConfig := obj.(*v1alpha1.GatewayConfig)

		return isGatewayConfig
	})

	reconciler := newInfraReconciler(t, objects...)
	reconcileEdge(t, reconciler)

	err := reconciler.Get(context.Background(),
		types.NamespacedName{Name: "cf-proxy-edge", Namespace: infraNamespace}, &appsv1.Deployment{})
	assert.True(t, apierrors.IsNotFound(err), "%v", err)
}

// Every class-default Gateway takes a slot of maxDataPlanesPerNamespace, the
// same as one naming its GatewayConfig itself.
func TestGatewayReconciler_ClassDefaultCountsTowardsTheQuota(t *testing.T) {
	t.Parallel()

	fakeClient := quotaFixtures(t, new(int32(1)))
	ctx := context.Background()

	var gateways gatewayv1.GatewayList
	require.NoError(t, fakeClient.List(ctx, &gateways, client.InNamespace("tenant")))

	for i := range gateways.Items {
		gateway := &gateways.Items[i]
		if gateway.Name == "gw-old" {
			continue
		}

		gateway.Spec.Infrastructure = nil
		require.NoError(t, fakeClient.Update(ctx, gateway))
	}

	var classConfig v1alpha1.GatewayClassConfig
	require.NoError(t, fakeClient.Get(ctx, types.NamespacedName{Name: "test-config"}, &classConfig))

	classConfig.Spec.PerGatewayDataPlanes = &v1alpha1.PerGatewayDataPlanes{DefaultGatewayConfigName: "gw-old-config"}
	require.NoError(t, fakeClient.Update(ctx, &classConfig))

	assert.Equal(t, metav1.ConditionTrue, quotaAcceptedCondition(t, fakeClient, "tenant", "gw-old").Status)

	refused := quotaAcceptedCondition(t, fakeClient, "tenant", "gw-new")
	assert.Equal(t, metav1.ConditionFalse, refused.Status)
	assert.Equal(t, string(reasonDataPlaneQuotaExceeded), refused.Reason)
	assert.NotContains(t, refused.Message, "drop spec.infrastructure.parametersRef",
		"a class-default Gateway has no ref to drop")
}

// A Gateway the shared plane serves reads its class only to learn there is no
// default; a broken class must still be reported as the class's problem.
func TestGatewayReconciler_SharedGatewayBrokenClassIsBlamedOnTheClass(t *testing.T) {
	t.Parallel()

	objects := slices.DeleteFunc(withClassDefault(perGatewayStatusFixtures(t), "pg-config"), func(obj client.Object) bool {
		_, isClassConfig := obj.(*v1alpha1.GatewayClassConfig)

		return isClassConfig
	})

	updated := reconcilePGGateway(t, setupGatewayFakeClient(objects...))

	accepted := findCondition(updated.Status.Conditions, string(gatewayv1.GatewayConditionAccepted))
	require.NotNil(t, accepted)
	assert.Equal(t, string(gatewayv1.GatewayReasonInvalidParameters), accepted.Reason)
	assert.NotContains(t, accepted.Message, "per-gateway configuration")
	assert.Contains(t, accepted.Message, "GatewayClass configuration")
}

func TestGatewayReconciler_ClassDefault_ConfigErrorKeepsTheTunnelAddress(t *testing.T) {
	t.Parallel()

	objects := slices.DeleteFunc(withClassDefault(perGatewayStatusFixtures(t), "pg-config"), func(obj client.Object) bool {
		secret, ok := obj.(*corev1.Secret)

		return ok && secret.Name == "pg-token"
	})

	for _, obj := range objects {
		if gateway, ok := obj.(*gatewayv1.Gateway); ok {
			gateway.Status.Addresses = []gatewayv1.GatewayStatusAddress{
				{Value: "550e8400-e29b-41d4-a716-446655440000" + cfArgotunnelSuffix},
			}
		}
	}

	updated := reconcilePGGateway(t, setupGatewayFakeClient(objects...))

	require.Len(t, updated.Status.Addresses, 1,
		"a class-default plane holds its tunnel through a config error, like an explicit one")
}

func TestGatewayInfraReconciler_ClassDefaultBrokenClassKeepsThePlane(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	reconciler := newInfraReconciler(t, classDefaultInfraFixtures(t)...)
	reconcileEdge(t, reconciler)

	require.NoError(t, reconciler.Delete(ctx, &v1alpha1.GatewayClassConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "class-config"},
	}))

	reconcileEdge(t, reconciler)

	assert.NoError(t, reconciler.Get(ctx,
		types.NamespacedName{Name: "cf-proxy-edge", Namespace: infraNamespace}, &appsv1.Deployment{}),
		"a missing class config says nothing about whether the Gateway keeps its plane")
}

// A class whose GatewayClassConfig is missing must not stop the listing for
// every other class.
func TestManagedClassConfigs_MissingConfigIsNil(t *testing.T) {
	t.Parallel()

	objects := slices.DeleteFunc(perGatewayStatusFixtures(t), func(obj client.Object) bool {
		_, isClassConfig := obj.(*v1alpha1.GatewayClassConfig)

		return isClassConfig
	})

	configs, err := managedClassConfigs(context.Background(), setupGatewayFakeClient(objects...), "test-controller")
	require.NoError(t, err)
	require.Contains(t, configs, "cloudflare-tunnel")
	assert.Nil(t, configs["cloudflare-tunnel"])
}

// Only a Gateway without its own parametersRef reaches the class read here, and
// nothing was going to render for it; the class error is reported on its
// status, not as a render failure.
func TestGatewayInfraReconciler_BrokenClassEmitsNoRenderEventForRefLessGateway(t *testing.T) {
	t.Parallel()

	objects := slices.DeleteFunc(classDefaultInfraFixtures(t), func(obj runtime.Object) bool {
		_, isClassConfig := obj.(*v1alpha1.GatewayClassConfig)

		return isClassConfig
	})

	reconciler := newInfraReconciler(t, objects...)
	recorder := events.NewFakeRecorder(10)
	reconciler.Recorder = recorder

	reconcileEdge(t, reconciler)

	assert.Empty(t, recorder.Events)
}

func TestGatewayClassInfraGateways(t *testing.T) {
	t.Parallel()

	other := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "elsewhere", Namespace: infraNamespace},
		Spec:       gatewayv1.GatewaySpec{GatewayClassName: "another-class"},
	}

	reconciler := newInfraReconciler(t, append(classDefaultInfraFixtures(t), other)...)

	requests := reconciler.gatewayClassInfraGateways(context.Background(), &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "cloudflare-tunnel"},
	})
	assert.Equal(t, []string{infraNamespace + "/edge"}, requestKeys(requests),
		"a class change can give or take a plane, or hand the class to another controller")
}

// An unreadable class cannot say whether the Gateway has a plane, but a plane
// it already runs proves it; dropping the address would release its tunnel
// and its DNS while that plane still serves.
func TestGatewayReconciler_ClassDefault_BrokenClassKeepsTheTunnelAddress(t *testing.T) {
	t.Parallel()

	const gatewayUID types.UID = "pg-gateway-uid"

	planeOwnedBy := func(uid types.UID) *appsv1.Deployment {
		return &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
			Name: "cf-proxy-pg-gateway", Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: gatewayv1.GroupVersion.String(), Kind: "Gateway", Name: "pg-gateway",
				UID: uid, Controller: new(true),
			}},
		}}
	}

	deploymentReadFails := interceptor.Funcs{
		Get: func(ctx context.Context, cli client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*appsv1.Deployment); ok {
				return apierrors.NewServiceUnavailable("informer not synced")
			}

			return cli.Get(ctx, key, obj, opts...)
		},
	}

	tests := []struct {
		name     string
		plane    *appsv1.Deployment
		funcs    *interceptor.Funcs
		wantKept bool
	}{
		{name: "owned plane keeps the address", plane: planeOwnedBy(gatewayUID), wantKept: true},
		{name: "no plane releases it"},
		{name: "a plane another object owns releases it", plane: planeOwnedBy("previous-incarnation")},
		{name: "an unreadable plane counts as running", funcs: &deploymentReadFails, wantKept: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			objects := slices.DeleteFunc(withClassDefault(perGatewayStatusFixtures(t), "pg-config"),
				func(obj client.Object) bool {
					_, isClassConfig := obj.(*v1alpha1.GatewayClassConfig)

					return isClassConfig
				})

			for _, obj := range objects {
				if gateway, ok := obj.(*gatewayv1.Gateway); ok {
					gateway.UID = gatewayUID
					gateway.Status.Addresses = []gatewayv1.GatewayStatusAddress{
						{Value: "550e8400-e29b-41d4-a716-446655440000" + cfArgotunnelSuffix},
					}
				}
			}

			if tt.plane != nil {
				objects = append(objects, tt.plane)
			}

			fakeClient := setupGatewayFakeClient(objects...)
			if tt.funcs != nil {
				fakeClient = interceptor.NewClient(fakeClient, *tt.funcs)
			}

			updated := reconcilePGGateway(t, fakeClient)

			if tt.wantKept {
				assert.Len(t, updated.Status.Addresses, 1)
			} else {
				assert.Empty(t, updated.Status.Addresses)
			}
		})
	}
}
