package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/cockroachdb/errors"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/ingress"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/logging"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/referencegrant"
	tracingpkg "github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/tracing"
)

// proxyPushTimeout bounds each controller -> proxy config-push request.
const proxyPushTimeout = 10 * time.Second

// serviceKind is the default Kind a Gateway API BackendObjectReference falls
// back to when Group/Kind are unset.
const serviceKind = "Service"

// coreGroup is the implicit Group for Kubernetes core resources (Service,
// ConfigMap). Gateway API treats "" and "core" as aliases.
const coreGroup = "core"

// configMapKind is the Kind value for ConfigMap references used by Gateway
// API BackendTLSPolicy CACertificateRefs.
const configMapKind = "ConfigMap"

// ProxySyncer converts HTTPRoute resources to proxy config
// and pushes it to enhanced-cloudflared replicas via HTTP API.
//
// lastCfg caches the most recent successfully-built config so the
// endpoint-watcher (see ProxyEndpointReconciler) can re-push to a
// newly-joined proxy pod without waiting for the next HTTPRoute
// reconcile. Before the first SyncRoutes call, lastCfg is nil and
// ResyncEndpoints is a no-op -- there is nothing to push yet.
// Guarded by syncMu so the cache update is consistent with the push
// it follows.
type ProxySyncer struct {
	clusterDomain        string
	logger               *slog.Logger
	pusher               *proxy.ConfigPusher
	k8sClient            client.Client
	backendValidator     proxy.BackendRefValidator
	grpcBackendValidator proxy.BackendRefValidator
	protocolResolver     proxy.BackendProtocolResolver
	tlsResolver          proxy.BackendTLSResolver
	gatewayCertResolver  proxy.GatewayClientCertResolver
	syncMu               sync.Mutex

	// defaultAuthToken is the shared data plane's push token (the chart's
	// proxy.authTokenSecretRef). Per-Gateway partitions carry their OWN
	// tokens — the shared token is never attached to tenant pushes.
	defaultAuthToken string

	// targets holds the per-partition push state, keyed by partition key
	// (sharedPartitionKey or the infra Gateway's "namespace/name"). Guarded
	// by syncMu. Each partition's steady-state skip, cached config, endpoint
	// URLs, and auth token are independent — that independence IS the
	// data-plane isolation at the push layer.
	targets map[string]*pushTarget

	// ViewStore caches the per-Gateway ListenerSet merge view across reconciles
	// (issue #332). Set by the manager after construction and shared with the
	// other reconcilers. nil disables cross-reconcile reuse.
	ViewStore *mergeViewStore
}

// pushTarget is one partition's push state: the cache that lets a resync
// replay config to a new pod, the hash/endpoint pair keying the steady-state
// skip, and the partition's own endpoints + auth token.
//
// The HTTPRoute and GRPCRoute reconcilers release RouteSyncer.syncMu before
// their push phases, so two can update one partition's endpointURLs/authToken
// concurrently (last-writer-wins; each critical section stays internally
// consistent, so -race is clean). This is benign: both derive the SAME values
// for a partition — its headless-Service DNS and its own auth Secret — so a
// concurrent overwrite writes identical bytes. Any genuine staleness (an
// endpoint set that changed between the two reconciles) self-heals on the next
// config or endpoint event, which re-pushes; the partition KEY never changes,
// so a stale write can never redirect a tenant's config to another plane.
type pushTarget struct {
	lastCfg             *proxy.Config
	lastPushedHash      string
	lastPushedToken     string
	lastPushedEndpoints map[string]struct{}
	endpointURLs        []string
	authToken           string
	// consecutivePushFail counts pushes that failed in a row for this
	// partition. It drives the route-status surfacing of a SUSTAINED push
	// failure (#487): a one-off blip (a pod rolling, a brief partition) must
	// not flip a route condition, so the failure is surfaced only past a
	// threshold. A successful push or a steady-state skip resets it to 0.
	consecutivePushFail int
}

// NewProxySyncer creates a ProxySyncer for pushing config to proxy replicas.
// The client is used to validate cross-namespace backend references via
// ReferenceGrant. controllerName scopes the Gateway client-cert resolver to
// Gateways whose GatewayClass.spec.controllerName matches ours — parentRefs
// pointing at a Gateway managed by another controller MUST NOT contribute
// their client cert to OUR proxy's mTLS handshake. controllerName may be
// empty (tests); the resolver then accepts any Gateway regardless of its
// GatewayClass.
func NewProxySyncer(
	clusterDomain string,
	authToken string,
	controllerName string,
	k8sClient client.Client,
	logger *slog.Logger,
	opts ...ProxySyncerOption,
) *ProxySyncer {
	if logger == nil {
		logger = slog.Default()
	}

	var settings proxySyncerSettings
	for _, opt := range opts {
		opt(&settings)
	}

	refGrantValidator := referencegrant.NewValidator(k8sClient)

	return &ProxySyncer{
		clusterDomain:        clusterDomain,
		logger:               logger.With("component", "proxy-syncer"),
		defaultAuthToken:     authToken,
		targets:              make(map[string]*pushTarget),
		pusher:               proxy.NewConfigPusher(proxyPushClient(settings.tracing), authToken),
		k8sClient:            k8sClient,
		backendValidator:     newBackendRefValidator(refGrantValidator, "HTTPRoute"),
		grpcBackendValidator: newBackendRefValidator(refGrantValidator, "GRPCRoute"),
		protocolResolver:     newBackendProtocolResolver(k8sClient),
		tlsResolver:          newBackendTLSResolver(k8sClient),
		gatewayCertResolver:  newGatewayClientCertResolver(k8sClient, controllerName),
	}
}

// proxySyncerSettings holds the options parsed at NewProxySyncer time.
type proxySyncerSettings struct {
	tracing bool
}

// ProxySyncerOption configures a ProxySyncer at construction.
type ProxySyncerOption func(*proxySyncerSettings)

// WithSyncerTracing instruments the controller -> proxy config-push HTTP client
// with OpenTelemetry so each push emits a client span linked to the active trace.
func WithSyncerTracing() ProxySyncerOption {
	return func(s *proxySyncerSettings) {
		s.tracing = true
	}
}

// proxyPushClient builds the config-push HTTP client. When tracing is enabled
// its transport is wrapped with otelhttp; either way the controller owns its
// transport rather than the process-global http.DefaultTransport.
//
// Leaving Transport nil would make net/http fall back to http.DefaultTransport,
// a singleton shared with every other HTTP client in the process. A
// CloseIdleConnections call on that shared transport — from unrelated code or,
// in tests, a sibling parallel case — can tear down an in-flight config push
// ("transport connection broken: http: CloseIdleConnections called"). Cloning
// DefaultTransport gives an isolated connection pool with the same defaults.
func proxyPushClient(tracing bool) *http.Client {
	// Always an isolated transport. Clone DefaultTransport for its tuned
	// defaults on the normal path; the bare *http.Transport fallback (an
	// unreachable case — DefaultTransport is always *http.Transport in the
	// stdlib) still avoids sharing the global pool.
	transport := &http.Transport{}
	if defaultTransport, ok := http.DefaultTransport.(*http.Transport); ok {
		transport = defaultTransport.Clone()
	}

	return &http.Client{
		Timeout:   proxyPushTimeout,
		Transport: tracingpkg.WrapTransport(transport, tracing),
	}
}

// newGatewayClientCertResolver returns a resolver that loads the Gateway's
// spec.tls.backend.clientCertificateRef Secret into the PEM-encoded keypair
// the proxy presents during backend mTLS handshakes. The resolver is scoped
// to Gateways managed by this controller — a parentRef pointing at another
// vendor's Gateway must NOT cause OUR proxy to present THEIR client cert
// (cross-controller credential leak guard). When controllerName is empty
// the GatewayClass check is skipped, matching test fixtures that don't model
// the full chain.
//
// Returns nil when the Gateway has no such ref, isn't managed by this
// controller, the Secret cannot be resolved, the ReferenceGrant is missing,
// or the keypair fails to parse. The first three reasons leave the Gateway's
// ResolvedRefs condition alone; the remaining ones drive it through the
// parallel emit path in the GatewayReconciler.
func newGatewayClientCertResolver(c client.Client, controllerName string) proxy.GatewayClientCertResolver {
	return func(ctx context.Context, gatewayNN types.NamespacedName) *proxy.ClientCertConfig {
		var gateway gatewayv1.Gateway
		if err := c.Get(ctx, gatewayNN, &gateway); err != nil {
			// NotFound is the expected outcome for routes pointing at a
			// foreign-namespace or deleted parent — silently skip. Any other
			// error is logged so a transient API-server hiccup that turns
			// mTLS into plaintext has a visible cause.
			if !apierrors.IsNotFound(err) {
				slog.Warn("gateway client cert resolver: Get(Gateway) failed — falling back to plaintext for this hop",
					"error", err,
					"namespace", gatewayNN.Namespace,
					"gateway", gatewayNN.Name,
				)
			}

			return nil
		}

		if !gatewayManagedByController(ctx, c, &gateway, controllerName) {
			return nil
		}

		certPEM, keyPEM, err := loadGatewayClientCertPEM(ctx, c, &gateway, gatewayClientCertGrantChecker(c))
		if err != nil {
			// The Gateway-status emit path already reports the same error
			// through ResolvedRefs; we also log on the syncer side so a
			// 502-from-handshake-fail incident has a visible cause without
			// scraping Gateway status. Transient API-server errors are
			// excluded — those are expected to recover on the next reconcile
			// without operator action.
			if !errors.Is(err, errGatewayClientCertTransientError) {
				slog.Warn("gateway client certificate resolution failed — backend mTLS handshake will be plaintext",
					"error", err,
					"namespace", gatewayNN.Namespace,
					"gateway", gatewayNN.Name,
				)
			}

			return nil
		}

		if certPEM == nil || keyPEM == nil {
			return nil
		}

		return &proxy.ClientCertConfig{CertPEM: certPEM, KeyPEM: keyPEM}
	}
}

// gatewayManagedByController reports whether the Gateway's GatewayClass.spec.
// controllerName matches ours. An empty controllerName disables the check —
// used by tests that don't construct a full GatewayClass chain. When the
// GatewayClass lookup itself fails (NotFound, transient error) we return
// false: better to NOT present a cert that may belong to another controller
// than to leak credentials on a doubtful match. The Gateway's status surface
// will already reflect "no managed parent" via existing reconcile paths.
func gatewayManagedByController(ctx context.Context, c client.Client, gateway *gatewayv1.Gateway, controllerName string) bool {
	if controllerName == "" {
		return true
	}

	var gatewayClass gatewayv1.GatewayClass
	if err := c.Get(ctx, types.NamespacedName{Name: string(gateway.Spec.GatewayClassName)}, &gatewayClass); err != nil {
		// NotFound is silent — a Gateway pointing at a missing GatewayClass
		// is correctly rejected from the "ours" set. Other errors (transient
		// API-server failure) get logged because they cause the same fail-
		// closed outcome but for an operational, not configuration, reason.
		if !apierrors.IsNotFound(err) {
			slog.Warn("gateway client cert resolver: Get(GatewayClass) failed — Gateway treated as foreign-controlled, no cert presented",
				"error", err,
				"gateway", gateway.Name,
				"gatewayClass", string(gateway.Spec.GatewayClassName),
			)
		}

		return false
	}

	return string(gatewayClass.Spec.ControllerName) == controllerName
}

// gatewayClientCertGrantChecker adapts the package-level grant lookup into a
// secretRefGrantChecker so the syncer can resolve cross-namespace refs without
// depending on a GatewayReconciler instance. The lookup mirrors the
// implementation on GatewayReconciler.checkSecretReferenceGrant verbatim.
func gatewayClientCertGrantChecker(c client.Client) secretRefGrantChecker {
	return func(
		ctx context.Context,
		gateway *gatewayv1.Gateway,
		targetNamespace string,
		ref gatewayv1.SecretObjectReference,
	) (bool, error) {
		return checkSecretReferenceGrantForGateway(ctx, c, gateway, targetNamespace, ref)
	}
}

// newBackendTLSResolver returns a resolver that looks up a BackendTLSPolicy
// targeting the given Service+Port and returns the corresponding TLS
// configuration (CA, hostname, SANs) for the proxy to apply on the backend
// hop. SectionName on the policy's TargetRef is honoured per Gateway API:
// when set, only the matching named port receives TLS — other ports of the
// same Service are unaffected.
//
// One asymmetry worth calling out: when the cache-backed `client.List` call
// itself errors (which only happens before the informer's initial sync or on
// API server unavailability), the resolver fails OPEN — returns nil and the
// proxy dials plaintext. Returning poisoned configs here would block every
// route in the namespace whenever the BackendTLSPolicy cache hiccups, which
// is a worse failure mode in practice. A WARN surfaces the event so the
// asymmetry is observable in logs.
//
// Behaviour by case:
//
//   - List error or no policy targets the service: returns nil → the proxy
//     dials the backend in plaintext (no policy applies, so there's no
//     operator intent to enforce).
//   - A policy targets the service BUT the CA cannot be resolved (missing
//     ConfigMap, unsupported Group/Kind, empty ca.crt, malformed PEM):
//     returns a *poisoned* config — an empty CA pool with the policy's
//     Hostname. This causes the proxy's TLS handshake to fail and the
//     request returns 502, NOT a silent plaintext downgrade.
//   - Hostname and URI SubjectAltNames are both honoured. URI SANs are
//     forwarded to the proxy as plain strings and matched via exact equality
//     against the leaf cert's URIs (SPIFFE convention used by the Gateway
//     API conformance suite). DNS Hostname SANs are matched via
//     x509.VerifyHostname (RFC 6125 wildcards).
//
// Precedence per Gateway API: oldest creationTimestamp wins; on tie,
// alphabetical {namespace}/{name}.
func newBackendTLSResolver(c client.Client) proxy.BackendTLSResolver {
	return func(ctx context.Context, namespace, serviceName string, port int32) *proxy.BackendTLSConfig {
		var policies gatewayv1.BackendTLSPolicyList
		if err := c.List(ctx, &policies, client.InNamespace(namespace)); err != nil {
			slog.Warn("failed to list BackendTLSPolicy — falling back to plaintext for this backend; "+
				"policy enforcement will resume once the cache recovers",
				"error", err,
				"namespace", namespace,
				"service", serviceName,
				"port", port,
			)

			return nil
		}

		policy := selectPolicyForServicePort(ctx, c, policies.Items, namespace, serviceName, port)
		if policy == nil {
			return nil
		}

		caBundle, ok := resolveCABundlePEM(ctx, c, policy)
		if !ok {
			return poisonedBackendTLS(policy)
		}

		dnsSANs, uriSANs, hasUnknown := splitSANsByType(policy)
		if hasUnknown {
			// CRD-newer-than-controller scenario: a SAN entry carries a Type
			// this controller doesn't recognise (Hostname/URI are the only
			// enum values today, but the spec may add more). Fail closed
			// rather than silently enforcing the subset we understand —
			// downgrading is worse than refusing the request.
			slog.Warn("BackendTLSPolicy carries a SubjectAltName of unsupported type — falling back to poisoned config to preserve operator intent",
				"namespace", policy.Namespace,
				"name", policy.Name,
			)

			return poisonedBackendTLS(policy)
		}

		return &proxy.BackendTLSConfig{
			CABundlePEM:        caBundle,
			ServerName:         string(policy.Spec.Validation.Hostname),
			SubjectAltNames:    dnsSANs,
			SubjectAltNameURIs: uriSANs,
		}
	}
}

// splitSANsByType separates the policy's SubjectAltNames into the DNS-hostname
// list and the URI list the proxy expects. The third return reports whether
// any entry carries a Type this controller doesn't recognise — the caller
// must then fail closed rather than ship the partial result.
func splitSANsByType(policy *gatewayv1.BackendTLSPolicy) ([]string, []string, bool) {
	sansLen := len(policy.Spec.Validation.SubjectAltNames)
	dnsSANs := make([]string, 0, sansLen)
	uriSANs := make([]string, 0, sansLen)

	hasUnknown := false

	for _, san := range policy.Spec.Validation.SubjectAltNames {
		switch san.Type {
		case gatewayv1.HostnameSubjectAltNameType:
			if san.Hostname != "" {
				dnsSANs = append(dnsSANs, string(san.Hostname))
			}
		case gatewayv1.URISubjectAltNameType:
			if san.URI != "" {
				uriSANs = append(uriSANs, string(san.URI))
			}
		default:
			hasUnknown = true
		}
	}

	return dnsSANs, uriSANs, hasUnknown
}

// poisonedBackendTLS returns a TLS config that is guaranteed to fail handshake
// (empty CA pool, no SAN list), used to short-circuit a request when a
// BackendTLSPolicy targets the Service but cannot be enforced — strictly
// preferred over silently downgrading to plaintext.
func poisonedBackendTLS(policy *gatewayv1.BackendTLSPolicy) *proxy.BackendTLSConfig {
	return &proxy.BackendTLSConfig{
		CABundlePEM: "",
		ServerName:  string(policy.Spec.Validation.Hostname),
	}
}

// selectPolicyForServicePort picks the precedence-winner among policies that
// target a Service+Port: TargetRefs without SectionName apply to every port;
// TargetRefs with SectionName apply only when the named port matches `port`.
// Older creationTimestamp wins; ties break alphabetically by name.
//
// `c`, `ctx`, and `namespace` enable the optional Service-port-name lookup —
// when a policy carries a SectionName, the function maps the port number back
// to the Service's port name and checks for a match.
func selectPolicyForServicePort(
	ctx context.Context,
	c client.Client,
	policies []gatewayv1.BackendTLSPolicy,
	namespace, serviceName string,
	port int32,
) *gatewayv1.BackendTLSPolicy {
	var portName string

	// Defer the Service lookup until at least one policy carries a SectionName —
	// most policies don't, so we avoid an extra Get for the common case.
	portNameResolved := false
	resolvePortName := func() string {
		if portNameResolved {
			return portName
		}

		portNameResolved = true
		portName = lookupServicePortName(ctx, c, namespace, serviceName, port)

		return portName
	}

	var best *gatewayv1.BackendTLSPolicy

	for policyIdx := range policies {
		policy := &policies[policyIdx]
		if !policyTargetsServicePort(policy, serviceName, resolvePortName) {
			continue
		}

		if best == nil || isPolicyOlder(policy, best) {
			best = policy
		}
	}

	return best
}

// policyTargetsServicePort reports whether any TargetRef in the policy points
// at a Service with the given name AND, when SectionName is set, only when the
// named port matches the resolved port name. resolvePortName is lazy — called
// only when at least one TargetRef carries a SectionName.
func policyTargetsServicePort(
	policy *gatewayv1.BackendTLSPolicy,
	serviceName string,
	resolvePortName func() string,
) bool {
	for _, target := range policy.Spec.TargetRefs {
		if string(target.Name) != serviceName {
			continue
		}

		kind := string(target.Kind)
		if kind != "" && kind != serviceKind {
			continue
		}

		if target.SectionName == nil || *target.SectionName == "" {
			return true
		}

		// SectionName set → only match when the actual backend port carries
		// the same name. If the Service has no port with that name (or the
		// Service can't be fetched), this policy does NOT apply to this port.
		if string(*target.SectionName) == resolvePortName() {
			return true
		}
	}

	return false
}

// lookupServicePortName returns the name of the Service port matching `port`,
// or "" if the Service or port can't be resolved. Errors are swallowed
// intentionally: a missing Service is treated as "no port name available",
// which leads SectionName-bearing policies to be rejected for this port.
func lookupServicePortName(ctx context.Context, c client.Client, namespace, serviceName string, port int32) string {
	var svc corev1.Service
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: serviceName}, &svc); err != nil {
		return ""
	}

	for portIdx := range svc.Spec.Ports {
		if svc.Spec.Ports[portIdx].Port == port {
			return svc.Spec.Ports[portIdx].Name
		}
	}

	return ""
}

// isPolicyOlder reports whether candidate was created before incumbent, or
// with the same timestamp and a smaller name (Gateway API precedence rule).
func isPolicyOlder(candidate, incumbent *gatewayv1.BackendTLSPolicy) bool {
	if candidate.CreationTimestamp.Before(&incumbent.CreationTimestamp) {
		return true
	}

	if incumbent.CreationTimestamp.Before(&candidate.CreationTimestamp) {
		return false
	}

	return candidate.Name < incumbent.Name
}

// resolveCABundlePEM returns the concatenated PEM-encoded CA certificate bundle
// referenced by the BackendTLSPolicy, or ok=false if any reference fails to
// resolve. Only same-namespace ConfigMap refs with a "ca.crt" key are
// supported (Core in Gateway API v1). Returns ok=false on any of: unsupported
// Group/Kind, missing ConfigMap, empty `ca.crt` key, or a `ca.crt` value that
// does not contain at least one parseable PEM CERTIFICATE block — the proxy
// then short-circuits the backend so traffic fails closed rather than
// downgrading silently to plaintext when the policy can't be enforced.
func resolveCABundlePEM(ctx context.Context, c client.Client, policy *gatewayv1.BackendTLSPolicy) (string, bool) {
	if len(policy.Spec.Validation.CACertificateRefs) == 0 {
		return "", false
	}

	var bundle strings.Builder

	for _, ref := range policy.Spec.Validation.CACertificateRefs {
		group := string(ref.Group)
		if group != "" && group != coreGroup {
			return "", false
		}

		if string(ref.Kind) != configMapKind {
			return "", false
		}

		var caCM corev1.ConfigMap

		key := client.ObjectKey{Namespace: policy.Namespace, Name: string(ref.Name)}
		if err := c.Get(ctx, key, &caCM); err != nil {
			return "", false
		}

		caPEM, hasKey := caCM.Data[configMapCAKey]
		if !hasKey || caPEM == "" {
			return "", false
		}

		// Mirror the reconciler's validateCARefs check — a ConfigMap with
		// garbage content under ca.crt would otherwise be passed through to
		// the proxy and silently fail every TLS handshake at runtime.
		if _, err := parseCABundle(caPEM); err != nil {
			return "", false
		}

		bundle.WriteString(caPEM)

		if !strings.HasSuffix(caPEM, "\n") {
			bundle.WriteByte('\n')
		}
	}

	return bundle.String(), true
}

// newBackendProtocolResolver returns a resolver that reads a backend Service's
// port appProtocol (e.g. kubernetes.io/h2c) so the proxy can pick the right
// backend transport. Unknown services or ports resolve to "".
func newBackendProtocolResolver(c client.Client) proxy.BackendProtocolResolver {
	return func(ctx context.Context, namespace, serviceName string, port int32) string {
		var svc corev1.Service

		key := client.ObjectKey{Namespace: namespace, Name: serviceName}
		if err := c.Get(ctx, key, &svc); err != nil {
			return ""
		}

		return portAppProtocol(&svc, port)
	}
}

// portAppProtocol returns the appProtocol of the Service port matching port, or "".
func portAppProtocol(svc *corev1.Service, port int32) string {
	for i := range svc.Spec.Ports {
		if svc.Spec.Ports[i].Port == port && svc.Spec.Ports[i].AppProtocol != nil {
			return *svc.Spec.Ports[i].AppProtocol
		}
	}

	return ""
}

// newBackendRefValidator creates a BackendRefValidator from a
// referencegrant.Validator. fromKind is the route kind the validator speaks for
// ("HTTPRoute" or "GRPCRoute"): a ReferenceGrant's from.kind must match the
// actual referencing route kind, so HTTP and gRPC conversion need separate
// validators — sharing one would deny a gRPC route guarded by a GRPCRoute grant
// (and wrongly allow one guarded by an HTTPRoute-only grant).
func newBackendRefValidator(validator *referencegrant.Validator, fromKind string) proxy.BackendRefValidator {
	return func(ctx context.Context, fromNamespace string, ref gatewayv1.BackendObjectReference) bool {
		fromRef := referencegrant.Reference{
			Group:     "gateway.networking.k8s.io",
			Kind:      fromKind,
			Namespace: fromNamespace,
		}

		toGroup := ""
		if ref.Group != nil {
			toGroup = string(*ref.Group)
		}

		toKind := serviceKind
		if ref.Kind != nil {
			toKind = string(*ref.Kind)
		}

		toNamespace := fromNamespace
		if ref.Namespace != nil {
			toNamespace = string(*ref.Namespace)
		}

		toRef := referencegrant.Reference{
			Group:     toGroup,
			Kind:      toKind,
			Namespace: toNamespace,
			Name:      string(ref.Name),
		}

		allowed, err := validator.IsReferenceAllowed(ctx, fromRef, toRef)
		if err != nil {
			slog.Warn("failed to validate cross-namespace reference",
				"error", err,
				"from_namespace", fromNamespace,
				"to_namespace", toNamespace,
				"service", string(ref.Name),
			)

			return false
		}

		return allowed
	}
}

// SyncRoutes converts pre-collected HTTPRoutes and GRPCRoutes to proxy config
// and pushes to all endpoints. Routes should come from the RouteSyncer's
// SyncResult to avoid redundant API calls. failedRefs / grpcFailedRefs contain
// the HTTP / gRPC backend refs that failed validation in the ingress builder.
// Both route families get their backends cleared for rules with failed refs so
// the proxy returns HTTP 500 (no backend) instead of dialing a nonexistent
// Service and surfacing a 502 — the converter alone does not detect a missing
// Service (it only drops kind/port/ReferenceGrant failures), so the builder's
// BackendNotFound findings must be applied here.
func (s *ProxySyncer) SyncRoutes(
	ctx context.Context,
	endpoints []string,
	routes []*gatewayv1.HTTPRoute,
	grpcRoutes []*gatewayv1.GRPCRoute,
	failedRefs []ingress.BackendRefError,
	grpcFailedRefs []ingress.BackendRefError,
) ([]proxy.RouteDiagnostic, error) {
	return s.SyncPartition(ctx, sharedPartitionKey, s.defaultAuthToken, endpoints, routes, grpcRoutes, failedRefs, grpcFailedRefs)
}

// SyncPartition is the per-data-plane push: it builds the proxy config from
// EXACTLY the partition's routes and pushes it to the partition's endpoints,
// authenticated with the partition's own token (empty = no auth header — the
// shared token is never reused for tenant planes). Push state (steady-state
// skip, replay cache) is independent per partition.
//
//nolint:funlen // sequential build → skip-check → push → cache pipeline
func (s *ProxySyncer) SyncPartition(
	ctx context.Context,
	key string,
	authToken string,
	endpoints []string,
	routes []*gatewayv1.HTTPRoute,
	grpcRoutes []*gatewayv1.GRPCRoute,
	failedRefs []ingress.BackendRefError,
	grpcFailedRefs []ingress.BackendRefError,
) ([]proxy.RouteDiagnostic, error) {
	// Resolve headless service DNS names before acquiring the lock
	// to avoid blocking concurrent reconciles during slow DNS lookups.
	resolved := resolveEndpoints(ctx, endpoints)

	s.syncMu.Lock()
	defer s.syncMu.Unlock()

	target := s.targetLocked(key)
	target.endpointURLs = endpoints
	target.authToken = authToken

	logger := logging.FromContext(ctx)
	if logger == slog.Default() {
		logger = s.logger
	}

	logger.Info("syncing proxy config",
		"partition", key, "httpRoutes", len(routes), "grpcRoutes", len(grpcRoutes))

	cfg := s.buildProxyConfig(ctx, routes, grpcRoutes, failedRefs, grpcFailedRefs)

	// Diagnostics are computed by the converter and are valid regardless of
	// whether the push below succeeds — they describe the route specs, not the
	// push. Capture them up front so a push failure still surfaces config the
	// controller will not serve as written on the route status.
	diagnostics := cfg.Diagnostics

	logger.Info("resolved endpoints",
		"partition", key,
		"original", len(endpoints),
		"resolved", len(resolved),
	)

	// Steady-state skip: when the rebuilt config is identical to the last
	// successful push and the replica set is unchanged, every endpoint
	// already holds this config — re-pushing it is pure churn. Reconciles
	// triggered by status updates or endpoint heartbeats hit this path
	// constantly in large fleets.
	cfgHash := hashProxyConfig(cfg)
	if shouldSkipPush(cfgHash, target.lastPushedHash, authToken, target.lastPushedToken, target.lastPushedEndpoints, resolved) {
		logger.Debug("proxy config unchanged; skipping push",
			"partition", key,
			"endpoints", len(resolved),
			"rules", len(cfg.Rules),
		)

		// A skip means every endpoint already holds this config, so the
		// partition is healthy — clear any prior failure streak (#487).
		target.consecutivePushFail = 0

		return diagnostics, nil
	}

	if err := s.pushToEndpoints(ctx, logger, cfg, resolved, authToken); err != nil {
		// A failed push may have PARTIALLY succeeded (the pusher fans out
		// concurrently): some replicas may already hold the new config.
		// Invalidate the skip key so the next sync re-pushes even when its
		// desired config equals the last fully-successful one -- otherwise a
		// rollback after a partial push would be skipped and the
		// half-updated replica would stay stale until an unrelated change.
		target.lastPushedHash = ""
		target.lastPushedEndpoints = nil
		target.consecutivePushFail++

		return diagnostics, err
	}

	// Cache the successfully-pushed config so a partition resync can replay
	// it to a newly-joined proxy pod that arrives between HTTPRoute
	// reconciles. We cache AFTER the push so a failed push does not poison
	// the cache with a config the replicas never actually received. The
	// hash/endpoint-set pair keys the steady-state skip above.
	target.lastCfg = cfg
	target.lastPushedHash = cfgHash
	target.lastPushedToken = authToken
	target.lastPushedEndpoints = make(map[string]struct{}, len(resolved))
	target.consecutivePushFail = 0

	for _, endpoint := range resolved {
		target.lastPushedEndpoints[endpoint] = struct{}{}
	}

	return diagnostics, nil
}

// pushFailureStreak returns the number of consecutive failed pushes for the
// partition keyed by key (0 if the partition is unknown or last pushed/skipped
// cleanly). It lets the route syncer surface a SUSTAINED push failure on route
// status without flapping on a single transient blip (#487).
func (s *ProxySyncer) pushFailureStreak(key string) int {
	s.syncMu.Lock()
	defer s.syncMu.Unlock()

	target, ok := s.targets[key]
	if !ok {
		return 0
	}

	return target.consecutivePushFail
}

// targetLocked returns (creating on demand) the partition's push state.
// Caller must hold syncMu.
func (s *ProxySyncer) targetLocked(key string) *pushTarget {
	if s.targets == nil {
		s.targets = make(map[string]*pushTarget)
	}

	target, ok := s.targets[key]
	if !ok {
		target = &pushTarget{}
		s.targets[key] = target
	}

	return target
}

// RetainPartitions evicts push state for partitions not in keep (deleted
// Gateways). The shared partition is always retained.
//
// Concurrency note: a slower concurrent reconcile carrying a stale keep set
// can transiently evict a partition another reconcile just pushed (push and
// retain are separate lock acquisitions). This self-heals on the next
// reconcile or endpoint event and is never a cross-tenant leak — a missing
// cache only forces a re-push, it never routes a tenant's config elsewhere.
func (s *ProxySyncer) RetainPartitions(keep map[string]bool) {
	s.syncMu.Lock()
	defer s.syncMu.Unlock()

	for key := range s.targets {
		if key == sharedPartitionKey || keep[key] {
			continue
		}

		delete(s.targets, key)
	}
}

// ResyncPartition replays a partition's cached config to its stored endpoint
// URLs (re-resolving DNS), using the partition's own auth token. Unknown or
// not-yet-synced partitions are a no-op.
func (s *ProxySyncer) ResyncPartition(ctx context.Context, key string) error {
	s.syncMu.Lock()
	target, ok := s.targets[key]

	var (
		endpoints []string
		authToken string
	)

	if ok {
		endpoints = target.endpointURLs
		authToken = target.authToken
	}
	s.syncMu.Unlock()

	if !ok {
		return nil
	}

	return s.resyncTarget(ctx, key, endpoints, authToken)
}

// ResyncAllPartitions replays every cached partition config; used when an
// endpoint event cannot be attributed to one partition.
func (s *ProxySyncer) ResyncAllPartitions(ctx context.Context) error {
	s.syncMu.Lock()
	keys := make([]string, 0, len(s.targets))

	for key := range s.targets {
		keys = append(keys, key)
	}
	s.syncMu.Unlock()

	var errs []error

	for _, key := range keys {
		if err := s.ResyncPartition(ctx, key); err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

// pushToEndpoints delivers cfg to every resolved endpoint, aggregating
// per-endpoint failures into one error. Extracted from SyncRoutes to keep it
// within the funlen budget.
func (s *ProxySyncer) pushToEndpoints(ctx context.Context, logger *slog.Logger, cfg *proxy.Config, resolved []string, authToken string) error {
	results := s.pusher.PushWithToken(ctx, cfg, resolved, authToken)

	var pushErrors []error

	for _, result := range results {
		if result.Err != nil {
			logger.Error("failed to push config to endpoint",
				"endpoint", result.Endpoint,
				"error", result.Err,
			)

			pushErrors = append(pushErrors, result.Err)
		}
	}

	if len(pushErrors) > 0 {
		return fmt.Errorf("failed to push config to %d/%d endpoints: %w",
			len(pushErrors), len(resolved), errors.Join(pushErrors...))
	}

	logger.Info("successfully pushed proxy config",
		"endpoints", len(resolved),
		"rules", len(cfg.Rules),
		"version", cfg.Version,
	)

	return nil
}

// shouldSkipPush reports whether the rebuilt config is already held by every
// resolved endpoint: the content hash matches the last fully-successful push,
// the auth token is unchanged, and the replica set is unchanged. An empty hash
// never skips -- "" is both the hash-failure sentinel and the zero value of the
// last-pushed key (and the invalidation value after a partial push), so
// treating it as a match would skip a push that must happen. The token is part
// of the key so a token rotation on otherwise-unchanged routes/endpoints still
// re-authenticates to every replica instead of waiting for the next genuine
// config change.
func shouldSkipPush(
	cfgHash, lastPushedHash, authToken, lastPushedToken string,
	lastPushedEndpoints map[string]struct{},
	resolved []string,
) bool {
	return cfgHash != "" && cfgHash == lastPushedHash &&
		authToken == lastPushedToken &&
		endpointSetsEqual(lastPushedEndpoints, resolved)
}

// hashProxyConfig returns a content hash of the config with the Version
// counter zeroed: the counter increments on every build, so two
// semantically-identical configs would otherwise never compare equal.
func hashProxyConfig(cfg *proxy.Config) string {
	versionless := *cfg
	versionless.Version = 0

	payload, err := json.Marshal(&versionless)
	if err != nil {
		// Marshal of the wire-format config cannot realistically fail; an
		// empty hash disables the skip for this sync AND for the comparison
		// against it (shouldSkipPush never matches on ""), so the push
		// always proceeds.
		return ""
	}

	sum := sha256.Sum256(payload)

	return hex.EncodeToString(sum[:])
}

// endpointSetsEqual reports whether the previously-pushed endpoint set covers
// exactly the newly-resolved endpoints.
func endpointSetsEqual(previous map[string]struct{}, resolved []string) bool {
	if len(previous) != len(resolved) {
		return false
	}

	for _, endpoint := range resolved {
		if _, ok := previous[endpoint]; !ok {
			return false
		}
	}

	return true
}

// buildProxyConfig converts the HTTP and gRPC route sets into a single merged
// proxy Config. HTTP routes inherit parent-listener hostnames and get invalid
// backend refs marked unavailable (→ 500 for that backend's fraction); gRPC
// routes are appended with backends forced to h2c and the same marking applied.
// Extracted from SyncRoutes to keep that function under the funlen budget.
func (s *ProxySyncer) buildProxyConfig(
	ctx context.Context,
	routes []*gatewayv1.HTTPRoute,
	grpcRoutes []*gatewayv1.GRPCRoute,
	failedRefs []ingress.BackendRefError,
	grpcFailedRefs []ingress.BackendRefError,
) *proxy.Config {
	// One merge-view cache for this whole proxy-config build: the hostname and
	// redirect-scheme passes both resolve the same Gateways, so they share a
	// single merge instead of rebuilding it per route per pass (issue #332).
	views := newListenerViewCache(s.k8sClient, s.ViewStore)

	// When a route binds to a Gateway listener or ListenerSet entry with a
	// non-empty hostname and itself declares spec.hostnames empty, the proxy
	// rule MUST serve only the parent listener's hostname. Augment in-memory
	// before handing to the converter; the input routes are left untouched.
	routes = withEffectiveHostnames(ctx, s.k8sClient, routes, views)

	// A RequestRedirect filter that leaves scheme empty must default to the
	// scheme of the request, which behind the tunnel means the parent
	// listener's protocol (cloudflared terminates TLS at the edge, so the
	// origin request carries no usable scheme). Resolve it here so the
	// converter sees an explicit scheme instead of the proxy's hardcoded
	// https fallback. Input routes are left untouched.
	routes = withDefaultRedirectScheme(ctx, s.k8sClient, routes, views)

	// Convert to proxy config with cross-namespace validation, backend
	// protocol resolution (e.g. h2c from Service appProtocol), and
	// BackendTLSPolicy lookup for the proxy → backend TLS hop.
	cfg := proxy.ConvertHTTPRoutes(ctx, routes, s.clusterDomain, s.backendValidator, s.protocolResolver, s.tlsResolver, s.gatewayCertResolver)

	// Mark each invalid backendRef (a nonexistent Service) so the proxy returns
	// 500 for that backend's traffic fraction instead of dialing a dead address
	// and surfacing a 502. The backend stays in the weighted pool so the
	// fraction is preserved per the Gateway API spec.
	markUnavailableBackends(cfg, s.clusterDomain, failedRefs)

	// Append GRPCRoute rules. gRPC method matching maps onto the same proxy
	// path matcher; backends are dialed h2c unless a BackendTLSPolicy puts TLS on
	// the wire, and a TLS appProtocol with no policy fails the backend closed
	// (the protocolResolver feeds that decision). The merged config keeps the
	// HTTP config's version (grpcCfg burns a version counter value that is
	// discarded — only the pushed config's version is observed downstream).
	if len(grpcRoutes) > 0 {
		// gRPC routes inherit their parent listener's hostname when they declare
		// none, exactly like HTTPRoutes — otherwise an empty-hostname gRPC rule
		// becomes a catch-all answering every Host (including hostnames owned by
		// other routes).
		grpcRoutes = withEffectiveHostnamesGRPC(ctx, s.k8sClient, grpcRoutes, views)

		grpcCfg := proxy.ConvertGRPCRoutes(ctx, grpcRoutes, s.clusterDomain, s.grpcBackendValidator, s.protocolResolver, s.tlsResolver, s.gatewayCertResolver)
		cfg.Rules = append(cfg.Rules, grpcCfg.Rules...)
		// Provenance MUST grow in lockstep with Rules (parallel slices) so the
		// shadow detection below attributes every flattened rule correctly.
		cfg.Provenance = append(cfg.Provenance, grpcCfg.Provenance...)
		cfg.Diagnostics = append(cfg.Diagnostics, grpcCfg.Diagnostics...)

		// Mark invalid gRPC backendRefs the same way as HTTP. Matching is by
		// service host:port across all rules, so no rule-offset bookkeeping is
		// needed.
		markUnavailableBackends(cfg, s.clusterDomain, grpcFailedRefs)
	}

	// Expand each headless Service (clusterIP: None) into one backend per ready
	// endpoint, dialing the endpoint targetPort. A headless Service has no VIP, so
	// the FQDN resolves to the pod IPs and dialing the Service port would reach a
	// pod listening on the targetPort and 502. Runs after the 500 markings (so a
	// dropped/invalid ref is never expanded) and before the 503 marking (so a
	// headless Service with ready endpoints loses its FQDN host before the 503 pass
	// looks, while one with no ready endpoints keeps it and gets marked 503).
	expandHeadlessBackends(ctx, s.k8sClient, cfg, s.clusterDomain, routes, grpcRoutes)

	// After the 500 (invalid-ref) markings, mark any backend whose Service
	// exists but has no ready endpoints with 503 (Gateway API SHOULD). Runs
	// last so the first-marking-wins rule keeps 500 for a backend that is both
	// nonexistent and endpoint-less.
	markZeroEndpointBackends(ctx, s.k8sClient, cfg, s.clusterDomain, routes, grpcRoutes)

	// Rewrite ExternalBackend sentinel URLs to the real scheme://host:port/path
	// from each ExternalBackend's spec (the converter has no client and emits a
	// sentinel). A missing ExternalBackend is marked 500 for its fraction.
	resolveExternalBackends(ctx, s.k8sClient, cfg)

	// Mark whether any GRPCRoute contributed to this config so the proxy can
	// upgrade an "auto"/unset edge transport to http2 at startup (gRPC needs
	// http2; cloudflared drops trailers over QUIC). gRPC rules look identical
	// to h2c HTTP rules on the wire, so the signal must be explicit.
	cfg.HasGRPCRoute = len(grpcRoutes) > 0

	// Cross-route shadow detection (#474) runs LAST, over the exact rule
	// stream the router will serve — after effective-hostname inheritance and
	// the gRPC merge — so a collision an operator can hit is a collision this
	// flags. Status/observability only: the diagnostics ride the existing
	// pipeline into a dedicated condition + Warning Event on the losing route.
	cfg.Diagnostics = append(cfg.Diagnostics, proxy.DetectShadowedRules(cfg)...)

	return cfg
}

// ResyncEndpoints replays the most recent successfully-pushed config to the
// supplied endpoints without rebuilding from HTTPRoutes. The endpoint
// watcher uses this to bring a newly-joined proxy pod up to date when the
// HTTPRoute set has not changed; without it the new pod stays at
// /readyz == 503 until the next HTTPRoute reconcile, which is the race
// the workaround "kubectl rollout restart deployment <controller>" was
// papering over.
//
// Before the first SyncRoutes call (or after a controller restart that
// has not yet built any config) lastCfg is nil and this is a no-op --
// nothing meaningful to push yet, and the next HTTPRoute reconcile will
// reach the new endpoint along with the others.
//
// Push errors are returned but not fatal: a transient endpoint flake
// gets corrected on the next endpoint-change event.
func (s *ProxySyncer) ResyncEndpoints(ctx context.Context, endpoints []string) error {
	return s.resyncTarget(ctx, sharedPartitionKey, endpoints, s.defaultAuthToken)
}

// resyncTarget replays a partition's cached config to the given endpoints
// with the given token, updating the partition's steady-state skip key on
// success and invalidating it on partial failure.
func (s *ProxySyncer) resyncTarget(ctx context.Context, key string, endpoints []string, authToken string) error {
	// Resolve headless service DNS names before acquiring the lock so a
	// slow DNS lookup does not block a concurrent sync -- mirrors the same
	// pattern in SyncPartition for symmetric lock-hold time.
	resolved := resolveEndpoints(ctx, endpoints)

	s.syncMu.Lock()
	defer s.syncMu.Unlock()

	// Plain lookup, NOT targetLocked: RetainPartitions can evict the key
	// between the caller's read-unlock and this re-lock, and re-creating an
	// empty target here would resurrect a garbage entry that lingers until
	// the next retain pass.
	target, ok := s.targets[key]
	if !ok || target.lastCfg == nil {
		return nil
	}

	logger := logging.FromContext(ctx)
	if logger == slog.Default() {
		logger = s.logger
	}

	logger.Info("resyncing cached proxy config to endpoints",
		"partition", key,
		"endpoints", len(resolved),
		"version", target.lastCfg.Version,
	)

	results := s.pusher.PushWithToken(ctx, target.lastCfg, resolved, authToken)

	var pushErrors []error

	for _, result := range results {
		if result.Err != nil {
			logger.Error("failed to resync config to endpoint",
				"endpoint", result.Endpoint,
				"error", result.Err,
			)

			pushErrors = append(pushErrors, result.Err)
		}
	}

	if len(pushErrors) > 0 {
		// Mirror SyncPartition: a partial resync means some replicas may hold
		// the cached config while others do not -- drop the skip key so the
		// next sync re-pushes unconditionally.
		target.lastPushedHash = ""
		target.lastPushedEndpoints = nil

		return fmt.Errorf("failed to resync config to %d/%d endpoints: %w",
			len(pushErrors), len(resolved), errors.Join(pushErrors...))
	}

	// Every resolved endpoint now holds the cached config: update the skip
	// key so the next sync does not re-push the identical config just
	// because the replica set grew.
	target.lastPushedHash = hashProxyConfig(target.lastCfg)
	target.lastPushedToken = authToken
	target.lastPushedEndpoints = make(map[string]struct{}, len(resolved))

	for _, endpoint := range resolved {
		target.lastPushedEndpoints[endpoint] = struct{}{}
	}

	return nil
}

// dnsLookupTimeout is the maximum time to wait for a single DNS resolution.
const dnsLookupTimeout = 5 * time.Second

// resolveEndpoints expands headless service DNS names to individual pod IPs.
// For each endpoint URL, it resolves the hostname via DNS. If the hostname
// resolves to multiple IPs (headless service), it creates a separate endpoint
// URL for each IP, preserving the original scheme, port, and path.
// If resolution fails or returns no results, the original endpoint is kept.
func resolveEndpoints(ctx context.Context, endpoints []string) []string {
	var resolved []string

	for _, endpoint := range endpoints {
		parsed, err := url.Parse(endpoint)
		if err != nil {
			resolved = append(resolved, endpoint)

			continue
		}

		hostname := parsed.Hostname()
		port := parsed.Port()

		lookupCtx, cancel := context.WithTimeout(ctx, dnsLookupTimeout)

		addrs, lookupErr := net.DefaultResolver.LookupHost(lookupCtx, hostname)

		cancel()

		if lookupErr != nil || len(addrs) == 0 {
			resolved = append(resolved, endpoint)

			continue
		}

		for _, addr := range addrs {
			epURL := &url.URL{
				Scheme: parsed.Scheme,
				Path:   parsed.Path,
			}

			if port != "" {
				epURL.Host = net.JoinHostPort(addr, port)
			} else {
				epURL.Host = addr
			}

			resolved = append(resolved, epURL.String())
		}
	}

	return resolved
}
