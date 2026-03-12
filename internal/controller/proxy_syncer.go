package controller

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/cockroachdb/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/ingress"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/logging"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/referencegrant"
)

// ProxySyncer converts HTTPRoute resources to proxy config
// and pushes it to enhanced-cloudflared replicas via HTTP API.
type ProxySyncer struct {
	clusterDomain    string
	logger           *slog.Logger
	pusher           *proxy.ConfigPusher
	backendValidator proxy.BackendRefValidator
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
	}
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

		toKind := "Service"
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

	// Convert to proxy config with cross-namespace validation.
	cfg := proxy.ConvertHTTPRoutes(ctx, routes, s.clusterDomain, s.backendValidator)

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
