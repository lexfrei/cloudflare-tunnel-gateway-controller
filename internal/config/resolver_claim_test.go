package config_test

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/api/v1alpha1"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/cfmetrics"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/config"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/tunnel"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/tunnelownership"
)

// recordingVerifier returns a fixed verdict and records the credential each
// claim was checked with.
type recordingVerifier struct {
	proof tunnelownership.Proof

	mu        sync.Mutex
	apiTokens []string
	tunnels   []string
}

func (v *recordingVerifier) Verify(_ context.Context, apiToken string, token *tunnel.Token) tunnelownership.Proof {
	v.mu.Lock()
	defer v.mu.Unlock()

	v.apiTokens = append(v.apiTokens, apiToken)
	v.tunnels = append(v.tunnels, token.TunnelID.String())

	return v.proof
}

func newClaimResolver(t *testing.T, verifier config.ClaimVerifier, objects ...runtime.Object) *config.Resolver {
	t.Helper()

	builder := fake.NewClientBuilder().WithScheme(perGatewayScheme(t))
	for _, obj := range objects {
		builder = builder.WithRuntimeObjects(obj)
	}

	return config.NewResolver(builder.Build(), "cf-system", cfmetrics.NewNoopCollector(),
		config.WithClaimVerifier(verifier))
}

func claimGatewayConfig(override *v1alpha1.LocalSecretReference) *v1alpha1.GatewayConfig {
	return &v1alpha1.GatewayConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "edge-config", Namespace: testGwNamespace},
		Spec: v1alpha1.GatewayConfigSpec{
			TunnelTokenSecretRef:           v1alpha1.LocalSecretReference{Name: "edge-tunnel-token"},
			CloudflareCredentialsSecretRef: override,
		},
	}
}

func TestResolveTunnelClaimForGateway_SharedModeReturnsNil(t *testing.T) {
	t.Parallel()

	verifier := &recordingVerifier{proof: tunnelownership.ProofVerified}
	resolver := newClaimResolver(t, verifier)

	gateway := gatewayWithInfra("cf.k8s.lex.la", "GatewayConfig", "edge-config")
	gateway.Spec.Infrastructure = nil

	claim, err := resolver.ResolveTunnelClaimForGateway(context.Background(), gateway)
	require.NoError(t, err)
	assert.Nil(t, claim)
	assert.Empty(t, verifier.apiTokens)
}

// TestResolveTunnelClaimForGateway_VerifiesWithTheClassCredential pins that a
// GatewayConfig without its own credential is checked with the class token —
// the one that will write the tunnel's configuration.
func TestResolveTunnelClaimForGateway_VerifiesWithTheClassCredential(t *testing.T) {
	t.Parallel()

	verifier := &recordingVerifier{proof: tunnelownership.ProofRefuted}
	objects := append(classFixtures(), claimGatewayConfig(nil), tokenSecret(t))
	resolver := newClaimResolver(t, verifier, objects...)

	claim, err := resolver.ResolveTunnelClaimForGateway(context.Background(),
		gatewayWithInfra("cf.k8s.lex.la", "GatewayConfig", "edge-config"))
	require.NoError(t, err)
	require.NotNil(t, claim)

	assert.Equal(t, testTunnelUUID, claim.TunnelID)
	assert.Equal(t, tunnelownership.ProofRefuted, claim.Proof, "the verifier's verdict must be carried unchanged")
	assert.Equal(t, []string{"class-api-token"}, verifier.apiTokens)
	assert.Equal(t, []string{testTunnelUUID}, verifier.tunnels)
}

func TestResolveTunnelClaimForGateway_VerifiesWithTheOverrideCredential(t *testing.T) {
	t.Parallel()

	verifier := &recordingVerifier{proof: tunnelownership.ProofVerified}
	tenantCredentials := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "tenant-credentials", Namespace: testGwNamespace},
		Data:       map[string][]byte{"api-token": []byte("tenant-api-token")},
	}
	objects := append(classFixtures(),
		claimGatewayConfig(&v1alpha1.LocalSecretReference{Name: "tenant-credentials"}), tokenSecret(t), tenantCredentials)
	resolver := newClaimResolver(t, verifier, objects...)

	claim, err := resolver.ResolveTunnelClaimForGateway(context.Background(),
		gatewayWithInfra("cf.k8s.lex.la", "GatewayConfig", "edge-config"))
	require.NoError(t, err)
	require.NotNil(t, claim)

	assert.Equal(t, tunnelownership.ProofVerified, claim.Proof)
	assert.Equal(t, []string{"tenant-api-token"}, verifier.apiTokens)
}

// TestResolveTunnelClaimForGateway_UnreadableCredentialIsUnknown pins that a
// credential that cannot be read leaves the claim unproven rather than
// failing it: the claim stays in the arbitration, where possession can still
// defend it, and nothing is asked with an empty token.
func TestResolveTunnelClaimForGateway_UnreadableCredentialIsUnknown(t *testing.T) {
	t.Parallel()

	verifier := &recordingVerifier{proof: tunnelownership.ProofVerified}
	resolver := newClaimResolver(t, verifier, claimGatewayConfig(nil), tokenSecret(t))

	claim, err := resolver.ResolveTunnelClaimForGateway(context.Background(),
		gatewayWithInfra("cf.k8s.lex.la", "GatewayConfig", "edge-config"))
	require.NoError(t, err)
	require.NotNil(t, claim)

	assert.Equal(t, testTunnelUUID, claim.TunnelID)
	assert.Equal(t, tunnelownership.ProofUnknown, claim.Proof)
	assert.Empty(t, verifier.apiTokens)
}

func TestResolveTunnelClaimForGateway_UnreadableTokenIsAnError(t *testing.T) {
	t.Parallel()

	verifier := &recordingVerifier{proof: tunnelownership.ProofVerified}
	objects := append(classFixtures(), claimGatewayConfig(nil))
	resolver := newClaimResolver(t, verifier, objects...)

	_, err := resolver.ResolveTunnelClaimForGateway(context.Background(),
		gatewayWithInfra("cf.k8s.lex.la", "GatewayConfig", "edge-config"))
	require.Error(t, err)
	assert.Empty(t, verifier.apiTokens)
}
