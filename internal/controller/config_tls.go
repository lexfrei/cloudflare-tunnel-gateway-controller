package controller

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	stderrors "errors"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/cockroachdb/errors"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/configtls"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/render"
)

// ErrPlaintextConfigEndpoint is returned for an http:// config endpoint while
// config API TLS is on. The push is refused rather than sent in the clear.
var ErrPlaintextConfigEndpoint = errors.New("plaintext config API endpoint refused: config API TLS is enabled")

// ensureConfigCA returns the config API CA stored at key, generating it once
// when the Secret does not exist. Create-only, like the shared auth token: a
// replica that loses the create race adopts the stored CA. A Secret that
// exists but holds no usable CA is an error, never overwritten, because a new
// CA would invalidate every leaf already issued.
func ensureConfigCA(ctx context.Context, c client.Client, key types.NamespacedName) (*configtls.Authority, error) {
	var existing corev1.Secret

	err := c.Get(ctx, key, &existing)
	if err == nil {
		return loadConfigCA(&existing, key)
	}

	if !apierrors.IsNotFound(err) {
		return nil, errors.Wrapf(err, "reading config API CA secret %s", key)
	}

	certPEM, keyPEM, err := configtls.NewAuthorityPEM(time.Now())
	if err != nil {
		return nil, errors.Wrap(err, "generating config API CA")
	}

	secret := tlsSecret(key, certPEM, keyPEM)

	if err := c.Create(ctx, secret); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return nil, errors.Wrapf(err, "creating config API CA secret %s", key)
		}

		if err := c.Get(ctx, key, &existing); err != nil {
			return nil, errors.Wrapf(err, "re-reading config API CA secret after create race %s", key)
		}

		return loadConfigCA(&existing, key)
	}

	return loadConfigCA(secret, key)
}

func loadConfigCA(secret *corev1.Secret, key types.NamespacedName) (*configtls.Authority, error) {
	authority, err := configtls.LoadAuthority(secret.Data[corev1.TLSCertKey], secret.Data[corev1.TLSPrivateKeyKey])
	if err != nil {
		return nil, errors.Wrapf(err, "config API CA secret %s does not hold a usable CA", key)
	}

	return authority, nil
}

func tlsSecret(key types.NamespacedName, certPEM, keyPEM []byte) *corev1.Secret {
	return &corev1.Secret{
		Name:      key.Name,
		Namespace: key.Namespace,
		Type:      corev1.SecretTypeTLS,
		Data:      map[string][]byte{corev1.TLSCertKey: certPEM, corev1.TLSPrivateKeyKey: keyPEM},
	}
}

// leafOutcome says what an issuance pass did to a leaf Secret.
type leafOutcome int

const (
	leafValid leafOutcome = iota
	leafIssued
	leafRenewed
	// leafReplacedInvalid means the stored leaf did not verify against the CA
	// (tampered, foreign, broken, or for other names) and was replaced.
	leafReplacedInvalid
)

// ensureSharedLeaf keeps the shared plane's serving certificate at key valid
// for names. It creates the Secret, or updates it in place when the leaf is
// due for renewal or no longer verifies; the proxy follows the mounted files,
// so an update needs no rollout. Every write is conditional (create, or
// update at the read resourceVersion), and a lost race re-reads and accepts
// the winner's leaf when it verifies, so concurrent issuers settle on one
// leaf instead of each overwriting the other.
func ensureSharedLeaf(
	ctx context.Context, c client.Client, authority *configtls.Authority,
	key types.NamespacedName, names []string, now time.Time,
) (leafOutcome, error) {
	var existing corev1.Secret

	err := c.Get(ctx, key, &existing)
	if apierrors.IsNotFound(err) {
		return createSharedLeaf(ctx, c, authority, key, names, now)
	}

	if err != nil {
		return leafValid, errors.Wrapf(err, "reading config API leaf secret %s", key)
	}

	outcome := leafOutcomeFor(authority, &existing, names, now)
	if outcome == leafValid {
		return leafValid, nil
	}

	certPEM, keyPEM, err := authority.Issue(names, now)
	if err != nil {
		return leafValid, errors.Wrap(err, "issuing config API leaf")
	}

	// The stored type is kept: the API server refuses to change it.
	updated := existing.DeepCopy()
	updated.Data = map[string][]byte{corev1.TLSCertKey: certPEM, corev1.TLSPrivateKeyKey: keyPEM}

	if err := c.Update(ctx, updated); err != nil {
		if apierrors.IsConflict(err) {
			return settleAfterLostRace(ctx, c, authority, key, names, now)
		}

		return leafValid, errors.Wrapf(err, "updating config API leaf secret %s", key)
	}

	return outcome, nil
}

func createSharedLeaf(
	ctx context.Context, c client.Client, authority *configtls.Authority,
	key types.NamespacedName, names []string, now time.Time,
) (leafOutcome, error) {
	certPEM, keyPEM, err := authority.Issue(names, now)
	if err != nil {
		return leafValid, errors.Wrap(err, "issuing config API leaf")
	}

	if err := c.Create(ctx, tlsSecret(key, certPEM, keyPEM)); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return settleAfterLostRace(ctx, c, authority, key, names, now)
		}

		return leafValid, errors.Wrapf(err, "creating config API leaf secret %s", key)
	}

	return leafIssued, nil
}

// errLeafRaceUnsettled is returned when the write that beat ours left a leaf
// that still does not verify; the next pass retries.
var errLeafRaceUnsettled = errors.New("config API leaf changed concurrently and still does not verify")

func settleAfterLostRace(
	ctx context.Context, c client.Client, authority *configtls.Authority,
	key types.NamespacedName, names []string, now time.Time,
) (leafOutcome, error) {
	var winner corev1.Secret
	if err := c.Get(ctx, key, &winner); err != nil {
		return leafValid, errors.Wrapf(err, "re-reading config API leaf secret %s", key)
	}

	if leafOutcomeFor(authority, &winner, names, now) != leafValid {
		return leafValid, errors.Wrapf(errLeafRaceUnsettled, "%s", key)
	}

	return leafValid, nil
}

func leafOutcomeFor(authority *configtls.Authority, secret *corev1.Secret, names []string, now time.Time) leafOutcome {
	err := authority.Check(secret.Data[corev1.TLSCertKey], secret.Data[corev1.TLSPrivateKeyKey], names, now)

	switch {
	case err == nil:
		return leafValid
	case errors.Is(err, configtls.ErrRenewalDue):
		return leafRenewed
	default:
		return leafReplacedInvalid
	}
}

// Shared-leaf maintenance cadence: a routine check well inside the renewal
// window, and a short retry after a failed pass.
const (
	sharedLeafCheckInterval = time.Hour
	sharedLeafRetryInterval = 30 * time.Second
)

// sharedLeafIssuer keeps the shared plane's leaf current. It runs on the
// elected leader only; the proxy pods wait on the Secret mount until the
// first pass creates it.
type sharedLeafIssuer struct {
	client    client.Client
	authority *configtls.Authority
	caKey     types.NamespacedName
	key       types.NamespacedName
	names     []string
	logger    *slog.Logger
	// recorder reports an expiring CA on its Secret. Nil is a no-op.
	recorder events.EventRecorder
}

// NeedLeaderElection keeps issuance on one replica.
func (i *sharedLeafIssuer) NeedLeaderElection() bool { return true }

// Start runs issuance passes until ctx ends.
func (i *sharedLeafIssuer) Start(ctx context.Context) error {
	for {
		wait := sharedLeafCheckInterval

		i.warnIfCAExpiring()

		outcome, err := ensureSharedLeaf(ctx, i.client, i.authority, i.key, i.names, time.Now())

		switch {
		case err != nil:
			i.logger.Error("config API leaf for the shared data plane could not be issued; "+
				"the proxy cannot start or renew its config API certificate until this succeeds",
				"secret", i.key.String(), "error", err)

			wait = sharedLeafRetryInterval
		case outcome == leafReplacedInvalid:
			i.logger.Warn("replaced a shared data plane config API certificate that did not verify against the controller CA",
				"secret", i.key.String())
		case outcome != leafValid:
			i.logger.Info("issued the shared data plane config API certificate", "secret", i.key.String())
		}

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}
	}
}

// warnIfCAExpiring logs, and records a Warning Event on the CA Secret, while
// the CA is within one leaf lifetime of its expiry.
func (i *sharedLeafIssuer) warnIfCAExpiring() {
	warning := caExpiryWarning(i.authority, time.Now())
	if warning == "" {
		return
	}

	i.logger.Warn(warning, "secret", i.caKey.String())

	if i.recorder != nil {
		caSecret := &corev1.Secret{Name: i.caKey.Name, Namespace: i.caKey.Namespace}
		i.recorder.Eventf(caSecret, nil, corev1.EventTypeWarning, eventReasonConfigTLSCAExpiring,
			eventActionConfigTLS, "%s", warning)
	}
}

// Event vocabulary for the config API CA.
const (
	eventReasonConfigTLSCAExpiring = "ConfigTLSCAExpiring"
	eventActionConfigTLS           = "IssueCertificate"
)

// describeConfigTLSError explains a push failure caused by the config API
// handshake, or returns "" for any other failure.
func describeConfigTLSError(err error) string {
	var (
		unknownAuthority x509.UnknownAuthorityError
		hostname         x509.HostnameError
		invalid          x509.CertificateInvalidError
		recordHeader     tls.RecordHeaderError
	)

	switch {
	case stderrors.Is(err, ErrPlaintextConfigEndpoint):
		return "the config endpoint is a plaintext http:// URL while config API TLS is enabled"
	case stderrors.As(err, &unknownAuthority):
		return "the data plane presented a config API certificate not issued by this controller's CA"
	case stderrors.As(err, &hostname):
		return "the data plane presented a config API certificate that names a different data plane"
	case stderrors.As(err, &invalid):
		return "the data plane presented a config API certificate that is not valid (" + invalid.Error() + ")"
	case stderrors.Is(err, http.ErrSchemeMismatch), stderrors.As(err, &recordHeader):
		return "the data plane's config API does not speak TLS (a proxy still running without config API TLS)"
	}

	return ""
}

// checkConfigEndpointScheme refuses an endpoint that would carry the config
// in the clear while TLS is on.
func checkConfigEndpointScheme(endpoint string) error {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return errors.Wrapf(err, "parsing config endpoint %q", endpoint)
	}

	if parsed.Scheme != "https" {
		return errors.Wrapf(ErrPlaintextConfigEndpoint, "%s", endpoint)
	}

	return nil
}

// WithConfigAPIAuthority switches the config push to TLS pinned to authority:
// only a plane serving a leaf from this CA that names the endpoint's host
// receives the config.
func WithConfigAPIAuthority(authority *configtls.Authority) ProxySyncerOption {
	return func(s *proxySyncerSettings) {
		s.configAuthority = authority
	}
}

// push delivers cfg to every resolved endpoint. Under TLS each endpoint is
// verified against its own configured host, and an http:// endpoint is
// refused without a connection.
func (s *ProxySyncer) push(ctx context.Context, cfg *proxy.Config, resolved []pushEndpoint, authToken string) []proxy.PushResult {
	if s.configAuthority == nil {
		return s.pusher.PushWithToken(ctx, cfg, endpointURLs(resolved), authToken)
	}

	results := make([]proxy.PushResult, 0, len(resolved))
	byServerName := make(map[string][]string)

	var serverNames []string

	for _, endpoint := range resolved {
		if err := checkConfigEndpointScheme(endpoint.url); err != nil {
			results = append(results, proxy.PushResult{Endpoint: endpoint.url, Err: err})

			continue
		}

		if _, seen := byServerName[endpoint.serverName]; !seen {
			serverNames = append(serverNames, endpoint.serverName)
		}

		byServerName[endpoint.serverName] = append(byServerName[endpoint.serverName], endpoint.url)
	}

	for _, serverName := range serverNames {
		results = append(results, s.tlsPusher(serverName).PushWithToken(ctx, cfg, byServerName[serverName], authToken)...)
	}

	return results
}

// tlsPushTarget is one plane's pusher and the transport under it.
type tlsPushTarget struct {
	pusher    *proxy.ConfigPusher
	transport *http.Transport
}

func (s *ProxySyncer) tlsPusher(serverName string) *proxy.ConfigPusher {
	s.tlsPushersMu.Lock()
	defer s.tlsPushersMu.Unlock()

	target, ok := s.tlsPushers[serverName]
	if !ok {
		pushClient, transport := proxyPushClientWithTLS(s.tracing, s.configAuthority.ClientConfig(serverName))
		target = tlsPushTarget{pusher: proxy.NewConfigPusher(pushClient, s.defaultAuthToken), transport: transport}
		s.tlsPushers[serverName] = target
	}

	return target.pusher
}

// configTLSSetup is the resolved config API TLS configuration.
type configTLSSetup struct {
	authority *configtls.Authority
	caKey     types.NamespacedName
	leafKey   types.NamespacedName
	leafNames []string
}

var (
	errConfigTLSRefsIncomplete = errors.New(
		"--proxy-config-ca-secret and --proxy-config-tls-secret must be set together")
	errInvalidConfigTLSSecretRef = errors.New("config API TLS secret reference must be in `<namespace>/<name>` form")
)

// setupConfigTLS resolves config API TLS at startup. With neither reference
// set it returns nil and touches nothing: the plaintext push, and no CA. Any
// partial or inconsistent configuration is an error, because the controller
// would otherwise start and then fail every push.
func setupConfigTLS(ctx context.Context, c client.Client, cfg *Config, endpoints []string) (*configTLSSetup, error) {
	if cfg.ProxyConfigCASecretRef == "" && cfg.ProxyConfigTLSSecretRef == "" {
		return nil, nil //nolint:nilnil // nil setup is the plaintext push
	}

	if cfg.ProxyConfigCASecretRef == "" || cfg.ProxyConfigTLSSecretRef == "" {
		return nil, errConfigTLSRefsIncomplete
	}

	caKey, err := parseConfigTLSSecretRef(cfg.ProxyConfigCASecretRef)
	if err != nil {
		return nil, errors.Wrap(err, "--proxy-config-ca-secret")
	}

	leafKey, err := parseConfigTLSSecretRef(cfg.ProxyConfigTLSSecretRef)
	if err != nil {
		return nil, errors.Wrap(err, "--proxy-config-tls-secret")
	}

	leafNames := make([]string, 0, len(endpoints))

	for _, endpoint := range endpoints {
		if err := checkConfigEndpointScheme(endpoint); err != nil {
			return nil, errors.Wrap(err, "--proxy-endpoints")
		}

		parsed, err := url.Parse(endpoint)
		if err != nil {
			return nil, errors.Wrapf(err, "parsing --proxy-endpoints entry %q", endpoint)
		}

		if !slices.Contains(leafNames, parsed.Hostname()) {
			leafNames = append(leafNames, parsed.Hostname())
		}
	}

	authority, err := ensureConfigCA(ctx, c, caKey)
	if err != nil {
		return nil, err
	}

	err = authority.CheckValidAt(time.Now())
	if err != nil {
		return nil, errors.Wrapf(err, "config API CA secret %s: to rotate it, delete the Secret and restart every "+
			"controller replica (Config API TLS in the security reference)", caKey)
	}

	return &configTLSSetup{authority: authority, caKey: caKey, leafKey: leafKey, leafNames: leafNames}, nil
}

// caExpiryWarning returns a warning when the CA expires within one leaf
// lifetime: every leaf issued from then on is cut short at the CA's expiry,
// and after it no push verifies. Empty otherwise.
func caExpiryWarning(authority *configtls.Authority, now time.Time) string {
	if authority.NotAfter().Sub(now) >= configtls.LeafValidity {
		return ""
	}

	return "config API CA expires " + authority.NotAfter().UTC().Format(time.RFC3339) +
		"; after that no config push verifies. Rotate it: delete the CA Secret and restart every controller replica"
}

func parseConfigTLSSecretRef(raw string) (types.NamespacedName, error) {
	namespace, name, found := strings.Cut(strings.TrimSpace(raw), "/")
	if !found || namespace == "" || name == "" || strings.Contains(name, "/") {
		return types.NamespacedName{}, errors.Wrapf(errInvalidConfigTLSSecretRef, "got %q", raw)
	}

	return types.NamespacedName{Namespace: namespace, Name: name}, nil
}

// resolveConfigTLS runs setupConfigTLS on a direct client, for the reason
// resolveProxyAuthToken gives: the manager's cache is not started yet.
func resolveConfigTLS(ctx context.Context, mgr ctrl.Manager, cfg *Config, endpoints []string) (*configTLSSetup, error) {
	if cfg.ProxyConfigCASecretRef == "" && cfg.ProxyConfigTLSSecretRef == "" {
		return nil, nil //nolint:nilnil // nil setup is the plaintext push
	}

	directClient, err := client.New(mgr.GetConfig(), client.Options{Scheme: mgr.GetScheme()})
	if err != nil {
		return nil, errors.Wrap(err, "creating direct client for config API TLS")
	}

	setup, err := setupConfigTLS(ctx, directClient, cfg, endpoints)
	if err != nil {
		return nil, errors.Wrap(err, "config API TLS")
	}

	if warning := caExpiryWarning(setup.authority, time.Now()); warning != "" {
		slog.Default().Warn(warning, "secret", setup.caKey.String())
	}

	return setup, nil
}

// addSharedLeafIssuer registers the leader-only shared leaf issuer when
// config API TLS is on.
func addSharedLeafIssuer(mgr ctrl.Manager, setup *configTLSSetup, logger *slog.Logger) error {
	if setup == nil {
		return nil
	}

	issuer := &sharedLeafIssuer{
		client:    mgr.GetClient(),
		authority: setup.authority,
		caKey:     setup.caKey,
		key:       setup.leafKey,
		names:     setup.leafNames,
		logger:    logger.With("component", "config-tls-issuer"),
		recorder:  mgr.GetEventRecorder("config-tls-issuer"),
	}

	if err := mgr.Add(issuer); err != nil {
		return errors.Wrap(err, "adding the config API certificate issuer")
	}

	return nil
}

// perGatewayConfigEndpoint is the push URL of a per-Gateway plane: https when
// config API TLS is on, http otherwise.
func (s *ProxySyncer) perGatewayConfigEndpoint(gateway *gatewayv1.Gateway, clusterDomain string, port int32) string {
	if s.configAuthority != nil {
		return render.ConfigTLSEndpointURL(gateway, clusterDomain, port)
	}

	return render.ConfigEndpointURL(gateway, clusterDomain, port)
}

// retainTLSPushersLocked drops the pushers, and their connection pools, of
// server names no remaining partition pushes to. Caller holds syncMu.
func (s *ProxySyncer) retainTLSPushersLocked() {
	live := make(map[string]bool)

	for _, target := range s.targets {
		for _, endpoint := range target.endpointURLs {
			if parsed, err := url.Parse(endpoint); err == nil {
				live[parsed.Hostname()] = true
			}
		}
	}

	s.tlsPushersMu.Lock()
	defer s.tlsPushersMu.Unlock()

	for serverName, target := range s.tlsPushers {
		if !live[serverName] {
			target.transport.CloseIdleConnections()
			delete(s.tlsPushers, serverName)
		}
	}
}
