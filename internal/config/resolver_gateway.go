package config

import (
	"context"

	"github.com/cockroachdb/errors"
	"k8s.io/apimachinery/pkg/types"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/api/v1alpha1"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/tunnel"
)

// GatewayParametersRefKind is the Gateway.spec.infrastructure.parametersRef
// kind that opts a Gateway into a dedicated data plane.
const GatewayParametersRefKind = "GatewayConfig"

// Default Secret keys for the per-Gateway secrets, matching the chart's
// conventions for the shared proxy.
const (
	tunnelTokenSecretKey = "tunnel-token"
	authTokenSecretKey   = "auth-token"
)

// ErrInvalidParameters classifies a Gateway whose
// infrastructure.parametersRef cannot be honoured: unsupported group/kind,
// missing referent, or invalid referenced material (absent/garbled connector
// token). The Gateway reconciler maps it onto Accepted=False with reason
// InvalidParameters, the condition shape the Gateway API recommends for this
// failure class. Transient infrastructure failures (API errors) are
// deliberately NOT wrapped with this sentinel.
var ErrInvalidParameters = errors.New("invalid infrastructure parametersRef")

// PerGatewayConfig is the resolution result for a Gateway opted into a
// dedicated data plane via infrastructure.parametersRef.
type PerGatewayConfig struct {
	// ResolvedConfig carries what the Cloudflare ingress sync needs: the API
	// token (override or class fallback) plus the tunnel identity PARSED from
	// the connector token — the tunnel always belongs to the token's account,
	// so AccountID is the token's account tag, never the class setting.
	ResolvedConfig

	// TunnelToken is the raw connector token handed to the rendered proxy
	// pods via their token Secret.
	TunnelToken string

	// TunnelTokenSecret names the token Secret (Gateway namespace) for watch
	// and rotation-hash purposes.
	TunnelTokenSecret types.NamespacedName

	// AuthToken optionally protects the per-Gateway proxy config API; the
	// controller authenticates its pushes with it. Empty = no auth.
	AuthToken string

	// GatewayConfig is the source object carrying the render knobs
	// (replicas, autoscaling, resources, image).
	GatewayConfig *v1alpha1.GatewayConfig
}

// HasInfrastructureParametersRef reports whether the Gateway opts into a
// per-Gateway data plane. It deliberately does not validate the ref — an
// unsupported group/kind is still an opt-in attempt and must surface as
// InvalidParameters rather than silently falling back to the shared plane.
func HasInfrastructureParametersRef(gateway *gatewayv1.Gateway) bool {
	return gateway.Spec.Infrastructure != nil && gateway.Spec.Infrastructure.ParametersRef != nil
}

// ResolveForGateway resolves the per-Gateway data-plane configuration. A
// Gateway without infrastructure.parametersRef returns (nil, nil) — shared
// mode, byte-for-byte the pre-existing behaviour. Resolution failures caused
// by the referenced material classify as ErrInvalidParameters (see the
// sentinel's doc for the status mapping).
func (r *Resolver) ResolveForGateway(
	ctx context.Context,
	gateway *gatewayv1.Gateway,
) (*PerGatewayConfig, error) {
	if !HasInfrastructureParametersRef(gateway) {
		return nil, nil //nolint:nilnil // nil,nil IS the shared-mode signal, documented contract
	}

	gwConfig, err := r.getGatewayConfig(ctx, gateway)
	if err != nil {
		return nil, err
	}

	token, tokenSecretName, err := r.readTunnelToken(ctx, gateway.Namespace, gwConfig)
	if err != nil {
		return nil, err
	}

	parsed, err := tunnel.ParseTunnelToken(token)
	if err != nil {
		return nil, errors.Wrapf(ErrInvalidParameters,
			"tunnel token in secret %s/%s does not parse: %v", gateway.Namespace, tokenSecretName.Name, err)
	}

	apiToken, err := r.resolveGatewayAPIToken(ctx, gateway, gwConfig)
	if err != nil {
		return nil, err
	}

	authToken, err := r.readAuthToken(ctx, gateway.Namespace, gwConfig)
	if err != nil {
		return nil, err
	}

	return &PerGatewayConfig{
		ResolvedConfig: ResolvedConfig{
			APIToken: apiToken,
			// The tunnel lives in the token's account by construction.
			AccountID:  parsed.AccountTag,
			TunnelID:   parsed.TunnelID.String(),
			ConfigName: "gatewayconfig:" + gwConfig.Namespace + "/" + gwConfig.Name,
		},
		TunnelToken:       token,
		TunnelTokenSecret: tokenSecretName,
		AuthToken:         authToken,
		GatewayConfig:     gwConfig,
	}, nil
}

// getGatewayConfig validates the parametersRef group/kind and fetches the
// referent from the Gateway's namespace (LocalParametersReference is
// namespace-local by spec).
func (r *Resolver) getGatewayConfig(
	ctx context.Context,
	gateway *gatewayv1.Gateway,
) (*v1alpha1.GatewayConfig, error) {
	ref := gateway.Spec.Infrastructure.ParametersRef

	if string(ref.Group) != ParametersRefGroup || string(ref.Kind) != GatewayParametersRefKind {
		return nil, errors.Wrapf(ErrInvalidParameters,
			"unsupported infrastructure parametersRef %s/%s (expected %s/%s)",
			ref.Group, ref.Kind, ParametersRefGroup, GatewayParametersRefKind)
	}

	gwConfig := &v1alpha1.GatewayConfig{}

	err := r.client.Get(ctx, types.NamespacedName{Name: ref.Name, Namespace: gateway.Namespace}, gwConfig)
	if err != nil {
		return nil, errors.Wrapf(ErrInvalidParameters,
			"GatewayConfig %s/%s: %v", gateway.Namespace, ref.Name, err)
	}

	return gwConfig, nil
}

// readTunnelToken fetches the connector token from the namespace-local Secret.
func (r *Resolver) readTunnelToken(
	ctx context.Context,
	namespace string,
	gwConfig *v1alpha1.GatewayConfig,
) (string, types.NamespacedName, error) {
	ref := gwConfig.Spec.TunnelTokenSecretRef
	secretName := types.NamespacedName{Name: ref.Name, Namespace: namespace}

	secret, err := r.getSecret(ctx, ref.Name, namespace)
	if err != nil {
		return "", secretName, errors.Wrapf(ErrInvalidParameters,
			"tunnel token secret %s/%s: %v", namespace, ref.Name, err)
	}

	key := ref.KeyOr(tunnelTokenSecretKey)

	token := secret.Data[key]
	if len(token) == 0 {
		return "", secretName, errors.Wrapf(ErrInvalidParameters,
			"tunnel token secret %s/%s has no %q key (or it is empty)", namespace, ref.Name, key)
	}

	return string(token), secretName, nil
}

// resolveGatewayAPIToken returns the Cloudflare API token for the Gateway's
// tunnel-document writes: the GatewayConfig-level override when set
// (namespace defaulting to the GatewayConfig's own namespace), otherwise the
// token resolved from the Gateway's GatewayClass → GatewayClassConfig chain.
func (r *Resolver) resolveGatewayAPIToken(
	ctx context.Context,
	gateway *gatewayv1.Gateway,
	gwConfig *v1alpha1.GatewayConfig,
) (string, error) {
	if override := gwConfig.Spec.CloudflareCredentialsSecretRef; override != nil {
		namespace := override.Namespace
		if namespace == "" {
			namespace = gwConfig.Namespace
		}

		secret, err := r.getSecret(ctx, override.Name, namespace)
		if err != nil {
			return "", errors.Wrapf(ErrInvalidParameters,
				"cloudflare credentials secret %s/%s: %v", namespace, override.Name, err)
		}

		key := override.GetAPITokenKey()

		token := secret.Data[key]
		if len(token) == 0 {
			return "", errors.Wrapf(ErrInvalidParameters,
				"cloudflare credentials secret %s/%s has no %q key (or it is empty)", namespace, override.Name, key)
		}

		return string(token), nil
	}

	classResolved, err := r.ResolveFromGatewayClassName(ctx, string(gateway.Spec.GatewayClassName))
	if err != nil {
		// Class-chain failures are not the Gateway's parametersRef being
		// invalid — keep them un-sentinelled (transient or class-level
		// misconfiguration with its own surfacing).
		return "", errors.Wrap(err, "resolving class credentials for per-Gateway data plane")
	}

	return classResolved.APIToken, nil
}

// readAuthToken fetches the optional config-API bearer token.
func (r *Resolver) readAuthToken(
	ctx context.Context,
	namespace string,
	gwConfig *v1alpha1.GatewayConfig,
) (string, error) {
	ref := gwConfig.Spec.AuthTokenSecretRef
	if ref == nil {
		return "", nil
	}

	secret, err := r.getSecret(ctx, ref.Name, namespace)
	if err != nil {
		return "", errors.Wrapf(ErrInvalidParameters,
			"auth token secret %s/%s: %v", namespace, ref.Name, err)
	}

	key := ref.KeyOr(authTokenSecretKey)

	token := secret.Data[key]
	if len(token) == 0 {
		return "", errors.Wrapf(ErrInvalidParameters,
			"auth token secret %s/%s has no %q key (or it is empty)", namespace, ref.Name, key)
	}

	return string(token), nil
}
