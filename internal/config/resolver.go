// Package config provides configuration resolution from GatewayClassConfig resources.
package config

import (
	"context"
	"sync"
	"time"

	"github.com/cloudflare/cloudflare-go/v6"
	"github.com/cloudflare/cloudflare-go/v6/accounts"
	"github.com/cloudflare/cloudflare-go/v6/option"
	"github.com/cockroachdb/errors"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/api/v1alpha1"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/metrics"
)

const (
	// ParametersRefGroup is the API group for GatewayClassConfig.
	ParametersRefGroup = "cf.k8s.lex.la"
	// ParametersRefKind is the kind for GatewayClassConfig.
	ParametersRefKind = "GatewayClassConfig"
)

// ResolvedConfig contains all configuration resolved from GatewayClassConfig and Secrets.
type ResolvedConfig struct {
	// Cloudflare API credentials
	APIToken  string
	AccountID string

	// Tunnel configuration
	TunnelID    string
	TunnelToken string

	// Cloudflared deployment settings
	CloudflaredEnabled   bool
	CloudflaredReplicas  int32
	CloudflaredNamespace string
	CloudflaredProtocol  string

	// AWG sidecar settings
	AWGSecretName      string
	AWGInterfacePrefix string

	// Reference to the source config for watch purposes
	ConfigName string
}

// Resolver resolves GatewayClassConfig from GatewayClass parametersRef.
type Resolver struct {
	client           client.Client
	defaultNamespace string
	metrics          metrics.Collector

	// accountIDCache caches resolved account IDs by config name to avoid
	// repeated API calls. Key is config name, value is account ID.
	accountIDCache sync.Map
}

// NewResolver creates a new config Resolver.
func NewResolver(c client.Client, defaultNamespace string, metricsCollector metrics.Collector) *Resolver {
	return &Resolver{
		client:           c,
		defaultNamespace: defaultNamespace,
		metrics:          metricsCollector,
	}
}

//nolint:wrapcheck // errors.Newf creates new errors
func (r *Resolver) ResolveFromGatewayClass(
	ctx context.Context,
	gatewayClass *gatewayv1.GatewayClass,
) (*ResolvedConfig, error) {
	if gatewayClass.Spec.ParametersRef == nil {
		return nil, errors.New("GatewayClass has no parametersRef")
	}

	ref := gatewayClass.Spec.ParametersRef
	if string(ref.Group) != ParametersRefGroup {
		return nil, errors.Newf("unsupported parametersRef group: %s (expected %s)", ref.Group, ParametersRefGroup)
	}

	if string(ref.Kind) != ParametersRefKind {
		return nil, errors.Newf("unsupported parametersRef kind: %s (expected %s)", ref.Kind, ParametersRefKind)
	}

	config := &v1alpha1.GatewayClassConfig{}

	err := r.client.Get(ctx, types.NamespacedName{Name: ref.Name}, config)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to get GatewayClassConfig %s", ref.Name)
	}

	return r.resolveConfig(ctx, config)
}

// ResolveFromGatewayClassName resolves configuration by GatewayClass name.
func (r *Resolver) ResolveFromGatewayClassName(
	ctx context.Context,
	gatewayClassName string,
) (*ResolvedConfig, error) {
	gatewayClass := &gatewayv1.GatewayClass{}

	err := r.client.Get(ctx, types.NamespacedName{Name: gatewayClassName}, gatewayClass)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to get GatewayClass %s", gatewayClassName)
	}

	return r.ResolveFromGatewayClass(ctx, gatewayClass)
}

//nolint:funcorder,wrapcheck,funlen // private helper, errors.Newf creates new errors
func (r *Resolver) resolveConfig(ctx context.Context, config *v1alpha1.GatewayClassConfig) (*ResolvedConfig, error) {
	resolved := &ResolvedConfig{
		TunnelID:             config.Spec.TunnelID,
		CloudflaredEnabled:   config.Spec.IsCloudflaredEnabled(),
		CloudflaredReplicas:  config.Spec.GetCloudflaredReplicas(),
		CloudflaredNamespace: config.Spec.GetCloudflaredNamespace(),
		CloudflaredProtocol:  config.Spec.Cloudflared.Protocol,
		ConfigName:           config.Name,
	}

	// Resolve AWG config
	if config.Spec.Cloudflared.AWG != nil {
		resolved.AWGSecretName = config.Spec.Cloudflared.AWG.SecretName

		resolved.AWGInterfacePrefix = config.Spec.Cloudflared.AWG.InterfacePrefix
		if resolved.AWGInterfacePrefix == "" {
			resolved.AWGInterfacePrefix = "awg-cfd"
		}
	}

	// Resolve Cloudflare credentials from Secret
	credentialsRef := config.Spec.CloudflareCredentialsSecretRef

	credentialsSecret, err := r.getSecret(ctx, credentialsRef.Name, credentialsRef.Namespace)
	if err != nil {
		return nil, errors.Wrap(err, "failed to get Cloudflare credentials secret")
	}

	apiTokenKey := credentialsRef.GetAPITokenKey()

	apiToken, ok := credentialsSecret.Data[apiTokenKey]
	if !ok {
		return nil, errors.Newf("secret %s/%s does not contain key %s",
			credentialsSecret.Namespace, credentialsSecret.Name, apiTokenKey)
	}

	resolved.APIToken = string(apiToken)

	// Account ID priority: spec > secret > auto-detect (handled later)
	if config.Spec.AccountID != "" {
		resolved.AccountID = config.Spec.AccountID
	} else if accountID, ok := credentialsSecret.Data["account-id"]; ok {
		resolved.AccountID = string(accountID)
	}

	// Resolve tunnel token if cloudflared management is enabled
	if resolved.CloudflaredEnabled {
		if config.Spec.TunnelTokenSecretRef == nil {
			return nil, errors.New("tunnelTokenSecretRef is required when cloudflared.enabled is true")
		}

		tokenRef := config.Spec.TunnelTokenSecretRef

		tokenSecret, secretErr := r.getSecret(ctx, tokenRef.Name, tokenRef.Namespace)
		if secretErr != nil {
			return nil, errors.Wrap(secretErr, "failed to get tunnel token secret")
		}

		tokenKey := tokenRef.GetTunnelTokenKey()

		tunnelToken, ok := tokenSecret.Data[tokenKey]
		if !ok {
			return nil, errors.Newf("secret %s/%s does not contain key %s",
				tokenSecret.Namespace, tokenSecret.Name, tokenKey)
		}

		resolved.TunnelToken = string(tunnelToken)
	}

	return resolved, nil
}

//nolint:funcorder // private helper
func (r *Resolver) getSecret(ctx context.Context, name, namespace string) (*corev1.Secret, error) {
	if namespace == "" {
		namespace = r.defaultNamespace
	}

	secret := &corev1.Secret{}

	err := r.client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, secret)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to get secret %s/%s", namespace, name)
	}

	return secret, nil
}

// CreateCloudflareClient creates a Cloudflare API client from resolved config.
func (r *Resolver) CreateCloudflareClient(resolved *ResolvedConfig) *cloudflare.Client {
	return cloudflare.NewClient(option.WithAPIToken(resolved.APIToken))
}

//nolint:wrapcheck // errors.Newf creates new errors
func (r *Resolver) ResolveAccountID(ctx context.Context, cfClient *cloudflare.Client, resolved *ResolvedConfig) (string, error) {
	// If account ID is already in config, use it directly
	if resolved.AccountID != "" {
		return resolved.AccountID, nil
	}

	// Check cache first
	if cached, ok := r.accountIDCache.Load(resolved.ConfigName); ok {
		if accountID, valid := cached.(string); valid {
			return accountID, nil
		}
	}

	// Auto-detect from API
	startTime := time.Now()

	result, err := cfClient.Accounts.List(ctx, accounts.AccountListParams{})
	if err != nil {
		r.metrics.RecordAPICall(ctx, "list", "accounts", "error", time.Since(startTime))
		r.metrics.RecordAPIError(ctx, "list", metrics.ClassifyCloudflareError(err))

		return "", errors.Wrap(err, "failed to list accounts")
	}

	r.metrics.RecordAPICall(ctx, "list", "accounts", "success", time.Since(startTime))

	accountList := result.Result
	if len(accountList) == 0 {
		return "", errors.New("no accounts found for this API token")
	}

	if len(accountList) > 1 {
		return "", errors.Newf("multiple accounts found (%d), please specify account-id in credentials secret", len(accountList))
	}

	accountID := accountList[0].ID

	// Cache the resolved account ID
	r.accountIDCache.Store(resolved.ConfigName, accountID)

	return accountID, nil
}

//nolint:wrapcheck // errors.Newf creates new errors
func (r *Resolver) GetConfigForGatewayClass(
	ctx context.Context,
	gatewayClass *gatewayv1.GatewayClass,
) (*v1alpha1.GatewayClassConfig, error) {
	if gatewayClass.Spec.ParametersRef == nil {
		return nil, errors.New("GatewayClass has no parametersRef")
	}

	ref := gatewayClass.Spec.ParametersRef
	if string(ref.Group) != ParametersRefGroup || string(ref.Kind) != ParametersRefKind {
		return nil, errors.Newf("unsupported parametersRef: %s/%s", ref.Group, ref.Kind)
	}

	config := &v1alpha1.GatewayClassConfig{}

	err := r.client.Get(ctx, types.NamespacedName{Name: ref.Name}, config)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to get GatewayClassConfig %s", ref.Name)
	}

	return config, nil
}
