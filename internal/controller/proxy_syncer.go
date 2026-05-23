package controller

import (
	"context"
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
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/ingress"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/logging"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/referencegrant"
)

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
type ProxySyncer struct {
	clusterDomain    string
	logger           *slog.Logger
	pusher           *proxy.ConfigPusher
	backendValidator proxy.BackendRefValidator
	protocolResolver proxy.BackendProtocolResolver
	tlsResolver      proxy.BackendTLSResolver
	syncMu           sync.Mutex
}

// NewProxySyncer creates a ProxySyncer for pushing config to proxy replicas.
// The client is used to validate cross-namespace backend references via ReferenceGrant.
func NewProxySyncer(
	clusterDomain string,
	authToken string,
	k8sClient client.Client,
	logger *slog.Logger,
) *ProxySyncer {
	if logger == nil {
		logger = slog.Default()
	}

	refGrantValidator := referencegrant.NewValidator(k8sClient)

	return &ProxySyncer{
		clusterDomain: clusterDomain,
		logger:        logger.With("component", "proxy-syncer"),
		pusher: proxy.NewConfigPusher(&http.Client{
			Timeout: 10 * time.Second,
		}, authToken),
		backendValidator: newBackendRefValidator(refGrantValidator),
		protocolResolver: newBackendProtocolResolver(k8sClient),
		tlsResolver:      newBackendTLSResolver(k8sClient),
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
//     ConfigMap, unsupported Group/Kind, empty ca.crt, malformed PEM) OR the
//     policy carries a non-Hostname SubjectAltName type that this controller
//     does not support: returns a *poisoned* config — an empty CA pool with
//     the policy's Hostname. This causes the proxy's TLS handshake to fail
//     and the request returns 502, NOT a silent plaintext downgrade. The
//     operator's stated intent ("traffic to this Service MUST be
//     authenticated TLS") is preserved.
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

		// Fail closed for any unsupported SAN type — the operator asked for
		// stricter identity than this controller can enforce.
		if !hasOnlyHostnameSANs(policy) {
			return poisonedBackendTLS(policy)
		}

		caBundle, ok := resolveCABundlePEM(ctx, c, policy)
		if !ok {
			return poisonedBackendTLS(policy)
		}

		sans := make([]string, 0, len(policy.Spec.Validation.SubjectAltNames))
		for _, san := range policy.Spec.Validation.SubjectAltNames {
			if san.Hostname != "" {
				sans = append(sans, string(san.Hostname))
			}
		}

		return &proxy.BackendTLSConfig{
			CABundlePEM:     caBundle,
			ServerName:      string(policy.Spec.Validation.Hostname),
			SubjectAltNames: sans,
		}
	}
}

// hasOnlyHostnameSANs reports whether every SubjectAltName entry on the
// policy is of the supported Hostname type. URI-type SANs (SPIFFE etc.) are
// not yet implemented end-to-end and must not silently degrade enforcement.
func hasOnlyHostnameSANs(policy *gatewayv1.BackendTLSPolicy) bool {
	for _, san := range policy.Spec.Validation.SubjectAltNames {
		if san.Type != gatewayv1.HostnameSubjectAltNameType {
			return false
		}
	}

	return true
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

// newBackendRefValidator creates a BackendRefValidator from a referencegrant.Validator.
func newBackendRefValidator(validator *referencegrant.Validator) proxy.BackendRefValidator {
	return func(ctx context.Context, fromNamespace string, ref gatewayv1.BackendObjectReference) bool {
		fromRef := referencegrant.Reference{
			Group:     "gateway.networking.k8s.io",
			Kind:      "HTTPRoute",
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

// SyncRoutes converts pre-collected HTTPRoutes to proxy config and pushes to all endpoints.
// Routes should come from the RouteSyncer's SyncResult to avoid redundant API calls.
// failedRefs contains backend refs that failed validation in the ingress builder — routes
// with failed refs will have their backends cleared so the proxy returns HTTP 500.
func (s *ProxySyncer) SyncRoutes(
	ctx context.Context,
	endpoints []string,
	routes []*gatewayv1.HTTPRoute,
	failedRefs []ingress.BackendRefError,
) error {
	// Resolve headless service DNS names before acquiring the lock
	// to avoid blocking concurrent reconciles during slow DNS lookups.
	resolved := resolveEndpoints(ctx, endpoints)

	s.syncMu.Lock()
	defer s.syncMu.Unlock()

	logger := logging.FromContext(ctx)
	if logger == slog.Default() {
		logger = s.logger
	}

	logger.Info("syncing proxy config", "routes", len(routes))

	// Convert to proxy config with cross-namespace validation, backend
	// protocol resolution (e.g. h2c from Service appProtocol), and
	// BackendTLSPolicy lookup for the proxy → backend TLS hop.
	cfg := proxy.ConvertHTTPRoutes(ctx, routes, s.clusterDomain, s.backendValidator, s.protocolResolver, s.tlsResolver)

	// Clear backends for routes that have failed backend refs.
	// This ensures the proxy returns 500 (no backend available) instead of
	// trying to connect to a nonexistent service (which would return 502).
	clearFailedBackends(cfg, routes, failedRefs)

	logger.Info("resolved endpoints",
		"original", len(endpoints),
		"resolved", len(resolved),
	)

	// Push to all endpoints.
	results := s.pusher.Push(ctx, cfg, resolved)

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

// clearFailedBackends removes backends from proxy config rules where the
// corresponding route rule has failed backend refs. This ensures the proxy
// returns 500 (no backend available) for rules with unresolvable backends,
// while leaving sibling rules with valid backends intact.
func clearFailedBackends(cfg *proxy.Config, routes []*gatewayv1.HTTPRoute, failedRefs []ingress.BackendRefError) {
	if len(failedRefs) == 0 {
		return
	}

	// Build a set of failed backend keys: "routeNS/routeName/backendName".
	failedBackends := make(map[string]bool, len(failedRefs))
	for _, ref := range failedRefs {
		failedBackends[ref.RouteNamespace+"/"+ref.RouteName+"/"+ref.BackendName] = true
	}

	// Walk routes and their rules in order, matching proxy config rules 1:1.
	// ConvertHTTPRoutes generates one proxy rule per route rule.
	ruleIdx := 0

	for _, route := range routes {
		for _, rule := range route.Spec.Rules {
			if ruleIdx >= len(cfg.Rules) {
				break
			}

			// Check if any backend ref in this rule failed.
			ruleHasFailedRef := false

			for _, backendRef := range rule.BackendRefs {
				key := route.Namespace + "/" + route.Name + "/" + string(backendRef.Name)
				if failedBackends[key] {
					ruleHasFailedRef = true

					break
				}
			}

			if ruleHasFailedRef {
				cfg.Rules[ruleIdx].Backends = nil
			}

			ruleIdx++
		}
	}
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
