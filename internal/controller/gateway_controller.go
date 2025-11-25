package controller

import (
	"context"

	"github.com/cockroachdb/errors"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/helm"
)

const (
	cloudflaredFinalizer = "cloudflare-tunnel.gateway.networking.k8s.io/cloudflared"
	cloudflaredRelease   = "cloudflared"
	defaultCloudflaredNS = "cloudflare-tunnel-system"
	cfArgotunnelSuffix   = ".cfargotunnel.com"
)

// GatewayReconciler reconciles Gateway resources for the cloudflare-tunnel GatewayClass.
//
// It performs the following functions:
//   - Watches Gateway resources matching the configured GatewayClassName
//   - Updates Gateway status with tunnel CNAME address (for external-dns integration)
//   - Manages cloudflared deployment lifecycle via Helm (when HelmManager is set)
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

	// TunnelID is the Cloudflare Tunnel ID, used for status address.
	TunnelID string

	// HelmManager handles cloudflared deployment. If nil, cloudflared
	// management is disabled and must be deployed separately.
	HelmManager *helm.Manager

	// TunnelToken is passed to cloudflared for tunnel authentication.
	TunnelToken string

	// CloudflaredNS is the namespace for cloudflared deployment.
	CloudflaredNS string

	// Protocol is the cloudflared transport protocol (auto, quic, http2).
	Protocol string

	// AWGSecretName enables AWG sidecar if set.
	AWGSecretName string

	// AWGInterfaceName is the AWG network interface name.
	AWGInterfaceName string
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

	if !gateway.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, &gateway)
	}

	if r.HelmManager != nil {
		if !controllerutil.ContainsFinalizer(&gateway, cloudflaredFinalizer) {
			controllerutil.AddFinalizer(&gateway, cloudflaredFinalizer)

			if err := r.Update(ctx, &gateway); err != nil {
				return ctrl.Result{}, errors.Wrap(err, "failed to add finalizer")
			}
		}

		if err := r.ensureCloudflared(ctx); err != nil {
			return ctrl.Result{}, errors.Wrap(err, "failed to ensure cloudflared deployment")
		}
	}

	if err := r.updateStatus(ctx, &gateway); err != nil {
		return ctrl.Result{}, errors.Wrap(err, "failed to update gateway status")
	}

	return ctrl.Result{}, nil
}

//nolint:funcorder // deletion handler
func (r *GatewayReconciler) handleDeletion(
	ctx context.Context,
	gateway *gatewayv1.Gateway,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if !controllerutil.ContainsFinalizer(gateway, cloudflaredFinalizer) {
		return ctrl.Result{}, nil
	}

	if r.HelmManager != nil {
		logger.Info("removing cloudflared deployment")

		removeErr := r.removeCloudflared()
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
func (r *GatewayReconciler) ensureCloudflared(ctx context.Context) error {
	logger := log.FromContext(ctx)

	namespace := r.CloudflaredNS
	if namespace == "" {
		namespace = defaultCloudflaredNS
	}

	latestVersion, err := r.HelmManager.GetLatestVersion(ctx, helm.DefaultChartRef)
	if err != nil {
		return errors.Wrap(err, "failed to get latest chart version")
	}

	logger.Info("ensuring cloudflared", "version", latestVersion, "namespace", namespace)

	loadedChart, err := r.HelmManager.LoadChart(ctx, helm.DefaultChartRef, latestVersion)
	if err != nil {
		return errors.Wrap(err, "failed to load chart")
	}

	values := r.buildCloudflaredValues()

	cfg, err := r.HelmManager.GetActionConfig(namespace)
	if err != nil {
		return errors.Wrap(err, "failed to get action config")
	}

	if r.HelmManager.ReleaseExists(cfg, cloudflaredRelease) {
		rel, getErr := r.HelmManager.GetRelease(cfg, cloudflaredRelease)
		if getErr != nil {
			return errors.Wrap(getErr, "failed to get existing release")
		}

		if rel.Chart.Metadata.Version != latestVersion {
			logger.Info("upgrading cloudflared",
				"from", rel.Chart.Metadata.Version,
				"to", latestVersion,
			)

			_, upgradeErr := r.HelmManager.Upgrade(ctx, cfg, cloudflaredRelease, loadedChart, values)
			if upgradeErr != nil {
				return errors.Wrap(upgradeErr, "failed to upgrade release")
			}
		}

		return nil
	}

	logger.Info("installing cloudflared", "version", latestVersion)

	_, err = r.HelmManager.Install(ctx, cfg, cloudflaredRelease, namespace, loadedChart, values)
	if err != nil {
		return errors.Wrap(err, "failed to install release")
	}

	return nil
}

//nolint:funcorder // helm operations
func (r *GatewayReconciler) removeCloudflared() error {
	namespace := r.CloudflaredNS
	if namespace == "" {
		namespace = defaultCloudflaredNS
	}

	cfg, err := r.HelmManager.GetActionConfig(namespace)
	if err != nil {
		return errors.Wrap(err, "failed to get action config")
	}

	if !r.HelmManager.ReleaseExists(cfg, cloudflaredRelease) {
		return nil
	}

	return errors.Wrap(r.HelmManager.Uninstall(cfg, cloudflaredRelease), "failed to uninstall cloudflared")
}

//nolint:funcorder // value builder
func (r *GatewayReconciler) buildCloudflaredValues() map[string]any {
	cloudflaredValues := &helm.CloudflaredValues{
		TunnelToken:  r.TunnelToken,
		Protocol:     r.Protocol,
		ReplicaCount: 1,
	}

	if r.AWGSecretName != "" {
		cloudflaredValues.Sidecar = &helm.SidecarConfig{
			ConfigSecretName: r.AWGSecretName,
			InterfaceName:    r.AWGInterfaceName,
		}
	}

	return cloudflaredValues.BuildValues()
}

//nolint:funcorder,funlen // status update logic
func (r *GatewayReconciler) updateStatus(ctx context.Context, gateway *gatewayv1.Gateway) error {
	now := metav1.Now()

	attachedRoutes := r.countAttachedRoutes(ctx, gateway)

	gateway.Status.Addresses = []gatewayv1.GatewayStatusAddress{
		{
			Type:  ptr(gatewayv1.HostnameAddressType),
			Value: r.TunnelID + cfArgotunnelSuffix,
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

func (r *GatewayReconciler) SetupWithManager(mgr ctrl.Manager) error {
	//nolint:wrapcheck // controller-runtime builder pattern
	return ctrl.NewControllerManagedBy(mgr).
		For(&gatewayv1.Gateway{}).
		WithEventFilter(predicate.GenerationChangedPredicate{}).
		Complete(r)
}

func ptr[T any](v T) *T {
	return &v
}
