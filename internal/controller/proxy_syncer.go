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
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/logging"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
)

// ProxySyncer converts HTTPRoute resources to proxy config
// and pushes it to enhanced-cloudflared replicas via HTTP API.
type ProxySyncer struct {
	clusterDomain string
	logger        *slog.Logger
	pusher        *proxy.ConfigPusher
	syncMu        sync.Mutex
}

// NewProxySyncer creates a ProxySyncer for pushing config to proxy replicas.
func NewProxySyncer(
	clusterDomain string,
	authToken string,
	logger *slog.Logger,
) *ProxySyncer {
	if logger == nil {
		logger = slog.Default()
	}

	return &ProxySyncer{
		clusterDomain: clusterDomain,
		logger:        logger.With("component", "proxy-syncer"),
		pusher: proxy.NewConfigPusher(&http.Client{
			Timeout: 10 * time.Second,
		}, authToken),
	}
}

// SyncRoutes converts pre-collected HTTPRoutes to proxy config and pushes to all endpoints.
// Routes should come from the RouteSyncer's SyncResult to avoid redundant API calls.
func (s *ProxySyncer) SyncRoutes(ctx context.Context, endpoints []string, routes []*gatewayv1.HTTPRoute) error {
	s.syncMu.Lock()
	defer s.syncMu.Unlock()

	logger := logging.FromContext(ctx)
	if logger == slog.Default() {
		logger = s.logger
	}

	logger.Info("syncing proxy config", "routes", len(routes))

	// Convert to proxy config.
	cfg := proxy.ConvertHTTPRoutes(routes, s.clusterDomain)

	// Resolve headless service DNS names to individual pod IPs.
	resolved := resolveEndpoints(ctx, endpoints)

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

		addrs, err := net.DefaultResolver.LookupHost(ctx, hostname)
		if err != nil || len(addrs) == 0 {
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
