package controller

import (
	"context"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/tunnelownership"
)

// statusTunnelResolver reads the tunnel a Gateway's connector token names and
// nothing else, so a Gateway is never dropped from the arbitration because
// some unrelated Secret was briefly unreadable.
type statusTunnelResolver interface {
	ResolveTunnelIdentityForGateway(ctx context.Context, gateway *gatewayv1.Gateway) (string, error)
}

// collectTunnelClaims returns every opted-in Gateway's claimed tunnel.
//
// Three layers arbitrate tunnel ownership — the route partitioner (whose
// config is built), the Gateway reconciler (whose status says so), and the
// infra reconciler (whose data plane is rendered) — and they must reach the
// same verdict. Sharing the pure decision function is not enough for that: fed
// different claim sets, the same function returns different answers. So the
// claim set is built HERE, once, and all three read it.
//
// Two properties are load-bearing and easy to lose by resolving separately:
//
//   - The class filter. A Gateway on a foreign GatewayClass is not ours to
//     arbitrate; including it in one layer and not another lets that layer
//     refuse a Gateway the others happily serve.
//   - The resolver. Arbitration needs only the tunnel the token names, so it
//     stops there. The fuller resolvers additionally read the generated auth
//     Secret (ResolveForGateway) or the Cloudflare API token, which falls back
//     to the class chain (ResolveStatusConfigForGateway) — either would drop a
//     claimant out of the set during an unrelated Secret's bootstrap or
//     rotation window, and the newcomer claiming its tunnel would then be
//     accepted, which is precisely the breach this rule exists to prevent.
//
// sharedTunnelID must already be canonical, like the value handed to
// tunnelownership.Arbitrate — advertised addresses are canonicalized on read,
// so a raw class tunnelID would miss the comparison below on case alone.
//
// What the sharing costs: every arbitrating reconcile rebuilds the whole set —
// two cache-served lists plus a GatewayConfig and a Secret read per opted-in
// Gateway. The reads are cheap; the deepcopies are not free at scale, and
// GatewayReconciler's Secret watch fans a single Secret write out to every
// managed Gateway, which makes that path quadratic in the number of dedicated
// planes. Unmeasurable at a handful, worth measuring at hundreds.
func collectTunnelClaims(
	ctx context.Context,
	cli client.Client,
	resolver statusTunnelResolver,
	controllerName string,
	sharedTunnelID string,
) ([]tunnelownership.Claim, error) {
	gateways, err := managedInfraGateways(ctx, cli, controllerName)
	if err != nil {
		return nil, err
	}

	claims := make([]tunnelownership.Claim, 0, len(gateways))

	for _, gateway := range gateways {
		advertised := advertisedTunnelID(gateway)

		claimed := claimedTunnelID(ctx, resolver, gateway, advertised, sharedTunnelID)
		if claimed == "" {
			continue
		}

		claims = append(claims, tunnelownership.Claim{
			Key:        gateway.Namespace + "/" + gateway.Name,
			Namespace:  gateway.Namespace,
			TunnelID:   claimed,
			CreatedAt:  gateway.CreationTimestamp.Time,
			UID:        string(gateway.UID),
			Advertised: advertised,
		})
	}

	return claims, nil
}

// claimedTunnelID returns the tunnel this Gateway claims, empty when it claims
// none. The token it names wins; the tunnel it already advertises is the
// fallback.
//
// That fallback is what keeps possession across a token rotation: a Gateway
// still SERVING a tunnel keeps holding it while its connector-token Secret is
// unreadable, because possession is recorded in status and needs no Secret to
// read. Dropping it would hand the tunnel to any challenger, and an ordinary
// delete-then-create rotation opens exactly that window — after which the
// challenger advertises the tunnel itself and the holder can never take it back.
func claimedTunnelID(
	ctx context.Context,
	resolver statusTunnelResolver,
	gateway *gatewayv1.Gateway,
	advertised string,
	sharedTunnelID string,
) string {
	claimed := advertised

	// An address naming the CLASS tunnel is not possession of it. The class
	// tunnel is the operator's and no dedicated plane can hold it, so there is
	// nothing here to defend — while a Gateway migrating off the shared plane
	// carries exactly that address until its own plane is up. Reading it as a
	// claim would refuse that Gateway for naming a tunnel neither its
	// GatewayConfig nor its token ever named.
	if claimed == sharedTunnelID {
		claimed = ""
	}

	tunnelID, resolveErr := resolver.ResolveTunnelIdentityForGateway(ctx, gateway)

	switch {
	case resolveErr == nil && tunnelID != "":
		claimed = canonicalTunnelID(tunnelID)
	case resolveErr != nil && claimed != "":
		// The claim now rests on the address alone, which is the one case where
		// a refusal cites a tunnel the Gateway's own configuration never named.
		// Debug level: this repeats every sync for as long as the configuration
		// stays unreadable.
		log.FromContext(ctx).V(1).Info(
			"arbitrating a tunnel claim from the advertised address; the Gateway's configuration is unreadable",
			"gateway", gateway.Namespace+"/"+gateway.Name,
			"tunnel", claimed,
			"error", resolveErr.Error())
	}

	return claimed
}

// advertisedTunnelID returns the tunnel this Gateway currently publishes in its
// status addresses, empty when it publishes none. That address is written by
// the Gateway reconciler as "<tunnel-id>.cfargotunnel.com" for external-dns, so
// it doubles as the record of which tunnel the Gateway is actually serving —
// the difference between holding a tunnel and merely asking for it.
func advertisedTunnelID(gateway *gatewayv1.Gateway) string {
	for _, address := range gateway.Status.Addresses {
		value, ok := strings.CutSuffix(address.Value, cfArgotunnelSuffix)
		if ok {
			return canonicalTunnelID(value)
		}
	}

	return ""
}
