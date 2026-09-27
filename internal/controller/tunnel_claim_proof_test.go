package controller

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/api/v1alpha1"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/cfmetrics"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/config"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/tunnel"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/tunnelownership"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/tunnelproof"
)

// The per-Gateway fixtures' connector tokens all name this tunnel.
const proofTokenTunnel = "550e8400-e29b-41d4-a716-446655440000"

// withVerdict makes a Resolver over cli answer every tunnel claim with proof.
func withVerdict(cli client.Client, namespace string, proof tunnelownership.Proof) *config.Resolver {
	return config.NewResolver(cli, namespace, cfmetrics.NewNoopCollector(),
		config.WithClaimVerifier(fixedClaimVerifier(proof)))
}

// reconcileProofGateway runs the Gateway reconciler once over the per-Gateway
// status fixtures, with every claim answered by proof.
func reconcileProofGateway(
	t *testing.T,
	proof tunnelownership.Proof,
	mutate func(client.Object),
) (gatewayv1.Gateway, []string) {
	t.Helper()

	objects := perGatewayStatusFixtures(t)
	for _, obj := range objects {
		mutate(obj)
	}

	fakeClient := setupGatewayFakeClient(objects...)
	recorder := events.NewFakeRecorder(10)

	reconciler := &GatewayReconciler{
		Client:         fakeClient,
		Scheme:         fakeClient.Scheme(),
		ControllerName: "test-controller",
		ConfigResolver: withVerdict(fakeClient, "default", proof),
		ProxyImage:     "ghcr.io/example/proxy:v1",
		Recorder:       recorder,
		ViewStore:      newMergeViewStore(),
	}

	ctx := context.Background()
	_, err := reconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "pg-gateway", Namespace: "default"},
	})
	require.NoError(t, err)

	var updated gatewayv1.Gateway
	require.NoError(t, fakeClient.Get(ctx, types.NamespacedName{Name: "pg-gateway", Namespace: "default"}, &updated))

	return updated, drainEvents(recorder)
}

func noMutation(client.Object) {}

// TestGatewayReconciler_RefutedClaimIsRefused pins the status half of claim
// verification: a Gateway whose token Cloudflare does not confirm is refused
// with a message the tenant can act on, a Warning Event for the operator, and
// no tunnel address — advertising it would turn the refusal into possession.
func TestGatewayReconciler_RefutedClaimIsRefused(t *testing.T) {
	t.Parallel()

	updated, recorded := reconcileProofGateway(t, tunnelownership.ProofRefuted, noMutation)

	accepted := findCondition(updated.Status.Conditions, string(gatewayv1.GatewayConditionAccepted))
	require.NotNil(t, accepted)
	assert.Equal(t, metav1.ConditionFalse, accepted.Status)
	assert.Equal(t, string(gatewayv1.GatewayReasonInvalidParameters), accepted.Reason)
	assert.True(t, strings.HasPrefix(accepted.Message, refusedConditionPrefix), "got %q", accepted.Message)
	assert.Contains(t, accepted.Message, proofTokenTunnel)
	assert.Contains(t, accepted.Message, "Cloudflare did not confirm")

	assert.Empty(t, updated.Status.Addresses, "a refused claim must not become possession")

	require.Len(t, recorded, 1)
	assert.Contains(t, recorded[0], eventReasonTunnelClaimRejected)
}

// TestGatewayReconciler_UnverifiableNewClaimIsRefused pins fail-closed for a
// first claim during a Cloudflare outage, and that its message says the check
// will be retried rather than blaming the token.
func TestGatewayReconciler_UnverifiableNewClaimIsRefused(t *testing.T) {
	t.Parallel()

	updated, _ := reconcileProofGateway(t, tunnelownership.ProofUnknown, noMutation)

	accepted := findCondition(updated.Status.Conditions, string(gatewayv1.GatewayConditionAccepted))
	require.NotNil(t, accepted)
	assert.Equal(t, metav1.ConditionFalse, accepted.Status)
	assert.Contains(t, accepted.Message, proofTokenTunnel)
	assert.Contains(t, accepted.Message, "retried")
	assert.NotContains(t, accepted.Message, "did not confirm",
		"nothing was refuted, so the message must not say so")
}

// TestGatewayReconciler_UnverifiableHolderKeepsItsTunnel pins the outage
// policy: a Gateway already advertising its tunnel keeps it while Cloudflare
// cannot be asked. The address is the only evidence here, so this is exactly
// as trustworthy as write access to gateways/status.
func TestGatewayReconciler_UnverifiableHolderKeepsItsTunnel(t *testing.T) {
	t.Parallel()

	updated, recorded := reconcileProofGateway(t, tunnelownership.ProofUnknown, func(obj client.Object) {
		if gateway, ok := obj.(*gatewayv1.Gateway); ok && gateway.Name == "pg-gateway" {
			gateway.Status.Addresses = []gatewayv1.GatewayStatusAddress{
				{Value: proofTokenTunnel + cfArgotunnelSuffix},
			}
		}
	})

	accepted := findCondition(updated.Status.Conditions, string(gatewayv1.GatewayConditionAccepted))
	require.NotNil(t, accepted)
	assert.Equal(t, metav1.ConditionTrue, accepted.Status, "an outage must not evict a holder: %q", accepted.Message)
	assert.Empty(t, recorded)
}

// TestGatewayReconciler_SharingOptInStillRequiresProof pins that
// allowSharedTunnels waives the contest between namespaces and nothing else.
func TestGatewayReconciler_SharingOptInStillRequiresProof(t *testing.T) {
	t.Parallel()

	updated, _ := reconcileProofGateway(t, tunnelownership.ProofRefuted, func(obj client.Object) {
		if classConfig, ok := obj.(*v1alpha1.GatewayClassConfig); ok {
			classConfig.Spec.AllowSharedTunnels = true
		}
	})

	accepted := findCondition(updated.Status.Conditions, string(gatewayv1.GatewayConditionAccepted))
	require.NotNil(t, accepted)
	assert.Equal(t, metav1.ConditionFalse, accepted.Status)
	assert.Contains(t, accepted.Message, "Cloudflare did not confirm")
}

// TestGatewayInfraReconciler_RefutedClaimRendersNothing pins the data-plane
// half: no Deployment, so no connector, even with sharing opted in.
func TestGatewayInfraReconciler_RefutedClaimRendersNothing(t *testing.T) {
	t.Parallel()

	objects := infraFixtures(t)
	for _, obj := range objects {
		if classConfig, ok := obj.(*v1alpha1.GatewayClassConfig); ok {
			classConfig.Spec.AllowSharedTunnels = true
		}
	}

	reconciler := newInfraReconciler(t, objects...)
	reconciler.ConfigResolver = withVerdict(reconciler.Client, "cf-system", tunnelownership.ProofRefuted)
	reconcileEdge(t, reconciler)

	var deployment appsv1.Deployment
	err := reconciler.Get(context.Background(),
		types.NamespacedName{Name: "cf-proxy-edge", Namespace: infraNamespace}, &deployment)

	require.Error(t, err, "an unproven claim must get no proxy Deployment")
	assert.True(t, apierrors.IsNotFound(err), "expected NotFound, got %v", err)
}

// TestSyncAllRoutes_RefutedClaimIsNeitherWrittenNorServed pins the Cloudflare
// half: the unproven tunnel's document is never written, the tenant's routes
// do not fall back to the shared tunnel, and the route says why.
func TestSyncAllRoutes_RefutedClaimIsNeitherWrittenNorServed(t *testing.T) {
	t.Parallel()

	const classTunnel = "99999999-9999-4999-8999-999999999999"

	api := newRecordingTunnelAPI(t)
	syncer := newPartitionSyncSyncer(t, api, classTunnel)
	syncer.ConfigResolver = withVerdict(syncer.Client, "default", tunnelownership.ProofRefuted)

	_, result, err := syncer.SyncAllRoutes(context.Background())
	require.NoError(t, err)
	require.NotNil(t, result)

	assert.Empty(t, api.hostnamesFor(tenantTunnelUUID),
		"no document may be written for a tunnel the claimant has not proven")
	assert.NotContains(t, api.hostnamesFor(classTunnel), "tenant.example.com",
		"an unproven claim must fail closed, not fall back to the shared tunnel")
	assert.Contains(t, api.hostnamesFor(classTunnel), "shared.example.com")

	binding := result.HTTPRouteBindings["default/tenant-route"]
	require.NotNil(t, binding.syncErrByGateway)

	routeErr := binding.syncErrByGateway["default/infra-gw"]
	require.Error(t, routeErr)
	assert.Contains(t, routeErr.Error(), "Cloudflare")
}

// TestUnprovenRejectionWording pins the tenant-visible wording of an unproven
// claim on both surfaces: it names the tunnel, names no other Gateway, and
// tells a refutation from a check that could not run.
func TestUnprovenRejectionWording(t *testing.T) {
	t.Parallel()

	const tunnelID = "22222222-2222-2222-2222-222222222222"

	for _, tc := range []struct {
		proof tunnelownership.Proof
		want  string
	}{
		{proof: tunnelownership.ProofRefuted, want: "did not confirm"},
		{proof: tunnelownership.ProofUnknown, want: "retried"},
	} {
		rejection := tunnelownership.Rejection{TunnelID: tunnelID, Unproven: true, Proof: tc.proof}

		infra := &infraGateways{
			resolved:  map[string]*infraGateway{},
			broken:    map[string]bool{"team-b/gw": true},
			transient: map[string]bool{},
			rejected:  map[string]tunnelownership.Rejection{"team-b/gw": rejection},
		}

		routeErr := gatewaySyncError("team-b/gw", map[string]error{}, infra)
		require.Error(t, routeErr)

		for _, surface := range []string{routeErr.Error(), tunnelRejectionMessage(rejection)} {
			assert.Contains(t, surface, tunnelID)
			assert.Contains(t, surface, tc.want)
			assert.NotContains(t, surface, "already in use", "an unproven claim lost to no neighbour")
		}
	}
}

// TestUnprovenRejectionMessageFitsTheCondition extends the truncation budget
// to the unproven wordings; see TestTunnelRejectionMessageSurvivesConditionTruncation.
func TestUnprovenRejectionMessageFitsTheCondition(t *testing.T) {
	t.Parallel()

	for _, proof := range []tunnelownership.Proof{tunnelownership.ProofRefuted, tunnelownership.ProofUnknown} {
		rejection := tunnelownership.Rejection{
			TunnelID: "22222222-2222-2222-2222-222222222222", Unproven: true, Proof: proof,
		}

		stored := refusedConditionPrefix + tunnelRejectionMessage(rejection)
		assert.LessOrEqual(t, len(stored), maxConditionMessageLength)
	}
}

// TestGatewayReconciler_AcceptedDedicatedGatewayComesBackToRecheck pins that a
// dedicated Gateway is reconciled again on its own. A confirmation expires with
// no event in the cluster, so without a requeue a rotated tunnel secret would
// go unnoticed by this layer until something unrelated touched the Gateway,
// while the route syncer already refused it.
func TestGatewayReconciler_AcceptedDedicatedGatewayComesBackToRecheck(t *testing.T) {
	t.Parallel()

	fakeClient := setupGatewayFakeClient(perGatewayStatusFixtures(t)...)

	reconciler := &GatewayReconciler{
		Client:         fakeClient,
		Scheme:         fakeClient.Scheme(),
		ControllerName: "test-controller",
		ConfigResolver: withVerdict(fakeClient, "default", tunnelownership.ProofVerified),
		ProxyImage:     "ghcr.io/example/proxy:v1",
		ViewStore:      newMergeViewStore(),
	}

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "pg-gateway", Namespace: "default"},
	})
	require.NoError(t, err)

	assert.Positive(t, result.RequeueAfter, "an accepted dedicated Gateway must come back to re-check its claim")
	assert.LessOrEqual(t, result.RequeueAfter, tunnelproof.RecheckInterval)
}

// switchableClaimVerifier answers every claim with whatever verdict it was
// last set to, so a test can end an outage.
type switchableClaimVerifier struct {
	proof atomic.Int32
}

func (v *switchableClaimVerifier) Verify(context.Context, string, *tunnel.Token) tunnelownership.Proof {
	return tunnelownership.Proof(v.proof.Load())
}

// TestSyncAllRoutes_UncheckableClaimRecovers pins the route half of the outage
// policy: a claim Cloudflare could not check keeps its routes out, and the
// first sync after the check succeeds programs them. That sync comes from the
// Gateway's Accepted flip, which the route controllers watch.
func TestSyncAllRoutes_UncheckableClaimRecovers(t *testing.T) {
	t.Parallel()

	const classTunnel = "99999999-9999-4999-8999-999999999999"

	verifier := &switchableClaimVerifier{}
	verifier.proof.Store(int32(tunnelownership.ProofUnknown))

	api := newRecordingTunnelAPI(t)
	syncer := newPartitionSyncSyncer(t, api, classTunnel)
	syncer.ConfigResolver = config.NewResolver(syncer.Client, "default", cfmetrics.NewNoopCollector(),
		config.WithClaimVerifier(verifier))

	result, _, err := syncer.SyncAllRoutes(context.Background())
	require.NoError(t, err)
	assert.Zero(t, result.RequeueAfter,
		"a tenant can keep its claim unchecked indefinitely, so it must not buy a periodic full sync")
	assert.Empty(t, api.hostnamesFor(tenantTunnelUUID))

	verifier.proof.Store(int32(tunnelownership.ProofVerified))

	_, _, err = syncer.SyncAllRoutes(context.Background())
	require.NoError(t, err)
	assert.Contains(t, api.hostnamesFor(tenantTunnelUUID), "tenant.example.com",
		"once Cloudflare answers, the routes must be programmed")
}

// TestUncheckableClaimMessageNamesBothCauses pins that the tenant is not sent
// hunting an outage when the credential is what Cloudflare rejects.
func TestUncheckableClaimMessageNamesBothCauses(t *testing.T) {
	t.Parallel()

	message := tunnelRejectionMessage(tunnelownership.Rejection{
		TunnelID: "22222222-2222-2222-2222-222222222222", Unproven: true, Proof: tunnelownership.ProofUnknown,
	})

	assert.Contains(t, message, "unreachable")
	assert.Contains(t, message, "credential")
}

// TestCollectTunnelClaims_BrokenOwnCredentialDoesNotKeepATunnel pins the squat
// a tenant could otherwise stage after being refuted: point its GatewayConfig
// at a credential that cannot be read, so its claim comes back unchecked, and
// lean on the address it still advertises. An unchecked claim must not outrank
// a confirmed one, however old it is and whatever it advertises.
func TestCollectTunnelClaims_BrokenOwnCredentialDoesNotKeepATunnel(t *testing.T) {
	t.Parallel()

	squatter := claimsGateway("team-a", "gw", 0, "a-token")
	squatter.Status.Addresses = []gatewayv1.GatewayStatusAddress{{Value: claimsTunnel + cfArgotunnelSuffix}}

	owner := claimsGateway("team-b", "gw", 5, "b-token")
	owner.Status.Addresses = []gatewayv1.GatewayStatusAddress{{Value: claimsTunnel + cfArgotunnelSuffix}}

	squatterConfig := claimsGatewayConfig("team-a", "a-token")
	squatterConfig.Spec.CloudflareCredentialsSecretRef = &v1alpha1.LocalSecretReference{Name: "missing"}

	fakeClient := setupGatewayFakeClient(
		squatter,
		owner,
		claimsGatewayClass(),
		claimsClassConfig(),
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "creds", Namespace: "default"},
			Data:       map[string][]byte{"api-token": []byte("test-token")},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "a-token", Namespace: "team-a"},
			Data:       map[string][]byte{"tunnel-token": []byte(infraTunnelTokenFor(t, claimsTunnel))},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "b-token", Namespace: "team-b"},
			Data:       map[string][]byte{"tunnel-token": []byte(infraTunnelTokenFor(t, claimsTunnel))},
		},
		squatterConfig,
		claimsGatewayConfig("team-b", "b-token"),
	)

	resolver := withVerdict(fakeClient, "default", tunnelownership.ProofVerified)

	claims, err := collectTunnelClaims(context.Background(), fakeClient, resolver, "test-controller", claimsClassTunnel)
	require.NoError(t, err)

	rejected := tunnelownership.Arbitrate(claimsClassTunnel, false, claims)
	assert.Contains(t, rejected, "team-a/gw", "an unchecked claim must not hold a tunnel against a confirmed one")
	assert.NotContains(t, rejected, "team-b/gw", "the confirmed owner must keep its tunnel")
}
