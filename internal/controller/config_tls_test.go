package controller

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/configtls"
)

const sharedLeafName = "proxy-headless.cf-system.svc.cluster.local"

func configCAKey() types.NamespacedName {
	return types.NamespacedName{Name: "release-config-ca", Namespace: "cf-system"}
}

func sharedLeafKey() types.NamespacedName {
	return types.NamespacedName{Name: "release-proxy-config-tls", Namespace: "cf-system"}
}

func testAuthority(t *testing.T) *configtls.Authority {
	t.Helper()

	certPEM, keyPEM, err := configtls.NewAuthorityPEM(time.Now())
	require.NoError(t, err)

	authority, err := configtls.LoadAuthority(certPEM, keyPEM)
	require.NoError(t, err)

	return authority
}

func TestEnsureConfigCA_CreatesOnceAndReuses(t *testing.T) {
	t.Parallel()

	c := fake.NewClientBuilder().Build()

	first, err := ensureConfigCA(t.Context(), c, configCAKey())
	require.NoError(t, err)

	var stored corev1.Secret
	require.NoError(t, c.Get(t.Context(), configCAKey(), &stored))
	assert.Equal(t, corev1.SecretTypeTLS, stored.Type)

	second, err := ensureConfigCA(t.Context(), c, configCAKey())
	require.NoError(t, err)

	assert.Equal(t, first.CertificatePEM(), second.CertificatePEM(), "an existing CA must be reused, never replaced")
}

// TestEnsureConfigCA_LostCreateRaceReusesTheWinner pins convergence when two
// replicas start together: the loser adopts the CA that was stored.
func TestEnsureConfigCA_LostCreateRaceReusesTheWinner(t *testing.T) {
	t.Parallel()

	winnerCert, winnerKey, err := configtls.NewAuthorityPEM(time.Now())
	require.NoError(t, err)

	c := fake.NewClientBuilder().WithInterceptorFuncs(interceptor.Funcs{
		Create: func(ctx context.Context, inner client.WithWatch, obj client.Object, _ ...client.CreateOption) error {
			winner := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: obj.GetName(), Namespace: obj.GetNamespace()},
				Type:       corev1.SecretTypeTLS,
				Data:       map[string][]byte{corev1.TLSCertKey: winnerCert, corev1.TLSPrivateKeyKey: winnerKey},
			}
			require.NoError(t, inner.Create(ctx, winner))

			return apierrors.NewAlreadyExists(schema.GroupResource{Resource: "secrets"}, obj.GetName())
		},
	}).Build()

	authority, err := ensureConfigCA(t.Context(), c, configCAKey())
	require.NoError(t, err)
	assert.Equal(t, winnerCert, authority.CertificatePEM())
}

// TestEnsureConfigCA_BrokenSecretFailsLoud pins that a CA Secret that does not
// hold a usable CA stops the controller instead of being replaced: replacing
// it would silently invalidate every issued leaf.
func TestEnsureConfigCA_BrokenSecretFailsLoud(t *testing.T) {
	t.Parallel()

	c := fake.NewClientBuilder().WithObjects(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: configCAKey().Name, Namespace: configCAKey().Namespace},
		Data:       map[string][]byte{corev1.TLSCertKey: []byte("garbage")},
	}).Build()

	_, err := ensureConfigCA(t.Context(), c, configCAKey())
	require.Error(t, err)
}

func readLeaf(t *testing.T, c client.Client) *corev1.Secret {
	t.Helper()

	var secret corev1.Secret
	require.NoError(t, c.Get(t.Context(), sharedLeafKey(), &secret))

	return &secret
}

func TestEnsureSharedLeaf_CreatesAValidLeaf(t *testing.T) {
	t.Parallel()

	authority := testAuthority(t)
	c := fake.NewClientBuilder().Build()

	outcome, err := ensureSharedLeaf(t.Context(), c, authority, sharedLeafKey(), []string{sharedLeafName}, time.Now())
	require.NoError(t, err)
	assert.Equal(t, leafIssued, outcome)

	secret := readLeaf(t, c)
	assert.Equal(t, corev1.SecretTypeTLS, secret.Type)
	require.NoError(t, authority.Check(secret.Data[corev1.TLSCertKey], secret.Data[corev1.TLSPrivateKeyKey],
		[]string{sharedLeafName}, time.Now()))
}

func TestEnsureSharedLeaf_ValidLeafIsLeftAlone(t *testing.T) {
	t.Parallel()

	authority := testAuthority(t)
	c := fake.NewClientBuilder().Build()

	_, err := ensureSharedLeaf(t.Context(), c, authority, sharedLeafKey(), []string{sharedLeafName}, time.Now())
	require.NoError(t, err)

	before := readLeaf(t, c)

	outcome, err := ensureSharedLeaf(t.Context(), c, authority, sharedLeafKey(), []string{sharedLeafName}, time.Now())
	require.NoError(t, err)
	assert.Equal(t, leafValid, outcome)
	assert.Equal(t, before.ResourceVersion, readLeaf(t, c).ResourceVersion)
}

func TestEnsureSharedLeaf_RenewsInsideTheWindow(t *testing.T) {
	t.Parallel()

	authority := testAuthority(t)
	c := fake.NewClientBuilder().Build()
	issuedAt := time.Now()

	_, err := ensureSharedLeaf(t.Context(), c, authority, sharedLeafKey(), []string{sharedLeafName}, issuedAt)
	require.NoError(t, err)

	later := issuedAt.Add(configtls.LeafValidity - configtls.RenewBefore + time.Hour)

	outcome, err := ensureSharedLeaf(t.Context(), c, authority, sharedLeafKey(), []string{sharedLeafName}, later)
	require.NoError(t, err)
	assert.Equal(t, leafRenewed, outcome)

	secret := readLeaf(t, c)
	require.NoError(t, authority.Check(secret.Data[corev1.TLSCertKey], secret.Data[corev1.TLSPrivateKeyKey],
		[]string{sharedLeafName}, later))
}

func TestEnsureSharedLeaf_ReplacesAForeignLeaf(t *testing.T) {
	t.Parallel()

	authority := testAuthority(t)
	foreign := testAuthority(t)

	foreignCert, foreignKey, err := foreign.Issue([]string{sharedLeafName}, time.Now())
	require.NoError(t, err)

	c := fake.NewClientBuilder().WithObjects(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: sharedLeafKey().Name, Namespace: sharedLeafKey().Namespace},
		Type:       corev1.SecretTypeTLS,
		Data:       map[string][]byte{corev1.TLSCertKey: foreignCert, corev1.TLSPrivateKeyKey: foreignKey},
	}).Build()

	outcome, err := ensureSharedLeaf(t.Context(), c, authority, sharedLeafKey(), []string{sharedLeafName}, time.Now())
	require.NoError(t, err)
	assert.Equal(t, leafReplacedInvalid, outcome)

	secret := readLeaf(t, c)
	require.NoError(t, authority.Check(secret.Data[corev1.TLSCertKey], secret.Data[corev1.TLSPrivateKeyKey],
		[]string{sharedLeafName}, time.Now()))
}

// TestEnsureSharedLeaf_LostRenewalRaceAcceptsTheWinner pins the update half of
// convergence: when another issuer renews the leaf between our read and our
// write, the conflict is settled by reading the winner's leaf, not reported.
func TestEnsureSharedLeaf_LostRenewalRaceAcceptsTheWinner(t *testing.T) {
	t.Parallel()

	authority := testAuthority(t)
	issuedAt := time.Now()

	dueCert, dueKey, err := authority.Issue([]string{sharedLeafName}, issuedAt)
	require.NoError(t, err)

	later := issuedAt.Add(configtls.LeafValidity - configtls.RenewBefore + time.Hour)

	winnerCert, winnerKey, err := authority.Issue([]string{sharedLeafName}, later)
	require.NoError(t, err)

	c := fake.NewClientBuilder().WithObjects(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: sharedLeafKey().Name, Namespace: sharedLeafKey().Namespace},
		Type:       corev1.SecretTypeTLS,
		Data:       map[string][]byte{corev1.TLSCertKey: dueCert, corev1.TLSPrivateKeyKey: dueKey},
	}).WithInterceptorFuncs(interceptor.Funcs{
		Update: func(ctx context.Context, inner client.WithWatch, obj client.Object, _ ...client.UpdateOption) error {
			var current corev1.Secret
			require.NoError(t, inner.Get(ctx, client.ObjectKeyFromObject(obj), &current))
			current.Data = map[string][]byte{corev1.TLSCertKey: winnerCert, corev1.TLSPrivateKeyKey: winnerKey}
			require.NoError(t, inner.Update(ctx, &current))

			return apierrors.NewConflict(schema.GroupResource{Resource: "secrets"}, obj.GetName(), nil)
		},
	}).Build()

	outcome, err := ensureSharedLeaf(t.Context(), c, authority, sharedLeafKey(), []string{sharedLeafName}, later)
	require.NoError(t, err)
	assert.Equal(t, leafValid, outcome)
	assert.Equal(t, winnerCert, readLeaf(t, c).Data[corev1.TLSCertKey])
}

// TestSharedLeafIssuer_PassSchedulesAndWarns pins what one issuance pass
// decides: the routine hourly check after a success, the short retry after a
// failure, and a Warning Event on the CA Secret while the CA is near expiry.
func TestSharedLeafIssuer_PassSchedulesAndWarns(t *testing.T) {
	t.Parallel()

	authority := testAuthority(t)
	recorder := events.NewFakeRecorder(10)

	issuer := &sharedLeafIssuer{
		client:    fake.NewClientBuilder().Build(),
		authority: authority,
		caKey:     configCAKey(),
		key:       sharedLeafKey(),
		names:     []string{sharedLeafName},
		logger:    slog.New(slog.DiscardHandler),
		recorder:  recorder,
	}

	assert.Equal(t, sharedLeafCheckInterval, issuer.pass(t.Context(), time.Now()))
	assert.Empty(t, recorder.Events, "a CA far from expiry is not reported")

	nearExpiry := authority.NotAfter().Add(-configtls.LeafValidity / 2)
	issuer.pass(t.Context(), nearExpiry)

	select {
	case event := <-recorder.Events:
		assert.Contains(t, event, eventReasonConfigTLSCAExpiring)
	default:
		t.Fatal("a CA within one leaf lifetime of expiry must be reported")
	}

	issuer.client = fake.NewClientBuilder().WithInterceptorFuncs(interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return apierrors.NewServiceUnavailable("apiserver unavailable")
		},
	}).Build()

	assert.Equal(t, sharedLeafRetryInterval, issuer.pass(t.Context(), time.Now()))
}

// TestEnsureSharedLeaf_ConcurrentIssuersConverge pins that two issuers racing
// on the same Secret end with one valid leaf and that a further pass by
// either changes nothing, so the proxy is not handed a different pair by
// each of them in turn.
func TestEnsureSharedLeaf_ConcurrentIssuersConverge(t *testing.T) {
	t.Parallel()

	authority := testAuthority(t)
	c := fake.NewClientBuilder().Build()
	now := time.Now()

	var wg sync.WaitGroup

	errs := make([]error, 8)

	for i := range errs {
		wg.Go(func() {
			_, errs[i] = ensureSharedLeaf(t.Context(), c, authority, sharedLeafKey(), []string{sharedLeafName}, now)
		})
	}

	wg.Wait()

	for _, err := range errs {
		require.NoError(t, err)
	}

	settled := readLeaf(t, c)
	require.NoError(t, authority.Check(settled.Data[corev1.TLSCertKey], settled.Data[corev1.TLSPrivateKeyKey],
		[]string{sharedLeafName}, now))

	for range 3 {
		outcome, err := ensureSharedLeaf(t.Context(), c, authority, sharedLeafKey(), []string{sharedLeafName}, now)
		require.NoError(t, err)
		assert.Equal(t, leafValid, outcome)
	}

	assert.Equal(t, settled.ResourceVersion, readLeaf(t, c).ResourceVersion)
}
