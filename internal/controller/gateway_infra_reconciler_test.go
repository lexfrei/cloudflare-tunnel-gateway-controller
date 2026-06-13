package controller

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/api/v1alpha1"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/cfmetrics"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/config"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/render"
)

const infraNamespace = "tenant-a"

func infraTunnelToken(t *testing.T) string {
	t.Helper()

	return infraTunnelTokenFor(t, "550e8400-e29b-41d4-a716-446655440000")
}

// infraTunnelTokenFor builds a valid connector token for a specific tunnel ID,
// so a test can simulate a token ROTATION to a different tunnel.
func infraTunnelTokenFor(t *testing.T, tunnelID string) string {
	t.Helper()

	payload, err := json.Marshal(map[string]any{
		"a": "abcdef0123456789abcdef0123456789",
		"s": base64.StdEncoding.EncodeToString([]byte("secret")),
		"t": tunnelID,
	})
	require.NoError(t, err)

	return base64.StdEncoding.EncodeToString(payload)
}

// infraFixtures builds the full per-Gateway opt-in object set: managed
// GatewayClass chain, Gateway with infrastructure.parametersRef,
// GatewayConfig, and the connector-token Secret.
func infraFixtures(t *testing.T) []runtime.Object {
	t.Helper()

	return []runtime.Object{
		&gatewayv1.GatewayClass{
			ObjectMeta: metav1.ObjectMeta{Name: "cloudflare-tunnel"},
			Spec: gatewayv1.GatewayClassSpec{
				ControllerName: "cf.k8s.lex.la/tunnel-controller",
				ParametersRef: &gatewayv1.ParametersReference{
					Group: "cf.k8s.lex.la", Kind: "GatewayClassConfig", Name: "class-config",
				},
			},
		},
		&v1alpha1.GatewayClassConfig{
			ObjectMeta: metav1.ObjectMeta{Name: "class-config"},
			Spec: v1alpha1.GatewayClassConfigSpec{
				TunnelID: "99999999-9999-4999-8999-999999999999",
				CloudflareCredentialsSecretRef: v1alpha1.SecretReference{
					Name: "class-credentials", Namespace: "cf-system",
				},
			},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "class-credentials", Namespace: "cf-system"},
			Data:       map[string][]byte{"api-token": []byte("class-api-token")},
		},
		&gatewayv1.Gateway{
			ObjectMeta: metav1.ObjectMeta{Name: "edge", Namespace: infraNamespace, UID: "gw-uid-1"},
			Spec: gatewayv1.GatewaySpec{
				GatewayClassName: "cloudflare-tunnel",
				Infrastructure: &gatewayv1.GatewayInfrastructure{
					ParametersRef: &gatewayv1.LocalParametersReference{
						Group: "cf.k8s.lex.la", Kind: "GatewayConfig", Name: "edge-config",
					},
				},
			},
		},
		&v1alpha1.GatewayConfig{
			ObjectMeta: metav1.ObjectMeta{Name: "edge-config", Namespace: infraNamespace},
			Spec: v1alpha1.GatewayConfigSpec{
				TunnelTokenSecretRef: v1alpha1.LocalSecretReference{Name: "edge-token"},
			},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "edge-token", Namespace: infraNamespace},
			Data:       map[string][]byte{"tunnel-token": []byte(infraTunnelToken(t))},
		},
	}
}

func newInfraReconciler(t *testing.T, objects ...runtime.Object) *GatewayInfraReconciler {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, gatewayv1.Install(scheme))
	require.NoError(t, v1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, autoscalingv2.AddToScheme(scheme))

	builder := fake.NewClientBuilder().WithScheme(scheme)
	for _, obj := range objects {
		builder = builder.WithRuntimeObjects(obj)
	}

	fakeClient := builder.Build()

	return &GatewayInfraReconciler{
		Client:         fakeClient,
		Scheme:         scheme,
		ControllerName: "cf.k8s.lex.la/tunnel-controller",
		ConfigResolver: config.NewResolver(fakeClient, "cf-system", cfmetrics.NewNoopCollector()),
		RenderDefaults: render.Defaults{ProxyImage: "ghcr.io/example/proxy:v1.2.3"},
	}
}

func reconcileEdge(t *testing.T, reconciler *GatewayInfraReconciler) {
	t.Helper()

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "edge", Namespace: infraNamespace},
	})
	require.NoError(t, err)
}

// TestGatewayInfraReconciler_RendersDataPlane pins the core contract: a
// Gateway opted in via infrastructure.parametersRef gets a proxy Deployment
// and a headless config Service in its namespace, both controller-owned (the
// ownerRef is the GC mechanism on Gateway deletion).
func TestGatewayInfraReconciler_RendersDataPlane(t *testing.T) {
	t.Parallel()

	reconciler := newInfraReconciler(t, infraFixtures(t)...)
	reconcileEdge(t, reconciler)

	var deployment appsv1.Deployment
	require.NoError(t, reconciler.Get(context.Background(),
		types.NamespacedName{Name: "cf-proxy-edge", Namespace: infraNamespace}, &deployment))

	assert.Equal(t, "ghcr.io/example/proxy:v1.2.3", deployment.Spec.Template.Spec.Containers[0].Image)

	require.Len(t, deployment.OwnerReferences, 1)
	assert.Equal(t, "Gateway", deployment.OwnerReferences[0].Kind)
	assert.Equal(t, "edge", deployment.OwnerReferences[0].Name)
	require.NotNil(t, deployment.OwnerReferences[0].Controller)
	assert.True(t, *deployment.OwnerReferences[0].Controller)

	var service corev1.Service
	require.NoError(t, reconciler.Get(context.Background(),
		types.NamespacedName{Name: "cf-proxy-edge-config", Namespace: infraNamespace}, &service))
	assert.True(t, service.Spec.PublishNotReadyAddresses)
	require.Len(t, service.OwnerReferences, 1)
	assert.Equal(t, "edge", service.OwnerReferences[0].Name)
}

// TestGatewayInfraReconciler_HealsDrift pins self-healing: a manual edit to
// the rendered Deployment is reverted on the next reconcile.
func TestGatewayInfraReconciler_HealsDrift(t *testing.T) {
	t.Parallel()

	reconciler := newInfraReconciler(t, infraFixtures(t)...)
	reconcileEdge(t, reconciler)

	ctx := context.Background()
	key := types.NamespacedName{Name: "cf-proxy-edge", Namespace: infraNamespace}

	var deployment appsv1.Deployment
	require.NoError(t, reconciler.Get(ctx, key, &deployment))
	deployment.Spec.Template.Spec.Containers[0].Image = "evil/image:latest"
	require.NoError(t, reconciler.Update(ctx, &deployment))

	reconcileEdge(t, reconciler)

	require.NoError(t, reconciler.Get(ctx, key, &deployment))
	assert.Equal(t, "ghcr.io/example/proxy:v1.2.3", deployment.Spec.Template.Spec.Containers[0].Image,
		"the reconciler must restore the rendered spec")
}

// TestGatewayInfraReconciler_PreservesHPAOwnedReplicas pins replica
// ownership: with autoscaling configured the reconciler must NOT reset the
// replica count the HPA set.
func TestGatewayInfraReconciler_PreservesHPAOwnedReplicas(t *testing.T) {
	t.Parallel()

	objects := infraFixtures(t)
	for _, obj := range objects {
		if gwConfig, ok := obj.(*v1alpha1.GatewayConfig); ok {
			gwConfig.Spec.Autoscaling = &v1alpha1.ProxyAutoscaling{MaxReplicas: 10, TargetInflightPerPod: 50}
		}
	}

	reconciler := newInfraReconciler(t, objects...)
	reconcileEdge(t, reconciler)

	ctx := context.Background()
	key := types.NamespacedName{Name: "cf-proxy-edge", Namespace: infraNamespace}

	// Simulate the HPA scaling to 7.
	var deployment appsv1.Deployment
	require.NoError(t, reconciler.Get(ctx, key, &deployment))
	seven := int32(7)
	deployment.Spec.Replicas = &seven
	require.NoError(t, reconciler.Update(ctx, &deployment))

	reconcileEdge(t, reconciler)

	require.NoError(t, reconciler.Get(ctx, key, &deployment))
	require.NotNil(t, deployment.Spec.Replicas)
	assert.Equal(t, int32(7), *deployment.Spec.Replicas,
		"autoscaling mode: the HPA owns the replica count, the reconciler must not fight it")
}

// TestGatewayInfraReconciler_SharedModeCleansUp pins the opt-out path:
// removing infrastructure.parametersRef deletes the previously-rendered
// resources (the Gateway is alive, so ownerRef GC alone cannot do it).
func TestGatewayInfraReconciler_SharedModeCleansUp(t *testing.T) {
	t.Parallel()

	reconciler := newInfraReconciler(t, infraFixtures(t)...)
	reconcileEdge(t, reconciler)

	ctx := context.Background()

	var gateway gatewayv1.Gateway
	require.NoError(t, reconciler.Get(ctx, types.NamespacedName{Name: "edge", Namespace: infraNamespace}, &gateway))
	gateway.Spec.Infrastructure = nil
	require.NoError(t, reconciler.Update(ctx, &gateway))

	reconcileEdge(t, reconciler)

	var deployment appsv1.Deployment
	err := reconciler.Get(ctx, types.NamespacedName{Name: "cf-proxy-edge", Namespace: infraNamespace}, &deployment)
	assert.Error(t, err, "rendered Deployment must be deleted when the Gateway opts back out")

	var service corev1.Service
	err = reconciler.Get(ctx, types.NamespacedName{Name: "cf-proxy-edge-config", Namespace: infraNamespace}, &service)
	assert.Error(t, err, "rendered Service must be deleted when the Gateway opts back out")
}

// TestGatewayInfraReconciler_TriggersRouteSyncOnCreate pins the readiness
// bootstrap: a freshly-rendered per-Gateway proxy needs an initial config
// push to pass /readyz (config version > 0), exactly as the shared proxy gets
// one from the startup sync. Route reconciles are route-event-driven, so a
// data plane with no routes yet would never be synced — the reconciler must
// trigger a full route sync when it CREATES a data plane so the new
// partition's (possibly empty) config is cached and delivered.
func TestGatewayInfraReconciler_TriggersRouteSyncOnCreate(t *testing.T) {
	t.Parallel()

	var syncs int

	reconciler := newInfraReconciler(t, infraFixtures(t)...)
	reconciler.TriggerRouteSync = func(context.Context) error {
		syncs++

		return nil
	}

	reconcileEdge(t, reconciler)
	assert.Equal(t, 1, syncs, "creating a data plane must trigger a route sync so the new proxy gets its initial config")

	// A second reconcile (no creation) must NOT re-trigger — avoid sync storms.
	reconcileEdge(t, reconciler)
	assert.Equal(t, 1, syncs, "a steady-state reconcile must not re-trigger a full route sync")
}

// TestGatewayInfraReconciler_OptOutSucceedsWithoutSecretDelete pins the
// least-privilege cleanup contract: the controller's RBAC grants
// create-but-not-delete on Secrets, so opt-out must NOT issue a Secret Delete
// (which would 403 in production and wedge the reconcile in a requeue loop).
// The generated Secret survives opt-out and is GC'd only on Gateway deletion;
// the Deployment and Service ARE deleted.
func TestGatewayInfraReconciler_OptOutSucceedsWithoutSecretDelete(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, gatewayv1.Install(scheme))
	require.NoError(t, v1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, autoscalingv2.AddToScheme(scheme))

	builder := fake.NewClientBuilder().WithScheme(scheme)
	for _, obj := range infraFixtures(t) {
		builder = builder.WithRuntimeObjects(obj)
	}

	var secretDeletes int

	fakeClient := builder.WithInterceptorFuncs(interceptor.Funcs{
		Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if _, ok := obj.(*corev1.Secret); ok {
				secretDeletes++

				return apierrors.NewForbidden(
					schema.GroupResource{Resource: "secrets"}, obj.GetName(), errSimulatedCacheMiss)
			}

			return cl.Delete(ctx, obj, opts...)
		},
	}).Build()

	reconciler := &GatewayInfraReconciler{
		Client:         fakeClient,
		Scheme:         scheme,
		ControllerName: "cf.k8s.lex.la/tunnel-controller",
		ConfigResolver: config.NewResolver(fakeClient, "cf-system", cfmetrics.NewNoopCollector()),
		RenderDefaults: render.Defaults{ProxyImage: "ghcr.io/example/proxy:v1.2.3"},
	}

	reconcileEdge(t, reconciler)

	ctx := context.Background()

	var gateway gatewayv1.Gateway
	require.NoError(t, reconciler.Get(ctx, types.NamespacedName{Name: "edge", Namespace: infraNamespace}, &gateway))
	gateway.Spec.Infrastructure = nil
	require.NoError(t, reconciler.Update(ctx, &gateway))

	// Opt-out cleanup must succeed under a Secret-delete-forbidding client.
	_, err := reconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "edge", Namespace: infraNamespace},
	})
	require.NoError(t, err, "opt-out must not depend on Secret delete (RBAC grants no delete on Secrets)")
	assert.Zero(t, secretDeletes, "cleanup must never attempt to delete the generated Secret")

	var deployment appsv1.Deployment
	assert.Error(t, reconciler.Get(ctx,
		types.NamespacedName{Name: "cf-proxy-edge", Namespace: infraNamespace}, &deployment),
		"the Deployment must be deleted on opt-out")

	var secret corev1.Secret
	assert.NoError(t, reconciler.Get(ctx,
		types.NamespacedName{Name: "cf-proxy-edge-auth", Namespace: infraNamespace}, &secret),
		"the generated Secret survives opt-out (GC'd on Gateway deletion via ownerRef)")
}

// TestGatewayInfraReconciler_RotationTriggersRouteSync pins that a
// GatewayConfig change that re-renders the data plane (here: a connector-token
// rotation to a new tunnel) triggers a full route sync. No route controller
// watches GatewayConfig, so without this the Cloudflare document and the proxy
// push would keep the OLD tunnel/token until an unrelated route or Secret
// event — a stale, unprogrammed data plane after a rotation.
func TestGatewayInfraReconciler_RotationTriggersRouteSync(t *testing.T) {
	t.Parallel()

	reconciler := newInfraReconciler(t, infraFixtures(t)...)

	var routeSyncs int
	reconciler.TriggerRouteSync = func(context.Context) error {
		routeSyncs++

		return nil
	}

	ctx := context.Background()

	reconcileEdge(t, reconciler)
	require.Equal(t, 1, routeSyncs, "creating the data plane triggers the initial route sync")

	// Rotate the connector token to a different tunnel — re-renders the
	// Deployment (new token hash), an Update, not a Create.
	var secret corev1.Secret
	require.NoError(t, reconciler.Get(ctx,
		types.NamespacedName{Name: "edge-token", Namespace: infraNamespace}, &secret))
	secret.Data["tunnel-token"] = []byte(infraTunnelTokenFor(t, "660e8400-e29b-41d4-a716-446655440099"))
	require.NoError(t, reconciler.Update(ctx, &secret))

	reconcileEdge(t, reconciler)
	assert.Equal(t, 2, routeSyncs, "a rotation that re-renders the data plane must re-trigger the route sync")

	// A steady-state reconcile (no spec change) must NOT re-sync.
	reconcileEdge(t, reconciler)
	assert.Equal(t, 2, routeSyncs, "a no-op reconcile must not trigger a redundant route sync")
}

// TestGatewayInfraReconciler_OptOutLeavesOwnerRefStrippedObjectAsOrphan pins
// the deleteIfOwned contract: opt-out does NOT delete a rendered object whose
// ownerRef has been stripped. "Never delete what we cannot prove we own"
// outranks "always clean up", so a re-parented or collision object survives
// opt-out as an orphan rather than being deleted.
func TestGatewayInfraReconciler_OptOutLeavesOwnerRefStrippedObjectAsOrphan(t *testing.T) {
	t.Parallel()

	reconciler := newInfraReconciler(t, infraFixtures(t)...)
	reconcileEdge(t, reconciler)

	ctx := context.Background()
	deploymentKey := types.NamespacedName{Name: "cf-proxy-edge", Namespace: infraNamespace}

	// Strip the controller ownerRef from the rendered Deployment.
	var deployment appsv1.Deployment
	require.NoError(t, reconciler.Get(ctx, deploymentKey, &deployment))
	deployment.OwnerReferences = nil
	require.NoError(t, reconciler.Update(ctx, &deployment))

	// Opt out.
	var gateway gatewayv1.Gateway
	require.NoError(t, reconciler.Get(ctx, types.NamespacedName{Name: "edge", Namespace: infraNamespace}, &gateway))
	gateway.Spec.Infrastructure = nil
	require.NoError(t, reconciler.Update(ctx, &gateway))

	_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "edge", Namespace: infraNamespace}})
	require.NoError(t, err)

	assert.NoError(t, reconciler.Get(ctx, deploymentKey, &deployment),
		"an ownerRef-stripped object must survive opt-out as an orphan, never be deleted")
}

// TestGatewayInfraReconciler_PostRenderInvalidationRetainsResources pins the
// fail-closed-keep-last-state behaviour: a Gateway whose config breaks AFTER a
// healthy render keeps its last-good Deployment/Service running, rather than
// tearing down a serving proxy on a mid-edit invalid spec. Cleanup is reserved
// for explicit opt-out or Gateway deletion.
func TestGatewayInfraReconciler_PostRenderInvalidationRetainsResources(t *testing.T) {
	t.Parallel()

	reconciler := newInfraReconciler(t, infraFixtures(t)...)
	reconcileEdge(t, reconciler)

	ctx := context.Background()

	// Confirm the healthy render exists, then break the config: delete the
	// connector-token Secret so resolution fails with InvalidParameters.
	var deployment appsv1.Deployment
	require.NoError(t, reconciler.Get(ctx,
		types.NamespacedName{Name: "cf-proxy-edge", Namespace: infraNamespace}, &deployment))

	require.NoError(t, reconciler.Delete(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "edge-token", Namespace: infraNamespace},
	}))

	reconcileEdge(t, reconciler)

	assert.NoError(t, reconciler.Get(ctx,
		types.NamespacedName{Name: "cf-proxy-edge", Namespace: infraNamespace}, &deployment),
		"a post-render config breakage must NOT tear down the last-good data plane")
}

// TestGatewayInfraReconciler_InvalidParametersRendersNothing pins fail-safe
// behaviour: an invalid parametersRef renders nothing and does NOT error the
// reconcile (the watches re-trigger when the user fixes the referent; the
// status surface is the Gateway reconciler's InvalidParameters condition).
func TestGatewayInfraReconciler_InvalidParametersRendersNothing(t *testing.T) {
	t.Parallel()

	objects := infraFixtures(t)
	// Drop the GatewayConfig so the ref dangles.
	filtered := make([]runtime.Object, 0, len(objects))

	for _, obj := range objects {
		if _, ok := obj.(*v1alpha1.GatewayConfig); ok {
			continue
		}

		filtered = append(filtered, obj)
	}

	reconciler := newInfraReconciler(t, filtered...)
	reconcileEdge(t, reconciler)

	var deployment appsv1.Deployment
	err := reconciler.Get(context.Background(),
		types.NamespacedName{Name: "cf-proxy-edge", Namespace: infraNamespace}, &deployment)
	assert.Error(t, err, "nothing must be rendered for an invalid parametersRef")
}

// TestGatewayInfraReconciler_IgnoresForeignGateways pins scoping: Gateways of
// another controller are left untouched even when they carry a parametersRef.
func TestGatewayInfraReconciler_IgnoresForeignGateways(t *testing.T) {
	t.Parallel()

	objects := infraFixtures(t)
	objects = append(objects, &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "other-class"},
		Spec:       gatewayv1.GatewayClassSpec{ControllerName: "example.com/other"},
	})

	for _, obj := range objects {
		if gateway, ok := obj.(*gatewayv1.Gateway); ok {
			gateway.Spec.GatewayClassName = "other-class"
		}
	}

	reconciler := newInfraReconciler(t, objects...)
	reconcileEdge(t, reconciler)

	var deployment appsv1.Deployment
	err := reconciler.Get(context.Background(),
		types.NamespacedName{Name: "cf-proxy-edge", Namespace: infraNamespace}, &deployment)
	assert.Error(t, err)
}

// TestGatewayInfraReconciler_NoProxyImageRendersNothing pins the
// misconfiguration guard: with no --proxy-image configured and no per-Gateway
// image override, rendering would produce a Deployment with an empty image
// that the apiserver rejects on every reconcile. The reconciler must instead
// render nothing and surface the problem via Event (manual installs that do
// not pass the flag hit this).
func TestGatewayInfraReconciler_NoProxyImageRendersNothing(t *testing.T) {
	t.Parallel()

	reconciler := newInfraReconciler(t, infraFixtures(t)...)
	reconciler.RenderDefaults.ProxyImage = ""

	reconcileEdge(t, reconciler)

	var deployment appsv1.Deployment
	err := reconciler.Get(context.Background(),
		types.NamespacedName{Name: "cf-proxy-edge", Namespace: infraNamespace}, &deployment)
	assert.Error(t, err, "no Deployment may be rendered without a resolvable proxy image")
}

// TestGatewayInfraReconciler_GeneratesAuthSecret pins the fail-secure
// default: a GatewayConfig without an explicit authTokenSecretRef gets a
// controller-generated, controller-owned auth Secret with a non-empty token,
// and the rendered Deployment wires PROXY_AUTH_TOKEN to it.
func TestGatewayInfraReconciler_GeneratesAuthSecret(t *testing.T) {
	t.Parallel()

	reconciler := newInfraReconciler(t, infraFixtures(t)...)
	reconcileEdge(t, reconciler)

	var secret corev1.Secret
	require.NoError(t, reconciler.Get(context.Background(),
		types.NamespacedName{Name: "cf-proxy-edge-auth", Namespace: infraNamespace}, &secret))

	assert.NotEmpty(t, secret.Data["auth-token"], "the generated auth token must not be empty")
	require.Len(t, secret.OwnerReferences, 1)
	assert.Equal(t, "edge", secret.OwnerReferences[0].Name, "the Secret must be GC-owned by the Gateway")

	var deployment appsv1.Deployment
	require.NoError(t, reconciler.Get(context.Background(),
		types.NamespacedName{Name: "cf-proxy-edge", Namespace: infraNamespace}, &deployment))

	var wired bool

	for _, env := range deployment.Spec.Template.Spec.Containers[0].Env {
		if env.Name == "PROXY_AUTH_TOKEN" && env.ValueFrom.SecretKeyRef.Name == "cf-proxy-edge-auth" {
			wired = true
		}
	}

	assert.True(t, wired, "PROXY_AUTH_TOKEN must reference the generated Secret")
}

// TestGatewayInfraReconciler_NeverRotatesGeneratedAuthToken pins the
// create-only contract: a second reconcile must NOT mint a fresh token (that
// would roll the proxy pods on every reconcile).
func TestGatewayInfraReconciler_NeverRotatesGeneratedAuthToken(t *testing.T) {
	t.Parallel()

	reconciler := newInfraReconciler(t, infraFixtures(t)...)
	reconcileEdge(t, reconciler)

	var first corev1.Secret
	require.NoError(t, reconciler.Get(context.Background(),
		types.NamespacedName{Name: "cf-proxy-edge-auth", Namespace: infraNamespace}, &first))

	reconcileEdge(t, reconciler)

	var second corev1.Secret
	require.NoError(t, reconciler.Get(context.Background(),
		types.NamespacedName{Name: "cf-proxy-edge-auth", Namespace: infraNamespace}, &second))

	assert.Equal(t, first.Data["auth-token"], second.Data["auth-token"],
		"the generated token must be stable across reconciles")
}

// TestGatewayInfraReconciler_PreservesForeignAnnotations pins the no-write-loop
// fix: a reconcile must not wipe annotations set by other actors (e.g.
// deployment.kubernetes.io/revision from kube-controller-manager), which would
// ping-pong writes forever.
func TestGatewayInfraReconciler_PreservesForeignAnnotations(t *testing.T) {
	t.Parallel()

	reconciler := newInfraReconciler(t, infraFixtures(t)...)
	reconcileEdge(t, reconciler)

	var deployment appsv1.Deployment
	key := types.NamespacedName{Name: "cf-proxy-edge", Namespace: infraNamespace}
	require.NoError(t, reconciler.Get(context.Background(), key, &deployment))

	// Simulate kube-controller-manager stamping its revision annotation.
	if deployment.Annotations == nil {
		deployment.Annotations = map[string]string{}
	}

	deployment.Annotations["deployment.kubernetes.io/revision"] = "7"
	require.NoError(t, reconciler.Update(context.Background(), &deployment))

	reconcileEdge(t, reconciler)

	require.NoError(t, reconciler.Get(context.Background(), key, &deployment))
	assert.Equal(t, "7", deployment.Annotations["deployment.kubernetes.io/revision"],
		"a foreign annotation must survive reconcile or the controller fights KCM forever")
}

// TestGatewayInfraReconciler_RefusesToAdoptForeignObject pins the adoption
// guard: an existing object NOT owned by this Gateway is never overwritten or
// adopted (which would later GC-delete a user's resource).
func TestGatewayInfraReconciler_RefusesToAdoptForeignObject(t *testing.T) {
	t.Parallel()

	foreign := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name: "cf-proxy-edge", Namespace: infraNamespace,
			Labels: map[string]string{"owned-by": "some-user"},
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "user-app"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "user-app"}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "user/app:v1"}}},
			},
		},
	}

	objects := append(infraFixtures(t), foreign)
	reconciler := newInfraReconciler(t, objects...)

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "edge", Namespace: infraNamespace},
	})
	require.Error(t, err, "the reconcile must refuse to adopt a foreign Deployment")

	var deployment appsv1.Deployment
	require.NoError(t, reconciler.Get(context.Background(),
		types.NamespacedName{Name: "cf-proxy-edge", Namespace: infraNamespace}, &deployment))

	assert.Equal(t, "user-app", deployment.Spec.Selector.MatchLabels["app"],
		"the user's Deployment must be left untouched")
	assert.Empty(t, deployment.OwnerReferences, "the foreign object must not be adopted")
}

// TestGatewayInfraReconciler_RefusesForeignAuthSecret pins that the
// generated-auth-Secret bootstrap never adopts a pre-existing Secret it does
// not own. Wiring the data plane's push auth to material the controller
// neither generated nor owns would break the same never-adopt invariant every
// apply path enforces, and silently use unverified token bytes.
func TestGatewayInfraReconciler_RefusesForeignAuthSecret(t *testing.T) {
	t.Parallel()

	foreign := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "cf-proxy-edge-auth", Namespace: infraNamespace,
			Labels: map[string]string{"owned-by": "some-user"},
		},
		Data: map[string][]byte{"auth-token": []byte("foreign-token")},
	}

	objects := append(infraFixtures(t), foreign)
	reconciler := newInfraReconciler(t, objects...)

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "edge", Namespace: infraNamespace},
	})
	require.Error(t, err, "the reconcile must refuse to adopt a foreign auth Secret")
	assert.ErrorIs(t, err, errRefusedAdoption)

	var secret corev1.Secret
	require.NoError(t, reconciler.Get(context.Background(),
		types.NamespacedName{Name: "cf-proxy-edge-auth", Namespace: infraNamespace}, &secret))

	assert.Equal(t, []byte("foreign-token"), secret.Data["auth-token"],
		"the foreign Secret's token must be left untouched")
	assert.Empty(t, secret.OwnerReferences, "the foreign Secret must not be adopted")
}

// TestGatewayInfraReconciler_TokenlessOwnedAuthSecretFailsClosedWithoutUpdate
// pins that an owned generated-auth Secret whose token key is empty fails
// CLOSED — the data plane is not rendered — WITHOUT the bootstrap writing to
// the Secret. The controller holds only secrets create (no update/delete) by
// deliberate least-privilege design, so the interceptor forbids any secrets
// Update: a bootstrap that tried to repair the Secret in place would hit
// `forbidden` on a real cluster and wedge the plane permanently. The tokenless
// state can only arise from external mutation; it surfaces downstream
// (readAuthToken -> ErrInvalidParameters -> RenderFailed) and heals when the
// Secret is deleted (the create path regenerates it).
func TestGatewayInfraReconciler_TokenlessOwnedAuthSecretFailsClosedWithoutUpdate(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, gatewayv1.Install(scheme))
	require.NoError(t, v1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, autoscalingv2.AddToScheme(scheme))

	owned := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "cf-proxy-edge-auth", Namespace: infraNamespace,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: gatewayv1.GroupVersion.String(),
				Kind:       "Gateway",
				Name:       "edge",
				UID:        "gw-uid-1",
				Controller: new(true),
			}},
		},
		Data: map[string][]byte{"auth-token": []byte("")},
	}

	builder := fake.NewClientBuilder().WithScheme(scheme)
	for _, obj := range append(infraFixtures(t), owned) {
		builder = builder.WithRuntimeObjects(obj)
	}

	var secretUpdates int

	fakeClient := builder.WithInterceptorFuncs(interceptor.Funcs{
		Update: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if _, ok := obj.(*corev1.Secret); ok {
				secretUpdates++

				return apierrors.NewForbidden(
					schema.GroupResource{Resource: "secrets"}, obj.GetName(), errSimulatedCacheMiss)
			}

			return cl.Update(ctx, obj, opts...)
		},
	}).Build()

	reconciler := &GatewayInfraReconciler{
		Client:         fakeClient,
		Scheme:         scheme,
		ControllerName: "cf.k8s.lex.la/tunnel-controller",
		ConfigResolver: config.NewResolver(fakeClient, "cf-system", cfmetrics.NewNoopCollector()),
		RenderDefaults: render.Defaults{ProxyImage: "ghcr.io/example/proxy:v1.2.3"},
	}

	// Fails closed without erroring the reconcile: ErrInvalidParameters renders
	// nothing and surfaces an Event rather than requeuing forever.
	reconcileEdge(t, reconciler)

	assert.Zero(t, secretUpdates, "the bootstrap must never write to an existing Secret (create-only RBAC)")

	var deployment appsv1.Deployment
	err := reconciler.Get(context.Background(),
		types.NamespacedName{Name: "cf-proxy-edge", Namespace: infraNamespace}, &deployment)
	assert.True(t, apierrors.IsNotFound(err), "a tokenless auth Secret must fail closed — no data plane rendered")

	var secret corev1.Secret
	require.NoError(t, reconciler.Get(context.Background(),
		types.NamespacedName{Name: "cf-proxy-edge-auth", Namespace: infraNamespace}, &secret))
	assert.Empty(t, secret.Data["auth-token"], "the tokenless Secret must be left untouched, not rewritten")
}

// TestGatewayInfraReconciler_SpecImageWorksWithoutDefault pins the override
// path: a GatewayConfig-level image makes rendering possible even when the
// controller-level default is absent.
func TestGatewayInfraReconciler_SpecImageWorksWithoutDefault(t *testing.T) {
	t.Parallel()

	objects := infraFixtures(t)
	for _, obj := range objects {
		if gwConfig, ok := obj.(*v1alpha1.GatewayConfig); ok {
			gwConfig.Spec.Image = "example.com/tenant-proxy:v1"
		}
	}

	reconciler := newInfraReconciler(t, objects...)
	reconciler.RenderDefaults.ProxyImage = ""

	reconcileEdge(t, reconciler)

	var deployment appsv1.Deployment
	require.NoError(t, reconciler.Get(context.Background(),
		types.NamespacedName{Name: "cf-proxy-edge", Namespace: infraNamespace}, &deployment))
	assert.Equal(t, "example.com/tenant-proxy:v1", deployment.Spec.Template.Spec.Containers[0].Image)
}
