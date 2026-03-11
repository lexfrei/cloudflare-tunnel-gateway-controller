package controller

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/cloudflare/cloudflare-go/v6"
	"github.com/cloudflare/cloudflare-go/v6/zero_trust"
	"github.com/cockroachdb/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/cfmetrics"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/config"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/ingress"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/logging"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/referencegrant"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/routebinding"
)

// RouteSyncer provides unified synchronization of HTTPRoute and GRPCRoute
// resources to Cloudflare Tunnel configuration.
//
// Both HTTPRouteReconciler and GRPCRouteReconciler use this to sync routes,
// ensuring that all route types are collected and synchronized together.
type RouteSyncer struct {
	client.Client

	Scheme           *runtime.Scheme
	ClusterDomain    string
	GatewayClassName string
	ConfigResolver   *config.Resolver
	Metrics          cfmetrics.Collector
	Logger           *slog.Logger

	httpBuilder      *ingress.Builder
	grpcBuilder      *ingress.GRPCBuilder
	bindingValidator *routebinding.Validator

	// syncMu protects concurrent calls to SyncAllRoutes.
	// Both HTTPRouteReconciler and GRPCRouteReconciler may call SyncAllRoutes
	// concurrently, and this mutex ensures serialized access to Cloudflare API.
	syncMu sync.Mutex
}

// NewRouteSyncer creates a new RouteSyncer.
func NewRouteSyncer(
	c client.Client,
	scheme *runtime.Scheme,
	clusterDomain string,
	gatewayClassName string,
	configResolver *config.Resolver,
	metricsCollector cfmetrics.Collector,
	logger *slog.Logger,
) *RouteSyncer {
	refGrantValidator := referencegrant.NewValidator(c)

	if logger == nil {
		logger = slog.Default()
	}

	componentLogger := logger.With("component", "route-syncer")

	return &RouteSyncer{
		Client:           c,
		Scheme:           scheme,
		ClusterDomain:    clusterDomain,
		GatewayClassName: gatewayClassName,
		ConfigResolver:   configResolver,
		Metrics:          metricsCollector,
		Logger:           componentLogger,
		httpBuilder:      ingress.NewBuilder(clusterDomain, refGrantValidator, c, metricsCollector, componentLogger),
		grpcBuilder:      ingress.NewGRPCBuilder(clusterDomain, refGrantValidator, c, metricsCollector, componentLogger),
		bindingValidator: routebinding.NewValidator(c),
	}
}

// routeBindingInfo contains a route and its binding validation results per parent.
type routeBindingInfo struct {
	// bindingResults maps ParentRef index to binding result for that parent.
	bindingResults map[int]routebinding.BindingResult
}

// SyncResult contains the results of a sync operation.
type SyncResult struct {
	HTTPRoutes        []gatewayv1.HTTPRoute
	GRPCRoutes        []gatewayv1.GRPCRoute
	HTTPRouteBindings map[string]routeBindingInfo // key: namespace/name
	GRPCRouteBindings map[string]routeBindingInfo // key: namespace/name
	HTTPFailedRefs    []ingress.BackendRefError   // Failed backend refs from HTTP routes
	GRPCFailedRefs    []ingress.BackendRefError   // Failed backend refs from GRPC routes
}

// httpStatusEntries builds routeStatusEntry slice for HTTP routes.
func (sr *SyncResult) httpStatusEntries(
	updateFn func(ctx context.Context, route *gatewayv1.HTTPRoute, bi routeBindingInfo, fr []ingress.BackendRefError, se error) error,
) []routeStatusEntry {
	return buildStatusEntries(sr.HTTPRoutes, sr.HTTPRouteBindings, sr.HTTPFailedRefs, updateFn)
}

// grpcStatusEntries builds routeStatusEntry slice for GRPC routes.
func (sr *SyncResult) grpcStatusEntries(
	updateFn func(ctx context.Context, route *gatewayv1.GRPCRoute, bi routeBindingInfo, fr []ingress.BackendRefError, se error) error,
) []routeStatusEntry {
	return buildStatusEntries(sr.GRPCRoutes, sr.GRPCRouteBindings, sr.GRPCFailedRefs, updateFn)
}

// routeObject is the constraint for Gateway API route types with Name and Namespace.
type routeObject interface {
	gatewayv1.HTTPRoute | gatewayv1.GRPCRoute
}

// buildStatusEntries creates routeStatusEntry slice from any route type.
func buildStatusEntries[T routeObject](
	routes []T,
	bindings map[string]routeBindingInfo,
	failedRefs []ingress.BackendRefError,
	updateFn func(ctx context.Context, route *T, bi routeBindingInfo, fr []ingress.BackendRefError, se error) error,
) []routeStatusEntry {
	entries := make([]routeStatusEntry, 0, len(routes))

	for i := range routes {
		route := &routes[i]
		// Both HTTPRoute and GRPCRoute embed ObjectMeta which provides these methods.
		obj, ok := any(route).(interface {
			GetName() string
			GetNamespace() string
		})
		if !ok {
			continue
		}

		name := obj.GetName()
		namespace := obj.GetNamespace()
		routeKey := namespace + "/" + name

		entries = append(entries, routeStatusEntry{
			name:        name,
			namespace:   namespace,
			bindingInfo: bindings[routeKey],
			failedRefs:  filterFailedRefs(failedRefs, namespace, name),
			update: func(ctx context.Context, bi routeBindingInfo, fr []ingress.BackendRefError, se error) error {
				return updateFn(ctx, route, bi, fr, se)
			},
		})
	}

	return entries
}

// syncUpdateParams holds the common parameters for route sync + status update.
type syncUpdateParams struct {
	routeSyncer    *RouteSyncer
	proxySyncer    *ProxySyncer
	proxyEndpoints []string
	statusEntries  func(*SyncResult) []routeStatusEntry
}

// syncAndUpdateStatusCommon performs a full route sync, pushes proxy config,
// and updates route status. Used by both HTTPRoute and GRPCRoute reconcilers.
func syncAndUpdateStatusCommon(ctx context.Context, params syncUpdateParams) (ctrl.Result, error) {
	logger := logging.FromContext(ctx)

	result, syncResult, syncErr := params.routeSyncer.SyncAllRoutes(ctx)

	// Push config to v2 proxy replicas (best-effort, non-blocking).
	if params.proxySyncer != nil && len(params.proxyEndpoints) > 0 && syncResult != nil {
		routes := httpRoutePtrs(syncResult.HTTPRoutes)
		if proxyErr := params.proxySyncer.SyncRoutes(ctx, params.proxyEndpoints, routes); proxyErr != nil {
			logger.Error("proxy sync failed (non-blocking)", "error", proxyErr)
		}
	}

	var statusUpdateErr error

	if syncResult != nil {
		statusUpdateErr = updateRoutesStatus(ctx, logger, params.statusEntries(syncResult), syncErr)
	}

	if syncErr != nil && result.RequeueAfter == 0 {
		return result, nil
	}

	if statusUpdateErr != nil {
		return ctrl.Result{}, statusUpdateErr
	}

	return result, nil
}

// buildResultForError creates a SyncResult containing all relevant routes.
// Used when early errors occur (before routes are collected) to ensure
// route statuses are updated to reflect the error.
func (s *RouteSyncer) buildResultForError(ctx context.Context) *SyncResult {
	httpRoutes, httpBindings, _ := s.getRelevantHTTPRoutes(ctx)
	grpcRoutes, grpcBindings, _ := s.getRelevantGRPCRoutes(ctx)

	return &SyncResult{
		HTTPRoutes:        httpRoutes,
		GRPCRoutes:        grpcRoutes,
		HTTPRouteBindings: httpBindings,
		GRPCRouteBindings: grpcBindings,
	}
}

// SyncAllRoutes synchronizes all HTTPRoute and GRPCRoute resources to Cloudflare Tunnel.
//
//nolint:funlen,wrapcheck // complex sync logic requires length; Cloudflare API errors are intentionally unwrapped
func (s *RouteSyncer) SyncAllRoutes(ctx context.Context) (ctrl.Result, *SyncResult, error) {
	// Serialize concurrent sync calls to prevent race conditions when
	// both HTTPRouteReconciler and GRPCRouteReconciler trigger syncs.
	s.syncMu.Lock()
	defer s.syncMu.Unlock()

	startTime := time.Now()

	// Prefer context logger (with reconcile ID) over struct logger
	logger := logging.FromContext(ctx)
	if logger == slog.Default() {
		logger = s.Logger
	}

	// Resolve configuration from GatewayClassConfig
	resolvedConfig, err := s.ConfigResolver.ResolveFromGatewayClassName(ctx, s.GatewayClassName)
	if err != nil {
		logger.Error("failed to resolve config from GatewayClassConfig", "error", err)

		return ctrl.Result{RequeueAfter: apiErrorRequeueDelay, Priority: new(priorityRoute)}, s.buildResultForError(ctx), err
	}

	// Create Cloudflare client with resolved credentials
	cfClient := s.ConfigResolver.CreateCloudflareClient(resolvedConfig)

	// Resolve account ID (auto-detect if not in config)
	accountID, err := s.ConfigResolver.ResolveAccountID(ctx, cfClient, resolvedConfig)
	if err != nil {
		logger.Error("failed to resolve account ID", "error", err)

		return ctrl.Result{RequeueAfter: apiErrorRequeueDelay, Priority: new(priorityRoute)}, s.buildResultForError(ctx), err
	}

	// Get current tunnel configuration
	getStart := time.Now()

	currentConfig, err := cfClient.ZeroTrust.Tunnels.Cloudflared.Configurations.Get(
		ctx,
		resolvedConfig.TunnelID,
		zero_trust.TunnelCloudflaredConfigurationGetParams{
			AccountID: cloudflare.String(accountID),
		},
	)
	if err != nil {
		s.Metrics.RecordAPICall(ctx, "get", "tunnel_config", "error", time.Since(getStart))
		s.Metrics.RecordAPIError(ctx, "get", cfmetrics.ClassifyCloudflareError(err))
		logger.Error("failed to get current tunnel configuration", "error", err)

		return ctrl.Result{RequeueAfter: apiErrorRequeueDelay, Priority: new(priorityRoute)}, s.buildResultForError(ctx), err
	}

	s.Metrics.RecordAPICall(ctx, "get", "tunnel_config", "success", time.Since(getStart))

	// Collect all relevant HTTPRoutes with binding validation
	httpRoutes, httpBindings, err := s.getRelevantHTTPRoutes(ctx)
	if err != nil {
		return ctrl.Result{}, nil, errors.Wrap(err, "failed to list httproutes")
	}

	// Collect all relevant GRPCRoutes with binding validation
	grpcRoutes, grpcBindings, err := s.getRelevantGRPCRoutes(ctx)
	if err != nil {
		return ctrl.Result{}, nil, errors.Wrap(err, "failed to list grpcroutes")
	}

	logger.Info("syncing routes to cloudflare",
		"httpRoutes", len(httpRoutes),
		"grpcRoutes", len(grpcRoutes),
	)

	// Build desired rules from both route types
	httpBuildResult := s.httpBuilder.Build(ctx, httpRoutes)
	grpcBuildResult := s.grpcBuilder.Build(ctx, grpcRoutes)

	// Merge rules (HTTP rules first, then GRPC, then sort)
	desiredRules := mergeAndSortRules(httpBuildResult.Rules, grpcBuildResult.Rules)

	// Compute diff between current and desired
	toAdd, toRemove := ingress.DiffRules(currentConfig.Config.Ingress, desiredRules)

	logger.Info("computed diff", "toAdd", len(toAdd), "toRemove", len(toRemove))

	// Apply diff to get final rules
	finalRules := ingress.ApplyDiff(currentConfig.Config.Ingress, toAdd, toRemove)

	// Ensure catch-all rule exists at the end
	finalRules = ingress.EnsureCatchAll(finalRules)

	// Check ingress rules limit before API call
	if len(finalRules) > maxIngressRules {
		limitErr := errors.Newf("ingress rules limit exceeded: %d rules (max %d)", len(finalRules), maxIngressRules)
		logger.Error("ingress rules limit exceeded", "count", len(finalRules), "max", maxIngressRules)

		// Record error metrics
		s.Metrics.RecordSyncDuration(ctx, "error", time.Since(startTime))
		s.Metrics.RecordSyncError(ctx, "limit_exceeded")

		result := &SyncResult{
			HTTPRoutes:        httpRoutes,
			GRPCRoutes:        grpcRoutes,
			HTTPRouteBindings: httpBindings,
			GRPCRouteBindings: grpcBindings,
			HTTPFailedRefs:    httpBuildResult.FailedRefs,
			GRPCFailedRefs:    grpcBuildResult.FailedRefs,
		}

		return ctrl.Result{}, result, limitErr
	}

	cfConfig := zero_trust.TunnelCloudflaredConfigurationUpdateParams{
		AccountID: cloudflare.String(accountID),
		Config: cloudflare.F(zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfig{
			Ingress: cloudflare.F(finalRules),
		}),
	}

	updateStart := time.Now()

	_, err = cfClient.ZeroTrust.Tunnels.Cloudflared.Configurations.Update(ctx, resolvedConfig.TunnelID, cfConfig)
	if err != nil {
		s.Metrics.RecordAPICall(ctx, "update", "tunnel_config", "error", time.Since(updateStart))
		s.Metrics.RecordAPIError(ctx, "update", cfmetrics.ClassifyCloudflareError(err))
		logger.Error("failed to update tunnel configuration", "error", err)

		// Record error metrics
		s.Metrics.RecordSyncDuration(ctx, "error", time.Since(startTime))
		s.Metrics.RecordSyncError(ctx, cfmetrics.ClassifyCloudflareError(err))

		result := &SyncResult{
			HTTPRoutes:        httpRoutes,
			GRPCRoutes:        grpcRoutes,
			HTTPRouteBindings: httpBindings,
			GRPCRouteBindings: grpcBindings,
			HTTPFailedRefs:    httpBuildResult.FailedRefs,
			GRPCFailedRefs:    grpcBuildResult.FailedRefs,
		}

		return ctrl.Result{RequeueAfter: apiErrorRequeueDelay, Priority: new(priorityRoute)}, result, err
	}

	s.Metrics.RecordAPICall(ctx, "update", "tunnel_config", "success", time.Since(updateStart))
	logger.Info("successfully updated tunnel configuration", "rules", len(finalRules))

	// Record success metrics
	s.Metrics.RecordSyncDuration(ctx, "success", time.Since(startTime))
	s.Metrics.RecordSyncedRoutes(ctx, "http", len(httpRoutes))
	s.Metrics.RecordSyncedRoutes(ctx, "grpc", len(grpcRoutes))
	s.Metrics.RecordIngressRules(ctx, len(finalRules))
	s.Metrics.RecordFailedBackendRefs(ctx, "http", len(httpBuildResult.FailedRefs))
	s.Metrics.RecordFailedBackendRefs(ctx, "grpc", len(grpcBuildResult.FailedRefs))

	result := &SyncResult{
		HTTPRoutes:        httpRoutes,
		GRPCRoutes:        grpcRoutes,
		HTTPRouteBindings: httpBindings,
		GRPCRouteBindings: grpcBindings,
		HTTPFailedRefs:    httpBuildResult.FailedRefs,
		GRPCFailedRefs:    grpcBuildResult.FailedRefs,
	}

	return ctrl.Result{}, result, nil
}

//nolint:funlen,dupl // complex binding validation logic; similar to GRPC but for HTTP types
func (s *RouteSyncer) getRelevantHTTPRoutes(
	ctx context.Context,
) ([]gatewayv1.HTTPRoute, map[string]routeBindingInfo, error) {
	// Prefer context logger (with reconcile ID) over struct logger
	logger := logging.FromContext(ctx)
	if logger == slog.Default() {
		logger = s.Logger
	}

	var routeList gatewayv1.HTTPRouteList

	err := s.List(ctx, &routeList)
	if err != nil {
		return nil, nil, errors.Wrap(err, "failed to list httproutes")
	}

	var relevantRoutes []gatewayv1.HTTPRoute

	bindings := make(map[string]routeBindingInfo)

	for i := range routeList.Items {
		route := &routeList.Items[i]
		routeKey := route.Namespace + "/" + route.Name
		bindingInfo := routeBindingInfo{
			bindingResults: make(map[int]routebinding.BindingResult),
		}

		hasAcceptedBinding := false

		for refIdx, ref := range route.Spec.ParentRefs {
			if ref.Kind != nil && *ref.Kind != kindGateway {
				continue
			}

			namespace := route.Namespace
			if ref.Namespace != nil {
				namespace = string(*ref.Namespace)
			}

			var gateway gatewayv1.Gateway

			getErr := s.Get(ctx, client.ObjectKey{Name: string(ref.Name), Namespace: namespace}, &gateway)
			if getErr != nil {
				continue
			}

			if gateway.Spec.GatewayClassName != gatewayv1.ObjectName(s.GatewayClassName) {
				continue
			}

			routeInfo := &routebinding.RouteInfo{
				Name:        route.Name,
				Namespace:   route.Namespace,
				Hostnames:   route.Spec.Hostnames,
				Kind:        routebinding.KindHTTPRoute,
				SectionName: ref.SectionName,
			}

			result, bindErr := s.bindingValidator.ValidateBinding(ctx, &gateway, routeInfo)
			if bindErr != nil {
				logger.Error("failed to validate route binding",
					"route", routeKey,
					"gateway", gateway.Name,
					"error", bindErr)

				continue
			}

			bindingInfo.bindingResults[refIdx] = result

			if result.Accepted {
				hasAcceptedBinding = true
			}
		}

		bindings[routeKey] = bindingInfo

		if hasAcceptedBinding {
			relevantRoutes = append(relevantRoutes, routeList.Items[i])
		}
	}

	return relevantRoutes, bindings, nil
}

//nolint:funlen,dupl // complex binding validation logic; similar to HTTP but for GRPC types
func (s *RouteSyncer) getRelevantGRPCRoutes(
	ctx context.Context,
) ([]gatewayv1.GRPCRoute, map[string]routeBindingInfo, error) {
	// Prefer context logger (with reconcile ID) over struct logger
	logger := logging.FromContext(ctx)
	if logger == slog.Default() {
		logger = s.Logger
	}

	var routeList gatewayv1.GRPCRouteList

	err := s.List(ctx, &routeList)
	if err != nil {
		return nil, nil, errors.Wrap(err, "failed to list grpcroutes")
	}

	var relevantRoutes []gatewayv1.GRPCRoute

	bindings := make(map[string]routeBindingInfo)

	for i := range routeList.Items {
		route := &routeList.Items[i]
		routeKey := route.Namespace + "/" + route.Name
		bindingInfo := routeBindingInfo{
			bindingResults: make(map[int]routebinding.BindingResult),
		}

		hasAcceptedBinding := false

		for refIdx, ref := range route.Spec.ParentRefs {
			if ref.Kind != nil && *ref.Kind != kindGateway {
				continue
			}

			namespace := route.Namespace
			if ref.Namespace != nil {
				namespace = string(*ref.Namespace)
			}

			var gateway gatewayv1.Gateway

			getErr := s.Get(ctx, client.ObjectKey{Name: string(ref.Name), Namespace: namespace}, &gateway)
			if getErr != nil {
				continue
			}

			if gateway.Spec.GatewayClassName != gatewayv1.ObjectName(s.GatewayClassName) {
				continue
			}

			routeInfo := &routebinding.RouteInfo{
				Name:        route.Name,
				Namespace:   route.Namespace,
				Hostnames:   route.Spec.Hostnames,
				Kind:        routebinding.KindGRPCRoute,
				SectionName: ref.SectionName,
			}

			result, bindErr := s.bindingValidator.ValidateBinding(ctx, &gateway, routeInfo)
			if bindErr != nil {
				logger.Error("failed to validate route binding",
					"route", routeKey,
					"gateway", gateway.Name,
					"error", bindErr)

				continue
			}

			bindingInfo.bindingResults[refIdx] = result

			if result.Accepted {
				hasAcceptedBinding = true
			}
		}

		bindings[routeKey] = bindingInfo

		if hasAcceptedBinding {
			relevantRoutes = append(relevantRoutes, routeList.Items[i])
		}
	}

	return relevantRoutes, bindings, nil
}

// mergeAndSortRules combines HTTP and GRPC rules and adds catch-all.
// Rules are already sorted within each builder, but we need to merge them.
func mergeAndSortRules(
	httpRules, grpcRules []zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress,
) []zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress {
	// Remove catch-all from httpRules if present (it's added at the end anyway)
	httpFiltered := filterOutCatchAll(httpRules)
	grpcFiltered := filterOutCatchAll(grpcRules)

	// Combine all rules
	combined := make(
		[]zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress,
		0,
		len(httpFiltered)+len(grpcFiltered)+1,
	)
	combined = append(combined, httpFiltered...)
	combined = append(combined, grpcFiltered...)

	// Rules are already sorted by each builder, but we want consistent ordering
	// Sort by: hostname -> path length (longer first)
	// Note: Since both builders use the same sorting logic, and hostnames
	// typically don't overlap between HTTP and GRPC routes, the merge is straightforward.

	return combined
}

func filterOutCatchAll(
	rules []zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress,
) []zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress {
	filtered := make([]zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress, 0, len(rules))

	for i := range rules {
		if !ingress.IsCatchAll(ingress.RuleFromUpdate(&rules[i])) {
			filtered = append(filtered, rules[i])
		}
	}

	return filtered
}
