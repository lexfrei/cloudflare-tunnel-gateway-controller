package controller

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/cockroachdb/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/logging"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
)

// ProxySyncer collects HTTPRoute resources and pushes proxy config
// to enhanced-cloudflared replicas via HTTP API.
type ProxySyncer struct {
	client.Client

	scheme           *runtime.Scheme
	clusterDomain    string
	gatewayClassName string
	logger           *slog.Logger
	pusher           *proxy.ConfigPusher
	syncMu           sync.Mutex
}

// NewProxySyncer creates a ProxySyncer for pushing config to proxy replicas.
func NewProxySyncer(
	kubeClient client.Client,
	scheme *runtime.Scheme,
	clusterDomain string,
	gatewayClassName string,
	authToken string,
	logger *slog.Logger,
) *ProxySyncer {
	if logger == nil {
		logger = slog.Default()
	}

	return &ProxySyncer{
		Client:           kubeClient,
		scheme:           scheme,
		clusterDomain:    clusterDomain,
		gatewayClassName: gatewayClassName,
		logger:           logger.With("component", "proxy-syncer"),
		pusher: proxy.NewConfigPusher(&http.Client{
			Timeout: 10 * time.Second,
		}, authToken),
	}
}

// SyncRoutes collects all relevant HTTPRoutes, converts them to proxy config,
// and pushes to all endpoints. Returns error if any endpoint push fails.
func (s *ProxySyncer) SyncRoutes(ctx context.Context, endpoints []string) error {
	s.syncMu.Lock()
	defer s.syncMu.Unlock()

	logger := logging.FromContext(ctx)
	if logger == slog.Default() {
		logger = s.logger
	}

	// Collect HTTPRoutes that reference our GatewayClass.
	routes, err := s.collectHTTPRoutes(ctx)
	if err != nil {
		return errors.Wrap(err, "collect HTTPRoutes")
	}

	logger.Info("collected HTTPRoutes for proxy sync", "count", len(routes))

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

// collectHTTPRoutes finds all HTTPRoutes that reference Gateways of our GatewayClass.
func (s *ProxySyncer) collectHTTPRoutes(ctx context.Context) ([]*gatewayv1.HTTPRoute, error) {
	// List all Gateways of our GatewayClass.
	var gatewayList gatewayv1.GatewayList

	err := s.List(ctx, &gatewayList)
	if err != nil {
		return nil, errors.Wrap(err, "list gateways")
	}

	gatewayNames := make(map[string]bool)

	for idx := range gatewayList.Items {
		if string(gatewayList.Items[idx].Spec.GatewayClassName) == s.gatewayClassName {
			key := gatewayList.Items[idx].Namespace + "/" + gatewayList.Items[idx].Name
			gatewayNames[key] = true
		}
	}

	// List all HTTPRoutes and filter by parentRef.
	var routeList gatewayv1.HTTPRouteList

	err = s.List(ctx, &routeList)
	if err != nil {
		return nil, errors.Wrap(err, "list httproutes")
	}

	var relevantRoutes []*gatewayv1.HTTPRoute

	for routeIdx := range routeList.Items {
		route := &routeList.Items[routeIdx]
		if s.routeReferencesGateway(route, gatewayNames) {
			relevantRoutes = append(relevantRoutes, route)
		}
	}

	return relevantRoutes, nil
}

// routeReferencesGateway checks if an HTTPRoute references any of our managed Gateways.
func (s *ProxySyncer) routeReferencesGateway(
	route *gatewayv1.HTTPRoute,
	gatewayNames map[string]bool,
) bool {
	for _, parentRef := range route.Spec.ParentRefs {
		// Skip non-Gateway parentRefs.
		if parentRef.Group != nil && *parentRef.Group != gatewayv1.GroupName {
			continue
		}

		if parentRef.Kind != nil && *parentRef.Kind != kindGateway {
			continue
		}

		namespace := route.Namespace
		if parentRef.Namespace != nil {
			namespace = string(*parentRef.Namespace)
		}

		key := namespace + "/" + string(parentRef.Name)
		if gatewayNames[key] {
			return true
		}
	}

	return false
}
