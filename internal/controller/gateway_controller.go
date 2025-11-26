package controller

import (
	"context"

	"github.com/cockroachdb/errors"
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
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/api/v1alpha1"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/config"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/helm"
)

const (
	cloudflaredFinalizer = "cloudflare-tunnel.gateway.networking.k8s.io/cloudflared"
	cloudflaredRelease   = "cloudflared"
	cfArgotunnelSuffix   = ".cfargotunnel.com"
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
		// Update Gateway status to reflect config error
		return ctrl.Result{}, r.setConfigErrorStatus(ctx, &gateway, err)
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

		if err := r.ensureCloudflared(ctx, resolvedConfig); err != nil {
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
		logger.Info("removing cloudflared deployment")

		removeErr := r.removeCloudflared(cfg)
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
func (r *GatewayReconciler) ensureCloudflared(ctx context.Context, cfg *config.ResolvedConfig) error {
	logger := log.FromContext(ctx)

	namespace := cfg.CloudflaredNamespace

	latestVersion, err := r.HelmManager.GetLatestVersion(ctx, helm.DefaultChartRef)
	if err != nil {
		return errors.Wrap(err, "failed to get latest chart version")
	}

	logger.Info("ensuring cloudflared", "version", latestVersion, "namespace", namespace)

	loadedChart, err := r.HelmManager.LoadChart(ctx, helm.DefaultChartRef, latestVersion)
	if err != nil {
		return errors.Wrap(err, "failed to load chart")
	}

	values := r.buildCloudflaredValues(cfg)

	actionCfg, err := r.HelmManager.GetActionConfig(namespace)
	if err != nil {
		return errors.Wrap(err, "failed to get action config")
	}

	if r.HelmManager.ReleaseExists(actionCfg, cloudflaredRelease) {
		rel, getErr := r.HelmManager.GetRelease(actionCfg, cloudflaredRelease)
		if getErr != nil {
			return errors.Wrap(getErr, "failed to get existing release")
		}

		if rel.Chart.Metadata.Version != latestVersion {
			logger.Info("upgrading cloudflared",
				"from", rel.Chart.Metadata.Version,
				"to", latestVersion,
			)

			_, upgradeErr := r.HelmManager.Upgrade(ctx, actionCfg, cloudflaredRelease, loadedChart, values)
			if upgradeErr != nil {
				return errors.Wrap(upgradeErr, "failed to upgrade release")
			}
		}

		return nil
	}

	logger.Info("installing cloudflared", "version", latestVersion)

	_, err = r.HelmManager.Install(ctx, actionCfg, cloudflaredRelease, namespace, loadedChart, values)
	if err != nil {
		return errors.Wrap(err, "failed to install release")
	}

	return nil
}

//nolint:funcorder // helm operations
func (r *GatewayReconciler) removeCloudflared(cfg *config.ResolvedConfig) error {
	namespace := cfg.CloudflaredNamespace

	actionCfg, err := r.HelmManager.GetActionConfig(namespace)
	if err != nil {
		return errors.Wrap(err, "failed to get action config")
	}

	if !r.HelmManager.ReleaseExists(actionCfg, cloudflaredRelease) {
		return nil
	}

	return errors.Wrap(r.HelmManager.Uninstall(actionCfg, cloudflaredRelease), "failed to uninstall cloudflared")
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
			InterfaceName:    cfg.AWGInterfaceName,
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

	return r.Status().Update(ctx, gateway)
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
			handler.EnqueueRequestsFromMapFunc(r.configToGateways),
		).
		// Watch Secrets for credential changes
		Watches(
			&corev1.Secret{},
			handler.EnqueueRequestsFromMapFunc(r.secretToGateways),
		).
		WithEventFilter(predicate.GenerationChangedPredicate{}).
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

// configToGateways maps GatewayClassConfig events to Gateway reconcile requests.
func (r *GatewayReconciler) configToGateways(
	ctx context.Context,
	obj client.Object,
) []reconcile.Request {
	cfg, ok := obj.(*v1alpha1.GatewayClassConfig)
	if !ok {
		return nil
	}

	// Check if this config is referenced by our GatewayClass
	gatewayClass := &gatewayv1.GatewayClass{}
	if err := r.Get(ctx, types.NamespacedName{Name: r.GatewayClassName}, gatewayClass); err != nil {
		return nil
	}

	if gatewayClass.Spec.ParametersRef == nil {
		return nil
	}

	if gatewayClass.Spec.ParametersRef.Name != cfg.Name {
		return nil
	}

	return r.getAllGatewaysForClass(ctx)
}

// secretToGateways maps Secret events to Gateway reconcile requests.
func (r *GatewayReconciler) secretToGateways(
	ctx context.Context,
	obj client.Object,
) []reconcile.Request {
	secret, ok := obj.(*corev1.Secret)
	if !ok {
		return nil
	}

	// Get the GatewayClassConfig for our class
	gatewayClass := &gatewayv1.GatewayClass{}
	if err := r.Get(ctx, types.NamespacedName{Name: r.GatewayClassName}, gatewayClass); err != nil {
		return nil
	}

	cfg, err := r.ConfigResolver.GetConfigForGatewayClass(ctx, gatewayClass)
	if err != nil {
		return nil
	}

	// Check if this secret is referenced by the config
	if r.secretMatchesConfig(secret, cfg) {
		return r.getAllGatewaysForClass(ctx)
	}

	return nil
}

func (r *GatewayReconciler) secretMatchesConfig(secret *corev1.Secret, cfg *v1alpha1.GatewayClassConfig) bool {
	// Check credentials secret
	credRef := cfg.Spec.CloudflareCredentialsSecretRef
	if secret.Name == credRef.Name && (credRef.Namespace == "" || credRef.Namespace == secret.Namespace) {
		return true
	}

	// Check tunnel token secret
	if cfg.Spec.TunnelTokenSecretRef != nil {
		tokenRef := cfg.Spec.TunnelTokenSecretRef
		if secret.Name == tokenRef.Name && (tokenRef.Namespace == "" || tokenRef.Namespace == secret.Namespace) {
			return true
		}
	}

	return false
}

func (r *GatewayReconciler) getAllGatewaysForClass(ctx context.Context) []reconcile.Request {
	var gatewayList gatewayv1.GatewayList
	if err := r.List(ctx, &gatewayList); err != nil {
		return nil
	}

	var requests []reconcile.Request
	for _, gw := range gatewayList.Items {
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
