package controller

import (
	"context"
	"strings"
	"testing"
	"time"

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
	"k8s.io/client-go/tools/events"
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

// TestGatewayReconciler_PerGateway_ClassTunnelClaimIsRejected pins the status
// half of the tunnel-ownership rule. A connector token proves nothing about
// the tunnel it names, and tunnel IDs are published in Gateway status for
// external-dns, so a tenant can point a dedicated plane at the class tunnel
// and be handed every shared route. The route syncer refuses to program such a
// Gateway; this is the operator-facing half of that refusal, and the two must
// agree — they share internal/tunnelownership.
//
// Without a status the refusal would be invisible: the Gateway would look
// healthy while none of its routes were ever programmed.
func TestGatewayReconciler_PerGateway_ClassTunnelClaimIsRejected(t *testing.T) {
	t.Parallel()

	objects := perGatewayStatusFixtures(t)

	// Point the dedicated plane's token at the CLASS tunnel.
	for _, obj := range objects {
		secret, ok := obj.(*corev1.Secret)
		if !ok || secret.Name != "pg-token" {
			continue
		}

		secret.Data["tunnel-token"] = []byte(infraTunnelTokenFor(t, "12345678-1234-1234-1234-123456789abc"))
	}

	fakeClient := setupGatewayFakeClient(objects...)
	updated := reconcilePGGateway(t, fakeClient)

	accepted := findCondition(updated.Status.Conditions, string(gatewayv1.GatewayConditionAccepted))
	require.NotNil(t, accepted, "a refused Gateway must carry an Accepted condition, not sit statusless")
	assert.Equal(t, metav1.ConditionFalse, accepted.Status)
	assert.Equal(t, string(gatewayv1.GatewayReasonInvalidParameters), accepted.Reason)
	assert.Contains(t, accepted.Message, "tunnel",
		"the message must name the contested tunnel so the operator can act on it")
	assert.True(t, strings.HasPrefix(accepted.Message, "Refused: "),
		"a refusal must not read as a failure to resolve: the configuration resolved fine, "+
			"got %q", accepted.Message)

	// Advertising IS possession here, so a refusal that wrote the address would
	// convert itself into the claim it just denied — and possession beats age,
	// so the refused Gateway would then outrank the holder on the next pass.
	assert.Empty(t, updated.Status.Addresses,
		"a refused Gateway must not start advertising the tunnel it was refused")
}

// TestGatewayReconciler_PerGateway_OptInSuppressesRejection pins that the
// status layer honours the same AllowSharedTunnels escape hatch the route
// syncer does. If only one layer read the flag, an operator who enabled
// sharing would get Gateways permanently reporting Accepted=False while their
// routes were programmed perfectly — the status-vs-behaviour divergence the
// shared vector table exists to prevent.
func TestGatewayReconciler_PerGateway_OptInSuppressesRejection(t *testing.T) {
	t.Parallel()

	objects := perGatewayStatusFixtures(t)

	for _, obj := range objects {
		if secret, ok := obj.(*corev1.Secret); ok && secret.Name == "pg-token" {
			secret.Data["tunnel-token"] = []byte(infraTunnelTokenFor(t, "12345678-1234-1234-1234-123456789abc"))
		}

		if classConfig, ok := obj.(*v1alpha1.GatewayClassConfig); ok {
			classConfig.Spec.AllowSharedTunnels = true
		}
	}

	fakeClient := setupGatewayFakeClient(objects...)
	updated := reconcilePGGateway(t, fakeClient)

	accepted := findCondition(updated.Status.Conditions, string(gatewayv1.GatewayConditionAccepted))
	require.NotNil(t, accepted)
	assert.Equal(t, metav1.ConditionTrue, accepted.Status,
		"with sharing opted in, a class-tunnel claim must not be refused in status either")
}

// TestGatewayReconciler_PerGateway_ConfigErrorKeepsTheTunnelAddress pins the
// half of the possession rule that lives in the status writer.
//
// Tunnel ownership is decided by which Gateway advertises the tunnel, so
// clearing the address on a configuration error would surrender the claim. A
// delete-then-create token rotation is enough to open that window, after which
// another namespace can take the tunnel and the owner is locked out for good.
// The class chain still clears — only a Gateway with its own data plane keeps
// the address, because the address describes the tunnel that plane is attached
// to and a failed read does not detach it.
func TestGatewayReconciler_PerGateway_ConfigErrorKeepsTheTunnelAddress(t *testing.T) {
	t.Parallel()

	objects := perGatewayStatusFixtures(t)
	filtered := make([]client.Object, 0, len(objects))

	for _, obj := range objects {
		// Drop the token Secret: a deterministic resolve failure.
		if secret, ok := obj.(*corev1.Secret); ok && secret.Name == "pg-token" {
			continue
		}

		if gateway, ok := obj.(*gatewayv1.Gateway); ok {
			gateway.Status.Addresses = []gatewayv1.GatewayStatusAddress{
				{Value: "550e8400-e29b-41d4-a716-446655440000" + cfArgotunnelSuffix},
			}
		}

		filtered = append(filtered, obj)
	}

	fakeClient := setupGatewayFakeClient(filtered...)
	updated := reconcilePGGateway(t, fakeClient)

	require.Len(t, updated.Status.Addresses, 1,
		"a per-Gateway Gateway must keep advertising its tunnel across a config error")
	assert.Equal(t, "550e8400-e29b-41d4-a716-446655440000"+cfArgotunnelSuffix,
		updated.Status.Addresses[0].Value)
}

// TestGatewayReconciler_PerGateway_UncomputableArbitrationDoesNotAdvertise
// pins that a Gateway never advertises a tunnel whose ownership could not be
// decided.
//
// An advertised address IS possession, and possession beats age. Writing one
// during a transient read failure would seat the claimant as holder and let it
// keep the seat afterwards, evicting the rightful owner and taking its data
// plane down with it. A failure that leaves ownership unknown must requeue,
// not fall through to the status writer.
func TestGatewayReconciler_PerGateway_UncomputableArbitrationDoesNotAdvertise(t *testing.T) {
	t.Parallel()

	objects := perGatewayStatusFixtures(t)
	filtered := make([]client.Object, 0, len(objects))

	for _, obj := range objects {
		// Drop the GatewayClassConfig so the class tunnel is unknowable and
		// arbitration cannot be computed.
		if _, ok := obj.(*v1alpha1.GatewayClassConfig); ok {
			continue
		}

		// Give the GatewayConfig its own credentials so the per-Gateway
		// resolve does not need the class chain either — otherwise Reconcile
		// fails earlier and never reaches the arbitration step.
		if gwConfig, ok := obj.(*v1alpha1.GatewayConfig); ok {
			gwConfig.Spec.CloudflareCredentialsSecretRef = &v1alpha1.LocalSecretReference{Name: "pg-creds"}
		}

		filtered = append(filtered, obj)
	}

	filtered = append(filtered, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "pg-creds", Namespace: "default"},
		Data:       map[string][]byte{"api-token": []byte("tenant-token")},
	})

	fakeClient := setupGatewayFakeClient(filtered...)

	reconciler := &GatewayReconciler{
		Client:         fakeClient,
		Scheme:         fakeClient.Scheme(),
		ControllerName: "test-controller",
		ConfigResolver: config.NewResolver(fakeClient, "default", cfmetrics.NewNoopCollector()),
		ProxyImage:     "ghcr.io/example/proxy:v1",
	}

	ctx := context.Background()
	_, err := reconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "pg-gateway", Namespace: "default"},
	})
	require.Error(t, err, "an uncomputable arbitration must requeue, not be swallowed")

	var updated gatewayv1.Gateway
	require.NoError(t, fakeClient.Get(ctx, types.NamespacedName{Name: "pg-gateway", Namespace: "default"}, &updated))

	assert.Empty(t, updated.Status.Addresses,
		"a Gateway must not advertise a tunnel whose ownership is unknown: the address is what confers possession")
}

// TestGatewayReconciler_PerGateway_RefusalEventFiresOncePerVerdict pins the
// operator's primary signal for a refused tunnel claim.
//
// The isolation guide points operators at `TunnelClaimRejected` Warning Events
// to detect a squatted tunnel, so the Event firing at all is a documented
// contract. The refusal also requeues every 30s for as long as the claim
// stands, so it must NOT re-fire on an unchanged verdict — and it must fire
// again when the verdict changes, since a different reason carries a different
// remedy.
func TestGatewayReconciler_PerGateway_RefusalEventFiresOncePerVerdict(t *testing.T) {
	t.Parallel()

	const classTunnel = "12345678-1234-1234-1234-123456789abc"

	objects := perGatewayStatusFixtures(t)
	for _, obj := range objects {
		if secret, ok := obj.(*corev1.Secret); ok && secret.Name == "pg-token" {
			secret.Data["tunnel-token"] = []byte(infraTunnelTokenFor(t, classTunnel))
		}
	}

	fakeClient := setupGatewayFakeClient(objects...)
	recorder := events.NewFakeRecorder(10)

	reconciler := &GatewayReconciler{
		Client:         fakeClient,
		Scheme:         fakeClient.Scheme(),
		ControllerName: "test-controller",
		ConfigResolver: config.NewResolver(fakeClient, "default", cfmetrics.NewNoopCollector()),
		ProxyImage:     "ghcr.io/example/proxy:v1",
		Recorder:       recorder,
		ViewStore:      newMergeViewStore(),
	}

	ctx := context.Background()
	request := ctrl.Request{NamespacedName: types.NamespacedName{Name: "pg-gateway", Namespace: "default"}}

	_, err := reconciler.Reconcile(ctx, request)
	require.NoError(t, err)
	assert.Len(t, drainEvents(recorder), 1, "the first refusal must reach the operator as a Warning Event")

	_, err = reconciler.Reconcile(ctx, request)
	require.NoError(t, err)
	assert.Empty(t, drainEvents(recorder),
		"an unchanged verdict must not re-event on every 30s requeue: the refused tenant would "+
			"otherwise choose the volume")

	// Same Gateway, different verdict: the class tunnel moves elsewhere, so
	// the refusal is now "a neighbour holds it" — a different remedy, and news
	// the operator has not heard. Keying the dedup on the tunnel ID alone
	// would stay silent here.
	var classConfig v1alpha1.GatewayClassConfig
	require.NoError(t, fakeClient.Get(ctx, types.NamespacedName{Name: "test-config"}, &classConfig))

	classConfig.Spec.TunnelID = "99999999-9999-4999-8999-999999999999"
	require.NoError(t, fakeClient.Update(ctx, &classConfig))

	neighbour := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "neighbour",
			Namespace:         "other-team",
			UID:               "other-team/neighbour",
			CreationTimestamp: metav1.NewTime(time.Date(2020, time.January, 1, 0, 0, 0, 0, time.UTC)),
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "cloudflare-tunnel",
			Listeners:        []gatewayv1.Listener{{Name: "http", Port: 80, Protocol: "HTTP"}},
			Infrastructure: &gatewayv1.GatewayInfrastructure{
				ParametersRef: &gatewayv1.LocalParametersReference{
					Group: "cf.k8s.lex.la", Kind: "GatewayConfig", Name: "pg-config",
				},
			},
		},
		Status: gatewayv1.GatewayStatus{
			Addresses: []gatewayv1.GatewayStatusAddress{{Value: classTunnel + cfArgotunnelSuffix}},
		},
	}
	require.NoError(t, fakeClient.Create(ctx, neighbour))
	require.NoError(t, fakeClient.Status().Update(ctx, neighbour))
	require.NoError(t, fakeClient.Create(ctx, &v1alpha1.GatewayConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "pg-config", Namespace: "other-team"},
		Spec: v1alpha1.GatewayConfigSpec{
			TunnelTokenSecretRef: v1alpha1.LocalSecretReference{Name: "pg-token"},
		},
	}))
	require.NoError(t, fakeClient.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "pg-token", Namespace: "other-team"},
		Data:       map[string][]byte{"tunnel-token": []byte(infraTunnelTokenFor(t, classTunnel))},
	}))

	_, err = reconciler.Reconcile(ctx, request)
	require.NoError(t, err)
	assert.Len(t, drainEvents(recorder), 1,
		"a changed verdict carries a different remedy and must reach the operator again")
}

// TestGatewayReconciler_PerGateway_BrokenClassIsReportedNotRetriedForever pins
// the deterministic half of an uncomputable arbitration.
//
// A GatewayClass whose parametersRef is missing or the wrong kind is a
// permanent misconfiguration. Retrying it forever would leave every dedicated
// plane on that class with no Accepted condition at all, so the only evidence
// would be controller logs. It must be reported like any other deterministic
// config error — and still advertise nothing, since an address is possession.
func TestGatewayReconciler_PerGateway_BrokenClassIsReportedNotRetriedForever(t *testing.T) {
	t.Parallel()

	objects := perGatewayStatusFixtures(t)
	for _, obj := range objects {
		if class, ok := obj.(*gatewayv1.GatewayClass); ok {
			class.Spec.ParametersRef = nil
		}

		// Own credentials, so the per-Gateway resolve does not need the class
		// chain and Reconcile reaches the arbitration step.
		if gwConfig, ok := obj.(*v1alpha1.GatewayConfig); ok {
			gwConfig.Spec.CloudflareCredentialsSecretRef = &v1alpha1.LocalSecretReference{Name: "pg-creds"}
		}
	}

	objects = append(objects, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "pg-creds", Namespace: "default"},
		Data:       map[string][]byte{"api-token": []byte("tenant-token")},
	})

	fakeClient := setupGatewayFakeClient(objects...)
	updated := reconcilePGGateway(t, fakeClient)

	accepted := findCondition(updated.Status.Conditions, string(gatewayv1.GatewayConditionAccepted))
	require.NotNil(t, accepted, "a permanently broken class must produce a condition, not just log lines")
	assert.Equal(t, metav1.ConditionFalse, accepted.Status)
	assert.Equal(t, string(gatewayv1.GatewayReasonInvalidParameters), accepted.Reason)

	assert.Empty(t, updated.Status.Addresses,
		"reporting the error must not START advertising a tunnel whose ownership is undecided; "+
			"an address this Gateway already had would be preserved, which is what keeps possession "+
			"across a config error")
}
