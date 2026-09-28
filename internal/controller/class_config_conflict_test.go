package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/api/v1alpha1"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/cfmetrics"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/config"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/tunnelownership"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/tunnelproof"
)

// conflictingClass is a second GatewayClass on the same controllerName whose
// parametersRef names a different GatewayClassConfig, plus a Gateway on it.
// Route sync refuses to program anything while a Gateway uses it, because it
// writes every class's routes to one tunnel.
func conflictingClass(controllerName string) []client.Object {
	return []client.Object{
		&gatewayv1.GatewayClass{
			ObjectMeta: metav1.ObjectMeta{Name: "zz-other-class"},
			Spec: gatewayv1.GatewayClassSpec{
				ControllerName: gatewayv1.GatewayController(controllerName),
				ParametersRef: &gatewayv1.ParametersReference{
					Group: config.ParametersRefGroup, Kind: config.ParametersRefKind, Name: "other-config",
				},
			},
		},
		&v1alpha1.GatewayClassConfig{
			ObjectMeta: metav1.ObjectMeta{Name: "other-config"},
			Spec: v1alpha1.GatewayClassConfigSpec{
				CloudflareCredentialsSecretRef: v1alpha1.SecretReference{Name: "cf-credentials", Namespace: "default"},
				TunnelID:                       "77777777-7777-4777-8777-777777777777",
			},
		},
		strayGateway(),
	}
}

// strayGateway is the Gateway that puts the conflicting class in use.
func strayGateway() *gatewayv1.Gateway {
	return &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "stray", Namespace: "elsewhere"},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "zz-other-class",
			Listeners:        []gatewayv1.Listener{{Name: "http", Port: 80, Protocol: "HTTP"}},
		},
	}
}

// withoutStray drops the stray Gateway, leaving the conflicting class unused.
func withoutStray(objects []client.Object) []client.Object {
	out := make([]client.Object, 0, len(objects))

	for _, obj := range objects {
		if _, ok := obj.(*gatewayv1.Gateway); !ok {
			out = append(out, obj)
		}
	}

	return out
}

func createAll(t *testing.T, cli client.Client, objects []client.Object) {
	t.Helper()

	for _, obj := range objects {
		require.NoError(t, cli.Create(context.Background(), obj))
	}
}

// TestGatewayReconciler_ConflictingClassConfigsRefuseEveryGateway pins that the
// status layer reaches the verdict route sync already reaches. With two managed
// classes in use on different GatewayClassConfigs, route sync programs nothing,
// so a Gateway reporting Accepted=True there has routes that silently never
// work.
func TestGatewayReconciler_ConflictingClassConfigsRefuseEveryGateway(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		name      string
		dedicated bool
	}{
		{name: "a Gateway with a dedicated data plane", dedicated: true},
		{name: "a Gateway on the shared data plane", dedicated: false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			fakeClient := quotaFixtures(t, nil)
			ctx := context.Background()

			if !testCase.dedicated {
				var gateway gatewayv1.Gateway
				require.NoError(t, fakeClient.Get(ctx, types.NamespacedName{Name: "gw-old", Namespace: "tenant"}, &gateway))

				gateway.Spec.Infrastructure = nil
				require.NoError(t, fakeClient.Update(ctx, &gateway))
			}

			served := reconcileQuotaGateway(t, fakeClient, nil, "tenant", "gw-old")
			require.NotEmpty(t, served.Status.Addresses, "the fixture must start from a Gateway advertising its tunnel")

			createAll(t, fakeClient, conflictingClass("test-controller"))

			refused := reconcileQuotaGateway(t, fakeClient, nil, "tenant", "gw-old")

			accepted := findCondition(refused.Status.Conditions, string(gatewayv1.GatewayConditionAccepted))
			require.NotNil(t, accepted)
			assert.Equal(t, metav1.ConditionFalse, accepted.Status,
				"route sync programs nothing while managed classes disagree, so the Gateway must not report Accepted")
			assert.Equal(t, string(gatewayv1.GatewayReasonInvalidParameters), accepted.Reason)
			assert.Contains(t, accepted.Message, "zz-other-class",
				"the operator must be told which classes disagree")

			// The tunnel keeps serving its last configuration, and external-dns
			// publishes records from this address: clearing it would turn a
			// frozen configuration into deleted DNS for every hostname.
			assert.Equal(t, served.Status.Addresses, refused.Status.Addresses,
				"a class conflict must not withdraw the address the tunnel still serves")
		})
	}
}

// TestGatewayReconciler_RefusalOutranksTheClassConflict pins the order the
// status layer decides in. The infra reconciler removes a plane the tunnel rule
// refuses whatever the classes say, and only this layer's status write wakes it
// when a Cloudflare confirmation lapses, which no cluster event announces. A
// conflict checked first would stop that write, and keep the Gateway off the
// recheck that notices the lapse, for as long as the classes disagree.
func TestGatewayReconciler_RefusalOutranksTheClassConflict(t *testing.T) {
	t.Parallel()

	fakeClient := quotaFixtures(t, nil)
	createAll(t, fakeClient, conflictingClass("test-controller"))

	reconciler := &GatewayReconciler{
		Client:         fakeClient,
		Scheme:         fakeClient.Scheme(),
		ControllerName: "test-controller",
		ConfigResolver: config.NewResolver(fakeClient, "default", cfmetrics.NewNoopCollector(),
			config.WithClaimVerifier(fixedClaimVerifier(tunnelownership.ProofRefuted))),
		ProxyImage: "ghcr.io/example/proxy:v1.2.3",
		ViewStore:  newMergeViewStore(),
	}

	ctx := context.Background()
	key := types.NamespacedName{Name: "gw-old", Namespace: "tenant"}

	result, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: key})
	require.NoError(t, err)

	var refused gatewayv1.Gateway
	require.NoError(t, fakeClient.Get(ctx, key, &refused))

	accepted := findCondition(refused.Status.Conditions, string(gatewayv1.GatewayConditionAccepted))
	require.NotNil(t, accepted)
	assert.Equal(t, metav1.ConditionFalse, accepted.Status)
	assert.Contains(t, accepted.Message, "Cloudflare did not confirm",
		"a refused claim must be reported as refused while the classes disagree")
	assert.NotContains(t, accepted.Message, "zz-other-class")

	assert.Positive(t, result.RequeueAfter, "the refusal must be rechecked")
	assert.LessOrEqual(t, result.RequeueAfter, tunnelproof.RecheckInterval)
}

// TestGatewayReconciler_ClassConflictKeepsTheRecheck pins the other half: a
// dedicated Gateway whose claim holds, refused only for the conflict, still
// comes back within the recheck interval, so a confirmation that lapses while
// the classes disagree is noticed.
func TestGatewayReconciler_ClassConflictKeepsTheRecheck(t *testing.T) {
	t.Parallel()

	fakeClient := quotaFixtures(t, nil)
	createAll(t, fakeClient, conflictingClass("test-controller"))

	reconciler := &GatewayReconciler{
		Client:         fakeClient,
		Scheme:         fakeClient.Scheme(),
		ControllerName: "test-controller",
		ConfigResolver: config.NewResolver(fakeClient, "default", cfmetrics.NewNoopCollector(), verifiedClaims()),
		ProxyImage:     "ghcr.io/example/proxy:v1.2.3",
		ViewStore:      newMergeViewStore(),
	}

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "gw-old", Namespace: "tenant"},
	})
	require.NoError(t, err)

	assert.Positive(t, result.RequeueAfter, "a dedicated Gateway must stay on the recheck during a conflict")
	assert.LessOrEqual(t, result.RequeueAfter, tunnelproof.RecheckInterval)
}

// TestGatewayReconciler_UnusedConflictingClassIsIgnored pins the other side:
// a class no Gateway names has no routes and no plane, so its parametersRef
// changes nothing that is served and must not refuse the Gateways on the
// classes in use.
func TestGatewayReconciler_UnusedConflictingClassIsIgnored(t *testing.T) {
	t.Parallel()

	fakeClient := quotaFixtures(t, nil)
	createAll(t, fakeClient, withoutStray(conflictingClass("test-controller")))

	assert.Equal(t, metav1.ConditionTrue, quotaAcceptedCondition(t, fakeClient, "tenant", "gw-old").Status,
		"a GatewayClass no Gateway uses must not refuse the Gateways on the classes in use")
}

// TestResolveConfigForController_PicksTheClassInUse pins route sync's side of
// the in-use rule: an unused class neither conflicts nor wins the selection.
func TestResolveConfigForController_PicksTheClassInUse(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, gatewayv1.Install(scheme))
	require.NoError(t, v1alpha1.AddToScheme(scheme))

	class := func(name, configName string) *gatewayv1.GatewayClass {
		return &gatewayv1.GatewayClass{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: gatewayv1.GatewayClassSpec{
				ControllerName: "test-controller",
				ParametersRef: &gatewayv1.ParametersReference{
					Group: config.ParametersRefGroup, Kind: config.ParametersRefKind, Name: configName,
				},
			},
		}
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(
			class("a-class", "config-tunnel-1"),
			class("b-class", "config-tunnel-2"),
			&gatewayv1.Gateway{
				ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "tenant"},
				Spec:       gatewayv1.GatewaySpec{GatewayClassName: "b-class"},
			},
		).
		Build()

	syncer := NewRouteSyncer(
		fakeClient, scheme, "cluster.local", "test-controller",
		config.NewResolver(fakeClient, "default", cfmetrics.NewNoopCollector(), verifiedClaims()),
		cfmetrics.NewNoopCollector(), nil,
	)

	// No GatewayClassConfig exists, so resolution fails, and the error names
	// the class that was selected.
	_, err := syncer.resolveConfigForController(context.Background())

	require.Error(t, err)
	assert.NotContains(t, err.Error(), "conflicting parametersRef",
		"a class no Gateway uses must not conflict")
	assert.Contains(t, err.Error(), "b-class", "the class in use must be selected")
}

// TestGatewayInfraReconciler_ConflictingClassConfigsRenderNoNewPlane pins the
// render layer's half: a plane rendered while route sync programs nothing never
// receives a config, and one rendered from a single class's policy while another
// class disagrees is a verdict the other two layers never reached.
func TestGatewayInfraReconciler_ConflictingClassConfigsRenderNoNewPlane(t *testing.T) {
	t.Parallel()

	objects := infraFixtures(t)
	for _, obj := range conflictingClass("cf.k8s.lex.la/tunnel-controller") {
		objects = append(objects, obj)
	}

	reconciler := newInfraReconciler(t, objects...)

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "edge", Namespace: infraNamespace},
	})
	require.NoError(t, err, "a class misconfiguration is permanent and must not be retried")

	var deployment appsv1.Deployment
	err = reconciler.Get(context.Background(),
		types.NamespacedName{Name: "cf-proxy-edge", Namespace: infraNamespace}, &deployment)
	require.Error(t, err, "no plane may be rendered while the managed classes disagree")
	assert.True(t, apierrors.IsNotFound(err), "expected NotFound, got %v", err)
}

// TestGatewayInfraReconciler_UnusedConflictingClassStillRenders is the control
// for the test above: the same stray class with no Gateway on it refuses
// nothing.
func TestGatewayInfraReconciler_UnusedConflictingClassStillRenders(t *testing.T) {
	t.Parallel()

	objects := infraFixtures(t)
	for _, obj := range withoutStray(conflictingClass("cf.k8s.lex.la/tunnel-controller")) {
		objects = append(objects, obj)
	}

	reconciler := newInfraReconciler(t, objects...)
	reconcileEdge(t, reconciler)

	var deployment appsv1.Deployment
	assert.NoError(t, reconciler.Get(context.Background(),
		types.NamespacedName{Name: "cf-proxy-edge", Namespace: infraNamespace}, &deployment),
		"a GatewayClass no Gateway uses must not stop a plane from rendering")
}

// TestGatewayInfraReconciler_ConflictingClassConfigsKeepARunningPlane pins that
// the refusal is not a teardown. The misconfiguration is the operator's and says
// nothing about the tenant's plane, which keeps its last pushed config either
// way; deleting it would turn a second GatewayClass into an outage for every
// dedicated plane in the cluster.
func TestGatewayInfraReconciler_ConflictingClassConfigsKeepARunningPlane(t *testing.T) {
	t.Parallel()

	reconciler := newInfraReconciler(t, infraFixtures(t)...)
	reconcileEdge(t, reconciler)

	createAll(t, reconciler.Client, conflictingClass("cf.k8s.lex.la/tunnel-controller"))

	reconcileEdge(t, reconciler)

	var deployment appsv1.Deployment
	assert.NoError(t, reconciler.Get(context.Background(),
		types.NamespacedName{Name: "cf-proxy-edge", Namespace: infraNamespace}, &deployment),
		"a second GatewayClass must not tear down a running plane")
}

// TestGatewayInfraReconciler_ConflictingClassConfigsStillTearDownARefusedPlane
// pins that the class conflict only stops rendering. A plane the tunnel rule or
// the cap refuses is torn down whatever the classes say: a pod left running on
// a tunnel its Gateway does not own registers a connector there on its next
// restart and answers a share of the holder's traffic.
func TestGatewayInfraReconciler_ConflictingClassConfigsStillTearDownARefusedPlane(t *testing.T) {
	t.Parallel()

	const classTunnel = "99999999-9999-4999-8999-999999999999"

	for _, testCase := range []struct {
		name   string
		refuse func(t *testing.T, cli client.Client)
	}{
		{
			name: "the token now names a tunnel the Gateway does not own",
			refuse: func(t *testing.T, cli client.Client) {
				t.Helper()

				var token corev1.Secret
				require.NoError(t, cli.Get(context.Background(),
					types.NamespacedName{Name: "edge-token", Namespace: infraNamespace}, &token))

				token.Data["tunnel-token"] = []byte(infraTunnelTokenFor(t, classTunnel))
				require.NoError(t, cli.Update(context.Background(), &token))
			},
		},
		{
			name: "the cap is lowered below the namespace's planes",
			refuse: func(t *testing.T, cli client.Client) {
				t.Helper()

				var classConfig v1alpha1.GatewayClassConfig
				require.NoError(t, cli.Get(context.Background(), types.NamespacedName{Name: "class-config"}, &classConfig))

				classConfig.Spec.MaxDataPlanesPerNamespace = new(int32(1))
				require.NoError(t, cli.Update(context.Background(), &classConfig))
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			reconciler := newInfraReconciler(t, infraQuotaFixtures(t, nil)...)
			reconcileEdge(t, reconciler)

			ctx := context.Background()
			deploymentKey := types.NamespacedName{Name: "cf-proxy-edge", Namespace: infraNamespace}

			var deployment appsv1.Deployment
			require.NoError(t, reconciler.Get(ctx, deploymentKey, &deployment),
				"the plane must exist before it is refused")

			createAll(t, reconciler.Client, conflictingClass("cf.k8s.lex.la/tunnel-controller"))
			testCase.refuse(t, reconciler.Client)

			reconcileEdge(t, reconciler)

			err := reconciler.Get(ctx, deploymentKey, &deployment)
			require.Error(t, err, "a refused plane must be removed while the managed classes disagree")
			assert.True(t, apierrors.IsNotFound(err), "expected NotFound, got %v", err)
		})
	}
}

// TestGatewayReconciler_TerminatingGatewayKeepsItsClassInUse pins that a
// Gateway being deleted still puts its class in use. Its plane and routes are
// still served until it is gone, so the class it names still decides where
// they go.
func TestGatewayReconciler_TerminatingGatewayKeepsItsClassInUse(t *testing.T) {
	t.Parallel()

	fakeClient := quotaFixtures(t, nil)
	createAll(t, fakeClient, conflictingClass("test-controller"))
	holdInTermination(t, fakeClient, "elsewhere", "stray")

	accepted := quotaAcceptedCondition(t, fakeClient, "tenant", "gw-old")
	assert.Equal(t, metav1.ConditionFalse, accepted.Status,
		"a class whose only Gateway is terminating is still in use")
}
