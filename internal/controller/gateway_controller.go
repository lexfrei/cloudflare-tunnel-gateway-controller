package controller

import (
	"context"
	"encoding/pem"
	"fmt"
	"strings"
	"time"

	"github.com/cockroachdb/errors"
	"helm.sh/helm/v4/pkg/action"
	"helm.sh/helm/v4/pkg/chart"
	"helm.sh/helm/v4/pkg/release"
	v1release "helm.sh/helm/v4/pkg/release/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/api/v1alpha1"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/config"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/helm"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/logging"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/routebinding"
)

const (
	cloudflaredFinalizer = "cloudflare-tunnel.gateway.networking.k8s.io/cloudflared"
	cfArgotunnelSuffix   = ".cfargotunnel.com"

	// configErrorRequeueDelay is the delay before retrying when config resolution fails.
	configErrorRequeueDelay = 30 * time.Second

	// maxHelmReleaseName is the maximum length for Helm release names.
	maxHelmReleaseName = 53

	// msgReferencesResolved is the standard message for ResolvedRefs condition.
	msgReferencesResolved = "References resolved"

	// kindSecret is the resource kind for Kubernetes Secrets.
	kindSecret = "Secret"
)

// truncateMessage truncates a message to maxConditionMessageLength.
func truncateMessage(msg string) string {
	if len(msg) > maxConditionMessageLength {
		return msg[:maxConditionMessageLength-3] + "..."
	}

	return msg
}

// GatewayReconciler reconciles Gateway resources for the cloudflare-tunnel GatewayClass.
//
// It performs the following functions:
//   - Watches Gateway resources matching the configured GatewayClassName
//   - Reads configuration from GatewayClassConfig via parametersRef
//   - Updates Gateway status with tunnel CNAME address (for external-dns integration)
//   - Manages cloudflared deployment lifecycle via Helm (when enabled in config)
//   - Handles Gateway deletion with proper cleanup of cloudflared resources
//
// The reconciler uses finalizers to ensure cloudflared is properly removed
// when a Gateway is deleted.
type GatewayReconciler struct {
	client.Client

	// Scheme is the runtime scheme for API type registration.
	Scheme *runtime.Scheme

	// GatewayClassName is the name of the GatewayClass to watch.
	GatewayClassName string

	// ControllerName is reported in Gateway status conditions.
	ControllerName string

	// ConfigResolver resolves configuration from GatewayClassConfig.
	ConfigResolver *config.Resolver

	// HelmManager handles cloudflared deployment. If nil, cloudflared
	// management is disabled regardless of config.
	HelmManager *helm.Manager
}

func (r *GatewayReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var gateway gatewayv1.Gateway

	if err := r.Get(ctx, req.NamespacedName, &gateway); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}

		return ctrl.Result{}, errors.Wrap(err, "failed to get gateway")
	}

	if gateway.Spec.GatewayClassName != gatewayv1.ObjectName(r.GatewayClassName) {
		return ctrl.Result{}, nil
	}

	logger.Info("reconciling gateway", "name", gateway.Name, "namespace", gateway.Namespace)

	// Resolve configuration from GatewayClassConfig
	resolvedConfig, err := r.ConfigResolver.ResolveFromGatewayClassName(ctx, r.GatewayClassName)
	if err != nil {
		logger.Error(err, "failed to resolve config from GatewayClassConfig")
		// Update Gateway status to reflect config error and requeue for retry
		if statusErr := r.setConfigErrorStatus(ctx, &gateway, err); statusErr != nil {
			logger.Error(statusErr, "failed to update gateway status")
		}

		return ctrl.Result{RequeueAfter: configErrorRequeueDelay, Priority: ptr.To(priorityGateway)}, nil
	}

	if !gateway.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, &gateway, resolvedConfig)
	}

	//nolint:nestif // cloudflared management requires nested validation
	if r.HelmManager != nil && resolvedConfig.CloudflaredEnabled {
		if !controllerutil.ContainsFinalizer(&gateway, cloudflaredFinalizer) {
			controllerutil.AddFinalizer(&gateway, cloudflaredFinalizer)

			if err := r.Update(ctx, &gateway); err != nil {
				return ctrl.Result{}, errors.Wrap(err, "failed to add finalizer")
			}
		}

		if err := r.ensureCloudflared(ctx, &gateway, resolvedConfig); err != nil {
			logger.Error(err, "failed to ensure cloudflared deployment")

			if statusErr := r.setCloudflaredErrorStatus(ctx, &gateway, resolvedConfig, err); statusErr != nil {
				logger.Error(statusErr, "failed to update gateway status")
			}

			return ctrl.Result{RequeueAfter: configErrorRequeueDelay}, nil
		}
	}

	if err := r.updateStatus(ctx, &gateway, resolvedConfig); err != nil {
		return ctrl.Result{}, errors.Wrap(err, "failed to update gateway status")
	}

	return ctrl.Result{}, nil
}

func (r *GatewayReconciler) handleDeletion(
	ctx context.Context,
	gateway *gatewayv1.Gateway,
	cfg *config.ResolvedConfig,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if !controllerutil.ContainsFinalizer(gateway, cloudflaredFinalizer) {
		return ctrl.Result{}, nil
	}

	if r.HelmManager != nil && cfg.CloudflaredEnabled {
		releaseName := cloudflaredReleaseName(gateway)
		logger.Info("removing cloudflared deployment", "release", releaseName)

		removeErr := r.removeCloudflared(ctx, gateway, cfg)
		if removeErr != nil {
			return ctrl.Result{}, errors.Wrap(removeErr, "failed to remove cloudflared")
		}
	}

	controllerutil.RemoveFinalizer(gateway, cloudflaredFinalizer)

	updateErr := r.Update(ctx, gateway)
	if updateErr != nil {
		return ctrl.Result{}, errors.Wrap(updateErr, "failed to remove finalizer")
	}

	return ctrl.Result{}, nil
}

func (r *GatewayReconciler) ensureCloudflared(
	ctx context.Context,
	gateway *gatewayv1.Gateway,
	cfg *config.ResolvedConfig,
) error {
	logger := log.FromContext(ctx)

	namespace := cfg.CloudflaredNamespace
	releaseName := cloudflaredReleaseName(gateway)

	latestVersion, err := r.HelmManager.GetLatestVersion(ctx, helm.DefaultChartRef)
	if err != nil {
		return errors.Wrap(err, "failed to get latest chart version")
	}

	logger.Info("ensuring cloudflared", "release", releaseName, "version", latestVersion, "namespace", namespace)

	loadedChart, err := r.HelmManager.LoadChart(ctx, helm.DefaultChartRef, latestVersion)
	if err != nil {
		return errors.Wrap(err, "failed to load chart")
	}

	values := r.buildCloudflaredValues(cfg)

	actionCfg, err := r.HelmManager.GetActionConfig(namespace)
	if err != nil {
		return errors.Wrap(err, "failed to get action config")
	}

	if r.HelmManager.ReleaseExists(actionCfg, releaseName) {
		return r.upgradeCloudflaredIfNeeded(ctx, actionCfg, releaseName, latestVersion, loadedChart, values)
	}

	logger.Info("installing cloudflared", "release", releaseName, "version", latestVersion)

	_, err = r.HelmManager.Install(ctx, actionCfg, releaseName, namespace, loadedChart, values)
	if err != nil {
		return errors.Wrap(err, "failed to install release")
	}

	return nil
}

func (r *GatewayReconciler) upgradeCloudflaredIfNeeded(
	ctx context.Context,
	actionCfg *action.Configuration,
	releaseName, latestVersion string,
	loadedChart chart.Charter,
	values map[string]any,
) error {
	logger := log.FromContext(ctx)

	rel, err := r.HelmManager.GetRelease(actionCfg, releaseName)
	if err != nil {
		return errors.Wrap(err, "failed to get existing release")
	}

	relAccessor, err := release.NewAccessor(rel)
	if err != nil {
		return errors.Wrap(err, "failed to create release accessor")
	}

	chartAccessor, err := chart.NewAccessor(relAccessor.Chart())
	if err != nil {
		return errors.Wrap(err, "failed to create chart accessor")
	}

	currentVersion := chartAccessor.MetadataAsMap()["Version"]
	versionChanged := currentVersion != latestVersion
	valuesChanged := cloudflaredValuesChanged(rel, values)

	if !versionChanged && !valuesChanged {
		return nil
	}

	reason := "version"
	if valuesChanged {
		reason = "values"
	}

	if versionChanged && valuesChanged {
		reason = "version and values"
	}

	logger.Info("upgrading cloudflared",
		"release", releaseName,
		"reason", reason,
		"fromVersion", currentVersion,
		"toVersion", latestVersion,
	)

	_, err = r.HelmManager.Upgrade(ctx, actionCfg, releaseName, loadedChart, values)
	if err != nil {
		return errors.Wrap(err, "failed to upgrade release")
	}

	return nil
}

// cloudflaredValuesChanged compares critical values between current and desired configurations.
// Returns true if an upgrade is needed due to values change.
func cloudflaredValuesChanged(rel release.Releaser, desired map[string]any) bool {
	// Type assert to get access to Config field
	v1rel, ok := rel.(*v1release.Release)
	if !ok {
		// If we can't determine current config, assume it changed to be safe
		return true
	}

	currentToken := getNestedString(v1rel.Config, "cloudflare", "tunnelToken")
	desiredToken := getNestedString(desired, "cloudflare", "tunnelToken")

	return currentToken != desiredToken
}

// getNestedString extracts a string value from a nested map structure.
// Returns empty string if path doesn't exist or value is not a string.
func getNestedString(m map[string]any, keys ...string) string {
	if len(keys) == 0 || m == nil {
		return ""
	}

	current := m

	for i, key := range keys {
		val, ok := current[key]
		if !ok {
			return ""
		}

		if i == len(keys)-1 {
			if str, ok := val.(string); ok {
				return str
			}

			return ""
		}

		if nested, ok := val.(map[string]any); ok {
			current = nested
		} else {
			return ""
		}
	}

	return ""
}

func (r *GatewayReconciler) removeCloudflared(
	ctx context.Context,
	gateway *gatewayv1.Gateway,
	cfg *config.ResolvedConfig,
) error {
	namespace := cfg.CloudflaredNamespace
	releaseName := cloudflaredReleaseName(gateway)

	actionCfg, err := r.HelmManager.GetActionConfig(namespace)
	if err != nil {
		return errors.Wrap(err, "failed to get action config")
	}

	if !r.HelmManager.ReleaseExists(actionCfg, releaseName) {
		return nil
	}

	return errors.Wrap(r.HelmManager.Uninstall(ctx, actionCfg, releaseName), "failed to uninstall cloudflared")
}

func (r *GatewayReconciler) buildCloudflaredValues(cfg *config.ResolvedConfig) map[string]any {
	cloudflaredValues := &helm.CloudflaredValues{
		TunnelToken:  cfg.TunnelToken,
		Protocol:     cfg.CloudflaredProtocol,
		ReplicaCount: int(cfg.CloudflaredReplicas),
		LivenessProbe: &helm.LivenessProbeValues{
			InitialDelaySeconds: cfg.LivenessProbeInitialDelay,
			TimeoutSeconds:      cfg.LivenessProbeTimeout,
			PeriodSeconds:       cfg.LivenessProbePeriod,
			SuccessThreshold:    cfg.LivenessProbeSuccess,
			FailureThreshold:    cfg.LivenessProbeFailure,
		},
	}

	if cfg.AWGSecretName != "" {
		cloudflaredValues.Sidecar = &helm.SidecarConfig{
			ConfigSecretName: cfg.AWGSecretName,
			InterfacePrefix:  cfg.AWGInterfacePrefix,
		}
	}

	return cloudflaredValues.BuildValues()
}

//nolint:funlen // status update logic with retry
func (r *GatewayReconciler) updateStatus(
	ctx context.Context,
	gateway *gatewayv1.Gateway,
	cfg *config.ResolvedConfig,
) error {
	gatewayKey := types.NamespacedName{Name: gateway.Name, Namespace: gateway.Namespace}

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// Get fresh copy of the gateway to avoid conflict errors
		var freshGateway gatewayv1.Gateway
		if err := r.Get(ctx, gatewayKey, &freshGateway); err != nil {
			return errors.Wrap(err, "failed to get fresh gateway")
		}

		now := metav1.Now()

		attachedRoutes := r.countAttachedRoutes(ctx, &freshGateway)

		freshGateway.Status.Addresses = []gatewayv1.GatewayStatusAddress{
			{
				Type:  ptr.To(gatewayv1.HostnameAddressType),
				Value: cfg.TunnelID + cfArgotunnelSuffix,
			},
		}

		freshGateway.Status.Conditions = []metav1.Condition{
			{
				Type:               string(gatewayv1.GatewayConditionAccepted),
				Status:             metav1.ConditionTrue,
				ObservedGeneration: freshGateway.Generation,
				LastTransitionTime: now,
				Reason:             string(gatewayv1.GatewayReasonAccepted),
				Message:            "Gateway accepted by cloudflare-tunnel controller",
			},
			{
				Type:               string(gatewayv1.GatewayConditionProgrammed),
				Status:             metav1.ConditionTrue,
				ObservedGeneration: freshGateway.Generation,
				LastTransitionTime: now,
				Reason:             string(gatewayv1.GatewayReasonProgrammed),
				Message:            "Gateway programmed in Cloudflare Tunnel",
			},
		}

		listenerStatuses := make([]gatewayv1.ListenerStatus, 0, len(freshGateway.Spec.Listeners))

		for i := range freshGateway.Spec.Listeners {
			listener := &freshGateway.Spec.Listeners[i]

			// Validate route kinds - filter to only supported kinds
			supportedKinds, hasValidKind, hasInvalidKind := routebinding.FilterSupportedKinds(
				listener.AllowedRoutes,
				listener.Protocol,
			)

			// Validate TLS certificate refs (if applicable)
			tlsStatus, tlsReason, tlsMessage := r.validateTLSCertificateRefs(
				ctx, &freshGateway, listener,
			)

			// Determine final ResolvedRefs condition
			resolvedRefsCondition := r.buildResolvedRefsCondition(
				freshGateway.Generation, now, hasValidKind, hasInvalidKind, tlsStatus, tlsReason, tlsMessage,
			)
			if !hasValidKind {
				supportedKinds = []gatewayv1.RouteGroupKind{} // Empty slice (not nil) when no valid kinds
			}

			listenerStatuses = append(listenerStatuses, gatewayv1.ListenerStatus{
				Name:           listener.Name,
				SupportedKinds: supportedKinds,
				AttachedRoutes: attachedRoutes[listener.Name],
				Conditions: []metav1.Condition{
					{
						Type:               string(gatewayv1.ListenerConditionAccepted),
						Status:             metav1.ConditionTrue,
						ObservedGeneration: freshGateway.Generation,
						LastTransitionTime: now,
						Reason:             string(gatewayv1.ListenerReasonAccepted),
						Message:            "Listener accepted",
					},
					{
						Type:               string(gatewayv1.ListenerConditionProgrammed),
						Status:             metav1.ConditionTrue,
						ObservedGeneration: freshGateway.Generation,
						LastTransitionTime: now,
						Reason:             string(gatewayv1.ListenerReasonProgrammed),
						Message:            "Listener programmed",
					},
					resolvedRefsCondition,
				},
			})
		}

		freshGateway.Status.Listeners = listenerStatuses

		if err := r.Status().Update(ctx, &freshGateway); err != nil {
			return errors.Wrap(err, "failed to update gateway status")
		}

		return nil
	})

	return errors.Wrap(err, "failed to update gateway status after retries")
}

func (r *GatewayReconciler) setConfigErrorStatus(
	ctx context.Context,
	gateway *gatewayv1.Gateway,
	configErr error,
) error {
	gatewayKey := types.NamespacedName{Name: gateway.Name, Namespace: gateway.Namespace}

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// Get fresh copy of the gateway to avoid conflict errors
		var freshGateway gatewayv1.Gateway
		if err := r.Get(ctx, gatewayKey, &freshGateway); err != nil {
			return errors.Wrap(err, "failed to get fresh gateway")
		}

		now := metav1.Now()
		errMsg := truncateMessage("Failed to resolve GatewayClassConfig: " + configErr.Error())

		// Clear addresses on config error (no valid tunnel to point to)
		freshGateway.Status.Addresses = nil

		freshGateway.Status.Conditions = []metav1.Condition{
			{
				Type:               string(gatewayv1.GatewayConditionAccepted),
				Status:             metav1.ConditionFalse,
				ObservedGeneration: freshGateway.Generation,
				LastTransitionTime: now,
				Reason:             string(gatewayv1.GatewayReasonInvalidParameters),
				Message:            errMsg,
			},
			{
				Type:               string(gatewayv1.GatewayConditionProgrammed),
				Status:             metav1.ConditionFalse,
				ObservedGeneration: freshGateway.Generation,
				LastTransitionTime: now,
				Reason:             string(gatewayv1.GatewayReasonInvalid),
				Message:            errMsg,
			},
		}

		// Clear listener statuses on config error
		freshGateway.Status.Listeners = nil

		if err := r.Status().Update(ctx, &freshGateway); err != nil {
			return errors.Wrap(err, "failed to update gateway status")
		}

		return nil
	})

	return errors.Wrap(err, "failed to update gateway status after retries")
}

func (r *GatewayReconciler) setCloudflaredErrorStatus(
	ctx context.Context,
	gateway *gatewayv1.Gateway,
	cfg *config.ResolvedConfig,
	cloudflaredErr error,
) error {
	gatewayKey := types.NamespacedName{Name: gateway.Name, Namespace: gateway.Namespace}

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var freshGateway gatewayv1.Gateway
		if err := r.Get(ctx, gatewayKey, &freshGateway); err != nil {
			return errors.Wrap(err, "failed to get fresh gateway")
		}

		now := metav1.Now()
		errMsg := truncateMessage("Failed to deploy cloudflared: " + cloudflaredErr.Error())

		// Set address even on error (tunnel exists, just cloudflared failed)
		freshGateway.Status.Addresses = []gatewayv1.GatewayStatusAddress{
			{
				Type:  ptr.To(gatewayv1.HostnameAddressType),
				Value: cfg.TunnelID + cfArgotunnelSuffix,
			},
		}

		freshGateway.Status.Conditions = []metav1.Condition{
			{
				Type:               string(gatewayv1.GatewayConditionAccepted),
				Status:             metav1.ConditionTrue,
				ObservedGeneration: freshGateway.Generation,
				LastTransitionTime: now,
				Reason:             string(gatewayv1.GatewayReasonAccepted),
				Message:            "Gateway accepted by cloudflare-tunnel controller",
			},
			{
				Type:               string(gatewayv1.GatewayConditionProgrammed),
				Status:             metav1.ConditionFalse,
				ObservedGeneration: freshGateway.Generation,
				LastTransitionTime: now,
				Reason:             "DeploymentFailed",
				Message:            errMsg,
			},
		}

		// Clear listener statuses on cloudflared error
		freshGateway.Status.Listeners = nil

		if err := r.Status().Update(ctx, &freshGateway); err != nil {
			return errors.Wrap(err, "failed to update gateway status")
		}

		return nil
	})

	return errors.Wrap(err, "failed to update gateway status after retries")
}

//nolint:gocognit,gocyclo,cyclop,dupl,funlen // complexity due to counting two route types
func (r *GatewayReconciler) countAttachedRoutes(
	ctx context.Context,
	gateway *gatewayv1.Gateway,
) map[gatewayv1.SectionName]int32 {
	logger := logging.FromContext(ctx)
	result := make(map[gatewayv1.SectionName]int32)

	for _, listener := range gateway.Spec.Listeners {
		result[listener.Name] = 0
	}

	validator := routebinding.NewValidator(r.Client)

	// Count HTTPRoutes with binding validation
	var httpRouteList gatewayv1.HTTPRouteList

	err := r.List(ctx, &httpRouteList)
	if err != nil {
		logger.Error("failed to list HTTPRoutes for attached routes count", "error", err)
	} else {
		for i := range httpRouteList.Items {
			route := &httpRouteList.Items[i]

			for _, ref := range route.Spec.ParentRefs {
				if !r.refMatchesGateway(ref, gateway, route.Namespace) {
					continue
				}

				routeInfo := &routebinding.RouteInfo{
					Name:        route.Name,
					Namespace:   route.Namespace,
					Hostnames:   route.Spec.Hostnames,
					Kind:        routebinding.KindHTTPRoute,
					SectionName: ref.SectionName,
				}

				bindingResult, bindErr := validator.ValidateBinding(ctx, gateway, routeInfo)
				if bindErr != nil || !bindingResult.Accepted {
					continue
				}

				// Count this route for each matched listener
				for _, listenerName := range bindingResult.MatchedListeners {
					result[listenerName]++
				}
			}
		}
	}

	// Count GRPCRoutes with binding validation
	var grpcRouteList gatewayv1.GRPCRouteList

	err = r.List(ctx, &grpcRouteList)
	if err != nil {
		logger.Error("failed to list GRPCRoutes for attached routes count", "error", err)
	} else {
		for i := range grpcRouteList.Items {
			route := &grpcRouteList.Items[i]

			for _, ref := range route.Spec.ParentRefs {
				if !r.refMatchesGateway(ref, gateway, route.Namespace) {
					continue
				}

				routeInfo := &routebinding.RouteInfo{
					Name:        route.Name,
					Namespace:   route.Namespace,
					Hostnames:   route.Spec.Hostnames,
					Kind:        routebinding.KindGRPCRoute,
					SectionName: ref.SectionName,
				}

				bindingResult, bindErr := validator.ValidateBinding(ctx, gateway, routeInfo)
				if bindErr != nil || !bindingResult.Accepted {
					continue
				}

				// Count this route for each matched listener
				for _, listenerName := range bindingResult.MatchedListeners {
					result[listenerName]++
				}
			}
		}
	}

	return result
}

func (r *GatewayReconciler) refMatchesGateway(
	ref gatewayv1.ParentReference,
	gateway *gatewayv1.Gateway,
	routeNamespace string,
) bool {
	if string(ref.Name) != gateway.Name {
		return false
	}

	refNamespace := routeNamespace
	if ref.Namespace != nil {
		refNamespace = string(*ref.Namespace)
	}

	return refNamespace == gateway.Namespace
}

// SetupWithManager sets up the controller with the Manager.
func (r *GatewayReconciler) SetupWithManager(mgr ctrl.Manager) error {
	mapper := &ConfigMapper{
		Client:           r.Client,
		GatewayClassName: r.GatewayClassName,
		ConfigResolver:   r.ConfigResolver,
	}

	//nolint:wrapcheck // controller-runtime builder pattern
	return ctrl.NewControllerManagedBy(mgr).
		For(&gatewayv1.Gateway{}).
		// Watch GatewayClass for parametersRef changes
		Watches(
			&gatewayv1.GatewayClass{},
			handler.EnqueueRequestsFromMapFunc(r.gatewayClassToGateways),
		).
		// Watch GatewayClassConfig for config changes
		Watches(
			&v1alpha1.GatewayClassConfig{},
			handler.EnqueueRequestsFromMapFunc(mapper.MapConfigToRequests(r.getAllGatewaysForClass)),
		).
		// Watch Secrets for credential changes
		Watches(
			&corev1.Secret{},
			handler.EnqueueRequestsFromMapFunc(mapper.MapSecretToRequests(r.getAllGatewaysForClass)),
		).
		// Watch ReferenceGrants for cross-namespace Secret access changes
		Watches(
			&gatewayv1beta1.ReferenceGrant{},
			handler.EnqueueRequestsFromMapFunc(r.referenceGrantToGateways),
		).
		Complete(r)
}

// gatewayClassToGateways maps GatewayClass events to Gateway reconcile requests.
func (r *GatewayReconciler) gatewayClassToGateways(
	ctx context.Context,
	obj client.Object,
) []reconcile.Request {
	gatewayClass, ok := obj.(*gatewayv1.GatewayClass)
	if !ok {
		return nil
	}

	if gatewayClass.Name != r.GatewayClassName {
		return nil
	}

	return r.getAllGatewaysForClass(ctx)
}

func (r *GatewayReconciler) getAllGatewaysForClass(ctx context.Context) []reconcile.Request {
	var gatewayList gatewayv1.GatewayList

	err := r.List(ctx, &gatewayList)
	if err != nil {
		return nil
	}

	var requests []reconcile.Request

	for i := range gatewayList.Items {
		gw := &gatewayList.Items[i]
		if string(gw.Spec.GatewayClassName) == r.GatewayClassName {
			requests = append(requests, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      gw.Name,
					Namespace: gw.Namespace,
				},
			})
		}
	}

	return requests
}

// referenceGrantToGateways maps ReferenceGrant events to Gateway reconcile requests.
// When a ReferenceGrant changes, we need to re-reconcile all Gateways that might
// reference Secrets in the ReferenceGrant's namespace.
func (r *GatewayReconciler) referenceGrantToGateways(
	ctx context.Context,
	obj client.Object,
) []reconcile.Request {
	grant, ok := obj.(*gatewayv1beta1.ReferenceGrant)
	if !ok {
		return nil
	}

	// Check if this ReferenceGrant allows Gateway access to Secrets
	allowsGatewayToSecrets := false

	for _, from := range grant.Spec.From {
		if from.Group == gatewayv1.GroupName && from.Kind == kindGateway {
			for _, to := range grant.Spec.To {
				if to.Group == "" && to.Kind == kindSecret {
					allowsGatewayToSecrets = true

					break
				}
			}
		}
	}

	if !allowsGatewayToSecrets {
		return nil
	}

	// Find all Gateways that reference Secrets in this namespace
	var gatewayList gatewayv1.GatewayList
	if err := r.List(ctx, &gatewayList); err != nil {
		return nil
	}

	var requests []reconcile.Request

	for i := range gatewayList.Items {
		gateway := &gatewayList.Items[i]
		if string(gateway.Spec.GatewayClassName) != r.GatewayClassName {
			continue
		}

		// Check if this Gateway references Secrets in the ReferenceGrant's namespace
		if r.gatewayReferencesSecretsInNamespace(gateway, grant.Namespace) {
			requests = append(requests, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      gateway.Name,
					Namespace: gateway.Namespace,
				},
			})
		}
	}

	return requests
}

// gatewayReferencesSecretsInNamespace checks if a Gateway references any Secrets
// in the given namespace through its TLS configuration.
func (r *GatewayReconciler) gatewayReferencesSecretsInNamespace(
	gateway *gatewayv1.Gateway,
	namespace string,
) bool {
	for i := range gateway.Spec.Listeners {
		listener := &gateway.Spec.Listeners[i]
		if listener.TLS == nil {
			continue
		}

		for _, ref := range listener.TLS.CertificateRefs {
			refNamespace := gateway.Namespace
			if ref.Namespace != nil {
				refNamespace = string(*ref.Namespace)
			}

			if refNamespace == namespace {
				return true
			}
		}
	}

	return false
}

// cloudflaredReleaseName generates a unique Helm release name for cloudflared
// based on the Gateway name and namespace. The name is truncated to fit Helm's
// 53 character limit for release names, ensuring it ends with an alphanumeric
// character (Helm requirement: must match ^[a-z0-9]([-a-z0-9]*[a-z0-9])?).
func cloudflaredReleaseName(gateway *gatewayv1.Gateway) string {
	name := "cfd-" + gateway.Namespace + "-" + gateway.Name
	if len(name) > maxHelmReleaseName {
		name = name[:maxHelmReleaseName]
	}

	// Trim trailing hyphens to ensure valid Helm release name
	name = strings.TrimRight(name, "-")

	return name
}

// validateTLSCertificateRefs validates TLS certificate references for a listener.
// Returns the condition status, reason, and message for the ResolvedRefs condition.
// Per Gateway API spec, TLS certificateRefs must point to valid Secrets of type
// kubernetes.io/tls, and cross-namespace references require ReferenceGrant.
func (r *GatewayReconciler) validateTLSCertificateRefs(
	ctx context.Context,
	gateway *gatewayv1.Gateway,
	listener *gatewayv1.Listener,
) (metav1.ConditionStatus, string, string) {
	// No TLS config - nothing to validate
	if listener.TLS == nil || len(listener.TLS.CertificateRefs) == 0 {
		return metav1.ConditionTrue,
			string(gatewayv1.ListenerReasonResolvedRefs),
			"References resolved"
	}

	for _, ref := range listener.TLS.CertificateRefs {
		status, reason, msg := r.validateSingleCertRef(ctx, gateway, ref)
		if status == metav1.ConditionFalse {
			return status, reason, msg
		}
	}

	return metav1.ConditionTrue,
		string(gatewayv1.ListenerReasonResolvedRefs),
		msgReferencesResolved
}

// validateSingleCertRef validates a single certificate reference.
func (r *GatewayReconciler) validateSingleCertRef(
	ctx context.Context,
	gateway *gatewayv1.Gateway,
	ref gatewayv1.SecretObjectReference,
) (metav1.ConditionStatus, string, string) {
	// Default to Secret in core v1
	refKind := kindSecret
	if ref.Kind != nil {
		refKind = string(*ref.Kind)
	}

	refGroup := ""
	if ref.Group != nil {
		refGroup = string(*ref.Group)
	}

	// Only support core/v1 Secrets
	if refGroup != "" || refKind != kindSecret {
		return metav1.ConditionFalse,
			string(gatewayv1.ListenerReasonInvalidCertificateRef),
			fmt.Sprintf("Unsupported certificate ref kind: %s/%s", refGroup, refKind)
	}

	// Determine namespace
	refNamespace := gateway.Namespace
	if ref.Namespace != nil {
		refNamespace = string(*ref.Namespace)
	}

	// Check cross-namespace access
	if refNamespace != gateway.Namespace {
		allowed, err := r.checkSecretReferenceGrant(ctx, gateway, refNamespace, ref)
		if err != nil {
			return metav1.ConditionFalse,
				string(gatewayv1.ListenerReasonRefNotPermitted),
				fmt.Sprintf("Failed to check ReferenceGrant: %v", err)
		}

		if !allowed {
			return metav1.ConditionFalse,
				string(gatewayv1.ListenerReasonRefNotPermitted),
				fmt.Sprintf("Cross-namespace reference to %s/%s not permitted", refNamespace, ref.Name)
		}
	}

	// Check Secret exists and has correct type
	return r.validateSecretExists(ctx, refNamespace, ref)
}

// validateSecretExists checks if a Secret exists and has type kubernetes.io/tls.
func (r *GatewayReconciler) validateSecretExists(
	ctx context.Context,
	namespace string,
	ref gatewayv1.SecretObjectReference,
) (metav1.ConditionStatus, string, string) {
	secret := &corev1.Secret{}

	err := r.Get(ctx, types.NamespacedName{
		Namespace: namespace,
		Name:      string(ref.Name),
	}, secret)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return metav1.ConditionFalse,
				string(gatewayv1.ListenerReasonInvalidCertificateRef),
				fmt.Sprintf("Secret %s/%s not found", namespace, ref.Name)
		}

		return metav1.ConditionFalse,
			string(gatewayv1.ListenerReasonInvalidCertificateRef),
			fmt.Sprintf("Failed to get secret: %v", err)
	}

	// Validate Secret type
	if secret.Type != corev1.SecretTypeTLS {
		return metav1.ConditionFalse,
			string(gatewayv1.ListenerReasonInvalidCertificateRef),
			fmt.Sprintf("Secret %s/%s is not of type kubernetes.io/tls", namespace, ref.Name)
	}

	// Validate certificate data exists and is valid PEM
	certData, hasCert := secret.Data[corev1.TLSCertKey]
	if !hasCert || len(certData) == 0 {
		return metav1.ConditionFalse,
			string(gatewayv1.ListenerReasonInvalidCertificateRef),
			fmt.Sprintf("Secret %s/%s missing tls.crt data", namespace, ref.Name)
	}

	keyData, hasKey := secret.Data[corev1.TLSPrivateKeyKey]
	if !hasKey || len(keyData) == 0 {
		return metav1.ConditionFalse,
			string(gatewayv1.ListenerReasonInvalidCertificateRef),
			fmt.Sprintf("Secret %s/%s missing tls.key data", namespace, ref.Name)
	}

	// Validate that certificate contains valid PEM data
	block, _ := pem.Decode(certData)
	if block == nil {
		return metav1.ConditionFalse,
			string(gatewayv1.ListenerReasonInvalidCertificateRef),
			fmt.Sprintf("Secret %s/%s contains invalid certificate PEM data", namespace, ref.Name)
	}

	return metav1.ConditionTrue, "", ""
}

// buildResolvedRefsCondition creates the ResolvedRefs condition based on validation results.
// Per Gateway API spec:
//   - If no supported kinds exist: ResolvedRefs=False, InvalidRouteKinds
//   - If any explicitly specified kinds are invalid: ResolvedRefs=False, InvalidRouteKinds
//   - If TLS validation fails: ResolvedRefs=False, with TLS-specific reason
//   - Otherwise: ResolvedRefs=True
func (r *GatewayReconciler) buildResolvedRefsCondition(
	generation int64,
	now metav1.Time,
	hasValidKind, hasInvalidKind bool,
	tlsStatus metav1.ConditionStatus,
	tlsReason, tlsMessage string,
) metav1.Condition {
	switch {
	case !hasValidKind:
		// No supported kinds at all
		return metav1.Condition{
			Type:               string(gatewayv1.ListenerConditionResolvedRefs),
			Status:             metav1.ConditionFalse,
			ObservedGeneration: generation,
			LastTransitionTime: now,
			Reason:             string(gatewayv1.ListenerReasonInvalidRouteKinds),
			Message:            "None of the specified route kinds are supported",
		}
	case hasInvalidKind:
		// Some valid kinds exist, but some explicitly specified kinds are invalid
		return metav1.Condition{
			Type:               string(gatewayv1.ListenerConditionResolvedRefs),
			Status:             metav1.ConditionFalse,
			ObservedGeneration: generation,
			LastTransitionTime: now,
			Reason:             string(gatewayv1.ListenerReasonInvalidRouteKinds),
			Message:            "One or more specified route kinds are not supported",
		}
	case tlsStatus == metav1.ConditionFalse:
		return metav1.Condition{
			Type:               string(gatewayv1.ListenerConditionResolvedRefs),
			Status:             tlsStatus,
			ObservedGeneration: generation,
			LastTransitionTime: now,
			Reason:             tlsReason,
			Message:            tlsMessage,
		}
	default:
		return metav1.Condition{
			Type:               string(gatewayv1.ListenerConditionResolvedRefs),
			Status:             metav1.ConditionTrue,
			ObservedGeneration: generation,
			LastTransitionTime: now,
			Reason:             string(gatewayv1.ListenerReasonResolvedRefs),
			Message:            msgReferencesResolved,
		}
	}
}

// checkSecretReferenceGrant checks if a cross-namespace Secret reference is allowed
// by a ReferenceGrant in the target namespace.
func (r *GatewayReconciler) checkSecretReferenceGrant(
	ctx context.Context,
	gateway *gatewayv1.Gateway,
	targetNamespace string,
	ref gatewayv1.SecretObjectReference,
) (bool, error) {
	var grants gatewayv1beta1.ReferenceGrantList
	if err := r.List(ctx, &grants, client.InNamespace(targetNamespace)); err != nil {
		return false, errors.Wrap(err, "failed to list ReferenceGrants")
	}

	for i := range grants.Items {
		grant := &grants.Items[i]

		if !r.grantAllowsGateway(grant, gateway.Namespace) {
			continue
		}

		// Check To: must allow Secret with matching name
		// Per Gateway API spec, if to.Name is nil or empty, it allows ALL secrets in namespace
		for _, to := range grant.Spec.To {
			if to.Group == "" && to.Kind == kindSecret {
				// nil or empty name means "all secrets in namespace"
				if to.Name == nil || *to.Name == "" || string(*to.Name) == string(ref.Name) {
					return true, nil
				}
			}
		}
	}

	return false, nil
}

// grantAllowsGateway checks if a ReferenceGrant allows Gateway from the given namespace.
func (r *GatewayReconciler) grantAllowsGateway(
	grant *gatewayv1beta1.ReferenceGrant,
	gatewayNamespace string,
) bool {
	for _, from := range grant.Spec.From {
		if from.Group == gatewayv1.GroupName &&
			from.Kind == "Gateway" &&
			string(from.Namespace) == gatewayNamespace {
			return true
		}
	}

	return false
}
