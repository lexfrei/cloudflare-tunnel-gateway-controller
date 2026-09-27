package controller

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

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

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/configtls"
)

const edgeServerName = "cf-proxy-edge-config.tenant-a.svc.cluster.local"

func newTLSInfraReconciler(t *testing.T) (*GatewayInfraReconciler, *configtls.Authority) {
	t.Helper()

	reconciler := newInfraReconciler(t, infraFixtures(t)...)
	authority := testAuthority(t)
	reconciler.ConfigAuthority = authority
	reconciler.ClusterDomain = "cluster.local"

	return reconciler, authority
}

func edgeLeafKey(index string) types.NamespacedName {
	return types.NamespacedName{Name: "cf-proxy-edge-config-tls-" + index, Namespace: infraNamespace}
}

func mountedLeaf(t *testing.T, c client.Client) string {
	t.Helper()

	var deployment appsv1.Deployment
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Name: "cf-proxy-edge", Namespace: infraNamespace}, &deployment))

	for _, volume := range deployment.Spec.Template.Spec.Volumes {
		if volume.Secret != nil {
			return volume.Secret.SecretName
		}
	}

	return ""
}

func reconcileEdgeResult(t *testing.T, reconciler *GatewayInfraReconciler) ctrl.Result {
	t.Helper()

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "edge", Namespace: infraNamespace},
	})
	require.NoError(t, err)

	return result
}

func requireLeafValid(t *testing.T, c client.Client, authority *configtls.Authority, key types.NamespacedName, now time.Time) {
	t.Helper()

	var secret corev1.Secret
	require.NoError(t, c.Get(context.Background(), key, &secret))
	require.NoError(t, authority.Check(secret.Data[corev1.TLSCertKey], secret.Data[corev1.TLSPrivateKeyKey],
		[]string{edgeServerName}, now))
}

// TestGatewayInfraReconciler_ConfigTLSIssuesAndMountsALeaf pins issuance for
// a per-Gateway plane: an owned leaf for the plane's own config Service name,
// mounted by the rendered Deployment, and a requeue that brings the renewal
// check back.
func TestGatewayInfraReconciler_ConfigTLSIssuesAndMountsALeaf(t *testing.T) {
	t.Parallel()

	reconciler, authority := newTLSInfraReconciler(t)
	result := reconcileEdgeResult(t, reconciler)

	assert.Positive(t, result.RequeueAfter, "a TLS plane must come back for its renewal check")

	requireLeafValid(t, reconciler.Client, authority, edgeLeafKey("0"), time.Now())
	assert.Equal(t, "cf-proxy-edge-config-tls-0", mountedLeaf(t, reconciler.Client))

	var secret corev1.Secret
	require.NoError(t, reconciler.Get(context.Background(), edgeLeafKey("0"), &secret))
	require.Len(t, secret.OwnerReferences, 1)
	assert.Equal(t, "edge", secret.OwnerReferences[0].Name)
}

// TestGatewayInfraReconciler_ConfigTLSIsStableAcrossReconciles pins that an
// unchanged plane is not re-issued or rolled.
func TestGatewayInfraReconciler_ConfigTLSIsStableAcrossReconciles(t *testing.T) {
	t.Parallel()

	reconciler, _ := newTLSInfraReconciler(t)
	reconcileEdgeResult(t, reconciler)

	var before corev1.Secret
	require.NoError(t, reconciler.Get(context.Background(), edgeLeafKey("0"), &before))

	reconcileEdgeResult(t, reconciler)
	reconcileEdgeResult(t, reconciler)

	var after corev1.Secret
	require.NoError(t, reconciler.Get(context.Background(), edgeLeafKey("0"), &after))
	assert.Equal(t, before.ResourceVersion, after.ResourceVersion)
	assert.Equal(t, "cf-proxy-edge-config-tls-0", mountedLeaf(t, reconciler.Client))
}

// TestGatewayInfraReconciler_ConfigTLSReplacesATamperedLeaf pins the tenant
// case: a leaf the tenant rewrote with a certificate of its own is never
// trusted; a fresh leaf is issued in the next slot, mounted, and the
// replacement is reported on the Gateway.
func TestGatewayInfraReconciler_ConfigTLSReplacesATamperedLeaf(t *testing.T) {
	t.Parallel()

	reconciler, authority := newTLSInfraReconciler(t)
	recorder := events.NewFakeRecorder(10)
	reconciler.Recorder = recorder

	reconcileEdgeResult(t, reconciler)

	forgedCert, forgedKey, err := testAuthority(t).Issue([]string{edgeServerName}, time.Now())
	require.NoError(t, err)

	var secret corev1.Secret
	require.NoError(t, reconciler.Get(context.Background(), edgeLeafKey("0"), &secret))
	secret.Data = map[string][]byte{corev1.TLSCertKey: forgedCert, corev1.TLSPrivateKeyKey: forgedKey}
	require.NoError(t, reconciler.Update(context.Background(), &secret))

	reconcileEdgeResult(t, reconciler)

	assert.Equal(t, "cf-proxy-edge-config-tls-1", mountedLeaf(t, reconciler.Client))
	requireLeafValid(t, reconciler.Client, authority, edgeLeafKey("1"), time.Now())
	assert.True(t, drainedEventContains(recorder, eventReasonConfigTLSReplaced),
		"replacing a leaf that failed verification must be reported on the Gateway")
}

// TestGatewayInfraReconciler_ConfigTLSNeverAdoptsAForeignSecret pins that a
// Secret the tenant created at a slot name is skipped, not adopted.
func TestGatewayInfraReconciler_ConfigTLSNeverAdoptsAForeignSecret(t *testing.T) {
	t.Parallel()

	reconciler, authority := newTLSInfraReconciler(t)

	// Even a genuine-looking leaf: the tenant cannot hold one from our CA
	// without us, but ownership is checked before anything else anyway.
	certPEM, keyPEM, err := authority.Issue([]string{edgeServerName}, time.Now())
	require.NoError(t, err)

	require.NoError(t, reconciler.Create(context.Background(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: edgeLeafKey("0").Name, Namespace: infraNamespace},
		Type:       corev1.SecretTypeTLS,
		Data:       map[string][]byte{corev1.TLSCertKey: certPEM, corev1.TLSPrivateKeyKey: keyPEM},
	}))

	reconcileEdgeResult(t, reconciler)

	assert.Equal(t, "cf-proxy-edge-config-tls-1", mountedLeaf(t, reconciler.Client))
}

// TestGatewayInfraReconciler_ConfigTLSNeverStepsBackToALowerSlot pins that a
// slot freed below the mounted one is not taken back: doing so would roll a
// healthy plane for nothing, and a tenant could keep it rolling by creating
// and deleting Secrets.
func TestGatewayInfraReconciler_ConfigTLSNeverStepsBackToALowerSlot(t *testing.T) {
	t.Parallel()

	reconciler, _ := newTLSInfraReconciler(t)

	squatter := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: edgeLeafKey("0").Name, Namespace: infraNamespace}}
	require.NoError(t, reconciler.Create(context.Background(), squatter))

	reconcileEdgeResult(t, reconciler)
	require.Equal(t, "cf-proxy-edge-config-tls-1", mountedLeaf(t, reconciler.Client))

	require.NoError(t, reconciler.Delete(context.Background(), squatter))

	reconcileEdgeResult(t, reconciler)
	assert.Equal(t, "cf-proxy-edge-config-tls-1", mountedLeaf(t, reconciler.Client))
}

// TestGatewayInfraReconciler_ConfigTLSRenewsInsideTheWindow pins rotation: a
// leaf inside the renewal window is replaced by a new slot, which rolls the
// plane onto it.
func TestGatewayInfraReconciler_ConfigTLSRenewsInsideTheWindow(t *testing.T) {
	t.Parallel()

	reconciler, authority := newTLSInfraReconciler(t)
	reconcileEdgeResult(t, reconciler)

	later := time.Now().Add(configtls.LeafValidity - configtls.RenewBefore + time.Hour)
	reconciler.now = func() time.Time { return later }

	reconcileEdgeResult(t, reconciler)

	assert.Equal(t, "cf-proxy-edge-config-tls-1", mountedLeaf(t, reconciler.Client))
	requireLeafValid(t, reconciler.Client, authority, edgeLeafKey("1"), later)
}

// TestGatewayInfraReconciler_ConfigTLSExpiredCAStopsTheWalk pins that an
// expired CA fails the reconcile instead of opening slot after slot, each of
// which would fail verification the moment it was written.
func TestGatewayInfraReconciler_ConfigTLSExpiredCAStopsTheWalk(t *testing.T) {
	t.Parallel()

	reconciler, authority := newTLSInfraReconciler(t)
	reconcileEdgeResult(t, reconciler)

	recorder := events.NewFakeRecorder(10)
	reconciler.Recorder = recorder

	expired := authority.NotAfter().Add(time.Hour)
	reconciler.now = func() time.Time { return expired }

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "edge", Namespace: infraNamespace},
	})
	require.ErrorIs(t, err, configtls.ErrCAExpired)
	assert.False(t, drainedEventContains(recorder, eventReasonConfigTLSReplaced),
		"an expired CA is not the mounted certificate's fault and must not be reported as its replacement")

	assert.Equal(t, "cf-proxy-edge-config-tls-0", mountedLeaf(t, reconciler.Client))

	var next corev1.Secret
	assert.True(t, apierrors.IsNotFound(reconciler.Get(context.Background(), edgeLeafKey("1"), &next)))
}

// TestGatewayInfraReconciler_ConfigTLSNoLeafWithoutAProxyImage pins that a
// plane the controller will not render gets no certificate either.
func TestGatewayInfraReconciler_ConfigTLSNoLeafWithoutAProxyImage(t *testing.T) {
	t.Parallel()

	reconciler, _ := newTLSInfraReconciler(t)
	reconciler.RenderDefaults.ProxyImage = ""

	reconcileEdgeResult(t, reconciler)

	var slot corev1.Secret
	assert.True(t, apierrors.IsNotFound(reconciler.Get(context.Background(), edgeLeafKey("0"), &slot)))
}

// TestGatewayInfraReconciler_ConfigTLSRecreatesADeletedLeafInPlace pins that
// a deleted leaf comes back at the mounted slot, so the plane is not rolled.
func TestGatewayInfraReconciler_ConfigTLSRecreatesADeletedLeafInPlace(t *testing.T) {
	t.Parallel()

	reconciler, authority := newTLSInfraReconciler(t)
	reconcileEdgeResult(t, reconciler)

	require.NoError(t, reconciler.Delete(context.Background(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: edgeLeafKey("0").Name, Namespace: infraNamespace},
	}))

	reconcileEdgeResult(t, reconciler)

	assert.Equal(t, "cf-proxy-edge-config-tls-0", mountedLeaf(t, reconciler.Client))
	requireLeafValid(t, reconciler.Client, authority, edgeLeafKey("0"), time.Now())
}

// TestGatewayInfraReconciler_ConfigTLSConcurrentIssuersConverge pins that two
// issuers racing on one plane settle on a single valid leaf, and that further
// passes by either leave the mounted slot alone, so the plane is not rolled
// back and forth.
func TestGatewayInfraReconciler_ConfigTLSConcurrentIssuersConverge(t *testing.T) {
	t.Parallel()

	first, authority := newTLSInfraReconciler(t)
	second := *first

	var wg sync.WaitGroup

	for _, reconciler := range []*GatewayInfraReconciler{first, &second} {
		wg.Go(func() {
			_, _ = reconciler.ensureConfigTLSSecret(context.Background(), edgeGateway(t, first.Client))
		})
	}

	wg.Wait()

	var slots corev1.SecretList
	require.NoError(t, first.List(context.Background(), &slots, client.InNamespace(infraNamespace)))

	var leafSlots []string

	for _, secret := range slots.Items {
		if strings.HasPrefix(secret.Name, "cf-proxy-edge-config-tls-") {
			leafSlots = append(leafSlots, secret.Name)
		}
	}

	assert.Equal(t, []string{"cf-proxy-edge-config-tls-0"}, leafSlots, "racing issuers must not open a second slot")
	requireLeafValid(t, first.Client, authority, edgeLeafKey("0"), time.Now())

	for range 3 {
		for _, reconciler := range []*GatewayInfraReconciler{first, &second} {
			reconcileEdgeResult(t, reconciler)
			assert.Equal(t, "cf-proxy-edge-config-tls-0", mountedLeaf(t, reconciler.Client))
		}
	}
}

// TestGatewayInfraReconciler_NoConfigTLSKeepsThePlaintextWire pins the
// controller half of the opt-out: without a CA nothing is issued and the
// plane mounts nothing.
func TestGatewayInfraReconciler_NoConfigTLSKeepsThePlaintextWire(t *testing.T) {
	t.Parallel()

	reconciler := newInfraReconciler(t, infraFixtures(t)...)
	result := reconcileEdgeResult(t, reconciler)

	assert.Zero(t, result.RequeueAfter)
	assert.Empty(t, mountedLeaf(t, reconciler.Client))

	var slots corev1.SecretList
	require.NoError(t, reconciler.List(context.Background(), &slots, client.InNamespace(infraNamespace)))

	for _, secret := range slots.Items {
		assert.NotContains(t, secret.Name, "config-tls")
	}
}

func edgeGateway(t *testing.T, c client.Client) *gatewayv1.Gateway {
	t.Helper()

	var gateway gatewayv1.Gateway
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: "edge", Namespace: infraNamespace}, &gateway))

	return &gateway
}

func drainedEventContains(recorder *events.FakeRecorder, reason string) bool {
	for {
		select {
		case event := <-recorder.Events:
			if strings.Contains(event, reason) {
				return true
			}
		default:
			return false
		}
	}
}
