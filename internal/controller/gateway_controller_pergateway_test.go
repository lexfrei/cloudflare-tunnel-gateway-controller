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
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/api/v1alpha1"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/cfmetrics"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/config"
)

// perGatewayStatusFixtures assembles the full per-Gateway opt-in chain in the
// "default" namespace for GatewayReconciler status tests.
func perGatewayStatusFixtures(t *testing.T) []client.Object {
	t.Helper()

	return []client.Object{
		&gatewayv1.Gateway{
			ObjectMeta: metav1.ObjectMeta{Name: "pg-gateway", Namespace: "default"},
			Spec: gatewayv1.GatewaySpec{
				GatewayClassName: "cloudflare-tunnel",
				Listeners: []gatewayv1.Listener{
					{Name: "http", Port: 80, Protocol: "HTTP"},
				},
				Infrastructure: &gatewayv1.GatewayInfrastructure{
					ParametersRef: &gatewayv1.LocalParametersReference{
						Group: "cf.k8s.lex.la", Kind: "GatewayConfig", Name: "pg-config",
					},
				},
			},
		},
		&v1alpha1.GatewayConfig{
			ObjectMeta: metav1.ObjectMeta{Name: "pg-config", Namespace: "default"},
			Spec: v1alpha1.GatewayConfigSpec{
				TunnelTokenSecretRef: v1alpha1.LocalSecretReference{Name: "pg-token"},
			},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "pg-token", Namespace: "default"},
			Data:       map[string][]byte{"tunnel-token": []byte(infraTunnelToken(t))},
		},
		// The generated config-API auth Secret the infra reconciler creates
		// for a GatewayConfig without an explicit authTokenSecretRef.
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "cf-proxy-pg-gateway-auth", Namespace: "default"},
			Data:       map[string][]byte{"auth-token": []byte("generated-bearer")},
		},
		&gatewayv1.GatewayClass{
			ObjectMeta: metav1.ObjectMeta{Name: "cloudflare-tunnel"},
			Spec: gatewayv1.GatewayClassSpec{
				ControllerName: "test-controller",
				ParametersRef: &gatewayv1.ParametersReference{
					Group: config.ParametersRefGroup, Kind: config.ParametersRefKind, Name: "test-config",
				},
			},
		},
		&v1alpha1.GatewayClassConfig{
			ObjectMeta: metav1.ObjectMeta{Name: "test-config"},
			Spec: v1alpha1.GatewayClassConfigSpec{
				CloudflareCredentialsSecretRef: v1alpha1.SecretReference{Name: "cf-credentials", Namespace: "default"},
				TunnelID:                       "12345678-1234-1234-1234-123456789abc",
			},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "cf-credentials", Namespace: "default"},
			Data:       map[string][]byte{"api-token": []byte("token")},
		},
	}
}

func reconcilePGGateway(t *testing.T, fakeClient client.WithWatch) gatewayv1.Gateway {
	t.Helper()

	reconciler := &GatewayReconciler{
		Client:         fakeClient,
		Scheme:         fakeClient.Scheme(),
		ControllerName: "test-controller",
		ConfigResolver: config.NewResolver(fakeClient, "default", cfmetrics.NewNoopCollector()),
		// The chart always wires --proxy-image; mirror that so the status path
		// does not classify these fixtures (no per-Gateway image override) as a
		// missing-image misconfig.
		ProxyImage: "ghcr.io/example/proxy:v1.2.3",
	}

	ctx := context.Background()

	_, err := reconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "pg-gateway", Namespace: "default"},
	})
	require.NoError(t, err)

	var updated gatewayv1.Gateway
	require.NoError(t, fakeClient.Get(ctx, types.NamespacedName{Name: "pg-gateway", Namespace: "default"}, &updated))

	return updated
}

// TestGatewayReconciler_PerGateway_AddressFromToken pins that an opted-in
// Gateway advertises ITS OWN tunnel's CNAME (parsed from the connector
// token), not the shared class tunnel.
func TestGatewayReconciler_PerGateway_AddressFromToken(t *testing.T) {
	t.Parallel()

	fakeClient := setupGatewayFakeClient(perGatewayStatusFixtures(t)...)
	updated := reconcilePGGateway(t, fakeClient)

	require.Len(t, updated.Status.Addresses, 1)
	assert.Equal(t, "550e8400-e29b-41d4-a716-446655440000.cfargotunnel.com", updated.Status.Addresses[0].Value,
		"the address must come from the per-Gateway connector token's tunnel ID")
}

// TestGatewayReconciler_PerGateway_ProgrammedGatesOnDeployment pins the
// Programmed semantics for dedicated data planes: no ready proxy replicas, no
// Programmed=True — the Gateway cannot serve traffic until a connector runs.
func TestGatewayReconciler_PerGateway_ProgrammedGatesOnDeployment(t *testing.T) {
	t.Parallel()

	fakeClient := setupGatewayFakeClient(perGatewayStatusFixtures(t)...)
	updated := reconcilePGGateway(t, fakeClient)

	programmed := findCondition(updated.Status.Conditions, string(gatewayv1.GatewayConditionProgrammed))
	require.NotNil(t, programmed)
	assert.Equal(t, metav1.ConditionFalse, programmed.Status,
		"no rendered deployment with ready replicas yet → not programmed")
	assert.Equal(t, string(gatewayv1.GatewayReasonPending), programmed.Reason)

	accepted := findCondition(updated.Status.Conditions, string(gatewayv1.GatewayConditionAccepted))
	require.NotNil(t, accepted)
	assert.Equal(t, metav1.ConditionTrue, accepted.Status, "pending data plane does not affect acceptance")
}

// TestGatewayReconciler_PerGateway_ProgrammedTrueWhenReady pins the happy
// path: ready proxy replicas flip Programmed to True.
func TestGatewayReconciler_PerGateway_ProgrammedTrueWhenReady(t *testing.T) {
	t.Parallel()

	objects := perGatewayStatusFixtures(t)
	objects = append(objects, &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "cf-proxy-pg-gateway", Namespace: "default"},
		Status:     appsv1.DeploymentStatus{ReadyReplicas: 2},
	})

	fakeClient := setupGatewayFakeClient(objects...)
	updated := reconcilePGGateway(t, fakeClient)

	programmed := findCondition(updated.Status.Conditions, string(gatewayv1.GatewayConditionProgrammed))
	require.NotNil(t, programmed)
	assert.Equal(t, metav1.ConditionTrue, programmed.Status)
}

// TestGatewayReconciler_PerGateway_ProgrammedTransientDeploymentReadKeepsPending
// pins the CURRENT behaviour of the transient Deployment-read branch: unlike
// the other transient paths (which propagate the error for controller-runtime
// backoff), a non-NotFound Get failure here is folded into
// Programmed=False/Pending and returned, not propagated. The Deployment watch +
// requeue self-heals, so this inconsistency is intentional and harmless; pin it
// so a future refactor that changes the shape is a conscious decision rather
// than a silent drift.
func TestGatewayReconciler_PerGateway_ProgrammedTransientDeploymentReadKeepsPending(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(gatewayv1.Install(scheme))
	utilruntime.Must(v1alpha1.AddToScheme(scheme))

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey,
				obj client.Object, opts ...client.GetOption,
			) error {
				if _, ok := obj.(*appsv1.Deployment); ok {
					return apierrors.NewInternalError(errSimulatedCacheMiss)
				}

				return cl.Get(ctx, key, obj, opts...)
			},
		}).
		Build()

	reconciler := &GatewayReconciler{Client: fakeClient, Scheme: scheme}

	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "pg-gateway", Namespace: "default"},
	}

	condition := reconciler.perGatewayProgrammedCondition(context.Background(), gateway, metav1.Now())

	assert.Equal(t, metav1.ConditionFalse, condition.Status,
		"a transient Deployment-read failure folds into Programmed=False, not a propagated error")
	assert.Equal(t, string(gatewayv1.GatewayReasonPending), condition.Reason)
	assert.Contains(t, condition.Message, "Failed to read per-Gateway proxy deployment")
}

// TestGatewayReconciler_PerGateway_TransientResolveErrorKeepsStatus pins the
// sentinel/transient split end to end: ResolveForGateway deliberately keeps a
// transient API failure's identity (only deterministic spec problems classify
// as ErrInvalidParameters), so the reconciler must NOT stamp
// Accepted=False/InvalidParameters over it — that would misreport a healthy
// spec and clear the listener statuses on every API hiccup. Transient errors
// propagate for controller-runtime backoff; the last written status stands.
func TestGatewayReconciler_PerGateway_TransientResolveErrorKeepsStatus(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(gatewayv1.Install(scheme))
	utilruntime.Must(v1alpha1.AddToScheme(scheme))

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(perGatewayStatusFixtures(t)...).
		WithStatusSubresource(&gatewayv1.Gateway{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey,
				obj client.Object, opts ...client.GetOption,
			) error {
				if _, ok := obj.(*v1alpha1.GatewayConfig); ok {
					return apierrors.NewInternalError(errSimulatedCacheMiss)
				}

				return cl.Get(ctx, key, obj, opts...)
			},
		}).
		Build()

	reconciler := &GatewayReconciler{
		Client:         fakeClient,
		Scheme:         fakeClient.Scheme(),
		ControllerName: "test-controller",
		ConfigResolver: config.NewResolver(fakeClient, "default", cfmetrics.NewNoopCollector()),
	}

	ctx := context.Background()

	_, err := reconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "pg-gateway", Namespace: "default"},
	})
	require.Error(t, err, "a transient resolve failure must propagate for backoff, not be swallowed")

	var updated gatewayv1.Gateway
	require.NoError(t, fakeClient.Get(ctx, types.NamespacedName{Name: "pg-gateway", Namespace: "default"}, &updated))

	accepted := findCondition(updated.Status.Conditions, string(gatewayv1.GatewayConditionAccepted))
	if accepted != nil {
		assert.NotEqual(t, string(gatewayv1.GatewayReasonInvalidParameters), accepted.Reason,
			"a momentary API failure must not be reported as a spec problem")
	}
}

// TestGatewayReconciler_PerGateway_InvalidParametersSurfaceOnStatus pins the
// spec-recommended shape: a broken parametersRef yields Accepted=False with
// reason InvalidParameters.
func TestGatewayReconciler_PerGateway_InvalidParametersSurfaceOnStatus(t *testing.T) {
	t.Parallel()

	objects := perGatewayStatusFixtures(t)
	filtered := make([]client.Object, 0, len(objects))

	for _, obj := range objects {
		if _, ok := obj.(*v1alpha1.GatewayConfig); ok {
			continue // dangling parametersRef
		}

		filtered = append(filtered, obj)
	}

	fakeClient := setupGatewayFakeClient(filtered...)
	updated := reconcilePGGateway(t, fakeClient)

	accepted := findCondition(updated.Status.Conditions, string(gatewayv1.GatewayConditionAccepted))
	require.NotNil(t, accepted)
	assert.Equal(t, metav1.ConditionFalse, accepted.Status)
	assert.Equal(t, string(gatewayv1.GatewayReasonInvalidParameters), accepted.Reason)
}

// TestGatewayReconciler_PerGateway_NoImageSurfacesInvalidParameters pins the
// diagnostic surface for the most common per-Gateway misconfig: a GatewayConfig
// with no spec.image on a controller with no --proxy-image default. The infra
// reconciler refuses to render the data plane (the Deployment never appears),
// so without this the Gateway would sit Programmed=Pending forever with the
// cause only in a transient Warning Event. The status path must instead report
// Accepted=False/InvalidParameters naming the missing image.
func TestGatewayReconciler_PerGateway_NoImageSurfacesInvalidParameters(t *testing.T) {
	t.Parallel()

	// Fixtures carry no GatewayConfig.spec.image; construct the reconciler with
	// an empty ProxyImage (no chart default) to reproduce the misconfig.
	fakeClient := setupGatewayFakeClient(perGatewayStatusFixtures(t)...)

	reconciler := &GatewayReconciler{
		Client:         fakeClient,
		Scheme:         fakeClient.Scheme(),
		ControllerName: "test-controller",
		ConfigResolver: config.NewResolver(fakeClient, "default", cfmetrics.NewNoopCollector()),
		ProxyImage:     "", // no chart default and no per-Gateway override
	}

	ctx := context.Background()
	_, err := reconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "pg-gateway", Namespace: "default"},
	})
	require.NoError(t, err, "a missing image is a deterministic spec problem, not a retryable error")

	var updated gatewayv1.Gateway
	require.NoError(t, fakeClient.Get(ctx, types.NamespacedName{Name: "pg-gateway", Namespace: "default"}, &updated))

	accepted := findCondition(updated.Status.Conditions, string(gatewayv1.GatewayConditionAccepted))
	require.NotNil(t, accepted, "the Gateway must carry an Accepted condition, not sit statusless")
	assert.Equal(t, metav1.ConditionFalse, accepted.Status)
	assert.Equal(t, string(gatewayv1.GatewayReasonInvalidParameters), accepted.Reason)
	assert.Contains(t, accepted.Message, "proxy image",
		"the condition message must name the missing image, not just say Pending")
}
