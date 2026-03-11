package controller

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
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

	// Push to all endpoints.
	results := s.pusher.Push(ctx, cfg, endpoints)

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
			len(pushErrors), len(endpoints), errors.Join(pushErrors...))
	}

	logger.Info("successfully pushed proxy config",
		"endpoints", len(endpoints),
		"rules", len(cfg.Rules),
		"version", cfg.Version,
	)

	return nil
}
