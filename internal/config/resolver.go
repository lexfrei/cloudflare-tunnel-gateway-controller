package config

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/cloudflare/cloudflare-go/v7"
	"github.com/cloudflare/cloudflare-go/v7/accounts"
	"github.com/cloudflare/cloudflare-go/v7/option"
	"github.com/cockroachdb/errors"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/api/v1alpha1"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/cfmetrics"
	tracingpkg "github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/tracing"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/tunnel"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/tunnelownership"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/tunnelproof"
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
	APIToken  string `json:"-"`
	AccountID string

	// Tunnel configuration
	TunnelID string

	// AllowSharedTunnels mirrors the GatewayClassConfig field: when true, a
	// dedicated data plane may serve a tunnel another namespace (or this
	// class) already serves. Off by default — see the CRD field's godoc for
	// why an unproven tunnel claim is a cross-tenant problem.
	AllowSharedTunnels bool

	// MaxDataPlanesPerNamespace mirrors the GatewayClassConfig field: the most
	// dedicated data planes one namespace may hold. Nil is unlimited; anything
	// below 1 is rejected at admission.
	MaxDataPlanesPerNamespace *int32

	// Reference to the source config for watch purposes
	ConfigName string
}

// Resolver resolves GatewayClassConfig from GatewayClass parametersRef.
type Resolver struct {
	client           client.Client
	defaultNamespace string
	metrics          cfmetrics.Collector

	// tracing, when true, instruments the Cloudflare API HTTP client with
	// otelhttp so outbound API calls emit client spans. Set via
	// WithCloudflareTracing.
	tracing bool

	// accountIDCache caches auto-detected account IDs to avoid repeated API
	// calls. Key is config name, value is an accountIDCacheEntry.
	accountIDCache sync.Map

	// claimVerifier checks per-Gateway tunnel claims against Cloudflare. It
	// is never nil: NewResolver installs the Cloudflare verifier unless an
	// option supplied another.
	claimVerifier ClaimVerifier
}

// ClaimVerifier decides whether a connector token really holds the tunnel it
// names, asking Cloudflare with apiToken.
type ClaimVerifier interface {
	Verify(ctx context.Context, apiToken string, token *tunnel.Token) tunnelownership.Proof
}

// accountIDCacheEntry is one config's auto-detected account ID together with a
// digest of the credential it was detected with.
//
// The digest is the invalidation: an account ID is a property of the API token,
// not of the config that names it, and rotating the credentials Secret to a
// token for a different Cloudflare account leaves the config name unchanged.
// Comparing the digest keeps the entry bounded at one per config — keying the
// map on the digest instead would leave an entry behind on every rotation.
type accountIDCacheEntry struct {
	tokenDigest [sha256.Size]byte
	accountID   string
}

// ResolverOption configures a Resolver at construction time.
type ResolverOption func(*Resolver)

// WithCloudflareTracing instruments the Cloudflare API HTTP client with
// OpenTelemetry so outbound API calls (config resolution, account detection)
// emit client spans linked to the active trace.
func WithCloudflareTracing() ResolverOption {
	return func(r *Resolver) {
		r.tracing = true
	}
}

// WithClaimVerifier replaces the Cloudflare tunnel-claim verifier. A nil
// verifier keeps the Cloudflare one, so no option can switch the check off.
func WithClaimVerifier(verifier ClaimVerifier) ResolverOption {
	return func(r *Resolver) {
		if verifier != nil {
			r.claimVerifier = verifier
		}
	}
}

// NewResolver creates a new config Resolver.
func NewResolver(c client.Client, defaultNamespace string, metricsCollector cfmetrics.Collector, opts ...ResolverOption) *Resolver {
	resolver := &Resolver{
		client:           c,
		defaultNamespace: defaultNamespace,
		metrics:          metricsCollector,
	}

	for _, opt := range opts {
		opt(resolver)
	}

	if resolver.claimVerifier == nil {
		resolver.claimVerifier = tunnelproof.NewVerifier(func(apiToken string) *cloudflare.Client {
			return cloudflare.NewClient(cloudflareRequestOptions(apiToken, resolver.tracing)...)
		})
	}

	return resolver
}

// ResolveFromGatewayClass resolves the configuration a GatewayClass's
// parametersRef names.
func (r *Resolver) ResolveFromGatewayClass(
	ctx context.Context,
	gatewayClass *gatewayv1.GatewayClass,
) (*ResolvedConfig, error) {
	resolved, err := r.resolveFromGatewayClass(ctx, gatewayClass)

	return resolved, classError(gatewayClass.Name, err)
}

//nolint:funcorder // private helper, kept next to its entry point
func (r *Resolver) resolveFromGatewayClass(
	ctx context.Context,
	gatewayClass *gatewayv1.GatewayClass,
) (*ResolvedConfig, error) {
	config, err := r.readClassConfig(ctx, gatewayClass.Spec.ParametersRef)
	if err != nil {
		return nil, err
	}

	return r.resolveConfig(ctx, config)
}

// readClassConfig reads the GatewayClassConfig a GatewayClass parametersRef
// names, marking a ref that cannot be served, or a config that does not
// exist, as a problem with the class's configuration.
//
//nolint:funcorder // private helper, kept next to the entry points that share it
func (r *Resolver) readClassConfig(
	ctx context.Context,
	ref *gatewayv1.ParametersReference,
) (*v1alpha1.GatewayClassConfig, error) {
	if problem := ParametersRefProblem(ref); problem != "" {
		return nil, invalidClassParameters(errors.New(problem))
	}

	config := &v1alpha1.GatewayClassConfig{}

	err := r.client.Get(ctx, types.NamespacedName{Name: ref.Name}, config)
	if err != nil {
		return nil, invalidIfNotFound(errors.Wrapf(err, "failed to get GatewayClassConfig %s", ref.Name))
	}

	return config, nil
}

// ParametersRefProblem says why a GatewayClass parametersRef cannot be served,
// or returns "" when its shape can. The checks run in order, so a ref of the
// wrong kind is reported for its kind before anything about its namespace.
// GatewayClassConfig is cluster-scoped, and Gateway API requires the
// namespace to be unset for a cluster-scoped referent. It is shared by every
// resolver entry point and the GatewayClass status, so they agree.
func ParametersRefProblem(ref *gatewayv1.ParametersReference) string {
	switch {
	case ref == nil:
		return "spec.parametersRef is required: it must name a " + ParametersRefKind
	case string(ref.Group) != ParametersRefGroup || string(ref.Kind) != ParametersRefKind:
		return fmt.Sprintf("spec.parametersRef must name a %s/%s, not %s/%s",
			ParametersRefGroup, ParametersRefKind, ref.Group, ref.Kind)
	case ref.Namespace != nil:
		return fmt.Sprintf("spec.parametersRef.namespace must be unset: %s is cluster-scoped", ParametersRefKind)
	}

	return ""
}

// RefersToClassConfig reports whether ref names the GatewayClassConfig called
// name, matching group and kind as well so a same-named object of another kind
// is not mistaken for it.
func RefersToClassConfig(ref *gatewayv1.ParametersReference, name string) bool {
	return ref != nil && string(ref.Group) == ParametersRefGroup &&
		string(ref.Kind) == ParametersRefKind && ref.Name == name
}

// TunnelPolicy is the part of a GatewayClassConfig the per-Gateway admission
// rules read: which tunnel the class itself serves, whether the operator permits
// several data planes to share one, and how many dedicated planes one namespace
// may hold.
type TunnelPolicy struct {
	TunnelID           string
	AllowSharedTunnels bool
	// MaxDataPlanesPerNamespace caps how many dedicated data planes one
	// namespace may hold. Nil is unlimited; anything below 1 is rejected at
	// admission.
	MaxDataPlanesPerNamespace *int32
}

// ResolveTunnelPolicyForGatewayClass reads ONLY the class fields those rules
// need, without resolving the Cloudflare credentials.
//
// They run on every per-Gateway reconcile, and going through the full
// resolver would make it inherit the credential Secret read: a missing or
// mid-rotation class credentials Secret would then stall status writes,
// rendering and drift healing for every dedicated data plane, none of which
// depends on those credentials.
func (r *Resolver) ResolveTunnelPolicyForGatewayClass(
	ctx context.Context,
	gatewayClassName string,
) (*TunnelPolicy, error) {
	gatewayClass, err := r.readGatewayClass(ctx, gatewayClassName)
	if err != nil {
		return nil, tunnelPolicyError(gatewayClassName, err)
	}

	classConfig, err := r.readClassConfig(ctx, gatewayClass.Spec.ParametersRef)
	if err != nil {
		return nil, tunnelPolicyError(gatewayClassName, err)
	}

	return &TunnelPolicy{
		TunnelID:                  classConfig.Spec.TunnelID,
		AllowSharedTunnels:        classConfig.Spec.AllowSharedTunnels,
		MaxDataPlanesPerNamespace: classConfig.Spec.MaxDataPlanesPerNamespace,
	}, nil
}

// tunnelPolicyError is classError for the tunnel policy, with one exception:
// a GatewayClass or GatewayClassConfig that does not exist is not classified.
// It may be mid-apply, and this read decides tunnel ownership, so the caller
// retries rather than refusing the Gateway or removing its data plane.
func tunnelPolicyError(gatewayClassName string, err error) error {
	if apierrors.IsNotFound(err) {
		return errors.Wrapf(err, "GatewayClass %q", gatewayClassName)
	}

	return classError(gatewayClassName, err)
}

// ResolveFromGatewayClassName resolves configuration by GatewayClass name.
func (r *Resolver) ResolveFromGatewayClassName(
	ctx context.Context,
	gatewayClassName string,
) (*ResolvedConfig, error) {
	gatewayClass, err := r.readGatewayClass(ctx, gatewayClassName)
	if err != nil {
		return nil, classError(gatewayClassName, err)
	}

	resolved, err := r.resolveFromGatewayClass(ctx, gatewayClass)

	return resolved, classError(gatewayClassName, err)
}

// readGatewayClass reads a GatewayClass by name, marking one that does not
// exist as a problem with the class's configuration.
//
//nolint:funcorder // private helper, kept next to the entry points that share it
func (r *Resolver) readGatewayClass(ctx context.Context, name string) (*gatewayv1.GatewayClass, error) {
	gatewayClass := &gatewayv1.GatewayClass{}

	err := r.client.Get(ctx, types.NamespacedName{Name: name}, gatewayClass)
	if err != nil {
		// The entry point names the class on every error.
		return nil, invalidIfNotFound(errors.Wrap(err, "failed to get GatewayClass"))
	}

	return gatewayClass, nil
}

// errClassParameters marks, inside the resolver, an error that is a
// deterministic problem with a GatewayClass's configuration. classError turns
// it into ErrInvalidParameters at the entry point, where the class is known.
var errClassParameters = errors.New("invalid GatewayClass configuration")

// invalidClassParameters marks err as a deterministic problem with the
// GatewayClass's configuration without changing its message.
//
//nolint:wrapcheck // marking rather than wrapping keeps that message as it is
func invalidClassParameters(err error) error {
	return errors.Mark(err, errClassParameters)
}

// invalidIfNotFound marks a failed read as a problem with the class's
// configuration when the object does not exist. Any other read failure says
// nothing about the configuration and keeps its own identity, so the caller
// retries it.
func invalidIfNotFound(err error) error {
	if apierrors.IsNotFound(err) {
		return invalidClassParameters(err)
	}

	return err
}

// classError is how every GatewayClass entry point returns an error: named
// after the class it belongs to, and, for a configuration problem, classified
// ErrInvalidParameters, so a Gateway's status points at the class's
// spec.parametersRef chain rather than at the Gateway.
func classError(gatewayClassName string, err error) error {
	if err == nil {
		return nil
	}

	named := errors.Wrapf(err, "GatewayClass %q", gatewayClassName)
	if !errors.Is(err, errClassParameters) {
		return named
	}

	return MarkInvalidParameters(named)
}

//nolint:funcorder // private helper
func (r *Resolver) resolveConfig(ctx context.Context, config *v1alpha1.GatewayClassConfig) (*ResolvedConfig, error) {
	// Validate required TunnelID
	if config.Spec.TunnelID == "" {
		return nil, invalidClassParameters(errors.New("tunnelID is required in GatewayClassConfig"))
	}

	resolved := &ResolvedConfig{
		TunnelID:                  config.Spec.TunnelID,
		AllowSharedTunnels:        config.Spec.AllowSharedTunnels,
		MaxDataPlanesPerNamespace: config.Spec.MaxDataPlanesPerNamespace,
		ConfigName:                config.Name,
	}

	// Resolve Cloudflare credentials from Secret
	credentialsRef := config.Spec.CloudflareCredentialsSecretRef

	credentialsSecret, err := r.getSecret(ctx, credentialsRef.Name, credentialsRef.Namespace)
	if err != nil {
		return nil, invalidIfNotFound(errors.Wrap(err, "failed to get Cloudflare credentials secret"))
	}

	apiTokenKey := credentialsRef.GetAPITokenKey()

	apiToken, ok := credentialsSecret.Data[apiTokenKey]
	if !ok {
		return nil, invalidClassParameters(errors.Newf("secret %s/%s does not contain key %s",
			credentialsSecret.Namespace, credentialsSecret.Name, apiTokenKey))
	}

	if len(apiToken) == 0 {
		return nil, invalidClassParameters(errors.Newf("secret %s/%s key %s is empty",
			credentialsSecret.Namespace, credentialsSecret.Name, apiTokenKey))
	}

	resolved.APIToken = string(apiToken)

	// Account ID priority: spec > secret > auto-detect (handled later)
	if config.Spec.AccountID != "" {
		resolved.AccountID = config.Spec.AccountID
	} else if accountID, ok := credentialsSecret.Data["account-id"]; ok {
		resolved.AccountID = string(accountID)
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
	return cloudflare.NewClient(cloudflareRequestOptions(resolved.APIToken, r.tracing)...)
}

// cloudflareRequestOptions builds the cloudflare-go request options. When
// tracing is enabled it adds a traced HTTP client so outbound API calls emit
// client spans; when disabled it passes only the API token so cloudflare-go
// keeps its own default client (outbound path unchanged).
func cloudflareRequestOptions(token string, tracing bool) []option.RequestOption {
	opts := []option.RequestOption{option.WithAPIToken(token)}

	if tracing {
		opts = append(opts, option.WithHTTPClient(&http.Client{
			Transport: tracingpkg.WrapTransport(http.DefaultTransport, true),
		}))
	}

	return opts
}

//nolint:wrapcheck // errors.Newf creates new errors
func (r *Resolver) ResolveAccountID(ctx context.Context, cfClient *cloudflare.Client, resolved *ResolvedConfig) (string, error) {
	// If account ID is already in config, use it directly
	if resolved.AccountID != "" {
		return resolved.AccountID, nil
	}

	// Serve from cache only while the credential that produced the entry is
	// still the credential in hand.
	tokenDigest := sha256.Sum256([]byte(resolved.APIToken))

	if cached, ok := r.accountIDCache.Load(resolved.ConfigName); ok {
		if entry, valid := cached.(accountIDCacheEntry); valid && entry.tokenDigest == tokenDigest {
			return entry.accountID, nil
		}
	}

	// Auto-detect from API
	startTime := time.Now()

	result, err := cfClient.Accounts.List(ctx, accounts.AccountListParams{})
	if err != nil {
		r.metrics.RecordAPICall(ctx, "list", "accounts", "error", time.Since(startTime))
		r.metrics.RecordAPIError(ctx, "list", cfmetrics.ClassifyCloudflareError(err))

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

	// Cache the resolved account ID against the credential it came from
	r.accountIDCache.Store(resolved.ConfigName, accountIDCacheEntry{
		tokenDigest: tokenDigest,
		accountID:   accountID,
	})

	return accountID, nil
}

// GetConfigForGatewayClass reads the GatewayClassConfig a GatewayClass's
// parametersRef names.
func (r *Resolver) GetConfigForGatewayClass(
	ctx context.Context,
	gatewayClass *gatewayv1.GatewayClass,
) (*v1alpha1.GatewayClassConfig, error) {
	config, err := r.readClassConfig(ctx, gatewayClass.Spec.ParametersRef)

	return config, classError(gatewayClass.Name, err)
}
