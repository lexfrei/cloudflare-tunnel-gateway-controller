package controller

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/api/v1alpha1"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/cfmetrics"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/config"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/tunnelownership"
)

const (
	claimsTunnel      = "22222222-2222-2222-2222-222222222222"
	claimsClassTunnel = "11111111-1111-1111-1111-111111111111"
	claimsStaleTunnel = "33333333-3333-3333-3333-333333333333"
)

// TestCollectTunnelClaims_TokenBeatsAStaleAdvertisedAddress pins the precedence
// the rule stands on: when a Gateway has BOTH a readable token and a different
// advertised address, the claim is the token's tunnel.
//
// Partitions key on the token's tunnel, so a claim keyed on anything else
// bypasses arbitration end to end rather than merely misreporting: a Gateway
// still advertising a tunnel it used to serve, with its token retargeted at a
// neighbour's, would be arbitrated on the old tunnel — uncontested, so never
// refused — and then partitioned into the neighbour's, collecting the merged
// routes and their backend-mTLS keys. That is the breach the rule exists to
// stop, so the precedence is pinned here and not left to the two tests either
// side of it, which each hold only one of the two inputs.
func TestCollectTunnelClaims_TokenBeatsAStaleAdvertisedAddress(t *testing.T) {
	t.Parallel()

	// Incumbent: serving the contested tunnel, token agrees.
	incumbent := claimsGateway("team-a", "gw", 0, "a-token")
	incumbent.Status.Addresses = []gatewayv1.GatewayStatusAddress{
		{Value: claimsTunnel + cfArgotunnelSuffix},
	}

	// Attacker: still advertising a tunnel of its own, token retargeted at the
	// incumbent's. Younger, so age cannot be what refuses it.
	attacker := claimsGateway("team-b", "gw", 5, "b-token")
	attacker.Status.Addresses = []gatewayv1.GatewayStatusAddress{
		{Value: claimsStaleTunnel + cfArgotunnelSuffix},
	}

	fakeClient := setupGatewayFakeClient(
		incumbent,
		attacker,
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
		claimsGatewayConfig("team-a", "a-token"),
		claimsGatewayConfig("team-b", "b-token"),
	)

	resolver := config.NewResolver(fakeClient, "default", cfmetrics.NewNoopCollector())

	claims, err := collectTunnelClaims(context.Background(), fakeClient, resolver, "test-controller", claimsClassTunnel)
	require.NoError(t, err)

	byKey := map[string]tunnelownership.Claim{}
	for _, claim := range claims {
		byKey[claim.Key] = claim
	}

	require.Contains(t, byKey, "team-b/gw")
	assert.Equal(t, claimsTunnel, byKey["team-b/gw"].TunnelID,
		"the claim must be the tunnel the token names, which is also the tunnel the partition uses")

	rejected := tunnelownership.Arbitrate(claimsClassTunnel, claims)
	assert.Contains(t, rejected, "team-b/gw",
		"a retargeted token must be arbitrated on its new tunnel, not on the address left over from its old one")
	assert.NotContains(t, rejected, "team-a/gw",
		"the Gateway actually serving the tunnel keeps it")
}

// TestCollectTunnelClaims_AdvertisedSurvivesAnUnreadableToken pins the
// property the whole possession rule rests on: a Gateway that is currently
// serving a tunnel keeps holding it even while its connector-token Secret is
// unreadable.
//
// Advertisement comes from status and needs no Secret, so dropping such a
// Gateway from the claim set would hand its tunnel to any challenger — and an
// ordinary delete-then-create token rotation is enough to open that window.
// The challenger would then be accepted, rendered, and start advertising the
// tunnel itself, so the incumbent could never take it back.
func TestCollectTunnelClaims_AdvertisedSurvivesAnUnreadableToken(t *testing.T) {
	t.Parallel()

	// Incumbent: younger, already serving the tunnel, token Secret missing.
	incumbent := claimsGateway("team-a", "gw", 1, "a-token")
	incumbent.Status.Addresses = []gatewayv1.GatewayStatusAddress{
		{Value: claimsTunnel + cfArgotunnelSuffix},
	}

	// Challenger: older, valid token naming the same tunnel.
	challenger := claimsGateway("team-b", "gw", 0, "b-token")

	// A Gateway on someone else's GatewayClass is not ours to arbitrate:
	// including it in one layer and not another is how the layers diverge.
	foreign := claimsGateway("team-c", "gw", 2, "c-token")
	foreign.Spec.GatewayClassName = "someone-elses-class"

	fakeClient := setupGatewayFakeClient(
		incumbent,
		challenger,
		foreign,
		claimsGatewayClass(),
		claimsClassConfig(),
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "creds", Namespace: "default"},
			Data:       map[string][]byte{"api-token": []byte("test-token")},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "b-token", Namespace: "team-b"},
			Data:       map[string][]byte{"tunnel-token": []byte(infraTunnelTokenFor(t, claimsTunnel))},
		},
		claimsGatewayConfig("team-a", "a-token"),
		claimsGatewayConfig("team-b", "b-token"),
	)

	resolver := config.NewResolver(fakeClient, "default", cfmetrics.NewNoopCollector())

	claims, err := collectTunnelClaims(context.Background(), fakeClient, resolver, "test-controller", claimsClassTunnel)
	require.NoError(t, err)

	byKey := map[string]tunnelownership.Claim{}
	for _, claim := range claims {
		byKey[claim.Key] = claim
	}

	assert.NotContains(t, byKey, "team-c/gw",
		"a Gateway on a foreign GatewayClass must not appear in our claim set")

	held, ok := byKey["team-a/gw"]
	require.True(t, ok, "a Gateway serving a tunnel must stay in the claim set while its Secret is unreadable")
	assert.Equal(t, claimsTunnel, held.Advertised, "its possession comes from status, not from the Secret")

	rejected := tunnelownership.Arbitrate(claimsClassTunnel, claims)
	assert.Contains(t, rejected, "team-b/gw",
		"the challenger must not take a tunnel its holder is still serving")
	assert.NotContains(t, rejected, "team-a/gw",
		"the holder must not be evicted while its token is briefly unreadable")
}

// TestCollectTunnelClaims_SharedPlaneAddressIsNotPossession pins the other half
// of the advertised fallback: an address naming the CLASS tunnel is not
// possession of it.
//
// A Gateway migrating off the shared plane still advertises the class tunnel
// from its shared-plane days. Between adding infrastructure.parametersRef and
// its GatewayConfig or token Secret existing, the fallback would read that
// leftover as a claim on the class tunnel and refuse the Gateway for naming a
// tunnel nothing of its own ever named. Nothing is surrendered by ignoring it:
// the class tunnel belongs to the operator and no dedicated plane can hold it,
// so there is no possession here to protect. The Gateway is still served
// nothing — its config does not resolve — but it is told that, rather than
// accused of claiming its operator's tunnel.
func TestCollectTunnelClaims_SharedPlaneAddressIsNotPossession(t *testing.T) {
	t.Parallel()

	migrating := claimsGateway("team-a", "gw", 1, "a-token")
	migrating.Status.Addresses = []gatewayv1.GatewayStatusAddress{
		{Value: claimsClassTunnel + cfArgotunnelSuffix},
	}

	fakeClient := setupGatewayFakeClient(
		migrating,
		claimsGatewayClass(),
		claimsClassConfig(),
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "creds", Namespace: "default"},
			Data:       map[string][]byte{"api-token": []byte("test-token")},
		},
		claimsGatewayConfig("team-a", "a-token"),
	)

	resolver := config.NewResolver(fakeClient, "default", cfmetrics.NewNoopCollector())

	claims, err := collectTunnelClaims(context.Background(), fakeClient, resolver, "test-controller", claimsClassTunnel)
	require.NoError(t, err)

	rejected := tunnelownership.Arbitrate(claimsClassTunnel, claims)
	assert.NotContains(t, rejected, "team-a/gw",
		"a leftover shared-plane address must not be read as a claim on the class tunnel")
}

func claimsGateway(namespace, name string, ageHours int, tokenSecret string) *gatewayv1.Gateway {
	return &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         namespace,
			UID:               types.UID(namespace + "/" + name),
			CreationTimestamp: metav1.NewTime(time.Date(2026, time.January, 1, ageHours, 0, 0, 0, time.UTC)),
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "cloudflare-tunnel",
			Listeners:        []gatewayv1.Listener{{Name: "http", Port: 80, Protocol: "HTTP"}},
			Infrastructure: &gatewayv1.GatewayInfrastructure{
				ParametersRef: &gatewayv1.LocalParametersReference{
					Group: "cf.k8s.lex.la", Kind: "GatewayConfig", Name: "cfg-" + tokenSecret,
				},
			},
		},
	}
}

func claimsGatewayConfig(namespace, tokenSecret string) *v1alpha1.GatewayConfig {
	return &v1alpha1.GatewayConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "cfg-" + tokenSecret, Namespace: namespace},
		Spec: v1alpha1.GatewayConfigSpec{
			TunnelTokenSecretRef: v1alpha1.LocalSecretReference{Name: tokenSecret},
		},
	}
}

func claimsGatewayClass() *gatewayv1.GatewayClass {
	return &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "cloudflare-tunnel"},
		Spec: gatewayv1.GatewayClassSpec{
			ControllerName: "test-controller",
			ParametersRef: &gatewayv1.ParametersReference{
				Group: config.ParametersRefGroup, Kind: config.ParametersRefKind, Name: "class-config",
			},
		},
	}
}

func claimsClassConfig() *v1alpha1.GatewayClassConfig {
	return &v1alpha1.GatewayClassConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "class-config"},
		Spec: v1alpha1.GatewayClassConfigSpec{
			TunnelID:                       claimsClassTunnel,
			CloudflareCredentialsSecretRef: v1alpha1.SecretReference{Name: "creds", Namespace: "default"},
		},
	}
}
