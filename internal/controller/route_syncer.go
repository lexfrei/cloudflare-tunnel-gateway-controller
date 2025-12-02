package controller

import (
	"context"
	"log/slog"

	"github.com/cloudflare/cloudflare-go/v6"
	"github.com/cloudflare/cloudflare-go/v6/zero_trust"
	"github.com/cockroachdb/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/config"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/ingress"
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

	httpBuilder      *ingress.Builder
	grpcBuilder      *ingress.GRPCBuilder
	bindingValidator *routebinding.Validator
}

// NewRouteSyncer creates a new RouteSyncer.
func NewRouteSyncer(
	c client.Client,
	scheme *runtime.Scheme,
	clusterDomain string,
	gatewayClassName string,
	configResolver *config.Resolver,
) *RouteSyncer {
	refGrantValidator := referencegrant.NewValidator(c)

	return &RouteSyncer{
		Client:           c,
		Scheme:           scheme,
		ClusterDomain:    clusterDomain,
		GatewayClassName: gatewayClassName,
		ConfigResolver:   configResolver,
		httpBuilder:      ingress.NewBuilder(clusterDomain, refGrantValidator),
		grpcBuilder:      ingress.NewGRPCBuilder(clusterDomain, refGrantValidator),
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

// SyncAllRoutes synchronizes all HTTPRoute and GRPCRoute resources to Cloudflare Tunnel.
//
//nolint:funlen,wrapcheck // complex sync logic requires length; Cloudflare API errors are intentionally unwrapped
func (s *RouteSyncer) SyncAllRoutes(ctx context.Context) (ctrl.Result, *SyncResult, error) {
	logger := slog.Default().With("component", "route-syncer")

	// Resolve configuration from GatewayClassConfig
	resolvedConfig, err := s.ConfigResolver.ResolveFromGatewayClassName(ctx, s.GatewayClassName)
	if err != nil {
		logger.Error("failed to resolve config from GatewayClassConfig", "error", err)

		return ctrl.Result{RequeueAfter: apiErrorRequeueDelay}, nil, nil
	}

	// Create Cloudflare client with resolved credentials
	cfClient := s.ConfigResolver.CreateCloudflareClient(resolvedConfig)

	// Resolve account ID (auto-detect if not in config)
	accountID, err := s.ConfigResolver.ResolveAccountID(ctx, cfClient, resolvedConfig)
	if err != nil {
		logger.Error("failed to resolve account ID", "error", err)

		return ctrl.Result{RequeueAfter: apiErrorRequeueDelay}, nil, nil
	}

	// Get current tunnel configuration
	currentConfig, err := cfClient.ZeroTrust.Tunnels.Cloudflared.Configurations.Get(
		ctx,
		resolvedConfig.TunnelID,
		zero_trust.TunnelCloudflaredConfigurationGetParams{
			AccountID: cloudflare.String(accountID),
		},
	)
	if err != nil {
		logger.Error("failed to get current tunnel configuration", "error", err)

		return ctrl.Result{RequeueAfter: apiErrorRequeueDelay}, nil, nil
	}

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

	_, err = cfClient.ZeroTrust.Tunnels.Cloudflared.Configurations.Update(ctx, resolvedConfig.TunnelID, cfConfig)
	if err != nil {
		logger.Error("failed to update tunnel configuration", "error", err)

		result := &SyncResult{
			HTTPRoutes:        httpRoutes,
			GRPCRoutes:        grpcRoutes,
			HTTPRouteBindings: httpBindings,
			GRPCRouteBindings: grpcBindings,
			HTTPFailedRefs:    httpBuildResult.FailedRefs,
			GRPCFailedRefs:    grpcBuildResult.FailedRefs,
		}

		return ctrl.Result{RequeueAfter: apiErrorRequeueDelay}, result, err
	}

	logger.Info("successfully updated tunnel configuration", "rules", len(finalRules))

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
	logger := slog.Default().With("component", "route-syncer")

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
	logger := slog.Default().With("component", "route-syncer")

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
