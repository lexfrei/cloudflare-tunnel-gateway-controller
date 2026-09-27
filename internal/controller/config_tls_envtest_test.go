//go:build envtest

package controller

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/api/v1alpha1"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/configtls"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/render"
)

// TestResolveConfigTLS_AgainstRealManager pins the startup path Run takes: the
// CA lands in the API server through a never-started manager's direct
// client, and a restart reuses it rather than minting another.
func TestResolveConfigTLS_AgainstRealManager(t *testing.T) {
	ctx := context.Background()
	namespace := driftNamespace(ctx, t)
	cfg := &Config{
		ProxyConfigCASecretRef:  namespace + "/release-config-ca",
		ProxyConfigTLSSecretRef: namespace + "/release-proxy-config-tls",
	}
	endpoints := []string{"https://release-proxy-headless." + namespace + ".svc.cluster.local:8081/config"}

	first, err := resolveConfigTLS(ctx, newUnstartedTestManager(t), cfg, endpoints)
	require.NoError(t, err)
	require.NotNil(t, first)

	second, err := resolveConfigTLS(ctx, newUnstartedTestManager(t), cfg, endpoints)
	require.NoError(t, err)
	assert.Equal(t, first.authority.CertificatePEM(), second.authority.CertificatePEM(),
		"a restart must reuse the stored CA, or every issued leaf stops verifying")
}

// TestResolveConfigTLS_OffCreatesNothingAgainstRealManager pins the controller
// half of the opt-out against a real API server.
func TestResolveConfigTLS_OffCreatesNothingAgainstRealManager(t *testing.T) {
	ctx := context.Background()
	namespace := driftNamespace(ctx, t)

	setup, err := resolveConfigTLS(ctx, newUnstartedTestManager(t), &Config{},
		[]string{"http://release-proxy-headless." + namespace + ".svc.cluster.local:8081/config"})
	require.NoError(t, err)
	assert.Nil(t, setup)

	var secrets corev1.SecretList
	require.NoError(t, envK8sClient.List(ctx, &secrets, client.InNamespace(namespace)))
	assert.Empty(t, secrets.Items)
}

// TestEnsureSharedLeaf_ConvergesAgainstAPIServer pins convergence with the API
// server's real optimistic concurrency: racing issuers leave one valid leaf,
// later passes do not write, and a due leaf is renewed in place.
func TestEnsureSharedLeaf_ConvergesAgainstAPIServer(t *testing.T) {
	ctx := context.Background()
	namespace := driftNamespace(ctx, t)
	authority := testAuthority(t)
	key := types.NamespacedName{Name: "release-proxy-config-tls", Namespace: namespace}
	names := []string{"release-proxy-headless." + namespace + ".svc.cluster.local"}
	now := time.Now()

	var wg sync.WaitGroup

	errs := make([]error, 6)

	for i := range errs {
		wg.Go(func() {
			_, errs[i] = ensureSharedLeaf(ctx, envK8sClient, authority, key, names, now)
		})
	}

	wg.Wait()

	for _, err := range errs {
		require.NoError(t, err)
	}

	var settled corev1.Secret
	require.NoError(t, envK8sClient.Get(ctx, key, &settled))
	require.NoError(t, authority.Check(settled.Data[corev1.TLSCertKey], settled.Data[corev1.TLSPrivateKeyKey], names, now))

	outcome, err := ensureSharedLeaf(ctx, envK8sClient, authority, key, names, now)
	require.NoError(t, err)
	assert.Equal(t, leafValid, outcome)

	var unchanged corev1.Secret
	require.NoError(t, envK8sClient.Get(ctx, key, &unchanged))
	assert.Equal(t, settled.ResourceVersion, unchanged.ResourceVersion)

	due := now.Add(configtls.LeafValidity - configtls.RenewBefore + time.Hour)

	outcome, err = ensureSharedLeaf(ctx, envK8sClient, authority, key, names, due)
	require.NoError(t, err)
	assert.Equal(t, leafRenewed, outcome)

	var renewed corev1.Secret
	require.NoError(t, envK8sClient.Get(ctx, key, &renewed))
	require.NoError(t, authority.Check(renewed.Data[corev1.TLSCertKey], renewed.Data[corev1.TLSPrivateKeyKey], names, due))
}

// TestEnsureConfigTLSSecret_ConvergesAgainstAPIServer pins per-Gateway
// issuance against a real API server: racing issuers settle on slot 0, and a
// leaf the tenant overwrote is abandoned for the next slot.
func TestEnsureConfigTLSSecret_ConvergesAgainstAPIServer(t *testing.T) {
	ctx := context.Background()
	namespace := driftNamespace(ctx, t)
	gateway := driftGateway(namespace)
	authority := testAuthority(t)

	newIssuer := func() *GatewayInfraReconciler {
		reconciler := driftReconciler()
		reconciler.ConfigAuthority = authority
		reconciler.ClusterDomain = "cluster.local"

		return reconciler
	}

	var wg sync.WaitGroup

	names := make([]string, 4)
	errs := make([]error, len(names))

	for i := range names {
		wg.Go(func() {
			names[i], errs[i] = newIssuer().ensureConfigTLSSecret(ctx, gateway)
		})
	}

	wg.Wait()

	for i := range names {
		require.NoError(t, errs[i])
		assert.Equal(t, "cf-proxy-edge-config-tls-0", names[i])
	}

	assert.Equal(t, []string{"cf-proxy-edge-config-tls-0"}, configTLSSlots(ctx, t, namespace))

	forgedCert, forgedKey, err := testAuthority(t).Issue(
		[]string{render.ConfigServerName(gateway, "cluster.local")}, time.Now())
	require.NoError(t, err)

	var slot corev1.Secret
	require.NoError(t, envK8sClient.Get(ctx, types.NamespacedName{Name: names[0], Namespace: namespace}, &slot))
	slot.Data = map[string][]byte{corev1.TLSCertKey: forgedCert, corev1.TLSPrivateKeyKey: forgedKey}
	require.NoError(t, envK8sClient.Update(ctx, &slot))

	replacement, err := newIssuer().ensureConfigTLSSecret(ctx, gateway)
	require.NoError(t, err)
	assert.Equal(t, "cf-proxy-edge-config-tls-1", replacement)
}

func configTLSSlots(ctx context.Context, t *testing.T, namespace string) []string {
	t.Helper()

	var secrets corev1.SecretList
	require.NoError(t, envK8sClient.List(ctx, &secrets, client.InNamespace(namespace)))

	var slots []string

	for _, secret := range secrets.Items {
		if strings.Contains(secret.Name, "-config-tls-") {
			slots = append(slots, secret.Name)
		}
	}

	return slots
}

// TestConfigTLSDeploymentConvergesAgainstAPIServer pins that the TLS wiring
// (Secret volume, mount, HTTPS probes) survives apiserver defaulting, so a
// TLS plane is not re-applied on every reconcile, and that moving to another
// slot does change the pod template.
func TestConfigTLSDeploymentConvergesAgainstAPIServer(t *testing.T) {
	ctx := context.Background()
	reconciler := driftReconciler()
	namespace := driftNamespace(ctx, t)
	gateway := driftGateway(namespace)
	input := &render.Input{
		Gateway:             gateway,
		TunnelToken:         "token",
		Defaults:            reconciler.RenderDefaults,
		ConfigTLSSecretName: render.ConfigTLSSecretName(gateway, 0),
		Config: &v1alpha1.GatewayConfig{
			ObjectMeta: metav1.ObjectMeta{Name: "edge-config", Namespace: namespace},
			Spec:       v1alpha1.GatewayConfigSpec{TunnelTokenSecretRef: v1alpha1.LocalSecretReference{Name: "edge-token"}},
		},
	}

	op, err := reconciler.applyDeployment(ctx, gateway, input)
	require.NoError(t, err)
	require.Equal(t, controllerutil.OperationResultCreated, op)

	for i := range 2 {
		op, err := reconciler.applyDeployment(ctx, gateway, input)
		require.NoError(t, err)
		assert.Equalf(t, controllerutil.OperationResultNone, op, "re-apply %d must be a no-op", i+1)
	}

	input.ConfigTLSSecretName = render.ConfigTLSSecretName(gateway, 1)

	op, err = reconciler.applyDeployment(ctx, gateway, input)
	require.NoError(t, err)
	assert.Equal(t, controllerutil.OperationResultUpdated, op, "a new slot must roll the plane")
}

// TestEnsureConfigCA_BrokenSecretIsNeverReplacedAgainstAPIServer pins that a
// CA Secret holding garbage fails startup and survives untouched.
func TestEnsureConfigCA_BrokenSecretIsNeverReplacedAgainstAPIServer(t *testing.T) {
	ctx := context.Background()
	namespace := driftNamespace(ctx, t)
	key := types.NamespacedName{Name: "release-config-ca", Namespace: namespace}

	require.NoError(t, envK8sClient.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: namespace},
		Data:       map[string][]byte{corev1.TLSCertKey: []byte("garbage")},
	}))

	_, err := ensureConfigCA(ctx, envK8sClient, key)
	require.Error(t, err)

	var stored corev1.Secret
	require.NoError(t, envK8sClient.Get(ctx, key, &stored))
	assert.Equal(t, []byte("garbage"), stored.Data[corev1.TLSCertKey])
}
