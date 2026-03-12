package controller

import (
	"cmp"
	"context"
	"log/slog"
	"slices"
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

	Scheme         *runtime.Scheme
	ClusterDomain  string
	ControllerName string
	ConfigResolver *config.Resolver
	Metrics        cfmetrics.Collector
	Logger         *slog.Logger

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
	controllerName string,
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
		ControllerName:   controllerName,
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

	// RejectedHTTPRoutes and RejectedGRPCRoutes are routes that reference
	// our Gateways but were not accepted by binding validation (e.g.,
	// sectionName or port mismatch). Their status must be updated with
	// Accepted=False so conformance tests can observe the rejection.
	RejectedHTTPRoutes []gatewayv1.HTTPRoute
	RejectedGRPCRoutes []gatewayv1.GRPCRoute
}

// httpStatusEntries builds routeStatusEntry slice for HTTP routes,
// including rejected routes that need Accepted=False status.
func (sr *SyncResult) httpStatusEntries(
	updateFn func(ctx context.Context, route *gatewayv1.HTTPRoute, bi routeBindingInfo, fr []ingress.BackendRefError, se error) error,
) []routeStatusEntry {
	entries := buildStatusEntries(sr.HTTPRoutes, sr.HTTPRouteBindings, sr.HTTPFailedRefs, updateFn)
	// Rejected routes have no failed refs — they were rejected at binding level.
	entries = append(entries, buildStatusEntries(sr.RejectedHTTPRoutes, sr.HTTPRouteBindings, nil, updateFn)...)

	return entries
}

// grpcStatusEntries builds routeStatusEntry slice for GRPC routes,
// including rejected routes that need Accepted=False status.
func (sr *SyncResult) grpcStatusEntries(
	updateFn func(ctx context.Context, route *gatewayv1.GRPCRoute, bi routeBindingInfo, fr []ingress.BackendRefError, se error) error,
) []routeStatusEntry {
	entries := buildStatusEntries(sr.GRPCRoutes, sr.GRPCRouteBindings, sr.GRPCFailedRefs, updateFn)
	entries = append(entries, buildStatusEntries(sr.RejectedGRPCRoutes, sr.GRPCRouteBindings, nil, updateFn)...)

	return entries
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
	pushProxy      bool
	statusEntries  func(*SyncResult) []routeStatusEntry
}

// syncAndUpdateStatusCommon performs a full route sync, pushes proxy config,
// and updates route status. Used by both HTTPRoute and GRPCRoute reconcilers.
func syncAndUpdateStatusCommon(ctx context.Context, params syncUpdateParams) (ctrl.Result, error) {
	logger := logging.FromContext(ctx)

	result, syncResult, syncErr := params.routeSyncer.SyncAllRoutes(ctx)

	// Push config to v2 proxy replicas (best-effort, non-blocking).
	// Only HTTPRoutes are pushed — the proxy converter does not yet support
	// gRPC-specific routing semantics. GRPCRoutes are handled by the
	// Cloudflare Tunnel ingress configuration (v1 path).
	// pushProxy is false for GRPCRoute reconciler to avoid redundant pushes.
	if params.pushProxy && params.proxySyncer != nil && len(params.proxyEndpoints) > 0 && syncResult != nil {
		routes := httpRoutePtrs(syncResult.HTTPRoutes)
		if proxyErr := params.proxySyncer.SyncRoutes(ctx, params.proxyEndpoints, routes, syncResult.HTTPFailedRefs); proxyErr != nil {
			logger.Error("proxy sync failed (non-blocking)", "error", proxyErr)
			params.routeSyncer.Metrics.RecordSyncError(ctx, "proxy_push")
		}
	}

	var statusUpdateErr error

	if syncResult != nil {
		statusUpdateErr = updateRoutesStatus(ctx, logger, params.statusEntries(syncResult), syncErr)
	}

	if syncErr != nil {
		if result.RequeueAfter > 0 {
			// Specific requeue interval requested (e.g., ingress rule limit exceeded).
			// Don't propagate error — controller-runtime would override the interval.
			return result, nil
		}

		// Propagate error for controller-runtime backoff-based requeue.
		// This is intentionally different from the pre-refactor behavior which
		// swallowed errors when RequeueAfter was 0, preventing retries.
		return result, syncErr
	}

	if statusUpdateErr != nil {
		return ctrl.Result{}, statusUpdateErr
	}

	return result, nil
}

// resolveConfigForController resolves configuration from the GatewayClass
// managed by this controller. Returns an error if no matching GatewayClass is found.
//
// When multiple GatewayClasses reference the same controllerName, the class
// with the lexicographically smallest name is used (deterministic ordering).
// A warning is logged because all GatewayClasses under one controller share
// the same tunnel credentials — multiple classes with different parametersRef
// may lead to unexpected behavior.
func (s *RouteSyncer) resolveConfigForController(ctx context.Context) (*config.ResolvedConfig, error) {
	classes := listGatewayClassesForController(ctx, s.Client, s.ControllerName)
	if len(classes) == 0 {
		return nil, errors.New("no GatewayClass found for controller " + s.ControllerName)
	}

	// Sort by name for deterministic selection.
	slices.SortFunc(classes, func(a, b gatewayv1.GatewayClass) int {
		return cmp.Compare(a.Name, b.Name)
	})

	if len(classes) > 1 {
		names := make([]string, len(classes))
		for i := range classes {
			names[i] = classes[i].Name
		}

		s.Logger.Warn("multiple GatewayClasses found for controller, using first alphabetically",
			"controllerName", s.ControllerName,
			"classes", names,
			"selected", classes[0].Name,
		)

		// Different parametersRef means different tunnel credentials — using one
		// class's credentials for another class's routes would silently send
		// traffic to the wrong tunnel. Return an error to prevent data integrity issues.
		if hasConflictingParametersRef(classes) {
			return nil, errors.Newf(
				"GatewayClasses %v have conflicting parametersRef for controller %s; "+
					"one controller instance supports only one tunnel configuration",
				names, s.ControllerName,
			)
		}
	}

	resolved, err := s.ConfigResolver.ResolveFromGatewayClass(ctx, &classes[0])
	if err != nil {
		return nil, errors.Wrap(err, "resolving config for GatewayClass "+classes[0].Name)
	}

	return resolved, nil
}

// hasConflictingParametersRef returns true if the given GatewayClasses
// reference different parametersRef names, indicating a misconfiguration.
func hasConflictingParametersRef(classes []gatewayv1.GatewayClass) bool {
	var firstName string

	for i := range classes {
		ref := classes[i].Spec.ParametersRef
		name := ""

		if ref != nil {
			name = ref.Name
		}

		if i == 0 {
			firstName = name

			continue
		}

		if name != firstName {
			return true
		}
	}

	return false
}

// buildResultForError creates a SyncResult containing all relevant routes.
// Used when early errors occur (before routes are collected) to ensure
// route statuses are updated to reflect the error.
func (s *RouteSyncer) buildResultForError(ctx context.Context) *SyncResult {
	httpResult, _ := s.getRelevantHTTPRoutes(ctx)
	grpcResult, _ := s.getRelevantGRPCRoutes(ctx)

	result := &SyncResult{}

	if httpResult != nil {
		result.HTTPRoutes = httpResult.accepted
		result.HTTPRouteBindings = httpResult.bindings
		result.RejectedHTTPRoutes = httpResult.rejected
	}

	if grpcResult != nil {
		result.GRPCRoutes = grpcResult.accepted
		result.GRPCRouteBindings = grpcResult.bindings
		result.RejectedGRPCRoutes = grpcResult.rejected
	}

	return result
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

	// Resolve configuration from the first matching GatewayClass.
	// All GatewayClasses managed by this controller share tunnel credentials.
	resolvedConfig, err := s.resolveConfigForController(ctx)
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
	httpResult, err := s.getRelevantHTTPRoutes(ctx)
	if err != nil {
		return ctrl.Result{}, nil, errors.Wrap(err, "failed to list httproutes")
	}

	// Collect all relevant GRPCRoutes with binding validation
	grpcResult, err := s.getRelevantGRPCRoutes(ctx)
	if err != nil {
		return ctrl.Result{}, nil, errors.Wrap(err, "failed to list grpcroutes")
	}

	logger.Info("syncing routes to cloudflare",
		"httpRoutes", len(httpResult.accepted),
		"grpcRoutes", len(grpcResult.accepted),
	)

	// Build desired rules from both route types (accepted routes only)
	httpBuildResult := s.httpBuilder.Build(ctx, httpResult.accepted)
	grpcBuildResult := s.grpcBuilder.Build(ctx, grpcResult.accepted)

	// Merge rules (HTTP rules first, then GRPC, then sort)
	desiredRules := mergeAndSortRules(httpBuildResult.Rules, grpcBuildResult.Rules)

	// Compute diff between current and desired
	toAdd, toRemove := ingress.DiffRules(currentConfig.Config.Ingress, desiredRules)

	logger.Info("computed diff", "toAdd", len(toAdd), "toRemove", len(toRemove))

	// Apply diff to get final rules
	finalRules := ingress.ApplyDiff(currentConfig.Config.Ingress, toAdd, toRemove)

	// Sort final rules — ApplyDiff returns rules in arbitrary order
	// (kept-from-current first, then toAdd). Wildcard rules (no hostname)
	// must come after specific hostname rules to avoid Cloudflare error 1056.
	finalRules = sortIngressRules(finalRules)

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
			HTTPRoutes:         httpResult.accepted,
			GRPCRoutes:         grpcResult.accepted,
			HTTPRouteBindings:  httpResult.bindings,
			GRPCRouteBindings:  grpcResult.bindings,
			HTTPFailedRefs:     httpBuildResult.FailedRefs,
			GRPCFailedRefs:     grpcBuildResult.FailedRefs,
			RejectedHTTPRoutes: httpResult.rejected,
			RejectedGRPCRoutes: grpcResult.rejected,
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
			HTTPRoutes:         httpResult.accepted,
			GRPCRoutes:         grpcResult.accepted,
			HTTPRouteBindings:  httpResult.bindings,
			GRPCRouteBindings:  grpcResult.bindings,
			HTTPFailedRefs:     httpBuildResult.FailedRefs,
			GRPCFailedRefs:     grpcBuildResult.FailedRefs,
			RejectedHTTPRoutes: httpResult.rejected,
			RejectedGRPCRoutes: grpcResult.rejected,
		}

		return ctrl.Result{RequeueAfter: apiErrorRequeueDelay, Priority: new(priorityRoute)}, result, err
	}

	s.Metrics.RecordAPICall(ctx, "update", "tunnel_config", "success", time.Since(updateStart))
	logger.Info("successfully updated tunnel configuration", "rules", len(finalRules))

	// Record success metrics
	s.Metrics.RecordSyncDuration(ctx, "success", time.Since(startTime))
	s.Metrics.RecordSyncedRoutes(ctx, "http", len(httpResult.accepted))
	s.Metrics.RecordSyncedRoutes(ctx, "grpc", len(grpcResult.accepted))
	s.Metrics.RecordIngressRules(ctx, len(finalRules))
	s.Metrics.RecordFailedBackendRefs(ctx, "http", len(httpBuildResult.FailedRefs))
	s.Metrics.RecordFailedBackendRefs(ctx, "grpc", len(grpcBuildResult.FailedRefs))

	result := &SyncResult{
		HTTPRoutes:         httpResult.accepted,
		GRPCRoutes:         grpcResult.accepted,
		HTTPRouteBindings:  httpResult.bindings,
		GRPCRouteBindings:  grpcResult.bindings,
		HTTPFailedRefs:     httpBuildResult.FailedRefs,
		GRPCFailedRefs:     grpcBuildResult.FailedRefs,
		RejectedHTTPRoutes: httpResult.rejected,
		RejectedGRPCRoutes: grpcResult.rejected,
	}

	return ctrl.Result{}, result, nil
}

// httpRouteResult holds accepted and rejected HTTPRoutes from binding validation.
type httpRouteResult struct {
	accepted []gatewayv1.HTTPRoute
	rejected []gatewayv1.HTTPRoute
	bindings map[string]routeBindingInfo
}

// grpcRouteResult holds accepted and rejected GRPCRoutes from binding validation.
type grpcRouteResult struct {
	accepted []gatewayv1.GRPCRoute
	rejected []gatewayv1.GRPCRoute
	bindings map[string]routeBindingInfo
}

//nolint:funlen,dupl // complex binding validation logic; similar to GRPC but for HTTP types
func (s *RouteSyncer) getRelevantHTTPRoutes(
	ctx context.Context,
) (*httpRouteResult, error) {
	// Prefer context logger (with reconcile ID) over struct logger
	logger := logging.FromContext(ctx)
	if logger == slog.Default() {
		logger = s.Logger
	}

	var routeList gatewayv1.HTTPRouteList

	err := s.List(ctx, &routeList)
	if err != nil {
		return nil, errors.Wrap(err, "failed to list httproutes")
	}

	classNames := managedClassNames(ctx, s.Client, s.ControllerName)

	result := &httpRouteResult{
		bindings: make(map[string]routeBindingInfo),
	}

	for i := range routeList.Items {
		route := &routeList.Items[i]
		routeKey := route.Namespace + "/" + route.Name
		bindingInfo := routeBindingInfo{
			bindingResults: make(map[int]routebinding.BindingResult),
		}

		hasAcceptedBinding := false
		referencesOurGateway := false

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

			if !classNames[string(gateway.Spec.GatewayClassName)] {
				continue
			}

			referencesOurGateway = true

			routeInfo := &routebinding.RouteInfo{
				Name:        route.Name,
				Namespace:   route.Namespace,
				Hostnames:   route.Spec.Hostnames,
				Kind:        routebinding.KindHTTPRoute,
				SectionName: ref.SectionName,
				Port:        ref.Port,
			}

			bindResult, bindErr := s.bindingValidator.ValidateBinding(ctx, &gateway, routeInfo)
			if bindErr != nil {
				logger.Error("failed to validate route binding",
					"route", routeKey,
					"gateway", gateway.Name,
					"error", bindErr)

				continue
			}

			bindingInfo.bindingResults[refIdx] = bindResult

			if bindResult.Accepted {
				hasAcceptedBinding = true
			}
		}

		result.bindings[routeKey] = bindingInfo

		if hasAcceptedBinding {
			result.accepted = append(result.accepted, routeList.Items[i])
		} else if referencesOurGateway {
			// Route references our Gateway but binding was rejected
			// (e.g., sectionName or port mismatch). Track it so we
			// can update its status with Accepted=False.
			result.rejected = append(result.rejected, routeList.Items[i])
		}
	}

	return result, nil
}

//nolint:funlen,dupl // complex binding validation logic; similar to HTTP but for GRPC types
func (s *RouteSyncer) getRelevantGRPCRoutes(
	ctx context.Context,
) (*grpcRouteResult, error) {
	// Prefer context logger (with reconcile ID) over struct logger
	logger := logging.FromContext(ctx)
	if logger == slog.Default() {
		logger = s.Logger
	}

	var routeList gatewayv1.GRPCRouteList

	err := s.List(ctx, &routeList)
	if err != nil {
		return nil, errors.Wrap(err, "failed to list grpcroutes")
	}

	classNames := managedClassNames(ctx, s.Client, s.ControllerName)

	result := &grpcRouteResult{
		bindings: make(map[string]routeBindingInfo),
	}

	for i := range routeList.Items {
		route := &routeList.Items[i]
		routeKey := route.Namespace + "/" + route.Name
		bindingInfo := routeBindingInfo{
			bindingResults: make(map[int]routebinding.BindingResult),
		}

		hasAcceptedBinding := false
		referencesOurGateway := false

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

			if !classNames[string(gateway.Spec.GatewayClassName)] {
				continue
			}

			referencesOurGateway = true

			routeInfo := &routebinding.RouteInfo{
				Name:        route.Name,
				Namespace:   route.Namespace,
				Hostnames:   route.Spec.Hostnames,
				Kind:        routebinding.KindGRPCRoute,
				SectionName: ref.SectionName,
				Port:        ref.Port,
			}

			bindResult, bindErr := s.bindingValidator.ValidateBinding(ctx, &gateway, routeInfo)
			if bindErr != nil {
				logger.Error("failed to validate route binding",
					"route", routeKey,
					"gateway", gateway.Name,
					"error", bindErr)

				continue
			}

			bindingInfo.bindingResults[refIdx] = bindResult

			if bindResult.Accepted {
				hasAcceptedBinding = true
			}
		}

		result.bindings[routeKey] = bindingInfo

		if hasAcceptedBinding {
			result.accepted = append(result.accepted, routeList.Items[i])
		} else if referencesOurGateway {
			result.rejected = append(result.rejected, routeList.Items[i])
		}
	}

	return result, nil
}

// sortIngressRules sorts ingress rules: specific hostnames alphabetically first,
// wildcard (no hostname) last, same hostname by path length (longer first).
// This ordering is required by Cloudflare API — rules without hostname before
// rules with hostname trigger error 1056.
func sortIngressRules(
	rules []zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress,
) []zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress {
	slices.SortStableFunc(rules, func(left, right zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress) int {
		leftPresent := left.Hostname.Present
		rightPresent := right.Hostname.Present

		// Rules without hostname (wildcard) sort after rules with hostname.
		if leftPresent != rightPresent {
			if leftPresent {
				return -1
			}

			return 1
		}

		// Both have hostname or both don't — sort alphabetically, then by path length (longer first).
		if c := cmp.Compare(left.Hostname.Value, right.Hostname.Value); c != 0 {
			return c
		}

		return cmp.Compare(len(right.Path.Value), len(left.Path.Value))
	})

	return rules
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

	return sortIngressRules(combined)
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
