package controller

import (
	"context"
	"time"

	"github.com/cockroachdb/errors"
	"helm.sh/helm/v4/pkg/action"
	"helm.sh/helm/v4/pkg/chart"
	"helm.sh/helm/v4/pkg/release"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/api/v1alpha1"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/config"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/helm"
)

const (
	cloudflaredFinalizer = "cloudflare-tunnel.gateway.networking.k8s.io/cloudflared"
	cfArgotunnelSuffix   = ".cfargotunnel.com"

	// configErrorRequeueDelay is the delay before retrying when config resolution fails.
	configErrorRequeueDelay = 30 * time.Second

	// maxHelmReleaseName is the maximum length for Helm release names.
	maxHelmReleaseName = 53
)

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

//nolint:noinlineerr // controller reconcile logic
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

		return ctrl.Result{RequeueAfter: configErrorRequeueDelay}, nil
	}

	if !gateway.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, &gateway, resolvedConfig)
	}

	if r.HelmManager != nil && resolvedConfig.CloudflaredEnabled {
		if !controllerutil.ContainsFinalizer(&gateway, cloudflaredFinalizer) {
			controllerutil.AddFinalizer(&gateway, cloudflaredFinalizer)

			if err := r.Update(ctx, &gateway); err != nil {
				return ctrl.Result{}, errors.Wrap(err, "failed to add finalizer")
			}
		}

		if err := r.ensureCloudflared(ctx, &gateway, resolvedConfig); err != nil {
			return ctrl.Result{}, errors.Wrap(err, "failed to ensure cloudflared deployment")
		}
	}

	if err := r.updateStatus(ctx, &gateway, resolvedConfig); err != nil {
		return ctrl.Result{}, errors.Wrap(err, "failed to update gateway status")
	}

	return ctrl.Result{}, nil
}

//nolint:funcorder // deletion handler
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

		removeErr := r.removeCloudflared(gateway, cfg)
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

//nolint:funcorder // helm operations
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

//nolint:funcorder // helm operations helper
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

	relAccessor, ok := rel.(release.Accessor)
	if !ok {
		return errors.New("failed to cast release to Accessor")
	}

	chartAccessor, ok := relAccessor.Chart().(chart.Accessor)
	if !ok {
		return errors.New("failed to cast chart to Accessor")
	}

	currentVersion := chartAccessor.MetadataAsMap()["version"]
	if currentVersion == latestVersion {
		return nil
	}

	logger.Info("upgrading cloudflared", "release", releaseName, "from", currentVersion, "to", latestVersion)

	_, err = r.HelmManager.Upgrade(ctx, actionCfg, releaseName, loadedChart, values)
	if err != nil {
		return errors.Wrap(err, "failed to upgrade release")
	}

	return nil
}

//nolint:funcorder // helm operations
func (r *GatewayReconciler) removeCloudflared(gateway *gatewayv1.Gateway, cfg *config.ResolvedConfig) error {
	namespace := cfg.CloudflaredNamespace
	releaseName := cloudflaredReleaseName(gateway)

	actionCfg, err := r.HelmManager.GetActionConfig(namespace)
	if err != nil {
		return errors.Wrap(err, "failed to get action config")
	}

	if !r.HelmManager.ReleaseExists(actionCfg, releaseName) {
		return nil
	}

	return errors.Wrap(r.HelmManager.Uninstall(actionCfg, releaseName), "failed to uninstall cloudflared")
}

//nolint:funcorder // value builder
func (r *GatewayReconciler) buildCloudflaredValues(cfg *config.ResolvedConfig) map[string]any {
	cloudflaredValues := &helm.CloudflaredValues{
		TunnelToken:  cfg.TunnelToken,
		Protocol:     cfg.CloudflaredProtocol,
		ReplicaCount: int(cfg.CloudflaredReplicas),
	}

	if cfg.AWGSecretName != "" {
		cloudflaredValues.Sidecar = &helm.SidecarConfig{
			ConfigSecretName: cfg.AWGSecretName,
			InterfacePrefix:  cfg.AWGInterfacePrefix,
		}
	}

	return cloudflaredValues.BuildValues()
}

//nolint:funcorder,funlen // status update logic
func (r *GatewayReconciler) updateStatus(
	ctx context.Context,
	gateway *gatewayv1.Gateway,
	cfg *config.ResolvedConfig,
) error {
	now := metav1.Now()

	attachedRoutes := r.countAttachedRoutes(ctx, gateway)

	gateway.Status.Addresses = []gatewayv1.GatewayStatusAddress{
		{
			Type:  ptr(gatewayv1.HostnameAddressType),
			Value: cfg.TunnelID + cfArgotunnelSuffix,
		},
	}

	gateway.Status.Conditions = []metav1.Condition{
		{
			Type:               string(gatewayv1.GatewayConditionAccepted),
			Status:             metav1.ConditionTrue,
			ObservedGeneration: gateway.Generation,
			LastTransitionTime: now,
			Reason:             string(gatewayv1.GatewayReasonAccepted),
			Message:            "Gateway accepted by cloudflare-tunnel controller",
		},
		{
			Type:               string(gatewayv1.GatewayConditionProgrammed),
			Status:             metav1.ConditionTrue,
			ObservedGeneration: gateway.Generation,
			LastTransitionTime: now,
			Reason:             string(gatewayv1.GatewayReasonProgrammed),
			Message:            "Gateway programmed in Cloudflare Tunnel",
		},
	}

	listenerStatuses := make([]gatewayv1.ListenerStatus, 0, len(gateway.Spec.Listeners))

	for _, listener := range gateway.Spec.Listeners {
		listenerStatuses = append(listenerStatuses, gatewayv1.ListenerStatus{
			Name: listener.Name,
			SupportedKinds: []gatewayv1.RouteGroupKind{
				{
					Group: (*gatewayv1.Group)(&gatewayv1.GroupVersion.Group),
					Kind:  "HTTPRoute",
				},
			},
			AttachedRoutes: attachedRoutes[listener.Name],
			Conditions: []metav1.Condition{
				{
					Type:               string(gatewayv1.ListenerConditionAccepted),
					Status:             metav1.ConditionTrue,
					ObservedGeneration: gateway.Generation,
					LastTransitionTime: now,
					Reason:             string(gatewayv1.ListenerReasonAccepted),
					Message:            "Listener accepted",
				},
				{
					Type:               string(gatewayv1.ListenerConditionProgrammed),
					Status:             metav1.ConditionTrue,
					ObservedGeneration: gateway.Generation,
					LastTransitionTime: now,
					Reason:             string(gatewayv1.ListenerReasonProgrammed),
					Message:            "Listener programmed",
				},
				{
					Type:               string(gatewayv1.ListenerConditionResolvedRefs),
					Status:             metav1.ConditionTrue,
					ObservedGeneration: gateway.Generation,
					LastTransitionTime: now,
					Reason:             string(gatewayv1.ListenerReasonResolvedRefs),
					Message:            "References resolved",
				},
			},
		})
	}

	gateway.Status.Listeners = listenerStatuses

	statusErr := r.Status().Update(ctx, gateway)
	if statusErr != nil {
		return errors.Wrap(statusErr, "failed to update gateway status")
	}

	return nil
}

//nolint:funcorder // error status helper
func (r *GatewayReconciler) setConfigErrorStatus(
	ctx context.Context,
	gateway *gatewayv1.Gateway,
	configErr error,
) error {
	now := metav1.Now()

	gateway.Status.Conditions = []metav1.Condition{
		{
			Type:               string(gatewayv1.GatewayConditionAccepted),
			Status:             metav1.ConditionFalse,
			ObservedGeneration: gateway.Generation,
			LastTransitionTime: now,
			Reason:             "InvalidParameters",
			Message:            "Failed to resolve GatewayClassConfig: " + configErr.Error(),
		},
	}

	return errors.Wrap(r.Status().Update(ctx, gateway), "failed to update gateway status")
}

//nolint:funcorder // helper method for status
func (r *GatewayReconciler) countAttachedRoutes(
	ctx context.Context,
	gateway *gatewayv1.Gateway,
) map[gatewayv1.SectionName]int32 {
	result := make(map[gatewayv1.SectionName]int32)

	for _, listener := range gateway.Spec.Listeners {
		result[listener.Name] = 0
	}

	var routeList gatewayv1.HTTPRouteList

	err := r.List(ctx, &routeList)
	if err != nil {
		return result
	}

	for i := range routeList.Items {
		route := &routeList.Items[i]

		for _, ref := range route.Spec.ParentRefs {
			if !r.refMatchesGateway(ref, gateway, route.Namespace) {
				continue
			}

			if ref.SectionName != nil {
				result[*ref.SectionName]++
			} else {
				for _, listener := range gateway.Spec.Listeners {
					result[listener.Name]++
				}
			}
		}
	}

	return result
}

//nolint:funcorder // helper method
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

func ptr[T any](v T) *T {
	return &v
}

// cloudflaredReleaseName generates a unique Helm release name for cloudflared
// based on the Gateway name and namespace. The name is truncated to fit Helm's
// 53 character limit for release names.
func cloudflaredReleaseName(gateway *gatewayv1.Gateway) string {
	name := "cfd-" + gateway.Namespace + "-" + gateway.Name
	if len(name) > maxHelmReleaseName {
		return name[:maxHelmReleaseName]
	}

	return name
}
